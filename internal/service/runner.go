package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/retry"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/postproc"
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

// ByteCounter receives the bytes each run transfers, for byte quotas. n is
// the number of bytes the task downloaded since the previous call (always
// > 0); it is called from the run's own goroutines and must not block.
type ByteCounter interface {
	AddBytes(taskID, queueID string, n int64)
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
	// Bytes, when set, receives the bytes every run downloads, sampled
	// every ProgressInterval and once more when the download ends.
	Bytes ByteCounter
	// TaskLocks serialises read-modify-write cycles of each task with the
	// services; it must be the instance in their Deps.TaskLocks. Nil creates
	// a private instance.
	TaskLocks *TaskLocks
	// LinkExpired, when set, is called when a download failed with an
	// expired link (engine.KindLinkExpired) and the run was not cancelled.
	// It decides what happens next (RefreshService.HandleExpired): it moves
	// the task to refreshing and then to queued, awaiting_link or error, and
	// returns nil. ErrRefreshDisabled makes the Runner record the failure
	// itself; a context error is treated like any cancellation.
	LinkExpired func(ctx context.Context, taskID string, cause error) error
	// Categories, when set, sorts the downloads of tasks without a directory
	// of their own into category directories (setting autoCategorize).
	Categories CategoryMatcher
	// Post, when set, runs the post-processing steps of a finished download
	// while the task is postprocessing (verifying the checksum, …). Nil
	// completes the task straight away.
	Post PostProcessor
}

// CategoryMatcher finds the category of a file (CategoryService).
type CategoryMatcher interface {
	// Match returns the first category whose rules match the file name and
	// MIME type, or nil.
	Match(ctx context.Context, name, mimeType string) (*domain.Category, error)
}

