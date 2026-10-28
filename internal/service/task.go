package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
)

// TaskService implements the task use-cases: resolve, create, list, update,
// move/reorder, pause/resume/restart and delete. It changes task state only
// through domain methods, persists every change, publishes events and wakes
// the scheduler whenever admission may have changed. Running tasks are
// interrupted through Scheduler.Stop; the runner persists their final state.
type TaskService struct {
	d Deps
}

// NewTaskService creates a TaskService. Deps.Engines is required.
func NewTaskService(d Deps) *TaskService {
	return &TaskService{d: d.withDefaults()}
}

// ResolveInput is the input of TaskService.Resolve.
type ResolveInput struct {
	Source domain.Source
	// QueueID selects the queue whose settings apply; empty means Main.
	QueueID string
	// Settings are task-level overrides (validated at the task level).
	Settings settings.Settings
}

// ResolveResult describes what a source would download. ResolveID can be
// passed to TaskService.Create until ExpiresAt to create the task without
// probing again.
type ResolveResult struct {
	ResolveID string          `json:"resolveId"`
	Engine    string          `json:"engine"`
	Source    domain.Source   `json:"source"`
	Resource  domain.Resource `json:"resource"`
	Name      string          `json:"name"`
	ExpiresAt time.Time       `json:"expiresAt"`
}

// CreateTaskInput are the fields of a new task.
type CreateTaskInput struct {
	// Source is required unless ResolveID is set; with ResolveID the
	// resolved source is used and Source is ignored.
	Source domain.Source
	// ResolveID is a TaskService.Resolve result to create the task from.
	ResolveID string
	// QueueID defaults to Main.
	QueueID string
	// Name is the file name (a base name without directories); empty means
	// the suggested name.
	Name string
	// Dir is the absolute download directory; empty means the effective
	// downloadDir at run time.
	Dir      string
	Settings settings.Settings
	Priority domain.Priority
	Labels   map[string]string
	// Paused creates the task paused instead of queued.
	Paused bool
	// AllowDuplicate skips the duplicate check: without it a task for a link,
	// a checksum, or (once resolved) a file name and size that another task
	// already has is refused with domain.ErrConflict.
	AllowDuplicate bool
}

// UpdateTaskInput changes a task. Nil / empty fields are left unchanged.
type UpdateTaskInput struct {
	Name     *string
	Dir      *string
	Priority *domain.Priority
	// Labels replaces all labels (an empty map removes them).
	Labels *map[string]string
	// SettingsPatch is an RFC 7396 JSON merge patch applied to the task's
	// overrides (see settings.MergePatch).
	SettingsPatch []byte
}

// DeleteOptions controls TaskService.Delete.
type DeleteOptions struct {
	// Files also removes the task's partial (.part) and final file from its
	// download directory.
	Files bool
}

// Resolve probes a source with the effective settings (Defaults ⊕ Global ⊕
// Queue ⊕ in.Settings) and caches the probe under a resolve ID. An unknown
// queue matches domain.ErrNotFound; an invalid or unsupported source,
// invalid settings, or a failed probe match domain.ErrInvalidArgument (a
// probe failure also unwraps to the *engine.Error describing it).
func (s *TaskService) Resolve(ctx context.Context, in ResolveInput) (*ResolveResult, error) {
	if err := resolveQueueRef(ctx, s.d.Store, &in.QueueID); err != nil {
		return nil, err
	}
	queueID := cmpOr(in.QueueID, domain.MainQueueID)
	if err := in.Source.Validate(); err != nil {
		return nil, fmt.Errorf("service: resolve: %w", err)
	}
	if err := validateLevel(in.Settings, settings.LevelTask); err != nil {
		return nil, err
	}
	eff, err := effective(ctx, s.d, queueID, in.Settings)
	if err != nil {
		return nil, err
	}
	in.Source, _ = s.applyPlugins(ctx, in.Source)
	eng, err := s.d.Engines.Select(in.Source)
	if err != nil {
		return nil, fmt.Errorf("service: resolve: %w", err)
	}
	probe, err := eng.Resolve(ctx, engine.Request{Source: in.Source, Settings: eff.Settings})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("service: resolve: %w", ctxErr)
		}
		return nil, domain.InvalidArgument("service: resolve %q: %w", in.Source.CurrentURL, err)
	}
	res := &ResolveResult{
		Engine:   probe.Engine,
		Source:   probe.Source,
		Resource: probe.Resource,
		Name:     strings.TrimSpace(probe.Suggested),
	}
	res.ResolveID = s.d.Resolves.Put(probe)
	res.ExpiresAt = s.d.now().Add(s.d.Resolves.TTL())
	return res, nil
}

