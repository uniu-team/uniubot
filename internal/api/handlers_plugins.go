package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/uniu-network/uniubot/internal/adapters"
)

func (s *Server) handlePluginsList(w http.ResponseWriter, r *http.Request) {
	instance, ok := s.requireInstance(w, r, nil)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, instance.Plugins.List())
}

func (s *Server) handlePluginsGetConfig(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	instance, ok := s.requireInstance(w, r, body)
	if !ok {
		return
	}
	name, _ := bodyMap(body)["name"].(string)
	if name == "" {
		writeError(w, http.StatusBadRequest, "plugin name is required")
		return
	}
	config, schema := instance.Plugins.GetConfig(name)
	writeJSON(w, http.StatusOK, map[string]any{
		"config":       config,
		"configSchema": schema,
	})
}

func (s *Server) handlePluginsSetConfig(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	instance, ok := s.requireInstance(w, r, body)
	if !ok {
		return
	}
	payload := bodyMap(body)
	name, _ := payload["name"].(string)
	if name == "" {
		writeError(w, http.StatusBadRequest, "plugin name is required")
		return
	}
	rawConfig, _ := payload["config"].(map[string]any)
	if rawConfig == nil {
		rawConfig = map[string]any{}
	}
	entry, exists := instance.Plugins.Get(name)
	if !exists {
		writeError(w, http.StatusNotFound, fmt.Sprintf("plugin %s not found", name))
		return
	}

	if len(entry.ConfigSchema) == 0 {
		if err := instance.Config.Set("plugin_config."+name, rawConfig); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"code": 0})
		return
	}

	next, err := instance.Plugins.SetConfig(r.Context(), name, rawConfig)
	if err != nil {
		if strings.Contains(err.Error(), "required") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"code":   400,
				"errors": []string{err.Error()},
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "config": next})
}

func (s *Server) handlePluginsReload(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	botID := resolveBotID(r, body)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	instance, ok := s.bots.GetInstance(botID)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Bot %q not found", botID))
		return
	}
	name, _ := bodyMap(body)["name"].(string)
	if name != "" {
		if err := instance.Plugins.Reload(r.Context(), name); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": fmt.Sprintf("plugin %s reloaded", name)})
		return
	}
	instance.Plugins.ReloadAll(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": "plugins reloaded"})
}

func (s *Server) handlePluginsUnload(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	botID := resolveBotID(r, body)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	instance, ok := s.bots.GetInstance(botID)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Bot %q not found", botID))
		return
	}
	name, _ := bodyMap(body)["name"].(string)
	if name == "" {
		writeError(w, http.StatusBadRequest, "plugin name is required")
		return
	}
	if !instance.Plugins.IsLoaded(name) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("plugin %s is not loaded", name))
		return
	}
	if err := instance.Plugins.Unload(r.Context(), name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": fmt.Sprintf("plugin %s unloaded", name)})
}

func (s *Server) handlePluginsLoad(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	botID := resolveBotID(r, body)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	instance, ok := s.bots.GetInstance(botID)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Bot %q not found", botID))
		return
	}
	name, _ := bodyMap(body)["name"].(string)
	if name == "" {
		writeError(w, http.StatusBadRequest, "plugin name is required")
		return
	}
	if instance.Plugins.IsLoaded(name) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("plugin %s is already loaded", name))
		return
	}
	if err := instance.Plugins.Load(r.Context(), name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": fmt.Sprintf("plugin %s loaded", name)})
}

func (s *Server) handlePluginsToggle(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	botID := resolveBotID(r, body)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	instance, ok := s.bots.GetInstance(botID)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Bot %q not found", botID))
		return
	}
	payload := bodyMap(body)
	name, _ := payload["name"].(string)
	enabled, hasEnabled := payload["enabled"].(bool)
	if name == "" {
		writeError(w, http.StatusBadRequest, "plugin name is required")
		return
	}
	if !hasEnabled {
		writeError(w, http.StatusBadRequest, "enabled (boolean) is required")
		return
	}
	if err := instance.Plugins.Toggle(r.Context(), name, enabled); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": fmt.Sprintf("plugin %s %s", name, state)})
}

