package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

// waitTimeout bounds every wait for an expected event.
const waitTimeout = 5 * time.Second

// quiet is how long negative assertions ("nothing else starts") wait.
const quiet = 100 * time.Millisecond

// fakeRunner is a controllable TaskRunner. Run marks the task running in
// the store (failing with ErrConflict when it is not queued, like the real
// runner), records the start, and blocks until the test releases it
// (completed) or its context is cancelled (paused for service.ErrPaused,
// otherwise queued).
type fakeRunner struct {
	store   storage.Store
	started chan string // every successful start, in order

	mu       sync.Mutex
	releases map[string]chan struct{}
	stubborn map[string]bool  // ignore cancellation until released
	causes   map[string]error // cancellation cause of each stopped run
	active   int
	maxSeen  int
}

func newFakeRunner(store storage.Store) *fakeRunner {
	return &fakeRunner{
		store:    store,
		started:  make(chan string, 1000),
		releases: make(map[string]chan struct{}),
		stubborn: make(map[string]bool),
		causes:   make(map[string]error),
	}
}

func (f *fakeRunner) releaseCh(id string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := f.releases[id]
	if ch == nil {
		ch = make(chan struct{})
		f.releases[id] = ch
	}
	return ch
}

// release lets the run of id complete.
func (f *fakeRunner) release(id string) { close(f.releaseCh(id)) }

func (f *fakeRunner) setStatus(ctx context.Context, id string, to domain.TaskStatus) error {
	ctx = context.WithoutCancel(ctx) // the final state is written after cancellation
	t, err := f.store.Tasks().Get(ctx, id)
	if err != nil {
		return err
	}
	if to == domain.StatusCompleted {
		if err := t.TransitionTo(domain.StatusPostprocessing, time.Now()); err != nil {
			return err
		}
	}
	if err := t.TransitionTo(to, time.Now()); err != nil {
		return err
	}
	return f.store.Tasks().Update(ctx, t)
}

func (f *fakeRunner) Run(ctx context.Context, id string) error {
	t, err := f.store.Tasks().Get(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != domain.StatusQueued {
		return domain.Conflict("fake: task %s is %s", id, t.Status)
	}
	if err := f.setStatus(ctx, id, domain.StatusRunning); err != nil {
		return err
	}
	f.mu.Lock()
	f.active++
	f.maxSeen = max(f.maxSeen, f.active)
	stubborn := f.stubborn[id]
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	f.started <- id

	done := ctx.Done()
	if stubborn {
		done = nil
	}
	select {
	case <-f.releaseCh(id):
		return f.setStatus(ctx, id, domain.StatusCompleted)
	case <-done:
		cause := context.Cause(ctx)
		f.mu.Lock()
		f.causes[id] = cause
		f.mu.Unlock()
		to := domain.StatusQueued
		if errors.Is(cause, service.ErrPaused) {
			to = domain.StatusPaused
		}
		return f.setStatus(ctx, id, to)
	}
}

func (f *fakeRunner) cause(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.causes[id]
}

func (f *fakeRunner) max() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxSeen
}

// nextStart waits for the next start.
func (f *fakeRunner) nextStart(t *testing.T) string {
	t.Helper()
	select {
	case id := <-f.started:
		return id
	case <-time.After(waitTimeout):
		t.Fatal("no task started")
		return ""
	}
}

// starts waits for n starts and returns them sorted.
func (f *fakeRunner) starts(t *testing.T, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = f.nextStart(t)
	}
	slices.Sort(ids)
	return ids
}

// noStart asserts that nothing starts for a while.
func (f *fakeRunner) noStart(t *testing.T) {
	t.Helper()
	select {
	case id := <-f.started:
		t.Fatalf("unexpected start of %s", id)
	case <-time.After(quiet):
	}
}

type harness struct {
	store  storage.Store
	runner *fakeRunner
	sched  *Scheduler
	cancel context.CancelFunc
	done   chan error
}

func openStore(t *testing.T) storage.Store {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "dlm.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// defaultsWith returns built-in defaults with maxConcurrentDownloads n.
func defaultsWith(n int) func() settings.Settings {
	return func() settings.Settings {
		d := settings.Defaults("")
		d.MaxConcurrentDownloads = settings.Some(n)
		return d
	}
}

func newHarness(t *testing.T, store storage.Store, o Options) *harness {
	t.Helper()
	if store == nil {
		store = openStore(t)
	}
	h := &harness{store: store, runner: newFakeRunner(store)}
	o.Store = store
	if o.Runner == nil {
		o.Runner = h.runner
	}
	if o.Defaults == nil {
		o.Defaults = defaultsWith(4)
	}
	if o.Tick == 0 {
		o.Tick = time.Hour // tests drive admission with Wake unless they test ticks
	}
	h.sched = New(o)
	return h
}

// start runs the scheduler until the test ends.
func (h *harness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- h.sched.Run(ctx) }()
	t.Cleanup(func() { h.stop(t) })
}

// stop cancels Run and waits for it to return.
func (h *harness) stop(t *testing.T) {
	t.Helper()
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return")
	}
}

func (h *harness) queue(t *testing.T, name string, s settings.Settings, state domain.QueueState) *domain.Queue {
	t.Helper()
	ctx := context.Background()
	q, err := domain.NewQueue(domain.NewID(), name, time.Now(), s)
	if err != nil {
		t.Fatal(err)
	}
	if state != "" {
		q.State = state
	}
	if q.Position, err = h.store.Queues().NextPosition(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Queues().Create(ctx, q); err != nil {
		t.Fatal(err)
	}
	return q
}

func (h *harness) task(t *testing.T, queueID string, prio domain.Priority) *domain.Task {
	t.Helper()
	ctx := context.Background()
	task, err := domain.NewTask(domain.NewID(), time.Now(), domain.NewTaskParams{
		QueueID: queueID, Priority: prio, Source: domain.Source{CurrentURL: "http://127.0.0.1:1/f"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Position, err = h.store.Tasks().NextPosition(ctx, task.QueueID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Tasks().Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	return task
}

func (h *harness) status(t *testing.T, id string) domain.TaskStatus {
	t.Helper()
	got, err := h.store.Tasks().Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got.Status
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func ids(tasks ...*domain.Task) []string {
	out := make([]string, len(tasks))
	for i, t := range tasks {
		out[i] = t.ID
	}
	slices.Sort(out)
	return out
}

// countingStore counts queue listings (one per admission pass) and can
// hold the first one until released.
type countingStore struct {
	storage.Store
	lists atomic.Int64
	gate  chan struct{} // nil: never block
	once  sync.Once
}

func (s *countingStore) Queues() storage.QueueRepository {
	return countingQueues{QueueRepository: s.Store.Queues(), s: s}
}

type countingQueues struct {
	storage.QueueRepository
	s *countingStore
}

func (q countingQueues) List(ctx context.Context) ([]*domain.Queue, error) {
	q.s.lists.Add(1)
	if q.s.gate != nil {
		q.s.once.Do(func() { <-q.s.gate })
	}
	return q.QueueRepository.List(ctx)
}
