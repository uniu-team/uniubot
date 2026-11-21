package api

import (
	"fmt"
	"strings"

	"github.com/uniu-network/uniubot/internal/core/config"
)

type GlobalConfigField struct {
	Key             string   `json:"key"`
	Type            string   `json:"type"`
	Label           string   `json:"label"`
	Description     string   `json:"description"`
	Section         string   `json:"section"`
	Enum            []string `json:"enum,omitempty"`
	Sensitive       bool     `json:"sensitive,omitempty"`
	HotReloadable   bool     `json:"hotReloadable,omitempty"`
	RestartRequired bool     `json:"restartRequired,omitempty"`
}

type FrontendGlobalConfigField struct {
	Key             string   `json:"key"`
	Label           string   `json:"label"`
	Type            string   `json:"type"`
	Value           any      `json:"value"`
	Description     string   `json:"description"`
	HotReloadable   bool     `json:"hotReloadable"`
	RestartRequired bool     `json:"restartRequired"`
	Options         []string `json:"options,omitempty"`
	Sensitive       bool     `json:"sensitive,omitempty"`
}

type GlobalConfigSection struct {
	Name   string                      `json:"name"`
	Fields []FrontendGlobalConfigField `json:"fields"`
}

var (
	logLevels       = []string{"TRACE", "DEBUG", "INFO", "WARN", "ERROR"}
	databaseEngines = []string{"sqlite", "pgsql"}

	hotReloadableKeys = map[string]bool{
		"log.level":    true,
		"web.username": true,
		"web.password": true,
	}
)

var GlobalConfigSchema = []GlobalConfigField{
	{Key: "log.level", Label: "日志级别", Type: "string", Description: "控制系统输出日志的详细程度。", Section: "日志", Enum: logLevels, HotReloadable: true},
	{Key: "web.host", Label: "监听地址", Type: "string", Description: "Web 服务绑定的主机地址。", Section: "Web 服务", RestartRequired: true},
	{Key: "web.port", Label: "监听端口", Type: "number", Description: "Web 服务监听的端口号。", Section: "Web 服务", RestartRequired: true},
	{Key: "web.username", Label: "登录用户名", Type: "string", Description: "Dashboard 登录使用的用户名。", Section: "Web 服务", HotReloadable: true},
	{Key: "web.password", Label: "登录密码", Type: "string", Description: "Dashboard 登录使用的密码，留空时不会覆盖现有密码。", Section: "Web 服务", Sensitive: true, HotReloadable: true},
	{Key: "dashboard.debug", Label: "调试模式", Type: "boolean", Description: "启用 Dashboard 调试相关输出。", Section: "Dashboard", RestartRequired: true},
	{Key: "database.engine", Label: "数据库引擎", Type: "string", Description: "选择后端使用的数据库类型。", Section: "数据库", Enum: databaseEngines, RestartRequired: true},
	{Key: "database.sqlite.file", Label: "SQLite 文件路径", Type: "string", Description: "SQLite 数据库文件保存位置。", Section: "数据库", RestartRequired: true},
	{Key: "database.pgsql.connectionString", Label: "PostgreSQL 连接字符串", Type: "string", Description: "连接 PostgreSQL 数据库使用的连接字符串。", Section: "数据库", RestartRequired: true},
	{Key: "database.pgsql.pool.min", Label: "连接池最小连接数", Type: "number", Description: "PostgreSQL 连接池保持的最小连接数量。", Section: "数据库", RestartRequired: true},
	{Key: "database.pgsql.pool.max", Label: "连接池最大连接数", Type: "number", Description: "PostgreSQL 连接池允许的最大连接数量。", Section: "数据库", RestartRequired: true},
	{Key: "database.message_retention_days", Label: "消息保留天数", Type: "number", Description: "消息记录在自动清理前保留的天数。", Section: "消息清理", RestartRequired: true},
	{Key: "database.cleanup_time", Label: "清理时间", Type: "string", Description: "每日执行消息清理任务的时间。", Section: "消息清理", RestartRequired: true},
	{Key: "database.timezone", Label: "清理时区", Type: "string", Description: "消息统计与清理调度使用的时区。", Section: "消息清理", RestartRequired: true},
}

var schemaByKey = func() map[string]GlobalConfigField {
	out := make(map[string]GlobalConfigField, len(GlobalConfigSchema))
	for _, field := range GlobalConfigSchema {
		out[field.Key] = field
	}
	return out
}()

func GlobalConfigFieldByKey(key string) (GlobalConfigField, bool) {
	field, ok := schemaByKey[key]
	return field, ok
}

