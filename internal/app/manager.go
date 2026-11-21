package app

import (
	"context"
	"fmt"
	"sync"

	"github.com/uniu-network/uniubot/internal/core/config"
	"github.com/uniu-network/uniubot/internal/core/logging"
	"github.com/uniu-network/uniubot/internal/storage"
)

type Manager struct {
	globalConfig *config.Store
	storage      *storage.Manager
	logger       *logging.Logger
	version      string
	pluginDir    string

	// baseCtx outlives HTTP requests: starting or reloading a bot through the API
	// must not tie the instance to that request.
	baseCtx context.Context

	mu        sync.RWMutex
	instances map[string]*BotInstance
	// order feeds FirstInstance, so the path-less POST /message endpoint is stable.
	order []string
}

func NewManager(globalConfig *config.Store, storageManager *storage.Manager, version string, logger *logging.Logger) *Manager {
	return &Manager{
		baseCtx:      context.Background(),
		globalConfig: globalConfig,
		storage:      storageManager,
		logger:       logger,
		version:      version,
		pluginDir:    "plugins",
		instances:    map[string]*BotInstance{},
	}
}

func (m *Manager) SetPluginDir(dir string) { m.pluginDir = dir }

func (m *Manager) LoadAll(ctx context.Context) error {
	if ctx != nil {
		m.mu.Lock()
		m.baseCtx = ctx
		m.mu.Unlock()
	}
	ids := config.ListBotIDs()
	m.logger.Info("[BotManager] Found %d bot configuration(s): %v", len(ids), ids)
	for _, botID := range ids {
		if err := m.StartInstance(ctx, botID); err != nil {
			continue
		}
	}
	return nil
}

func (m *Manager) lifetimeContext() context.Context {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.baseCtx == nil {
		return context.Background()
	}
	return m.baseCtx
}

// StartInstance uses ctx for startup only; the instance lives on the manager
// lifetime context.
func (m *Manager) StartInstance(ctx context.Context, botID string) error {
	_ = ctx
	m.mu.RLock()
	_, exists := m.instances[botID]
	m.mu.RUnlock()
	if exists {
		m.logger.Warn("[BotManager] Bot %s is already loaded", botID)
		return nil
	}
	instance, err := Create(InstanceOptions{
		BotID:        botID,
		Version:      m.version,
		GlobalConfig: m.globalConfig,
		Storage:      m.storage,
		Logger:       m.logger,
		PluginDir:    m.pluginDir,
	})
	if err != nil {
		m.logger.Error("[BotManager] Failed to start bot %s: %v", botID, err)
		return err
	}
	lifetime := m.lifetimeContext()
	if err := instance.Start(lifetime); err != nil {
		m.logger.Error("[BotManager] Failed to start bot %s: %v", botID, err)
		_ = instance.Stop(lifetime)
		return err
	}
	m.mu.Lock()
	if _, exists := m.instances[botID]; !exists {
		m.order = append(m.order, botID)
	}
	m.instances[botID] = instance
	m.mu.Unlock()
	return nil
}

func (m *Manager) StopInstance(ctx context.Context, botID string) error {
	m.mu.RLock()
	instance, exists := m.instances[botID]
	m.mu.RUnlock()
	if !exists {
		m.logger.Warn("[BotManager] Bot %s not found", botID)
		return nil
	}
	if err := instance.Stop(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.instances, botID)
	order := m.order[:0]
	for _, id := range m.order {
		if id != botID {
			order = append(order, id)
		}
	}
	m.order = order
	m.mu.Unlock()
	return nil
}

func (m *Manager) ReloadInstance(ctx context.Context, botID string) error {
	if err := m.StopInstance(ctx, botID); err != nil {
		return err
	}
	return m.StartInstance(ctx, botID)
}

func (m *Manager) HasInstance(botID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.instances[botID]
	return ok
}

func (m *Manager) GetInstance(botID string) (*BotInstance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	instance, ok := m.instances[botID]
	return instance, ok
}

// FirstInstance is the first bot in load order.
func (m *Manager) FirstInstance() (*BotInstance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, botID := range m.order {
		if instance, ok := m.instances[botID]; ok {
			return instance, true
		}
	}
	return nil, false
}

