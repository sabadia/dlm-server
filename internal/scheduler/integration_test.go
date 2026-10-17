package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
)

// slowServer serves a fixed-size body without range support, pausing
// between chunks so downloads overlap.
func slowServer(t *testing.T, size int, delay time.Duration) *httptest.Server {
	t.Helper()
	body := make([]byte, size)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		if r.Method == http.MethodHead {
			return
		}
		const chunk = 16 << 10
		for off := 0; off < size; off += chunk {
			if _, err := w.Write(body[off:min(off+chunk, size)]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type realEnv struct {
	store    storage.Store
	bus      *events.Bus
	runner   *service.Runner
	defaults func() settings.Settings
}

func newRealEnv(t *testing.T) *realEnv {
	t.Helper()
	store := openStore(t)
	clients := netclient.New(netclient.Options{})
	t.Cleanup(func() { _ = clients.Close() })
	reg, err := engine.NewRegistry(httpx.New(engine.Env{Clock: time.Now, Clients: clients},
		httpx.WithCheckpointInterval(20*time.Millisecond),
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
	runner := service.NewRunner(service.RunnerDeps{
		Store: store, Engines: reg, Limits: ratelimit.NewManager(0), Bus: bus, Defaults: defaultsFn,
		ProgressInterval: 10 * time.Millisecond,
	})
	return &realEnv{store: store, bus: bus, runner: runner, defaults: defaultsFn}
}

// runScheduler runs a scheduler on env until the test ends.
func (e *realEnv) runScheduler(t *testing.T) *Scheduler {
	t.Helper()
	s := New(Options{Store: e.store, Runner: e.runner, Defaults: e.defaults, Tick: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(waitTimeout):
			t.Error("Run did not return")
		}
	})
	return s
}

func TestIntegrationQueueConcurrency(t *testing.T) {
	env := newRealEnv(t)
	srv := slowServer(t, 256<<10, 2*time.Millisecond)
	h := &harness{store: env.store}
	q := h.queue(t, "fast", settings.Settings{MaxConcurrentDownloads: settings.Some(2)}, "")
	var tasks []*domain.Task
	for i := range 5 {
		task, err := domain.NewTask(domain.NewID(), time.Now(), domain.NewTaskParams{
			QueueID: q.ID, Source: domain.Source{CurrentURL: srv.URL + "/f" + strconv.Itoa(i) + ".bin"},
		})
		if err != nil {
			t.Fatal(err)
		}
		task.Position = int64(i)
		if err := env.store.Tasks().Create(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}

	// Track how many tasks are running from the status events.
	sub := env.bus.Subscribe(t.Context(), events.Filter{Types: []events.Type{events.TypeTaskStatus}})
	var (
		mu        sync.Mutex
		running   int
		maxRun    int
		completed = make(map[string]bool)
		allDone   = make(chan struct{})
	)
	go func() {
		for ev := range sub.C() {
			d, ok := ev.Data.(service.StatusData)
			if !ok {
				continue
			}
			mu.Lock()
			if d.To == domain.StatusRunning {
				running++
				maxRun = max(maxRun, running)
			}
			if d.From == domain.StatusRunning {
				running--
			}
			if d.To == domain.StatusCompleted {
				completed[ev.TaskID] = true
				if len(completed) == len(tasks) {
					close(allDone)
				}
			}
			mu.Unlock()
		}
	}()

	sched := env.runScheduler(t)
	sampleDone := make(chan struct{})
	maxSampled := 0
	go func() {
		defer close(sampleDone)
		for {
			select {
			case <-allDone:
				return
			case <-time.After(time.Millisecond):
				maxSampled = max(maxSampled, len(sched.Running()))
			}
		}
	}()
	select {
	case <-allDone:
	case <-time.After(20 * time.Second):
		t.Fatal("not all tasks completed")
	}
	<-sampleDone
	mu.Lock()
	defer mu.Unlock()
	if maxRun > 2 || maxSampled > 2 {
		t.Fatalf("max running = %d (events), %d (Running()), want <= 2", maxRun, maxSampled)
	}
	if maxRun < 2 {
		t.Errorf("max running = %d, want the 2 slots used", maxRun)
	}
	for _, task := range tasks {
		got, err := env.store.Tasks().Get(context.Background(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.StatusCompleted || got.Downloaded != 256<<10 {
			t.Errorf("task %s: status %s downloaded %d", task.ID, got.Status, got.Downloaded)
		}
	}
}

func TestIntegrationStartupResumesRecoveredTasks(t *testing.T) {
	env := newRealEnv(t)
	srv := slowServer(t, 64<<10, 0)
	ctx := context.Background()
	// A task left running by a crash, and a plain queued one.
	var ids []string
	for i, interrupted := range []bool{true, false} {
		task, err := domain.NewTask(domain.NewID(), time.Now(), domain.NewTaskParams{
			Source: domain.Source{CurrentURL: srv.URL + "/r" + strconv.Itoa(i) + ".bin"},
		})
		if err != nil {
			t.Fatal(err)
		}
		task.Position = int64(i)
		if interrupted {
			if err := task.TransitionTo(domain.StatusRunning, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		if err := env.store.Tasks().Create(ctx, task); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}

	// Daemon startup order: recover, then run the scheduler.
	if n, err := service.RecoverInterrupted(ctx, env.store, time.Now, nil); err != nil || n != 1 {
		t.Fatalf("RecoverInterrupted = %d, %v", n, err)
	}
	env.runScheduler(t)
	waitFor(t, "recovered tasks completed", func() bool {
		for _, id := range ids {
			got, err := env.store.Tasks().Get(ctx, id)
			if err != nil || got.Status != domain.StatusCompleted {
				return false
			}
		}
		return true
	})
}