// Create adds a task at the end of its queue, queued (or paused when
// in.Paused) and wakes the scheduler. With in.ResolveID the cached probe is
// taken (an expired or unknown ID matches domain.ErrNotFound), its engine,
// source, resource and name are copied onto the task, and the probe is
// handed to the Runner for the first run. Otherwise the source must be
// supported by an engine. Invalid input matches domain.ErrInvalidArgument;
// an unknown queue matches domain.ErrNotFound.
func (s *TaskService) Create(ctx context.Context, in CreateTaskInput) (*domain.Task, error) {
	if err := resolveQueueRef(ctx, s.d.Store, &in.QueueID); err != nil {
		return nil, err
	}
	queueID := cmpOr(in.QueueID, domain.MainQueueID)
	if in.ResolveID == "" {
		var name string
		if in.Source, name = s.applyPlugins(ctx, in.Source); in.Name == "" {
			in.Name = name
		}
	}
	if err := validateName(in.Name); err != nil {
		return nil, err
	}
	dir, err := cleanDir(in.Dir)
	if err != nil {
		return nil, err
	}
	if _, err := s.d.Store.Queues().Get(ctx, queueID); err != nil {
		return nil, fmt.Errorf("service: create task: %w", err)
	}
	params := domain.NewTaskParams{
		QueueID: queueID, Name: in.Name, Source: in.Source,
		Settings: in.Settings, Priority: in.Priority, Labels: in.Labels,
	}
	var peeked *engine.Probe
	if in.ResolveID == "" {
		eng, err := s.d.Engines.Select(in.Source)
		if err != nil {
			return nil, fmt.Errorf("service: create task: %w", err)
		}
		params.Engine = eng.Name()
	} else {
		// Build and validate the task from a peek first, so a rejected
		// request does not consume the resolve result.
		if peeked, err = s.d.Resolves.Peek(in.ResolveID); err != nil {
			return nil, fmt.Errorf("service: create task: %w", err)
		}
		params.Engine, params.Source = peeked.Engine, peeked.Source
	}
	t, err := s.buildTask(params, peeked, dir, in.Paused)
	if err != nil {
		return nil, err
	}
	if !in.AllowDuplicate {
		if err := s.checkDuplicate(ctx, t, in.Settings); err != nil {
			return nil, err
		}
	}
	var probe *engine.Probe
	if in.ResolveID != "" {
		// Take fails when the entry expired or another request took it.
		if probe, err = s.d.Resolves.Take(in.ResolveID); err != nil {
			return nil, fmt.Errorf("service: create task: %w", err)
		}
	}
	if err := s.insertTask(ctx, t); err != nil {
		_ = probe.Close() // the task was not created; nil-safe
		return nil, err
	}
	if probe != nil {
		if s.d.Runner != nil {
			s.d.Runner.AttachProbe(t.ID, probe)
		} else {
			_ = probe.Close() // no runner to hand it to; its data is on the task
		}
	}
	s.d.publishTask(events.TypeTaskCreated, t)
	if t.Status == domain.StatusQueued {
		s.d.Scheduler.Wake()
	}
	return t, nil
}

// applyPlugins lets the plugins turn a plain link into the one to download. A
// result that is not a valid source is ignored, and so is a name that is not
// a valid file name: a plugin can improve a download, not break it.
func (s *TaskService) applyPlugins(ctx context.Context, src domain.Source) (domain.Source, string) {
	if s.d.Plugins == nil || (src.ResolverKind != "" && src.ResolverKind != domain.ResolverDirect) {
		return src, ""
	}
	next, name, ok := s.d.Plugins.Resolve(ctx, src)
	if !ok {
		return src, ""
	}
	if err := next.Validate(); err != nil {
		s.d.Logger.WarnContext(ctx, "ignoring a plugin's source", "error", err)
		return src, ""
	}
	if name = strings.TrimSpace(name); validateName(name) != nil {
		name = ""
	}
	return next, name
}

