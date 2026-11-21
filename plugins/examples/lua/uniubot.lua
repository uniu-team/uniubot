-- uniubot.lua - Lua SDK for uniubot plugins (same model as the Go/Python SDKs).
--   local plugin = uniubot.new{ id = "my-plugin", capabilities = {"message.reply"} }
--   plugin:command("my.echo", "echo", function(ctx) return uniubot.result(...) end)
--   plugin:on("message", function(ctx) return uniubot.none() end)
--   plugin:run()
-- Single-threaded: a handler waiting for a host reply keeps serving inline
-- requests, so the two sides cannot deadlock.

local json = require("json")

local uniubot = {}

uniubot.PROTOCOL_VERSION = "1.0"
uniubot.API_VERSION = "v1"

-- JSON-RPC error codes (see docs/plugin-protocol.md).
uniubot.PARSE_ERROR = -32700
uniubot.INVALID_REQUEST = -32600
uniubot.METHOD_NOT_FOUND = -32601
uniubot.INVALID_PARAMS = -32602
uniubot.INTERNAL_ERROR = -32603
uniubot.PLUGIN_ERROR = -32000
uniubot.TIMEOUT = -32001
uniubot.CAPABILITY_DENIED = -32004


function uniubot.text(value)
  return { type = "text", data = { text = tostring(value) } }
end

function uniubot.at(user_id)
  return { type = "at", data = { user_id = tostring(user_id) } }
end

function uniubot.reply(message_id)
  return { type = "reply", data = { id = message_id } }
end

function uniubot.image(file)
  return { type = "image", data = { file = file } }
end

function uniubot.raw(platform, segment_type, data)
  return { type = "raw", data = { platform = platform, type = segment_type, data = data or {} } }
end

-- Build a handler result that the host sends as the reply.
function uniubot.result(...)
  return { segments = json.array({ ... }), finish = true }
end

-- Build a handler result that produces no reply.
function uniubot.none()
  return {}
end


local Host = {}
Host.__index = Host

function Host:call(method, params, timeout_seconds)
  return self._peer:call(method, params, timeout_seconds or self.timeout)
end

function Host:log(level, message)
  self._peer:notify("host.log", { level = level, message = message })
end

function Host:info(message)
  self:log("INFO", message)
end

function Host:warn(message)
  self:log("WARN", message)
end

function Host:error(message)
  self:log("ERROR", message)
end

function Host:bot_info()
  return self:call("host.bot.info")
end

function Host:command_list()
  return self:call("host.command.list")
end

function Host:config_get(scope, key)
  local response = self:call("host.config.get", { scope = scope, key = key })
  return response and response.value
end

function Host:reply(event_id, segments)
  return self:call("host.message.reply", { eventId = event_id, segments = json.array(segments) })
end

function Host:send(message_type, target_id, segments)
  return self:call("host.message.send", {
    messageType = message_type,
    targetId = target_id,
    segments = json.array(segments),
  })
end

function Host:publish(name, payload)
  return self:call("host.event.publish", { name = name, payload = payload or {} })
end

function Host:db(call)
  return self:call("host.db.call", call)
end


local Context = {}
Context.__index = Context

function Context:reply(...)
  self.host:reply(self.event_id, { ... })
  return uniubot.none()
end


local Peer = {}
Peer.__index = Peer

function Peer.new(plugin_id)
  return setmetatable({
    plugin_id = plugin_id,
    handlers = {},
    next_id = 0,
    pending = {},
    host = nil,
  }, Peer)
end

function Peer:send(message)
  message.jsonrpc = "2.0"
  io.stdout:write(json.encode(message) .. "\n")
  io.stdout:flush()
end

function Peer:respond(id, payload)
  self:send({ id = id, result = payload or {} })
end

function Peer:fail(id, code, message)
  self:send({ id = id, error = { code = code, message = message } })
end

function Peer:notify(method, params)
  local message = { method = method }
  if params then
    message.params = params
  end
  self:send(message)
end

