package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uniu-network/uniubot/internal/adapters"
	"github.com/uniu-network/uniubot/internal/bot"
	"github.com/uniu-network/uniubot/internal/core/args"
	"github.com/uniu-network/uniubot/internal/core/config"
	"github.com/uniu-network/uniubot/internal/core/events"
	"github.com/uniu-network/uniubot/internal/core/logging"
	"github.com/uniu-network/uniubot/internal/core/message"
	"github.com/uniu-network/uniubot/internal/core/roles"
	"github.com/uniu-network/uniubot/internal/storage"
	"github.com/uniu-network/uniubot/pkg/pluginrpc"
)

type Options struct {
	BotID        string
	BotName      string
	Version      string
	Bot          *bot.Bot
	Adapters     *adapters.Manager
	BotConfig    *config.Store
	GlobalConfig *config.Store
	Storage      *storage.Manager
	Logger       *logging.Logger
	PluginDir    string
}

type Manager struct {
	botID    string
	botName  string
	version  string
	bot      *bot.Bot
	adapters *adapters.Manager
	botCfg   *config.Store
	global   *config.Store
	storage  *storage.Manager
	logger   *logging.Logger
	dir      string
	perm     Permissions

	mu       sync.RWMutex
	registry *Registry
	sessions map[string]*session
	order    []string
	nextEvt  atomic.Uint64

	lifecycleMu sync.Mutex
	started     bool

	// baseCtx outlives the HTTP request that triggered a load or reload.
	ctxMu   sync.RWMutex
	baseCtx context.Context
}

type session struct {
	id       string
	manifest *Manifest
	dir      string
	jobs     chan invocation
	pending  sync.Map // eventID -> *events.Event

	// mu guards the fields below: replaced on start while peers read them.
	mu      sync.RWMutex
	process *Process
	peer    *pluginrpc.Peer
	granted map[string]bool
	cmds    []commandMatcher

	stopping atomic.Bool
	restarts int
	lastUp   time.Time

	ctx    context.Context
	cancel context.CancelFunc
}

func (s *session) rpcPeer() *pluginrpc.Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peer
}

func (s *session) currentProcess() *Process {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.process
}

func (s *session) matchers() []commandMatcher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cmds
}

func (s *session) setGranted(capabilities []string) {
	granted := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		granted[capability] = true
	}
	s.mu.Lock()
	s.granted = granted
	s.mu.Unlock()
}

func (s *session) hasCapability(capability string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.granted[capability]
}

// invocation is a queued host -> plugin call, drained by the session workers.
type invocation struct {
	method  string
	params  any
	event   *events.Event
	handler *bot.Handler
	eventID string
}

type commandMatcher struct {
	spec     pluginrpc.CommandSpec
	re       *regexp.Regexp
	rawName  string
	required roles.Role
}

func New(opts Options) *Manager {
	dir := opts.PluginDir
	if dir == "" {
		dir = "plugins"
	}
	logger := opts.Logger
	if logger == nil {
		logger = logging.Default()
	}
	return &Manager{
		botID:    opts.BotID,
		baseCtx:  context.Background(),
		botName:  opts.BotName,
		version:  opts.Version,
		bot:      opts.Bot,
		adapters: opts.Adapters,
		botCfg:   opts.BotConfig,
		global:   opts.GlobalConfig,
		storage:  opts.Storage,
		logger:   logger,
		dir:      dir,
		perm:     LoadPermissions(opts.GlobalConfig),
		registry: NewRegistry(),
		sessions: map[string]*session{},
	}
}

func (m *Manager) Registry() *Registry { return m.registry }

func (m *Manager) BotID() string { return m.botID }

