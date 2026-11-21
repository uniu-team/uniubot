package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/uniu-network/uniubot/internal/core/logging"
)

type QueryOptions struct {
	Where   map[string]any
	Limit   int
	Offset  int
	OrderBy string
	Order   string // "asc" or "desc"
}

// ScopedClient prefixes every table name, so a component cannot reach another
// component's data.
type ScopedClient struct {
	driver Driver
	prefix string
	logger *logging.Logger

	mu            sync.RWMutex
	knownSchemas  map[string]TableSchema
	preparedTable map[string]bool
}

// NewScoped prefixes bot_<botId>_<scope>_<owner>, or <scope>_<owner> without a bot.
func NewScoped(driver Driver, owner string, scope Scope, botID string, logger *logging.Logger) *ScopedClient {
	safeOwner := sanitize(owner)
	prefix := fmt.Sprintf("%s_%s", scope, safeOwner)
	if botID != "" {
		prefix = fmt.Sprintf("bot_%s_%s_%s", sanitize(botID), scope, safeOwner)
	}
	return &ScopedClient{
		driver:        driver,
		prefix:        prefix,
		logger:        logger,
		knownSchemas:  map[string]TableSchema{},
		preparedTable: map[string]bool{},
	}
}

func (c *ScopedClient) Prefix() string { return c.prefix }

func (c *ScopedClient) Driver() Driver { return c.driver }

func (c *ScopedClient) resolveTable(table string) (string, error) {
	if !validIdentifier(table) {
		return "", fmt.Errorf("[ScopedDB:%s] Invalid table name: %q. Only alphanumeric and underscore characters allowed, must start with a letter or underscore", c.prefix, table)
	}
	return c.prefix + "_" + table, nil
}

func (c *ScopedClient) mapSimpleType(columnType string, engine Engine) string {
	typeMap := map[string]map[Engine]string{
		"text":    {EngineSQLite: "TEXT", EnginePgsql: "TEXT"},
		"integer": {EngineSQLite: "INTEGER", EnginePgsql: "INTEGER"},
		"real":    {EngineSQLite: "REAL", EnginePgsql: "REAL"},
		"blob":    {EngineSQLite: "BLOB", EnginePgsql: "BYTEA"},
		"json":    {EngineSQLite: "TEXT", EnginePgsql: "JSONB"},
		"boolean": {EngineSQLite: "INTEGER", EnginePgsql: "BOOLEAN"},
		"serial":  {EngineSQLite: "INTEGER PRIMARY KEY AUTOINCREMENT", EnginePgsql: "SERIAL PRIMARY KEY"},
	}
	normalized := strings.ToLower(strings.TrimSpace(columnType))
	if mapping, ok := typeMap[normalized]; ok {
		return mapping[engine]
	}
	return columnType
}

func (c *ScopedClient) mapColumnType(column ColumnDefinition, engine Engine) string {
	parts := []string{c.mapSimpleType(column.Type, engine)}
	if column.PrimaryKey {
		if engine == EngineSQLite {
			if column.AutoIncrement {
				parts = append(parts, "PRIMARY KEY AUTOINCREMENT")
			} else {
				parts = append(parts, "PRIMARY KEY")
			}
		} else {
			if column.AutoIncrement {
				parts = append(parts, "PRIMARY KEY GENERATED ALWAYS AS IDENTITY")
			} else {
				parts = append(parts, "PRIMARY KEY")
			}
		}
		return strings.Join(parts, " ")
	}
	// Modifiers are only applied to non primary key columns: schema parity.
	if column.NotNull {
		parts = append(parts, "NOT NULL")
	}
	if column.Unique {
		parts = append(parts, "UNIQUE")
	}
	if column.HasDefault {
		parts = append(parts, "DEFAULT "+sqlLiteral(column.Default))
	}
	return strings.Join(parts, " ")
}

func sqlLiteral(value any) string {
	switch typed := value.(type) {
	case string:
		return "'" + strings.ReplaceAll(typed, "'", "''") + "'"
	case bool:
		if typed {
			return "1"
		}
		return "0"
	case nil:
		return "NULL"
	default:
		return fmt.Sprint(typed)
	}
}