-- Dispatch one inbound request. Returns true if it was handled.
function Peer:dispatch(message)
  local method, id = message.method, message.id
  if not method then
    return false -- a response, consumed by call()
  end
  if method == "rpc.cancel" then
    return true
  end
  local handler = self.handlers[method]
  if not handler then
    if id ~= nil then
      self:fail(id, uniubot.METHOD_NOT_FOUND, "method not found: " .. tostring(method))
    end
    return true
  end
  local ok, payload = pcall(handler, message.params or {})
  if id == nil then
    return true
  end
  if ok then
    self:respond(id, payload)
  else
    self:fail(id, uniubot.PLUGIN_ERROR, tostring(payload))
  end
  return true
end

-- Wait for the response to `id`, handling interleaved host requests inline so
-- that a plugin calling the host from inside an event handler cannot deadlock.
function Peer:call(method, params, timeout_seconds)
  self.next_id = self.next_id + 1
  local id = self.next_id
  local message = { id = id, method = method }
  if params then
    message.params = params
  end
  self:send(message)

  local deadline = os.time() + (timeout_seconds or 10)
  while true do
    local line = io.read("*l")
    if not line then
      error("host connection closed while waiting for " .. tostring(method))
    end
    line = line:gsub("\r$", "")
    if line ~= "" then
      local ok, decoded = pcall(json.decode, line)
      if not ok then
        io.stderr:write("[plugin] invalid JSON frame: " .. tostring(decoded) .. "\n")
      elseif type(decoded) == "table" then
        if decoded.id == id and decoded.method == nil then
          if decoded.error then
            error(string.format("host error %s: %s", tostring(decoded.error.code), tostring(decoded.error.message)))
          end
          return decoded.result
        elseif decoded.method then
          self:dispatch(decoded)
        end
      end
    end
    if os.time() > deadline then
      error("host call timed out: " .. tostring(method))
    end
  end
end


local Plugin = {}
Plugin.__index = Plugin

function uniubot.new(spec)
  spec = spec or {}
  if not spec.id then
    error("uniubot.new: id is required")
  end
  local plugin = setmetatable({
    id = spec.id,
    name = spec.name or spec.id,
    version = spec.version or "1.0.0",
    api_version = spec.apiVersion or uniubot.API_VERSION,
    capabilities = spec.capabilities or {},
    config_schema = spec.configSchema or {},
    declared_events = spec.events or {},
    config = {},
    params = {},
    initialized = false,
    commands = {},
    command_specs = {},
    events = {},
    custom = {},
  }, Plugin)

  plugin.peer = Peer.new(plugin.id)
  plugin.host = setmetatable({ _peer = plugin.peer, timeout = 10 }, Host)
  plugin.peer.host = plugin.host
  plugin:_register_builtins()
  return plugin
end

function Plugin:log(message)
  io.stderr:write(string.format("[plugin:%s] %s\n", self.id, tostring(message)))
  io.stderr:flush()
end

