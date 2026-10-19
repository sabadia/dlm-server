package mcp_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/daemon"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/mcp"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

type fixture struct {
	svc   *daemon.Services
	bus   *events.Bus
	store storage.Store
	srv   *sdk.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "dlm.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	clients := netclient.New(netclient.Options{})
	t.Cleanup(func() { _ = clients.Close() })
	reg, err := engine.NewRegistry(httpx.New(engine.Env{Clock: time.Now, Clients: clients}))
	if err != nil {
		t.Fatal(err)
	}
	defaults := settings.Defaults(t.TempDir())
	defaults.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxyNone})
	resolves := engine.NewResolveCache(engine.CacheOptions{})
	t.Cleanup(resolves.Close)
	bus := events.New(events.Options{})
	t.Cleanup(bus.Close)
	svc := daemon.NewServices(service.Deps{
		Store: store, Engines: reg, Resolves: resolves, Bus: bus, Defaults: func() settings.Settings { return defaults },
	})
	srv := mcp.New(mcp.Deps{Tasks: svc.Tasks, Queues: svc.Queues, Settings: svc.Settings, Bus: bus, Version: "test"})
	return &fixture{svc: svc, bus: bus, store: store, srv: srv}
}

// connect returns a client session to the fixture's server over an in-memory
// transport. onProgress receives the progress notifications.
func (f *fixture) connect(t *testing.T, onProgress func(*sdk.ProgressNotificationClientRequest)) *sdk.ClientSession {
	t.Helper()
	ctx := t.Context()
	t1, t2 := sdk.NewInMemoryTransports()
	if _, err := f.srv.Connect(ctx, t1, nil); err != nil {
		t.Fatal(err)
	}
	opts := &sdk.ClientOptions{}
	if onProgress != nil {
		opts.ProgressNotificationHandler = func(_ context.Context, r *sdk.ProgressNotificationClientRequest) { onProgress(r) }
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, opts).Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call runs a tool and decodes its structured output into out. It returns the
// tool's error text, if the tool failed.
func call(t *testing.T, cs *sdk.ClientSession, name string, args, out any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		var msg strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				msg.WriteString(tc.Text)
			}
		}
		return msg.String()
	}
	if out != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: output %s: %v", name, raw, err)
		}
	}
	return ""
}

func ok(t *testing.T, cs *sdk.ClientSession, name string, args, out any) {
	t.Helper()
	if msg := call(t, cs, name, args, out); msg != "" {
		t.Fatalf("%s failed: %s", name, msg)
	}
}

func TestToolsAreListedWithAnnotations(t *testing.T) {
	cs := newFixture(t).connect(t, nil)
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
		if tool.Description == "" || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("tool %s lacks a description or schema", tool.Name)
		}
	}
	for _, name := range []string{
		"add_download", "list_downloads", "get_download", "wait_for_download", "pause_downloads", "resume_downloads",
		"restart_download", "delete_downloads", "get_stats", "list_queues", "create_queue", "start_queue", "stop_queue",
		"get_settings", "update_settings",
	} {
		if got[name] == nil {
			t.Errorf("tool %s is missing", name)
		}
	}
	if a := got["delete_downloads"].Annotations; a == nil || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("delete_downloads must be marked destructive: %+v", a)
	}
	if a := got["list_downloads"].Annotations; a == nil || !a.ReadOnlyHint {
		t.Errorf("list_downloads must be read-only: %+v", a)
	}
}