func (m *Manager) Start(ctx context.Context) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.started {
		return nil
	}
	m.started = true
	m.setBaseContext(ctx)
	m.registerDispatcher()

	count := 0
	for _, item := range Discover(m.dir) {
		if item.Manifest == nil {
			m.logger.Error("Failed to load plugin %s: %v", item.Dir, item.Err)
			continue
		}
		manifest := item.Manifest
		m.registry.Upsert(Entry{
			ID:          manifest.ID,
			Name:        manifest.Name,
			Version:     manifest.Version,
			Description: manifest.Description,
			Author:      manifest.Author,
			Runtime:     manifest.Runtime,
			Dir:         item.Dir,
			Manifest:    manifest,
			State:       StateDiscovered,
			Reason:      errorString(item.Err),
		})
		enabled := m.pluginEnabled(manifest)
		if item.Err != nil || !enabled {
			state := StateFailed
			if item.Err == nil {
				state = StateDisabled
			}
			m.registry.Update(manifest.ID, func(e *Entry) {
				e.State = state
				if item.Err != nil {
					e.LastError = item.Err.Error()
				}
			})
			if item.Err == nil {
				m.logger.Info("Plugin %s is disabled", manifest.ID)
			}
			continue
		}
		if err := m.startPlugin(manifest, item.Dir); err != nil {
			m.logger.Error("Failed to load plugin %s: %v", manifest.ID, err)
			m.registry.Update(manifest.ID, func(e *Entry) {
				e.State = StateFailed
				e.LastError = err.Error()
				e.Reason = err.Error()
			})
			continue
		}
		count++
	}
	m.logger.Info("[plugins:%s] %d plugin(s) loaded", m.botID, count)
	return nil
}

func (m *Manager) Stop(ctx context.Context) {
	m.lifecycleMu.Lock()
	if !m.started {
		m.lifecycleMu.Unlock()
		return
	}
	m.started = false
	m.lifecycleMu.Unlock()

	m.mu.RLock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.RUnlock()
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *session) {
			defer wg.Done()
			s.stopping.Store(true)
			m.registry.Update(s.id, func(e *Entry) { e.State = StateStopping })
			if process := s.currentProcess(); process != nil {
				if err := process.Stop(ctx); err != nil {
					m.logger.Error("[plugin:%s] failed to stop: %v", s.id, err)
				}
			}
			s.cancel()
			m.bot.UnloadScope("plugin:" + s.id)
			m.registry.Update(s.id, func(e *Entry) {
				e.State = StateStopped
				e.PID = 0
			})
			m.mu.Lock()
			delete(m.sessions, s.id)
			m.mu.Unlock()
		}(s)
	}
	wg.Wait()
	m.mu.Lock()
	m.order = nil
	m.mu.Unlock()
}

func (m *Manager) registerDispatcher() {
	for _, name := range []string{"message", "notice", "request", "meta_event", "bot_send"} {
		eventName := name
		m.bot.On(eventName, func(ctx context.Context, event *events.Event, handler *bot.Handler) error {
			m.dispatch(ctx, eventName, event, handler)
			return nil
		})
	}
}

func (m *Manager) pluginEnabled(manifest *Manifest) bool {
	raw := m.botCfg.Get("plugin_config."+manifest.ID, nil)
	if raw == nil {
		raw = m.global.Get("plugins."+manifest.ID, nil)
	}
	if settings, ok := raw.(map[string]any); ok {
		if enabled, ok := settings["enabled"].(bool); ok {
			return enabled
		}
	}
	return manifest.EnabledOrDefault()
}

