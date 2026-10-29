package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
)

// CleanupInput selects the completed tasks Cleanup removes.
type CleanupInput struct {
	// OlderThanDays keeps tasks that completed less than this many days ago;
	// 0 removes every completed task.
	OlderThanDays int
	// QueueID restricts the cleanup to one queue (empty: all).
	QueueID string
	// Files also deletes the downloaded files of the removed tasks.
	Files bool
	// DryRun only counts what would be removed.
	DryRun bool
}

// CleanupResult says what Cleanup did (or, for a dry run, would do).
type CleanupResult struct {
	// Matched is the number of completed tasks that qualified.
	Matched int `json:"matched"`
	// Bytes is their total size (tasks of unknown size count as 0).
	Bytes int64 `json:"bytes"`
	// Deleted and Failed count the tasks removed and the ones that could not
	// be; both are 0 for a dry run.
	Deleted int `json:"deleted"`
	Failed  int `json:"failed"`
}

// maxCleanup bounds how many tasks one Cleanup call removes.
const maxCleanup = 100_000

// Cleanup removes completed tasks, optionally with their files. It is
// domain.ErrInvalidArgument to give a negative age or an unknown queue.
func (s *TaskService) Cleanup(ctx context.Context, in CleanupInput) (*CleanupResult, error) {
	if in.OlderThanDays < 0 {
		return nil, domain.InvalidArgumentCode("cleanup.bad_age", nil, "service: cleanup: olderThanDays must not be negative")
	}
	f := storage.TaskFilter{
		QueueID: in.QueueID, Statuses: []domain.TaskStatus{domain.StatusCompleted},
		Sort: storage.TaskSortCompleted, Limit: storage.MaxTaskLimit,
	}
	if in.OlderThanDays > 0 {
		f.CompletedBefore = s.d.now().Add(-time.Duration(in.OlderThanDays) * 24 * time.Hour)
	}
	var (
		ids []string
		res CleanupResult
	)
	for len(ids) < maxCleanup {
		page, err := s.List(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("service: cleanup: %w", err)
		}
		for _, t := range page.Tasks {
			ids = append(ids, t.ID)
			res.Bytes += max(t.Size, 0)
		}
		if page.NextCursor == "" {
			break
		}
		f.Cursor = page.NextCursor
	}
	res.Matched = len(ids)
	if in.DryRun || len(ids) == 0 {
		return &res, nil
	}
	for _, r := range s.Delete(ctx, ids, DeleteOptions{Files: in.Files}) {
		if r.Err != nil {
			res.Failed++
		} else {
			res.Deleted++
		}
	}
	return &res, nil
}

// Housekeeper applies the retentionDays setting: it removes completed tasks
// from the list once they are older than that. Files are never deleted.
type Housekeeper struct {
	tasks *TaskService
	d     Deps
}

// NewHousekeeper creates a Housekeeper.
func NewHousekeeper(d Deps, tasks *TaskService) *Housekeeper {
	return &Housekeeper{tasks: tasks, d: d.withDefaults()}
}

// HousekeepingInterval is how often Run applies the retention.
const HousekeepingInterval = time.Hour

// RunOnce applies the retention now and returns how many tasks it removed.
func (h *Housekeeper) RunOnce(ctx context.Context) (int, error) {
	g, err := h.d.Store.Settings().GetGlobal(ctx)
	if err != nil {
		return 0, fmt.Errorf("service: housekeeping: %w", err)
	}
	days := settings.Merge(h.d.Defaults(), g).RetentionDays.Value()
	if days <= 0 {
		return 0, nil
	}
	res, err := h.tasks.Cleanup(ctx, CleanupInput{OlderThanDays: days})
	if err != nil {
		return 0, err
	}
	if res.Deleted > 0 || res.Failed > 0 {
		h.d.Logger.InfoContext(ctx, "housekeeping removed completed tasks", "removed", res.Deleted, "failed", res.Failed, "retention_days", days)
	}
	return res.Deleted, nil
}

// Run applies the retention every HousekeepingInterval until ctx is done,
// the first time shortly after the start.
func (h *Housekeeper) Run(ctx context.Context) error {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			if _, err := h.RunOnce(ctx); err != nil && ctx.Err() == nil {
				h.d.Logger.WarnContext(ctx, "housekeeping failed", slog.Any("error", err))
			}
			timer.Reset(HousekeepingInterval)
		}
	}
}
