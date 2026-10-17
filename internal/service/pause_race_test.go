package service_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/scheduler"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

// errorRecorder is a slog handler that keeps error-level records.
type errorRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (h *errorRecorder) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelError }

func (h *errorRecorder) Handle(_ context.Context, r slog.Record) error {
	msg := r.Message
	r.Attrs(func(a slog.Attr) bool {
		msg += " " + a.String()
		return true
	})
	h.mu.Lock()
	h.msgs = append(h.msgs, msg)
	h.mu.Unlock()
	return nil
}

func (h *errorRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *errorRecorder) WithGroup(string) slog.Handler      { return h }

func (h *errorRecorder) errors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.msgs)
}

func isRunning(ctx context.Context, t *testing.T, store storage.Store, id string) bool {
	t.Helper()
	got, err := store.Tasks().Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return got.Status == domain.StatusRunning
}

// TestPauseRacesWithSchedulerStart is the regression test for the lost
// update between TaskService.Pause and the scheduler starting the same
// task: whenever Pause succeeds the task must end up paused (never running
// or completed), and no run may fail on an unexpected state.
func TestPauseRacesWithSchedulerStart(t *testing.T) {
	const iterations = 200
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "dlm.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Every download blocks after its first bytes until it is cancelled.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(make([]byte, 1024))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	clients := netclient.New(netclient.Options{})
	t.Cleanup(func() { _ = clients.Close() })
	reg, err := engine.NewRegistry(httpx.New(engine.Env{Clock: time.Now, Clients: clients}))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	defaults := settings.Defaults(dir)
	defaults.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxyNone})
	defaultsFn := func() settings.Settings { return defaults }
	rec := &errorRecorder{}
	log := slog.New(rec)

	locks := service.NewTaskLocks()
	runner := service.NewRunner(service.RunnerDeps{
		Store: store, Engines: reg, Defaults: defaultsFn, Logger: log, TaskLocks: locks,
	})
	sched := scheduler.New(scheduler.Options{
		Store: store, Runner: runner, Defaults: defaultsFn, Logger: log, Tick: time.Hour,
	})
	tasks := service.NewTaskService(service.Deps{
		Store: store, Engines: reg, Scheduler: sched, Runner: runner,
		Defaults: defaultsFn, Logger: log, TaskLocks: locks,
	})
	runCtx, stop := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- sched.Run(runCtx) }()
	defer func() {
		stop()
		if err := <-runDone; err != nil {
			t.Errorf("scheduler Run: %v", err)
		}
	}()

	var ids []string
	for i := range iterations {
		task, err := tasks.Create(ctx, service.CreateTaskInput{Source: domain.Source{CurrentURL: srv.URL + "/f.bin"}})
		if err != nil {
			t.Fatalf("iteration %d: Create: %v", i, err)
		}
		ids = append(ids, task.ID)
		// Vary the interleaving: pause at once, after yielding, after a
		// delay spread over the admission window, or once the run started.
		switch i % 4 {
		case 1:
			runtime.Gosched()
		case 2:
			time.Sleep(time.Duration(i%20) * 250 * time.Microsecond)
		case 3:
			for len(sched.Running()) == 0 || !isRunning(ctx, t, store, task.ID) {
				time.Sleep(100 * time.Microsecond)
			}
		}
		res := tasks.Pause(ctx, []string{task.ID})
		if res[0].Err != nil {
			t.Fatalf("iteration %d: Pause: %v", i, res[0].Err)
		}
		if res[0].Task.Status != domain.StatusPaused {
			t.Fatalf("iteration %d: Pause returned status %s", i, res[0].Task.Status)
		}
		if got, err := store.Tasks().Get(ctx, task.ID); err != nil || got.Status != domain.StatusPaused {
			t.Fatalf("iteration %d: stored status after Pause = %v, %v", i, got.Status, err)
		}
	}

	// Let any admission still in flight settle, then check that nothing
	// overwrote a pause later on.
	sched.Wake()
	deadline := time.Now().Add(5 * time.Second)
	for len(sched.Running()) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("tasks still running: %v", sched.Running())
		}
		time.Sleep(time.Millisecond)
	}
	started := 0
	for _, id := range ids {
		got, err := store.Tasks().Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.StatusPaused {
			t.Errorf("task %s: final status %s, want paused", id, got.Status)
		}
		if got.StartedAt != nil {
			started++
		}
	}
	t.Logf("%d of %d tasks were running when paused", started, len(ids))
	if errs := rec.errors(); len(errs) > 0 {
		t.Errorf("unexpected error logs (%d), first: %s", len(errs), errs[0])
	}
}
