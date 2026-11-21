package storage

import (
	"context"
	"fmt"
	"sync"
)

// Repository is a table oriented wrapper; the table is created lazily.
type Repository struct {
	db       *ScopedClient
	Table    string
	schema   *TableSchema
	autoMake bool
	ready    bool
	mu       sync.Mutex
}

// NewRepository takes a nil schema for tables managed elsewhere.
func NewRepository(db *ScopedClient, table string, schema *TableSchema) *Repository {
	return &Repository{db: db, Table: table, schema: schema, autoMake: true}
}

func (r *Repository) EnsureTable(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ready {
		return nil
	}
	if r.schema != nil && r.autoMake {
		if err := r.db.CreateTable(ctx, r.Table, *r.schema); err != nil {
			return err
		}
	}
	r.ready = true
	return nil
}

func (r *Repository) Create(ctx context.Context, data map[string]any) (map[string]any, error) {
	if err := r.EnsureTable(ctx); err != nil {
		return nil, err
	}
	if _, err := r.db.Insert(ctx, r.Table, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (r *Repository) CreateMany(ctx context.Context, rows []map[string]any) error {
	if err := r.EnsureTable(ctx); err != nil {
		return err
	}
	return r.db.InsertMany(ctx, r.Table, rows)
}

func (r *Repository) Find(ctx context.Context, where map[string]any, options QueryOptions) ([]map[string]any, error) {
	if err := r.EnsureTable(ctx); err != nil {
		return nil, err
	}
	options.Where = where
	return r.db.Select(ctx, r.Table, options)
}

func (r *Repository) FindAll(ctx context.Context, options QueryOptions) ([]map[string]any, error) {
	if err := r.EnsureTable(ctx); err != nil {
		return nil, err
	}
	return r.db.Select(ctx, r.Table, options)
}

func (r *Repository) FindOne(ctx context.Context, where map[string]any) (map[string]any, error) {
	if err := r.EnsureTable(ctx); err != nil {
		return nil, err
	}
	return r.db.SelectOne(ctx, r.Table, where)
}

func (r *Repository) Update(ctx context.Context, data, where map[string]any) (int64, error) {
	if err := r.EnsureTable(ctx); err != nil {
		return 0, err
	}
	return r.db.Update(ctx, r.Table, data, where)
}

func (r *Repository) Delete(ctx context.Context, where map[string]any) (int64, error) {
	if err := r.EnsureTable(ctx); err != nil {
		return 0, err
	}
	return r.db.Delete(ctx, r.Table, where)
}

func (r *Repository) DeleteAll(ctx context.Context) (int64, error) {
	if err := r.EnsureTable(ctx); err != nil {
		return 0, err
	}
	return r.db.DeleteAll(ctx, r.Table)
}

func (r *Repository) Count(ctx context.Context, where map[string]any) (int, error) {
	if err := r.EnsureTable(ctx); err != nil {
		return 0, err
	}
	return r.db.Count(ctx, r.Table, where)
}

func (r *Repository) HasTable(ctx context.Context) (bool, error) {
	return r.db.HasTable(ctx, r.Table)
}

func (r *Repository) DropTable(ctx context.Context) error {
	if err := r.db.DropTable(ctx, r.Table); err != nil {
		return err
	}
	r.mu.Lock()
	r.ready = false
	r.mu.Unlock()
	return nil
}

// SchemaFor builds a schema from a column slice; order is significant.
func SchemaFor(columns []ColumnDefinition) *TableSchema {
	if len(columns) == 0 {
		return nil
	}
	for i := range columns {
		if columns[i].Name == "" {
			panic(fmt.Sprintf("storage: column %d has no name", i))
		}
	}
	return &TableSchema{Columns: columns}
}
