// Package pluginsdk is the Go plugin development kit.
// A plugin is a standalone executable; the SDK hides the JSON-RPC plumbing.
// See docs/plugin-system.md and plugins/examples/go for a full example.
package pluginsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/uniu-network/uniubot/pkg/pluginrpc"
)

// Meta is reported during the handshake; the host validates it against the manifest.
type Meta struct {
	ID           string
	Name         string
	Version      string
	APIVersion   string
	Methods      []string
	Capabilities []string
	Commands     []pluginrpc.CommandSpec
	Events       []string
	ConfigSchema []pluginrpc.ConfigField
}

type HandlerFunc func(ctx context.Context, params json.RawMessage) (any, error)

type CommandRequest struct {
	Command  string
	Args     map[string]any
	Event    json.RawMessage
	EventID  string
	Bot      pluginrpc.BotInfo
	Adapters []string
}

type EventRequest struct {
	// EventName is a core event name ("message", "notice", ...) or a custom one.
	EventName string
	Event     json.RawMessage
	EventID   string
	Bot       pluginrpc.BotInfo
}

type CommandFunc func(ctx context.Context, req *CommandRequest) (pluginrpc.HandlerResult, error)

type EventFunc func(ctx context.Context, req *EventRequest) (pluginrpc.HandlerResult, error)

type Initializer func(ctx context.Context, params pluginrpc.InitializeParams) error

type Option func(*Plugin)

func WithStderrLogger(enabled bool) Option {
	return func(p *Plugin) { p.stderr = enabled }
}

type Plugin struct {
	meta Meta

	mu           sync.RWMutex
	config       map[string]any
	params       pluginrpc.InitializeParams
	initialized  bool
	schemaFields []pluginrpc.ConfigField

	onInit       Initializer
	onConfigure  func(ctx context.Context, config map[string]any) error
	onShutdown   func(ctx context.Context) error
	custom       map[string]HandlerFunc
	handlers     map[string]HandlerFunc
	commands     map[string]CommandFunc
	commandOrder []string
	events       map[string]EventFunc
	stderr       bool
	schemaFilled bool

	peer *pluginrpc.Peer
	host *Host
	done chan struct{}
	once sync.Once
}

func New(meta Meta, opts ...Option) *Plugin {
	if meta.APIVersion == "" {
		meta.APIVersion = pluginrpc.APIVersion
	}
	if meta.Version == "" {
		meta.Version = "0.0.0"
	}
	p := &Plugin{
		meta:     meta,
		config:   map[string]any{},
		custom:   map[string]HandlerFunc{},
		handlers: map[string]HandlerFunc{},
		commands: map[string]CommandFunc{},
		events:   map[string]EventFunc{},
		stderr:   true,
		done:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(p)
	}
	p.registerBuiltins()
	return p
}

func (p *Plugin) ID() string { return p.meta.ID }

func (p *Plugin) Meta() Meta { return p.meta }

func (p *Plugin) OnInitialize(fn Initializer) { p.onInit = fn }

func (p *Plugin) OnConfigure(fn func(ctx context.Context, config map[string]any) error) {
	p.onConfigure = fn
}

func (p *Plugin) OnShutdown(fn func(ctx context.Context) error) { p.onShutdown = fn }

// Command declares the command and routes command.execute for it to fn, the
// equivalent of the former ctx.bot.command(...).
func (p *Plugin) Command(name, description string, fn CommandFunc) {
	p.CommandAs(name, description, 0, fn)
}