function Plugin:command(name, description, fn, require_role)
  if type(name) ~= "string" or name == "" then
    error("uniubot: command name must be a non-empty string")
  end
  if type(fn) ~= "function" then
    error("uniubot: command " .. name .. " needs a handler function")
  end
  if self.commands[name] then
    error("command " .. tostring(name) .. " is already registered")
  end
  self.commands[name] = fn
  local spec = { command = name, description = description or name }
  if require_role and require_role ~= 0 then
    spec.requireRole = require_role
  end
  self.command_specs[#self.command_specs + 1] = spec
end

function Plugin:on(event_name, fn)
  if self.events[event_name] then
    error("event " .. tostring(event_name) .. " is already registered")
  end
  self.events[event_name] = fn
  if event_name == "*" then
    self.declared_events = { "*" }
    return
  end
  for _, existing in ipairs(self.declared_events) do
    if existing == "*" or existing == event_name then
      return
    end
  end
  self.declared_events[#self.declared_events + 1] = event_name
end

function Plugin:handle(method, fn)
  self.custom[method] = fn
end

function Plugin:_apply_defaults(raw)
  local config = {}
  for key, value in pairs(raw or {}) do
    config[key] = value
  end
  for _, field in ipairs(self.config_schema) do
    local value = config[field.key]
    if value == nil then
      config[field.key] = field.default
    elseif field.type == "boolean" and type(value) ~= "boolean" then
      config[field.key] = (value == true or value == "true" or value == "1")
    elseif field.type == "number" and type(value) ~= "number" then
      config[field.key] = tonumber(value) or field.default
    elseif field.type == "string" and type(value) ~= "string" then
      config[field.key] = tostring(value)
    end
  end
  return config
end

function Plugin:_methods()
  local methods = {}
  if next(self.commands) ~= nil then
    methods[#methods + 1] = "command.execute"
  end
  if next(self.events) ~= nil then
    methods[#methods + 1] = "event.handle"
  end
  for method in pairs(self.custom) do
    local exists = false
    for _, candidate in ipairs(methods) do
      if candidate == method then
        exists = true
      end
    end
    if not exists then
      methods[#methods + 1] = method
    end
  end
  return methods
end

function Plugin:_register_builtins()
  local peer = self.peer

  peer.handlers["plugin.initialize"] = function(params)
    local protocol = params.protocolVersion or uniubot.PROTOCOL_VERSION
    if protocol ~= uniubot.PROTOCOL_VERSION then
      error("unsupported protocol version " .. tostring(protocol))
    end
    local api_version = params.apiVersion or uniubot.API_VERSION
    if api_version ~= self.api_version then
      error("unsupported api version " .. tostring(api_version))
    end
    self.params = params
    self.config = self:_apply_defaults(params.config)
    self.initialized = true
    local bot = params.bot or {}
    self:log(string.format("initialized for bot %s (%s)", tostring(bot.botId), tostring(bot.selfId)))
    return {
      id = self.id,
      name = self.name,
      version = self.version,
      apiVersion = self.api_version,
      methods = json.array(self:_methods()),
      capabilities = json.array(self.capabilities),
      commands = json.array(self.command_specs),
      events = json.array(self.declared_events),
      configSchema = json.array(self.config_schema),
    }
  end

  peer.handlers["plugin.health"] = function()
    return { status = "ok", id = self.id, version = self.version, initialized = self.initialized }
  end

  peer.handlers["plugin.shutdown"] = function()
    self:log("shutdown requested")
    return { status = "stopped" }
  end

  peer.handlers["plugin.configure"] = function(params)
    self.config = self:_apply_defaults(params.config)
    return { status = "ok" }
  end

end

function Plugin:run()
  -- Dispatchers are installed here, not in the constructor: commands and
  -- events are declared after uniubot.new() returns.
  if next(self.commands) ~= nil then
    self.peer.handlers["command.execute"] = function(params)
      local name = params.command or ""
      local handler = self.commands[name]
      if not handler then
        error("command not registered: " .. tostring(name))
      end
      local ctx = setmetatable({
        host = self.host,
        command = name,
        args = params.args or { _ = {} },
        event = params.event or {},
        event_id = params.eventId or "",
        bot = params.bot or {},
        adapters = params.adapters or {},
      }, Context)
      return handler(ctx) or {}
    end
  end

  if next(self.events) ~= nil then
    self.peer.handlers["event.handle"] = function(params)
      local event_name = params.eventName or ""
      local handler = self.events[event_name] or self.events["*"]
      if not handler then
        error("no handler for event " .. tostring(event_name))
      end
      local ctx = setmetatable({
        host = self.host,
        event_name = event_name,
        event = params.event or {},
        event_id = params.eventId or "",
        bot = params.bot or {},
      }, Context)
      return handler(ctx) or {}
    end
  end

  -- Raw handlers registered with Handle() win over the generated ones.
  for method, fn in pairs(self.custom) do
    self.peer.handlers[method] = fn
  end

  while true do
    local line = io.read("*l")
    if not line then
      break
    end
    line = line:gsub("\r$", "")
    if line ~= "" then
      local ok, message = pcall(json.decode, line)
      if not ok then
        self.peer:fail(nil, uniubot.PARSE_ERROR, "parse error: " .. tostring(message))
      elseif type(message) ~= "table" then
        self.peer:fail(nil, uniubot.INVALID_REQUEST, "frame is not a JSON object")
      else
        self.peer:dispatch(message)
      end
    end
  end
  self:log("stdin closed, exiting")
end

return uniubot
