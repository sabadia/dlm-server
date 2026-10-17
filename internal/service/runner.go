package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
)

// Stop causes for a running task, passed with context.CancelCauseFunc.
// They decide the state the task is left in when its run is cancelled.
var (
	// ErrPaused leaves the task paused (user action).
	ErrPaused = errors.New("service: task paused")
	// ErrRequeued leaves the task queued (scheduler preemption, shutdown).
	ErrRequeued = errors.New("service: task requeued")
)

// DefaultProgressInterval is how often a running task publishes progress.
const DefaultProgressInterval = 500 * time.Millisecond

// ProgressData is the payload of task.progress events.
type ProgressData struct {
	Downloaded  int64 `json:"downloaded"`
	Size        int64 `json:"size"`
	Speed       int64 `json:"speed"` // bytes per second, smoothed
	Connections int   `json:"connections"`
}

// StatusData is the payload of task.status events.
type StatusData struct {
	From  domain.TaskStatus `json:"from"`
	To    domain.TaskStatus `json:"to"`
	Error *domain.TaskError `json:"error,omitempty"`
}

// RunnerDeps are the dependencies of a Runner.
type RunnerDeps struct {
	Store   storage.Store
	Engines *engine.Registry
	Limits  *ratelimit.Manager
	Bus     *events.Bus
	// Defaults returns the built-in lowest settings layer.
	Defaults         func() settings.Settings
	Logger           *slog.Logger
	Clock            func() time.Time
	ProgressInterval time.Duration
}

// Runner executes one task end to end: it resolves the effective settings,
// probes the source when needed, runs the engine download with the task's
// rate limiter and a storage-backed checkpointer, publishes status and
// progress events, and records the outcome on the task. Deciding when a
// task runs (queues, slots, schedules) is the scheduler's job.
type Runner struct {
	d RunnerDeps

	mu     sync.Mutex
	probes map[string]*engine.Probe // attached resolve probes by task ID
}

// NewRunner creates a Runner.
func NewRunner(d RunnerDeps) *Runner {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.ProgressInterval <= 0 {
		d.ProgressInterval = DefaultProgressInterval
	}
	if d.Defaults == nil {
		d.Defaults = func() settings.Settings { return settings.Settings{} }
	}
	return &Runner{d: d, probes: make(map[string]*engine.Probe)}
}

// AttachProbe hands a fresh resolve probe to the next Run of taskID, which
// uses it instead of probing again (so one-time URLs keep working between
// resolve and create) and clears it. The Runner takes ownership of p; a
// probe already attached to the task is closed.
func (r *Runner) AttachProbe(taskID string, p *engine.Probe) {
	if p == nil {
		return
	}
	r.mu.Lock()
	prev := r.probes[taskID]
	r.probes[taskID] = p
	r.mu.Unlock()
	if prev != nil && prev != p {
		_ = prev.Close() // replaced probe is no longer reachable
	}
}

// ReleaseProbe closes and forgets the probe attached to taskID, if any
// (used when the task is deleted before it ran).
func (r *Runner) ReleaseProbe(taskID string) {
	_ = r.takeProbe(taskID).Close() // Close is nil-safe; nothing to report for an abandoned probe
}

// takeProbe removes and returns the probe attached to taskID, or nil.
func (r *Runner) takeProbe(taskID string) *engine.Probe {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.probes[taskID]
	delete(r.probes, taskID)
	return p
}

// Run executes the task until it completes, fails, or ctx is cancelled.
// When cancelled with cause ErrPaused the task is left paused; any other
// cancellation (including ErrRequeued) leaves it queued so it resumes from
// its last checkpoint. Download failures are recorded on the task and are
// not returned; Run returns an error only when the task cannot be run or
// its state cannot be persisted.
func (r *Runner) Run(ctx context.Context, taskID string) error {
	task, eff, err := r.load(ctx, taskID)
	if err != nil {
		return err
	}
	if err := r.transition(ctx, task, domain.StatusRunning, nil); err != nil {
		return err
	}

	runErr := r.download(ctx, task, eff)
	return r.finish(context.WithoutCancel(ctx), task, runErr, context.Cause(ctx))
}

