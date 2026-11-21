// Command help is the built-in /help command.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/uniu-network/uniubot/pkg/pluginrpc"
	"github.com/uniu-network/uniubot/pkg/pluginsdk"
)

func main() {
	plugin := pluginsdk.New(pluginsdk.Meta{
		ID:      "help",
		Name:    "help",
		Version: "1.0.0",
		Commands: []pluginrpc.CommandSpec{
			{Command: "help", Description: "获取帮助信息"},
		},
		Capabilities: []string{"command.list", "message.reply"},
	})

	var prefixes []string
	plugin.OnInitialize(func(ctx context.Context, params pluginrpc.InitializeParams) error {
		prefixes = params.Bot.Prefix
		if len(prefixes) == 0 {
			prefixes = []string{"/"}
		}
		return nil
	})

	plugin.Handle("command.execute", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req pluginrpc.ExecuteParams
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, err.Error())
		}
		if req.Command != "help" {
			return nil, pluginrpc.NewError(pluginrpc.CodeMethodNotFound, "unknown command "+req.Command)
		}
		commands, err := plugin.Host().CommandList(ctx)
		if err != nil {
			return nil, err
		}
		if len(prefixes) == 0 {
			prefixes = req.Bot.Prefix
		}
		var builder strings.Builder
		builder.WriteString("当前共有以下可用指令:\n")
		for _, command := range commands {
			prefix := prefixes[0]
			if len(prefixes) > 1 {
				prefix = "[" + strings.Join(prefixes, "/") + "]"
			}
			builder.WriteString(fmt.Sprintf("%s%s - %s\n", prefix, command.Command, command.Description))
		}
		return pluginrpc.HandlerResult{
			Segments: []pluginrpc.Segment{pluginsdk.SegmentText(builder.String())},
			Finish:   true,
		}, nil
	})

	if err := plugin.Serve(); err != nil {
		plugin.LogError(err)
		os.Exit(1)
	}
}