func (m *Manager) pluginSettings(id string) map[string]any {
	settings := m.botCfg.Map("plugin_config." + id)
	if len(settings) == 0 {
		settings = m.global.Map("plugins." + id)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	return settings
}

func (m *Manager) setBaseContext(ctx context.Context) {
	m.ctxMu.Lock()
	m.baseCtx = ctx
	m.ctxMu.Unlock()
}

func (m *Manager) baseContext() context.Context {
	m.ctxMu.RLock()
	defer m.ctxMu.RUnlock()
	if m.baseCtx == nil {
		return context.Background()
	}
	return m.baseCtx
}

func (m *Manager) startPlugin(manifest *Manifest, dir string) error {
	id := manifest.ID
	m.registry.Update(id, func(e *Entry) {
		e.State = StateValidating
		e.Reason = ""
	})
	if err := manifest.Validate(); err != nil {
		return err
	}

	sessionCtx, cancel := context.WithCancel(m.baseContext())
	s := &session{
		id:       id,
		manifest: manifest,
		dir:      dir,
		jobs:     make(chan invocation, manifest.Concurrency*8),
		ctx:      sessionCtx,
		cancel:   cancel,
	}
	for i := 0; i < manifest.Concurrency; i++ {
		go m.worker(s)
	}
	if err := m.launch(sessionCtx, s); err != nil {
		cancel()
		return err
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.order = append(m.order, id)
	m.mu.Unlock()
	return nil
}

func (m *Manager) launch(ctx context.Context, s *session) error {
	id := s.id
	manifest := s.manifest
	m.registry.Update(id, func(e *Entry) {
		e.State = StateStarting
		e.Reason = ""
	})

	process, err := StartProcess(ctx, ProcessOptions{
		Dir:      s.dir,
		Manifest: manifest,
		Logger:   m.logger,
		OnExit:   func(err error) { m.onProcessExit(s, err) },
		RegisterHandlers: func(peer *pluginrpc.Peer) {
			m.attachHostHandlers(s, peer)
		},
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.process = process
	s.peer = process.Peer()
	s.mu.Unlock()

	m.registry.Update(id, func(e *Entry) {
		e.State = StateInitializing
		e.PID = process.PID()
	})

	initParams := pluginrpc.InitializeParams{
		ProtocolVersion: pluginrpc.ProtocolVersion,
		APIVersion:      pluginrpc.APIVersion,
		PluginID:        id,
		Bot: pluginrpc.BotInfo{
			BotID:    m.botID,
			SelfID:   m.bot.SelfID,
			Name:     m.botName,
			Prefix:   m.bot.Prefix,
			Adapters: m.adapterIDs(),
		},
		Host: pluginrpc.HostInfo{
			Version:      m.version,
			Protocol:     pluginrpc.Transport,
			PID:          os.Getpid(),
			Platform:     runtimePlatform(),
			HostAPILevel: pluginrpc.APIVersion,
		},
		Config:       m.pluginConfigFor(id),
		WorkDir:      s.dir,
		Capabilities: manifest.Capabilities,
	}

	callCtx, cancel := context.WithTimeout(ctx, manifest.Timeout()+5*time.Second)
	defer cancel()
	var result pluginrpc.InitializeResult
	if err := s.peer.CallJSON(callCtx, pluginrpc.MethodInitialize, initParams, &result); err != nil {
		_ = process.Kill()
		return fmt.Errorf("initialize handshake failed: %w", err)
	}
	if err := validateRegistration(id, manifest, &result); err != nil {
		_ = process.Stop(context.Background())
		return err
	}

	m.registry.Update(id, func(e *Entry) {
		e.State = StateRegistering
		e.Version = result.Version
		if result.Name != "" {
			e.Name = result.Name
		}
	})

	granted := m.perm.Resolve(id, manifest.Capabilities, result.Capabilities)
	s.setGranted(granted)
	if denied := missingCapabilities(result.Capabilities, granted); len(denied) > 0 {
		m.logger.Warn("[plugin:%s] capabilities not granted: %s", id, strings.Join(denied, ", "))
	}

	matchers := m.buildMatchers(result.Commands)
	s.mu.Lock()
	s.cmds = matchers
	s.mu.Unlock()
	m.registerCommands(s)

	if err := s.peer.Notify(pluginrpc.MethodInitialized, map[string]any{"status": "ok"}); err != nil {
		m.logger.Warn("[plugin:%s] failed to send plugin.initialized: %v", id, err)
	}

	s.lastUp = time.Now()
	m.registry.Update(id, func(e *Entry) {
		e.State = StateReady
		e.Commands = result.Commands
		e.Methods = result.Methods
		e.DeclaredCapabilities = result.Capabilities
		e.GrantedCapabilities = granted
		e.ConfigSchema = result.ConfigSchema
		e.Events = result.Events
		e.PID = process.PID()
		e.StartedAt = s.lastUp
		e.Reason = ""
		e.LastError = ""
	})
	m.logger.Info("Loaded plugin: %s (runtime=%s version=%s)", id, manifest.Runtime, result.Version)
	return nil
}

func declaresMethod(methods []string, method string) bool {
	for _, candidate := range methods {
		if candidate == method || candidate == "*" {
			return true
		}
	}
	return false
}

func missingCapabilities(declared, granted []string) []string {
	allowed := map[string]bool{}
	for _, capability := range granted {
		allowed[capability] = true
	}
	out := []string{}
	for _, capability := range declared {
		if !allowed[capability] {
			out = append(out, capability)
		}
	}
	return out
}

func (m *Manager) buildMatchers(specs []pluginrpc.CommandSpec) []commandMatcher {
	prefixes := make([]string, 0, len(m.bot.Prefix))
	for _, prefix := range m.bot.Prefix {
		prefixes = append(prefixes, regexp.QuoteMeta(prefix))
	}
	prefixGroup := strings.Join(prefixes, "|")
	out := make([]commandMatcher, 0, len(specs))
	for _, spec := range specs {
		if strings.TrimSpace(spec.Command) == "" {
			continue
		}
		pattern := fmt.Sprintf("^(?:%s)%s(?:\\s+(.*))?$", prefixGroup, regexp.QuoteMeta(spec.Command))
		re, err := regexp.Compile(pattern)
		if err != nil {
			m.logger.Error("invalid command pattern for %q: %v", spec.Command, err)
			continue
		}
		out = append(out, commandMatcher{
			spec:     spec,
			re:       re,
			rawName:  spec.Command,
			required: roles.Parse(spec.RequireRole),
		})
	}
	return out
}

func (m *Manager) registerCommands(s *session) {
	scope := "plugin:" + s.id
	for _, matcher := range s.cmds {
		description := matcher.spec.Description
		if description == "" {
			description = matcher.spec.Command
		}
		if err := m.bot.RegisterCommand(matcher.spec.Command, description, scope, matcher.required); err != nil {
			m.logger.Warn("[plugin:%s] command %q not registered: %v", s.id, matcher.spec.Command, err)
		}
	}
}

func (m *Manager) onProcessExit(s *session, err error) {
	if s.stopping.Load() {
		return
	}
	m.bot.UnloadScope("plugin:" + s.id)
	id := s.id
	manifest := s.manifest
	m.logger.Error("[plugin:%s] process exited unexpectedly: %v", id, err)
	m.registry.Update(id, func(e *Entry) {
		e.State = StateCrashed
		e.PID = 0
		if err != nil {
			e.LastError = err.Error()
		}
	})

	if manifest.Restart == nil || manifest.Restart.MaxAttempts <= 0 {
		return
	}
	if time.Since(s.lastUp) > 60*time.Second {
		s.restarts = 0
	}
	if s.restarts >= manifest.Restart.MaxAttempts {
		m.registry.Update(id, func(e *Entry) {
			e.State = StateUnavailable
			e.Reason = fmt.Sprintf("crashed %d time(s); restart budget exhausted", s.restarts)
		})
		m.logger.Error("[plugin:%s] restart budget exhausted", id)
		return
	}
	backoff := time.Duration(manifest.Restart.BackoffMS) * time.Millisecond
	for i := 0; i < s.restarts; i++ {
		backoff *= 2
	}
	s.restarts++
	m.registry.Update(id, func(e *Entry) { e.RestartCount = s.restarts })
	m.logger.Warn("[plugin:%s] restarting in %s (attempt %d/%d)", id, backoff, s.restarts, manifest.Restart.MaxAttempts)

	go func() {
		timer := time.NewTimer(backoff)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.ctx.Done():
			return
		}
		if s.stopping.Load() {
			return
		}
		if err := m.launch(s.ctx, s); err != nil {
			m.logger.Error("[plugin:%s] restart failed: %v", id, err)
			m.registry.Update(id, func(e *Entry) {
				e.State = StateFailed
				e.LastError = err.Error()
			})
			m.onProcessExit(s, err)
		}
	}()
}

func (m *Manager) pluginConfigFor(id string) map[string]any {
	settings := m.pluginSettings(id)
	out := map[string]any{}
	for key, value := range settings {
		if key == "enabled" {
			continue
		}
		out[key] = value
	}
	return out
}

func (m *Manager) adapterIDs() []string {
	ids := []string{}
	if m.adapters == nil {
		return ids
	}
	for _, info := range m.adapters.List() {
		ids = append(ids, info.ID)
	}
	return ids
}

func (m *Manager) dispatch(ctx context.Context, eventName string, event *events.Event, handler *bot.Handler) {
	sessions := m.readySessions()
	if len(sessions) == 0 {
		return
	}
	eventPayload, err := event.Payload()
	if err != nil {
		m.logger.Error("failed to encode event for plugin dispatch: %v", err)
		return
	}
	for _, s := range sessions {
		if eventName == "message" && event.IsMessage() {
			for _, matcher := range s.matchers() {
				match := matcher.re.FindStringSubmatch(event.RawMessage)
				if match == nil {
					continue
				}
				effective := m.bot.EffectiveRole(event)
				if effective < matcher.required {
					reply := message.Text("无足够权限执行指令")
					if err := handler.Finish(context.Background(), reply); err != nil && !errors.Is(err, bot.ErrFinish) {
						m.logger.Error("failed to send permission denial: %v", err)
					}
					continue
				}
				args := map[string]any{"_": []string{}}
				if len(match) > 1 && strings.TrimSpace(match[1]) != "" {
					args = argsFromLine(match[1])
				}
				eventID := m.newEventID()
				s.pending.Store(eventID, event)
				params := pluginrpc.ExecuteParams{
					Command:  matcher.spec.Command,
					Args:     args,
					Event:    eventPayload,
					EventID:  eventID,
					Bot:      m.botInfo(),
					Adapters: m.adapterIDs(),
				}
				m.invoke(s, pluginrpc.MethodCommand, params, event, handler, eventID)
				break
			}
		}
		if m.wantsEvent(s, eventName) {
			eventID := m.newEventID()
			s.pending.Store(eventID, event)
			params := pluginrpc.EventParams{
				EventName: eventName,
				Event:     eventPayload,
				EventID:   eventID,
				Bot:       m.botInfo(),
			}
			m.invoke(s, pluginrpc.MethodEvent, params, event, handler, eventID)
		}
	}
}

func (m *Manager) newEventID() string {
	return fmt.Sprintf("evt-%s-%d", m.botID, m.nextEvt.Add(1))
}

func (m *Manager) wantsEvent(s *session, name string) bool {
	entry, ok := m.registry.Get(s.id)
	if !ok {
		return false
	}
	// Skip plugins without event.handle: they would answer "method not found" on
	// every message.
	if !declaresMethod(entry.Methods, pluginrpc.MethodEvent) {
		return false
	}
	if len(entry.Events) == 0 {
		// Plugins that declare no interest still receive core events, like a bare
		// bot.on(...) listener.
		switch name {
		case "message", "notice", "request", "meta_event", "bot_send":
			return true
		default:
			return false
		}
	}
	for _, wanted := range entry.Events {
		if wanted == name || wanted == "*" {
			return true
		}
	}
	return false
}

func (m *Manager) invoke(s *session, method string, params any, event *events.Event, handler *bot.Handler, eventID string) {
	if peer := s.rpcPeer(); peer == nil || peer.Closed() {
		s.pending.Delete(eventID)
		return
	}
	job := invocation{method: method, params: params, event: event, handler: handler, eventID: eventID}
	select {
	case s.jobs <- job:
	case <-s.ctx.Done():
		s.pending.Delete(eventID)
	default:
		// The queue is bounded (concurrency * 8) so a stuck plugin cannot accumulate
		// goroutines or memory.
		s.pending.Delete(eventID)
		m.logger.Warn("[plugin:%s] %s dropped: queue is full", s.id, method)
	}
}

// worker drains the queue; concurrency from the manifest bounds parallel calls.
func (m *Manager) worker(s *session) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case job := <-s.jobs:
			m.callPlugin(s, job)
		}
	}
}