// PostProcessor runs the steps after a download (postproc.Pipeline).
type PostProcessor interface {
	// Process works on the finished task and returns the job as the steps
	// left it (its Path says where the file is now). A *postproc.MismatchError
	// asks for the download to be repeated.
	Process(ctx context.Context, t *domain.Task, eff settings.Settings) (*postproc.Job, error)
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
	// redownloads counts, by task ID, how often a download was repeated
	// because its checksum did not match. It starts over with the daemon.
	redownloads map[string]int
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
	if d.TaskLocks == nil {
		d.TaskLocks = NewTaskLocks()
	}
	if d.Defaults == nil {
		d.Defaults = func() settings.Settings { return settings.Settings{} }
	}
	return &Runner{d: d, probes: make(map[string]*engine.Probe), redownloads: make(map[string]int)}
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
//
// The task must be queued in a running queue when Run takes it: the check
// and the queued → running transition happen under the task's lock in one
// store transaction, so a task paused, deleted, moved or whose queue was
// stopped meanwhile is never started (an error matching domain.ErrConflict
// or domain.ErrNotFound). When ctx is already cancelled at that point the
// download does not start and the task is left as the cause says (paused
// for ErrPaused, otherwise queued).
func (r *Runner) Run(ctx context.Context, taskID string) error {
	task, eff, started, err := r.start(ctx, taskID)
	if err != nil || !started {
		return err
	}
	runErr := r.download(ctx, task, eff)
	if errors.Is(runErr, errExistingFile) {
		// The file is already there (policy skip, or resume with the same
		// size): the task is done, after the post-processing steps.
		if err := r.markPresent(context.WithoutCancel(ctx), task); err != nil {
			return err
		}
		runErr = nil
	}
	return r.finish(ctx, task, eff, runErr)
}

// start re-reads the task under its lock and moves it from queued to
// running, returning it with its effective settings. started is false when
// ctx was cancelled before the download began.
func (r *Runner) start(ctx context.Context, taskID string) (task *domain.Task, eff settings.Settings, started bool, err error) {
	// The store work uses a context without cancellation so a run stopped
	// before it started still records the state its cause asks for.
	wctx := context.WithoutCancel(ctx)
	unlock, err := r.d.TaskLocks.Lock(wctx, taskID)
	if err != nil {
		return nil, settings.Settings{}, false, err
	}
	defer unlock()
	var (
		from domain.TaskStatus
		to   domain.TaskStatus // empty: leave the task queued
	)
	err = r.d.Store.WithTx(wctx, func(tx storage.Store) error {
		var err error
		if task, err = tx.Tasks().Get(wctx, taskID); err != nil {
			return fmt.Errorf("service: run: %w", err)
		}
		if task.Status != domain.StatusQueued {
			return domain.Conflict("service: run task %q: status is %s, not queued", taskID, task.Status)
		}
		queue, err := tx.Queues().Get(wctx, task.QueueID)
		if err != nil {
			return fmt.Errorf("service: run task %q: %w", taskID, err)
		}
		if queue.State != domain.QueueRunning {
			return domain.Conflict("service: run task %q: queue %q is %s", taskID, queue.ID, queue.State)
		}
		global, err := tx.Settings().GetGlobal(wctx)
		if err != nil {
			return fmt.Errorf("service: run task %q: %w", taskID, err)
		}
		eff = settings.Resolve(settings.Layers{
			Defaults: r.d.Defaults(),
			Global:   global,
			Queue:    queue.Settings,
			Task:     task.Settings,
		}).Settings
		from, to = task.Status, domain.StatusRunning
		if cause := context.Cause(ctx); cause != nil {
			if !errors.Is(cause, ErrPaused) {
				to = ""
				return nil
			}
			to = domain.StatusPaused
		}
		if err := task.TransitionTo(to, r.d.Clock()); err != nil {
			return fmt.Errorf("service: task %q: %w", taskID, err)
		}
		if err := tx.Tasks().Update(wctx, task); err != nil {
			return fmt.Errorf("service: save task %q: %w", taskID, err)
		}
		return nil
	})
	if err != nil {
		return nil, settings.Settings{}, false, err //nolint:wrapcheck // wrapped inside the transaction
	}
	if to != "" {
		r.publishStatus(task, from)
	}
	return task, eff, to == domain.StatusRunning, nil
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
		if err := r.saveResolved(ctx, task); err != nil {
			_ = probe.Close() // not handed to the engine
			return err
		}
	}

	dir := eff.DownloadDir.Value()
	switch {
	case task.Dir != "":
		dir = task.Dir
	case eff.AutoCategorize.OrElse(false) && r.d.Categories != nil:
		// The task has no directory of its own: its category (by extension or
		// MIME type, known now that it is resolved) chooses one.
		c, err := r.d.Categories.Match(ctx, task.Name, task.Source.Fingerprint.ContentType)
		switch {
		case err != nil:
			r.d.Logger.WarnContext(ctx, "cannot match a category", "task_id", task.ID, "error", err)
		case c != nil && dir != "":
			dir = c.ResolveDir(dir)
			r.d.Logger.InfoContext(ctx, "sorted into a category", "task_id", task.ID, "category", c.Name, "dir", dir)
		}
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

	if len(segs) == 0 && state == nil {
		// A download that starts from nothing meets whatever is at its
		// destination under the file-exists policy.
		if err := r.applyFileExists(ctx, task, eff); err != nil {
			_ = probe.Close()
			return err
		}
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
		Checkpointer: &storeCheckpointer{store: r.d.Store, locks: r.d.TaskLocks, taskID: task.ID, now: r.d.Clock},
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

// finish records the run outcome. runCtx is the run's own context: its
// cancellation cause says whether the task was paused or requeued. Each
// transition re-reads the task, so the counters the checkpointer persisted
// and the fields services changed during the run (queue, order, priority,
// labels, settings) are kept.
func (r *Runner) finish(runCtx context.Context, task *domain.Task, eff settings.Settings, runErr error) error {
	ctx := context.WithoutCancel(runCtx) // the outcome is recorded even when the run was stopped
	cause := context.Cause(runCtx)
	kind := engine.Classify(runErr)
	if kind == engine.KindLinkExpired && cause == nil && r.d.LinkExpired != nil {
		err := r.d.LinkExpired(runCtx, task.ID, runErr)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrRefreshDisabled):
			// Refresh is off for this task: fail it like any other error.
		case runCtx.Err() != nil:
			// Stopped while refreshing: settle like a stopped download.
			return r.settleStopped(ctx, task, context.Cause(runCtx))
		default:
			// The refresh machinery itself failed (storage, conflict): fail
			// the task with the download error rather than leave it refreshing.
			r.d.Logger.WarnContext(ctx, "link refresh failed", "task_id", task.ID, "error", err)
		}
	}
	switch {
	case runErr == nil:
		if err := r.transition(ctx, task, domain.StatusPostprocessing, nil); err != nil {
			return err
		}
		return r.postprocess(runCtx, task, eff)
	case kind == engine.KindCanceled || cause != nil:
		return r.settleStopped(ctx, task, cause)
	default:
		te := engine.TaskError(runErr, r.d.Clock())
		r.d.Logger.WarnContext(ctx, "task failed", "task_id", task.ID, "code", te.Code, "error", runErr)
		return r.transition(ctx, task, domain.StatusError, &te)
	}
}

// postprocess runs the post-processing steps of a task that is in the
// postprocessing state and records the outcome: completed, a repeated
// download (checksum mismatch), or an error.
func (r *Runner) postprocess(runCtx context.Context, task *domain.Task, eff settings.Settings) error {
	ctx := context.WithoutCancel(runCtx)
	if r.d.Post == nil {
		return r.complete(ctx, task, "", nil)
	}
	job, err := r.d.Post.Process(runCtx, task, eff)
	var mm *postproc.MismatchError
	switch {
	case err == nil:
		return r.complete(ctx, task, job.Path, job.Notes)
	case errors.As(err, &mm):
		return r.redownload(ctx, task, eff, mm)
	case runCtx.Err() != nil:
		// Stopped (paused, deleted, shutting down): a postprocessing task
		// has no stopped state, so it is left as a retryable error like after
		// a crash (RecoverInterrupted).
		te := domain.TaskError{Code: domain.ErrorCodeFatal, Message: "interrupted during post-processing; retry to run it again", Retryable: true, At: r.d.Clock()}
		return r.transition(ctx, task, domain.StatusError, &te)
	default:
		te := domain.TaskError{Code: domain.ErrorCodePostprocess, Message: err.Error(), Retryable: true, At: r.d.Clock()}
		r.d.Logger.WarnContext(ctx, "post-processing failed", "task_id", task.ID, "error", err)
		return r.transition(ctx, task, domain.StatusError, &te)
	}
}

// PostprocessLabel is the label that keeps what the post actions did (and why
// one could not) on a completed task.
const PostprocessLabel = "postprocess"

// maxNotesLength bounds the label.
const maxNotesLength = 1000

// complete marks the task completed; path, when it differs from where the
// task says its file is, becomes the task's directory and name. notes are what
// the post actions reported; they are kept in the postprocess label.
func (r *Runner) complete(ctx context.Context, task *domain.Task, path string, notes []string) error {
	r.mu.Lock()
	delete(r.redownloads, task.ID)
	r.mu.Unlock()
	return r.mutate(ctx, task, func(fresh *domain.Task) error {
		if path != "" && path != filepath.Join(fresh.Dir, fresh.Name) {
			fresh.Dir, fresh.Name = filepath.Dir(path), filepath.Base(path)
		}
		if len(notes) > 0 {
			text := strings.Join(notes, "; ")
			if len(text) > maxNotesLength {
				text = text[:maxNotesLength] + "…"
			}
			labels := maps.Clone(fresh.Labels)
			if labels == nil {
				labels = map[string]string{}
			}
			labels[PostprocessLabel] = text
			fresh.Labels = labels
		}
		if err := fresh.TransitionTo(domain.StatusCompleted, r.d.Clock()); err != nil {
			return fmt.Errorf("service: task %q: %w", fresh.ID, err)
		}
		return nil
	})
}

// redownload deals with a file whose checksum did not match: the file is
// deleted and the task starts over from nothing, until retry.maxAttempts
// downloads in a row (counted per daemon run) have failed the check, when the
// task fails with checksum_mismatch.
func (r *Runner) redownload(ctx context.Context, task *domain.Task, eff settings.Settings, mm *postproc.MismatchError) error {
	limit := max(eff.Retry.MaxAttempts.OrElse(retry.DefaultMaxAttempts), 1)
	r.mu.Lock()
	r.redownloads[task.ID]++
	n := r.redownloads[task.ID]
	if n > limit {
		delete(r.redownloads, task.ID)
	}
	r.mu.Unlock()
	// The file is wrong either way: it goes, so that a download (this one or a
	// retry later) does not stop at "destination exists".
	if err := os.Remove(mm.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		te := domain.TaskError{
			Code: domain.ErrorCodeChecksumMismatch, Retryable: true, At: r.d.Clock(),
			Message: fmt.Sprintf("%v; the file cannot be removed for a new download: %v", mm, err),
		}
		return r.transition(ctx, task, domain.StatusError, &te)
	}
	if n > limit {
		te := domain.TaskError{
			Code: domain.ErrorCodeChecksumMismatch, Retryable: true, At: r.d.Clock(),
			Message: fmt.Sprintf("%v (downloaded %d times)", mm, n),
		}
		r.d.Logger.WarnContext(ctx, "checksum mismatch; giving up", "task_id", task.ID, "downloads", n)
		return r.transition(ctx, task, domain.StatusError, &te)
	}
	r.d.Logger.WarnContext(ctx, "checksum mismatch; downloading again", "task_id", task.ID, "attempt", n, "expected", mm.Expected, "got", mm.Got)
	return r.requeueFresh(ctx, task)
}

// requeueFresh puts a postprocessing task back in the queue with its download
// progress cleared (segments, engine state, downloaded bytes).
func (r *Runner) requeueFresh(ctx context.Context, task *domain.Task) error {
	unlock, err := r.d.TaskLocks.Lock(ctx, task.ID)
	if err != nil {
		return err
	}
	defer unlock()
	var fresh *domain.Task
	err = r.d.Store.WithTx(ctx, func(tx storage.Store) error {
		var err error
		if fresh, err = tx.Tasks().Get(ctx, task.ID); err != nil {
			return fmt.Errorf("service: requeue task %q: %w", task.ID, err)
		}
		if err := fresh.TransitionTo(domain.StatusQueued, r.d.Clock()); err != nil {
			return fmt.Errorf("service: requeue task %q: %w", task.ID, err)
		}
		fresh.Downloaded = 0
		if err := tx.Segments().Replace(ctx, task.ID, nil); err != nil {
			return fmt.Errorf("service: requeue task %q: %w", task.ID, err)
		}
		if err := tx.EngineState().Delete(ctx, task.ID); err != nil {
			return fmt.Errorf("service: requeue task %q: %w", task.ID, err)
		}
		return tx.Tasks().Update(ctx, fresh) //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return fmt.Errorf("service: requeue task %q: %w", task.ID, err)
	}
	from := task.Status
	*task = *fresh
	r.publishStatus(task, from)
	return nil
}

// settleStopped leaves a stopped task paused (cause ErrPaused) or queued.
func (r *Runner) settleStopped(ctx context.Context, task *domain.Task, cause error) error {
	to := domain.StatusQueued
	if errors.Is(cause, ErrPaused) {
		to = domain.StatusPaused
	}
	return r.transition(ctx, task, to, nil)
}

// transition changes the task status under the task's lock: it re-reads
// the task in a transaction, carries over the fields the runner owns from
// task (see keepRunnerFields), applies the transition, persists it and
// publishes a task.status event. On success *task is the persisted state.
func (r *Runner) transition(ctx context.Context, task *domain.Task, to domain.TaskStatus, te *domain.TaskError) error {
	return r.mutate(ctx, task, func(fresh *domain.Task) error {
		now := r.d.Clock()
		var err error
		if to == domain.StatusError && te != nil {
			err = fresh.SetError(*te, now)
		} else {
			err = fresh.TransitionTo(to, now)
		}
		if err != nil {
			return fmt.Errorf("service: task %q: %w", fresh.ID, err)
		}
		return nil
	})
}

// saveResolved persists the resolve results applyProbe put on task.
func (r *Runner) saveResolved(ctx context.Context, task *domain.Task) error {
	return r.mutate(ctx, task, func(fresh *domain.Task) error {
		fresh.Source, fresh.Resource, fresh.Size, fresh.Name = task.Source, task.Resource, task.Size, task.Name
		fresh.UpdatedAt = r.d.Clock().UTC()
		return nil
	})
}

// mutate re-reads the task under its lock inside a transaction, carries
// over the fields the runner owns from task, applies fn and persists the
// result, which then replaces *task. A status change made by fn is
// published as a task.status event while the lock is still held, so the
// events of a task are published in order.
func (r *Runner) mutate(ctx context.Context, task *domain.Task, fn func(fresh *domain.Task) error) error {
	unlock, err := r.d.TaskLocks.Lock(ctx, task.ID)
	if err != nil {
		return err
	}
	defer unlock()
	var fresh *domain.Task
	err = r.d.Store.WithTx(ctx, func(tx storage.Store) error {
		var err error
		if fresh, err = tx.Tasks().Get(ctx, task.ID); err != nil {
			return fmt.Errorf("service: save task %q: %w", task.ID, err)
		}
		keepRunnerFields(fresh, task)
		if err := fn(fresh); err != nil {
			return err
		}
		if err := tx.Tasks().Update(ctx, fresh); err != nil {
			return fmt.Errorf("service: save task %q: %w", task.ID, err)
		}
		return nil
	})
	if err != nil {
		return err //nolint:wrapcheck // wrapped inside the transaction
	}
	from := task.Status
	*task = *fresh
	if task.Status != from {
		r.publishStatus(task, from)
	}
	return nil
}

// keepRunnerFields copies onto fresh the fields the runner decides during a
// run and may not have persisted yet: the selected engine and the download
// directory chosen from the effective settings.
func keepRunnerFields(fresh, task *domain.Task) {
	if task.Engine != "" {
		fresh.Engine = task.Engine
	}
	if fresh.Dir == "" {
		fresh.Dir = task.Dir
	}
}

// publishStatus publishes a task.status event for a change from → task.Status.
func (r *Runner) publishStatus(task *domain.Task, from domain.TaskStatus) {
	if r.d.Bus == nil {
		return
	}
	r.d.Bus.Publish(events.Event{
		Type:    events.TypeTaskStatus,
		TaskID:  task.ID,
		QueueID: task.QueueID,
		Data:    StatusData{From: from, To: task.Status, Error: task.Error},
	})
}

// publishProgress publishes task.progress events and reports downloaded
// bytes to the ByteCounter until the returned stop function is called,
// which also reports the bytes downloaded since the last sample. Speed is
// an exponentially smoothed byte rate.
func (r *Runner) publishProgress(task *domain.Task, dl engine.Download) (stop func()) {
	if r.d.Bus == nil && r.d.Bytes == nil {
		return func() {}
	}
	// The baseline is the progress restored from checkpoints, so resumed
	// bytes are not counted again.
	counter := byteDelta{last: dl.Progress().Downloaded}
	count := func(p engine.Progress) {
		if r.d.Bytes == nil {
			return
		}
		if n := counter.observe(p.Downloaded); n > 0 {
			r.d.Bytes.AddBytes(task.ID, task.QueueID, n)
		}
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
				count(p)
				if r.d.Bus == nil {
					continue
				}
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
		count(dl.Progress()) // the sampler has exited; counter is ours now
	}
}

// byteDelta turns a download's absolute progress into byte deltas. A
// progress that goes backwards (the engine restarted the file from
// scratch) moves the baseline down without counting, so the bytes fetched
// again afterwards are counted as the new transfer they are.
type byteDelta struct {
	last int64
}

// observe returns the bytes downloaded since the previous observation.
func (b *byteDelta) observe(downloaded int64) int64 {
	n := downloaded - b.last
	b.last = downloaded
	return max(n, 0)
}