func (m *Manager) ListInstances() []BotInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]BotInfo, 0, len(m.instances))
	for _, botID := range m.order {
		if instance, ok := m.instances[botID]; ok {
			out = append(out, instance.Info())
		}
	}
	return out
}

func (m *Manager) ListAllBots() []BotInfo {
	ids := config.ListBotIDs()
	out := make([]BotInfo, 0, len(ids))
	for _, botID := range ids {
		if instance, ok := m.GetInstance(botID); ok {
			out = append(out, instance.Info())
			continue
		}
		store, err := config.OpenBot(botID)
		if err != nil {
			out = append(out, BotInfo{BotID: botID, Name: botID, Prefix: []string{"/"}})
			continue
		}
		out = append(out, BotInfo{
			BotID:   botID,
			Name:    store.String("name", botID),
			SelfID:  store.Int64("self_id", 0),
			Running: false,
			Prefix:  config.NormalizePrefix(store.Get("prefix", nil)),
		})
	}
	return out
}

type CreateOptions struct {
	BotID             string
	Name              string
	SelfID            *int64
	Prefix            []string
	ErrorReplyEnabled *bool
	Admin             []string
	Adapters          []any
	PluginConfig      map[string]any
}

func (m *Manager) CreateInstance(ctx context.Context, opts CreateOptions) (*BotInstance, error) {
	if config.BotConfigExists(opts.BotID) {
		return nil, fmt.Errorf("Bot %q already exists", opts.BotID)
	}
	if !config.ValidBotID(opts.BotID) {
		return nil, fmt.Errorf("botId can only contain letters, numbers, underscores, and hyphens")
	}
	name := opts.Name
	if name == "" {
		name = opts.BotID
	}
	selfID := int64(10000)
	if opts.SelfID != nil {
		selfID = *opts.SelfID
	}
	errorReply := true
	if opts.ErrorReplyEnabled != nil {
		errorReply = *opts.ErrorReplyEnabled
	}
	prefix := opts.Prefix
	if len(prefix) == 0 {
		prefix = []string{"/"}
	}
	template := config.BotConfigTemplate{
		Name:              name,
		SelfID:            selfID,
		Prefix:            prefix,
		ErrorReplyEnabled: errorReply,
		Admin:             opts.Admin,
		Adapters:          opts.Adapters,
		PluginConfig:      opts.PluginConfig,
	}
	if err := config.CreateBotConfigFile(opts.BotID, template); err != nil {
		return nil, err
	}
	if err := m.StartInstance(ctx, opts.BotID); err != nil {
		return nil, fmt.Errorf("Failed to start bot %q after creation: %w", opts.BotID, err)
	}
	instance, ok := m.GetInstance(opts.BotID)
	if !ok {
		return nil, fmt.Errorf("Failed to start bot %q after creation", opts.BotID)
	}
	return instance, nil
}

func (m *Manager) DeleteInstance(ctx context.Context, botID string) error {
	if m.HasInstance(botID) {
		if err := m.StopInstance(ctx, botID); err != nil {
			return err
		}
	}
	if !config.BotConfigExists(botID) {
		return fmt.Errorf("Bot config %q not found", botID)
	}
	return config.DeleteBotConfigFile(botID)
}

func (m *Manager) StopAll(ctx context.Context) {
	m.mu.RLock()
	instances := make(map[string]*BotInstance, len(m.instances))
	for id, instance := range m.instances {
		instances[id] = instance
	}
	m.mu.RUnlock()
	for _, id := range m.order {
		instance, ok := instances[id]
		if !ok {
			continue
		}
		if err := instance.Stop(ctx); err != nil {
			m.logger.Error("[BotManager] Error stopping bot %s: %v", id, err)
		}
	}
	m.mu.Lock()
	m.instances = map[string]*BotInstance{}
	m.order = nil
	m.mu.Unlock()
}

func (m *Manager) GlobalConfig() *config.Store { return m.globalConfig }

func (m *Manager) Version() string { return m.version }

func (m *Manager) Logger() *logging.Logger { return m.logger }

func (m *Manager) Storage() *storage.Manager { return m.storage }
