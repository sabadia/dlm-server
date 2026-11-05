package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

// AutoRetryInterval is how often the AutoRetrier looks for tasks whose retry
// is due.
const AutoRetryInterval = 10 * time.Second

// AutoRetrier starts failed tasks again when their automatic retry is due: a
// task that gave up on a network problem is marked with TaskError.Retry (see
// Runner.fail) and waits in the error state until then.
type AutoRetrier struct {
	tasks *TaskService
	d     Deps
}

// NewAutoRetrier creates an AutoRetrier.
func NewAutoRetrier(d Deps, tasks *TaskService) *AutoRetrier {
	return &AutoRetrier{tasks: tasks, d: d.withDefaults()}
}

// RunOnce queues every task whose retry is due and returns how many it did.
func (a *AutoRetrier) RunOnce(ctx context.Context) (int, error) {
	now := a.d.now()
	started := 0
	cursor := ""
	for {
		page, err := a.d.Store.Tasks().List(ctx, storage.TaskFilter{Statuses: []domain.TaskStatus{domain.StatusError}, Cursor: cursor})
		if err != nil {
			return started, fmt.Errorf("service: auto retry: %w", err)
		}
		for _, t := range page.Tasks {
			if t.Error == nil || t.Error.Retry == nil || t.Error.Retry.NextAt.After(now) {
				continue
			}
			if err := a.queue(ctx, t.ID); err != nil {
				a.d.Logger.WarnContext(ctx, "automatic retry could not start", "task_id", t.ID, slog.Any("error", err))
				continue
			}
			started++
		}
		if page.NextCursor == "" {
			return started, nil
		}
		cursor = page.NextCursor
	}
}

// queue moves the task to queued again, unless something else (a person)
// has dealt with it since it was listed. The retry counter is kept.
func (a *AutoRetrier) queue(ctx context.Context, id string) error {
	return a.d.withTaskLock(ctx, id, func() error { //nolint:wrapcheck // wrapped below
		t, err := a.d.Store.Tasks().Get(ctx, id)
		if err != nil {
			return fmt.Errorf("get: %w", err)
		}
		if t.Status != domain.StatusError || t.Error == nil || t.Error.Retry == nil || t.Error.Retry.NextAt.After(a.d.now()) {
			return nil
		}
		_, err = a.tasks.transitionLocked(ctx, id, domain.StatusQueued, false)
		return err
	})
}

// Run checks every AutoRetryInterval until ctx is done.
func (a *AutoRetrier) Run(ctx context.Context) error {
	t := time.NewTicker(AutoRetryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if n, err := a.RunOnce(ctx); err != nil && ctx.Err() == nil {
				a.d.Logger.WarnContext(ctx, "automatic retry failed", slog.Any("error", err))
			} else if n > 0 {
				a.d.Logger.InfoContext(ctx, "failed tasks started again", "count", n)
			}
		}
	}
}