// CommandAs roles: 0 = USER, 1 = GROUP_ADMIN, 2 = GROUP_OWNER, 3 = BOT_ADMIN.
// The host rejects the invocation before it reaches the plugin.
func (p *Plugin) CommandAs(name, description string, requireRole int, fn CommandFunc) {
	name = strings.TrimSpace(name)
	if name == "" || fn == nil {
		panic("pluginsdk: CommandAs requires a name and a handler")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.commands[name]; exists {
		panic("pluginsdk: command " + name + " is already registered")
	}
	p.commands[name] = fn
	p.commandOrder = append(p.commandOrder, name)
	p.meta.Commands = append(p.meta.Commands, pluginrpc.CommandSpec{
		Command:     name,
		Description: description,
		RequireRole: requireRole,
	})
}

// On declares interest in an event kind ("*" for all), so the host only forwards
// the events asked for.
func (p *Plugin) On(eventName string, fn EventFunc) {
	eventName = strings.TrimSpace(eventName)
	if eventName == "" || fn == nil {
		panic("pluginsdk: On requires an event name and a handler")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.events[eventName]; exists {
		panic("pluginsdk: event " + eventName + " is already registered")
	}
	p.events[eventName] = fn
	if eventName == "*" {
		p.meta.Events = []string{"*"}
		return
	}
	for _, existing := range p.meta.Events {
		if existing == "*" || existing == eventName {
			return
		}
	}
	p.meta.Events = append(p.meta.Events, eventName)
}

// Commands returns the declarations in registration order.
func (p *Plugin) Commands() []pluginrpc.CommandSpec {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]pluginrpc.CommandSpec, len(p.meta.Commands))
	copy(out, p.meta.Commands)
	return out
}

// Handle registers a raw plugin method (e.g. "command.execute").
func (p *Plugin) Handle(method string, fn HandlerFunc) {
	p.mu.Lock()
	p.custom[method] = fn
	p.mu.Unlock()
}

func (p *Plugin) Config() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cloneMap(p.config)
}

func (p *Plugin) Params() (pluginrpc.InitializeParams, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.params, p.initialized
}

func (p *Plugin) Host() *Host { return p.host }

// Logf writes to stderr: stdout carries the protocol only.
func (p *Plugin) Logf(format string, args ...any) {
	if !p.stderr {
		return
	}
	fmt.Fprintf(os.Stderr, "[plugin:%s] "+format+"\n", append([]any{p.meta.ID}, args...)...)
}

func (p *Plugin) LogError(err error) {
	p.Logf("fatal: %v", err)
}

// Serve blocks until shutdown or stdin EOF.
func (p *Plugin) Serve() error {
	peer := pluginrpc.NewPeer(os.Stdin, os.Stdout, pluginrpc.Options{
		Name: "plugin:" + p.meta.ID,
		OnClose: func(error) {
			p.stop()
		},
		// Handlers must exist first: the host sends plugin.initialize right after spawn.
		OnReady: func(peer *pluginrpc.Peer) {
			p.attachHandlersTo(peer)
		},
	})
	p.peer = peer
	p.host = &Host{peer: peer, pluginID: p.meta.ID}
	<-p.done
	peer.Close(nil)
	return nil
}

func (p *Plugin) stop() {
	p.once.Do(func() { close(p.done) })
}

func (p *Plugin) registerBuiltins() {
	p.handlers[pluginrpc.MethodCommand] = p.handleCommandDispatch
	p.handlers[pluginrpc.MethodEvent] = p.handleEventDispatch
	p.handlers[pluginrpc.MethodInitialize] = p.handleInitialize
	p.handlers[pluginrpc.MethodHealth] = p.handleHealth
	p.handlers[pluginrpc.MethodShutdown] = p.handleShutdown
	p.handlers[pluginrpc.MethodConfigure] = p.handleConfigure
}

func (p *Plugin) attachHandlersTo(peer *pluginrpc.Peer) {
	p.mu.Lock()
	handlers := make(map[string]HandlerFunc, len(p.handlers)+len(p.custom))
	for method, handler := range p.handlers {
		handlers[method] = handler
	}
	for method, handler := range p.custom {
		handlers[method] = handler
	}
	p.mu.Unlock()
	for method, handler := range handlers {
		peer.Handle(method, pluginrpc.HandlerFunc(handler))
	}
}

