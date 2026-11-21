package api

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/uniu-network/uniubot/internal/app"
	"github.com/uniu-network/uniubot/internal/core/config"
	"github.com/uniu-network/uniubot/internal/core/logging"
	"github.com/uniu-network/uniubot/internal/services"
)

// resolveBotID rejects ids that could escape config/bots (separators, "..").
func resolveBotID(r *http.Request, body any) string {
	value := ""
	if fromQuery := r.URL.Query().Get("botId"); fromQuery != "" {
		value = fromQuery
	} else if payload := bodyMap(body); payload != nil {
		if fromBody, ok := payload["botId"].(string); ok {
			value = fromBody
		}
	}
	if value == "" {
		return ""
	}
	if !config.ValidBotID(value) {
		return ""
	}
	return value
}

func (s *Server) requireInstance(w http.ResponseWriter, r *http.Request, body any) (*app.BotInstance, bool) {
	botID := resolveBotID(r, body)
	if botID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":    400,
			"message": "botId is required (query param or body)",
		})
		return nil, false
	}
	instance, ok := s.bots.GetInstance(botID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"code":    404,
			"message": fmt.Sprintf("Bot %q not found", botID),
		})
		return nil, false
	}
	return instance, true
}

func (s *Server) handleBotsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.bots.ListAllBots())
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"code":     0,
		"sections": GlobalConfigSections(s.global),
		"fields":   GlobalConfigFields(s.global),
	})
}

func (s *Server) handleConfigSet(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	validation := ValidateGlobalConfigUpdate(body)
	if !validation.Valid {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":    400,
			"message": "invalid config update",
			"errors":  validation.Errors,
		})
		return
	}
	applied := []string{}
	requiresRestart := []string{}
	for _, field := range GlobalConfigSchema {
		value, ok := validation.Values[field.Key]
		if !ok {
			continue
		}
		if err := s.global.Set(field.Key, value); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"code":    500,
				"message": fmt.Sprintf("failed to persist %s: %v", field.Key, err),
			})
			return
		}
		if hotReloadableKeys[field.Key] {
			applied = append(applied, field.Key)
		} else {
			requiresRestart = append(requiresRestart, field.Key)
		}
		if field.Key == "log.level" {
			if text, ok := value.(string); ok {
				if err := s.logger.SetLevel(logging.ParseLevel(text)); err != nil {
					s.logger.Warn("invalid log level %q: %v", text, err)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":            0,
		"message":         "config saved",
		"applied":         applied,
		"requiresRestart": requiresRestart,
		"skipped":         validation.Skipped,
	})
}

func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	go func() {
		time.Sleep(250 * time.Millisecond)
		s.logger.Info("restart requested through the API, exiting")
		os.Exit(0)
	}()
	writeJSON(w, http.StatusOK, map[string]any{
		"code":    0,
		"message": "restart scheduled; external process manager must restart the process",
	})
}

func (s *Server) handleBotsCreate(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	payload := bodyMap(body)
	botID, _ := payload["botId"].(string)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	var errorReply *bool
	if raw, exists := payload["error_reply_enabled"]; exists {
		value, ok := raw.(bool)
		if !ok {
			writeError(w, http.StatusBadRequest, "error_reply_enabled must be a boolean")
			return
		}
		errorReply = &value
	}
	var selfID *int64
	if raw, exists := payload["self_id"]; exists {
		if number, ok := raw.(float64); ok {
			value := int64(number)
			selfID = &value
		} else if number, ok := raw.(int64); ok {
			selfID = &number
		}
	}
	name, _ := payload["name"].(string)
	adapters, _ := payload["adapters"].([]any)
	pluginConfig, _ := payload["plugin_config"].(map[string]any)
	var admin []string
	if list, ok := payload["admin"].([]any); ok {
		for _, item := range list {
			admin = append(admin, fmt.Sprint(item))
		}
	}
	instance, err := s.bots.CreateInstance(r.Context(), app.CreateOptions{
		BotID:             botID,
		Name:              name,
		SelfID:            selfID,
		Prefix:            config.NormalizePrefix(payload["prefix"]),
		ErrorReplyEnabled: errorReply,
		Admin:             admin,
		Adapters:          adapters,
		PluginConfig:      pluginConfig,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":    0,
		"message": fmt.Sprintf("bot %s created", botID),
		"bot":     instance.Info(),
	})
}

