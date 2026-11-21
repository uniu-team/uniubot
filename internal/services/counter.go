// Package services holds message counters, statistics and scheduled maintenance.
package services

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	_ "time/tzdata" // embeds the IANA database so timezones work on Windows

	"github.com/uniu-network/uniubot/internal/core/events"
	"github.com/uniu-network/uniubot/internal/core/logging"
	"github.com/uniu-network/uniubot/internal/storage"
)

type TodayStats struct {
	Messages int `json:"messages"`
	Users    int `json:"users"`
	Groups   int `json:"groups"`
}

type DailyStats struct {
	ID       int64  `json:"id,omitempty"`
	StatDate string `json:"stat_date"`
	Groups   int    `json:"groups"`
	Users    int    `json:"users"`
	Messages int    `json:"messages"`
}

type MessageRow struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	EventJSON string `json:"event_json"`
	CreatedAt int64  `json:"created_at"`
}

type Counter struct {
	db     *storage.ScopedClient
	logger *logging.Logger

	groups     *storage.Repository
	users      *storage.Repository
	messages   *storage.Repository
	dailyStats *storage.Repository

	initialized bool
}

func NewCounter(db *storage.ScopedClient, logger *logging.Logger) *Counter {
	return &Counter{
		db:     db,
		logger: logger,
		groups: storage.NewRepository(db, "groups", storage.SchemaFor([]storage.ColumnDefinition{
			{Name: "group_id", Type: "integer", PrimaryKey: true},
			{Name: "created_at", Type: "integer", NotNull: true},
		})),
		users: storage.NewRepository(db, "users", storage.SchemaFor([]storage.ColumnDefinition{
			{Name: "user_id", Type: "integer", PrimaryKey: true},
			{Name: "created_at", Type: "integer", NotNull: true},
		})),
		messages: storage.NewRepository(db, "messages", storage.SchemaFor([]storage.ColumnDefinition{
			{Name: "id", Type: "integer", PrimaryKey: true, AutoIncrement: true},
			{Name: "type", Type: "text", NotNull: true, Default: "recv", HasDefault: true},
			{Name: "event_json", Type: "text", NotNull: true},
			{Name: "created_at", Type: "integer", NotNull: true},
		})),
		dailyStats: storage.NewRepository(db, "daily_stats", storage.SchemaFor([]storage.ColumnDefinition{
			{Name: "id", Type: "integer", PrimaryKey: true, AutoIncrement: true},
			{Name: "stat_date", Type: "text", NotNull: true, Unique: true},
			{Name: "groups", Type: "integer", NotNull: true, Default: 0, HasDefault: true},
			{Name: "users", Type: "integer", NotNull: true, Default: 0, HasDefault: true},
			{Name: "messages", Type: "integer", NotNull: true, Default: 0, HasDefault: true},
		})),
	}
}

func (c *Counter) Init(ctx context.Context) error {
	if c.initialized {
		return nil
	}
	for _, repo := range []*storage.Repository{c.groups, c.users, c.messages, c.dailyStats} {
		if err := repo.EnsureTable(ctx); err != nil {
			return err
		}
	}
	c.migrateAddTypeColumn(ctx)
	c.initialized = true
	return nil
}

