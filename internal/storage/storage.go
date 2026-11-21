// Package storage is the SQL persistence layer: SQLite or PostgreSQL, with table
// names prefixed bot_<botId>_<scope>_<owner>.
package storage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Engine string

const (
	EngineSQLite Engine = "sqlite"
	EnginePgsql  Engine = "pgsql"
)

type Scope string

const (
	ScopeCore    Scope = "core"
	ScopePlugin  Scope = "plugin"
	ScopeAdapter Scope = "adapter"
)

type RunResult struct {
	Changes         int64 `json:"changes"`
	LastInsertRowID int64 `json:"lastInsertRowid"`
}

type Driver interface {
	Type() Engine
	Placeholder(index int) string
	Execute(ctx context.Context, sqlText string, params ...any) error
	Query(ctx context.Context, sqlText string, params ...any) ([]map[string]any, error)
	Run(ctx context.Context, sqlText string, params ...any) (RunResult, error)
	// Transaction runs fn with a driver bound to that transaction.
	Transaction(ctx context.Context, fn func(tx Driver) error) error
	Ping(ctx context.Context) error
	Close() error
}

// ColumnDefinition accepts "text" and {"type":"text",...} in JSON.
type ColumnDefinition struct {
	Name          string
	Type          string
	PrimaryKey    bool
	AutoIncrement bool
	NotNull       bool
	Unique        bool
	Default       any
	HasDefault    bool
}

func (c *ColumnDefinition) UnmarshalJSON(raw []byte) error {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, `"`) {
		var simple string
		if err := json.Unmarshal(raw, &simple); err != nil {
			return err
		}
		c.Type = simple
		return nil
	}
	var wire struct {
		Type          string `json:"type"`
		PrimaryKey    bool   `json:"primaryKey"`
		AutoIncrement bool   `json:"autoIncrement"`
		NotNull       bool   `json:"notNull"`
		Unique        bool   `json:"unique"`
		Default       any    `json:"default"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	c.Type = wire.Type
	c.PrimaryKey = wire.PrimaryKey
	c.AutoIncrement = wire.AutoIncrement
	c.NotNull = wire.NotNull
	c.Unique = wire.Unique
	c.Default = wire.Default
	c.HasDefault = wire.Default != nil
	return nil
}

func (c ColumnDefinition) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": c.Type}
	if c.PrimaryKey {
		out["primaryKey"] = true
	}
	if c.AutoIncrement {
		out["autoIncrement"] = true
	}
	if c.NotNull {
		out["notNull"] = true
	}
	if c.Unique {
		out["unique"] = true
	}
	if c.HasDefault {
		out["default"] = c.Default
	}
	return json.Marshal(out)
}

type IndexDefinition struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
}

// TableSchema keeps column order, so generated DDL stays identical.
type TableSchema struct {
	Columns []ColumnDefinition
	Indexes []IndexDefinition
}

func (s *TableSchema) UnmarshalJSON(raw []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return errors.New("table schema must be an object")
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, _ := keyToken.(string)
		switch key {
		case "columns":
			columns, err := decodeOrderedColumns(decoder)
			if err != nil {
				return err
			}
			s.Columns = columns
		case "indexes":
			var indexes []IndexDefinition
			if err := decoder.Decode(&indexes); err != nil {
				return err
			}
			s.Indexes = indexes
		default:
			var skip any
			if err := decoder.Decode(&skip); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeOrderedColumns(decoder *json.Decoder) ([]ColumnDefinition, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil, errors.New("columns must be an object")
	}
	columns := []ColumnDefinition{}
	if delim == '[' {
		var list []ColumnDefinition
		if err := decoder.Decode(&list); err != nil {
			return nil, err
		}
		return list, nil
	}
	if delim != '{' {
		return nil, errors.New("columns must be an object")
	}
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, _ := nameToken.(string)
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		column := ColumnDefinition{Name: name}
		if err := column.UnmarshalJSON(raw); err != nil {
			return nil, err
		}
		column.Name = name
		columns = append(columns, column)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return columns, nil
}

func (s TableSchema) MarshalJSON() ([]byte, error) {
	var builder strings.Builder
	builder.WriteString(`{"columns":{`)
	for i, column := range s.Columns {
		if i > 0 {
			builder.WriteByte(',')
		}
		key, _ := json.Marshal(column.Name)
		value, err := json.Marshal(column)
		if err != nil {
			return nil, err
		}
		builder.Write(key)
		builder.WriteByte(':')
		builder.Write(value)
	}
	builder.WriteString(`}`)
	if len(s.Indexes) > 0 {
		encoded, err := json.Marshal(s.Indexes)
		if err != nil {
			return nil, err
		}
		builder.WriteString(`,"indexes":`)
		builder.Write(encoded)
	}
	builder.WriteString(`}`)
	return []byte(builder.String()), nil
}

func (s TableSchema) Column(name string) (ColumnDefinition, bool) {
	for _, column := range s.Columns {
		if column.Name == name {
			return column, true
		}
	}
	return ColumnDefinition{}, false
}

func validIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			continue
		case r >= '0' && r <= '9' && i > 0:
			continue
		default:
			return false
		}
	}
	return true
}

func ValidIdentifier(name string) bool { return validIdentifier(name) }

// QuoteIdentifier rejects names it cannot quote safely instead of guessing.
func QuoteIdentifier(name string) (string, error) {
	if !validIdentifier(name) {
		return "", fmt.Errorf("invalid SQL identifier: %q", name)
	}
	return `"` + name + `"`, nil
}

func sanitize(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// normalizeValue makes driver types JSON friendly.
func normalizeValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case []byte:
		if json.Valid(typed) && len(typed) > 0 && (typed[0] == '{' || typed[0] == '[') {
			return json.RawMessage(typed)
		}
		if isPrintable(typed) {
			return string(typed)
		}
		return "0x" + hex.EncodeToString(typed)
	case time.Time:
		return typed.Format(time.RFC3339Nano)
	case bool, int64, float64, string:
		return typed
	case int:
		return int64(typed)
	case uint64:
		return int64(typed)
	case json.Number:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func isPrintable(raw []byte) bool {
	for _, b := range raw {
		if b == 0 {
			return false
		}
	}
	return true
}

func toInt(value any) int {
	switch typed := value.(type) {
	case int64:
		return int(typed)
	case int:
		return typed
	case float64:
		return int(typed)
	case []byte:
		var parsed int
		fmt.Sscan(string(typed), &parsed)
		return parsed
	default:
		return 0
	}
}

func scanRows(rows *sql.Rows) ([]map[string]any, error) {
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			row[column] = normalizeValue(values[i])
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
