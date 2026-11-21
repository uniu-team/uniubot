package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the pure Go SQLite driver

	"github.com/uniu-network/uniubot/internal/core/logging"
)

// SQLiteDriver uses the pure Go modernc driver, so cross compilation needs no cgo.
type SQLiteDriver struct {
	db     *sql.DB
	file   string
	logger *logging.Logger
}

func OpenSQLite(file string, logger *logging.Logger) (*SQLiteDriver, error) {
	if file == "" {
		file = "data/skibot.sqlite"
	}
	if dir := filepath.Dir(file); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", file)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection keeps writes serialised and avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	driver := &SQLiteDriver{db: db, file: file, logger: logger}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	logger.Info("SQLite connected: %s", file)
	return driver, nil
}

func (d *SQLiteDriver) Type() Engine { return EngineSQLite }

func (d *SQLiteDriver) Placeholder(int) string { return "?" }

func (d *SQLiteDriver) Execute(ctx context.Context, sqlText string, params ...any) error {
	if _, err := d.db.ExecContext(ctx, sqlText, params...); err != nil {
		d.logger.Error("SQLite execute error: %s - %v", sqlText, err)
		return err
	}
	return nil
}

func (d *SQLiteDriver) Query(ctx context.Context, sqlText string, params ...any) ([]map[string]any, error) {
	start := time.Now()
	rows, err := d.db.QueryContext(ctx, sqlText, params...)
	if err != nil {
		d.logger.Error("SQLite query error: %s - %v", sqlText, err)
		return nil, err
	}
	result, err := scanRows(rows)
	if err != nil {
		d.logger.Error("SQLite scan error: %s - %v", sqlText, err)
		return nil, err
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		d.logger.Warn("slow SQLite query (%s): %s", elapsed, sqlText)
	}
	return result, nil
}

func (d *SQLiteDriver) Run(ctx context.Context, sqlText string, params ...any) (RunResult, error) {
	result, err := d.db.ExecContext(ctx, sqlText, params...)
	if err != nil {
		d.logger.Error("SQLite run error: %s - %v", sqlText, err)
		return RunResult{}, err
	}
	changes, _ := result.RowsAffected()
	insertID, _ := result.LastInsertId()
	return RunResult{Changes: changes, LastInsertRowID: insertID}, nil
}

func (d *SQLiteDriver) Transaction(ctx context.Context, fn func(tx Driver) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	wrapped := &txDriver{base: d, tx: tx, logger: d.logger}
	if err := fn(wrapped); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			d.logger.Error("SQLite transaction error: %v (rollback: %v)", err, rollbackErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		d.logger.Error("SQLite transaction commit error: %v", err)
		return err
	}
	return nil
}

func (d *SQLiteDriver) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

func (d *SQLiteDriver) Close() error {
	if d.db == nil {
		return nil
	}
	err := d.db.Close()
	d.db = nil
	d.logger.Info("SQLite connection closed")
	return err
}

func (d *SQLiteDriver) File() string { return d.file }

type txDriver struct {
	base   Driver
	tx     *sql.Tx
	logger *logging.Logger
}

func (t *txDriver) Type() Engine               { return t.base.Type() }
func (t *txDriver) Placeholder(i int) string   { return t.base.Placeholder(i) }
func (t *txDriver) Ping(context.Context) error { return nil }
func (t *txDriver) Close() error               { return nil }

func (t *txDriver) Execute(ctx context.Context, sqlText string, params ...any) error {
	if _, err := t.tx.ExecContext(ctx, sqlText, params...); err != nil {
		t.logger.Error("transaction execute error: %s - %v", sqlText, err)
		return err
	}
	return nil
}

func (t *txDriver) Query(ctx context.Context, sqlText string, params ...any) ([]map[string]any, error) {
	rows, err := t.tx.QueryContext(ctx, sqlText, params...)
	if err != nil {
		t.logger.Error("transaction query error: %s - %v", sqlText, err)
		return nil, err
	}
	return scanRows(rows)
}

func (t *txDriver) Run(ctx context.Context, sqlText string, params ...any) (RunResult, error) {
	result, err := t.tx.ExecContext(ctx, sqlText, params...)
	if err != nil {
		t.logger.Error("transaction run error: %s - %v", sqlText, err)
		return RunResult{}, err
	}
	changes, _ := result.RowsAffected()
	insertID, _ := result.LastInsertId()
	return RunResult{Changes: changes, LastInsertRowID: insertID}, nil
}

func (t *txDriver) Transaction(ctx context.Context, fn func(tx Driver) error) error {
	return fn(t)
}
