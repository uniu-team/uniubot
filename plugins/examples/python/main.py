#!/usr/bin/env python3
"""Example Python plugin for uniubot.

Registration uses the same model as the Go SDK (see uniubot_plugin.py):

    @plugin.command("py.echo", "回显参数")
    @plugin.command("py.sudo", "...", require_role=3)
    @plugin.on("message")

Only the Python standard library is required, so the same file runs inside a
Conda environment without extra packages:

    conda env create -f environment.yml
    conda run -n uniubot-plugin-example python main.py

Standard output is reserved for protocol frames; diagnostics go to stderr.
"""

from __future__ import annotations

import json
import sys
import time

from uniubot_plugin import Plugin, none, result, text

COUNTER_SCHEMA = {
    "columns": {
        "key": {"type": "text", "primaryKey": True},
        "value": {"type": "integer", "notNull": True},
        "updated_at": {"type": "integer", "notNull": True},
    }
}

plugin = Plugin(
    plugin_id="example-python",
    name="Example Python Plugin",
    version="1.0.0",
    capabilities=[
        "logger",
        "config.read",
        "bot.info",
        "command.list",
        "message.reply",
        "db",
    ],
    config_schema=[
        {
            "key": "greeting",
            "type": "string",
            "default": "Hello from Python",
            "description": "echo 前缀",
        },
        {
            "key": "verbose",
            "type": "boolean",
            "default": False,
            "description": "记录更多日志",
        },
    ],
)


@plugin.on_initialize
def _on_initialize(params: dict) -> None:
    bot = params.get("bot") or {}
    plugin.log(f"initialized for bot {bot.get('botId')} ({bot.get('selfId')}), prefix={bot.get('prefix')}")


def increment_counter() -> int:
    host = plugin.host
    host.db(op="createTable", table="counters", schema=COUNTER_SCHEMA)
    row = host.db(op="selectOne", table="counters", where={"key": "commands"})
    value = 0
    if isinstance(row, dict) and row.get("value") is not None:
        value = int(row["value"])
    value += 1
    now = int(time.time() * 1000)
    if not row:
        host.db(
            op="insert",
            table="counters",
            data={"key": "commands", "value": value, "updated_at": now},
        )
    else:
        host.db(
            op="update",
            table="counters",
            data={"value": value, "updated_at": now},
            where={"key": "commands"},
        )
    return value




@plugin.command("py.echo", "回显参数 (Python 插件)")
def py_echo(ctx):
    greeting = plugin.config.get("greeting") or "Hello from Python"
    positional = ctx.args.get("_") or []
    if isinstance(positional, list):
        tail = " ".join(str(item) for item in positional)
    else:
        tail = str(positional)
    return result(text(f"{greeting}: {tail or 'world'}"))


@plugin.command("py.ping", "检查 Python 插件与宿主的连通性")
def py_ping(ctx):
    bot = ctx.host.bot_info()
    commands = ctx.host.command_list()
    body = "\n".join(
        [
            "pong (python)",
            f"bot: {bot.get('name')} ({bot.get('selfId')})",
            f"adapters: {', '.join(bot.get('adapters') or [])}",
            f"commands: {len(commands or [])}",
            f"time: {time.strftime('%H:%M:%S')}",
        ]
    )
    return result(text(body))


@plugin.command("py.count", "累加并显示 Python 插件的调用次数")
def py_count(ctx):
    return result(text(f"example-python 调用次数: {increment_counter()}"))


@plugin.command("py.send", "通过 host.message.reply 回复，而不是返回 segments")
def py_send(ctx):
    # Exercises the async reply path (eventId + message.reply capability).
    return ctx.reply(text("sent via host.message.reply"))


@plugin.command("py.sudo", "示例：仅 BOT_ADMIN 可用", require_role=3)
def py_sudo(ctx):
    return result(text("you are a bot admin"))




@plugin.on("message")
def on_message(ctx):
    if plugin.config.get("verbose"):
        event = ctx.event
        sender = event.get("sender") or {}
        plugin.host.info(
            "message from %s(%s) in group %s: %r"
            % (
                sender.get("nickname"),
                sender.get("user_id"),
                event.get("group_id"),
                event.get("raw_message"),
            )
        )
    # Returning none() keeps the plugin silent for ordinary chat.
    return none()


def main() -> int:
    try:
        plugin.run()
    except KeyboardInterrupt:
        return 0
    except Exception as error:  # noqa: BLE001 - top level guard
        plugin.log(f"fatal: {error}")
        import traceback

        traceback.print_exc(file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