func (p *Plugin) handleInitialize(ctx context.Context, raw json.RawMessage) (any, error) {
	var params pluginrpc.InitializeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, "invalid initialize params: "+err.Error())
		}
	}
	if params.ProtocolVersion != "" && params.ProtocolVersion != pluginrpc.ProtocolVersion {
		return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams,
			fmt.Sprintf("unsupported protocol version %q (host: %s)", params.ProtocolVersion, pluginrpc.ProtocolVersion))
	}
	if params.APIVersion != "" && params.APIVersion != pluginrpc.APIVersion && p.meta.APIVersion == pluginrpc.APIVersion {
		return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams,
			fmt.Sprintf("unsupported api version %q (plugin: %s)", params.APIVersion, p.meta.APIVersion))
	}

	config := applyConfigDefaults(params.Config, p.meta.ConfigSchema)
	params.Config = config

	p.mu.Lock()
	p.params = params
	p.config = config
	p.initialized = true
	p.mu.Unlock()

	if p.onInit != nil {
		if err := p.onInit(ctx, params); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodePluginError, "plugin initialization failed: "+err.Error())
		}
	}

	// Advertise only the methods the plugin can serve: the host forwards events only
	// when "event.handle" is advertised.
	methods := append([]string{}, p.meta.Methods...)
	p.mu.Lock()
	if len(p.commands) > 0 && !containsString(methods, pluginrpc.MethodCommand) {
		methods = append(methods, pluginrpc.MethodCommand)
	}
	if len(p.events) > 0 && !containsString(methods, pluginrpc.MethodEvent) {
		methods = append(methods, pluginrpc.MethodEvent)
	}
	for method := range p.custom {
		if !containsString(methods, method) {
			methods = append(methods, method)
		}
	}
	p.mu.Unlock()

	return pluginrpc.InitializeResult{
		ID:           p.meta.ID,
		Name:         p.meta.Name,
		Version:      p.meta.Version,
		APIVersion:   p.meta.APIVersion,
		Methods:      methods,
		Capabilities: p.meta.Capabilities,
		Commands:     p.meta.Commands,
		Events:       p.meta.Events,
		ConfigSchema: p.meta.ConfigSchema,
	}, nil
}

func (p *Plugin) handleCommandDispatch(ctx context.Context, raw json.RawMessage) (any, error) {
	var params pluginrpc.ExecuteParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, "invalid command params: "+err.Error())
		}
	}
	p.mu.RLock()
	handler, ok := p.commands[params.Command]
	p.mu.RUnlock()
	if !ok {
		return nil, pluginrpc.NewError(pluginrpc.CodeMethodNotFound, "command not registered: "+params.Command)
	}
	args := params.Args
	if args == nil {
		args = map[string]any{"_": []any{}}
	}
	result, err := handler(ctx, &CommandRequest{
		Command:  params.Command,
		Args:     args,
		Event:    params.Event,
		EventID:  params.EventID,
		Bot:      params.Bot,
		Adapters: params.Adapters,
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (p *Plugin) handleEventDispatch(ctx context.Context, raw json.RawMessage) (any, error) {
	var params pluginrpc.EventParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, "invalid event params: "+err.Error())
		}
	}
	p.mu.RLock()
	handler, ok := p.events[params.EventName]
	if !ok {
		handler, ok = p.events["*"]
	}
	p.mu.RUnlock()
	if !ok {
		return nil, pluginrpc.NewError(pluginrpc.CodeMethodNotFound, "no handler for event "+params.EventName)
	}
	result, err := handler(ctx, &EventRequest{
		EventName: params.EventName,
		Event:     params.Event,
		EventID:   params.EventID,
		Bot:       params.Bot,
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (p *Plugin) handleHealth(context.Context, json.RawMessage) (any, error) {
	p.mu.RLock()
	initialized := p.initialized
	p.mu.RUnlock()
	return map[string]any{
		"status":      "ok",
		"id":          p.meta.ID,
		"version":     p.meta.Version,
		"initialized": initialized,
	}, nil
}

func (p *Plugin) handleShutdown(ctx context.Context, _ json.RawMessage) (any, error) {
	if p.onShutdown != nil {
		if err := p.onShutdown(ctx); err != nil {
			p.Logf("shutdown hook failed: %v", err)
		}
	}
	// The peer writes the response after this handler returns: delay the exit.
	go func() {
		time.Sleep(50 * time.Millisecond)
		p.stop()
	}()
	return map[string]any{"status": "stopped"}, nil
}

func (p *Plugin) handleConfigure(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, err.Error())
	}
	config := applyConfigDefaults(params.Config, p.meta.ConfigSchema)
	p.mu.Lock()
	p.config = config
	p.mu.Unlock()
	if p.onConfigure != nil {
		if err := p.onConfigure(ctx, config); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodePluginError, err.Error())
		}
	}
	return map[string]any{"status": "ok"}, nil
}

