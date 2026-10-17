package service

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
)

// oneTimeServer serves data exactly once; later requests get 410 Gone, like
// a one-time download link.
type oneTimeServer struct {
	*httptest.Server
	data     []byte
	requests atomic.Int64
}

func newOneTimeServer(t *testing.T, size int) *oneTimeServer {
	t.Helper()
	s := &oneTimeServer{data: make([]byte, size)}
	for i := range s.data {
		s.data[i] = byte(i % 251)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if s.requests.Add(1) > 1 {
			http.Error(w, "gone", http.StatusGone)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
		w.Header().Set("Content-Disposition", `attachment; filename="once.bin"`)
		_, _ = w.Write(s.data)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestPartSuffixMatchesHTTPEngine(t *testing.T) {
	if partSuffix != httpx.PartSuffix {
		t.Fatalf("partSuffix %q != httpx.PartSuffix %q", partSuffix, httpx.PartSuffix)
	}
}

func TestTaskServiceCreateDefaults(t *testing.T) {
	f := newSvcFixture(t)
	sub := f.bus.Subscribe(t.Context(), events.Filter{Types: []events.Type{events.TypeTaskCreated}})

	a := f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: "https://example.com/a.zip"}})
	if a.QueueID != domain.MainQueueID || a.Status != domain.StatusQueued || a.Position != 0 || a.Engine != httpx.EngineName ||
		a.Priority != domain.PriorityNormal || a.Source.OriginalURL != a.Source.CurrentURL || a.Size != -1 {
		t.Errorf("task = %+v", a)
	}
	if f.sched.wakes.Load() != 1 {
		t.Errorf("wakes = %d, want 1", f.sched.wakes.Load())
	}
	dir := filepath.Join(f.dir, "sub")
	b := f.create(t, CreateTaskInput{
		Source: domain.Source{CurrentURL: "https://example.com/b"}, Name: "b.bin", Dir: dir,
		Priority: domain.PriorityHigh, Labels: map[string]string{"k": "v"}, Paused: true,
	})
	if b.Status != domain.StatusPaused || b.Position != 1 || b.Name != "b.bin" || b.Dir != dir || b.Labels["k"] != "v" {
		t.Errorf("task = %+v", b)
	}
	if f.sched.wakes.Load() != 1 {
		t.Error("creating a paused task woke the scheduler")
	}
	if got := f.get(t, b.ID); got.Status != domain.StatusPaused {
		t.Errorf("stored status = %s", got.Status)
	}
	evs := collect(sub)
	if len(evs) != 2 || evs[0].TaskID != a.ID || evs[1].TaskID != b.ID {
		t.Fatalf("events = %+v", evs)
	}
	if p, ok := evs[0].Data.(*domain.Task); !ok || p.ID != a.ID {
		t.Errorf("payload = %#v", evs[0].Data)
	}
}

func TestTaskServiceCreateRejects(t *testing.T) {
	f := newSvcFixture(t)
	src := domain.Source{CurrentURL: "https://example.com/x"}
	tests := []struct {
		name string
		in   CreateTaskInput
		want error
	}{
		{"missing url", CreateTaskInput{}, domain.ErrInvalidArgument},
		{"unsupported scheme", CreateTaskInput{Source: domain.Source{CurrentURL: "ftp://example.com/x"}}, domain.ErrInvalidArgument},
		{"relative url", CreateTaskInput{Source: domain.Source{CurrentURL: "example.com/x"}}, domain.ErrInvalidArgument},
		{"unknown queue", CreateTaskInput{Source: src, QueueID: "nope"}, domain.ErrNotFound},
		{"relative dir", CreateTaskInput{Source: src, Dir: "rel/dir"}, domain.ErrInvalidArgument},
		{"name with dirs", CreateTaskInput{Source: src, Name: filepath.Join("..", "x")}, domain.ErrInvalidArgument},
		{"dot dot name", CreateTaskInput{Source: src, Name: ".."}, domain.ErrInvalidArgument},
		{"queue-only setting", CreateTaskInput{Source: src, Settings: settings.Settings{MaxConcurrentDownloads: settings.Some(2)}}, domain.ErrInvalidArgument},
		{"bad priority", CreateTaskInput{Source: src, Priority: 7}, domain.ErrInvalidArgument},
		{"unknown resolve id", CreateTaskInput{ResolveID: "nope"}, domain.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.tasks.Create(t.Context(), tt.in)
			wantErr(t, err, tt.want)
		})
	}
	page, err := f.tasks.List(t.Context(), storage.TaskFilter{})
	if err != nil || len(page.Tasks) != 0 {
		t.Errorf("rejected creates stored tasks: %d, %v", len(page.Tasks), err)
	}
	if f.sched.wakes.Load() != 0 {
		t.Error("rejected creates woke the scheduler")
	}
}