func TestDownloadLifecycleTools(t *testing.T) {
	cs := newFixture(t).connect(t, nil)
	var added mcp.TaskInfo
	ok(t, cs, "add_download", map[string]any{"url": "http://127.0.0.1:1/a.bin", "name": "a.bin", "paused": true, "mirrors": []string{"https://m.example/a.bin"}}, &added)
	if added.ID == "" || added.Name != "a.bin" || added.Status != "paused" || added.URL != "http://127.0.0.1:1/a.bin" {
		t.Fatalf("added = %+v", added)
	}
	// The same link twice is refused, like through the API.
	if msg := call(t, cs, "add_download", map[string]any{"url": "http://127.0.0.1:1/a.bin", "paused": true}, nil); !strings.Contains(msg, "duplicate") && !strings.Contains(strings.ToLower(msg), "already") {
		t.Errorf("duplicate add: %q", msg)
	}
	if msg := call(t, cs, "add_download", map[string]any{"url": "not a url"}, nil); msg == "" {
		t.Error("an invalid link was accepted")
	}

	var list struct {
		Tasks      []mcp.TaskInfo `json:"tasks"`
		NextCursor string         `json:"nextCursor"`
	}
	ok(t, cs, "list_downloads", map[string]any{"status": []string{"paused"}, "search": "a.bin"}, &list)
	if len(list.Tasks) != 1 || list.Tasks[0].ID != added.ID {
		t.Fatalf("list = %+v", list)
	}
	ok(t, cs, "list_downloads", map[string]any{"status": []string{"completed"}}, &list)
	if len(list.Tasks) != 0 {
		t.Errorf("completed list = %+v", list)
	}
	if msg := call(t, cs, "list_downloads", map[string]any{"status": []string{"bogus"}}, nil); !strings.Contains(msg, "bogus") {
		t.Errorf("unknown status: %q", msg)
	}

	var got mcp.TaskInfo
	ok(t, cs, "get_download", map[string]any{"id": added.ID}, &got)
	if got.ID != added.ID {
		t.Errorf("get = %+v", got)
	}
	if msg := call(t, cs, "get_download", map[string]any{"id": "nope"}, nil); !strings.Contains(strings.ToLower(msg), "not found") {
		t.Errorf("get missing: %q", msg)
	}

	var bulk mcp.BulkOutput
	ok(t, cs, "resume_downloads", map[string]any{"ids": []string{added.ID, "nope"}}, &bulk)
	if len(bulk.Results) != 2 || !bulk.Results[0].OK || bulk.Results[1].OK || bulk.Results[1].Error == "" {
		t.Fatalf("resume = %+v", bulk)
	}
	ok(t, cs, "pause_downloads", map[string]any{"ids": []string{added.ID}}, &bulk)
	if !bulk.Results[0].OK || bulk.Results[0].Task.Status != "paused" {
		t.Errorf("pause = %+v", bulk)
	}
	if msg := call(t, cs, "restart_download", map[string]any{"id": added.ID}, nil); !strings.Contains(msg, "can be restarted") {
		t.Errorf("restart of a paused download: %q", msg)
	}

	var stats struct {
		Tasks    int            `json:"tasks"`
		ByStatus map[string]int `json:"byStatus"`
	}
	ok(t, cs, "get_stats", nil, &stats)
	if stats.Tasks != 1 {
		t.Errorf("stats = %+v", stats)
	}

	ok(t, cs, "delete_downloads", map[string]any{"ids": []string{added.ID}}, &bulk)
	if !bulk.Results[0].OK {
		t.Errorf("delete = %+v", bulk)
	}
	if msg := call(t, cs, "get_download", map[string]any{"id": added.ID}, nil); msg == "" {
		t.Error("the deleted download is still there")
	}
}

func TestAddDownloadCannotChooseADirectory(t *testing.T) {
	cs := newFixture(t).connect(t, nil)
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "add_download" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		if strings.Contains(string(raw), `"dir"`) {
			t.Errorf("add_download offers a directory: %s", raw)
		}
	}
}

func TestQueueTools(t *testing.T) {
	cs := newFixture(t).connect(t, nil)
	var q mcp.QueueInfo
	ok(t, cs, "create_queue", map[string]any{"name": "night", "stopped": true}, &q)
	if q.Name != "night" || q.State != "stopped" {
		t.Fatalf("created = %+v", q)
	}
	ok(t, cs, "start_queue", map[string]any{"id": q.ID}, &q)
	if q.State != "running" {
		t.Errorf("started = %+v", q)
	}
	ok(t, cs, "stop_queue", map[string]any{"id": q.ID}, &q)
	if q.State != "stopped" {
		t.Errorf("stopped = %+v", q)
	}
	var list struct {
		Queues []mcp.QueueInfo `json:"queues"`
	}
	ok(t, cs, "list_queues", nil, &list)
	if len(list.Queues) != 2 || !slices.ContainsFunc(list.Queues, func(q mcp.QueueInfo) bool { return q.IsDefault }) {
		t.Errorf("queues = %+v", list.Queues)
	}
	if msg := call(t, cs, "create_queue", map[string]any{"name": "night"}, nil); msg == "" {
		t.Error("a duplicate queue name was accepted")
	}
	if msg := call(t, cs, "start_queue", map[string]any{"id": "nope"}, nil); msg == "" {
		t.Error("an unknown queue was started")
	}
}

