package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

// RecoverInterrupted repairs tasks left in an active state by a crash or an
// unclean shutdown, so the scheduler can pick them up again:
//
//   - running and refreshing tasks go back to queued; their persisted
//     segments let the engine resume where the last checkpoint left off;
//   - postprocessing tasks are marked as a retryable error, because their
//     post-download actions may have run partially and must not be assumed
//     done.
//
// awaiting_link tasks are left alone: they wait for a user or extension
// action, which survives a restart. It returns the number of tasks changed.
func RecoverInterrupted(ctx context.Context, store storage.Store, now func() time.Time, log *slog.Logger) (int, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	filter := storage.TaskFilter{
		Statuses: []domain.TaskStatus{domain.StatusRunning, domain.StatusRefreshing, domain.StatusPostprocessing},
		Limit:    storage.MaxTaskLimit,
	}
	changed := 0
	for {
		page, err := store.Tasks().List(ctx, filter)
		if err != nil {
			return changed, fmt.Errorf("service: recover: list tasks: %w", err)
		}
		for _, t := range page.Tasks {
			if err := recoverTask(ctx, store, t, now().UTC()); err != nil {
				return changed, err
			}
			changed++
			log.InfoContext(ctx, "recovered interrupted task", "task_id", t.ID, "status", t.Status)
		}
		// Recovered tasks no longer match the filter, but the cursor is
		// keyset-based so following it stays correct either way.
		if page.NextCursor == "" {
			return changed, nil
		}
		filter.Cursor = page.NextCursor
	}
}

func recoverTask(ctx context.Context, store storage.Store, t *domain.Task, now time.Time) error {
	var err error
	if t.Status == domain.StatusPostprocessing {
		err = t.SetError(domain.TaskError{
			Code:      domain.ErrorCodeFatal,
			Message:   "interrupted during post-processing; retry to run it again",
			Retryable: true,
		}, now)
	} else {
		err = t.TransitionTo(domain.StatusQueued, now)
	}
	if err != nil {
		return fmt.Errorf("service: recover task %q: %w", t.ID, err)
	}
	if err := store.Tasks().Update(ctx, t); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("service: recover task %q: %w", t.ID, err)
	}
	return nil
}
