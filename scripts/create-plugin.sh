#!/usr/bin/env bash
# Scaffold a new uniubot plugin (Go, Python or Lua).
#
#   ./scripts/create-plugin.sh
#
# The script asks for the plugin id and runtime, then creates
# plugins/<id>/ with a manifest and a runnable skeleton. Go plugins receive a
# main.go; build it with `go build -o plugins/<id>/plugin ./plugins/<id>`.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

read -r -p "插件 ID (字母/数字/._-, 例如 my-plugin): " plugin_id
if [[ ! "$plugin_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
  echo "无效的插件 ID: $plugin_id" >&2
  exit 1
fi

read -r -p "插件名称: " plugin_name
read -r -p "插件描述: " plugin_description
read -r -p "运行时 [go/python/lua] (默认 go): " runtime
runtime="${runtime:-go}"

target="plugins/$plugin_id"
if [[ -e "$target" ]]; then
  echo "$target 已存在" >&2
  exit 1
fi
mkdir -p "$target"

case "$runtime" in
  go)
    command="./plugin"
    args='[]'
    mkdir -p "$target"
    cat > "$target/main.go" <<EOF
// Command $plugin_id is a uniubot plugin.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/uniu-network/uniubot/pkg/pluginrpc"
	"github.com/uniu-network/uniubot/pkg/pluginsdk"
)

func main() {
	plugin := pluginsdk.New(pluginsdk.Meta{
		ID:      "$plugin_id",
		Name:    "$plugin_name",
		Version: "1.0.0",
		Commands: []pluginrpc.CommandSpec{
			{Command: "$plugin_id.hello", Description: "示例指令"},
		},
		Capabilities: []string{"logger", "message.reply"},
	})

	plugin.Handle("command.execute", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req pluginrpc.ExecuteParams
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, err.Error())
		}
		return pluginrpc.HandlerResult{
			Segments: []pluginrpc.Segment{pluginsdk.SegmentText(fmt.Sprintf("hello from %s", req.Command))},
			Finish:   true,
		}, nil
	})

	if err := plugin.Serve(); err != nil {
		plugin.LogError(err)
		os.Exit(1)
	}
}
EOF
    ;;
  python)
    command="python3"
    args='["main.py"]'
    cat > "$target/main.py" <<EOF
#!/usr/bin/env python3
"""$plugin_name — uniubot plugin."""
import json
import sys

PLUGIN_ID = "$plugin_id"


def send(message):
    message["jsonrpc"] = "2.0"
    sys.stdout.write(json.dumps(message, ensure_ascii=False) + "\n")
    sys.stdout.flush()


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    message = json.loads(line)
    method = message.get("method")
    request_id = message.get("id")
    if method == "plugin.initialize":
        send({"id": request_id, "result": {
            "id": PLUGIN_ID,
            "name": "$plugin_name",
            "version": "1.0.0",
            "apiVersion": "v1",
            "methods": ["command.execute"],
            "capabilities": ["logger", "message.reply"],
            "commands": [{"command": "$plugin_id.hello", "description": "示例指令"}],
        }})
    elif method == "plugin.health":
        send({"id": request_id, "result": {"status": "ok"}})
    elif method == "plugin.shutdown":
        send({"id": request_id, "result": {"status": "stopped"}})
        break
    elif method == "command.execute":
        send({"id": request_id, "result": {
            "segments": [{"type": "text", "data": {"text": "hello from $PLUGIN_ID"}}],
            "finish": True,
        }})
    elif request_id is not None:
        send({"id": request_id, "error": {"code": -32601, "message": "method not found"}})
EOF
    ;;
  lua)
    command="lua"
    args='["main.lua"]'
    cat > "$target/main.lua" <<EOF
-- $plugin_name — uniubot plugin.
package.path = "./?.lua;" .. package.path
local json = require("json")

local function send(message)
  message.jsonrpc = "2.0"
  io.stdout:write(json.encode(message) .. "\n")
  io.stdout:flush()
end

for line in io.lines() do
  local message = json.decode(line)
  local method, id = message.method, message.id
  if method == "plugin.initialize" then
    send({ id = id, result = {
      id = "$plugin_id", name = "$plugin_name", version = "1.0.0", apiVersion = "v1",
      methods = json.array({ "command.execute" }),
      capabilities = json.array({ "logger", "message.reply" }),
      commands = json.array({ { command = "$plugin_id.hello", description = "示例指令" } }),
    } })
  elseif method == "plugin.health" then
    send({ id = id, result = { status = "ok" } })
  elseif method == "plugin.shutdown" then
    send({ id = id, result = { status = "stopped" } })
    os.exit(0)
  elseif method == "command.execute" then
    send({ id = id, result = {
      segments = json.array({ { type = "text", data = { text = "hello from $plugin_id" } } }),
      finish = true,
    } })
  elseif id ~= nil then
    send({ id = id, error = { code = -32601, message = "method not found" } })
  end
end
EOF
    cp plugins/examples/lua/json.lua "$target/json.lua"
    ;;
  *)
    echo "未知运行时: $runtime" >&2
    exit 1
    ;;
esac

cat > "$target/plugin.json" <<EOF
{
  "id": "$plugin_id",
  "name": "$plugin_name",
  "version": "1.0.0",
  "description": "$plugin_description",
  "author": "",
  "protocol": "jsonrpc-2.0-ndjson",
  "apiVersion": "v1",
  "runtime": "$runtime",
  "command": "$command",
  "args": $args,
  "enabled": true,
  "capabilities": ["logger", "message.reply"],
  "concurrency": 4,
  "timeoutMs": 5000
}
EOF

if [[ "$runtime" == "go" ]]; then
  echo "已创建 $target，构建插件：go build -o $target/plugin ./$target"
else
  echo "已创建 $target（运行时 $runtime）"
fi