func (m *Manager) callPlugin(s *session, job invocation) {
	// A plugin replying through host.message.reply must do so while handling the
	// call: the pending entry is gone afterwards.
	defer s.pending.Delete(job.eventID)

	peer := s.rpcPeer()
	if peer == nil || peer.Closed() {
		return
	}
	callCtx, cancel := context.WithTimeout(s.ctx, s.manifest.Timeout())
	defer cancel()
	var result pluginrpc.HandlerResult
	if err := peer.CallJSON(callCtx, job.method, job.params, &result); err != nil {
		m.logger.Error("[plugin:%s] %s failed: %v", s.id, job.method, err)
		m.registry.Update(s.id, func(e *Entry) { e.LastError = err.Error() })
		return
	}
	if job.handler == nil || !result.Finish || len(result.Segments) == 0 {
		return
	}
	if err := job.handler.Finish(context.Background(), toMessage(result.Segments)); err != nil && !errors.Is(err, bot.ErrFinish) {
		m.logger.Error("[plugin:%s] failed to send reply: %v", s.id, err)
	}
}

func toMessage(segments []pluginrpc.Segment) message.Message {
	msg := make(message.Message, 0, len(segments))
	for _, segment := range segments {
		data := segment.Data
		if data == nil {
			data = map[string]any{}
		}
		msg = append(msg, message.Segment{Type: segment.Type, Data: data})
	}
	return msg
}

