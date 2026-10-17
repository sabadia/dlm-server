package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// fileServer serves data with range support, optionally slowly, and counts
// the body bytes it sent.
type fileServer struct {
	*httptest.Server
	data  []byte
	sent  atomic.Int64
	delay atomic.Int64 // nanoseconds to pause after every 32 KiB written
}

func newFileServer(t *testing.T, size int, delay time.Duration) *fileServer {
	t.Helper()
	data := make([]byte, size)
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test data
	for i := range data {
		data[i] = byte(r.UintN(256))
	}
	fs := &fileServer{data: data}
	fs.delay.Store(int64(delay))
	fs.Server = httptest.NewServer(http.HandlerFunc(fs.serve))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fileServer) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/missing") {
		http.NotFound(w, r)
		return
	}
	start, end := 0, len(fs.data)
	if rg := r.Header.Get("Range"); rg != "" {
		spec := strings.TrimPrefix(rg, "bytes=")
		a, b, _ := strings.Cut(spec, "-")
		start, _ = strconv.Atoi(a)
		if b != "" {
			e, _ := strconv.Atoi(b)
			end = e + 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(fs.data)))
		w.Header().Set("Content-Length", strconv.Itoa(end-start))
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(fs.data)))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"v1"`)
	}
	const chunk = 32 << 10
	for off := start; off < end; off += chunk {
		n, err := w.Write(fs.data[off:min(off+chunk, end)])
		fs.sent.Add(int64(n))
		if err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if d := time.Duration(fs.delay.Load()); d > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(d):
			}
		}
	}
}

type fixture struct {
	store  storage.Store
	runner *Runner
	bus    *events.Bus
	dir    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "dlm.db"), sqlite.Options{})
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
	r := NewRunner(RunnerDeps{
		Store: store, Engines: reg, Limits: ratelimit.NewManager(0), Bus: bus,
		Defaults:         func() settings.Settings { return defaults },
		ProgressInterval: 10 * time.Millisecond,
	})
	return &fixture{store: store, runner: r, bus: bus, dir: dir}
}

func (f *fixture) createTask(ctx context.Context, t *testing.T, url string) *domain.Task {
	t.Helper()
	task, err := domain.NewTask(domain.NewID(), time.Now(), domain.NewTaskParams{Source: domain.Source{CurrentURL: url}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Tasks().Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *fixture) task(t *testing.T, id string) *domain.Task {
	t.Helper()
	task, err := f.store.Tasks().Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func sameContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

func TestRunnerCompletesDownload(t *testing.T) {
	f := newFixture(t)
	srv := newFileServer(t, 2<<20, 0)
	task := f.createTask(t.Context(), t, srv.URL+"/files/data.bin")
	sub := f.bus.Subscribe(t.Context(), events.Filter{Types: []events.Type{events.TypeTaskStatus}})

	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := f.task(t, task.ID)
	if got.Status != domain.StatusCompleted || got.Downloaded != int64(len(srv.data)) || got.CompletedAt == nil {
		t.Fatalf("task = status %s downloaded %d completedAt %v", got.Status, got.Downloaded, got.CompletedAt)
	}
	if got.Engine != httpx.EngineName || got.Name != "data.bin" || got.Dir != f.dir {
		t.Errorf("resolve not recorded: engine=%q name=%q dir=%q", got.Engine, got.Name, got.Dir)
	}
	sameContent(t, filepath.Join(f.dir, "data.bin"), srv.data)

	var seq []domain.TaskStatus
	for len(seq) < 3 {
		select {
		case e := <-sub.C():
			d, ok := e.Data.(StatusData)
			if !ok {
				t.Fatalf("unexpected payload %T", e.Data)
			}
			seq = append(seq, d.To)
		case <-time.After(2 * time.Second):
			t.Fatalf("status events so far: %v", seq)
		}
	}
	if fmt.Sprint(seq) != "[running postprocessing completed]" {
		t.Errorf("status events = %v", seq)
	}
}

func TestRunnerPauseAndResumeWithoutRedownload(t *testing.T) {
	f := newFixture(t)
	size := 2 << 20
	srv := newFileServer(t, size, 2*time.Millisecond)
	task := f.createTask(t.Context(), t, srv.URL+"/files/resume.bin")

	ctx, cancel := context.WithCancelCause(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.runner.Run(ctx, task.ID) }()
	deadline := time.Now().Add(5 * time.Second)
	for srv.sent.Load() < int64(size/3) {
		if time.Now().After(deadline) {
			t.Fatal("download made no progress")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel(ErrPaused)
	if err := <-done; err != nil {
		t.Fatalf("Run after pause: %v", err)
	}
	paused := f.task(t, task.ID)
	segs, err := f.store.Segments().List(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != domain.StatusPaused || paused.Downloaded == 0 || len(segs) == 0 {
		t.Fatalf("after pause: status=%s downloaded=%d segments=%d", paused.Status, paused.Downloaded, len(segs))
	}

	// Resume: paused -> queued -> run again.
	if err := paused.TransitionTo(domain.StatusQueued, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Tasks().Update(t.Context(), paused); err != nil {
		t.Fatal(err)
	}
	srv.delay.Store(0)
	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if got := f.task(t, task.ID); got.Status != domain.StatusCompleted {
		t.Fatalf("after resume: status %s, error %+v", got.Status, got.Error)
	}
	sameContent(t, filepath.Join(f.dir, "resume.bin"), srv.data)
	// Bytes already checkpointed must not be fetched again (allow a little
	// over-read for in-flight chunks at pause time).
	if sent := srv.sent.Load(); sent > int64(size)+int64(size)/4 {
		t.Errorf("server sent %d bytes for a %d-byte file: resume re-downloaded data", sent, size)
	}
}

func TestRunnerCancelWithoutCauseRequeues(t *testing.T) {
	f := newFixture(t)
	srv := newFileServer(t, 2<<20, 5*time.Millisecond)
	task := f.createTask(t.Context(), t, srv.URL+"/files/slow.bin")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.runner.Run(ctx, task.ID) }()
	for srv.sent.Load() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := f.task(t, task.ID); got.Status != domain.StatusQueued {
		t.Fatalf("status = %s, want queued", got.Status)
	}
}

func TestRunnerRecordsFailure(t *testing.T) {
	f := newFixture(t)
	srv := newFileServer(t, 1024, 0)
	task := f.createTask(t.Context(), t, srv.URL+"/missing")
	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := f.task(t, task.ID)
	if got.Status != domain.StatusError || got.Error == nil || got.Error.Code != domain.ErrorCodeLinkExpired || !got.Error.Retryable {
		t.Fatalf("task = %s %+v", got.Status, got.Error)
	}
}

func TestRunnerRejectsNonQueuedTask(t *testing.T) {
	f := newFixture(t)
	task := f.createTask(t.Context(), t, "http://127.0.0.1:1/x")
	if err := task.TransitionTo(domain.StatusPaused, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Tasks().Update(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.Run(t.Context(), task.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if err := f.runner.Run(t.Context(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRecoverInterrupted(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	mk := func(path ...domain.TaskStatus) string {
		task := f.createTask(ctx, t, "http://example.com/f")
		for _, s := range path {
			if err := task.TransitionTo(s, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.store.Tasks().Update(ctx, task); err != nil {
			t.Fatal(err)
		}
		return task.ID
	}
	running := mk(domain.StatusRunning)
	refreshing := mk(domain.StatusRunning, domain.StatusRefreshing)
	post := mk(domain.StatusRunning, domain.StatusPostprocessing)
	awaiting := mk(domain.StatusRunning, domain.StatusAwaitingLink)
	queued := mk()

	n, err := RecoverInterrupted(ctx, f.store, time.Now, nil)
	if err != nil || n != 3 {
		t.Fatalf("RecoverInterrupted = %d, %v; want 3", n, err)
	}
	want := map[string]domain.TaskStatus{
		running: domain.StatusQueued, refreshing: domain.StatusQueued, post: domain.StatusError,
		awaiting: domain.StatusAwaitingLink, queued: domain.StatusQueued,
	}
	for id, st := range want {
		if got := f.task(t, id); got.Status != st {
			t.Errorf("task %s: status %s, want %s", id, got.Status, st)
		}
	}
	if e := f.task(t, post).Error; e == nil || !e.Retryable {
		t.Errorf("postprocessing task error = %+v, want retryable", e)
	}
	if n, err := RecoverInterrupted(ctx, f.store, time.Now, nil); err != nil || n != 0 {
		t.Errorf("second recovery = %d, %v; want 0", n, err)
	}
}
