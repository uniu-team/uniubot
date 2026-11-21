package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/uniu-network/uniubot/internal/storage"
)

var (
	forbiddenSQL = regexp.MustCompile(`(?i)\b(DROP\s+DATABASE|ALTER\s+DATABASE|CREATE\s+DATABASE)\b`)
	// columnTypePattern only accepts plain SQL type names such as DECIMAL(10,2).
	columnTypePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_ (),]*$`)
)

func (s *Server) dbUnavailable(w http.ResponseWriter) bool {
	if s.storage == nil || !s.storage.IsInitialized() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"code": 503, "message": "database not initialized"})
		return true
	}
	return false
}

func (s *Server) handleDBTables(w http.ResponseWriter, r *http.Request) {
	if s.dbUnavailable(w) {
		return
	}
	tables, err := s.storage.ListTables(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	infos := make([]map[string]any, 0, len(tables))
	for _, table := range tables {
		count, err := s.storage.RowCount(r.Context(), table)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		infos = append(infos, map[string]any{"name": table, "rowCount": count})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"engine": s.storage.EngineType(),
		"tables": infos,
	})
}

func (s *Server) handleDBTable(w http.ResponseWriter, r *http.Request) {
	if s.dbUnavailable(w) {
		return
	}
	tableName := r.URL.Query().Get("name")
	if tableName == "" {
		writeError(w, http.StatusBadRequest, "table name is required")
		return
	}
	columns, err := s.storage.DescribeTable(r.Context(), tableName)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	count, err := s.storage.RowCount(r.Context(), tableName)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":     tableName,
		"columns":  columns,
		"rowCount": count,
	})
}

func (s *Server) handleDBTableData(w http.ResponseWriter, r *http.Request) {
	if s.dbUnavailable(w) {
		return
	}
	tableName := r.URL.Query().Get("name")
	if tableName == "" {
		writeError(w, http.StatusBadRequest, "table name is required")
		return
	}
	quotedTable, err := quoteIdentifier(tableName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clamp(queryInt(r, "limit", 50), 1, 500)
	offset := queryInt(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	sortOrder := "ASC"
	if r.URL.Query().Get("sortOrder") == "desc" {
		sortOrder = "DESC"
	}
	sqlText := fmt.Sprintf("SELECT * FROM %s", quotedTable)
	if sortBy := r.URL.Query().Get("sortBy"); sortBy != "" {
		quotedSort, err := quoteIdentifier(sortBy)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		sqlText += fmt.Sprintf(" ORDER BY %s %s", quotedSort, sortOrder)
	}
	sqlText += fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)
	rows, err := s.storage.QueryRaw(r.Context(), sqlText)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, err := s.storage.RowCount(r.Context(), tableName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":   rows,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

func (s *Server) handleDBQuery(w http.ResponseWriter, r *http.Request) {
	if s.dbUnavailable(w) {
		return
	}
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	sqlText, _ := bodyMap(body)["sql"].(string)
	if sqlText == "" {
		writeError(w, http.StatusBadRequest, "sql is required")
		return
	}
	if forbiddenSQL.MatchString(sqlText) {
		writeError(w, http.StatusForbidden, "this SQL statement is not allowed")
		return
	}
	trimmed := strings.ToUpper(strings.TrimSpace(sqlText))
	if strings.HasPrefix(trimmed, "SELECT") || strings.HasPrefix(trimmed, "PRAGMA") ||
		strings.HasPrefix(trimmed, "EXPLAIN") || strings.HasPrefix(trimmed, "WITH") {
		rows, err := s.storage.QueryRaw(r.Context(), sqlText)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"type":     "query",
			"rows":     rows,
			"rowCount": len(rows),
		})
		return
	}
	result, err := s.storage.ExecuteRaw(r.Context(), sqlText)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type":            "execute",
		"changes":         result.Changes,
		"lastInsertRowid": result.LastInsertRowID,
	})
}

func (s *Server) handleDBInsertRow(w http.ResponseWriter, r *http.Request) {
	if s.dbUnavailable(w) {
		return
	}
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	payload := bodyMap(body)
	tableName, _ := payload["table"].(string)
	if tableName == "" {
		writeError(w, http.StatusBadRequest, "table name is required")
		return
	}
	data, ok := payload["data"].(map[string]any)
	if !ok || len(data) == 0 {
		writeError(w, http.StatusBadRequest, "data object is required")
		return
	}
	quotedTable, err := quoteIdentifier(tableName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	columns := make([]string, 0, len(data))
	values := make([]any, 0, len(data))
	placeholders := make([]string, 0, len(data))
	index := 1
	for key, value := range data {
		quotedColumn, err := quoteIdentifier(key)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		columns = append(columns, quotedColumn)
		values = append(values, value)
		placeholders = append(placeholders, s.storage.Placeholder(index))
		index++
	}
	sqlText := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", quotedTable, strings.Join(columns, ", "), strings.Join(placeholders, ", "))
	result, err := s.storage.ExecuteRaw(r.Context(), sqlText, values...)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":            0,
		"changes":         result.Changes,
		"lastInsertRowid": result.LastInsertRowID,
	})
}

func (s *Server) handleDBUpdateRow(w http.ResponseWriter, r *http.Request) {
	if s.dbUnavailable(w) {
		return
	}
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	payload := bodyMap(body)
	tableName, _ := payload["table"].(string)
	if tableName == "" {
		writeError(w, http.StatusBadRequest, "table name is required")
		return
	}
	data, dataOK := payload["data"].(map[string]any)
	if !dataOK || len(data) == 0 {
		writeError(w, http.StatusBadRequest, "data object is required")
		return
	}
	where, whereOK := payload["where"].(map[string]any)
	if !whereOK || len(where) == 0 {
		writeError(w, http.StatusBadRequest, "where object is required")
		return
	}
	quotedTable, err := quoteIdentifier(tableName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	values := []any{}
	setClauses := []string{}
	index := 1
	for key, value := range data {
		quotedColumn, err := quoteIdentifier(key)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		setClauses = append(setClauses, fmt.Sprintf("%s = %s", quotedColumn, s.storage.Placeholder(index)))
		values = append(values, value)
		index++
	}
	whereClauses := []string{}
	for key, value := range where {
		quotedColumn, err := quoteIdentifier(key)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		whereClauses = append(whereClauses, fmt.Sprintf("%s = %s", quotedColumn, s.storage.Placeholder(index)))
		values = append(values, value)
		index++
	}
	sqlText := fmt.Sprintf("UPDATE %s SET %s WHERE %s", quotedTable, strings.Join(setClauses, ", "), strings.Join(whereClauses, " AND "))
	result, err := s.storage.ExecuteRaw(r.Context(), sqlText, values...)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "changes": result.Changes})
}

func (s *Server) handleDBDeleteRow(w http.ResponseWriter, r *http.Request) {
	if s.dbUnavailable(w) {
		return
	}
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	payload := bodyMap(body)
	tableName, _ := payload["table"].(string)
	if tableName == "" {
		writeError(w, http.StatusBadRequest, "table name is required")
		return
	}
	where, ok := payload["where"].(map[string]any)
	if !ok || len(where) == 0 {
		writeError(w, http.StatusBadRequest, "where object with at least one condition is required")
		return
	}
	quotedTable, err := quoteIdentifier(tableName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	values := []any{}
	clauses := []string{}
	index := 1
	for key, value := range where {
		quotedColumn, err := quoteIdentifier(key)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		clauses = append(clauses, fmt.Sprintf("%s = %s", quotedColumn, s.storage.Placeholder(index)))
		values = append(values, value)
		index++
	}
	sqlText := fmt.Sprintf("DELETE FROM %s WHERE %s", quotedTable, strings.Join(clauses, " AND "))
	result, err := s.storage.ExecuteRaw(r.Context(), sqlText, values...)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "changes": result.Changes})
}

func (s *Server) handleDBCreateTable(w http.ResponseWriter, r *http.Request) {
	if s.dbUnavailable(w) {
		return
	}
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	payload := bodyMap(body)
	tableName, _ := payload["name"].(string)
	if tableName == "" {
		writeError(w, http.StatusBadRequest, "table name is required")
		return
	}
	columns, ok := payload["columns"].([]any)
	if !ok {
		writeError(w, http.StatusBadRequest, "columns array is required")
		return
	}
	definitions := make([]string, 0, len(columns))
	for _, raw := range columns {
		column, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, ok := column["name"].(string)
		if !ok {
			writeError(w, http.StatusBadRequest, "column name must be a string")
			return
		}
		quotedName, err := quoteIdentifier(name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		columnType := "TEXT"
		if value, ok := column["type"].(string); ok && value != "" {
			if !columnTypePattern.MatchString(value) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid column type %q", value))
				return
			}
			columnType = value
		}
		definition := fmt.Sprintf("%s %s", quotedName, columnType)
		if truthy(column["primaryKey"]) {
			definition += " PRIMARY KEY"
		}
		if truthy(column["autoIncrement"]) && s.storage.EngineType() == "sqlite" {
			definition += " AUTOINCREMENT"
		}
		if truthy(column["notNull"]) {
			definition += " NOT NULL"
		}
		if truthy(column["unique"]) {
			definition += " UNIQUE"
		}
		if value, exists := column["defaultValue"]; exists && value != nil {
			definition += " DEFAULT " + sqlDefaultLiteral(value)
		}
		definitions = append(definitions, definition)
	}
	quotedTable, err := quoteIdentifier(tableName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sqlText := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n  %s\n)", quotedTable, strings.Join(definitions, ",\n  "))
	if _, err := s.storage.ExecuteRaw(r.Context(), sqlText); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": fmt.Sprintf("table %q created", tableName)})
}

func (s *Server) handleDBDropTable(w http.ResponseWriter, r *http.Request) {
	if s.dbUnavailable(w) {
		return
	}
	_, body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tableName, _ := bodyMap(body)["name"].(string)
	if tableName == "" {
		writeError(w, http.StatusBadRequest, "table name is required")
		return
	}
	quotedTable, err := quoteIdentifier(tableName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.storage.ExecuteRaw(r.Context(), fmt.Sprintf("DROP TABLE IF EXISTS %s", quotedTable)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 0, "message": fmt.Sprintf("table %q dropped", tableName)})
}

func truthy(value any) bool {
	flag, ok := value.(bool)
	return ok && flag
}

func sqlDefaultLiteral(value any) string {
	switch typed := value.(type) {
	case string:
		return "'" + strings.ReplaceAll(typed, "'", "''") + "'"
	case bool:
		if typed {
			return "1"
		}
		return "0"
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(typed, 10)
	case int:
		return strconv.Itoa(typed)
	default:
		return "NULL"
	}
}

// quoteIdentifier rejects names with quotes or backslashes instead of guessing.
func quoteIdentifier(name string) (string, error) {
	return storage.QuoteIdentifier(name)
}