func (m *Manager) botInfo() pluginrpc.BotInfo {
	return pluginrpc.BotInfo{
		BotID:    m.botID,
		SelfID:   m.bot.SelfID,
		Name:     m.botName,
		Prefix:   m.bot.Prefix,
		Adapters: m.adapterIDs(),
	}
}

func (m *Manager) readySessions() []*session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if entry, ok := m.registry.Get(s.id); ok && entry.State == StateReady {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func (m *Manager) List() []Info {
	entries := m.registry.Snapshot()
	out := make([]Info, 0, len(entries))
	for _, entry := range entries {
		schema := (*SchemaInfo)(nil)
		if len(entry.ConfigSchema) > 0 {
			schema = &SchemaInfo{Fields: entry.ConfigSchema}
		}
		enabled := true
		if entry.Manifest != nil {
			enabled = m.pluginEnabled(entry.Manifest)
		}
		out = append(out, Info{
			Name:         entry.ID,
			ID:           entry.ID,
			Description:  entry.Description,
			Version:      entry.Version,
			Author:       entry.Author,
			Runtime:      entry.Runtime,
			State:        string(entry.State),
			IsEnabled:    enabled,
			IsLoaded:     entry.State == StateReady,
			ConfigSchema: schema,
			Commands:     entry.Commands,
			Capabilities: entry.GrantedCapabilities,
			Reason:       entry.Reason,
			PID:          entry.PID,
		})
	}
	return out
}

func (m *Manager) Count() int {
	return len(m.registry.Ready())
}

func (m *Manager) Names() []string { return m.registry.Ready() }

func (m *Manager) IsLoaded(id string) bool {
	entry, ok := m.registry.Get(id)
	return ok && entry.State == StateReady
}

func (m *Manager) Get(id string) (Entry, bool) { return m.registry.Get(id) }

func (m *Manager) Load(ctx context.Context, id string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if entry, ok := m.registry.Get(id); ok && entry.State == StateReady {
		return fmt.Errorf("plugin %s is already loaded", id)
	}
	dir, ok := findPluginDir(m.dir, id)
	if !ok {
		return fmt.Errorf("plugin %s not found", id)
	}
	manifest, err := LoadManifest(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return err
	}
	if err := manifest.Validate(); err != nil {
		m.registry.Upsert(Entry{ID: manifest.ID, Name: manifest.Name, Version: manifest.Version,
			Description: manifest.Description, Author: manifest.Author, Runtime: manifest.Runtime,
			Dir: dir, Manifest: manifest, State: StateFailed, LastError: err.Error(), Reason: err.Error()})
		return err
	}
	m.registry.Upsert(Entry{
		ID: manifest.ID, Name: manifest.Name, Version: manifest.Version,
		Description: manifest.Description, Author: manifest.Author, Runtime: manifest.Runtime,
		Dir: dir, Manifest: manifest, State: StateDiscovered,
	})
	return m.startPlugin(manifest, dir)
}

func (m *Manager) Unload(ctx context.Context, id string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("plugin %s is not loaded", id)
	}
	s.stopping.Store(true)
	m.registry.Update(id, func(e *Entry) { e.State = StateStopping })
	if process := s.currentProcess(); process != nil {
		if err := process.Stop(ctx); err != nil {
			m.logger.Error("[plugin:%s] stop failed: %v", id, err)
		}
	}
	s.cancel()
	m.bot.UnloadScope("plugin:" + id)
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	m.registry.Update(id, func(e *Entry) {
		e.State = StateStopped
		e.PID = 0
		e.Commands = nil
		e.GrantedCapabilities = nil
		e.ConfigSchema = nil
		e.Events = nil
		e.Methods = nil
	})
	return nil
}

