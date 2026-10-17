package service

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
)

// QueueService manages queues: CRUD, start/stop and ordering. The built-in
// Main queue (domain.MainQueueID) always exists and cannot be deleted.
type QueueService struct {
	d Deps
}

// NewQueueService creates a QueueService.
func NewQueueService(d Deps) *QueueService {
	return &QueueService{d: d.withDefaults()}
}

// CreateQueueInput are the fields of a new queue.
type CreateQueueInput struct {
	Name string
	// Settings are the queue-level overrides (validated at the queue level).
	Settings settings.Settings
	// State defaults to domain.QueueRunning.
	State domain.QueueState
}

// UpdateQueueInput changes a queue. Nil / empty fields are left unchanged.
// Settings and SettingsPatch are mutually exclusive.
type UpdateQueueInput struct {
	Name *string
	// Settings replaces the queue's overrides as a whole.
	Settings *settings.Settings
	// SettingsPatch is an RFC 7396 JSON merge patch applied to the queue's
	// overrides (see settings.MergePatch).
	SettingsPatch []byte
}

// DeleteQueueOptions controls QueueService.Delete.
type DeleteQueueOptions struct {
	// MoveTasksTo, when set, names the queue that receives the deleted
	// queue's tasks (appended at its end, keeping their order). Without it a
	// queue that still has tasks cannot be deleted.
	MoveTasksTo string
}

// List returns every queue ordered by position.
func (s *QueueService) List(ctx context.Context) ([]*domain.Queue, error) {
	qs, err := s.d.Store.Queues().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: list queues: %w", err)
	}
	return qs, nil
}

// Get returns a queue or an error matching domain.ErrNotFound.
func (s *QueueService) Get(ctx context.Context, id string) (*domain.Queue, error) {
	if err := resolveQueueRef(ctx, s.d.Store, &id); err != nil {
		return nil, err
	}
	q, err := s.d.Store.Queues().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("service: get queue: %w", err)
	}
	return q, nil
}

