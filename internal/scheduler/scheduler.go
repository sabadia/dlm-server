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
	"github.com/sabadia/dlm-server/internal/events"
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

// LimitsApplier applies the effective speed limits (including time-of-day
// speed profiles) for the stored settings at now; service.LimitsController
// implements it.
type LimitsApplier interface {
	Apply(global settings.Settings, queues []*domain.Queue, now time.Time)
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
	// Clock defaults to time.Now. It decides time windows, speed profiles,
	// quota windows and post-queue action delays, and times runs for the
	// logs.
	Clock func() time.Time
	// Tick is the interval of periodic admission passes, which pick up
	// changes nobody woke the scheduler for; <= 0 means DefaultTick.
	Tick time.Duration
	// Bus receives queue.completed, queue.action_cancelled,
	// quota.exhausted and quota.reset events; nil disables them.
	Bus *events.Bus
	// Limits, when set, re-applies the speed limits on every pass so
	// speed profiles switch on time.
	Limits LimitsApplier
	// Quotas meters bytes for quotas; the runner must report to the same
	// meter (service.RunnerDeps.Bytes). Nil creates a private meter that
	// only sees bytes reported to Scheduler.Quotas().AddBytes.
	Quotas *QuotaMeter
	// Actions executes the sleep, shutdown and command post-queue actions;
	// nil makes them fail (logged).
	Actions ActionExecutor
	// StopQueue performs the "stop" post-queue action (QueueService.Stop in
	// the daemon); nil makes it fail (logged).
	StopQueue func(ctx context.Context, queueID string) error
}

// Scheduler admits queued tasks into runs. It implements service.Scheduler.
// Create it with New and drive it with Run.
type Scheduler struct {
	o    Options
	wake chan struct{} // capacity 1: pending wake-ups coalesce

	mu      sync.Mutex
	running map[string]*run // by task ID
	// completed counts, per queue, the tasks completed since the queue
	// last ran out of work (post-queue actions).
	completed map[string]int
	started   bool
	wg        sync.WaitGroup // run and post-queue action goroutines

	// pending post-queue actions by queue ID; owned by the Run goroutine.
	pending map[string]*pendingAction
}

// run is one admitted task.
type run struct {
	queueID string // queue the task was admitted from; its slot is counted there
	cancel  context.CancelCauseFunc
	done    chan struct{} // closed once Runner.Run returned and the run was removed
	// requeued is set (under Scheduler.mu) once a gate (time window,
	// quota) cancelled the run, so it is logged once.
	requeued bool
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
	if o.Quotas == nil {
		o.Quotas = NewQuotaMeter(o.Clock)
	}
	s := &Scheduler{
		o:         o,
		wake:      make(chan struct{}, 1),
		running:   make(map[string]*run),
		completed: make(map[string]int),
		pending:   make(map[string]*pendingAction),
	}
	o.Quotas.setWake(s.Wake)
	return s
}

// Quotas returns the scheduler's quota meter.
func (s *Scheduler) Quotas() *QuotaMeter { return s.o.Quotas }

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

// passState is the stored state one admission pass works on.
type passState struct {
	queues   []*domain.Queue
	global   settings.Settings
	defaults settings.Settings
}

// admit runs one admission pass (see the package documentation). Errors
// are logged; the next pass retries.
func (s *Scheduler) admit(ctx, runCtx context.Context) {
	if ctx.Err() != nil {
		return
	}
	st, err := s.load(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.o.Logger.ErrorContext(ctx, "scheduler: admission failed", "error", err)
		}
		return
	}
	now := s.o.Clock()
	if s.o.Limits != nil {
		s.o.Limits.Apply(st.global, st.queues, now)
	}
	open := s.gates(ctx, st, now)
	plan, globalFree, err := s.plan(ctx, st, open)
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
	s.queueActions(ctx, st, now)
}

// load reads the queues and the settings layers of a pass.
func (s *Scheduler) load(ctx context.Context) (*passState, error) {
	queues, err := s.o.Store.Queues().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list queues: %w", err)
	}
	global, err := s.o.Store.Settings().GetGlobal(ctx)
	if err != nil {
		return nil, fmt.Errorf("scheduler: get global settings: %w", err)
	}
	return &passState{queues: queues, global: global, defaults: s.o.Defaults()}, nil
}