func (m *Manager) Reload(ctx context.Context, id string) error {
	if err := m.Unload(ctx, id); err != nil {
		if !strings.Contains(err.Error(), "not loaded") {
			return err
		}
	}
	return m.Load(ctx, id)
}

func (m *Manager) ReloadAll(ctx context.Context) {
	m.lifecycleMu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.lifecycleMu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		if err := m.Reload(ctx, id); err != nil {
			m.logger.Error("failed to reload plugin %s: %v", id, err)
		}
	}
}

func (m *Manager) Toggle(ctx context.Context, id string, enabled bool) error {
	settings := m.pluginSettings(id)
	next := map[string]any{}
	for key, value := range settings {
		next[key] = value
	}
	next["enabled"] = enabled
	if err := m.botCfg.Set("plugin_config."+id, next); err != nil {
		return err
	}
	if enabled {
		if m.IsLoaded(id) {
			return nil
		}
		return m.Load(ctx, id)
	}
	if m.IsLoaded(id) {
		return m.Unload(ctx, id)
	}
	m.registry.Update(id, func(e *Entry) { e.State = StateDisabled })
	return nil
}

func (m *Manager) GetConfig(id string) (map[string]any, *SchemaInfo) {
	settings := m.pluginSettings(id)
	out := map[string]any{}
	for key, value := range settings {
		out[key] = value
	}
	entry, ok := m.registry.Get(id)
	if !ok || len(entry.ConfigSchema) == 0 {
		return out, nil
	}
	return out, &SchemaInfo{Fields: entry.ConfigSchema}
}

