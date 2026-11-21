# Example Python plugin

只使用 Python 标准库实现的 uniubot 插件，注册方式与 Go SDK 同构：

```python
@plugin.command("py.echo", "回显参数")
def py_echo(ctx):
    return result(text("hello"))

@plugin.command("py.sudo", "仅 BOT_ADMIN", require_role=3)
def py_sudo(ctx):
    return result(text("ok"))

@plugin.on("message")
def on_message(ctx):
    return none()
```

演示内容：命令与角色声明、事件订阅、`host.bot.info` / `host.command.list` / `host.db.call` /
`host.log`、`plugin.configure` 配置热更新、`plugin.shutdown` 优雅退出、`host.message.reply`
异步回复。

SDK 位于同目录的 `uniubot_plugin.py`，**仅依赖标准库**，复制到自己插件的目录即可使用。

协议：JSON-RPC 2.0，NDJSON（每行一条消息）走 stdin/stdout；日志只写 stderr。

## 直接使用解释器（默认）

```bash
python3 main.py          # 手工验证：可直接粘贴一行 JSON 到 stdin
```

`plugin.json` 已配置为：

```json
{"runtime": "python", "command": "python3", "args": ["main.py"]}
```

## 使用 Conda 环境

```bash
conda env create -f environment.yml
conda activate uniubot-plugin-example
python main.py
```

让宿主通过 Conda 启动插件，把 `plugin.json` 替换为 `plugin.conda.json`：

```bash
cp plugin.conda.json plugin.json
```

它使用：

```json
{"command": "conda",
 "args": ["run", "-n", "uniubot-plugin-example", "--no-capture-output", "python", "main.py"]}
```

> Go 主程序本身**不需要**运行在 Conda 环境中，Conda 只服务于这个插件进程。

## 手工验证握手

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"plugin.initialize","params":{"protocolVersion":"1.0","apiVersion":"v1","pluginId":"example-python","bot":{"botId":"b","selfId":1},"config":{}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"plugin.health","params":{}}' \
  | python3 main.py
```

预期两行 JSON 响应（`result` 中包含 `commands` / `configSchema` / `status: ok`）。

## 结构

| 方法 | 说明 |
| --- | --- |
| `plugin.initialize` | 校验协议版本，返回注册信息 |
| `plugin.health` | 健康检查 |
| `plugin.configure` | 应用新配置（带默认值） |
| `plugin.shutdown` | 返回响应后退出进程 |
| `command.execute` | 按命令名分发到 `@plugin.command` 注册的函数 |
| `event.handle` | 按事件名分发到 `@plugin.on` 注册的函数（`"*"` 为通配） |

`methods` 由 SDK 根据注册情况生成：只注册命令的插件不会声明 `event.handle`，
宿主因此不会向它投递事件。

`Peer.call()` 用于调用宿主：它发送带自增 id 的请求，并在独立线程中等待匹配的响应，
因此可以同时处理多个请求。
