// Package config is the YAML configuration store. Keys are dot separated
// (web.port); a missing key returns the default and Set persists the whole file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

var ErrNotObject = errors.New("config: intermediate key is not a mapping")

type Store struct {
	mu     sync.RWMutex
	saveMu sync.Mutex
	path   string
	data   map[string]any
}

// Open: a missing file is not an error, Save creates it.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Reload() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			s.data = map[string]any{}
			s.mu.Unlock()
			return nil
		}
		return err
	}
	data := map[string]any{}
	if len(raw) > 0 {
		decoded, err := DecodeYAML(raw)
		if err != nil {
			return fmt.Errorf("config: parse %s: %w", s.path, err)
		}
		data = decoded
	}
	s.mu.Lock()
	s.data = data
	s.mu.Unlock()
	return nil
}

func DecodeYAML(raw []byte) (map[string]any, error) {
	var value any
	if err := yaml.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return NormalizeMap(value), nil
}

// NormalizeMap converts JSON decoded maps (map[string]interface{}) to map[string]any.
func NormalizeMap(value any) map[string]any {
	out := map[string]any{}
	switch typed := value.(type) {
	case nil:
		return out
	case map[string]any:
		for key, item := range typed {
			out[key] = NormalizeValue(item)
		}
	case map[any]any:
		for key, item := range typed {
			out[fmt.Sprint(key)] = NormalizeValue(item)
		}
	default:
		return out
	}
	return out
}

func NormalizeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return NormalizeMap(typed)
	case map[any]any:
		return NormalizeMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = NormalizeValue(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = NormalizeMap(item)
		}
		return out
	default:
		return value
	}
}

func (s *Store) Snapshot() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMap(s.data)
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = cloneValue(value)
	}
	return out
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return value
	}
}

func (s *Store) Get(path string, def any) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return lookup(s.data, strings.Split(path, "."), def)
}

func (s *Store) String(path, def string) string {
	if value, ok := s.Get(path, nil).(string); ok {
		return value
	}
	return def
}

func (s *Store) Int(path string, def int) int {
	switch value := s.Get(path, nil).(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case uint64:
		return int(value)
	default:
		return def
	}
}

func (s *Store) Int64(path string, def int64) int64 {
	switch value := s.Get(path, nil).(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	case uint64:
		return int64(value)
	default:
		return def
	}
}

func (s *Store) Bool(path string, def bool) bool {
	if value, ok := s.Get(path, nil).(bool); ok {
		return value
	}
	return def
}

func (s *Store) Map(path string) map[string]any {
	if value, ok := NormalizeValue(s.Get(path, nil)).(map[string]any); ok {
		return value
	}
	return map[string]any{}
}

func (s *Store) Slice(path string) []any {
	if value, ok := s.Get(path, nil).([]any); ok {
		return value
	}
	return nil
}

func lookup(root map[string]any, parts []string, def any) any {
	var current any = root
	for _, part := range parts {
		node, ok := current.(map[string]any)
		if !ok {
			return def
		}
		next, exists := node[part]
		if !exists {
			return def
		}
		current = next
	}
	return current
}

func (s *Store) Set(path string, value any) error {
	parts := strings.Split(path, ".")
	s.mu.Lock()
	if s.data == nil {
		s.data = map[string]any{}
	}
	current := s.data
	for _, part := range parts[:len(parts)-1] {
		next, exists := current[part]
		if !exists {
			child := map[string]any{}
			current[part] = child
			current = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			s.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrNotObject, part)
		}
		current = child
	}
	current[parts[len(parts)-1]] = NormalizeValue(value)
	s.mu.Unlock()
	return s.Save()
}

func (s *Store) SetSilent(path string, value any) {
	parts := strings.Split(path, ".")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string]any{}
	}
	current := s.data
	for _, part := range parts[:len(parts)-1] {
		child, ok := current[part].(map[string]any)
		if !ok {
			child = map[string]any{}
			current[part] = child
		}
		current = child
	}
	current[parts[len(parts)-1]] = NormalizeValue(value)
}