// checkDuplicate refuses a task that repeats another one: the same link, the
// same expected checksum, or (known only for a resolved task) the same file
// name and size.
func (s *TaskService) checkDuplicate(ctx context.Context, t *domain.Task, task settings.Settings) error {
	q := storage.DuplicateQuery{URLs: []string{t.Source.CurrentURL, t.Source.OriginalURL}}
	if cs, ok := task.Checksum.Get(); ok {
		q.Checksum = cs.Expected
	}
	if t.Resource != nil {
		q.Name, q.Size = t.Name, t.Size
	}
	dup, err := s.d.Store.Tasks().FindDuplicate(ctx, q)
	if err != nil {
		return fmt.Errorf("service: create task: %w", err)
	}
	if dup != nil {
		return domain.ConflictCode("task.duplicate", map[string]string{"taskId": dup.ID, "name": dup.Name, "status": string(dup.Status)},
			"service: create task: duplicate of task %s (%q, %s); allow duplicates to add it anyway", dup.ID, dup.Name, dup.Status)
	}
	return nil
}

// buildTask builds a validated task; probe, when set, supplies the
// resolved resource and suggested name.
func (s *TaskService) buildTask(p domain.NewTaskParams, probe *engine.Probe, dir string, paused bool) (*domain.Task, error) {
	now := s.d.now()
	t, err := domain.NewTask(s.d.NewID(), now, p)
	if err != nil {
		return nil, fmt.Errorf("service: create task: %w", err)
	}
	t.Dir = dir
	if probe != nil {
		res := probe.Resource
		res.Files = slices.Clone(res.Files)
		t.Resource, t.Size = &res, res.Size
		if t.Name == "" {
			t.Name = cmpOr(strings.TrimSpace(probe.Suggested), "download")
		}
		if err := validateName(t.Name); err != nil {
			return nil, err
		}
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("service: create task: %w", err)
		}
	}
	if paused {
		if err := t.TransitionTo(domain.StatusPaused, now); err != nil {
			return nil, fmt.Errorf("service: create task: %w", err)
		}
	}
	return t, nil
}

// insertTask persists a new task at the end of its queue.
func (s *TaskService) insertTask(ctx context.Context, t *domain.Task) error {
	return s.d.Store.WithTx(ctx, func(tx storage.Store) error { //nolint:wrapcheck // wrapped inside the transaction
		pos, err := tx.Tasks().NextPosition(ctx, t.QueueID)
		if err != nil {
			return fmt.Errorf("service: create task: %w", err)
		}
		t.Position = pos
		if err := tx.Tasks().Create(ctx, t); err != nil {
			return fmt.Errorf("service: create task: %w", err)
		}
		return nil
	})
}

// CreateBatch creates each task independently (one failure does not stop
// the others) and returns one result per input, in order.
func (s *TaskService) CreateBatch(ctx context.Context, in []CreateTaskInput) []BatchResult {
	out := make([]BatchResult, len(in))
	for i, item := range in {
		t, err := s.Create(ctx, item)
		out[i] = BatchResult{Task: t, Err: err}
	}
	return out
}

// Get returns a task or an error matching domain.ErrNotFound.
func (s *TaskService) Get(ctx context.Context, id string) (*domain.Task, error) {
	t, err := s.d.Store.Tasks().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("service: get task: %w", err)
	}
	return t, nil
}

// List returns one page of tasks (see storage.TaskFilter). A malformed
// cursor matches domain.ErrInvalidArgument.
func (s *TaskService) List(ctx context.Context, f storage.TaskFilter) (storage.TaskPage, error) {
	if err := resolveQueueRef(ctx, s.d.Store, &f.QueueID); err != nil {
		return storage.TaskPage{}, err
	}
	page, err := s.d.Store.Tasks().List(ctx, f)
	if err != nil {
		return storage.TaskPage{}, fmt.Errorf("service: list tasks: %w", err)
	}
	return page, nil
}

