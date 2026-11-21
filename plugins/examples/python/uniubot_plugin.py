"""uniubot_plugin - Python SDK for uniubot plugins.

A plugin declares commands and events, the host handles prefixes and roles:

    plugin = Plugin(plugin_id="my-plugin", capabilities=["message.reply"])

    @plugin.command("my.echo", "echo back")
    def echo(ctx):
        return result(text(" ".join(str(x) for x in ctx.args.get("_") or [])))

    @plugin.on("message")
    def on_message(ctx):
        return none()

Standard library only. stdout carries NDJSON protocol frames, stderr the logs.
"""

from __future__ import annotations

import json
import os
import sys
import threading
import time
import traceback
from typing import Any, Callable, Dict, List, Optional

PROTOCOL_VERSION = "1.0"
API_VERSION = "v1"

# JSON-RPC error codes (see docs/plugin-protocol.md).
PARSE_ERROR = -32700
INVALID_REQUEST = -32600
METHOD_NOT_FOUND = -32601
INVALID_PARAMS = -32602
INTERNAL_ERROR = -32603
PLUGIN_ERROR = -32000
TIMEOUT = -32001
NOT_READY = -32003
CAPABILITY_DENIED = -32004
CANCELLED = -32005


class RpcError(Exception):
    """Returned to the caller as a JSON-RPC error object."""

    def __init__(self, code: int, message: str, data: Any = None) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.data = data

    def to_dict(self) -> Dict[str, Any]:
        payload: Dict[str, Any] = {"code": self.code, "message": self.message}
        if self.data is not None:
            payload["data"] = self.data
        return payload




def text(value: Any) -> dict:
    return {"type": "text", "data": {"text": str(value)}}


def at(user_id: Any) -> dict:
    return {"type": "at", "data": {"user_id": str(user_id)}}


def reply(message_id: Any) -> dict:
    return {"type": "reply", "data": {"id": message_id}}


def image(file: str) -> dict:
    return {"type": "image", "data": {"file": file}}


def raw(platform: str, segment_type: str, data: Optional[dict] = None) -> dict:
    return {
        "type": "raw",
        "data": {"platform": platform, "type": segment_type, "data": data or {}},
    }


def result(*segments: dict) -> dict:
    """Build a handler result that the host sends as the reply."""
    return {"segments": list(segments), "finish": True}


def none() -> dict:
    """Build a handler result that produces no reply."""
    return {}




class CommandContext:
    """Argument bundle passed to a command handler."""

    __slots__ = ("command", "args", "event", "event_id", "bot", "adapters", "host")

    def __init__(self, host: "Host", params: dict) -> None:
        self.host = host
        self.command: str = params.get("command") or ""
        self.args: Dict[str, Any] = params.get("args") or {"_": []}
        self.event: dict = params.get("event") or {}
        self.event_id: str = params.get("eventId") or ""
        self.bot: dict = params.get("bot") or {}
        self.adapters: List[str] = params.get("adapters") or []

    def reply(self, *segments: dict) -> dict:
        """Reply through the host RPC and return an empty handler result."""
        self.host.reply(self.event_id, list(segments))
        return {}


class EventContext:
    """Argument bundle passed to an event handler."""

    __slots__ = ("event_name", "event", "event_id", "bot", "host")

    def __init__(self, host: "Host", params: dict) -> None:
        self.host = host
        self.event_name: str = params.get("eventName") or ""
        self.event: dict = params.get("event") or {}
        self.event_id: str = params.get("eventId") or ""
        self.bot: dict = params.get("bot") or {}

    def reply(self, *segments: dict) -> dict:
        self.host.reply(self.event_id, list(segments))
        return {}




class Host:
    """The plugin side of the host RPC (capability checked by the host)."""

    def __init__(self, peer: "Peer") -> None:
        self._peer = peer
        self.timeout = 10.0

    def call(self, method: str, params: Optional[dict] = None) -> Any:
        return self._peer.call(method, params, self.timeout)

    def log(self, level: str, message: str, **fields: Any) -> None:
        payload: Dict[str, Any] = {"level": level, "message": message}
        if fields:
            payload["fields"] = fields
        self._peer.notify("host.log", payload)

    def info(self, message: str, **fields: Any) -> None:
        self.log("INFO", message, **fields)

    def warn(self, message: str, **fields: Any) -> None:
        self.log("WARN", message, **fields)

    def error(self, message: str, **fields: Any) -> None:
        self.log("ERROR", message, **fields)

    def reply(self, event_id: str, segments: List[dict]) -> Any:
        return self.call("host.message.reply", {"eventId": event_id, "segments": segments})

    def send(self, message_type: str, target_id: Any, segments: List[dict]) -> Any:
        return self.call(
            "host.message.send",
            {"messageType": message_type, "targetId": target_id, "segments": segments},
        )

    def bot_info(self) -> dict:
        return self.call("host.bot.info") or {}

    def command_list(self) -> List[dict]:
        return self.call("host.command.list") or []

    def config_get(self, scope: str, key: str) -> Any:
        response = self.call("host.config.get", {"scope": scope, "key": key}) or {}
        return response.get("value")

    def publish(self, name: str, payload: Optional[dict] = None) -> Any:
        return self.call("host.event.publish", {"name": name, "payload": payload or {}})

    def db(self, **kwargs: Any) -> Any:
        return self.call("host.db.call", kwargs)




