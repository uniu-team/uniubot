package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/uniu-network/uniubot/internal/core/events"
	"github.com/uniu-network/uniubot/internal/storage"
	"github.com/uniu-network/uniubot/pkg/pluginrpc"
)

// attachHostHandlers registers the host services; every handler checks that the
// capability was granted.
func (m *Manager) attachHostHandlers(s *session, peer *pluginrpc.Peer) {
	peer.Handle(pluginrpc.MethodLog, func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := s.require(CapLogger); err != nil {
			return nil, err
		}
		var params pluginrpc.LogParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, err.Error())
		}
		m.logPluginMessage(s.id, params)
		return map[string]any{"status": "ok"}, nil
	})

	peer.Handle(pluginrpc.MethodConfigGet, func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := s.require(CapConfigRead); err != nil {
			return nil, err
		}
		var params pluginrpc.ConfigGetParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, err.Error())
		}
		value, err := m.configGet(s.id, params)
		if err != nil {
			return nil, err
		}
		return map[string]any{"value": value}, nil
	})

	peer.Handle(pluginrpc.MethodBotInfo, func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := s.require(CapBotInfo); err != nil {
			return nil, err
		}
		return m.botInfo(), nil
	})

	peer.Handle(pluginrpc.MethodCommandList, func(ctx context.Context, _ json.RawMessage) (any, error) {
		if err := s.require(CapCommandList); err != nil {
			return nil, err
		}
		commands := m.bot.Commands()
		out := make([]map[string]any, 0, len(commands))
		for _, command := range commands {
			entry := map[string]any{
				"command":     command.Command,
				"description": command.Description,
				"requireRole": int(command.RequireRole),
			}
			if command.Scope != "" {
				entry["scope"] = command.Scope
			}
			out = append(out, entry)
		}
		return out, nil
	})

	peer.Handle(pluginrpc.MethodMessageReply, func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := s.require(CapMessageReply); err != nil {
			return nil, err
		}
		var params pluginrpc.MessageParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, err.Error())
		}
		value, ok := s.pending.Load(params.EventID)
		if !ok {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, "unknown eventId: "+params.EventID)
		}
		event, ok := value.(*events.Event)
		if !ok {
			return nil, pluginrpc.NewError(pluginrpc.CodePluginError, "event cannot be replied to")
		}
		if err := event.Reply(ctx, toMessage(params.Segments)); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodePluginError, err.Error())
		}
		return map[string]any{"status": "ok"}, nil
	})

	peer.Handle(pluginrpc.MethodMessageSend, func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := s.require(CapMessageSend); err != nil {
			return nil, err
		}
		var params pluginrpc.MessageParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, err.Error())
		}
		if err := m.sendMessage(ctx, params); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodePluginError, err.Error())
		}
		return map[string]any{"status": "ok"}, nil
	})

	peer.Handle(pluginrpc.MethodEventPublish, func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := s.require(CapEventPublish); err != nil {
			return nil, err
		}
		var params pluginrpc.EventPublishParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, err.Error())
		}
		if strings.TrimSpace(params.Name) == "" {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, "event name is required")
		}
		m.publishCustomEvent(s.id, params)
		return map[string]any{"status": "ok"}, nil
	})

	peer.Handle(pluginrpc.MethodDBCall, func(ctx context.Context, raw json.RawMessage) (any, error) {
		if err := s.require(CapDB); err != nil {
			return nil, err
		}
		var params pluginrpc.DBCallParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, err.Error())
		}
		result, err := m.databaseCall(ctx, s.id, params)
		if err != nil {
			return nil, pluginrpc.NewError(pluginrpc.CodePluginError, err.Error())
		}
		return result, nil
	})
}

func (s *session) require(capability string) error {
	if s.hasCapability(capability) {
		return nil
	}
	return pluginrpc.NewError(pluginrpc.CodeCapabilityDenied,
		fmt.Sprintf("plugin %s was not granted the %q capability", s.id, capability))
}

func (m *Manager) logPluginMessage(id string, params pluginrpc.LogParams) {
	logger := m.logger.WithPrefix("[plugin:" + id + "]")
	text := params.Message
	if len(params.Fields) > 0 {
		text = fmt.Sprintf("%s %v", text, params.Fields)
	}
	switch strings.ToUpper(params.Level) {
	case "TRACE":
		logger.Trace("%s", text)
	case "DEBUG":
		logger.Debug("%s", text)
	case "WARN", "WARNING":
		logger.Warn("%s", text)
	case "ERROR":
		logger.Error("%s", text)
	default:
		logger.Info("%s", text)
	}
}

func (m *Manager) configGet(pluginID string, params pluginrpc.ConfigGetParams) (any, error) {
	switch params.Scope {
	case "", "plugin":
		if params.Key == "" {
			return m.pluginConfigFor(pluginID), nil
		}
		return m.pluginConfigFor(pluginID)[params.Key], nil
	case "bot":
		switch params.Key {
		case "name", "self_id", "prefix", "error_reply_enabled", "adapters":
			return m.botCfg.Get(params.Key, nil), nil
		default:
			return nil, pluginrpc.NewError(pluginrpc.CodeCapabilityDenied,
				fmt.Sprintf("bot config key %q is not exposed to plugins", params.Key))
		}
	case "global":
		switch params.Key {
		case "log.level", "database.timezone", "web.port":
			return m.global.Get(params.Key, nil), nil
		default:
			return nil, pluginrpc.NewError(pluginrpc.CodeCapabilityDenied,
				fmt.Sprintf("global config key %q is not exposed to plugins", params.Key))
		}
	default:
		return nil, pluginrpc.NewError(pluginrpc.CodeInvalidParams, "unknown config scope: "+params.Scope)
	}
}