func (c *ScopedClient) CreateTable(ctx context.Context, table string, schema TableSchema) error {
	fullName, err := c.resolveTable(table)
	if err != nil {
		return err
	}
	engine := c.driver.Type()
	definitions := make([]string, 0, len(schema.Columns))
	for _, column := range schema.Columns {
		if !validIdentifier(column.Name) {
			return fmt.Errorf("[ScopedDB:%s] Invalid column name: %q", c.prefix, column.Name)
		}
		definitions = append(definitions, fmt.Sprintf("%q %s", column.Name, c.mapColumnType(column, engine)))
	}
	sqlText := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %q (\n  %s\n)", fullName, strings.Join(definitions, ",\n  "))
	if err := c.driver.Execute(ctx, sqlText); err != nil {
		return err
	}
	for _, index := range schema.Indexes {
		unique := ""
		if index.Unique {
			unique = "UNIQUE "
		}
		columns := make([]string, 0, len(index.Columns))
		for _, column := range index.Columns {
			quoted, err := QuoteIdentifier(column)
			if err != nil {
				return err
			}
			columns = append(columns, quoted)
		}
		indexName := fmt.Sprintf("idx_%s_%s", fullName, index.Name)
		indexSQL := fmt.Sprintf("CREATE %sINDEX IF NOT EXISTS %q ON %q (%s)", unique, indexName, fullName, strings.Join(columns, ", "))
		if err := c.driver.Execute(ctx, indexSQL); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.knownSchemas[table] = schema
	c.preparedTable[table] = true
	c.mu.Unlock()
	return nil
}

func (c *ScopedClient) DropTable(ctx context.Context, table string) error {
	fullName, err := c.resolveTable(table)
	if err != nil {
		return err
	}
	if err := c.driver.Execute(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %q", fullName)); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.knownSchemas, table)
	delete(c.preparedTable, table)
	c.mu.Unlock()
	c.logger.Info("[ScopedDB:%s] Table dropped: %s", c.prefix, fullName)
	return nil
}

func (c *ScopedClient) Insert(ctx context.Context, table string, data map[string]any) (RunResult, error) {
	fullName, err := c.resolveTable(table)
	if err != nil {
		return RunResult{}, err
	}
	columns, values, err := flattenRow(data)
	if err != nil {
		return RunResult{}, err
	}
	placeholders := make([]string, len(columns))
	for i := range columns {
		placeholders[i] = c.driver.Placeholder(i + 1)
	}
	sqlText := fmt.Sprintf("INSERT INTO %q (%s) VALUES (%s)", fullName, quoteList(columns), strings.Join(placeholders, ", "))
	return c.driver.Run(ctx, sqlText, values...)
}

func (c *ScopedClient) InsertMany(ctx context.Context, table string, rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	fullName, err := c.resolveTable(table)
	if err != nil {
		return err
	}
	columns := make([]string, 0, len(rows[0]))
	for key := range rows[0] {
		columns = append(columns, key)
	}
	sort.Strings(columns)
	return c.driver.Transaction(ctx, func(tx Driver) error {
		for _, row := range rows {
			values := make([]any, len(columns))
			for i, column := range columns {
				values[i] = row[column]
			}
			placeholders := make([]string, len(columns))
			for i := range columns {
				placeholders[i] = tx.Placeholder(i + 1)
			}
			sqlText := fmt.Sprintf("INSERT INTO %q (%s) VALUES (%s)", fullName, quoteList(columns), strings.Join(placeholders, ", "))
			if _, err := tx.Run(ctx, sqlText, values...); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *ScopedClient) Update(ctx context.Context, table string, data map[string]any, where map[string]any) (int64, error) {
	fullName, err := c.resolveTable(table)
	if err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, fmt.Errorf("[ScopedDB:%s] update() requires at least one field to update.", c.prefix)
	}
	if len(where) == 0 {
		return 0, fmt.Errorf("[ScopedDB:%s] update() requires at least one where condition to prevent accidental bulk updates.", c.prefix)
	}
	setColumns, setValues, err := flattenRow(data)
	if err != nil {
		return 0, err
	}
	whereColumns, whereValues, err := flattenRow(where)
	if err != nil {
		return 0, err
	}
	values := append(append([]any{}, setValues...), whereValues...)
	sets := make([]string, len(setColumns))
	for i, column := range setColumns {
		sets[i] = fmt.Sprintf("%q = %s", column, c.driver.Placeholder(i+1))
	}
	conditions := make([]string, len(whereColumns))
	for i, column := range whereColumns {
		conditions[i] = fmt.Sprintf("%q = %s", column, c.driver.Placeholder(len(setColumns)+i+1))
	}
	sqlText := fmt.Sprintf("UPDATE %q SET %s WHERE %s", fullName, strings.Join(sets, ", "), strings.Join(conditions, " AND "))
	result, err := c.driver.Run(ctx, sqlText, values...)
	if err != nil {
		return 0, err
	}
	return result.Changes, nil
}

func (c *ScopedClient) Delete(ctx context.Context, table string, where map[string]any) (int64, error) {
	if len(where) == 0 {
		return 0, fmt.Errorf("[ScopedDB:%s] delete() requires at least one where condition. Use deleteAll() to delete all rows.", c.prefix)
	}
	fullName, err := c.resolveTable(table)
	if err != nil {
		return 0, err
	}
	columns, values, err := flattenRow(where)
	if err != nil {
		return 0, err
	}
	conditions := make([]string, len(columns))
	for i, column := range columns {
		conditions[i] = fmt.Sprintf("%q = %s", column, c.driver.Placeholder(i+1))
	}
	sqlText := fmt.Sprintf("DELETE FROM %q WHERE %s", fullName, strings.Join(conditions, " AND "))
	result, err := c.driver.Run(ctx, sqlText, values...)
	if err != nil {
		return 0, err
	}
	return result.Changes, nil
}

func (c *ScopedClient) DeleteAll(ctx context.Context, table string) (int64, error) {
	fullName, err := c.resolveTable(table)
	if err != nil {
		return 0, err
	}
	result, err := c.driver.Run(ctx, fmt.Sprintf("DELETE FROM %q", fullName))
	if err != nil {
		return 0, err
	}
	return result.Changes, nil
}

func (c *ScopedClient) Select(ctx context.Context, table string, options QueryOptions) ([]map[string]any, error) {
	fullName, err := c.resolveTable(table)
	if err != nil {
		return nil, err
	}
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("SELECT * FROM %q", fullName))
	var values []any

	if len(options.Where) > 0 {
		columns, whereValues, err := flattenRow(options.Where)
		if err != nil {
			return nil, err
		}
		conditions := make([]string, len(columns))
		for i, column := range columns {
			conditions[i] = fmt.Sprintf("%q = %s", column, c.driver.Placeholder(i+1))
		}
		values = whereValues
		builder.WriteString(" WHERE ")
		builder.WriteString(strings.Join(conditions, " AND "))
	}
	if options.OrderBy != "" {
		quoted, err := QuoteIdentifier(options.OrderBy)
		if err != nil {
			return nil, err
		}
		order := options.Order
		if order == "" {
			order = "asc"
		}
		if strings.EqualFold(order, "desc") {
			order = "DESC"
		} else {
			order = "ASC"
		}
		builder.WriteString(fmt.Sprintf(" ORDER BY %s %s", quoted, order))
	}
	if options.Limit > 0 {
		builder.WriteString(fmt.Sprintf(" LIMIT %d", options.Limit))
	}
	if options.Offset > 0 {
		builder.WriteString(fmt.Sprintf(" OFFSET %d", options.Offset))
	}
	return c.driver.Query(ctx, builder.String(), values...)
}

func (c *ScopedClient) SelectOne(ctx context.Context, table string, where map[string]any) (map[string]any, error) {
	rows, err := c.Select(ctx, table, QueryOptions{Where: where, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func (c *ScopedClient) Count(ctx context.Context, table string, where map[string]any) (int, error) {
	fullName, err := c.resolveTable(table)
	if err != nil {
		return 0, err
	}
	sqlText := fmt.Sprintf("SELECT COUNT(*) AS cnt FROM %q", fullName)
	var values []any
	if len(where) > 0 {
		columns, whereValues, err := flattenRow(where)
		if err != nil {
			return 0, err
		}
		conditions := make([]string, len(columns))
		for i, column := range columns {
			conditions[i] = fmt.Sprintf("%q = %s", column, c.driver.Placeholder(i+1))
		}
		values = whereValues
		sqlText += " WHERE " + strings.Join(conditions, " AND ")
	}
	return c.scalarCount(ctx, sqlText, values...)
}

func (c *ScopedClient) scalarCount(ctx context.Context, sqlText string, params ...any) (int, error) {
	rows, err := c.driver.Query(ctx, sqlText, params...)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return toInt(rows[0]["cnt"]), nil
}

func (c *ScopedClient) HasTable(ctx context.Context, table string) (bool, error) {
	fullName, err := c.resolveTable(table)
	if err != nil {
		return false, err
	}
	if c.driver.Type() == EnginePgsql {
		rows, err := c.driver.Query(ctx, "SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename = $1", fullName)
		if err != nil {
			return false, err
		}
		return len(rows) > 0, nil
	}
	rows, err := c.driver.Query(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name=?", fullName)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

func (c *ScopedClient) DeleteWhereLessThan(ctx context.Context, table, column string, value any) (int64, error) {
	fullName, err := c.resolveTable(table)
	if err != nil {
		return 0, err
	}
	quoted, err := QuoteIdentifier(column)
	if err != nil {
		return 0, err
	}
	sqlText := fmt.Sprintf("DELETE FROM %q WHERE %s < %s", fullName, quoted, c.driver.Placeholder(1))
	result, err := c.driver.Run(ctx, sqlText, value)
	if err != nil {
		return 0, err
	}
	return result.Changes, nil
}

func (c *ScopedClient) CountWhereLessThan(ctx context.Context, table, column string, value any) (int, error) {
	fullName, err := c.resolveTable(table)
	if err != nil {
		return 0, err
	}
	quoted, err := QuoteIdentifier(column)
	if err != nil {
		return 0, err
	}
	sqlText := fmt.Sprintf("SELECT COUNT(*) AS cnt FROM %q WHERE %s < %s", fullName, quoted, c.driver.Placeholder(1))
	return c.scalarCount(ctx, sqlText, value)
}

func (c *ScopedClient) CountWhereGreaterThanOrEqual(ctx context.Context, table, column string, value any) (int, error) {
	fullName, err := c.resolveTable(table)
	if err != nil {
		return 0, err
	}
	quoted, err := QuoteIdentifier(column)
	if err != nil {
		return 0, err
	}
	sqlText := fmt.Sprintf("SELECT COUNT(*) AS cnt FROM %q WHERE %s >= %s", fullName, quoted, c.driver.Placeholder(1))
	return c.scalarCount(ctx, sqlText, value)
}

func (c *ScopedClient) CountWhereBetween(ctx context.Context, table, column string, start, end any) (int, error) {
	fullName, err := c.resolveTable(table)
	if err != nil {
		return 0, err
	}
	quoted, err := QuoteIdentifier(column)
	if err != nil {
		return 0, err
	}
	sqlText := fmt.Sprintf("SELECT COUNT(*) AS cnt FROM %q WHERE %s >= %s AND %s < %s", fullName, quoted, c.driver.Placeholder(1), quoted, c.driver.Placeholder(2))
	return c.scalarCount(ctx, sqlText, start, end)
}

func quoteList(columns []string) string {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = fmt.Sprintf("%q", column)
	}
	return strings.Join(quoted, ", ")
}

func flattenRow(data map[string]any) ([]string, []any, error) {
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("row data must not be empty")
	}
	columns := make([]string, 0, len(data))
	for column := range data {
		if !validIdentifier(column) {
			return nil, nil, fmt.Errorf("invalid column name: %q", column)
		}
		columns = append(columns, column)
	}
	sort.Strings(columns)
	values := make([]any, len(columns))
	for i, column := range columns {
		values[i] = data[column]
	}
	return columns, values, nil
}
