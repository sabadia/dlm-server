package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
)

// DefaultTick is the default interval of periodic admission passes.
const DefaultTick = 2 * time.Second

// ErrAlreadyRunning is returned by Run when the scheduler is already
// running (or has run).
var ErrAlreadyRunning = errors.New("scheduler: already running")

// TaskRunner executes one task until it ends or ctx is cancelled (see
// service.Runner.Run, which satisfies it).
type TaskRunner interface {
	Run(ctx context.Context, taskID string) error
}

// Options configures New. Store and Runner are required.
type Options struct {
	Store  storage.Store
	Runner TaskRunner
	// Defaults returns the built-in lowest settings layer; nil means empty
	// (no per-queue limit is then set, so queues admit nothing unless the
	// global or queue layer sets maxConcurrentDownloads).
	Defaults func() settings.Settings
	// Logger defaults to a discarding logger.
	Logger *slog.Logger
	// Clock defaults to time.Now; it times runs for the logs.
	Clock func() time.Time
	// Tick is the interval of periodic admission passes, which pick up
	// changes nobody woke the scheduler for; <= 0 means DefaultTick.
	Tick time.Duration
}

// Scheduler admits queued tasks into runs. It implements service.Scheduler.
// Create it with New and drive it with Run.
type Scheduler struct {
	o    Options
	wake chan struct{} // capacity 1: pending wake-ups coalesce

	mu      sync.Mutex
	running map[string]*run // by task ID
	started bool
	wg      sync.WaitGroup // run goroutines
}

// run is one admitted task.
type run struct {
	queueID string // queue the task was admitted from; its slot is counted there
	cancel  context.CancelCauseFunc
	done    chan struct{} // closed once Runner.Run returned and the run was removed
}

var _ service.Scheduler = (*Scheduler)(nil)

// New creates a Scheduler.
func New(o Options) *Scheduler {
	if o.Defaults == nil {
		o.Defaults = func() settings.Settings { return settings.Settings{} }
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Tick <= 0 {
		o.Tick = DefaultTick
	}
	return &Scheduler{
		o:       o,
		wake:    make(chan struct{}, 1),
		running: make(map[string]*run),
	}
}

// Wake asks for an admission pass soon. It never blocks; wake-ups that
// arrive while one is pending are merged into it.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Stop cancels the run of taskID with cause and waits until Runner.Run
// returned (the runner has persisted the final state) or ctx is done (then
// it returns an error wrapping ctx.Err()). It returns nil at once when the
// task is not running.
func (s *Scheduler) Stop(ctx context.Context, taskID string, cause error) error {
	s.mu.Lock()
	r := s.running[taskID]
	s.mu.Unlock()
	if r == nil {
		return nil
	}
	r.cancel(cause)
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("scheduler: stop task %q: %w", taskID, ctx.Err())
	}
}

// Running returns the IDs of the tasks currently admitted, sorted.
func (s *Scheduler) Running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.running))
	for id := range s.running {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Run admits tasks until ctx is cancelled. It then stops every running task
// with service.ErrRequeued, waits for all runs to end and returns nil. All
// goroutines it starts have ended when it returns. A Scheduler runs once;
// a second call returns ErrAlreadyRunning.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	s.started = true
	s.mu.Unlock()

	// Runs outlive ctx's cancellation until they are stopped explicitly
	// with ErrRequeued, so they derive from a context without it.
	runCtx := context.WithoutCancel(ctx)
	ticker := time.NewTicker(s.o.Tick)
	defer ticker.Stop()
	s.admit(ctx, runCtx)
	for {
		select {
		case <-ctx.Done():
			s.shutdown()
			return nil
		case <-s.wake:
			s.admit(ctx, runCtx)
		case <-ticker.C:
			s.admit(ctx, runCtx)
		}
	}
}

// shutdown stops every run with ErrRequeued and waits for all of them.
func (s *Scheduler) shutdown() {
	s.mu.Lock()
	n := len(s.running)
	for _, r := range s.running {
		r.cancel(service.ErrRequeued)
	}
	s.mu.Unlock()
	s.o.Logger.Info("scheduler stopping", "running_tasks", n)
	s.wg.Wait()
}

// queueSlots are one queue's admission candidates of a pass.
type queueSlots struct {
	queue      *domain.Queue
	candidates []string // queued task IDs in queue order, at most the free slots
}

// admit runs one admission pass (see the package documentation). Errors
// are logged; the next pass retries.
func (s *Scheduler) admit(ctx, runCtx context.Context) {
	if ctx.Err() != nil {
		return
	}
	plan, globalFree, err := s.plan(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.o.Logger.ErrorContext(ctx, "scheduler: admission failed", "error", err)
		}
		return
	}
	for _, qs := range distribute(plan, globalFree) {
		for _, id := range qs.candidates {
			s.start(runCtx, qs.queue.ID, id)
		}
	}
}

