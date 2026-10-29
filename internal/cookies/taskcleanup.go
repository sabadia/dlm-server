package cookies

import (
	"context"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
)

// RunTaskCleanup removes a task's cookies (see ImportForTask) when the task
// completes or is deleted, until ctx ends. A task that fails keeps them, so
// restarting it still works; TaskCookieTTL bounds how long. It must be called
// once.
func (s *Store) RunTaskCleanup(ctx context.Context, bus *events.Bus) error {
	sub := bus.Subscribe(ctx, events.Filter{Types: []events.Type{events.TypeTaskStatus, events.TypeTaskDeleted}})
	defer sub.Unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-sub.C():
			if !ok {
				return nil
			}
			if e.TaskID == "" || !endsTask(e) {
				continue
			}
			if n, err := s.DeleteTask(ctx, e.TaskID); err != nil {
				s.log.Warn("cookies: cannot remove a finished task's cookies", "task_id", e.TaskID, "error", err)
			} else if n > 0 {
				s.log.Debug("cookies: removed a finished task's cookies", "task_id", e.TaskID, "count", n)
			}
		}
	}
}

// endsTask reports whether e says the task is done or gone.
func endsTask(e events.Event) bool {
	switch e.Type {
	case events.TypeTaskDeleted:
		return true
	case events.TypeTaskStatus:
		d, ok := e.Data.(events.StatusData)
		return ok && d.To == domain.StatusCompleted
	default:
		return false
	}
}
