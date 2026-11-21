package pluginsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/uniu-network/uniubot/pkg/pluginrpc"
)

// Host calls host services; the host enforces the plugin capabilities.
type Host struct {
	peer     *pluginrpc.Peer
	pluginID string
	// Timeout bounds one call, zero means 10s.
	Timeout time.Duration
}

func (h *Host) timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return ctx, func() {}
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

// Log is a notification: failures are silent, so logging never breaks logic.
func (h *Host) Log(level, format string, args ...any) {
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	_ = h.peer.Notify(pluginrpc.MethodLog, pluginrpc.LogParams{Level: level, Message: message})
}

func (h *Host) Logf(format string, args ...any) { h.Log("INFO", format, args...) }

type CommandInfo struct {
	Command     string `json:"command"`
	Description string `json:"description"`
	Scope       string `json:"scope,omitempty"`
	RequireRole int    `json:"requireRole"`
}

func (h *Host) CommandList(ctx context.Context) ([]CommandInfo, error) {
	callCtx, cancel := h.timeout(ctx)
	defer cancel()
	var out []CommandInfo
	err := h.peer.CallJSON(callCtx, pluginrpc.MethodCommandList, nil, &out)
	return out, err
}

func (h *Host) BotInfo(ctx context.Context) (pluginrpc.BotInfo, error) {
	callCtx, cancel := h.timeout(ctx)
	defer cancel()
	var out pluginrpc.BotInfo
	err := h.peer.CallJSON(callCtx, pluginrpc.MethodBotInfo, nil, &out)
	return out, err
}

// ConfigGet scopes: "plugin", "bot" (whitelisted keys), "global".
func (h *Host) ConfigGet(ctx context.Context, scope, key string) (any, error) {
	callCtx, cancel := h.timeout(ctx)
	defer cancel()
	var out struct {
		Value any `json:"value"`
	}
	err := h.peer.CallJSON(callCtx, pluginrpc.MethodConfigGet, pluginrpc.ConfigGetParams{Scope: scope, Key: key}, &out)
	return out.Value, err
}

func (h *Host) Reply(ctx context.Context, eventID string, segments []pluginrpc.Segment) error {
	callCtx, cancel := h.timeout(ctx)
	defer cancel()
	_, err := h.peer.Call(callCtx, pluginrpc.MethodMessageReply, pluginrpc.MessageParams{EventID: eventID, Segments: segments})
	return err
}

func (h *Host) Send(ctx context.Context, messageType string, targetID any, segments []pluginrpc.Segment) error {
	callCtx, cancel := h.timeout(ctx)
	defer cancel()
	_, err := h.peer.Call(callCtx, pluginrpc.MethodMessageSend, pluginrpc.MessageParams{
		MessageType: messageType,
		TargetID:    targetID,
		Segments:    segments,
	})
	return err
}

func (h *Host) Publish(ctx context.Context, name string, payload map[string]any) error {
	callCtx, cancel := h.timeout(ctx)
	defer cancel()
	_, err := h.peer.Call(callCtx, pluginrpc.MethodEventPublish, pluginrpc.EventPublishParams{Name: name, Payload: payload})
	return err
}

// DB tables are prefixed with the plugin namespace.
func (h *Host) DB(ctx context.Context, call pluginrpc.DBCallParams) (json.RawMessage, error) {
	callCtx, cancel := h.timeout(ctx)
	defer cancel()
	return h.peer.Call(callCtx, pluginrpc.MethodDBCall, call)
}

func (h *Host) DBInto(ctx context.Context, call pluginrpc.DBCallParams, out any) error {
	raw, err := h.DB(ctx, call)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (h *Host) CreateTable(ctx context.Context, table string, schema any) error {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	_, err = h.DB(ctx, pluginrpc.DBCallParams{Op: "createTable", Table: table, Schema: encoded})
	return err
}

func (h *Host) Ping(ctx context.Context) error {
	callCtx, cancel := h.timeout(ctx)
	defer cancel()
	_, err := h.peer.Call(callCtx, pluginrpc.MethodBotInfo, nil)
	return err
}