// plan computes every running queue's candidates and the free global slots
// (-1 when there is no global cap).
func (s *Scheduler) plan(ctx context.Context) ([]queueSlots, int, error) {
	queues, err := s.o.Store.Queues().List(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("scheduler: list queues: %w", err)
	}
	global, err := s.o.Store.Settings().GetGlobal(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("scheduler: get global settings: %w", err)
	}
	defaults := s.o.Defaults()

	s.mu.Lock()
	perQueue := make(map[string]int)
	admitted := make(map[string]bool, len(s.running))
	for id, r := range s.running {
		perQueue[r.queueID]++
		admitted[id] = true
	}
	total := len(s.running)
	s.mu.Unlock()

	globalFree := -1
	if limit := settings.Resolve(settings.Layers{Defaults: defaults, Global: global}).Settings.MaxActiveDownloadsTotal.Value(); limit > 0 {
		globalFree = max(limit-total, 0)
		if globalFree == 0 {
			return nil, 0, nil
		}
	}

	var plan []queueSlots
	for _, q := range queues {
		if q.State != domain.QueueRunning {
			continue
		}
		limit := settings.Resolve(settings.Layers{Defaults: defaults, Global: global, Queue: q.Settings}).Settings.MaxConcurrentDownloads.Value()
		free := limit - perQueue[q.ID]
		if globalFree >= 0 {
			free = min(free, globalFree)
		}
		if free <= 0 {
			continue
		}
		ids, err := s.queued(ctx, q.ID, free, admitted)
		if err != nil {
			return nil, 0, err
		}
		if len(ids) > 0 {
			plan = append(plan, queueSlots{queue: q, candidates: ids})
		}
	}
	return plan, globalFree, nil
}

// queued returns up to n queued tasks of a queue in queue order, skipping
// tasks already admitted whose runner has not marked them running yet.
func (s *Scheduler) queued(ctx context.Context, queueID string, n int, admitted map[string]bool) ([]string, error) {
	filter := storage.TaskFilter{
		QueueID:  queueID,
		Statuses: []domain.TaskStatus{domain.StatusQueued},
		Sort:     storage.TaskSortPosition,
		Limit:    min(n+len(admitted), storage.MaxTaskLimit),
	}
	var ids []string
	for {
		page, err := s.o.Store.Tasks().List(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("scheduler: list queued tasks of queue %q: %w", queueID, err)
		}
		for _, t := range page.Tasks {
			if admitted[t.ID] {
				continue
			}
			ids = append(ids, t.ID)
			if len(ids) == n {
				return ids, nil
			}
		}
		if page.NextCursor == "" {
			return ids, nil
		}
		filter.Cursor = page.NextCursor
	}
}

// distribute limits the candidates to globalFree tasks in total (no limit
// when it is negative), taking one task per queue per round in queue
// order. The result keeps each queue's own order.
func distribute(plan []queueSlots, globalFree int) []queueSlots {
	if globalFree < 0 {
		return plan
	}
	take := make([]int, len(plan))
	for round := 0; globalFree > 0; round++ {
		progressed := false
		for i, qs := range plan {
			if globalFree == 0 {
				break
			}
			if round < len(qs.candidates) {
				take[i]++
				globalFree--
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	out := make([]queueSlots, 0, len(plan))
	for i, qs := range plan {
		if take[i] > 0 {
			out = append(out, queueSlots{queue: qs.queue, candidates: qs.candidates[:take[i]]})
		}
	}
	return out
}

// start records the run of taskID and executes it in an owned goroutine.
func (s *Scheduler) start(runCtx context.Context, queueID, taskID string) {
	ctx, cancel := context.WithCancelCause(runCtx)
	r := &run{queueID: queueID, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.running[taskID] = r
	s.mu.Unlock()
	s.o.Logger.DebugContext(ctx, "scheduler: starting task", "task_id", taskID, "queue_id", queueID)
	s.wg.Go(func() {
		begin := s.o.Clock()
		err := s.o.Runner.Run(ctx, taskID)
		cancel(nil) // release the context's resources
		s.mu.Lock()
		delete(s.running, taskID)
		s.mu.Unlock()
		close(r.done)
		s.finished(ctx, taskID, err, s.o.Clock().Sub(begin))
	})
}

// finished logs the outcome of a run and wakes the scheduler so the freed
// slot is reused. A run that failed for another reason than a state
// conflict is not retried before the next tick, so a persistent failure
// (for example a store error) cannot turn into a busy loop.
func (s *Scheduler) finished(ctx context.Context, taskID string, err error, took time.Duration) {
	switch {
	case err == nil:
		s.o.Logger.DebugContext(ctx, "scheduler: task run ended", "task_id", taskID, "duration", took)
	case errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound):
		// The task changed state (paused, deleted, moved, queue stopped)
		// between admission and start; nothing ran.
		s.o.Logger.DebugContext(ctx, "scheduler: task not started", "task_id", taskID, "reason", err)
	default:
		s.o.Logger.ErrorContext(ctx, "scheduler: task run failed", "task_id", taskID, "error", err, "duration", took)
		return
	}
	s.Wake()
}