class Peer:
    """Bidirectional JSON-RPC peer over NDJSON streams."""

    def __init__(self, plugin_id: str) -> None:
        self.plugin_id = plugin_id
        self._write_lock = threading.Lock()
        self._pending_lock = threading.Lock()
        self._pending: Dict[str, Dict[str, Any]] = {}
        self._handlers: Dict[str, Callable[[dict], Any]] = {}
        self._next_id = 0
        self._threads: List[threading.Thread] = []
        self._threads_lock = threading.Lock()
        self.on_close: Optional[Callable[[], None]] = None

    # -- transport
    def send(self, message: Dict[str, Any]) -> None:
        encoded = json.dumps(message, ensure_ascii=False, separators=(",", ":"))
        with self._write_lock:
            sys.stdout.write(encoded + "\n")
            sys.stdout.flush()

    def respond(self, request_id: Any, payload: Any) -> None:
        self.send({"jsonrpc": "2.0", "id": request_id, "result": payload})

    def fail(self, request_id: Any, code: int, message: str, data: Any = None) -> None:
        error: Dict[str, Any] = {"code": code, "message": message}
        if data is not None:
            error["data"] = data
        self.send({"jsonrpc": "2.0", "id": request_id, "error": error})

    def notify(self, method: str, params: Optional[dict] = None) -> None:
        message: Dict[str, Any] = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            message["params"] = params
        self.send(message)

    def call(self, method: str, params: Optional[dict] = None, timeout: float = 10.0) -> Any:
        self._next_id += 1
        request_id = self._next_id
        key = str(request_id)
        entry: Dict[str, Any] = {"event": threading.Event(), "response": None}
        with self._pending_lock:
            self._pending[key] = entry
        message: Dict[str, Any] = {"jsonrpc": "2.0", "id": request_id, "method": method}
        if params is not None:
            message["params"] = params
        self.send(message)
        if not entry["event"].wait(timeout):
            with self._pending_lock:
                self._pending.pop(key, None)
            raise RpcError(TIMEOUT, f"host call timed out: {method}")
        response = entry["response"] or {}
        if response.get("error"):
            error = response["error"]
            raise RpcError(
                int(error.get("code", INTERNAL_ERROR)),
                str(error.get("message", "host error")),
                error.get("data"),
            )
        return response.get("result")

    # -- dispatch
    def handle(self, method: str, handler: Callable[[dict], Any]) -> None:
        self._handlers[method] = handler

    def _deliver_response(self, message: Dict[str, Any]) -> None:
        key = str(message.get("id"))
        with self._pending_lock:
            entry = self._pending.pop(key, None)
        if entry is None:
            return
        entry["response"] = message
        entry["event"].set()

    def _run_handler(self, message: Dict[str, Any], handler: Callable[[dict], Any]) -> None:
        request_id = message.get("id")
        try:
            payload = handler(message.get("params") or {})
            if request_id is not None:
                self.respond(request_id, payload if payload is not None else {})
        except RpcError as error:
            if request_id is not None:
                self.fail(request_id, error.code, error.message, error.data)
        except Exception as error:  # noqa: BLE001 - protocol boundary
            traceback.print_exc(file=sys.stderr)
            if request_id is not None:
                self.fail(request_id, PLUGIN_ERROR, f"{type(error).__name__}: {error}")

    def run(self) -> None:
        for line in sys.stdin:
            line = line.strip()
            if not line:
                continue
            try:
                message = json.loads(line)
            except json.JSONDecodeError as error:
                self.fail(None, PARSE_ERROR, f"parse error: {error}")
                continue
            if not isinstance(message, dict):
                self.fail(None, INVALID_REQUEST, "frame is not a JSON object")
                continue
            method = message.get("method")
            if not method:
                self._deliver_response(message)
                continue
            if method == "rpc.cancel":
                continue
            handler = self._handlers.get(method)
            if handler is None:
                if message.get("id") is not None:
                    self.fail(message["id"], METHOD_NOT_FOUND, f"method not found: {method}")
                continue
            thread = threading.Thread(
                target=self._run_handler, args=(message, handler), daemon=True
            )
            with self._threads_lock:
                self._threads = [t for t in self._threads if t.is_alive()]
                self._threads.append(thread)
            thread.start()
        # stdin closed: give in-flight handlers a moment to write their
        # responses (the host closes stdin on shutdown, and killing the process
        # mid-response would look like a crash).
        self._join_handlers(2.0)
        if self.on_close:
            self.on_close()

    def _join_handlers(self, timeout: float) -> None:
        deadline = time.monotonic() + timeout
        with self._threads_lock:
            threads = list(self._threads)
        for thread in threads:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return
            thread.join(remaining)




