package service

import (
	"context"

	"github.com/sabadia/dlm-server/internal/domain"
)

// MoveMany moves each task to the end of another queue. The queue is checked
// once; every task then has its own outcome.
func (s *TaskService) MoveMany(ctx context.Context, ids []string, queueID string) []BatchResult {
	return s.each(ids, func(id string) (*domain.Task, error) { return s.Move(ctx, id, queueID) })
}

// UpdateMany applies the same change to each task (see Update); a task that
// cannot take it reports its own error. When queueID is set the task also
// moves there, after the update.
func (s *TaskService) UpdateMany(ctx context.Context, ids []string, in UpdateTaskInput, queueID string) []BatchResult {
	return s.each(ids, func(id string) (*domain.Task, error) {
		t, err := s.Update(ctx, id, in)
		if err != nil {
			return nil, err
		}
		if queueID != "" && t.QueueID != queueID {
			return s.Move(ctx, id, queueID)
		}
		return t, nil
	})
}

// RestartMany restarts each task (see Restart).
func (s *TaskService) RestartMany(ctx context.Context, ids []string) []BatchResult {
	return s.each(ids, func(id string) (*domain.Task, error) { return s.Restart(ctx, id) })
}
