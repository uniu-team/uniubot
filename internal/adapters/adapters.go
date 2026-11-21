// Package adapters is the adapter contract, the compiled catalog and the per bot
// manager. Adapters are in-process Go components; only plugins run out of process.
package adapters

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/uniu-network/uniubot/internal/bot"
	"github.com/uniu-network/uniubot/internal/core/config"
	"github.com/uniu-network/uniubot/internal/core/events"
	"github.com/uniu-network/uniubot/internal/core/logging"
	"github.com/uniu-network/uniubot/internal/core/message"
	"github.com/uniu-network/uniubot/internal/storage"
)

type Deps struct {
	Bot    *bot.Bot
	DB     *storage.ScopedClient
	BotCfg *config.Store
	Global *config.Store
	Logger *logging.Logger
	Emit   func(eventName string, event *events.Event)
}

type Adapter interface {
	ID() string
	Name() string
	Start(ctx context.Context, deps Deps) error
	Stop(ctx context.Context) error
}

type HTTPRequest struct {
	Body    any
	RawBody string
	Headers map[string]string
}

func (r *HTTPRequest) Header(name string) string {
	for key, value := range r.Headers {
		if equalFoldASCII(key, name) {
			return value
		}
	}
	return ""
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// HTTPResponse: an object Body is encoded as JSON, anything else as plain text.
type HTTPResponse struct {
	Status int
	Body   any
}

type HTTPAdapter interface {
	HandleHTTPRequest(ctx context.Context, req *HTTPRequest) (*HTTPResponse, bool)
}

type Meta struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	DefaultConfig map[string]any `json:"defaultConfig"`
}

type Factory func(config map[string]any) Adapter

type registration struct {
	Meta
	factory Factory
}

var (
	catalogMu    sync.RWMutex
	catalog      = map[string]registration{}
	catalogOrder []string
)

