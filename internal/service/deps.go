package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
)

// Deps are the dependencies shared by SettingsService, QueueService and
// TaskService. Store is required (and Engines for TaskService); every
// other field is optional and defaults as documented.
type Deps struct {
	Store   storage.Store
	Engines *engine.Registry
	// Resolves keeps resolve results between TaskService.Resolve and
	// TaskService.Create; nil creates a private cache with default limits
	// (whose expired entries are then only swept on access).
	Resolves *engine.ResolveCache
	// Scheduler defaults to NopScheduler.
	Scheduler Scheduler
	// Bus receives entity events; nil disables them.
	Bus *events.Bus
	// Limits receives speed-limit changes; nil disables them.
	Limits *ratelimit.Manager
	// Runner receives resolve probes from TaskService.Create so the first
	// run reuses them; nil closes the probe after its data was copied.
	Runner *Runner
	// Defaults returns the built-in lowest settings layer; nil means empty.
	Defaults func() settings.Settings
	// Clock defaults to time.Now.
	Clock func() time.Time
	// NewID generates entity IDs; defaults to domain.NewID.
	NewID func() string
	// Logger defaults to a discarding logger.
	Logger *slog.Logger
}

func (d Deps) withDefaults() Deps {
	if d.Scheduler == nil {
		d.Scheduler = NopScheduler{}
	}
	if d.Defaults == nil {
		d.Defaults = func() settings.Settings { return settings.Settings{} }
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.NewID == nil {
		d.NewID = domain.NewID
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Resolves == nil {
		d.Resolves = engine.NewResolveCache(engine.CacheOptions{Clock: d.Clock})
	}
	return d
}

func (d Deps) now() time.Time { return d.Clock().UTC() }

// publish sends an event when a bus is configured.
func (d Deps) publish(typ events.Type, taskID, queueID string, data any) {
	if d.Bus == nil {
		return
	}
	d.Bus.Publish(events.Event{Type: typ, TaskID: taskID, QueueID: queueID, Data: data})
}

func (d Deps) publishTask(typ events.Type, t *domain.Task) {
	d.publish(typ, t.ID, t.QueueID, t)
}

func (d Deps) publishStatus(t *domain.Task, from domain.TaskStatus) {
	d.publish(events.TypeTaskStatus, t.ID, t.QueueID, StatusData{From: from, To: t.Status, Error: t.Error})
}

func (d Deps) publishQueue(typ events.Type, q *domain.Queue) {
	d.publish(typ, "", q.ID, q)
}

// BatchResult is the outcome of one item of a bulk operation. Exactly one
// of Task and Err is set, except for Delete, which also returns the deleted
// task's last state when it succeeds.
type BatchResult struct {
	// ID is the requested task ID (empty for CreateBatch).
	ID   string
	Task *domain.Task
	Err  error
}

// patchSettings applies a JSON merge patch, translating patch errors into
// domain.ErrInvalidArgument.
func patchSettings(base settings.Settings, patch []byte) (settings.Settings, error) {
	s, err := settings.MergePatch(base, patch)
	if err != nil {
		if errors.Is(err, settings.ErrInvalidPatch) {
			return settings.Settings{}, domain.InvalidArgument("service: settings patch: %w", err)
		}
		return settings.Settings{}, fmt.Errorf("service: settings patch: %w", err)
	}
	return s, nil
}

// validateLevel validates s at level, wrapping failures so they match
// domain.ErrInvalidArgument and unwrap to *settings.ValidationError.
func validateLevel(s settings.Settings, level settings.Level) error {
	if err := s.Validate(level); err != nil {
		return domain.InvalidArgument("service: %w", err)
	}
	return nil
}

// queueTasks returns every task of a queue ordered by position, then
// creation time (priority is ignored: positions are what reordering edits).
func queueTasks(ctx context.Context, store storage.Store, queueID string) ([]*domain.Task, error) {
	filter := storage.TaskFilter{QueueID: queueID, Limit: storage.MaxTaskLimit}
	var out []*domain.Task
	for {
		page, err := store.Tasks().List(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("service: list tasks of queue %q: %w", queueID, err)
		}
		out = append(out, page.Tasks...)
		if page.NextCursor == "" {
			break
		}
		filter.Cursor = page.NextCursor
	}
	slices.SortStableFunc(out, func(a, b *domain.Task) int {
		return cmp.Or(cmp.Compare(a.Position, b.Position), a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// activeTasks returns the tasks of a queue that a runner may hold.
func activeTasks(ctx context.Context, store storage.Store, queueID string) ([]*domain.Task, error) {
	filter := storage.TaskFilter{
		QueueID:  queueID,
		Statuses: []domain.TaskStatus{domain.StatusRunning, domain.StatusRefreshing},
		Limit:    storage.MaxTaskLimit,
	}
	var out []*domain.Task
	for {
		page, err := store.Tasks().List(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("service: list active tasks of queue %q: %w", queueID, err)
		}
		out = append(out, page.Tasks...)
		if page.NextCursor == "" {
			return out, nil
		}
		filter.Cursor = page.NextCursor
	}
}

// stoppable reports whether a task in status s is held by a runner that
// Scheduler.Stop can interrupt.
func stoppable(s domain.TaskStatus) bool {
	return s == domain.StatusRunning || s == domain.StatusRefreshing
}

// stopTask stops a running or refreshing task through the scheduler with
// cause (ErrPaused or ErrRequeued) and returns its persisted state. When the
// scheduler reports the task as not running although the store still says
// it is (a stale state, e.g. no scheduler is active), the task is moved to
// the cause's target state directly so the request still takes effect.
func stopTask(ctx context.Context, d Deps, id string, cause error) (*domain.Task, error) {
	if err := d.Scheduler.Stop(ctx, id, cause); err != nil {
		return nil, fmt.Errorf("service: stop task %q: %w", id, err)
	}
	t, err := d.Store.Tasks().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("service: stop task %q: %w", id, err)
	}
	if !stoppable(t.Status) {
		return t, nil
	}
	to := domain.StatusQueued
	if errors.Is(cause, ErrPaused) {
		to = domain.StatusPaused
	}
	from := t.Status
	if err := t.TransitionTo(to, d.now()); err != nil {
		return nil, fmt.Errorf("service: stop task %q: %w", id, err)
	}
	if err := d.Store.Tasks().Update(ctx, t); err != nil {
		return nil, fmt.Errorf("service: stop task %q: %w", id, err)
	}
	d.Logger.InfoContext(ctx, "stopped task without an active run", "task_id", id, "from", from, "to", to)
	d.publishStatus(t, from)
	return t, nil
}
