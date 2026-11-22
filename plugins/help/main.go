// Command help is the built-in /help command.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/uniu-network/uniubot/pkg/pluginrpc"
	"github.com/uniu-network/uniubot/pkg/pluginsdk"
)

func main() {
	plugin := pluginsdk.New(pluginsdk.Meta{
		ID:           "help",
		Name:         "help",
		Version:      "1.0.0",
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

	plugin.Command("help", "获取帮助信息", func(ctx context.Context, req *pluginsdk.CommandRequest) (pluginrpc.HandlerResult, error) {
		commands, err := plugin.Host().CommandList(ctx)
		if err != nil {
			return pluginrpc.HandlerResult{}, err
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
		return pluginsdk.Result(pluginsdk.SegmentText(builder.String())), nil
	})

	if err := plugin.Serve(); err != nil {
		plugin.LogError(err)
		os.Exit(1)
	}
}