func TestSettingsToolsOnlyChangeWhatIsAllowed(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(t, nil)
	var out struct {
		Scope    string         `json:"scope"`
		Settings map[string]any `json:"settings"`
	}
	ok(t, cs, "update_settings", map[string]any{"patch": map[string]any{"speedLimit": 1048576, "retry": map[string]any{"maxAttempts": 7}}}, &out)
	if out.Scope != "global" || out.Settings["speedLimit"] != float64(1048576) {
		t.Fatalf("update = %+v", out)
	}
	g, err := f.svc.Settings.Global(t.Context())
	if err != nil || g.SpeedLimit.Value() != 1048576 || g.Retry.MaxAttempts.Value() != 7 {
		t.Errorf("global = %+v, %v", g, err)
	}
	ok(t, cs, "get_settings", map[string]any{}, &out)
	if retry, _ := out.Settings["retry"].(map[string]any); out.Scope != "global" || retry["maxAttempts"] != float64(7) {
		t.Errorf("get global = %+v", out)
	}

	for _, patch := range []map[string]any{
		{"postActions": []any{map[string]any{"kind": "command", "params": map[string]any{"cmd": "/bin/sh"}}}},
		{"proxy": map[string]any{"mode": "none"}},
		{"downloadDir": "/etc"},
		{"headers": map[string]any{"X": "y"}},
		{"speedLimit": 5, "tlsVerify": false},
	} {
		msg := call(t, cs, "update_settings", map[string]any{"patch": patch}, nil)
		if !strings.Contains(msg, "cannot be changed through MCP") {
			t.Errorf("patch %v: %q", patch, msg)
		}
	}
	g, _ = f.svc.Settings.Global(t.Context())
	if g.SpeedLimit.Value() != 1048576 || g.PostActions.IsSet() || g.TLSVerify.IsSet() {
		t.Errorf("a refused patch changed the settings: %+v", g)
	}
	if msg := call(t, cs, "update_settings", map[string]any{"patch": map[string]any{}}, nil); msg == "" {
		t.Error("an empty patch was accepted")
	}
	if msg := call(t, cs, "update_settings", map[string]any{"patch": map[string]any{"maxConnections": -3}}, nil); msg == "" {
		t.Error("an invalid value was accepted")
	}

	task := f.mustTask(t, true)
	ok(t, cs, "get_settings", map[string]any{"taskId": task.ID}, &out)
	if out.Scope != "task" || len(out.Settings) == 0 {
		t.Errorf("task settings = %+v", out)
	}
	ok(t, cs, "get_settings", map[string]any{"queueId": domain.MainQueueID}, &out)
	if out.Scope != "queue" {
		t.Errorf("queue settings = %+v", out)
	}
	if msg := call(t, cs, "get_settings", map[string]any{"taskId": task.ID, "queueId": "main"}, nil); msg == "" {
		t.Error("taskId and queueId together were accepted")
	}
}