// migrateAddTypeColumn probes with SELECT then ALTER TABLE; failures are ignored
// on purpose so old databases still open.
func (c *Counter) migrateAddTypeColumn(ctx context.Context) {
	if _, err := c.messages.Find(ctx, map[string]any{"type": "recv"}, storage.QueryOptions{Limit: 1}); err == nil {
		return
	}
	tableName := fmt.Sprintf("%s_messages", c.db.Prefix())
	stmt := fmt.Sprintf(`ALTER TABLE "%s" ADD COLUMN "type" TEXT NOT NULL DEFAULT 'recv'`, tableName)
	if err := c.db.Driver().Execute(ctx, stmt); err != nil {
		c.logger.Debug("[counter] messages.type already present or migration failed: %v", err)
	}
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func (c *Counter) AddGroup(ctx context.Context, groupID int64) error {
	existing, err := c.groups.FindOne(ctx, map[string]any{"group_id": groupID})
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	_, err = c.groups.Create(ctx, map[string]any{"group_id": groupID, "created_at": nowMillis()})
	return err
}

func (c *Counter) AddUser(ctx context.Context, userID int64) error {
	existing, err := c.users.FindOne(ctx, map[string]any{"user_id": userID})
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	_, err = c.users.Create(ctx, map[string]any{"user_id": userID, "created_at": nowMillis()})
	return err
}

func (c *Counter) AddMessage(ctx context.Context, event *events.Event, messageType string) error {
	if messageType == "" {
		messageType = "recv"
	}
	payload, err := event.Payload()
	if err != nil {
		return err
	}
	_, err = c.messages.Create(ctx, map[string]any{
		"type":       messageType,
		"event_json": string(payload),
		"created_at": nowMillis(),
	})
	return err
}

func (c *Counter) AddMessageJSON(ctx context.Context, eventJSON, messageType string) error {
	if messageType == "" {
		messageType = "recv"
	}
	_, err := c.messages.Create(ctx, map[string]any{
		"type":       messageType,
		"event_json": eventJSON,
		"created_at": nowMillis(),
	})
	return err
}

func (c *Counter) AddBotSendMessage(ctx context.Context, event *events.Event) error {
	payload, err := event.Payload()
	if err != nil {
		return err
	}
	_, err = c.messages.Create(ctx, map[string]any{
		"type":       "bot_send",
		"event_json": string(payload),
		"created_at": nowMillis(),
	})
	return err
}

func (c *Counter) GetTodayStats(ctx context.Context, timezone string) (TodayStats, error) {
	location := loadLocation(timezone)
	today := DateKey(time.Now(), location)
	start := DateStart(today, location)

	groups, err := c.groups.Count(ctx, nil)
	if err != nil {
		return TodayStats{}, err
	}
	users, err := c.users.Count(ctx, nil)
	if err != nil {
		return TodayStats{}, err
	}
	messages, err := c.db.CountWhereGreaterThanOrEqual(ctx, "messages", "created_at", start.UnixMilli())
	if err != nil {
		return TodayStats{}, err
	}
	return TodayStats{Messages: messages, Groups: groups, Users: users}, nil
}

func (c *Counter) GetStatsForDate(ctx context.Context, dateKey, timezone string) (DailyStats, error) {
	location := loadLocation(timezone)
	start := DateStart(dateKey, location)
	end := DateStart(AddDays(dateKey, 1), location)

	groups, err := c.db.CountWhereLessThan(ctx, "groups", "created_at", end.UnixMilli())
	if err != nil {
		return DailyStats{}, err
	}
	users, err := c.db.CountWhereLessThan(ctx, "users", "created_at", end.UnixMilli())
	if err != nil {
		return DailyStats{}, err
	}
	messages, err := c.db.CountWhereBetween(ctx, "messages", "created_at", start.UnixMilli(), end.UnixMilli())
	if err != nil {
		return DailyStats{}, err
	}
	return DailyStats{StatDate: dateKey, Groups: groups, Users: users, Messages: messages}, nil
}

// GetDailyStats returns the last limit days, oldest first.
func (c *Counter) GetDailyStats(ctx context.Context, limit int, timezone string) ([]DailyStats, error) {
	location := loadLocation(timezone)
	if limit < 1 {
		limit = 1
	}
	today := DateKey(time.Now(), location)
	startDate := AddDays(today, -(limit - 1))
	out := make([]DailyStats, 0, limit)
	for i := 0; i < limit; i++ {
		stats, err := c.GetStatsForDate(ctx, AddDays(startDate, i), timezone)
		if err != nil {
			return nil, err
		}
		out = append(out, stats)
	}
	return out, nil
}

func (c *Counter) GetMessageCount(ctx context.Context, messageType string) (int, error) {
	if messageType != "" {
		return c.messages.Count(ctx, map[string]any{"type": messageType})
	}
	return c.messages.Count(ctx, nil)
}

func (c *Counter) GetMessages(ctx context.Context, limit, offset int, messageType string) ([]MessageRow, error) {
	options := storage.QueryOptions{OrderBy: "id", Order: "desc", Limit: limit, Offset: offset}
	var rows []map[string]any
	var err error
	if messageType != "" {
		rows, err = c.messages.Find(ctx, map[string]any{"type": messageType}, options)
	} else {
		rows, err = c.messages.FindAll(ctx, options)
	}
	if err != nil {
		return nil, err
	}
	out := make([]MessageRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, MessageRow{
			ID:        int64(toInt64(row["id"])),
			Type:      asString(row["type"]),
			EventJSON: asString(row["event_json"]),
			CreatedAt: toInt64(row["created_at"]),
		})
	}
	return out, nil
}

