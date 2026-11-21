// Package bot is the bot core: prefixes, command registry, role checks, the event
// queue and reply semantics. Handlers of one event run concurrently; events run
// in arrival order. Handler.Finish sends the reply and ends the handler.
package bot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/uniu-network/uniubot/internal/core/args"
	"github.com/uniu-network/uniubot/internal/core/events"
	"github.com/uniu-network/uniubot/internal/core/logging"
	"github.com/uniu-network/uniubot/internal/core/message"
	"github.com/uniu-network/uniubot/internal/core/roles"
)

// ErrFinish unwinds the handler stack from Handler.Finish.
var ErrFinish = errors.New("finish")

var ErrCommandExists = errors.New("Command already registered")

type CommandInfo struct {
	Command     string     `json:"command"`
	Description string     `json:"description"`
	Scope       string     `json:"scope,omitempty"`
	RequireRole roles.Role `json:"requireRole"`
}

type Handler struct {
	bot   *Bot
	event *events.Event
}

func NewHandler(b *Bot, event *events.Event) *Handler {
	return &Handler{bot: b, event: event}
}

func (h *Handler) Event() *events.Event { return h.event }

func (h *Handler) Finish(ctx context.Context, msg message.Message) error {
	if h.event == nil {
		return ErrFinish
	}
	messageType, targetID := h.event.Target()
	if messageType == "" {
		return ErrFinish
	}
	if err := h.event.Reply(ctx, msg); err != nil {
		h.bot.logger.Error("error when sending finish message, %v", err)
		return ErrFinish
	}
	h.bot.EmitEvent("bot_send", &events.Event{
		Time:            time.Now().UnixMilli(),
		SelfID:          h.event.SelfID,
		AdapterID:       h.event.AdapterID,
		Adapter:         h.event.Adapter,
		Kind:            events.KindBotSend,
		SendMessageType: messageType,
		TargetID:        targetID,
		SendMessage:     msg,
	})
	return ErrFinish
}

// CommandFunc: a non-nil error (other than ErrFinish) triggers the error reply.
type CommandFunc func(ctx context.Context, cmd *CommandContext) error

type CommandContext struct {
	Bot     *Bot
	Event   *events.Event
	Args    *args.Args
	Reply   message.Message
	Handler *Handler
}

type EventHandler func(ctx context.Context, event *events.Event, handler *Handler) error

type handlerEntry struct {
	callback EventHandler
	scope    string
}

type queuedEvent struct {
	name  string
	event *events.Event
}

type Options struct {
	SelfID            int64
	Prefix            []string
	ErrorReplyEnabled bool
	Admins            []string
	Logger            *logging.Logger
	QueueSize         int
}

type Bot struct {
	SelfID            int64
	Prefix            []string
	ErrorReplyEnabled bool
	Admins            []string

	logger *logging.Logger

	mu       sync.RWMutex
	adapters map[string]events.Sender
	handlers map[string][]handlerEntry
	commands []CommandInfo
	scope    string
	running  bool
	queue    chan queuedEvent
	stopCh   chan struct{}
	stopOnce sync.Once
	dispatch sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
}

func New(opts Options) *Bot {
	prefix := opts.Prefix
	if len(prefix) == 0 {
		prefix = []string{"/"}
	}
	logger := opts.Logger
	if logger == nil {
		logger = logging.Default()
	}
	queueSize := opts.QueueSize
	if queueSize <= 0 {
		queueSize = 1024
	}
	b := &Bot{
		SelfID:            opts.SelfID,
		Prefix:            prefix,
		ErrorReplyEnabled: opts.ErrorReplyEnabled,
		Admins:            opts.Admins,
		logger:            logger,
		adapters:          map[string]events.Sender{},
		handlers:          map[string][]handlerEntry{},
		commands:          []CommandInfo{},
		queue:             make(chan queuedEvent, queueSize),
		stopCh:            make(chan struct{}),
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	return b
}

func (b *Bot) Logger() *logging.Logger { return b.logger }

func (b *Bot) Start() {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return
	}
	b.running = true
	b.mu.Unlock()
	b.dispatch.Add(1)
	go b.loop()
}

func (b *Bot) Stop() {
	b.stopOnce.Do(func() {
		close(b.stopCh)
		// Cancel in-flight handlers so a slow adapter cannot block shutdown.
		b.cancel()
	})
	b.mu.Lock()
	running := b.running
	b.running = false
	b.mu.Unlock()
	if running {
		b.dispatch.Wait()
	}
}

func (b *Bot) UseAdapter(ctx context.Context, sender events.Sender) error {
	b.mu.Lock()
	b.adapters[sender.ID()] = sender
	b.mu.Unlock()
	b.logger.Info("Adapter %q (%s) started", sender.Name(), sender.ID())
	return nil
}

func (b *Bot) GetAdapter(id string) (events.Sender, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	sender, ok := b.adapters[id]
	return sender, ok
}

func (b *Bot) AdapterIDs() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	ids := make([]string, 0, len(b.adapters))
	for id := range b.adapters {
		ids = append(ids, id)
	}
	return ids
}

func (b *Bot) RemoveAdapter(id string) {
	b.mu.Lock()
	delete(b.adapters, id)
	b.mu.Unlock()
}

// EmitEvent queues asynchronously. A full queue drops the event and logs an error.
func (b *Bot) EmitEvent(name string, event *events.Event) {
	name = strings.ToLower(name)
	select {
	case b.queue <- queuedEvent{name: name, event: event}:
		return
	case <-b.stopCh:
		return
	default:
		b.logger.Error("event queue is full, dropping %s event for bot %d", name, b.SelfID)
	}
}