func (f *fixture) mustTask(t *testing.T, paused bool) *domain.Task {
	t.Helper()
	task, err := f.svc.Tasks.Create(t.Context(), service.CreateTaskInput{
		Source: domain.Source{CurrentURL: "http://127.0.0.1:1/x.bin"}, Paused: paused, AllowDuplicate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestWaitForDownloadReportsProgressThenTheOutcome(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var progress []float64
	cs := f.connect(t, func(r *sdk.ProgressNotificationClientRequest) {
		mu.Lock()
		progress = append(progress, r.Params.Progress)
		mu.Unlock()
	})
	task := f.mustTask(t, false)

	go func() {
		time.Sleep(100 * time.Millisecond)
		for _, n := range []int64{100, 600} {
			f.bus.Publish(events.Event{Type: events.TypeTaskProgress, TaskID: task.ID, Data: service.ProgressData{Downloaded: n, Size: 1000, Speed: 50}})
			time.Sleep(20 * time.Millisecond)
		}
		fresh, _ := f.store.Tasks().Get(context.Background(), task.ID)
		_ = fresh.TransitionTo(domain.StatusQueued, time.Now())
		_ = fresh.TransitionTo(domain.StatusRunning, time.Now())
		_ = fresh.TransitionTo(domain.StatusPostprocessing, time.Now())
		_ = fresh.TransitionTo(domain.StatusCompleted, time.Now())
		fresh.Downloaded, fresh.Size = 1000, 1000
		_ = f.store.Tasks().Update(context.Background(), fresh)
		f.bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: task.ID})
	}()

	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{
		Name: "wait_for_download", Arguments: map[string]any{"id": task.ID, "timeoutSeconds": 10},
		Meta: sdk.Meta{"progressToken": "tok1"},
	})
	if err != nil || res.IsError {
		t.Fatalf("wait: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out struct {
		Task     mcp.TaskInfo `json:"task"`
		TimedOut bool         `json:"timedOut"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.TimedOut || out.Task.Status != "completed" || out.Task.Downloaded != 1000 {
		t.Errorf("outcome = %+v", out)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(progress)
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	// The bus coalesces progress per task, so 100 and 600 may arrive as one.
	if len(progress) == 0 || progress[len(progress)-1] != 600 {
		t.Errorf("progress notifications = %v, want them to end at 600", progress)
	}
}

func TestWaitForDownloadTimeoutAndImmediateResults(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(t, nil)
	task := f.mustTask(t, true) // paused: already settled
	var out struct {
		Task     mcp.TaskInfo `json:"task"`
		TimedOut bool         `json:"timedOut"`
	}
	start := time.Now()
	ok(t, cs, "wait_for_download", map[string]any{"id": task.ID}, &out)
	if out.TimedOut || out.Task.Status != "paused" || time.Since(start) > 2*time.Second {
		t.Errorf("a settled download must return at once: %+v after %s", out, time.Since(start))
	}
	// A download that never settles times out with its current state.
	fresh, _ := f.store.Tasks().Get(context.Background(), task.ID)
	_ = fresh.TransitionTo(domain.StatusQueued, time.Now())
	if err := f.store.Tasks().Update(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	ok(t, cs, "wait_for_download", map[string]any{"id": task.ID, "timeoutSeconds": 1}, &out)
	if !out.TimedOut || out.Task.Status != "queued" {
		t.Errorf("timeout outcome = %+v", out)
	}
	if msg := call(t, cs, "wait_for_download", map[string]any{"id": "nope"}, nil); msg == "" {
		t.Error("an unknown download was waited for")
	}
}

func TestEndpointNeedsTheToken(t *testing.T) {
	f := newFixture(t)
	cfg := config.Default()
	cfg.Home = t.TempDir()
	const token = "test-token-0123456789"
	srv, err := daemon.BuildServer(&cfg, api.Deps{
		Token: token, Version: "test", Settings: f.svc.Settings, Queues: f.svc.Queues, Tasks: f.svc.Tasks,
		MCP: mcp.Handler(f.srv),
	})
	if err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	endpoint := "http://" + ln.Addr().String() + api.MCPPath

	resp, err := http.Post(endpoint, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)) //nolint:noctx // a test against a local server
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a token: status %d, want 401", resp.StatusCode)
	}

	hc := &http.Client{Transport: bearerTransport{token: token}}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).
		Connect(t.Context(), &sdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	var q struct {
		Queues []mcp.QueueInfo `json:"queues"`
	}
	ok(t, cs, "list_queues", nil, &q)
	if len(q.Queues) != 1 {
		t.Errorf("queues over HTTP = %+v", q)
	}
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r) //nolint:wrapcheck // test transport
}
