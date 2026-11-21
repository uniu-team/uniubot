// Package app wires the core, adapters, plugins, storage and services into bot
// instances and manages their lifecycle.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/uniu-network/uniubot/internal/adapters"
	"github.com/uniu-network/uniubot/internal/bot"
	"github.com/uniu-network/uniubot/internal/core/config"
	"github.com/uniu-network/uniubot/internal/core/events"
	"github.com/uniu-network/uniubot/internal/core/logging"
	"github.com/uniu-network/uniubot/internal/plugins"
	"github.com/uniu-network/uniubot/internal/services"
	"github.com/uniu-network/uniubot/internal/storage"
)

type InstanceOptions struct {
	BotID        string
	Version      string
	GlobalConfig *config.Store
	Storage      *storage.Manager
	Logger       *logging.Logger
	PluginDir    string
}

type BotInstance struct {
	BotID        string
	Version      string
	GlobalConfig *config.Store
	Config       *config.Store
	Bot          *bot.Bot
	Adapters     *adapters.Manager
	Plugins      *plugins.Manager
	Counter      *services.Counter
	Scheduler    *services.Scheduler

	logger  *logging.Logger
	storage *storage.Manager
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	running bool
}

type BotInfo struct {
	BotID        string   `json:"botId"`
	Name         string   `json:"name"`
	SelfID       int64    `json:"self_id"`
	Running      bool     `json:"running"`
	PluginCount  int      `json:"pluginCount"`
	AdapterCount int      `json:"adapterCount"`
	Prefix       []string `json:"prefix"`
}

func NewInstance(opts InstanceOptions) (*BotInstance, error) {
	botConfig, err := config.OpenBot(opts.BotID)
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = logging.Default()
	}
	prefix := config.NormalizePrefix(botConfig.Get("prefix", nil))
	selfID := botConfig.Int64("self_id", 0)
	admins := toStringSlice(botConfig.Get("admin", nil))

	core := bot.New(bot.Options{
		SelfID:            selfID,
		Prefix:            prefix,
		ErrorReplyEnabled: botConfig.Bool("error_reply_enabled", true),
		Admins:            admins,
		Logger:            logger.WithPrefix("[bot:" + opts.BotID + "]"),
	})
	adapterManager := adapters.NewManager(core, botConfig, opts.GlobalConfig, logger.WithPrefix("[bot:"+opts.BotID+"]"))

	counterDB, err := opts.Storage.BotCore(opts.BotID, "counter")
	if err != nil {
		return nil, err
	}
	instance := &BotInstance{
		BotID:        opts.BotID,
		Version:      opts.Version,
		GlobalConfig: opts.GlobalConfig,
		Config:       botConfig,
		Bot:          core,
		Adapters:     adapterManager,
		Counter:      services.NewCounter(counterDB, logger),
		logger:       logger,
		storage:      opts.Storage,
	}
	instance.Plugins = plugins.New(plugins.Options{
		BotID:        opts.BotID,
		BotName:      botConfig.String("name", opts.BotID),
		Version:      opts.Version,
		Bot:          core,
		Adapters:     adapterManager,
		BotConfig:    botConfig,
		GlobalConfig: opts.GlobalConfig,
		Storage:      opts.Storage,
		Logger:       logger,
		PluginDir:    opts.PluginDir,
	})
	instance.Adapters.SetDBProvider(func(adapterID string) (*storage.ScopedClient, error) {
		return opts.Storage.BotAdapter(opts.BotID, adapterID)
	})
	return instance, nil
}

func toStringSlice(value any) []string {
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, fmt.Sprint(item))
		}
		return out
	case []string:
		return typed
	case string:
		if typed == "" {
			return nil
		}
		return []string{typed}
	default:
		return nil
	}
}

func Create(opts InstanceOptions) (*BotInstance, error) {
	instance, err := NewInstance(opts)
	if err != nil {
		return nil, err
	}
	if err := instance.Counter.Init(context.Background()); err != nil {
		return nil, err
	}
	return instance, nil
}

func (i *BotInstance) Start(ctx context.Context) error {
	i.mu.Lock()
	if i.running {
		i.mu.Unlock()
		return nil
	}
	i.running = true
	i.ctx, i.cancel = context.WithCancel(ctx)
	instanceCtx := i.ctx
	i.mu.Unlock()

	i.logger.Info("[BotInstance:%s] Starting bot %q", i.BotID, i.Config.String("name", i.BotID))

	registerEventHandlers(i)
	i.Bot.Start()

	if err := i.Plugins.Start(instanceCtx); err != nil {
		i.logger.Error("[BotInstance:%s] plugin startup error: %v", i.BotID, err)
	}
	if err := i.Adapters.LoadFromConfig(instanceCtx); err != nil {
		i.logger.Error("[BotInstance:%s] adapter startup error: %v", i.BotID, err)
	}

	schedulerConfig := services.SchedulerConfig{
		RetentionDays: i.GlobalConfig.Int("database.message_retention_days", 0),
		CleanupTime:   i.GlobalConfig.String("database.cleanup_time", "01:00"),
		Timezone:      i.GlobalConfig.String("database.timezone", ""),
	}
	i.Scheduler = services.NewScheduler(i.Counter, schedulerConfig, i.logger)
	i.Scheduler.Start(instanceCtx)

	i.logger.Info("[BotInstance:%s] Bot started", i.BotID)
	return nil
}