// Update changes a task's name, directory, priority, labels and/or
// settings overrides (patched, validated at the task level). Name and Dir
// cannot change (domain.ErrConflict) while the task is active or has
// partial data, because the .part file path would change. A speed-limit
// change of a running task applies immediately.
func (s *TaskService) Update(ctx context.Context, id string, in UpdateTaskInput) (*domain.Task, error) {
	if in.Name != nil {
		if err := validateName(*in.Name); err != nil {
			return nil, err
		}
	}
	var dir string
	if in.Dir != nil {
		var err error
		if dir, err = cleanDir(*in.Dir); err != nil {
			return nil, err
		}
	}
	var t *domain.Task
	err := s.d.withTaskLock(ctx, id, func() error {
		return s.update(ctx, id, in, dir, &t)
	})
	if err != nil {
		return nil, err
	}
	if s.d.Limits != nil && in.SettingsPatch != nil && stoppable(t.Status) {
		s.d.Limits.SetTask(t.ID, t.Settings.SpeedLimit.Value())
	}
	s.d.publishTask(events.TypeTaskUpdated, t)
	if in.Priority != nil {
		s.d.Scheduler.Wake()
	}
	return t, nil
}

// update applies in to task id inside a transaction; the caller holds the
// task's lock. The updated task is stored in *out.
func (s *TaskService) update(ctx context.Context, id string, in UpdateTaskInput, dir string, out **domain.Task) error {
	return s.d.Store.WithTx(ctx, func(tx storage.Store) error { //nolint:wrapcheck // wrapped inside the transaction
		t, err := tx.Tasks().Get(ctx, id)
		if err != nil {
			return fmt.Errorf("service: update task: %w", err)
		}
		*out = t
		if (in.Name != nil && *in.Name != t.Name) || (in.Dir != nil && dir != t.Dir) {
			if t.Status.Active() || (t.Downloaded > 0 && t.Status != domain.StatusCompleted) {
				return domain.ConflictCode("task.locked", map[string]string{"taskId": id},
					"service: task %q: name and dir cannot change while it is active or has partial data", id)
			}
		}
		if in.Name != nil {
			t.Name = *in.Name
		}
		if in.Dir != nil {
			t.Dir = dir
		}
		if in.Priority != nil {
			t.Priority = *in.Priority
		}
		if in.Labels != nil {
			t.Labels = maps.Clone(*in.Labels)
			if len(t.Labels) == 0 {
				t.Labels = nil
			}
		}
		if in.SettingsPatch != nil {
			if t.Settings, err = patchSettings(t.Settings, in.SettingsPatch); err != nil {
				return err
			}
		}
		if err := t.Validate(); err != nil {
			return fmt.Errorf("service: update task: %w", err)
		}
		t.UpdatedAt = s.d.now()
		if err := tx.Tasks().Update(ctx, t); err != nil {
			return fmt.Errorf("service: update task: %w", err)
		}
		return nil
	})
}

