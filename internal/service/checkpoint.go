package service

import (
	"context"
	"fmt"
	"time"

	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/storage"
)

// storeCheckpointer persists engine checkpoints for one task. Engines
// already throttle checkpoints (about once per second plus a final one), so
// every call is written; all writes of one checkpoint share a transaction.
// A checkpoint that changes the task's source rewrites the task row, so it
// is a read-modify-write and runs under the task's lock; a counters-only
// checkpoint writes just the counters and needs no lock.
type storeCheckpointer struct {
	store  storage.Store
	locks  *TaskLocks
	taskID string
	now    func() time.Time
}

func (c *storeCheckpointer) Checkpoint(ctx context.Context, cp engine.Checkpoint) error {
	if cp.Source != nil {
		unlock, err := c.locks.Lock(ctx, c.taskID)
		if err != nil {
			return fmt.Errorf("service: checkpoint task %q: %w", c.taskID, err)
		}
		defer unlock()
	}
	err := c.store.WithTx(ctx, func(tx storage.Store) error {
		if err := tx.Segments().Replace(ctx, c.taskID, cp.Segments); err != nil {
			return err //nolint:wrapcheck // wrapped once below
		}
		if cp.EngineState != nil {
			if err := tx.EngineState().Put(ctx, c.taskID, cp.EngineState); err != nil {
				return err //nolint:wrapcheck // wrapped once below
			}
		}
		if cp.Source == nil {
			return tx.Tasks().UpdateProgress(ctx, c.taskID, cp.Downloaded, cp.Size, c.now()) //nolint:wrapcheck // wrapped once below
		}
		t, err := tx.Tasks().Get(ctx, c.taskID)
		if err != nil {
			return err //nolint:wrapcheck // wrapped once below
		}
		t.Source = *cp.Source
		t.Downloaded, t.Size, t.UpdatedAt = cp.Downloaded, cp.Size, c.now().UTC()
		return tx.Tasks().Update(ctx, t) //nolint:wrapcheck // wrapped once below
	})
	if err != nil {
		return fmt.Errorf("service: checkpoint task %q: %w", c.taskID, err)
	}
	return nil
}