// load reads the task and computes its effective settings.
func (r *Runner) load(ctx context.Context, taskID string) (*domain.Task, settings.Settings, error) {
	task, err := r.d.Store.Tasks().Get(ctx, taskID)
	if err != nil {
		return nil, settings.Settings{}, fmt.Errorf("service: run: %w", err)
	}
	queue, err := r.d.Store.Queues().Get(ctx, task.QueueID)
	if err != nil {
		return nil, settings.Settings{}, fmt.Errorf("service: run task %q: %w", taskID, err)
	}
	global, err := r.d.Store.Settings().GetGlobal(ctx)
	if err != nil {
		return nil, settings.Settings{}, fmt.Errorf("service: run task %q: %w", taskID, err)
	}
	eff := settings.Resolve(settings.Layers{
		Defaults: r.d.Defaults(),
		Global:   global,
		Queue:    queue.Settings,
		Task:     task.Settings,
	})
	return task, eff.Settings, nil
}

// download prepares and runs the engine download. Its error is the
// download outcome (nil on success).
func (r *Runner) download(ctx context.Context, task *domain.Task, eff settings.Settings) error {
	// A probe attached by TaskService.Create (resolve → create) is used
	// instead of probing again.
	probe := r.takeProbe(task.ID)
	eng, err := r.engineFor(task)
	if err != nil {
		_ = probe.Close()
		return err
	}
	if task.Resource == nil || task.Name == "" {
		if probe == nil {
			probe, err = eng.Resolve(ctx, engine.Request{Source: task.Source, Settings: eff})
			if err != nil {
				return err //nolint:wrapcheck // engine errors are classified; kept intact
			}
		}
		applyProbe(task, probe)
		if err := r.d.Store.Tasks().Update(ctx, task); err != nil {
			_ = probe.Close() // not handed to the engine
			return fmt.Errorf("service: save resolved task %q: %w", task.ID, err)
		}
	}

	dir := eff.DownloadDir.Value()
	if task.Dir != "" {
		dir = task.Dir
	}
	if dir == "" || !filepath.IsAbs(dir) {
		_ = probe.Close()
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("service: no absolute download directory for task %q", task.ID))
	}
	if task.Dir == "" {
		task.Dir = dir
	}

	segs, err := r.d.Store.Segments().List(ctx, task.ID)
	if err != nil {
		_ = probe.Close()
		return fmt.Errorf("service: load segments of %q: %w", task.ID, err)
	}
	state, err := r.d.Store.EngineState().Get(ctx, task.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		_ = probe.Close()
		return fmt.Errorf("service: load engine state of %q: %w", task.ID, err)
	}

	var limiter engine.Limiter
	if r.d.Limits != nil {
		r.d.Limits.SetTask(task.ID, task.Settings.SpeedLimit.Value())
		limiter = r.d.Limits.ForTask(task.ID, task.QueueID)
		defer r.d.Limits.RemoveTask(task.ID)
	}

	dl, err := eng.NewDownload(engine.Spec{
		TaskID:       task.ID,
		Source:       task.Source,
		Resource:     task.Resource,
		Path:         filepath.Join(task.Dir, task.Name),
		Settings:     eff,
		Segments:     segs,
		EngineState:  state,
		Probe:        probe,
		Limiter:      limiter,
		Checkpointer: &storeCheckpointer{store: r.d.Store, taskID: task.ID, now: r.d.Clock},
	})
	if err != nil {
		return err //nolint:wrapcheck // engine errors are classified; kept intact
	}

	stopProgress := r.publishProgress(task, dl)
	defer stopProgress()
	return dl.Run(ctx) //nolint:wrapcheck // engine errors are classified; kept intact
}

// engineFor returns the task's engine, selecting and recording one for a
// new task.
func (r *Runner) engineFor(task *domain.Task) (engine.Engine, error) {
	if task.Engine != "" {
		e, err := r.d.Engines.Get(task.Engine)
		if err != nil {
			return nil, engine.Fatal(domain.ErrorCodeFatal, err)
		}
		return e, nil
	}
	e, err := r.d.Engines.Select(task.Source)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	task.Engine = e.Name()
	return e, nil
}