func (c *Counter) SnapshotDailyStats(ctx context.Context, dateKey, timezone string) error {
	if dateKey == "" {
		location := loadLocation(timezone)
		dateKey = AddDays(DateKey(time.Now(), location), -1)
	}
	stats, err := c.GetStatsForDate(ctx, dateKey, timezone)
	if err != nil {
		return err
	}
	existing, err := c.dailyStats.FindOne(ctx, map[string]any{"stat_date": dateKey})
	if err != nil {
		return err
	}
	if existing != nil {
		_, err = c.dailyStats.Update(ctx, map[string]any{
			"groups":   stats.Groups,
			"users":    stats.Users,
			"messages": stats.Messages,
		}, map[string]any{"stat_date": dateKey})
		return err
	}
	_, err = c.dailyStats.Create(ctx, map[string]any{
		"stat_date": dateKey,
		"groups":    stats.Groups,
		"users":     stats.Users,
		"messages":  stats.Messages,
	})
	return err
}

func (c *Counter) CleanExpiredMessages(ctx context.Context, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	threshold := nowMillis() - int64(retentionDays)*86400000
	return c.db.DeleteWhereLessThan(ctx, "messages", "created_at", threshold)
}

func (c *Counter) CleanOldDailyStats(ctx context.Context, keepDays int) (int64, error) {
	if keepDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays).UTC().Format("2006-01-02")
	return c.db.DeleteWhereLessThan(ctx, "daily_stats", "stat_date", cutoff)
}

func (c *Counter) CleanToday(ctx context.Context) error {
	if _, err := c.messages.DeleteAll(ctx); err != nil {
		return err
	}
	if _, err := c.groups.DeleteAll(ctx); err != nil {
		return err
	}
	if _, err := c.users.DeleteAll(ctx); err != nil {
		return err
	}
	count, err := c.dailyStats.Count(ctx, nil)
	if err != nil {
		return err
	}
	if count >= 30 {
		rows, err := c.dailyStats.FindAll(ctx, storage.QueryOptions{OrderBy: "id", Order: "asc"})
		if err != nil {
			return err
		}
		if len(rows) > 29 {
			for _, row := range rows[:len(rows)-29] {
				if _, err := c.dailyStats.Delete(ctx, map[string]any{"id": toInt64(row["id"])}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func loadLocation(timezone string) *time.Location {
	if timezone == "" {
		return time.Local
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Local
	}
	return location
}

// SystemTimezone probes TZ, /etc/timezone and /etc/localtime: time.Local only
// reports "Local" when TZ is unset.
func SystemTimezone() string {
	if name := strings.TrimSpace(os.Getenv("TZ")); name != "" && name != "Local" {
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}
	if raw, err := os.ReadFile("/etc/timezone"); err == nil {
		name := strings.TrimSpace(string(raw))
		if name != "" {
			if _, err := time.LoadLocation(name); err == nil {
				return name
			}
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		const marker = "zoneinfo/"
		if index := strings.LastIndex(target, marker); index >= 0 {
			name := target[index+len(marker):]
			if _, err := time.LoadLocation(name); err == nil {
				return name
			}
		}
	}
	name := time.Local.String()
	if name == "" || name == "Local" || name == "UTC" {
		return "UTC"
	}
	return name
}

func DateKey(value time.Time, location *time.Location) string {
	return value.In(location).Format("2006-01-02")
}

func AddDays(dateKey string, days int) string {
	parsed, err := time.Parse("2006-01-02", dateKey)
	if err != nil {
		return dateKey
	}
	return parsed.AddDate(0, 0, days).Format("2006-01-02")
}

func DateStart(dateKey string, location *time.Location) time.Time {
	parsed, err := time.ParseInLocation("2006-01-02", dateKey, location)
	if err != nil {
		return time.Now()
	}
	return parsed
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.RawMessage:
		return string(typed)
	case []byte:
		return string(typed)
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

func toInt64(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	default:
		return 0
	}
}
