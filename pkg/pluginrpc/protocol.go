// Package pluginrpc is the plugin protocol: JSON-RPC 2.0 framed as NDJSON over a
// stream pair. The protocol is symmetric, so host and plugin share one Peer.
package pluginrpc

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	ProtocolVersion       = "1.0"
	APIVersion            = "v1"
	Transport             = "jsonrpc-2.0-ndjson"
	DefaultMaxMessageSize = 8 << 20
	DefaultMaxInflight    = 64
)

const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

const (
	CodePluginError      = -32000
	CodeTimeout          = -32001
	CodeUnauthorized     = -32002
	CodeNotReady         = -32003
	CodeCapabilityDenied = -32004
	CodeCancelled        = -32005
	CodePayloadTooLarge  = -32006
)

const (
	MethodInitialize  = "plugin.initialize"
	MethodInitialized = "plugin.initialized"
	MethodShutdown    = "plugin.shutdown"
	MethodHealth      = "plugin.health"
	MethodConfigure   = "plugin.configure"
	MethodCommand     = "command.execute"
	MethodEvent       = "event.handle"

	MethodLog          = "host.log"
	MethodConfigGet    = "host.config.get"
	MethodCommandList  = "host.command.list"
	MethodBotInfo      = "host.bot.info"
	MethodMessageReply = "host.message.reply"
	MethodMessageSend  = "host.message.send"
	MethodEventPublish = "host.event.publish"
	MethodDBCall       = "host.db.call"

	NotificationCancel = "rpc.cancel"
)

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Data != nil {
		return fmt.Sprintf("%s (code %d): %v", e.Message, e.Code, e.Data)
	}
	return fmt.Sprintf("%s (code %d)", e.Message, e.Code)
}

func NewError(code int, message string, data ...any) *Error {
	err := &Error{Code: code, Message: message}
	if len(data) > 0 {
		err.Data = data[0]
	}
	return err
}

func AsError(err error) *Error {
	var rpcErr *Error
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	return nil
}

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

func (m *Message) IsRequest() bool { return m.Method != "" }

func (m *Message) IsNotification() bool {
	return m.Method != "" && (len(m.ID) == 0 || string(m.ID) == "null")
}

func (m *Message) HasID() bool { return len(m.ID) > 0 && string(m.ID) != "null" }

type BotInfo struct {
	BotID    string   `json:"botId"`
	SelfID   int64    `json:"selfId"`
	Name     string   `json:"name"`
	Prefix   []string `json:"prefix"`
	Adapters []string `json:"adapters"`
}

type HostInfo struct {
	Version      string `json:"version"`
	Protocol     string `json:"protocol"`
	PID          int    `json:"pid"`
	Platform     string `json:"platform"`
	HostAPILevel string `json:"apiVersion"`
}

type CommandSpec struct {
	Command     string `json:"command"`
	Description string `json:"description"`
	RequireRole int    `json:"requireRole,omitempty"`
}

type ConfigField struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Default     any    `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	APIVersion      string         `json:"apiVersion"`
	PluginID        string         `json:"pluginId"`
	Bot             BotInfo        `json:"bot"`
	Host            HostInfo       `json:"host"`
	Config          map[string]any `json:"config"`
	WorkDir         string         `json:"workDir"`
	Capabilities    []string       `json:"capabilities"`
}

// InitializeResult is authoritative: the host validates and registers what the
// plugin declares here.
type InitializeResult struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Version      string        `json:"version"`
	APIVersion   string        `json:"apiVersion"`
	Methods      []string      `json:"methods"`
	Capabilities []string      `json:"capabilities"`
	Commands     []CommandSpec `json:"commands"`
	Events       []string      `json:"events"`
	ConfigSchema []ConfigField `json:"configSchema"`
}

type Segment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data,omitempty"`
}

type ExecuteParams struct {
	Command  string          `json:"command"`
	Args     map[string]any  `json:"args"`
	Event    json.RawMessage `json:"event"`
	EventID  string          `json:"eventId"`
	Bot      BotInfo         `json:"bot"`
	Adapters []string        `json:"adapters"`
}

type EventParams struct {
	EventName string          `json:"eventName"`
	Event     json.RawMessage `json:"event"`
	EventID   string          `json:"eventId"`
	Bot       BotInfo         `json:"bot"`
}

type HandlerResult struct {
	Segments []Segment `json:"segments,omitempty"`
	Finish   bool      `json:"finish,omitempty"`
}

type ConfigGetParams struct {
	Scope string `json:"scope"`
	Key   string `json:"key"`
}

type LogParams struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type MessageParams struct {
	EventID     string    `json:"eventId,omitempty"`
	MessageType string    `json:"messageType,omitempty"`
	TargetID    any       `json:"targetId,omitempty"`
	Segments    []Segment `json:"segments"`
}

type EventPublishParams struct {
	Name    string         `json:"name"`
	Payload map[string]any `json:"payload,omitempty"`
}

type DBCallParams struct {
	Op      string           `json:"op"`
	Table   string           `json:"table,omitempty"`
	Data    map[string]any   `json:"data,omitempty"`
	Where   map[string]any   `json:"where,omitempty"`
	Schema  json.RawMessage  `json:"schema,omitempty"`
	Rows    []map[string]any `json:"rows,omitempty"`
	Column  string           `json:"column,omitempty"`
	Value   any              `json:"value,omitempty"`
	Start   any              `json:"start,omitempty"`
	End     any              `json:"end,omitempty"`
	Options *DBCallOptions   `json:"options,omitempty"`
}

type DBCallOptions struct {
	Limit   int    `json:"limit,omitempty"`
	Offset  int    `json:"offset,omitempty"`
	OrderBy string `json:"orderBy,omitempty"`
	Order   string `json:"order,omitempty"`
}

type CancelParams struct {
	ID json.RawMessage `json:"id"`
}
