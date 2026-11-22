// Command about is the built-in /about command.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/uniu-network/uniubot/pkg/pluginrpc"
	"github.com/uniu-network/uniubot/pkg/pluginsdk"
)

func main() {
	plugin := pluginsdk.New(pluginsdk.Meta{
		ID:           "about",
		Name:         "about",
		Version:      "1.0.0",
		Capabilities: []string{"message.reply"},
	})

	hostVersion := "unknown"
	plugin.OnInitialize(func(ctx context.Context, params pluginrpc.InitializeParams) error {
		if params.Host.Version != "" {
			hostVersion = params.Host.Version
		}
		return nil
	})

	plugin.Command("about", "获取关于信息", func(ctx context.Context, req *pluginsdk.CommandRequest) (pluginrpc.HandlerResult, error) {
		text := fmt.Sprintf("SkiBot v%s\nAuthor: @unify-z\nhttps://github.com/unify-z/skibot", hostVersion)
		return pluginsdk.Result(pluginsdk.SegmentText(text)), nil
	})

	if err := plugin.Serve(); err != nil {
		plugin.LogError(err)
		os.Exit(1)
	}
}