// Register is called from adapter package init functions.
func Register(meta Meta, factory Factory) {
	if meta.ID == "" || factory == nil {
		panic("adapters: Register requires an id and a factory")
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if _, exists := catalog[meta.ID]; exists {
		panic("adapters: duplicate adapter id " + meta.ID)
	}
	if meta.DefaultConfig == nil {
		meta.DefaultConfig = map[string]any{}
	}
	catalog[meta.ID] = registration{Meta: meta, factory: factory}
	catalogOrder = append(catalogOrder, meta.ID)
}

func Catalog() []Meta {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	out := make([]Meta, 0, len(catalogOrder))
	for _, id := range catalogOrder {
		out = append(out, catalog[id].Meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func MetaOf(id string) (Meta, bool) {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	entry, ok := catalog[id]
	return entry.Meta, ok
}

func New(id string, cfg map[string]any) (Adapter, bool) {
	catalogMu.RLock()
	entry, ok := catalog[id]
	catalogMu.RUnlock()
	if !ok {
		return nil, false
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return entry.factory(cfg), true
}

type Info struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Loaded bool           `json:"loaded"`
	Config map[string]any `json:"config"`
}

type DBProvider func(adapterID string) (*storage.ScopedClient, error)

type Manager struct {
	bot      *bot.Bot
	botCfg   *config.Store
	global   *config.Store
	logger   *logging.Logger
	dbSource DBProvider

	mu        sync.RWMutex
	adapters  map[string]Adapter
	configs   map[string]map[string]any
	order     []string
	reloading bool
}

func NewManager(b *bot.Bot, botCfg, global *config.Store, logger *logging.Logger) *Manager {
	return &Manager{
		bot:      b,
		botCfg:   botCfg,
		global:   global,
		logger:   logger,
		adapters: map[string]Adapter{},
		configs:  map[string]map[string]any{},
	}
}

func (m *Manager) SetDBProvider(provider DBProvider) { m.dbSource = provider }

func (m *Manager) adapterDB(id string) *storage.ScopedClient {
	if m.dbSource == nil {
		return nil
	}
	db, err := m.dbSource(id)
	if err != nil {
		m.logger.Warn("failed to create database scope for adapter %s: %v", id, err)
		return nil
	}
	return db
}

func (m *Manager) RegisterAdapter(ctx context.Context, adapter Adapter) error {
	deps := Deps{
		Bot:    m.bot,
		DB:     m.adapterDB(adapter.ID()),
		BotCfg: m.botCfg,
		Global: m.global,
		Logger: m.logger,
		Emit:   m.bot.EmitEvent,
	}
	if err := adapter.Start(ctx, deps); err != nil {
		return err
	}
	if err := m.bot.UseAdapter(ctx, asSender(adapter)); err != nil {
		_ = adapter.Stop(ctx)
		return err
	}
	m.mu.Lock()
	if _, exists := m.adapters[adapter.ID()]; !exists {
		m.order = append(m.order, adapter.ID())
	}
	m.adapters[adapter.ID()] = adapter
	m.mu.Unlock()
	m.logger.Info("Adapter %q (%s) registered", adapter.Name(), adapter.ID())
	return nil
}

func asSender(adapter Adapter) events.Sender {
	if sender, ok := adapter.(events.Sender); ok {
		return sender
	}
	return unsupportedSender{adapter: adapter}
}

// unsupportedSender keeps the registry consistent for adapters that cannot send.
type unsupportedSender struct{ adapter Adapter }

func (u unsupportedSender) ID() string   { return u.adapter.ID() }
func (u unsupportedSender) Name() string { return u.adapter.Name() }
func (u unsupportedSender) SendGroup(context.Context, string, message.Message) error {
	return fmt.Errorf("adapter %s cannot send messages", u.adapter.ID())
}
func (u unsupportedSender) SendPrivate(context.Context, string, message.Message) error {
	return fmt.Errorf("adapter %s cannot send messages", u.adapter.ID())
}

func (m *Manager) LoadFromConfig(ctx context.Context) error {
	configured := m.botCfg.Slice("adapters")
	for _, raw := range configured {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		adapterType := fmt.Sprint(entry["type"])
		if adapterType == "" || adapterType == "<nil>" {
			continue
		}
		cfg, _ := entry["config"].(map[string]any)
		if cfg == nil {
			cfg = map[string]any{}
		}
		if err := m.LoadAdapter(ctx, adapterType, cfg); err != nil {
			m.logger.Error("failed to load adapter %s: %v", adapterType, err)
		}
	}
	return nil
}

func (m *Manager) LoadAdapter(ctx context.Context, adapterType string, cfg map[string]any) error {
	adapter, ok := New(adapterType, cfg)
	if !ok {
		return fmt.Errorf("adapter %q is not compiled into this binary", adapterType)
	}
	m.mu.Lock()
	m.configs[adapterType] = cfg
	m.mu.Unlock()
	return m.RegisterAdapter(ctx, adapter)
}

func (m *Manager) UnloadAdapter(ctx context.Context, id string) error {
	m.mu.Lock()
	adapter, exists := m.adapters[id]
	m.mu.Unlock()
	if !exists {
		return nil
	}
	if err := adapter.Stop(ctx); err != nil {
		m.logger.Error("Error stopping adapter %s: %v", id, err)
	}
	m.bot.RemoveAdapter(id)
	m.mu.Lock()
	delete(m.adapters, id)
	delete(m.configs, id)
	order := m.order[:0]
	for _, existing := range m.order {
		if existing != id {
			order = append(order, existing)
		}
	}
	m.order = order
	m.mu.Unlock()
	return nil
}

func (m *Manager) ReloadAdapter(ctx context.Context, adapterType string) error {
	if err := m.botCfg.Reload(); err != nil {
		m.logger.Warn("failed to reload bot config: %v", err)
	}
	configured := m.botCfg.Slice("adapters")
	var cfg map[string]any
	for _, raw := range configured {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprint(entry["type"]) != adapterType {
			continue
		}
		cfg, _ = entry["config"].(map[string]any)
	}
	if cfg == nil {
		m.mu.RLock()
		cfg = m.configs[adapterType]
		m.mu.RUnlock()
	}
	if cfg == nil {
		return nil
	}
	m.logger.Info("Reloading adapter %s", adapterType)
	if err := m.UnloadAdapter(ctx, adapterType); err != nil {
		return err
	}
	return m.LoadAdapter(ctx, adapterType, cloneConfig(cfg))
}

func (m *Manager) ReloadAll(ctx context.Context) error {
	m.mu.Lock()
	m.reloading = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.reloading = false
		m.mu.Unlock()
	}()
	if err := m.botCfg.Reload(); err != nil {
		m.logger.Warn("failed to reload bot config: %v", err)
	}
	if err := m.StopAll(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.configs = map[string]map[string]any{}
	m.mu.Unlock()
	return m.LoadFromConfig(ctx)
}

func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	adapters := make([]Adapter, 0, len(m.adapters))
	ids := make([]string, 0, len(m.adapters))
	for id, adapter := range m.adapters {
		ids = append(ids, id)
		adapters = append(adapters, adapter)
	}
	m.mu.Unlock()
	for i, adapter := range adapters {
		if err := adapter.Stop(ctx); err != nil {
			m.logger.Error("Error stopping adapter %s: %v", ids[i], err)
		}
		m.bot.RemoveAdapter(ids[i])
	}
	m.mu.Lock()
	m.adapters = map[string]Adapter{}
	m.order = nil
	m.mu.Unlock()
	return nil
}

func (m *Manager) Get(id string) (Adapter, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	adapter, ok := m.adapters[id]
	return adapter, ok
}

func (m *Manager) List() []Info {
	result := []Info{}
	seen := map[string]bool{}
	for _, raw := range m.botCfg.Slice("adapters") {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := fmt.Sprint(entry["type"])
		cfg, _ := entry["config"].(map[string]any)
		if cfg == nil {
			cfg = map[string]any{}
		}
		m.mu.RLock()
		adapter, loaded := m.adapters[id]
		m.mu.RUnlock()
		name := id
		if loaded {
			name = adapter.Name()
		}
		result = append(result, Info{ID: id, Name: name, Loaded: loaded, Config: cfg})
		seen[id] = true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range m.order {
		if seen[id] {
			continue
		}
		adapter, ok := m.adapters[id]
		if !ok {
			continue
		}
		cfg := m.configs[id]
		if cfg == nil {
			cfg = map[string]any{}
		}
		result = append(result, Info{ID: id, Name: adapter.Name(), Loaded: true, Config: cfg})
	}
	return result
}

// HandleHTTPRequest offers the request to every adapter until one handles it.
func (m *Manager) HandleHTTPRequest(ctx context.Context, req *HTTPRequest) (*HTTPResponse, bool) {
	m.mu.RLock()
	if m.reloading {
		m.mu.RUnlock()
		return &HTTPResponse{Status: 503, Body: "Adapters are reloading"}, true
	}
	handlers := make([]HTTPAdapter, 0, len(m.order))
	for _, id := range m.order {
		if adapter, ok := m.adapters[id]; ok {
			if handler, ok := adapter.(HTTPAdapter); ok {
				handlers = append(handlers, handler)
			}
		}
	}
	m.mu.RUnlock()
	for _, handler := range handlers {
		response, handled := handler.HandleHTTPRequest(ctx, req)
		if handled {
			return response, true
		}
	}
	return nil, false
}

func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.adapters)
}

func cloneConfig(cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg))
	for key, value := range cfg {
		out[key] = value
	}
	return out
}