func (s *Store) Delete(path string) error {
	parts := strings.Split(path, ".")
	s.mu.Lock()
	current := s.data
	for _, part := range parts[:len(parts)-1] {
		child, ok := current[part].(map[string]any)
		if !ok {
			s.mu.Unlock()
			return nil
		}
		current = child
	}
	delete(current, parts[len(parts)-1])
	s.mu.Unlock()
	return s.Save()
}

// Save writes YAML with 2 space indentation. Concurrent saves are serialised
// through a unique temp file, so they cannot interleave.
func (s *Store) Save() error {
	s.mu.RLock()
	snapshot := cloneMap(s.data)
	s.mu.RUnlock()

	encoded, err := yaml.Marshal(snapshot)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if dir == "." || dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

const BotsDir = "config/bots"

const GlobalConfigPath = "config/config.yml"

func ListBotIDs() []string {
	entries, err := os.ReadDir(BotsDir)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") {
			ids = append(ids, strings.TrimSuffix(strings.TrimSuffix(name, ".yml"), ".yaml"))
		}
	}
	sort.Strings(ids)
	return ids
}

// BotConfigPath rejects invalid ids so API parameters cannot escape config/bots.
func BotConfigPath(botID string) (string, bool) {
	if !ValidBotID(botID) {
		return "", false
	}
	yml := filepath.Join(BotsDir, botID+".yml")
	if _, err := os.Stat(yml); err == nil {
		return yml, true
	}
	yamlPath := filepath.Join(BotsDir, botID+".yaml")
	if _, err := os.Stat(yamlPath); err == nil {
		return yamlPath, true
	}
	return "", false
}

func BotConfigExists(botID string) bool {
	_, ok := BotConfigPath(botID)
	return ok
}

func OpenBot(botID string) (*Store, error) {
	path, ok := BotConfigPath(botID)
	if !ok {
		return nil, fmt.Errorf("bot config %q not found", botID)
	}
	return Open(path)
}

type BotConfigTemplate struct {
	Name              string
	SelfID            int64
	Prefix            []string
	ErrorReplyEnabled bool
	Admin             []string
	Adapters          []any
	PluginConfig      map[string]any
}

// CreateBotConfigFile refuses to overwrite an existing file.
func CreateBotConfigFile(botID string, template BotConfigTemplate) error {
	if _, exists := BotConfigPath(botID); exists {
		return fmt.Errorf("Bot config file already exists: %s", botID)
	}
	if err := os.MkdirAll(BotsDir, 0o755); err != nil {
		return err
	}
	name := template.Name
	if name == "" {
		name = botID
	}
	prefix := template.Prefix
	if len(prefix) == 0 {
		prefix = []string{"/"}
	}
	adapters := template.Adapters
	if adapters == nil {
		adapters = []any{}
	}
	pluginConfig := template.PluginConfig
	if pluginConfig == nil {
		pluginConfig = map[string]any{}
	}
	data := map[string]any{
		"name":                name,
		"self_id":             template.SelfID,
		"prefix":              prefix,
		"error_reply_enabled": template.ErrorReplyEnabled,
		"admin":               template.Admin,
		"adapters":            adapters,
		"plugin_config":       pluginConfig,
	}
	encoded, err := yaml.Marshal(data)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(BotsDir, botID+".yml"), encoded, 0o644)
}

func DeleteBotConfigFile(botID string) error {
	path, ok := BotConfigPath(botID)
	if !ok {
		return fmt.Errorf("Bot config %q not found", botID)
	}
	return os.Remove(path)
}

func NormalizePrefix(value any) []string {
	switch typed := value.(type) {
	case nil:
		return []string{"/"}
	case string:
		return []string{typed}
	case []string:
		out := make([]string, len(typed))
		copy(out, typed)
		return out
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, fmt.Sprint(item))
		}
		return out
	default:
		return []string{fmt.Sprint(typed)}
	}
}

func ValidBotID(botID string) bool {
	if botID == "" {
		return false
	}
	for _, r := range botID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
