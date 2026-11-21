package services

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uniu-network/uniubot/internal/core/logging"
)

type SchedulerConfig struct {
	RetentionDays int
	CleanupTime   string
	Timezone      string
}

// Scheduler runs the daily jobs with a single timer.
type Scheduler struct {
	counter *Counter
	config  SchedulerConfig
	logger  *logging.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewScheduler(counter *Counter, config SchedulerConfig, logger *logging.Logger) *Scheduler {
	return &Scheduler{counter: counter, config: config, logger: logger}
}

func parseCleanupTime(value string) (int, int) {
	parts := strings.Split(value, ":")
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	minute := 0
	if len(parts) > 1 {
		minute, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
	}
	if err != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 1, 0
	}
	return hour, minute
}

func (s *Scheduler) Start(parent context.Context) {
	hour, minute := parseCleanupTime(s.config.CleanupTime)
	location := loadLocation(s.config.Timezone)
	if s.config.Timezone == "" {
		location = time.Local
	}
	s.logger.Info("[Scheduler] Daily snapshot scheduled at %02d:%02d (%s)", hour, minute, location)
	if s.config.RetentionDays > 0 {
		s.logger.Info("[Scheduler] Message cleanup scheduled: retain %d days", s.config.RetentionDays)
	}

	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			next := nextOccurrence(time.Now(), hour, minute, location)
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				s.runOnce(ctx)
			}
		}
	}()
}

func nextOccurrence(now time.Time, hour, minute int, location *time.Location) time.Time {
	local := now.In(location)
	next := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, location)
	if !next.After(local) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func (s *Scheduler) runOnce(ctx context.Context) {
	if err := s.counter.SnapshotDailyStats(ctx, "", s.config.Timezone); err != nil {
		s.logger.Error("Failed to snapshot daily stats: %v", err)
	}
	if s.config.RetentionDays <= 0 {
		return
	}
	count, err := s.counter.CleanExpiredMessages(ctx, s.config.RetentionDays)
	if err != nil {
		s.logger.Error("Failed to clean expired messages: %v", err)
	} else {
		s.logger.Info("[Scheduler] Cleaned %d expired messages (retention: %d days)", count, s.config.RetentionDays)
	}
	stats, err := s.counter.CleanOldDailyStats(ctx, s.config.RetentionDays)
	if err != nil {
		s.logger.Error("Failed to clean old daily stats: %v", err)
	} else {
		s.logger.Info("[Scheduler] Cleaned %d old daily stats entries (keep: %d days)", stats, s.config.RetentionDays)
	}
}

func (s *Scheduler) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}