func TestTaskServiceResolveAndCreateReusesProbe(t *testing.T) {
	f := newSvcFixture(t)
	srv := newOneTimeServer(t, 100<<10)

	res, err := f.tasks.Resolve(t.Context(), ResolveInput{Source: domain.Source{CurrentURL: srv.URL + "/dl?token=1"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ResolveID == "" || res.Engine != httpx.EngineName || res.Name != "once.bin" || res.Resource.Size != int64(len(srv.data)) ||
		!res.ExpiresAt.After(time.Now()) {
		t.Fatalf("resolve result = %+v", res)
	}

	task := f.create(t, CreateTaskInput{ResolveID: res.ResolveID})
	if task.Name != "once.bin" || task.Size != int64(len(srv.data)) || task.Resource == nil || task.Engine != httpx.EngineName ||
		task.Source.CurrentURL != srv.URL+"/dl?token=1" {
		t.Fatalf("task = %+v", task)
	}
	f.runner.mu.Lock()
	attached := f.runner.probes[task.ID] != nil
	f.runner.mu.Unlock()
	if !attached {
		t.Fatal("probe not attached to the runner")
	}
	// The resolve result is consumed.
	_, err = f.tasks.Create(t.Context(), CreateTaskInput{ResolveID: res.ResolveID})
	wantErr(t, err, domain.ErrNotFound)

	// The first run uses the attached probe: the one-time URL is fetched once.
	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	got := f.get(t, task.ID)
	if got.Status != domain.StatusCompleted {
		t.Fatalf("status = %s, error %+v", got.Status, got.Error)
	}
	if n := srv.requests.Load(); n != 1 {
		t.Errorf("server saw %d requests, want 1", n)
	}
	sameContent(t, filepath.Join(f.dir, "once.bin"), srv.data)
	f.runner.mu.Lock()
	left := len(f.runner.probes)
	f.runner.mu.Unlock()
	if left != 0 {
		t.Errorf("%d probes still attached after the run", left)
	}
}

func TestTaskServiceResolveErrors(t *testing.T) {
	f := newSvcFixture(t)
	srv := newFileServer(t, 1024, 0)
	ctx := t.Context()
	_, err := f.tasks.Resolve(ctx, ResolveInput{Source: domain.Source{CurrentURL: srv.URL + "/missing"}})
	wantErr(t, err, domain.ErrInvalidArgument)
	var ee *engine.Error
	if !errors.As(err, &ee) {
		t.Errorf("err %v does not unwrap to *engine.Error", err)
	}
	_, err = f.tasks.Resolve(ctx, ResolveInput{Source: domain.Source{CurrentURL: "ftp://x/y"}})
	wantErr(t, err, domain.ErrInvalidArgument)
	_, err = f.tasks.Resolve(ctx, ResolveInput{Source: domain.Source{CurrentURL: srv.URL + "/a"}, QueueID: "nope"})
	wantErr(t, err, domain.ErrNotFound)
	_, err = f.tasks.Resolve(ctx, ResolveInput{Source: domain.Source{CurrentURL: srv.URL + "/a"}, Settings: settings.Settings{Schedule: settings.Some(settings.Schedule{})}})
	wantErr(t, err, domain.ErrInvalidArgument)
}

func TestTaskServiceCreateExpiredResolve(t *testing.T) {
	f := newSvcFixture(t)
	srv := newFileServer(t, 1024, 0)
	res, err := f.tasks.Resolve(t.Context(), ResolveInput{Source: domain.Source{CurrentURL: srv.URL + "/files/x.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(2 * time.Minute) // TTL is one minute
	_, err = f.tasks.Create(t.Context(), CreateTaskInput{ResolveID: res.ResolveID})
	wantErr(t, err, domain.ErrNotFound)
}

func TestTaskServiceCreateRejectedKeepsResolve(t *testing.T) {
	f := newSvcFixture(t)
	srv := newFileServer(t, 1024, 0)
	res, err := f.tasks.Resolve(t.Context(), ResolveInput{Source: domain.Source{CurrentURL: srv.URL + "/files/x.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.tasks.Create(t.Context(), CreateTaskInput{ResolveID: res.ResolveID, Priority: 9})
	wantErr(t, err, domain.ErrInvalidArgument)
	task, err := f.tasks.Create(t.Context(), CreateTaskInput{ResolveID: res.ResolveID, Name: "renamed.bin"})
	if err != nil {
		t.Fatalf("resolve result consumed by a rejected create: %v", err)
	}
	if task.Name != "renamed.bin" {
		t.Errorf("name = %q", task.Name)
	}
}

func TestTaskServiceCreateBatch(t *testing.T) {
	f := newSvcFixture(t)
	res := f.tasks.CreateBatch(t.Context(), []CreateTaskInput{
		{Source: domain.Source{CurrentURL: "https://example.com/1"}},
		{Source: domain.Source{CurrentURL: "mailto:x@example.com"}},
		{Source: domain.Source{CurrentURL: "https://example.com/3"}, QueueID: "nope"},
		{Source: domain.Source{CurrentURL: "https://example.com/4"}},
	})
	if len(res) != 4 {
		t.Fatalf("results = %d", len(res))
	}
	if res[0].Err != nil || res[0].Task == nil || res[0].Task.Position != 0 {
		t.Errorf("item 0 = %+v", res[0])
	}
	if !errors.Is(res[1].Err, domain.ErrInvalidArgument) || res[1].Task != nil {
		t.Errorf("item 1 = %+v", res[1])
	}
	if !errors.Is(res[2].Err, domain.ErrNotFound) || res[2].Task != nil {
		t.Errorf("item 2 = %+v", res[2])
	}
	if res[3].Err != nil || res[3].Task == nil || res[3].Task.Position != 1 || res[3].Task.Source.CurrentURL != "https://example.com/4" {
		t.Errorf("item 3 = %+v", res[3])
	}
}

func TestTaskServiceGetList(t *testing.T) {
	f := newSvcFixture(t)
	a := f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: "https://example.com/a"}})
	f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: "https://example.com/b"}, Paused: true})
	got, err := f.tasks.Get(t.Context(), a.ID)
	if err != nil || got.ID != a.ID {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	_, err = f.tasks.Get(t.Context(), "nope")
	wantErr(t, err, domain.ErrNotFound)
	page, err := f.tasks.List(t.Context(), storage.TaskFilter{Statuses: []domain.TaskStatus{domain.StatusPaused}})
	if err != nil || len(page.Tasks) != 1 {
		t.Fatalf("List = %d, %v", len(page.Tasks), err)
	}
	_, err = f.tasks.List(t.Context(), storage.TaskFilter{Cursor: "garbage"})
	wantErr(t, err, domain.ErrInvalidArgument)
}

func TestTaskServiceUpdate(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	sub := f.bus.Subscribe(ctx, events.Filter{Types: []events.Type{events.TypeTaskUpdated}})
	task := f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: "https://example.com/a"}, Labels: map[string]string{"a": "1"}})

	name, dir, prio := "new.bin", filepath.Join(f.dir, "d"), domain.PriorityLow
	labels := map[string]string{"b": "2"}
	got, err := f.tasks.Update(ctx, task.ID, UpdateTaskInput{
		Name: &name, Dir: &dir, Priority: &prio, Labels: &labels,
		SettingsPatch: []byte(`{"maxConnections":3,"headers":{"X-A":"1"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != name || got.Dir != dir || got.Priority != prio || got.Labels["b"] != "2" || got.Labels["a"] != "" ||
		got.Settings.MaxConnections.Value() != 3 || got.Settings.Headers.Value()["X-A"] != "1" {
		t.Errorf("updated = %+v", got)
	}
	if stored := f.get(t, task.ID); stored.Name != name || stored.Settings.MaxConnections.Value() != 3 {
		t.Errorf("stored = %+v", stored)
	}
	if evs := collect(sub); len(evs) != 1 || evs[0].TaskID != task.ID {
		t.Errorf("events = %+v", evs)
	}

	bad := map[string]UpdateTaskInput{
		"queue-only setting": {SettingsPatch: []byte(`{"maxConcurrentDownloads":1}`)},
		"malformed patch":    {SettingsPatch: []byte(`{`)},
		"bad priority":       {Priority: ptr(domain.Priority(5))},
		"name with dirs":     {Name: ptr(filepath.Join("a", "b"))},
		"relative dir":       {Dir: ptr("rel")},
		"empty label key":    {Labels: &map[string]string{" ": "x"}},
	}
	for name, in := range bad {
		if _, err := f.tasks.Update(ctx, task.ID, in); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	_, err = f.tasks.Update(ctx, "nope", UpdateTaskInput{Name: &name})
	wantErr(t, err, domain.ErrNotFound)

	// Partial data pins the name and directory; other fields still change.
	if err := f.store.Tasks().UpdateProgress(ctx, task.ID, 10, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	other := "other.bin"
	_, err = f.tasks.Update(ctx, task.ID, UpdateTaskInput{Name: &other})
	wantErr(t, err, domain.ErrConflict)
	_, err = f.tasks.Update(ctx, task.ID, UpdateTaskInput{Dir: ptr(f.dir)})
	wantErr(t, err, domain.ErrConflict)
	if _, err := f.tasks.Update(ctx, task.ID, UpdateTaskInput{Name: &name, Priority: ptr(domain.PriorityHigh)}); err != nil {
		t.Errorf("unchanged name with partial data: %v", err)
	}
}

func TestTaskServiceUpdateRunning(t *testing.T) {
	f := newSvcFixture(t)
	srv := newFileServer(t, 2<<20, 5*time.Millisecond)
	task := f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: srv.URL + "/files/r.bin"}})
	f.sched.run(t, f.store, task.ID)

	name := "renamed.bin"
	_, err := f.tasks.Update(t.Context(), task.ID, UpdateTaskInput{Name: &name})
	wantErr(t, err, domain.ErrConflict)
	if _, err := f.tasks.Update(t.Context(), task.ID, UpdateTaskInput{
		SettingsPatch: []byte(`{"speedLimit":65536}`), Labels: &map[string]string{"l": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if l := f.limits.Task(task.ID).Limit(); l != 65536 {
		t.Errorf("task limit = %d, want 65536", l)
	}
	// The runner's final write keeps the changes made while it ran.
	res := f.tasks.Pause(t.Context(), []string{task.ID})
	if res[0].Err != nil {
		t.Fatal(res[0].Err)
	}
	got := f.get(t, task.ID)
	if got.Status != domain.StatusPaused || got.Settings.SpeedLimit.Value() != 65536 || got.Labels["l"] != "1" {
		t.Errorf("after pause: status %s settings %+v labels %v", got.Status, got.Settings, got.Labels)
	}
}

func TestTaskServiceMoveReorder(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	q, err := f.queues.Create(ctx, CreateQueueInput{Name: "q"})
	if err != nil {
		t.Fatal(err)
	}
	inQ := f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: "https://example.com/q"}, QueueID: q.ID})
	var ids []string
	for i := range 4 {
		ids = append(ids, f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: fmt.Sprintf("https://example.com/%d", i)}}).ID)
	}

	wakes := f.sched.wakes.Load()
	moved, err := f.tasks.Move(ctx, ids[0], q.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.QueueID != q.ID || moved.Position != inQ.Position+1 {
		t.Errorf("moved = queue %s position %d", moved.QueueID, moved.Position)
	}
	if f.sched.wakes.Load() != wakes+1 {
		t.Error("Move did not wake the scheduler")
	}
	if same, err := f.tasks.Move(ctx, ids[0], q.ID); err != nil || same.Position != moved.Position {
		t.Errorf("move to own queue = %+v, %v", same, err)
	}
	_, err = f.tasks.Move(ctx, ids[1], "nope")
	wantErr(t, err, domain.ErrNotFound)
	_, err = f.tasks.Move(ctx, "nope", q.ID)
	wantErr(t, err, domain.ErrNotFound)

	// Main now holds ids[1..3] at positions 1, 2, 3 (gap at 0).
	order := func() []string {
		tasks, err := queueTasks(ctx, f.store, domain.MainQueueID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for i, tk := range tasks {
			if tk.Position != int64(i) {
				t.Errorf("task %s at index %d has position %d", tk.ID, i, tk.Position)
			}
			out = append(out, tk.ID)
		}
		return out
	}
	if _, err := f.tasks.Reorder(ctx, ids[3], 0); err != nil {
		t.Fatal(err)
	}
	if got, want := order(), []string{ids[3], ids[1], ids[2]}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if _, err := f.tasks.Reorder(ctx, ids[3], 50); err != nil {
		t.Fatal(err)
	}
	if got, want := order(), []string{ids[1], ids[2], ids[3]}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	_, err = f.tasks.Reorder(ctx, ids[1], -1)
	wantErr(t, err, domain.ErrInvalidArgument)
	_, err = f.tasks.Reorder(ctx, "nope", 0)
	wantErr(t, err, domain.ErrNotFound)
}

func TestTaskServicePauseMatrix(t *testing.T) {
	f := newSvcFixture(t)
	tests := []struct {
		name    string
		path    []domain.TaskStatus
		want    domain.TaskStatus
		wantErr error
		stopped bool
	}{
		{"queued", nil, domain.StatusPaused, nil, false},
		{"paused is a no-op", []domain.TaskStatus{domain.StatusPaused}, domain.StatusPaused, nil, false},
		{"error", []domain.TaskStatus{domain.StatusError}, domain.StatusPaused, nil, false},
		{"awaiting link", []domain.TaskStatus{domain.StatusRunning, domain.StatusAwaitingLink}, domain.StatusPaused, nil, false},
		{"stale running", []domain.TaskStatus{domain.StatusRunning}, domain.StatusPaused, nil, true},
		{"stale refreshing", []domain.TaskStatus{domain.StatusRunning, domain.StatusRefreshing}, domain.StatusPaused, nil, true},
		{"postprocessing", []domain.TaskStatus{domain.StatusRunning, domain.StatusPostprocessing}, domain.StatusPostprocessing, domain.ErrConflict, false},
		{"completed", []domain.TaskStatus{domain.StatusRunning, domain.StatusPostprocessing, domain.StatusCompleted}, domain.StatusCompleted, domain.ErrConflict, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := f.withStatus(t, tt.path...)
			before := len(f.sched.stopCalls())
			res := f.tasks.Pause(t.Context(), []string{task.ID})
			if len(res) != 1 || res[0].ID != task.ID || !errors.Is(res[0].Err, tt.wantErr) || (tt.wantErr == nil && res[0].Err != nil) {
				t.Fatalf("result = %+v", res)
			}
			if got := f.get(t, task.ID); got.Status != tt.want {
				t.Errorf("status = %s, want %s", got.Status, tt.want)
			}
			calls := f.sched.stopCalls()[before:]
			if tt.stopped != (len(calls) == 1) {
				t.Fatalf("stop calls = %+v", calls)
			}
			if tt.stopped && !errors.Is(calls[0].Cause, ErrPaused) {
				t.Errorf("stop cause = %v", calls[0].Cause)
			}
		})
	}
	res := f.tasks.Pause(t.Context(), []string{"nope"})
	wantErr(t, res[0].Err, domain.ErrNotFound)
}

func TestTaskServicePauseRunning(t *testing.T) {
	f := newSvcFixture(t)
	srv := newFileServer(t, 2<<20, 5*time.Millisecond)
	task := f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: srv.URL + "/files/p.bin"}})
	f.sched.run(t, f.store, task.ID)
	sub := f.bus.Subscribe(t.Context(), events.Filter{Types: []events.Type{events.TypeTaskStatus}, TaskID: task.ID})

	res := f.tasks.Pause(t.Context(), []string{task.ID})
	if res[0].Err != nil || res[0].Task.Status != domain.StatusPaused {
		t.Fatalf("result = %+v", res[0])
	}
	calls := f.sched.stopCalls()
	if len(calls) != 1 || calls[0].TaskID != task.ID || !errors.Is(calls[0].Cause, ErrPaused) {
		t.Fatalf("stop calls = %+v", calls)
	}
	// The runner persisted paused; the service did not transition again.
	evs := collect(sub)
	if len(evs) != 1 {
		t.Fatalf("status events = %+v", evs)
	}
	if d, ok := evs[0].Data.(StatusData); !ok || d.From != domain.StatusRunning || d.To != domain.StatusPaused {
		t.Errorf("status event = %#v", evs[0].Data)
	}
}

func TestTaskServiceResumeMatrix(t *testing.T) {
	f := newSvcFixture(t)
	tests := []struct {
		name    string
		path    []domain.TaskStatus
		want    domain.TaskStatus
		wantErr error
	}{
		{"paused", []domain.TaskStatus{domain.StatusPaused}, domain.StatusQueued, nil},
		{"error", []domain.TaskStatus{domain.StatusError}, domain.StatusQueued, nil},
		{"queued is a no-op", nil, domain.StatusQueued, nil},
		{"running is a no-op", []domain.TaskStatus{domain.StatusRunning}, domain.StatusRunning, nil},
		{"awaiting link", []domain.TaskStatus{domain.StatusRunning, domain.StatusAwaitingLink}, domain.StatusAwaitingLink, domain.ErrConflict},
		{"completed", []domain.TaskStatus{domain.StatusRunning, domain.StatusPostprocessing, domain.StatusCompleted}, domain.StatusCompleted, domain.ErrConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := f.withStatus(t, tt.path...)
			wakes := f.sched.wakes.Load()
			res := f.tasks.Resume(t.Context(), []string{task.ID})
			if !errors.Is(res[0].Err, tt.wantErr) || (tt.wantErr == nil && res[0].Err != nil) {
				t.Fatalf("result = %+v", res[0])
			}
			got := f.get(t, task.ID)
			if got.Status != tt.want {
				t.Errorf("status = %s, want %s", got.Status, tt.want)
			}
			if got.Status == domain.StatusQueued && got.Error != nil {
				t.Errorf("error not cleared: %+v", got.Error)
			}
			woke := f.sched.wakes.Load() > wakes
			if changed := len(tt.path) > 0 && tt.want == domain.StatusQueued; changed != woke {
				t.Errorf("woke = %v", woke)
			}
		})
	}
}

func TestTaskServiceRestart(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	task := f.withStatus(t, domain.StatusRunning, domain.StatusPostprocessing, domain.StatusCompleted)
	task.Name, task.Dir, task.Size = "done.bin", f.dir, 100
	task.Downloaded = 100
	if err := f.store.Tasks().Update(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Segments().Replace(ctx, task.ID, []domain.Segment{{Index: 0, Start: 0, End: 100, Done: 100}}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.EngineState().Put(ctx, task.ID, []byte("state")); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(f.dir, "done.bin")
	writeFile(t, final)
	writeFile(t, final+partSuffix)

	_, err := f.tasks.Restart(ctx, task.ID)
	wantErr(t, err, domain.ErrConflict)
	if !exists(t, final) {
		t.Fatal("restart removed the final file")
	}
	if err := os.Remove(final); err != nil {
		t.Fatal(err)
	}
	got, err := f.tasks.Restart(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusQueued || got.Downloaded != 0 || got.CompletedAt != nil || got.Resource != nil && got.Resource.Size < -1 {
		t.Errorf("restarted = %+v", got)
	}
	if exists(t, final+partSuffix) {
		t.Error(".part file not removed")
	}
	segs, err := f.store.Segments().List(ctx, task.ID)
	if err != nil || len(segs) != 0 {
		t.Errorf("segments = %v, %v", segs, err)
	}
	_, err = f.store.EngineState().Get(ctx, task.ID)
	wantErr(t, err, domain.ErrNotFound)

	errored := f.withStatus(t, domain.StatusError)
	if got, err := f.tasks.Restart(ctx, errored.ID); err != nil || got.Status != domain.StatusQueued || got.Error != nil {
		t.Errorf("restart errored = %+v, %v", got, err)
	}
	for _, path := range [][]domain.TaskStatus{nil, {domain.StatusPaused}, {domain.StatusRunning}} {
		tk := f.withStatus(t, path...)
		_, err := f.tasks.Restart(ctx, tk.ID)
		wantErr(t, err, domain.ErrConflict)
	}
	_, err = f.tasks.Restart(ctx, "nope")
	wantErr(t, err, domain.ErrNotFound)
}

func TestTaskServiceDelete(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	srv := newFileServer(t, 2<<20, 5*time.Millisecond)
	sub := f.bus.Subscribe(ctx, events.Filter{Types: []events.Type{events.TypeTaskDeleted}})

	running := f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: srv.URL + "/files/run.bin"}})
	f.sched.run(t, f.store, running.ID)
	plain := f.withStatus(t, domain.StatusPaused)

	wakes := f.sched.wakes.Load()
	res := f.tasks.Delete(ctx, []string{running.ID, "nope", plain.ID}, DeleteOptions{})
	if res[0].Err != nil || res[2].Err != nil || !errors.Is(res[1].Err, domain.ErrNotFound) {
		t.Fatalf("results = %+v", res)
	}
	if res[0].Task == nil || res[0].Task.ID != running.ID {
		t.Errorf("deleted snapshot = %+v", res[0].Task)
	}
	calls := f.sched.stopCalls()
	if len(calls) != 1 || calls[0].TaskID != running.ID || !errors.Is(calls[0].Cause, ErrRequeued) {
		t.Fatalf("stop calls = %+v", calls)
	}
	for _, id := range []string{running.ID, plain.ID} {
		_, err := f.store.Tasks().Get(ctx, id)
		wantErr(t, err, domain.ErrNotFound)
	}
	if f.sched.wakes.Load() == wakes {
		t.Error("Delete did not wake the scheduler")
	}
	if evs := collect(sub); len(evs) != 2 || evs[0].TaskID != running.ID || evs[1].TaskID != plain.ID {
		t.Errorf("deleted events = %+v", evs)
	}
}

func TestTaskServiceDeleteFiles(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	dir := filepath.Join(f.dir, "dl")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) *domain.Task {
		tk, err := f.tasks.Create(ctx, CreateTaskInput{Source: domain.Source{CurrentURL: "https://example.com/" + name}})
		if err != nil {
			t.Fatal(err)
		}
		tk.Name, tk.Dir = name, dir
		if err := f.store.Tasks().Update(ctx, tk); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	withFiles := mk("file.bin")
	writeFile(t, filepath.Join(dir, "file.bin"))
	writeFile(t, filepath.Join(dir, "file.bin"+partSuffix))
	neighbour := filepath.Join(dir, "neighbour.bin")
	writeFile(t, neighbour)

	// A name that escapes the directory (only possible through older data)
	// must never be followed.
	outside := filepath.Join(f.dir, "outside.bin")
	writeFile(t, outside)
	evil := mk("file.bin")
	evil.Name = filepath.Join("..", "outside.bin")
	if err := f.store.Tasks().Update(ctx, evil); err != nil {
		t.Fatal(err)
	}
	// A directory at the final path is left alone.
	dirTask := mk("folder")
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0o750); err != nil {
		t.Fatal(err)
	}
	keep := mk("keep.bin")
	writeFile(t, filepath.Join(dir, "keep.bin"))
	neverRan := f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: "https://example.com/n"}})

	res := f.tasks.Delete(ctx, []string{withFiles.ID, evil.ID, dirTask.ID, neverRan.ID}, DeleteOptions{Files: true})
	for i, r := range res {
		if r.Err != nil {
			t.Errorf("item %d: %v", i, r.Err)
		}
	}
	if exists(t, filepath.Join(dir, "file.bin")) || exists(t, filepath.Join(dir, "file.bin"+partSuffix)) {
		t.Error("task files not removed")
	}
	if !exists(t, neighbour) || !exists(t, outside) || !exists(t, filepath.Join(dir, "folder")) {
		t.Error("files outside the task were removed")
	}
	if res := f.tasks.Delete(ctx, []string{keep.ID}, DeleteOptions{}); res[0].Err != nil {
		t.Fatal(res[0].Err)
	}
	if !exists(t, filepath.Join(dir, "keep.bin")) {
		t.Error("delete without Files removed the file")
	}
}

func TestRunnerProbeAttachAndRelease(t *testing.T) {
	r := NewRunner(RunnerDeps{})
	first, second := &closeCounter{}, &closeCounter{}
	p1, p2 := &engine.Probe{}, &engine.Probe{}
	p1.Attach(first)
	p2.Attach(second)

	r.AttachProbe("t", p1)
	r.AttachProbe("t", p2)
	if first.n.Load() != 1 {
		t.Error("replaced probe not closed")
	}
	r.ReleaseProbe("t")
	if second.n.Load() != 1 {
		t.Error("released probe not closed")
	}
	r.ReleaseProbe("t") // no probe: no-op
	r.AttachProbe("t", nil)
	if r.takeProbe("t") != nil {
		t.Error("nil probe attached")
	}
}

func TestTaskServiceDeleteReleasesProbe(t *testing.T) {
	f := newSvcFixture(t)
	srv := newOneTimeServer(t, 1024)
	res, err := f.tasks.Resolve(t.Context(), ResolveInput{Source: domain.Source{CurrentURL: srv.URL + "/x"}})
	if err != nil {
		t.Fatal(err)
	}
	task := f.create(t, CreateTaskInput{ResolveID: res.ResolveID})
	if r := f.tasks.Delete(t.Context(), []string{task.ID}, DeleteOptions{}); r[0].Err != nil {
		t.Fatal(r[0].Err)
	}
	f.runner.mu.Lock()
	n := len(f.runner.probes)
	f.runner.mu.Unlock()
	if n != 0 {
		t.Errorf("%d probes attached after delete", n)
	}
}

type closeCounter struct{ n atomic.Int64 }

func (c *closeCounter) Close() error {
	c.n.Add(1)
	return nil
}

func ptr[T any](v T) *T { return &v }

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	ok, err := fileExists(path)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}