// SetConfig validates, persists and pushes the configuration to the process.
func (m *Manager) SetConfig(ctx context.Context, id string, raw map[string]any) (map[string]any, error) {
	entry, ok := m.registry.Get(id)
	if !ok {
		return nil, fmt.Errorf("plugin %s not found", id)
	}
	validated, errors := ValidateConfig(raw, entry.ConfigSchema)
	if len(errors) > 0 {
		for _, message := range errors {
			if strings.Contains(message, "required") {
				return nil, fmt.Errorf("%s", strings.Join(errors, ", "))
			}
		}
		m.logger.Warn("Plugin %s config validation warnings: %s", id, strings.Join(errors, ", "))
	}
	existing := m.pluginSettings(id)
	enabled := true
	if value, ok := existing["enabled"].(bool); ok {
		enabled = value
	}
	if value, ok := raw["enabled"].(bool); ok {
		enabled = value
	}
	next := map[string]any{}
	for key, value := range validated {
		next[key] = value
	}
	next["enabled"] = enabled
	if err := m.botCfg.Set("plugin_config."+id, next); err != nil {
		return nil, err
	}
	if s := m.session(id); s != nil {
		if peer := s.rpcPeer(); peer != nil {
			callCtx, cancel := context.WithTimeout(ctx, s.manifest.Timeout())
			_, err := peer.Call(callCtx, pluginrpc.MethodConfigure, map[string]any{"config": validated})
			cancel()
			if err != nil {
				m.logger.Warn("[plugin:%s] plugin.configure failed: %v", id, err)
			}
		}
	}
	return next, nil
}

func (m *Manager) session(id string) *session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