// Create adds a queue at the end of the queue order. Invalid input matches
// domain.ErrInvalidArgument; a name already used (case-insensitively)
// matches domain.ErrAlreadyExists.
func (s *QueueService) Create(ctx context.Context, in CreateQueueInput) (*domain.Queue, error) {
	q, err := domain.NewQueue(s.d.NewID(), in.Name, s.d.now(), in.Settings)
	if err != nil {
		return nil, fmt.Errorf("service: create queue: %w", err)
	}
	if in.State != "" {
		if !in.State.Valid() {
			return nil, domain.InvalidArgument("service: create queue: unknown state %q", in.State)
		}
		q.State = in.State
	}
	err = s.d.Store.WithTx(ctx, func(tx storage.Store) error {
		pos, err := tx.Queues().NextPosition(ctx)
		if err != nil {
			return fmt.Errorf("service: create queue: %w", err)
		}
		q.Position = pos
		if err := tx.Queues().Create(ctx, q); err != nil {
			return fmt.Errorf("service: create queue: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped inside the transaction
	}
	s.applyLimit(q)
	s.d.publishQueue(events.TypeQueueCreated, q)
	return q, nil
}

// Update renames a queue and/or replaces or patches its settings
// overrides (validated at the queue level). The queue speed limit is
// applied immediately and the scheduler is woken.
func (s *QueueService) Update(ctx context.Context, id string, in UpdateQueueInput) (*domain.Queue, error) {
	if err := resolveQueueRef(ctx, s.d.Store, &id); err != nil {
		return nil, err
	}
	if in.Settings != nil && in.SettingsPatch != nil {
		return nil, domain.InvalidArgument("service: update queue %q: settings and settingsPatch are mutually exclusive", id)
	}
	var q *domain.Queue
	err := s.d.Store.WithTx(ctx, func(tx storage.Store) error {
		var err error
		q, err = tx.Queues().Get(ctx, id)
		if err != nil {
			return fmt.Errorf("service: update queue: %w", err)
		}
		now := s.d.now()
		if in.Name != nil {
			if err := q.Rename(*in.Name, now); err != nil {
				return fmt.Errorf("service: update queue: %w", err)
			}
		}
		next, changed := q.Settings, false
		switch {
		case in.Settings != nil:
			next, changed = *in.Settings, true
		case in.SettingsPatch != nil:
			if next, err = patchSettings(q.Settings, in.SettingsPatch); err != nil {
				return err
			}
			changed = true
		default:
		}
		if changed {
			if err := q.SetSettings(next, now); err != nil {
				return fmt.Errorf("service: update queue: %w", err)
			}
		}
		if err := tx.Queues().Update(ctx, q); err != nil {
			return fmt.Errorf("service: update queue: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped inside the transaction
	}
	s.applyLimit(q)
	s.d.publishQueue(events.TypeQueueUpdated, q)
	s.d.Scheduler.Wake()
	return q, nil
}

// Delete removes a queue. The default queue cannot be deleted
// (domain.ErrConflict). A queue that still has tasks returns
// domain.ErrConflict unless opts.MoveTasksTo names another existing queue,
// which then receives the tasks at its end in their current order.
func (s *QueueService) Delete(ctx context.Context, id string, opts DeleteQueueOptions) error {
	if err := resolveQueueRef(ctx, s.d.Store, &opts.MoveTasksTo); err != nil {
		return err
	}
	if err := resolveQueueRef(ctx, s.d.Store, &id); err != nil {
		return err
	}
	if opts.MoveTasksTo == id {
		return domain.InvalidArgument("service: delete queue %q: cannot move its tasks to itself", id)
	}
	var (
		deleted *domain.Queue
		moved   []*domain.Task
	)
	deleteTx := func() error {
		return s.d.Store.WithTx(ctx, func(tx storage.Store) error { //nolint:wrapcheck // wrapped inside the transaction
			q, err := tx.Queues().Get(ctx, id)
			if err != nil {
				return fmt.Errorf("service: delete queue: %w", err)
			}
			if err := q.CanDelete(); err != nil {
				return fmt.Errorf("service: delete queue: %w", err)
			}
			if opts.MoveTasksTo != "" {
				if moved, err = moveAllTasks(ctx, tx, id, opts.MoveTasksTo, s.d.now()); err != nil {
					return err
				}
			}
			if err := tx.Queues().Delete(ctx, id); err != nil {
				return fmt.Errorf("service: delete queue: %w", err)
			}
			deleted = q
			return nil
		})
	}
	var err error
	if opts.MoveTasksTo == "" {
		// Without moving, a queue that still has tasks cannot be deleted, so
		// no task changes.
		err = deleteTx()
	} else {
		// Moving rewrites the queue and position of every task of the queue:
		// lock them all (in ID order); the transaction re-reads them.
		var tasks []*domain.Task
		if tasks, err = queueTasks(ctx, s.d.Store, id); err != nil {
			return err
		}
		ids := make([]string, len(tasks))
		for i, t := range tasks {
			ids[i] = t.ID
		}
		err = s.d.withTaskLocks(ctx, ids, deleteTx)
	}
	if err != nil {
		return err
	}
	if s.d.Limits != nil {
		s.d.Limits.RemoveQueue(id)
	}
	for _, t := range moved {
		s.d.publishTask(events.TypeTaskUpdated, t)
	}
	s.d.publishQueue(events.TypeQueueDeleted, deleted)
	if len(moved) > 0 {
		s.d.Scheduler.Wake()
	}
	return nil
}

// moveAllTasks appends every task of queue from to the end of queue to.
func moveAllTasks(ctx context.Context, tx storage.Store, from, to string, now time.Time) ([]*domain.Task, error) {
	if _, err := tx.Queues().Get(ctx, to); err != nil {
		return nil, fmt.Errorf("service: move tasks to queue: %w", err)
	}
	tasks, err := queueTasks(ctx, tx, from)
	if err != nil {
		return nil, err
	}
	next, err := tx.Tasks().NextPosition(ctx, to)
	if err != nil {
		return nil, fmt.Errorf("service: move tasks to queue %q: %w", to, err)
	}
	for i, t := range tasks {
		t.QueueID, t.Position, t.UpdatedAt = to, next+int64(i), now
		if err := tx.Tasks().Update(ctx, t); err != nil {
			return nil, fmt.Errorf("service: move task %q: %w", t.ID, err)
		}
	}
	return tasks, nil
}

// Start lets the scheduler admit tasks from the queue and wakes it.
func (s *QueueService) Start(ctx context.Context, id string) (*domain.Queue, error) {
	q, err := s.setState(ctx, id, domain.QueueRunning)
	if err != nil {
		return nil, err
	}
	s.d.Scheduler.Wake()
	return q, nil
}

// Stop prevents the scheduler from admitting tasks from the queue and
// stops its running tasks with ErrRequeued, so they return to queued and
// resume when the queue is started again.
func (s *QueueService) Stop(ctx context.Context, id string) (*domain.Queue, error) {
	q, err := s.setState(ctx, id, domain.QueueStopped)
	if err != nil {
		return nil, err
	}
	active, err := activeTasks(ctx, s.d.Store, id)
	if err != nil {
		return nil, err
	}
	for _, t := range active {
		if err := stopTask(ctx, s.d, t.ID, ErrRequeued); err != nil {
			return nil, err
		}
	}
	s.d.Scheduler.Wake() // freed global slots may admit other queues' tasks
	return q, nil
}

func (s *QueueService) setState(ctx context.Context, id string, state domain.QueueState) (*domain.Queue, error) {
	if err := resolveQueueRef(ctx, s.d.Store, &id); err != nil {
		return nil, err
	}
	var (
		q       *domain.Queue
		changed bool
	)
	err := s.d.Store.WithTx(ctx, func(tx storage.Store) error {
		var err error
		q, err = tx.Queues().Get(ctx, id)
		if err != nil {
			return fmt.Errorf("service: set queue state: %w", err)
		}
		if q.State == state {
			return nil
		}
		changed = true
		if state == domain.QueueRunning {
			q.Start(s.d.now())
		} else {
			q.Stop(s.d.now())
		}
		if err := tx.Queues().Update(ctx, q); err != nil {
			return fmt.Errorf("service: set queue state: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped inside the transaction
	}
	if changed {
		s.d.publishQueue(events.TypeQueueUpdated, q)
	}
	return q, nil
}

// Reorder moves a queue to the 0-based index position in the queue order
// (clamped to the last index) and renumbers all positions densely. The Main
// queue can be reordered too. A negative position matches
// domain.ErrInvalidArgument.
func (s *QueueService) Reorder(ctx context.Context, id string, position int) (*domain.Queue, error) {
	if err := resolveQueueRef(ctx, s.d.Store, &id); err != nil {
		return nil, err
	}
	if position < 0 {
		return nil, domain.InvalidArgument("service: reorder queue %q: position must be >= 0", id)
	}
	var (
		target  *domain.Queue
		changed []*domain.Queue
	)
	err := s.d.Store.WithTx(ctx, func(tx storage.Store) error {
		qs, err := tx.Queues().List(ctx)
		if err != nil {
			return fmt.Errorf("service: reorder queues: %w", err)
		}
		i := slices.IndexFunc(qs, func(q *domain.Queue) bool { return q.ID == id })
		if i < 0 {
			return domain.NotFound("queue", id)
		}
		target = qs[i]
		qs = moveTo(qs, i, position)
		now := s.d.now()
		for pos, q := range qs {
			if q.Position == int64(pos) {
				continue
			}
			q.Position, q.UpdatedAt = int64(pos), now
			if err := tx.Queues().Update(ctx, q); err != nil {
				return fmt.Errorf("service: reorder queues: %w", err)
			}
			changed = append(changed, q)
		}
		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped inside the transaction
	}
	for _, q := range changed {
		s.d.publishQueue(events.TypeQueueUpdated, q)
	}
	return target, nil
}

// applyLimit sets the queue bucket to the queue's own speed limit.
func (s *QueueService) applyLimit(q *domain.Queue) {
	if s.d.Limits != nil {
		s.d.Limits.SetQueue(q.ID, q.Settings.SpeedLimit.Value())
	}
}

// moveTo moves items[from] to index to (clamped to the last index),
// shifting the items in between, and returns the reordered slice.
func moveTo[T any](items []T, from, to int) []T {
	item := items[from]
	rest := slices.Delete(slices.Clone(items), from, from+1)
	return slices.Insert(rest, min(to, len(rest)), item)
}
