-- Example Lua plugin for uniubot: commands, events and host calls.
-- Run with a plain interpreter (no luarocks, no C modules): lua main.lua
-- stdout carries protocol frames, diagnostics go to stderr.

local uniubot = require("uniubot")

local COUNTER_SCHEMA = {
  columns = {
    key = { type = "text", primaryKey = true },
    value = { type = "integer", notNull = true },
    updated_at = { type = "integer", notNull = true },
  },
}

local plugin = uniubot.new({
  id = "example-lua",
  name = "Example Lua Plugin",
  version = "1.0.0",
  capabilities = {
    "logger", "config.read", "bot.info", "command.list", "message.reply", "db",
  },
  configSchema = {
    { key = "greeting", type = "string", default = "Hello from Lua", description = "echo 前缀" },
    { key = "verbose", type = "boolean", default = false, description = "记录更多日志" },
  },
})


plugin:command("lua.echo", "回显参数 (Lua 插件)", function(ctx)
  local greeting = plugin.config.greeting or "Hello from Lua"
  local words = {}
  for _, item in ipairs(ctx.args._ or {}) do
    words[#words + 1] = tostring(item)
  end
  local tail = table.concat(words, " ")
  return uniubot.result(uniubot.text(greeting .. ": " .. (tail ~= "" and tail or "world")))
end)

plugin:command("lua.ping", "检查 Lua 插件与宿主的连通性", function(ctx)
  local bot = ctx.host:bot_info() or {}
  local commands = ctx.host:command_list() or {}
  local adapters = {}
  for _, item in ipairs(bot.adapters or {}) do
    adapters[#adapters + 1] = tostring(item)
  end
  local body = table.concat({
    "pong (lua)",
    string.format("bot: %s (%s)", tostring(bot.name), tostring(bot.selfId)),
    "adapters: " .. table.concat(adapters, ", "),
    string.format("commands: %d", #commands),
    "time: " .. os.date("%H:%M:%S"),
  }, "\n")
  return uniubot.result(uniubot.text(body))
end)

local function increment_counter(ctx)
  local host = ctx.host
  host:db({ op = "createTable", table = "counters", schema = COUNTER_SCHEMA })
  local row = host:db({ op = "selectOne", table = "counters", where = { key = "commands" } })
  local value = 0
  if type(row) == "table" and row.value ~= nil then
    value = tonumber(row.value) or 0
  end
  value = value + 1
  local now = os.time() * 1000
  if type(row) ~= "table" or row.value == nil then
    host:db({
      op = "insert",
      table = "counters",
      data = { key = "commands", value = value, updated_at = now },
    })
  else
    host:db({
      op = "update",
      table = "counters",
      data = { value = value, updated_at = now },
      where = { key = "commands" },
    })
  end
  return value
end

plugin:command("lua.count", "累加并显示 Lua 插件的调用次数", function(ctx)
  return uniubot.result(uniubot.text(string.format("example-lua 调用次数: %d", increment_counter(ctx))))
end)

plugin:command("lua.send", "通过 host.message.reply 回复，而不是返回 segments", function(ctx)
  -- Exercises the async reply path (eventId + message.reply capability).
  return ctx:reply(uniubot.text("sent via host.message.reply"))
end)

plugin:command("lua.sudo", "示例：仅 BOT_ADMIN 可用", function(ctx)
  return uniubot.result(uniubot.text("you are a bot admin"))
end, 3)


plugin:on("message", function(ctx)
  if plugin.config.verbose then
    local event = ctx.event
    local sender = event.sender or {}
    ctx.host:info(string.format(
      "message from %s(%s) in group %s: %q",
      tostring(sender.nickname), tostring(sender.user_id),
      tostring(event.group_id), tostring(event.raw_message)
    ))
  end
  return uniubot.none()
end)

plugin:run()