func (s *Server) handleBotsDelete(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	botID, _ := bodyMap(body)["botId"].(string)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	if err := s.bots.DeleteInstance(r.Context(), botID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": fmt.Sprintf("bot %s deleted", botID)})
}

func (s *Server) handleBotsStart(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	botID, _ := bodyMap(body)["botId"].(string)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	if s.bots.HasInstance(botID) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("bot %s is already running", botID))
		return
	}
	if err := s.bots.StartInstance(r.Context(), botID); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to start bot %s: %v", botID, err))
		return
	}
	response := map[string]any{"code": 0, "message": fmt.Sprintf("bot %s started", botID)}
	if instance, ok := s.bots.GetInstance(botID); ok {
		response["bot"] = instance.Info()
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleBotsStop(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	botID, _ := bodyMap(body)["botId"].(string)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	if len(s.bots.ListAllBots()) <= 1 {
		writeError(w, http.StatusBadRequest, "cannot stop the only bot instance")
		return
	}
	if !s.bots.HasInstance(botID) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("bot %s not found or not running", botID))
		return
	}
	if err := s.bots.StopInstance(r.Context(), botID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": fmt.Sprintf("bot %s stopped", botID)})
}

func (s *Server) handleBotsConfig(w http.ResponseWriter, r *http.Request) {
	botID := resolveBotID(r, nil)
	if botID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "message": "botId is required (query param)"})
		return
	}
	if !config.BotConfigExists(botID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": fmt.Sprintf("Bot %q config not found", botID)})
		return
	}
	store, err := config.OpenBot(botID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{}
	for _, key := range []string{"name", "self_id", "prefix", "error_reply_enabled", "admin", "adapters", "plugin_config"} {
		value := store.Get(key, nil)
		if value != nil {
			out[key] = value
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleBotsSetConfig(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	payload := bodyMap(body)
	botID := resolveBotID(r, body)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	if !config.BotConfigExists(botID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": fmt.Sprintf("Bot %q config not found", botID)})
		return
	}
	if raw, exists := payload["error_reply_enabled"]; exists {
		if _, ok := raw.(bool); !ok {
			writeError(w, http.StatusBadRequest, "error_reply_enabled must be a boolean")
			return
		}
	}
	store, err := config.OpenBot(botID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updated := map[string]any{}
	for _, key := range []string{"name", "self_id", "prefix", "error_reply_enabled", "admin"} {
		value, exists := payload[key]
		if !exists {
			continue
		}
		if key == "prefix" {
			value = config.NormalizePrefix(value)
		}
		if err := store.Set(key, value); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"code":    500,
				"message": fmt.Sprintf("failed to persist %s: %v", key, err),
			})
			return
		}
		updated[key] = value
	}
	if s.bots.HasInstance(botID) {
		if err := s.bots.ReloadInstance(r.Context(), botID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":    0,
		"message": "config updated and bot reloaded",
		"updated": updated,
	})
}

func (s *Server) handleBotsReload(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	botID, _ := bodyMap(body)["botId"].(string)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	if err := s.bots.ReloadInstance(r.Context(), botID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": fmt.Sprintf("bot %s reloaded", botID)})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	instance, ok := s.requireInstance(w, r, nil)
	if !ok {
		return
	}
	timezone := s.global.String("database.timezone", "")
	if timezone == "" {
		timezone = services.SystemTimezone()
	}
	ctx := r.Context()
	today, err := instance.Counter.GetTodayStats(ctx, timezone)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	daily, err := instance.Counter.GetDailyStats(ctx, 30, timezone)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dailyPayload := make([]map[string]any, 0, len(daily))
	for _, entry := range daily {
		dailyPayload = append(dailyPayload, map[string]any{
			"date":     entry.StatDate,
			"messages": entry.Messages,
			"groups":   entry.Groups,
			"users":    entry.Users,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"version":  s.version,
		"botId":    instance.BotID,
		"timezone": timezone,
		"plugins":  instance.Plugins.Count(),
		"today": map[string]any{
			"messages": today.Messages,
			"users":    today.Users,
			"groups":   today.Groups,
		},
		"daily": dailyPayload,
	})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	instance, ok := s.requireInstance(w, r, nil)
	if !ok {
		return
	}
	limit := clamp(queryInt(r, "limit", 20), 1, 200)
	offset := queryInt(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	messageType := r.URL.Query().Get("type")
	if messageType == "null" {
		messageType = ""
	}
	ctx := r.Context()
	data, err := instance.Counter.GetMessages(ctx, limit, offset, messageType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, err := instance.Counter.GetMessageCount(ctx, messageType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":  total,
		"limit":  limit,
		"offset": offset,
		"data":   data,
	})
}

func (s *Server) handleCommandsList(w http.ResponseWriter, r *http.Request) {
	instance, ok := s.requireInstance(w, r, nil)
	if !ok {
		return
	}
	commands := instance.Bot.Commands()
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
	writeJSON(w, http.StatusOK, out)
}
