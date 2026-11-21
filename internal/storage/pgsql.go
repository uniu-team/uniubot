package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq" // registers the postgres driver

	"github.com/uniu-network/uniubot/internal/core/logging"
)

type PgsqlConfig struct {
	ConnectionString string
	PoolMin          int
	PoolMax          int
}

type PgsqlDriver struct {
	db     *sql.DB
	config PgsqlConfig
	logger *logging.Logger
}

func OpenPgsql(cfg PgsqlConfig, logger *logging.Logger) (*PgsqlDriver, error) {
	if cfg.ConnectionString == "" {
		return nil, fmt.Errorf("PostgreSQL config missing. Provide pgsql.connectionString")
	}
	db, err := sql.Open("postgres", cfg.ConnectionString)
	if err != nil {
		return nil, err
	}
	if cfg.PoolMax > 0 {
		db.SetMaxOpenConns(cfg.PoolMax)
	}
	if cfg.PoolMin > 0 {
		db.SetMaxIdleConns(cfg.PoolMin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		logger.Error("PostgreSQL connection failed: %v", err)
		return nil, err
	}
	logger.Info("PostgreSQL connected")
	return &PgsqlDriver{db: db, config: cfg, logger: logger}, nil
}

func (d *PgsqlDriver) Type() Engine { return EnginePgsql }

func (d *PgsqlDriver) Placeholder(index int) string { return fmt.Sprintf("$%d", index) }

func (d *PgsqlDriver) Execute(ctx context.Context, sqlText string, params ...any) error {
	if _, err := d.db.ExecContext(ctx, sqlText, params...); err != nil {
		d.logger.Error("PostgreSQL execute error: %s - %v", sqlText, err)
		return err
	}
	return nil
}

func (d *PgsqlDriver) Query(ctx context.Context, sqlText string, params ...any) ([]map[string]any, error) {
	rows, err := d.db.QueryContext(ctx, sqlText, params...)
	if err != nil {
		d.logger.Error("PostgreSQL query error: %s - %v", sqlText, err)
		return nil, err
	}
	result, err := scanRows(rows)
	if err != nil {
		d.logger.Error("PostgreSQL scan error: %s - %v", sqlText, err)
		return nil, err
	}
	return result, nil
}

func (d *PgsqlDriver) Run(ctx context.Context, sqlText string, params ...any) (RunResult, error) {
	result, err := d.db.ExecContext(ctx, sqlText, params...)
	if err != nil {
		d.logger.Error("PostgreSQL run error: %s - %v", sqlText, err)
		return RunResult{}, err
	}
	changes, _ := result.RowsAffected()
	return RunResult{Changes: changes, LastInsertRowID: 0}, nil
}

func (d *PgsqlDriver) Transaction(ctx context.Context, fn func(tx Driver) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	wrapped := &txDriver{base: d, tx: tx, logger: d.logger}
	if err := fn(wrapped); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			d.logger.Error("PostgreSQL transaction error: %v (rollback: %v)", err, rollbackErr)
		}
		return err
	}
	return tx.Commit()
}

func (d *PgsqlDriver) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

func (d *PgsqlDriver) Close() error {
	if d.db == nil {
		return nil
	}
	err := d.db.Close()
	d.db = nil
	d.logger.Info("PostgreSQL connection closed")
	return err
}
