package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/uniu-network/uniubot/internal/core/config"
	"github.com/uniu-network/uniubot/internal/core/logging"
)

var ErrNotInitialized = errors.New("[DatabaseManager] Not initialized. Call init() first.")

type Manager struct {
	mu          sync.RWMutex
	driver      Driver
	engine      Engine
	initialized bool
	logger      *logging.Logger
}

func NewManager(logger *logging.Logger) *Manager {
	return &Manager{logger: logger}
}

type Options struct {
	Engine     Engine
	SQLiteFile string
	Pgsql      PgsqlConfig
}

func OptionsFromConfig(cfg *config.Store) Options {
	engine := Engine(cfg.String("database.engine", "sqlite"))
	if engine != EngineSQLite && engine != EnginePgsql {
		engine = EngineSQLite
	}
	return Options{
		Engine:     engine,
		SQLiteFile: cfg.String("database.sqlite.file", "./data/skibot.sqlite"),
		Pgsql: PgsqlConfig{
			ConnectionString: cfg.String("database.pgsql.connectionString", ""),
			PoolMin:          cfg.Int("database.pgsql.pool.min", 2),
			PoolMax:          cfg.Int("database.pgsql.pool.max", 10),
		},
	}
}

// Init is a no-op when called twice.
func (m *Manager) Init(opts Options) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.initialized {
		m.logger.Warn("[DatabaseManager] Already initialized, skipping")
		return nil
	}
	var driver Driver
	var err error
	switch opts.Engine {
	case EnginePgsql:
		driver, err = OpenPgsql(opts.Pgsql, m.logger)
	default:
		driver, err = OpenSQLite(opts.SQLiteFile, m.logger)
	}
	if err != nil {
		return err
	}
	m.driver = driver
	m.engine = opts.Engine
	m.initialized = true
	m.logger.Info("[DatabaseManager] Initialized with engine: %s", opts.Engine)
	return nil
}

func (m *Manager) getDriver() (Driver, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.driver == nil || !m.initialized {
		return nil, ErrNotInitialized
	}
	return m.driver, nil
}

func (m *Manager) Driver() (Driver, error) { return m.getDriver() }

func (m *Manager) Core(namespace string) (*ScopedClient, error) {
	driver, err := m.getDriver()
	if err != nil {
		return nil, err
	}
	return NewScoped(driver, namespace, ScopeCore, "", m.logger), nil
}

func (m *Manager) Plugin(pluginName string) (*ScopedClient, error) {
	driver, err := m.getDriver()
	if err != nil {
		return nil, err
	}
	return NewScoped(driver, pluginName, ScopePlugin, "", m.logger), nil
}

func (m *Manager) Adapter(adapterID string) (*ScopedClient, error) {
	driver, err := m.getDriver()
	if err != nil {
		return nil, err
	}
	return NewScoped(driver, adapterID, ScopeAdapter, "", m.logger), nil
}

func (m *Manager) BotCore(botID, namespace string) (*ScopedClient, error) {
	driver, err := m.getDriver()
	if err != nil {
		return nil, err
	}
	return NewScoped(driver, namespace, ScopeCore, botID, m.logger), nil
}

func (m *Manager) BotPlugin(botID, pluginName string) (*ScopedClient, error) {
	driver, err := m.getDriver()
	if err != nil {
		return nil, err
	}
	return NewScoped(driver, pluginName, ScopePlugin, botID, m.logger), nil
}

func (m *Manager) BotAdapter(botID, adapterID string) (*ScopedClient, error) {
	driver, err := m.getDriver()
	if err != nil {
		return nil, err
	}
	return NewScoped(driver, adapterID, ScopeAdapter, botID, m.logger), nil
}

// SharedBotCore is the process wide client the database dashboard queries.
func (m *Manager) SharedBotCore(botID, namespace string) (*ScopedClient, error) {
	return m.BotCore(botID, namespace)
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.driver == nil {
		return nil
	}
	err := m.driver.Close()
	m.driver = nil
	m.initialized = false
	return err
}

func (m *Manager) IsInitialized() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.initialized
}

func (m *Manager) EngineType() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.engine == "" {
		return "unknown"
	}
	return string(m.engine)
}

func (m *Manager) ListTables(ctx context.Context) ([]string, error) {
	driver, err := m.getDriver()
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	if driver.Type() == EngineSQLite {
		rows, err = driver.Query(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(rows))
		for _, row := range rows {
			names = append(names, fmt.Sprint(row["name"]))
		}
		return names, nil
	}
	rows, err = driver.Query(ctx, "SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, fmt.Sprint(row["tablename"]))
	}
	return names, nil
}

type ColumnInfo struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	NotNull      bool   `json:"notNull"`
	DefaultValue any    `json:"defaultValue"`
	PrimaryKey   bool   `json:"primaryKey"`
}

func (m *Manager) DescribeTable(ctx context.Context, tableName string) ([]ColumnInfo, error) {
	driver, err := m.getDriver()
	if err != nil {
		return nil, err
	}
	if !validIdentifier(tableName) {
		return nil, fmt.Errorf("invalid table name: %q", tableName)
	}
	if driver.Type() == EngineSQLite {
		rows, err := driver.Query(ctx, fmt.Sprintf("PRAGMA table_info(%q)", tableName))
		if err != nil {
			return nil, err
		}
		out := make([]ColumnInfo, 0, len(rows))
		for _, row := range rows {
			out = append(out, ColumnInfo{
				Name:         fmt.Sprint(row["name"]),
				Type:         fmt.Sprint(row["type"]),
				NotNull:      toInt(row["notnull"]) == 1,
				DefaultValue: row["dflt_value"],
				PrimaryKey:   toInt(row["pk"]) == 1,
			})
		}
		return out, nil
	}
	rows, err := driver.Query(ctx,
		"SELECT column_name, data_type, is_nullable, column_default FROM information_schema.columns WHERE table_name = $1 ORDER BY ordinal_position",
		tableName)
	if err != nil {
		return nil, err
	}
	out := make([]ColumnInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, ColumnInfo{
			Name:         fmt.Sprint(row["column_name"]),
			Type:         fmt.Sprint(row["data_type"]),
			NotNull:      fmt.Sprint(row["is_nullable"]) == "NO",
			DefaultValue: row["column_default"],
			PrimaryKey:   false,
		})
	}
	return out, nil
}

func (m *Manager) RowCount(ctx context.Context, tableName string) (int, error) {
	driver, err := m.getDriver()
	if err != nil {
		return 0, err
	}
	if !validIdentifier(tableName) {
		return 0, fmt.Errorf("invalid table name: %q", tableName)
	}
	rows, err := driver.Query(ctx, fmt.Sprintf("SELECT COUNT(*) AS cnt FROM %q", tableName))
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return toInt(rows[0]["cnt"]), nil
}

func (m *Manager) QueryRaw(ctx context.Context, sqlText string, params ...any) ([]map[string]any, error) {
	driver, err := m.getDriver()
	if err != nil {
		return nil, err
	}
	return driver.Query(ctx, sqlText, params...)
}

func (m *Manager) ExecuteRaw(ctx context.Context, sqlText string, params ...any) (RunResult, error) {
	driver, err := m.getDriver()
	if err != nil {
		return RunResult{}, err
	}
	return driver.Run(ctx, sqlText, params...)
}

func (m *Manager) Placeholder(index int) string {
	driver, err := m.getDriver()
	if err != nil {
		return "?"
	}
	return driver.Placeholder(index)
}