// Gate reasons, logged when a gate requeues running tasks.
const (
	gateOutsideWindow = "outside_window"
	gateQueueQuota    = "queue_quota"
	gateGlobalQuota   = "global_quota"
)

// gates decides which running queues may admit tasks now: a queue is open
// when it is inside its schedule's time windows (or has no schedule) and
// neither its own quota nor the global quota is exhausted. It publishes
// quota state changes and requeues the running tasks of every running
// queue whose gate is closed. Stopped queues are not open; their tasks
// are stopped by QueueService.Stop, not here.
func (s *Scheduler) gates(ctx context.Context, st *passState, now time.Time) map[string]bool {
	globalQuota := settings.Merge(st.defaults, st.global).Quota
	globalExhausted, change := s.o.Quotas.check(globalScope, globalQuota, now)
	s.publishQuota(ctx, change)

	open := make(map[string]bool, len(st.queues))
	closed := make(map[string]string) // queue ID -> reason
	known := make(map[string]bool, len(st.queues))
	for _, q := range st.queues {
		known[q.ID] = true
	}
	s.o.Quotas.prune(known)
	for _, q := range st.queues {
		queueExhausted, change := s.o.Quotas.check(q.ID, q.Settings.Quota, now)
		s.publishQuota(ctx, change)
		if q.State != domain.QueueRunning {
			continue
		}
		reason := ""
		switch sch, ok := q.Settings.Schedule.Get(); {
		case globalExhausted:
			reason = gateGlobalQuota
		case ok && !sch.ActiveAt(now):
			reason = gateOutsideWindow
		case queueExhausted:
			reason = gateQueueQuota
		}
		if reason != "" {
			closed[q.ID] = reason
			continue
		}
		open[q.ID] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.running {
		reason, ok := closed[r.queueID]
		if !ok && globalExhausted {
			reason, ok = gateGlobalQuota, true
		}
		if !ok || r.requeued {
			continue
		}
		r.requeued = true
		s.o.Logger.InfoContext(ctx, "scheduler: requeueing task", "task_id", id, "queue_id", r.queueID, "reason", reason)
		r.cancel(service.ErrRequeued)
	}
	return open
}

// publishQuota logs and publishes a quota state change, if any.
func (s *Scheduler) publishQuota(ctx context.Context, c *quotaChange) {
	if c == nil {
		return
	}
	typ := events.TypeQuotaReset
	if c.typ == quotaExhausted {
		typ = events.TypeQuotaExhausted
	}
	s.o.Logger.InfoContext(ctx, "scheduler: quota state changed", "state", c.typ, "scope", c.data.Scope, "queue_id", c.data.QueueID, "bytes", c.data.Bytes, "used", c.data.Used)
	s.publish(typ, c.data.QueueID, c.data)
}

// plan computes every open queue's candidates and the free global slots
// (-1 when there is no global cap).
func (s *Scheduler) plan(ctx context.Context, st *passState, open map[string]bool) ([]queueSlots, int, error) {
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
	if limit := settings.Resolve(settings.Layers{Defaults: st.defaults, Global: st.global}).Settings.MaxActiveDownloadsTotal.Value(); limit > 0 {
		globalFree = max(limit-total, 0)
		if globalFree == 0 {
			return nil, 0, nil
		}
	}

	var plan []queueSlots
	for _, q := range st.queues {
		if !open[q.ID] {
			continue
		}
		limit := settings.Resolve(settings.Layers{Defaults: st.defaults, Global: st.global, Queue: q.Settings}).Settings.MaxConcurrentDownloads.Value()
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
		completedIn := ""
		if err == nil {
			completedIn = s.completedQueue(context.WithoutCancel(ctx), taskID)
		}
		s.mu.Lock()
		delete(s.running, taskID)
		if completedIn != "" {
			s.completed[completedIn]++
		}
		s.mu.Unlock()
		close(r.done)
		s.finished(ctx, taskID, err, s.o.Clock().Sub(begin))
	})
}

// completedQueue returns the queue of taskID when its run left it
// completed, else "". Errors are logged: they only affect post-queue
// actions.
func (s *Scheduler) completedQueue(ctx context.Context, taskID string) string {
	t, err := s.o.Store.Tasks().Get(ctx, taskID)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			s.o.Logger.WarnContext(ctx, "scheduler: read task after run", "task_id", taskID, "error", err)
		}
		return ""
	}
	if t.Status != domain.StatusCompleted {
		return ""
	}
	return t.QueueID
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