func (i *BotInstance) Stop(ctx context.Context) error {
	i.mu.Lock()
	if !i.running {
		i.mu.Unlock()
		return nil
	}
	i.running = false
	cancel := i.cancel
	i.mu.Unlock()

	i.logger.Info("[BotInstance:%s] Stopping bot", i.BotID)
	if err := i.Adapters.StopAll(ctx); err != nil {
		i.logger.Error("[BotInstance:%s] failed to stop adapters: %v", i.BotID, err)
	}
	i.Plugins.Stop(ctx)
	if i.Scheduler != nil {
		i.Scheduler.Stop()
	}
	if cancel != nil {
		cancel()
	}
	i.Bot.Stop()
	i.logger.Info("[BotInstance:%s] Bot stopped", i.BotID)
	return nil
}

func (i *BotInstance) IsRunning() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.running
}

func (i *BotInstance) Info() BotInfo {
	return BotInfo{
		BotID:        i.BotID,
		Name:         i.Config.String("name", i.BotID),
		SelfID:       i.Bot.SelfID,
		Running:      i.IsRunning(),
		PluginCount:  i.Plugins.Count(),
		AdapterCount: i.Adapters.Count(),
		Prefix:       i.Bot.Prefix,
	}
}

// registerEventHandlers logs first, then counts.
func registerEventHandlers(i *BotInstance) {
	logger := i.logger
	counter := i.Counter
	botID := i.BotID

	i.Bot.On("message", func(ctx context.Context, event *events.Event, handler *bot.Handler) error {
		if !event.IsMessage() {
			return nil
		}
		logger.Info("[%s] %s", botID, formatEvent(event))
		debugEvent(logger, botID, event)
		if err := counter.AddMessage(ctx, event, "recv"); err != nil {
			logger.Error("[%s] failed to store message: %v", botID, err)
		}
		if event.Kind == events.KindGroupMessage {
			if err := counter.AddGroup(ctx, event.GroupID); err != nil {
				logger.Error("[%s] failed to store group: %v", botID, err)
			}
		}
		if err := counter.AddUser(ctx, event.Sender.UserID); err != nil {
			logger.Error("[%s] failed to store user: %v", botID, err)
		}
		return nil
	})

	i.Bot.On("bot_send", func(ctx context.Context, event *events.Event, handler *bot.Handler) error {
		logger.Info("[%s] %s", botID, formatEvent(event))
		debugEvent(logger, botID, event)
		if err := counter.AddBotSendMessage(ctx, event); err != nil {
			logger.Error("[%s] failed to store sent message: %v", botID, err)
		}
		return nil
	})

	for _, name := range []string{"meta_event", "request", "notice"} {
		i.Bot.On(name, func(ctx context.Context, event *events.Event, handler *bot.Handler) error {
			logger.Info("[%s] %s", botID, formatEvent(event))
			debugEvent(logger, botID, event)
			return nil
		})
	}
}

func formatEvent(event *events.Event) string {
	base := fmt.Sprintf("adapter=%s self=%d", event.AdapterID, event.SelfID)
	switch event.Kind {
	case events.KindGroupMessage:
		return fmt.Sprintf("message group %s group=%d user=%d message_id=%v text=%s",
			base, event.GroupID, event.UserID, event.MessageID, quote(event.RawMessage))
	case events.KindPrivateMessage:
		return fmt.Sprintf("message private %s user=%d message_id=%v text=%s",
			base, event.UserID, event.MessageID, quote(event.RawMessage))
	case events.KindBotSend:
		return fmt.Sprintf("bot_send %s %s target=%d", event.SendMessageType, base, event.TargetID)
	case events.KindNotice:
		return fmt.Sprintf("notice %s type=%s", base, event.NoticeType)
	case events.KindRequest:
		return fmt.Sprintf("request %s type=%s", base, event.RequestType)
	case events.KindMeta:
		return fmt.Sprintf("meta_event %s type=%s", base, event.MetaEventType)
	default:
		return "event " + base
	}
}

func quote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func debugEvent(logger *logging.Logger, botID string, event *events.Event) {
	if !logger.IsLevelEnabled(logging.LevelDebug) {
		return
	}
	payload, err := event.Payload()
	if err != nil {
		return
	}
	logger.Debug("[%s] %s", botID, string(payload))
}