func (m *Manager) sendMessage(ctx context.Context, params pluginrpc.MessageParams) error {
	if m.adapters == nil {
		return fmt.Errorf("no adapters available")
	}
	var target string
	switch typed := params.TargetID.(type) {
	case nil:
		return fmt.Errorf("targetId is required")
	case string:
		target = typed
	case float64:
		target = fmt.Sprintf("%.0f", typed)
	case json.Number:
		target = typed.String()
	default:
		target = fmt.Sprint(typed)
	}
	sender, ok := m.bot.GetAdapter(params.MessageType)
	if !ok {
		return fmt.Errorf("adapter %q is not loaded", params.MessageType)
	}
	switch params.MessageType {
	case "group":
		return sender.SendGroup(ctx, target, toMessage(params.Segments))
	case "private":
		return sender.SendPrivate(ctx, target, toMessage(params.Segments))
	default:
		return fmt.Errorf("unsupported messageType %q (expected group or private)", params.MessageType)
	}
}

func (m *Manager) publishCustomEvent(pluginID string, params pluginrpc.EventPublishParams) {
	payload, err := json.Marshal(params.Payload)
	if err != nil {
		payload = nil
	}
	event := events.NewRaw(time.Now().Unix(), m.bot.SelfID, "plugin:"+pluginID, nil, params.Name, payload)
	m.bot.EmitEvent(params.Name, event)

	for _, s := range m.readySessions() {
		if s.id == pluginID || !m.wantsEvent(s, params.Name) {
			continue
		}
		eventID := m.newEventID()
		s.pending.Store(eventID, event)
		m.invoke(s, pluginrpc.MethodEvent, pluginrpc.EventParams{
			EventName: params.Name,
			Event:     payload,
			EventID:   eventID,
			Bot:       m.botInfo(),
		}, event, nil, eventID)
	}
}

func (m *Manager) databaseCall(ctx context.Context, pluginID string, params pluginrpc.DBCallParams) (any, error) {
	if m.storage == nil {
		return nil, fmt.Errorf("database is not available")
	}
	client, err := m.storage.BotPlugin(m.botID, pluginID)
	if err != nil {
		return nil, err
	}
	return runScopedOperation(ctx, client, params)
}

func runScopedOperation(ctx context.Context, client *storage.ScopedClient, params pluginrpc.DBCallParams) (any, error) {
	options := storage.QueryOptions{}
	if params.Options != nil {
		options.Limit = params.Options.Limit
		options.Offset = params.Options.Offset
		options.OrderBy = params.Options.OrderBy
		options.Order = params.Options.Order
	}
	switch params.Op {
	case "createTable":
		if len(params.Schema) == 0 {
			return nil, fmt.Errorf("schema is required for createTable")
		}
		var schema storage.TableSchema
		if err := json.Unmarshal(params.Schema, &schema); err != nil {
			return nil, err
		}
		return map[string]any{"status": "ok"}, client.CreateTable(ctx, params.Table, schema)
	case "dropTable":
		return map[string]any{"status": "ok"}, client.DropTable(ctx, params.Table)
	case "insert":
		result, err := client.Insert(ctx, params.Table, params.Data)
		if err != nil {
			return nil, err
		}
		return result, nil
	case "insertMany":
		return map[string]any{"status": "ok"}, client.InsertMany(ctx, params.Table, params.Rows)
	case "update":
		changes, err := client.Update(ctx, params.Table, params.Data, params.Where)
		if err != nil {
			return nil, err
		}
		return map[string]any{"changes": changes}, nil
	case "delete":
		changes, err := client.Delete(ctx, params.Table, params.Where)
		if err != nil {
			return nil, err
		}
		return map[string]any{"changes": changes}, nil
	case "deleteAll":
		changes, err := client.DeleteAll(ctx, params.Table)
		if err != nil {
			return nil, err
		}
		return map[string]any{"changes": changes}, nil
	case "select":
		options.Where = params.Where
		rows, err := client.Select(ctx, params.Table, options)
		if err != nil {
			return nil, err
		}
		return rows, nil
	case "selectOne":
		row, err := client.SelectOne(ctx, params.Table, params.Where)
		if err != nil {
			return nil, err
		}
		return row, nil
	case "count":
		count, err := client.Count(ctx, params.Table, params.Where)
		if err != nil {
			return nil, err
		}
		return map[string]any{"count": count}, nil
	case "hasTable":
		exists, err := client.HasTable(ctx, params.Table)
		if err != nil {
			return nil, err
		}
		return map[string]any{"exists": exists}, nil
	case "deleteWhereLessThan":
		changes, err := client.DeleteWhereLessThan(ctx, params.Table, params.Column, params.Value)
		if err != nil {
			return nil, err
		}
		return map[string]any{"changes": changes}, nil
	case "countWhereLessThan":
		count, err := client.CountWhereLessThan(ctx, params.Table, params.Column, params.Value)
		if err != nil {
			return nil, err
		}
		return map[string]any{"count": count}, nil
	case "countWhereGreaterThanOrEqual":
		count, err := client.CountWhereGreaterThanOrEqual(ctx, params.Table, params.Column, params.Value)
		if err != nil {
			return nil, err
		}
		return map[string]any{"count": count}, nil
	case "countWhereBetween":
		count, err := client.CountWhereBetween(ctx, params.Table, params.Column, params.Start, params.End)
		if err != nil {
			return nil, err
		}
		return map[string]any{"count": count}, nil
	default:
		return nil, fmt.Errorf("unsupported db operation %q", params.Op)
	}
}