// applyProbe copies resolve results onto a task that has not been resolved.
func applyProbe(task *domain.Task, p *engine.Probe) {
	task.Engine = p.Engine
	task.Source = p.Source
	res := p.Resource
	task.Resource = &res
	task.Size = res.Size
	if task.Name == "" {
		task.Name = strings.TrimSpace(p.Suggested)
	}
	if task.Name == "" {
		task.Name = "download"
	}
}

// finish records the run outcome. cause is the context cancellation cause.
func (r *Runner) finish(ctx context.Context, task *domain.Task, runErr, cause error) error {
	// Reload the counters the checkpointer persisted during the run, and the
	// fields services may change while the task runs (queue, order,
	// priority, labels, settings) so the final write does not revert them.
	if fresh, err := r.d.Store.Tasks().Get(ctx, task.ID); err == nil {
		task.Downloaded, task.Size, task.Source = fresh.Downloaded, fresh.Size, fresh.Source
		task.QueueID, task.Position, task.Priority = fresh.QueueID, fresh.Position, fresh.Priority
		task.Labels, task.Settings = fresh.Labels, fresh.Settings
	}
	switch kind := engine.Classify(runErr); {
	case runErr == nil:
		if err := r.transition(ctx, task, domain.StatusPostprocessing, nil); err != nil {
			return err
		}
		// Post-download actions arrive in Phase 9; completion is immediate.
		return r.transition(ctx, task, domain.StatusCompleted, nil)
	case kind == engine.KindCanceled || cause != nil:
		to := domain.StatusQueued
		if errors.Is(cause, ErrPaused) {
			to = domain.StatusPaused
		}
		return r.transition(ctx, task, to, nil)
	default:
		te := engine.TaskError(runErr, r.d.Clock())
		r.d.Logger.WarnContext(ctx, "task failed", "task_id", task.ID, "code", te.Code, "error", runErr)
		return r.transition(ctx, task, domain.StatusError, &te)
	}
}

// transition changes the task status, persists the task and publishes a
// task.status event.
func (r *Runner) transition(ctx context.Context, task *domain.Task, to domain.TaskStatus, te *domain.TaskError) error {
	from := task.Status
	now := r.d.Clock()
	var err error
	if to == domain.StatusError && te != nil {
		err = task.SetError(*te, now)
	} else {
		err = task.TransitionTo(to, now)
	}
	if err != nil {
		return fmt.Errorf("service: task %q: %w", task.ID, err)
	}
	if err := r.d.Store.Tasks().Update(ctx, task); err != nil {
		return fmt.Errorf("service: save task %q: %w", task.ID, err)
	}
	if r.d.Bus != nil {
		r.d.Bus.Publish(events.Event{
			Type:    events.TypeTaskStatus,
			TaskID:  task.ID,
			QueueID: task.QueueID,
			Data:    StatusData{From: from, To: to, Error: task.Error},
		})
	}
	return nil
}

// publishProgress publishes task.progress events until the returned stop
// function is called. Speed is an exponentially smoothed byte rate.
func (r *Runner) publishProgress(task *domain.Task, dl engine.Download) (stop func()) {
	if r.d.Bus == nil {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(r.d.ProgressInterval)
		defer t.Stop()
		var (
			last    = dl.Progress().Downloaded
			lastAt  = r.d.Clock()
			smoothd float64
		)
		for {
			select {
			case <-done:
				return
			case <-t.C:
				p := dl.Progress()
				now := r.d.Clock()
				if dt := now.Sub(lastAt).Seconds(); dt > 0 {
					inst := float64(p.Downloaded-last) / dt
					smoothd = 0.3*inst + 0.7*smoothd
				}
				last, lastAt = p.Downloaded, now
				r.d.Bus.Publish(events.Event{
					Type:    events.TypeTaskProgress,
					TaskID:  task.ID,
					QueueID: task.QueueID,
					Data: ProgressData{
						Downloaded: p.Downloaded, Size: p.Size,
						Speed: int64(max(smoothd, 0)), Connections: p.Connections,
					},
				})
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}