func (s *Server) handleAdaptersList(w http.ResponseWriter, r *http.Request) {
	instance, ok := s.requireInstance(w, r, nil)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, instance.Adapters.List())
}

func (s *Server) handleAdaptersAvailable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, adapters.Catalog())
}

func (s *Server) handleAdaptersSetConfig(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	instance, ok := s.requireInstance(w, r, body)
	if !ok {
		return
	}
	payload := bodyMap(body)
	adapterType, _ := payload["type"].(string)
	if adapterType == "" {
		writeError(w, http.StatusBadRequest, "adapter type is required")
		return
	}
	config, _ := payload["config"].(map[string]any)
	if config == nil {
		config = map[string]any{}
	}
	list := adapterConfigList(instance.Config.Get("adapters", nil))
	found := false
	for i, entry := range list {
		if fmt.Sprint(entry["type"]) == adapterType {
			list[i]["config"] = config
			found = true
			break
		}
	}
	if !found {
		list = append(list, map[string]any{"type": adapterType, "config": config})
	}
	if err := instance.Config.Set("adapters", toAnySlice(list)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "config": config})
}

func (s *Server) handleAdaptersReload(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	botID := resolveBotID(r, body)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	instance, ok := s.bots.GetInstance(botID)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Bot %q not found", botID))
		return
	}
	adapterType, _ := bodyMap(body)["type"].(string)
	if adapterType != "" {
		if err := instance.Adapters.ReloadAdapter(r.Context(), adapterType); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": fmt.Sprintf("adapter %s reloaded", adapterType)})
		return
	}
	if err := instance.Adapters.ReloadAll(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": "adapters reloaded"})
}

func (s *Server) handleAdaptersAdd(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	botID := resolveBotID(r, body)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	instance, ok := s.bots.GetInstance(botID)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Bot %q not found", botID))
		return
	}
	payload := bodyMap(body)
	adapterType, _ := payload["type"].(string)
	if adapterType == "" {
		writeError(w, http.StatusBadRequest, "adapter type is required")
		return
	}
	list := adapterConfigList(instance.Config.Get("adapters", nil))
	for _, entry := range list {
		if fmt.Sprint(entry["type"]) == adapterType {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("adapter %s already exists in config", adapterType))
			return
		}
	}
	config, _ := payload["config"].(map[string]any)
	if config == nil {
		config = map[string]any{}
	}
	list = append(list, map[string]any{"type": adapterType, "config": config})
	if err := instance.Config.Set("adapters", toAnySlice(list)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := instance.Adapters.LoadAdapter(r.Context(), adapterType, config); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to load adapter %s: %v", adapterType, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": fmt.Sprintf("adapter %s added and loaded", adapterType)})
}

func (s *Server) handleAdaptersRemove(w http.ResponseWriter, r *http.Request) {
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	botID := resolveBotID(r, body)
	if botID == "" {
		writeError(w, http.StatusBadRequest, "botId is required")
		return
	}
	instance, ok := s.bots.GetInstance(botID)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Bot %q not found", botID))
		return
	}
	adapterType, _ := bodyMap(body)["type"].(string)
	if adapterType == "" {
		writeError(w, http.StatusBadRequest, "adapter type is required")
		return
	}
	if err := instance.Adapters.UnloadAdapter(r.Context(), adapterType); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	filtered := []map[string]any{}
	for _, entry := range adapterConfigList(instance.Config.Get("adapters", nil)) {
		if fmt.Sprint(entry["type"]) != adapterType {
			filtered = append(filtered, entry)
		}
	}
	if err := instance.Config.Set("adapters", toAnySlice(filtered)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": fmt.Sprintf("adapter %s removed", adapterType)})
}

func adapterConfigList(raw any) []map[string]any {
	list, ok := raw.([]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		copyEntry := map[string]any{}
		for key, value := range entry {
			copyEntry[key] = value
		}
		out = append(out, copyEntry)
	}
	return out
}

func toAnySlice(list []map[string]any) []any {
	out := make([]any, 0, len(list))
	for _, item := range list {
		out = append(out, item)
	}
	return out
}
