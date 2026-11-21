# Example Lua plugin

使用纯 Lua 解释器（不依赖 luarocks/C 模块）实现的 uniubot 插件。

注册方式与 Go/Python SDK 同构：

```lua
local uniubot = require("uniubot")
local plugin = uniubot.new{ id = "example-lua", name = "Example Lua Plugin" }

plugin:command("lua.echo", "回显参数", function(ctx)
  return uniubot.result(uniubot.text("hello"))
end)

plugin:command("lua.sudo", "仅 BOT_ADMIN", function(ctx)
  return uniubot.result(uniubot.text("ok"))
end, 3)

plugin:on("message", function(ctx)
  return uniubot.none()
end)

plugin:run()
```

演示内容：命令与角色声明、事件订阅、`host.bot.info` / `host.command.list` / `host.db.call` /
`host.log`、`host.message.reply` 异步回复。

SDK 位于同目录的 `uniubot.lua`（配合 `json.lua`），不依赖 luarocks 或 C 模块。

## 运行

```bash
lua main.lua
```

`plugin.json`：

```json
{"runtime": "lua", "command": "lua", "args": ["main.lua"]}
```

## 手工验证

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"plugin.initialize","params":{"protocolVersion":"1.0","apiVersion":"v1","pluginId":"example-lua","bot":{"botId":"b","selfId":1}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"plugin.health","params":{}}' \
  | lua main.lua
```

## 实现说明

- `json.lua` 是自带的 JSON 编解码器，数组用 `json.array{...}` 标记，解码出的数组带有同一标记。
- `methods` 由 SDK 根据注册情况生成；命令/事件分发器在 `plugin:run()` 时安装，
  因此注册顺序不影响结果。
- Lua 是单线程的：`call_host()` 在等待宿主响应时会阻塞读取 stdin，并在期间内联处理宿主同时发来的
  其他请求（例如 `plugin.health`），从而避免死锁。这要求宿主的 Host 处理器不会反向同步调用该插件
  （uniubot 的宿主满足这一点）。
- `stdout` 只输出协议帧，日志写 `stderr`。