func GlobalConfigFields(store *config.Store) []map[string]any {
	out := make([]map[string]any, 0, len(GlobalConfigSchema))
	for _, field := range GlobalConfigSchema {
		entry := map[string]any{
			"key":         field.Key,
			"type":        field.Type,
			"label":       field.Label,
			"description": field.Description,
			"section":     field.Section,
			"value":       fieldValue(store, field),
		}
		if len(field.Enum) > 0 {
			entry["enum"] = field.Enum
		}
		if field.Sensitive {
			entry["sensitive"] = true
		}
		if field.HotReloadable {
			entry["hotReloadable"] = true
		}
		if field.RestartRequired {
			entry["restartRequired"] = true
		}
		out = append(out, entry)
	}
	return out
}

func fieldValue(store *config.Store, field GlobalConfigField) any {
	if field.Sensitive {
		return nil
	}
	return store.Get(field.Key, nil)
}

func GlobalConfigSections(store *config.Store) []GlobalConfigSection {
	sections := []GlobalConfigSection{
		{Name: "日志", Fields: []FrontendGlobalConfigField{}},
		{Name: "Web 服务", Fields: []FrontendGlobalConfigField{}},
		{Name: "Dashboard", Fields: []FrontendGlobalConfigField{}},
		{Name: "数据库", Fields: []FrontendGlobalConfigField{}},
		{Name: "消息清理", Fields: []FrontendGlobalConfigField{}},
	}
	index := map[string]int{}
	for i, section := range sections {
		index[section.Name] = i
	}
	for _, field := range GlobalConfigSchema {
		position, ok := index[field.Section]
		if !ok {
			continue
		}
		sections[position].Fields = append(sections[position].Fields, toFrontendField(store, field))
	}
	return sections
}

func toFrontendField(store *config.Store, field GlobalConfigField) FrontendGlobalConfigField {
	hotReloadable := hotReloadableKeys[field.Key]
	out := FrontendGlobalConfigField{
		Key:             field.Key,
		Label:           field.Label,
		Type:            controlType(field),
		Value:           fieldValue(store, field),
		Description:     field.Description,
		HotReloadable:   hotReloadable,
		RestartRequired: !hotReloadable,
	}
	if len(field.Enum) > 0 {
		out.Options = field.Enum
	}
	if field.Sensitive {
		out.Sensitive = true
	}
	return out
}

func controlType(field GlobalConfigField) string {
	if len(field.Enum) > 0 {
		return "enum"
	}
	if field.Key == "web.password" {
		return "password"
	}
	return field.Type
}

type GlobalConfigValidation struct {
	Valid   bool
	Values  map[string]any
	Skipped []string
	Errors  []string
}

func ValidateGlobalConfigUpdate(input any) GlobalConfigValidation {
	result := GlobalConfigValidation{Values: map[string]any{}, Skipped: []string{}, Errors: []string{}}
	body, ok := input.(map[string]any)
	if !ok || body == nil {
		result.Errors = append(result.Errors, "request body must be an object")
		return result
	}
	source := body
	if nested, ok := body["config"].(map[string]any); ok {
		source = nested
	}
	for key, rawValue := range source {
		field, exists := schemaByKey[key]
		if !exists {
			result.Errors = append(result.Errors, key+" is not an allowed config key")
			continue
		}
		if field.Sensitive {
			if text, ok := rawValue.(string); ok && strings.TrimSpace(text) == "" {
				result.Skipped = append(result.Skipped, key)
				continue
			}
		}
		value, err := parseConfigValue(field, rawValue)
		if err != nil {
			result.Errors = append(result.Errors, err.Error())
			continue
		}
		result.Values[key] = value
	}
	result.Valid = len(result.Errors) == 0
	return result
}

func parseConfigValue(field GlobalConfigField, value any) (any, error) {
	switch field.Type {
	case "string":
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be a string", field.Key)
		}
		normalized := text
		if field.Key == "log.level" {
			normalized = strings.ToUpper(text)
		}
		if len(field.Enum) > 0 {
			allowed := false
			for _, option := range field.Enum {
				if option == normalized {
					allowed = true
					break
				}
			}
			if !allowed {
				return nil, fmt.Errorf("%s must be one of: %s", field.Key, strings.Join(field.Enum, ", "))
			}
		}
		return normalized, nil
	case "number":
		switch typed := value.(type) {
		case float64:
			return typed, nil
		case int:
			return float64(typed), nil
		case int64:
			return float64(typed), nil
		default:
			return nil, fmt.Errorf("%s must be a finite number", field.Key)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return nil, fmt.Errorf("%s must be a boolean", field.Key)
		}
		return value, nil
	default:
		return value, nil
	}
}