func (m *Manager) SessionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// ValidateConfig fills defaults, coerces types and reports errors.
func ValidateConfig(raw map[string]any, schema []pluginrpc.ConfigField) (map[string]any, []string) {
	out := map[string]any{}
	errs := []string{}
	if raw == nil {
		raw = map[string]any{}
	}
	for _, field := range schema {
		value, exists := raw[field.Key]
		if !exists || value == nil {
			if field.Required {
				errs = append(errs, fmt.Sprintf("Config field %q is required but missing", field.Key))
				continue
			}
			value = field.Default
		}
		coerced, err := coerceValue(value, field.Type)
		if err != nil {
			errs = append(errs, fmt.Sprintf("Config field %q: %v", field.Key, err))
			value = field.Default
		}
		out[field.Key] = coerced
	}
	return out, errs
}

func coerceValue(value any, fieldType string) (any, error) {
	switch fieldType {
	case "boolean":
		switch typed := value.(type) {
		case bool:
			return typed, nil
		case string:
			if typed == "true" || typed == "1" {
				return true, nil
			}
			if typed == "false" || typed == "0" {
				return false, nil
			}
		case float64:
			return typed != 0, nil
		case int:
			return typed != 0, nil
		}
		return nil, fmt.Errorf("expected boolean, got %T", value)
	case "number":
		switch typed := value.(type) {
		case float64:
			return typed, nil
		case int:
			return float64(typed), nil
		case int64:
			return float64(typed), nil
		case string:
			var parsed float64
			if _, err := fmt.Sscan(typed, &parsed); err == nil {
				return parsed, nil
			}
			return nil, fmt.Errorf("cannot convert %q to number", typed)
		}
		return nil, fmt.Errorf("expected number, got %T", value)
	case "string":
		if text, ok := value.(string); ok {
			return text, nil
		}
		return fmt.Sprint(value), nil
	case "string[]":
		switch typed := value.(type) {
		case []any:
			out := make([]string, len(typed))
			for i, item := range typed {
				out[i] = fmt.Sprint(item)
			}
			return out, nil
		case []string:
			return typed, nil
		}
		return nil, fmt.Errorf("expected array, got %T", value)
	case "object":
		if object, ok := value.(map[string]any); ok {
			return object, nil
		}
		return nil, fmt.Errorf("expected object, got %T", value)
	default:
		return value, nil
	}
}

func validateRegistration(id string, manifest *Manifest, result *pluginrpc.InitializeResult) error {
	if result.ID != "" && result.ID != id {
		return fmt.Errorf("plugin declared id %q but the manifest says %q", result.ID, id)
	}
	if result.APIVersion != "" && result.APIVersion != pluginrpc.APIVersion {
		return fmt.Errorf("plugin reported api version %q, host supports %q", result.APIVersion, pluginrpc.APIVersion)
	}
	allowed := map[string]bool{}
	for _, capability := range manifest.Capabilities {
		allowed[capability] = true
	}
	for _, capability := range result.Capabilities {
		if !IsKnownCapability(capability) {
			return fmt.Errorf("plugin declared unknown capability %q", capability)
		}
		if len(allowed) > 0 && !allowed[capability] {
			return fmt.Errorf("plugin declared capability %q which is not listed in its manifest", capability)
		}
	}
	seen := map[string]bool{}
	for _, spec := range result.Commands {
		name := strings.TrimSpace(spec.Command)
		if name == "" {
			return fmt.Errorf("plugin declared a command with an empty name")
		}
		if strings.ContainsAny(name, " \t\n") {
			return fmt.Errorf("plugin command %q must not contain whitespace", name)
		}
		if seen[name] {
			return fmt.Errorf("plugin declared command %q twice", name)
		}
		seen[name] = true
	}
	return nil
}

func argsFromLine(raw string) map[string]any {
	parsed := args.Parse(strings.Fields(raw))
	encoded, err := parsed.MarshalJSON()
	if err != nil {
		return map[string]any{"_": []string{}}
	}
	out := map[string]any{}
	if err := json.Unmarshal(encoded, &out); err != nil {
		return map[string]any{"_": []string{}}
	}
	return out
}

func runtimePlatform() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
