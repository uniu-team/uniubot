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
		ID:      "about",
		Name:    "about",
		Version: "1.0.0",
		Commands: []pluginrpc.CommandSpec{
			{Command: "about", Description: "获取关于信息"},
		},
		Capabilities: []string{"message.reply"},
	})

	hostVersion := "unknown"
	plugin.OnInitialize(func(ctx context.Context, params pluginrpc.InitializeParams) error {
		if params.Host.Version != "" {
			hostVersion = params.Host.Version
		}
		return nil
	})

	plugin.Handle("command.execute", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req pluginrpc.ExecuteParams
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, err.Error())
		}
		if req.Command != "about" {
			return nil, pluginrpc.NewError(pluginrpc.CodeMethodNotFound, "unknown command "+req.Command)
		}
		text := fmt.Sprintf("SkiBot v%s\nAuthor: @unify-z\nhttps://github.com/unify-z/skibot", hostVersion)
		return pluginrpc.HandlerResult{
			Segments: []pluginrpc.Segment{pluginsdk.SegmentText(text)},
			Finish:   true,
		}, nil
	})

	if err := plugin.Serve(); err != nil {
		plugin.LogError(err)
		os.Exit(1)
	}
}
