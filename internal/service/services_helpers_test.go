package service

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

// stopCall records one Scheduler.Stop call.
type stopCall struct {
	TaskID string
	Cause  error
}

// fakeScheduler records Wake and Stop calls. Tasks started with run are
// executed by the real Runner; Stop cancels them with the given cause and
// waits for the runner to persist the final state, like the real scheduler.
type fakeScheduler struct {
	runner *Runner
	wakes  atomic.Int64

	mu      sync.Mutex
	stops   []stopCall
	running map[string]*runningTask
}

type runningTask struct {
	cancel context.CancelCauseFunc
	done   chan struct{} // closed when Run returned
	err    error         // Run's result, valid once done is closed
}

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{running: make(map[string]*runningTask)}
}

func (s *fakeScheduler) Wake() { s.wakes.Add(1) }

func (s *fakeScheduler) Stop(ctx context.Context, taskID string, cause error) error {
	s.mu.Lock()
	s.stops = append(s.stops, stopCall{TaskID: taskID, Cause: cause})
	rt := s.running[taskID]
	delete(s.running, taskID)
	s.mu.Unlock()
	if rt == nil {
		return nil
	}
	rt.cancel(cause)
	select {
	case <-rt.done:
		return rt.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run starts the task on the runner in the background and waits until the
// runner marked it running.
func (s *fakeScheduler) run(t *testing.T, store storage.Store, taskID string) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	rt := &runningTask{cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.running[taskID] = rt
	s.mu.Unlock()
	go func() {
		defer close(rt.done)
		rt.err = s.runner.Run(ctx, taskID)
	}()
	t.Cleanup(func() {
		cancel(ErrRequeued)
		select {
		case <-rt.done:
		case <-time.After(5 * time.Second):
		}
	})
	waitFor(t, func() bool {
		got, err := store.Tasks().Get(context.Background(), taskID)
		return err == nil && got.Status == domain.StatusRunning
	})
}

func (s *fakeScheduler) stopCalls() []stopCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stopCall(nil), s.stops...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 5s")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// manualClock is a settable clock for the resolve cache.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type svcFixture struct {
	store    storage.Store
	bus      *events.Bus
	limits   *ratelimit.Manager
	runner   *Runner
	sched    *fakeScheduler
	clock    *manualClock
	dir      string
	settings *SettingsService
	queues   *QueueService
	tasks    *TaskService
}

func newSvcFixture(t *testing.T) *svcFixture {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "dlm.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	clients := netclient.New(netclient.Options{})
	t.Cleanup(func() { _ = clients.Close() })
	reg, err := engine.NewRegistry(httpx.New(engine.Env{Clock: time.Now, Clients: clients},
		httpx.WithSlowStartInterval(20*time.Millisecond),
		httpx.WithCheckpointInterval(20*time.Millisecond),
		httpx.WithMinSplitSize(256<<10),
	))
	if err != nil {
		t.Fatal(err)
	}
	bus := events.New(events.Options{})
	t.Cleanup(bus.Close)
	dir := t.TempDir()
	defaults := settings.Defaults(dir)
	defaults.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxyNone})
	defaultsFn := func() settings.Settings { return defaults }
	limits := ratelimit.NewManager(0)
	runner := NewRunner(RunnerDeps{
		Store: store, Engines: reg, Limits: limits, Bus: bus, Defaults: defaultsFn,
		ProgressInterval: 10 * time.Millisecond,
	})
	sched := newFakeScheduler()
	sched.runner = runner
	clk := &manualClock{now: time.Now()}
	resolves := engine.NewResolveCache(engine.CacheOptions{Clock: clk.Now, TTL: time.Minute})
	t.Cleanup(resolves.Close)
	d := Deps{
		Store: store, Engines: reg, Resolves: resolves, Scheduler: sched, Bus: bus,
		Limits: limits, Runner: runner, Defaults: defaultsFn,
	}
	return &svcFixture{
		store: store, bus: bus, limits: limits, runner: runner, sched: sched, clock: clk, dir: dir,
		settings: NewSettingsService(d), queues: NewQueueService(d), tasks: NewTaskService(d),
	}
}

func (f *svcFixture) get(t *testing.T, id string) *domain.Task {
	t.Helper()
	got, err := f.store.Tasks().Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// create adds a task through the service.
func (f *svcFixture) create(t *testing.T, in CreateTaskInput) *domain.Task {
	t.Helper()
	got, err := f.tasks.Create(t.Context(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return got
}

// withStatus creates a task and drives it through the status path directly
// in the store (bypassing the services).
func (f *svcFixture) withStatus(t *testing.T, path ...domain.TaskStatus) *domain.Task {
	t.Helper()
	task := f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: "http://127.0.0.1:1/f.bin"}})
	for _, s := range path {
		var err error
		if s == domain.StatusError {
			err = task.SetError(domain.TaskError{Code: domain.ErrorCodeFatal, Message: "boom"}, time.Now())
		} else {
			err = task.TransitionTo(s, time.Now())
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Tasks().Update(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

// collect drains the events already delivered to sub. Bus.Publish delivers
// synchronously, so every event published before the call is present.
func collect(sub *events.Subscription) []events.Event {
	var out []events.Event
	for {
		select {
		case e, ok := <-sub.C():
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

func eventTypes(evs []events.Event) []events.Type {
	out := make([]events.Type, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}