func SegmentText(text string) pluginrpc.Segment {
	return pluginrpc.Segment{Type: "text", Data: map[string]any{"text": text}}
}

func SegmentAt(userID any) pluginrpc.Segment {
	return pluginrpc.Segment{Type: "at", Data: map[string]any{"user_id": fmt.Sprint(userID)}}
}

func SegmentReply(messageID any) pluginrpc.Segment {
	return pluginrpc.Segment{Type: "reply", Data: map[string]any{"id": messageID}}
}

func SegmentImage(file string) pluginrpc.Segment {
	return pluginrpc.Segment{Type: "image", Data: map[string]any{"file": file}}
}

func SegmentRaw(platform, segmentType string, data map[string]any) pluginrpc.Segment {
	return pluginrpc.Segment{Type: "raw", Data: map[string]any{
		"platform": platform,
		"type":     segmentType,
		"data":     data,
	}}
}

// HandlerResultNone produces no reply: for observers and for commands that
// already replied through Host.Reply.
func HandlerResultNone() pluginrpc.HandlerResult {
	return pluginrpc.HandlerResult{}
}

// Result builds a handler result with Finish=true.
func Result(segments ...pluginrpc.Segment) pluginrpc.HandlerResult {
	return pluginrpc.HandlerResult{Segments: segments, Finish: true}
}

// applyConfigDefaults coerces values with the declared schema and keeps unknown
// keys, so plugins can evolve their configuration.
func applyConfigDefaults(raw map[string]any, schema []pluginrpc.ConfigField) map[string]any {
	out := map[string]any{}
	for key, value := range raw {
		out[key] = value
	}
	for _, field := range schema {
		value, exists := out[field.Key]
		if !exists || value == nil {
			out[field.Key] = field.Default
			continue
		}
		if coerced, ok := coerce(value, field.Type); ok {
			out[field.Key] = coerced
		}
	}
	return out
}

func coerce(value any, fieldType string) (any, bool) {
	switch fieldType {
	case "boolean":
		switch typed := value.(type) {
		case string:
			if typed == "true" || typed == "1" {
				return true, true
			}
			if typed == "false" || typed == "0" {
				return false, true
			}
		case float64:
			return typed != 0, true
		case bool:
			return typed, true
		}
	case "number":
		switch typed := value.(type) {
		case string:
			var parsed float64
			if _, err := fmt.Sscan(typed, &parsed); err == nil {
				return parsed, true
			}
		case float64:
			return typed, true
		case int:
			return float64(typed), true
		}
	case "string":
		return fmt.Sprint(value), true
	case "string[]":
		switch typed := value.(type) {
		case []any:
			out := make([]string, len(typed))
			for i, item := range typed {
				out[i] = fmt.Sprint(item)
			}
			return out, true
		case []string:
			return typed, true
		}
	case "object":
		if _, ok := value.(map[string]any); ok {
			return value, true
		}
	}
	return value, false
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
