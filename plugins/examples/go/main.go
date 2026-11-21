// Command example-go is the reference plugin: Commands, events, host calls,
// scoped storage and hot-reloaded configuration.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/uniu-network/uniubot/pkg/pluginrpc"
	"github.com/uniu-network/uniubot/pkg/pluginsdk"
)

var counterSchema = map[string]any{
	"columns": map[string]any{
		"key":        map[string]any{"type": "text", "primaryKey": true},
		"value":      map[string]any{"type": "integer", "notNull": true},
		"updated_at": map[string]any{"type": "integer", "notNull": true},
	},
}

type event struct {
	RawMessage string `json:"raw_message"`
	GroupID    int64  `json:"group_id"`
	Sender     struct {
		UserID   int64  `json:"user_id"`
		Nickname string `json:"nickname"`
	} `json:"sender"`
}

func main() {
	plugin := pluginsdk.New(pluginsdk.Meta{
		ID:         "example-go",
		Name:       "Example Go Plugin",
		Version:    "1.0.0",
		APIVersion: pluginrpc.APIVersion,
		Capabilities: []string{
			"logger", "config.read", "bot.info", "command.list", "message.reply", "db",
		},
		ConfigSchema: []pluginrpc.ConfigField{
			{Key: "greeting", Type: "string", Default: "Hello from Go", Description: "echo 前缀"},
			{Key: "verbose", Type: "boolean", Default: false, Description: "记录更多日志"},
		},
	})

	plugin.OnInitialize(func(ctx context.Context, params pluginrpc.InitializeParams) error {
		plugin.Logf("initialized for bot %s (%d), prefix=%v",
			params.Bot.BotID, params.Bot.SelfID, params.Bot.Prefix)
		return nil
	})

	plugin.Command("go.echo", "回显参数 (Go 插件)", func(ctx context.Context, req *pluginsdk.CommandRequest) (pluginrpc.HandlerResult, error) {
		return pluginsdk.Result(pluginsdk.SegmentText(echoText(plugin, req.Args))), nil
	})

	plugin.Command("go.ping", "检查 Go 插件与宿主的连通性", func(ctx context.Context, req *pluginsdk.CommandRequest) (pluginrpc.HandlerResult, error) {
		bot, err := plugin.Host().BotInfo(ctx)
		if err != nil {
			return pluginrpc.HandlerResult{}, err
		}
		commands, err := plugin.Host().CommandList(ctx)
		if err != nil {
			return pluginrpc.HandlerResult{}, err
		}
		text := fmt.Sprintf("pong\nbot: %s (%d)\nadapters: %s\ncommands: %d\ntime: %s",
			bot.Name, bot.SelfID, strings.Join(bot.Adapters, ", "), len(commands),
			time.Now().Format("15:04:05"))
		return pluginsdk.Result(pluginsdk.SegmentText(text)), nil
	})

	plugin.Command("go.count", "累加并显示 Go 插件的调用次数", func(ctx context.Context, req *pluginsdk.CommandRequest) (pluginrpc.HandlerResult, error) {
		count, err := incrementCounter(ctx, plugin)
		if err != nil {
			return pluginrpc.HandlerResult{}, err
		}
		return pluginsdk.Result(pluginsdk.SegmentText(fmt.Sprintf("example-go 调用次数: %d", count))), nil
	})

	// Replies through the host RPC, which needs the message.reply capability.
	plugin.Command("go.send", "通过 host.message.reply 回复，而不是返回 segments", func(ctx context.Context, req *pluginsdk.CommandRequest) (pluginrpc.HandlerResult, error) {
		if err := plugin.Host().Reply(ctx, req.EventID, []pluginrpc.Segment{
			pluginsdk.SegmentText("sent via host.message.reply"),
		}); err != nil {
			return pluginrpc.HandlerResult{}, err
		}
		return pluginsdk.HandlerResultNone(), nil
	})

	plugin.CommandAs("go.sudo", "示例：仅 BOT_ADMIN 可用", 3, func(ctx context.Context, req *pluginsdk.CommandRequest) (pluginrpc.HandlerResult, error) {
		return pluginsdk.Result(pluginsdk.SegmentText("you are a bot admin")), nil
	})

	plugin.Command("go.crash", "测试用：立即退出进程以验证宿主崩溃处理", func(ctx context.Context, req *pluginsdk.CommandRequest) (pluginrpc.HandlerResult, error) {
		plugin.Logf("crash requested, exiting")
		os.Exit(3)
		return pluginrpc.HandlerResult{}, nil
	})

	plugin.On("message", func(ctx context.Context, req *pluginsdk.EventRequest) (pluginrpc.HandlerResult, error) {
		var payload event
		if err := json.Unmarshal(req.Event, &payload); err != nil {
			plugin.Logf("cannot decode message event: %v", err)
			return pluginsdk.HandlerResultNone(), nil
		}
		if verbose, _ := plugin.Config()["verbose"].(bool); verbose {
			plugin.Host().Log("INFO", "message from %s(%d) in group %d: %q",
				payload.Sender.Nickname, payload.Sender.UserID, payload.GroupID, payload.RawMessage)
		}
		return pluginsdk.HandlerResultNone(), nil
	})

	if err := plugin.Serve(); err != nil {
		plugin.LogError(err)
		os.Exit(1)
	}
}

func echoText(plugin *pluginsdk.Plugin, args map[string]any) string {
	greeting, _ := plugin.Config()["greeting"].(string)
	if greeting == "" {
		greeting = "Hello from Go"
	}
	positional := ""
	switch typed := args["_"].(type) {
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, fmt.Sprint(item))
		}
		positional = strings.Join(parts, " ")
	case []string:
		positional = strings.Join(typed, " ")
	}
	if positional == "" {
		positional = "world"
	}
	return fmt.Sprintf("%s: %s", greeting, positional)
}

func incrementCounter(ctx context.Context, plugin *pluginsdk.Plugin) (int, error) {
	schema, err := json.Marshal(counterSchema)
	if err != nil {
		return 0, err
	}
	if _, err := plugin.Host().DB(ctx, pluginrpc.DBCallParams{
		Op:     "createTable",
		Table:  "counters",
		Schema: schema,
	}); err != nil {
		return 0, err
	}
	var row map[string]any
	if err := plugin.Host().DBInto(ctx, pluginrpc.DBCallParams{
		Op:    "selectOne",
		Table: "counters",
		Where: map[string]any{"key": "commands"},
	}, &row); err != nil {
		return 0, err
	}
	value := 0
	if row != nil {
		if number, ok := row["value"].(float64); ok {
			value = int(number)
		}
	}
	value++
	if row == nil {
		if _, err := plugin.Host().DB(ctx, pluginrpc.DBCallParams{
			Op:    "insert",
			Table: "counters",
			Data: map[string]any{
				"key":        "commands",
				"value":      value,
				"updated_at": time.Now().UnixMilli(),
			},
		}); err != nil {
			return 0, err
		}
		return value, nil
	}
	if _, err := plugin.Host().DB(ctx, pluginrpc.DBCallParams{
		Op:    "update",
		Table: "counters",
		Data: map[string]any{
			"value":      value,
			"updated_at": time.Now().UnixMilli(),
		},
		Where: map[string]any{"key": "commands"},
	}); err != nil {
		return 0, err
	}
	return value, nil
}