// Move appends a task at the end of another queue and wakes the scheduler.
// Moving a task to its own queue is a no-op. An unknown task or queue
// matches domain.ErrNotFound.
func (s *TaskService) Move(ctx context.Context, id, queueID string) (*domain.Task, error) {
	if err := resolveQueueRef(ctx, s.d.Store, &queueID); err != nil {
		return nil, err
	}
	var (
		t     *domain.Task
		moved bool
	)
	err := s.d.withTaskLock(ctx, id, func() error {
		return s.d.Store.WithTx(ctx, func(tx storage.Store) error { //nolint:wrapcheck // wrapped inside the transaction
			var err error
			if t, err = tx.Tasks().Get(ctx, id); err != nil {
				return fmt.Errorf("service: move task: %w", err)
			}
			if t.QueueID == queueID {
				return nil
			}
			if _, err := tx.Queues().Get(ctx, queueID); err != nil {
				return fmt.Errorf("service: move task: %w", err)
			}
			pos, err := tx.Tasks().NextPosition(ctx, queueID)
			if err != nil {
				return fmt.Errorf("service: move task: %w", err)
			}
			t.QueueID, t.Position, t.UpdatedAt = queueID, pos, s.d.now()
			if err := tx.Tasks().Update(ctx, t); err != nil {
				return fmt.Errorf("service: move task: %w", err)
			}
			moved = true
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if moved {
		s.d.publishTask(events.TypeTaskUpdated, t)
		s.d.Scheduler.Wake()
	}
	return t, nil
}

// Reorder moves a task to the 0-based index position among its queue's
// tasks ordered by position (clamped to the last index) and renumbers the
// queue's positions densely. Priority still orders admission first. A
// negative position matches domain.ErrInvalidArgument.
func (s *TaskService) Reorder(ctx context.Context, id string, position int) (*domain.Task, error) {
	if position < 0 {
		return nil, domain.InvalidArgument("service: reorder task %q: position must be >= 0", id)
	}
	// Renumbering rewrites the positions of the queue's other tasks too, so
	// all of them are locked (in ID order); the transaction re-reads them.
	t, err := s.d.Store.Tasks().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("service: reorder task: %w", err)
	}
	tasks, err := queueTasks(ctx, s.d.Store, t.QueueID)
	if err != nil {
		return nil, err
	}
	ids := []string{id}
	for _, qt := range tasks {
		ids = append(ids, qt.ID)
	}
	var (
		target  *domain.Task
		changed []*domain.Task
	)
	err = s.d.withTaskLocks(ctx, ids, func() error {
		return s.reorder(ctx, id, position, &target, &changed)
	})
	if err != nil {
		return nil, err
	}
	for _, t := range changed {
		s.d.publishTask(events.TypeTaskUpdated, t)
	}
	if len(changed) > 0 {
		s.d.Scheduler.Wake()
	}
	return target, nil
}

// reorder renumbers task id's queue inside a transaction; the caller holds
// the locks of the queue's tasks. Tasks added to the queue after they were
// locked are renumbered too: only their position changes, and the
// transaction re-reads every row, so no update is lost.
func (s *TaskService) reorder(ctx context.Context, id string, position int, target **domain.Task, changed *[]*domain.Task) error {
	return s.d.Store.WithTx(ctx, func(tx storage.Store) error { //nolint:wrapcheck // wrapped inside the transaction
		t, err := tx.Tasks().Get(ctx, id)
		if err != nil {
			return fmt.Errorf("service: reorder task: %w", err)
		}
		tasks, err := queueTasks(ctx, tx, t.QueueID)
		if err != nil {
			return err
		}
		i := indexOfTask(tasks, id)
		if i < 0 {
			return domain.NotFound("task", id) // deleted concurrently
		}
		*target = tasks[i]
		now := s.d.now()
		for pos, qt := range moveTo(tasks, i, position) {
			if qt.Position == int64(pos) {
				continue
			}
			qt.Position, qt.UpdatedAt = int64(pos), now
			if err := tx.Tasks().Update(ctx, qt); err != nil {
				return fmt.Errorf("service: reorder task %q: %w", qt.ID, err)
			}
			*changed = append(*changed, qt)
		}
		return nil
	})
}

// retryAttempts bounds how often Pause and Delete re-evaluate a task that
// was running: after stopping it the decision is taken again on its fresh
// state, and a scheduler could in principle re-admit a requeued task in
// between.
const retryAttempts = 4

// Pause pauses each task independently. Queued, errored and awaiting_link
// tasks become paused at once; running and refreshing tasks are stopped
// through Scheduler.Stop(ErrPaused) and the runner persists them paused.
// Pausing a paused task is a no-op. Postprocessing and completed tasks
// cannot be paused (domain.ErrConflict). When Pause succeeds the task is
// paused: the decision is made under the task's lock on its current state,
// so a concurrent start by the scheduler cannot be lost.
func (s *TaskService) Pause(ctx context.Context, ids []string) []BatchResult {
	return s.each(ids, func(id string) (*domain.Task, error) { return s.pause(ctx, id) })
}

func (s *TaskService) pause(ctx context.Context, id string) (*domain.Task, error) {
	for range retryAttempts {
		var (
			t       *domain.Task
			running bool
		)
		err := s.d.withTaskLock(ctx, id, func() error {
			var err error
			if t, err = s.d.Store.Tasks().Get(ctx, id); err != nil {
				return fmt.Errorf("service: pause task: %w", err)
			}
			switch {
			case t.Status == domain.StatusPaused:
				return nil
			case stoppable(t.Status):
				running = true
				return nil
			default:
				t, err = s.transitionLocked(ctx, id, domain.StatusPaused, false)
				return err
			}
		})
		if err != nil {
			return nil, err
		}
		if !running {
			return t, nil
		}
		// Stop without holding the lock: the runner needs it to persist
		// the paused state. The next iteration checks the outcome.
		if err := stopTask(ctx, s.d, id, ErrPaused); err != nil {
			return nil, err
		}
	}
	return nil, domain.ConflictCode("task.state_changed", map[string]string{"taskId": id, "operation": "pause"}, "service: pause task %q: it was restarted while being stopped", id)
}

// Resume queues each paused or errored task again (retrying an errored
// task is a resume) and wakes the scheduler. Resuming a queued or active
// task is a no-op; completed and awaiting_link tasks cannot be resumed
// (domain.ErrConflict; use Restart for a completed task).
func (s *TaskService) Resume(ctx context.Context, ids []string) []BatchResult {
	return s.each(ids, func(id string) (*domain.Task, error) {
		var t *domain.Task
		err := s.d.withTaskLock(ctx, id, func() error {
			var err error
			if t, err = s.d.Store.Tasks().Get(ctx, id); err != nil {
				return fmt.Errorf("service: resume task: %w", err)
			}
			switch t.Status {
			case domain.StatusQueued, domain.StatusRunning, domain.StatusRefreshing, domain.StatusPostprocessing:
				return nil
			case domain.StatusPaused, domain.StatusError:
				t, err = s.transitionLocked(ctx, id, domain.StatusQueued, false)
				return err
			default:
				return domain.ConflictCode("task.invalid_state", map[string]string{"taskId": id, "status": string(t.Status), "operation": "resume"}, "service: task %q: cannot resume a %s task", id, t.Status)
			}
		})
		if err != nil {
			return nil, err
		}
		return t, nil
	})
}

// Restart downloads a completed or errored task again from scratch: its
// segments, engine state and .part file are removed, Downloaded is reset
// and it is queued (the resolved resource is kept). The final file is never
// deleted: when it exists the restart fails with domain.ErrConflict. Other
// states return domain.ErrConflict.
func (s *TaskService) Restart(ctx context.Context, id string) (*domain.Task, error) {
	var out *domain.Task
	err := s.d.withTaskLock(ctx, id, func() error {
		t, err := s.d.Store.Tasks().Get(ctx, id)
		if err != nil {
			return fmt.Errorf("service: restart task: %w", err)
		}
		if t.Status != domain.StatusCompleted && t.Status != domain.StatusError {
			return domain.ConflictCode("task.invalid_state", map[string]string{"taskId": id, "status": string(t.Status), "operation": "restart"}, "service: task %q: only completed or failed tasks can be restarted, not %s", id, t.Status)
		}
		if final, part, ok := taskPaths(t); ok {
			exists, err := fileExists(final)
			if err != nil {
				return fmt.Errorf("service: restart task %q: %w", id, err)
			}
			if exists {
				return domain.ConflictCode("task.file_exists", map[string]string{"taskId": id, "path": final}, "service: task %q: file %q exists; delete or move it before restarting", id, final)
			}
			if err := removeFile(part); err != nil {
				return fmt.Errorf("service: restart task %q: %w", id, err)
			}
		}
		out, err = s.transitionLocked(ctx, id, domain.StatusQueued, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes each task independently. A running or refreshing task is
// first stopped through Scheduler.Stop(ErrRequeued). With opts.Files its
// .part and final file are removed from its download directory (a
// directory at the final path is left alone). The result carries the
// task's last state. The scheduler is woken afterwards.
func (s *TaskService) Delete(ctx context.Context, ids []string, opts DeleteOptions) []BatchResult {
	res := s.each(ids, func(id string) (*domain.Task, error) { return s.delete(ctx, id, opts) })
	s.d.Scheduler.Wake()
	return res
}

func (s *TaskService) delete(ctx context.Context, id string, opts DeleteOptions) (*domain.Task, error) {
	for range retryAttempts {
		var (
			t       *domain.Task
			running bool
		)
		err := s.d.withTaskLock(ctx, id, func() error {
			var err error
			if t, err = s.d.Store.Tasks().Get(ctx, id); err != nil {
				return fmt.Errorf("service: delete task: %w", err)
			}
			if stoppable(t.Status) {
				running = true
				return nil
			}
			if opts.Files {
				if err := s.removeFiles(ctx, t); err != nil {
					return err
				}
			}
			if err := s.d.Store.Tasks().Delete(ctx, id); err != nil {
				return fmt.Errorf("service: delete task: %w", err)
			}
			if s.d.Runner != nil {
				s.d.Runner.ReleaseProbe(id)
			}
			if s.d.Limits != nil {
				s.d.Limits.RemoveTask(id)
			}
			s.d.publishTask(events.TypeTaskDeleted, t)
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !running {
			return t, nil
		}
		// Stop without holding the lock (the runner persists the final
		// state under it); the next iteration deletes the stopped task.
		if err := stopTask(ctx, s.d, id, ErrRequeued); err != nil {
			return nil, err
		}
	}
	return nil, domain.ConflictCode("task.state_changed", map[string]string{"taskId": id, "operation": "delete"}, "service: delete task %q: it was restarted while being stopped", id)
}

// removeFiles deletes a task's .part and final file. Only plain files and
// symlinks directly inside the task's directory are removed.
func (s *TaskService) removeFiles(ctx context.Context, t *domain.Task) error {
	final, part, ok := taskPaths(t)
	if !ok {
		return nil
	}
	if err := removeFile(part); err != nil {
		return fmt.Errorf("service: delete task %q files: %w", t.ID, err)
	}
	if err := removeFile(final); err != nil {
		if errors.Is(err, errIsDir) {
			s.d.Logger.WarnContext(ctx, "not removing directory of deleted task", "task_id", t.ID, "path", final)
			return nil
		}
		return fmt.Errorf("service: delete task %q files: %w", t.ID, err)
	}
	return nil
}

// transitionLocked moves a task to status to inside a transaction that
// re-reads it, persists it and publishes a task.status event. With reset
// it also clears the task's download progress (segments, engine state,
// Downloaded). The caller holds the task's lock.
func (s *TaskService) transitionLocked(ctx context.Context, id string, to domain.TaskStatus, reset bool) (*domain.Task, error) {
	var (
		t    *domain.Task
		from domain.TaskStatus
	)
	err := s.d.Store.WithTx(ctx, func(tx storage.Store) error {
		var err error
		if t, err = tx.Tasks().Get(ctx, id); err != nil {
			return fmt.Errorf("service: task status: %w", err)
		}
		from = t.Status
		if err := t.TransitionTo(to, s.d.now()); err != nil {
			return fmt.Errorf("service: task status: %w", err)
		}
		if reset {
			if err := tx.Segments().Replace(ctx, id, nil); err != nil {
				return fmt.Errorf("service: reset task %q: %w", id, err)
			}
			if err := tx.EngineState().Delete(ctx, id); err != nil {
				return fmt.Errorf("service: reset task %q: %w", id, err)
			}
			t.Downloaded = 0
		}
		if err := tx.Tasks().Update(ctx, t); err != nil {
			return fmt.Errorf("service: task status: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped inside the transaction
	}
	s.d.publishStatus(t, from)
	if to == domain.StatusQueued {
		s.d.Scheduler.Wake()
	}
	return t, nil
}

// each runs fn for every id and collects the results in order.
func (s *TaskService) each(ids []string, fn func(id string) (*domain.Task, error)) []BatchResult {
	out := make([]BatchResult, len(ids))
	for i, id := range ids {
		t, err := fn(id)
		out[i] = BatchResult{ID: id, Task: t, Err: err}
	}
	return out
}

func indexOfTask(tasks []*domain.Task, id string) int {
	for i, t := range tasks {
		if t.ID == id {
			return i
		}
	}
	return -1
}

func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// validateName checks that a task name, when set, is a single path element
// so files are only ever created and deleted inside the task's directory.
func validateName(name string) error {
	if name == "" {
		return nil
	}
	if !isBaseName(name) {
		return domain.InvalidArgument("service: name %q must be a file name without directories", name)
	}
	return nil
}

// cleanDir validates and cleans a task directory: empty, or absolute.
func cleanDir(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", domain.InvalidArgument("service: dir %q must be an absolute path", dir)
	}
	return filepath.Clean(dir), nil
}
