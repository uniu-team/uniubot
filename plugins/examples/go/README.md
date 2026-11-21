# Example Go plugin

参考实现，演示 uniubot Go 插件的完整能力：

- `plugin.Command(...)` / `plugin.CommandAs(...)`：声明命令（等价 `ctx.bot.command`）；
- `plugin.On("message", ...)`：订阅事件（等价 `ctx.bot.on`）；
- `/go.echo`、`/go.ping`、`/go.count`、`/go.send`、`/go.sudo`（仅 BOT_ADMIN）、`/go.crash`；
- `host.bot.info`、`host.command.list`：调用宿主服务；
- `host.db.call`：使用作用域数据库（表名由宿主加前缀）；
- `host.log`：通过宿主日志输出。

## 构建与启用

```bash
# 在仓库根目录
go build -o plugins/examples/go/plugin ./plugins/examples/go
```

插件默认启用（`plugin.json` 中 `"enabled": true`）。可在 Bot 配置中覆盖：

```yaml
plugin_config:
  example-go:
    enabled: true
    greeting: "你好"
    verbose: false
```

## 验证

在聊天中发送：

```
/go.echo hello
/go.ping
/go.count
```

`/go.crash` 会让插件进程以退出码 3 退出，用于验证宿主的崩溃检测与重启策略（默认 2 次，指数退避）。

## 关键代码

```go
plugin := pluginsdk.New(pluginsdk.Meta{
    ID: "example-go", Name: "Example Go Plugin", Version: "1.0.0",
    Capabilities: []string{"logger", "message.reply", "db"},
})

plugin.Command("go.echo", "回显参数", func(ctx context.Context, req *pluginsdk.CommandRequest) (pluginrpc.HandlerResult, error) {
    return pluginsdk.Result(pluginsdk.SegmentText("hello")), nil
})

plugin.CommandAs("go.sudo", "仅 BOT_ADMIN", 3, func(ctx context.Context, req *pluginsdk.CommandRequest) (pluginrpc.HandlerResult, error) {
    return pluginsdk.Result(pluginsdk.SegmentText("ok")), nil
})

plugin.On("message", func(ctx context.Context, req *pluginsdk.EventRequest) (pluginrpc.HandlerResult, error) {
    return pluginsdk.HandlerResultNone(), nil
})

plugin.Serve()
```

参见 [docs/plugin-system.md](../../../docs/plugin-system.md) 与
[docs/plugin-protocol.md](../../../docs/plugin-protocol.md)。