class Plugin:
    """Plugin runtime: declares commands/events and serves the RPC loop."""

    def __init__(
        self,
        plugin_id: str,
        name: str = "",
        version: str = "1.0.0",
        api_version: str = API_VERSION,
        capabilities: Optional[List[str]] = None,
        config_schema: Optional[List[dict]] = None,
        events: Optional[List[str]] = None,
        stderr_log: bool = True,
    ) -> None:
        if not plugin_id:
            raise ValueError("plugin_id is required")
        self.plugin_id = plugin_id
        self.name = name or plugin_id
        self.version = version
        self.api_version = api_version
        self.capabilities = list(capabilities or [])
        self.config_schema = list(config_schema or [])
        self.stderr_log = stderr_log

        self.config: Dict[str, Any] = {}
        self.params: Dict[str, Any] = {}
        self.initialized = False

        self._commands: Dict[str, Callable[[CommandContext], Any]] = {}
        self._command_specs: List[dict] = []
        self._events: Dict[str, Callable[[EventContext], Any]] = {}
        self._declared_events: List[str] = list(events or [])
        self._custom: Dict[str, Callable[[dict], Any]] = {}

        self._on_init: Optional[Callable[[dict], None]] = None
        self._on_configure: Optional[Callable[[dict], None]] = None
        self._on_shutdown: Optional[Callable[[], None]] = None

        self._peer = Peer(plugin_id)
        self.peer = self._peer
        self.host = Host(self._peer)
        self._register_builtins()

    # -- registration
    def command(
        self, name: str, description: str = "", require_role: int = 0
    ) -> Callable[[Callable[[CommandContext], Any]], Callable[[CommandContext], Any]]:
        """Decorator: declare a command and route command.execute to it."""

        def decorator(fn: Callable[[CommandContext], Any]) -> Callable[[CommandContext], Any]:
            if name in self._commands:
                raise ValueError(f"command {name} is already registered")
            self._commands[name] = fn
            spec: Dict[str, Any] = {"command": name, "description": description or name}
            if require_role:
                spec["requireRole"] = int(require_role)
            self._command_specs.append(spec)
            return fn

        return decorator

    def on(
        self, event_name: str
    ) -> Callable[[Callable[[EventContext], Any]], Callable[[EventContext], Any]]:
        """Decorator: subscribe to one event kind ("*" for every event)."""

        def decorator(fn: Callable[[EventContext], Any]) -> Callable[[EventContext], Any]:
            if event_name in self._events:
                raise ValueError(f"event {event_name} is already registered")
            self._events[event_name] = fn
            if event_name == "*":
                self._declared_events = ["*"]
            elif "*" not in self._declared_events and event_name not in self._declared_events:
                self._declared_events.append(event_name)
            return fn

        return decorator

    def handle(self, method: str) -> Callable[[Callable[[dict], Any]], Callable[[dict], Any]]:
        """Decorator: register a raw RPC method (overrides the dispatchers)."""

        def decorator(fn: Callable[[dict], Any]) -> Callable[[dict], Any]:
            self._custom[method] = fn
            return fn

        return decorator

    def on_initialize(self, fn: Callable[[dict], None]) -> Callable[[dict], None]:
        self._on_init = fn
        return fn

    def on_configure(self, fn: Callable[[dict], None]) -> Callable[[dict], None]:
        self._on_configure = fn
        return fn

    def on_shutdown(self, fn: Callable[[], None]) -> Callable[[], None]:
        self._on_shutdown = fn
        return fn

    # -- logging (stderr only: stdout carries protocol frames)
    def log(self, message: str) -> None:
        if self.stderr_log:
            sys.stderr.write(f"[plugin:{self.plugin_id}] {message}\n")
            sys.stderr.flush()

    # -- builtins
    def _methods(self) -> List[str]:
        methods: List[str] = []
        if self._commands:
            methods.append("command.execute")
        if self._events:
            methods.append("event.handle")
        for method in self._custom:
            if method not in methods:
                methods.append(method)
        return methods

    def _apply_defaults(self, raw: Optional[dict]) -> Dict[str, Any]:
        config = dict(raw or {})
        for field in self.config_schema:
            key = field.get("key")
            value = config.get(key)
            if value is None:
                config[key] = field.get("default")
                continue
            kind = field.get("type")
            if kind == "boolean" and not isinstance(value, bool):
                if isinstance(value, str):
                    config[key] = value in ("true", "1")
                else:
                    config[key] = bool(value)
            elif kind == "number" and not isinstance(value, (int, float)):
                try:
                    config[key] = float(value)
                except (TypeError, ValueError):
                    config[key] = field.get("default")
            elif kind == "string" and not isinstance(value, str):
                config[key] = str(value)
        return config

    def _handle_initialize(self, params: dict) -> dict:
        protocol = params.get("protocolVersion") or PROTOCOL_VERSION
        if protocol != PROTOCOL_VERSION:
            raise RpcError(INVALID_PARAMS, f"unsupported protocol version {protocol}")
        api_version = params.get("apiVersion") or API_VERSION
        if api_version != self.api_version:
            raise RpcError(INVALID_PARAMS, f"unsupported api version {api_version}")

        self.params = params
        self.config = self._apply_defaults(params.get("config"))
        self.initialized = True
        if self._on_init:
            self._on_init(params)

        bot = params.get("bot") or {}
        self.log(f"initialized for bot {bot.get('botId')} ({bot.get('selfId')})")

        return {
            "id": self.plugin_id,
            "name": self.name,
            "version": self.version,
            "apiVersion": self.api_version,
            "methods": self._methods(),
            "capabilities": self.capabilities,
            "commands": self._command_specs,
            "events": self._declared_events,
            "configSchema": self.config_schema,
        }

    def _handle_health(self, _params: dict) -> dict:
        return {
            "status": "ok",
            "id": self.plugin_id,
            "version": self.version,
            "initialized": self.initialized,
            "pid": os.getpid(),
        }

    def _handle_shutdown(self, _params: dict) -> dict:
        self.log("shutdown requested")
        if self._on_shutdown:
            try:
                self._on_shutdown()
            except Exception:  # noqa: BLE001 - shutdown must not crash
                traceback.print_exc(file=sys.stderr)
        # Let the response frame reach the host before the process exits.
        threading.Timer(0.05, lambda: os._exit(0)).start()
        return {"status": "stopped"}

    def _handle_configure(self, params: dict) -> dict:
        self.config = self._apply_defaults(params.get("config"))
        if self._on_configure:
            self._on_configure(self.config)
        return {"status": "ok"}

    def _handle_command(self, params: dict) -> Any:
        name = params.get("command") or ""
        handler = self._commands.get(name)
        if handler is None:
            raise RpcError(METHOD_NOT_FOUND, f"command not registered: {name}")
        return handler(CommandContext(self.host, params)) or {}

    def _handle_event(self, params: dict) -> Any:
        event_name = params.get("eventName") or ""
        handler = self._events.get(event_name) or self._events.get("*")
        if handler is None:
            raise RpcError(METHOD_NOT_FOUND, f"no handler for event {event_name}")
        return handler(EventContext(self.host, params)) or {}

    def _register_builtins(self) -> None:
        self._peer.handle("plugin.initialize", self._handle_initialize)
        self._peer.handle("plugin.health", self._handle_health)
        self._peer.handle("plugin.shutdown", self._handle_shutdown)
        self._peer.handle("plugin.configure", self._handle_configure)

    def _attach_handlers(self) -> None:
        handlers: Dict[str, Callable[[dict], Any]] = {}
        if self._commands:
            handlers["command.execute"] = self._handle_command
        if self._events:
            handlers["event.handle"] = self._handle_event
        for method, fn in self._custom.items():
            handlers[method] = fn
        if "plugin.initialize" not in handlers:
            handlers["plugin.initialize"] = self._handle_initialize
        handlers["plugin.health"] = self._handle_health
        handlers["plugin.shutdown"] = self._handle_shutdown
        handlers["plugin.configure"] = self._handle_configure
        for method, fn in handlers.items():
            self._peer.handle(method, fn)

    # -- run
    def run(self) -> None:
        """Serve the protocol loop until stdin closes or the host stops us."""
        self._attach_handlers()
        self._peer.run()


__all__ = [
    "Plugin",
    "CommandContext",
    "EventContext",
    "Host",
    "Peer",
    "RpcError",
    "text",
    "at",
    "reply",
    "image",
    "raw",
    "result",
    "none",
    "API_VERSION",
    "PROTOCOL_VERSION",
    "PARSE_ERROR",
    "INVALID_REQUEST",
    "METHOD_NOT_FOUND",
    "INVALID_PARAMS",
    "INTERNAL_ERROR",
    "PLUGIN_ERROR",
    "TIMEOUT",
    "NOT_READY",
    "CAPABILITY_DENIED",
    "CANCELLED",
]