func (b *Bot) On(event string, callback EventHandler) {
	name := strings.ToLower(event)
	b.mu.Lock()
	b.handlers[name] = append(b.handlers[name], handlerEntry{callback: callback, scope: b.scope})
	b.mu.Unlock()
}

func (b *Bot) WithScope(scope string, fn func() error) error {
	b.mu.Lock()
	previous := b.scope
	b.scope = scope
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.scope = previous
		b.mu.Unlock()
	}()
	return fn()
}

func (b *Bot) UnloadScope(scope string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	commands := b.commands[:0]
	for _, command := range b.commands {
		if command.Scope != scope {
			commands = append(commands, command)
		}
	}
	b.commands = commands
	for name, entries := range b.handlers {
		filtered := entries[:0]
		for _, entry := range entries {
			if entry.scope != scope {
				filtered = append(filtered, entry)
			}
		}
		if len(filtered) == 0 {
			delete(b.handlers, name)
		} else {
			b.handlers[name] = filtered
		}
	}
}

// RegisterCommand rejects duplicate names.
func (b *Bot) RegisterCommand(command, description string, scope string, requireRole roles.Role) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if scope == "" {
		scope = b.scope
	}
	for _, existing := range b.commands {
		if existing.Command == command {
			return ErrCommandExists
		}
	}
	b.commands = append(b.commands, CommandInfo{
		Command:     command,
		Description: description,
		Scope:       scope,
		RequireRole: requireRole,
	})
	return nil
}

func (b *Bot) Commands() []CommandInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]CommandInfo, len(b.commands))
	copy(out, b.commands)
	return out
}

func (b *Bot) Command(name, description string, requireRole roles.Role, fn CommandFunc) error {
	if err := b.RegisterCommand(name, description, "", requireRole); err != nil {
		return err
	}
	prefixes := make([]string, 0, len(b.Prefix))
	for _, prefix := range b.Prefix {
		prefixes = append(prefixes, regexp.QuoteMeta(prefix))
	}
	pattern := fmt.Sprintf("^(?:%s)%s(?:\\s+(.*))?$", strings.Join(prefixes, "|"), regexp.QuoteMeta(name))
	re, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}
	b.On("message", func(ctx context.Context, event *events.Event, handler *Handler) error {
		if !event.IsMessage() {
			return nil
		}
		match := re.FindStringSubmatch(event.RawMessage)
		if match == nil {
			return nil
		}
		effective := b.EffectiveRole(event)
		if effective < requireRole {
			reply := message.Text("无足够权限执行指令")
			return handler.Finish(ctx, reply)
		}
		var tokens []string
		if len(match) > 1 && strings.TrimSpace(match[1]) != "" {
			tokens = strings.Fields(match[1])
		}
		cmd := &CommandContext{
			Bot:     b,
			Event:   event,
			Args:    args.Parse(tokens),
			Reply:   message.Message{},
			Handler: handler,
		}
		err := fn(ctx, cmd)
		if err == nil || errors.Is(err, ErrFinish) {
			return nil
		}
		b.logger.Error("error when handling command %s, %v", name, err)
		b.replyError(ctx, event, err)
		return nil
	})
	return nil
}

// EffectiveRole never mutates the event: handlers run concurrently and serialise
// it into storage.
func (b *Bot) EffectiveRole(event *events.Event) roles.Role {
	senderRole := roles.Parse(event.Sender.SkipRole)
	adminKey := fmt.Sprintf("%s:%d", event.AdapterID, event.Sender.UserID)
	for _, admin := range b.Admins {
		if admin == adminKey {
			return roles.Max(senderRole, roles.BotAdmin)
		}
	}
	return senderRole
}

func (b *Bot) replyError(ctx context.Context, event *events.Event, err error) {
	if !b.ErrorReplyEnabled {
		b.logger.Error("error handling %s event: %v", event.TypeName(), err)
		return
	}
	reply := message.Text(fmt.Sprintf("在处理 %s 事件时出错: %v", event.TypeName(), err))
	handler := NewHandler(b, event)
	if finishErr := handler.Finish(ctx, reply); finishErr != nil && !errors.Is(finishErr, ErrFinish) {
		b.logger.Error("failed to send error reply: %v", finishErr)
	}
}

func (b *Bot) loop() {
	defer b.dispatch.Done()
	for {
		select {
		case <-b.stopCh:
			for {
				select {
				case item := <-b.queue:
					b.dispatchEvent(item)
				default:
					return
				}
			}
		case item := <-b.queue:
			b.dispatchEvent(item)
		}
	}
}

func (b *Bot) dispatchEvent(item queuedEvent) {
	b.mu.RLock()
	entries := b.handlers[item.name]
	snapshot := make([]handlerEntry, len(entries))
	copy(snapshot, entries)
	b.mu.RUnlock()
	if len(snapshot) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, entry := range snapshot {
		wg.Add(1)
		go func(entry handlerEntry) {
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					b.logger.Error("panic in handler for event %s: %v", item.name, recovered)
				}
			}()
			handler := NewHandler(b, item.event)
			ctx := b.ctx
			if err := entry.callback(ctx, item.event, handler); err != nil && !errors.Is(err, ErrFinish) {
				b.logger.Error("error in handler for event %s: %v", item.name, err)
				b.replyError(ctx, item.event, err)
			}
		}(entry)
	}
	wg.Wait()
}
