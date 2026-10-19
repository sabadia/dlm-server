package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/plugin"
	"github.com/sabadia/dlm-server/internal/refresh"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
	"github.com/sabadia/dlm-server/internal/webhook"
)

// opsFixture is an API server backed by real services on a temp SQLite
// store, plus a local origin server.
type opsFixture struct {
	s      *Server
	origin *httptest.Server
	dir    string
	store  storage.Store
	// browser is the fake headless browser behind the /v1/browser operations.
	browser *fakeBrowser
}

var fileContent = bytes.Repeat([]byte("0123456789abcdef"), 64) // 1 KiB

func newOpsFixture(t *testing.T) *opsFixture {
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
	dir := t.TempDir()
	defaults := settings.Defaults(dir)
	defaults.Proxy = settings.Some(settings.Proxy{Mode: settings.ProxyNone})
	resolves := engine.NewResolveCache(engine.CacheOptions{})
	t.Cleanup(resolves.Close)
	d := service.Deps{
		Store: store, Engines: reg, Resolves: resolves,
		Defaults: func() settings.Settings { return defaults },
	}
	plugins := plugin.NewManager(plugin.Options{Dir: filepath.Join(t.TempDir(), "plugins"), Store: store})
	d.Plugins = plugins
	cipher, err := cookies.NewCipher(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	jar := cookies.NewStore(store.Cookies(), cipher, cookies.Options{})
	tasks := service.NewTaskService(d)
	refreshSvc := service.NewRefreshService(d, refresh.OriginRedirect{Engines: reg})
	fb := &fakeBrowser{}
	cfg := config.Default()
	cfg.Server.AllowedOrigins = []string{testOrigin}
	s, err := New(&cfg, Deps{
		Token: testToken, Version: testVersion,
		Settings: service.NewSettingsService(d), Queues: service.NewQueueService(d), Tasks: tasks,
		Cookies: service.NewCookieService(jar), Capture: service.NewCaptureService(tasks, jar, refreshSvc), Refresh: refreshSvc,
		Browser: service.NewBrowserService(fb, tasks), Categories: service.NewCategoryService(d),
		Webhooks: service.NewWebhookService(d, cipher, webhook.New(nil, nil, webhook.Options{})), Credentials: service.NewCredentialService(d, cipher),
		Plugins: service.NewPluginService(plugins),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/file.bin", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Unix(1_700_000_000, 0), bytes.NewReader(fileContent))
	})
	mux.HandleFunc("/broken", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	origin := httptest.NewServer(mux)
	t.Cleanup(origin.Close)
	return &opsFixture{s: s, origin: origin, dir: dir, store: store, browser: fb}
}

// call sends an authenticated JSON request and decodes a 2xx body into out.
func (f *opsFixture) call(t *testing.T, method, path, body string, wantStatus int, out any) {
	t.Helper()
	callServer(t, f.s, method, path, body, "", wantStatus, out)
}

func callServer(t *testing.T, s *Server, method, path, body, contentType string, wantStatus int, out any) {
	t.Helper()
	headers := bearer(testToken)
	if contentType != "" {
		headers["Content-Type"] = contentType
	}
	w := do(t, s, request{method: method, path: path, body: body, headers: headers})
	if w.Code != wantStatus {
		t.Fatalf("%s %s: status = %d, want %d; body: %s", method, path, w.Code, wantStatus, w.Body)
	}
	if out != nil {
		reflect.ValueOf(out).Elem().SetZero() // absent fields must not keep old values
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode: %v; body: %s", method, path, err, w.Body)
		}
	}
}

type problemBody struct {
	Status int                 `json:"status"`
	Title  string              `json:"title"`
	Detail string              `json:"detail"`
	Errors []*huma.ErrorDetail `json:"errors"`
}

func decodeProblem(t *testing.T, w *httptest.ResponseRecorder, status int) problemBody {
	t.Helper()
	assertProblem(t, w, status)
	var p problemBody
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *opsFixture) createTask(t *testing.T, body string) *domain.Task {
	t.Helper()
	var task domain.Task
	// Many tests make several tasks for one link.
	body = strings.Replace(strings.TrimSpace(body), "{", `{"allowDuplicate":true,`, 1)
	f.call(t, http.MethodPost, "/v1/tasks", body, http.StatusCreated, &task)
	return &task
}

func (f *opsFixture) createQueue(t *testing.T, name string) *domain.Queue {
	t.Helper()
	var q domain.Queue
	f.call(t, http.MethodPost, "/v1/queues", `{"name":"`+name+`"}`, http.StatusCreated, &q)
	return &q
}

func (f *opsFixture) fileURL() string { return f.origin.URL + "/file.bin" }

func TestSettingsSchemaComplete(t *testing.T) {
	s := newTestServer(t)
	reg := s.API().OpenAPI().Components.Schemas
	var walk func(prefix string, sch *huma.Schema)
	walk = func(prefix string, sch *huma.Schema) {
		for name, p := range sch.Properties {
			if name == "$schema" {
				continue
			}
			path := prefix + name
			if p.Ref != "" {
				walk(path+".", reg.SchemaFromRef(p.Ref))
				continue
			}
			if p.Type == "" || !p.Nullable {
				t.Errorf("setting %s: schema type %q nullable %v; add an alias in registerSettingsSchemas", path, p.Type, p.Nullable)
			}
		}
	}
	root := reg.Map()["Settings"]
	if root == nil {
		t.Fatal("no Settings schema")
	}
	walk("", root)
	if got := len(settings.SetPaths(settings.Defaults("x"))); got == 0 {
		t.Fatal("no default settings")
	}
}

func TestServiceOperationsUnavailableWithoutServices(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/resolve", `{"url":"http://x/y"}`},
		{http.MethodPost, "/v1/tasks", `{"url":"http://x/y"}`},
		{http.MethodPost, "/v1/tasks/batch", `{"items":[{"url":"http://x/y"}]}`},
		{http.MethodGet, "/v1/tasks", ""},
		{http.MethodGet, "/v1/tasks/x", ""},
		{http.MethodPatch, "/v1/tasks/x", `{"name":"a"}`},
		{http.MethodPost, "/v1/tasks/x/move", `{"queueId":"main"}`},
		{http.MethodPost, "/v1/tasks/x/reorder", `{"position":0}`},
		{http.MethodPost, "/v1/tasks/pause", `{"ids":["x"]}`},
		{http.MethodPost, "/v1/tasks/resume", `{"ids":["x"]}`},
		{http.MethodPost, "/v1/tasks/delete", `{"ids":["x"]}`},
		{http.MethodPost, "/v1/tasks/x/restart", ""},
		{http.MethodDelete, "/v1/tasks/x", ""},
		{http.MethodGet, "/v1/tasks/x/settings/effective", ""},
		{http.MethodGet, "/v1/queues", ""},
		{http.MethodPost, "/v1/queues", `{"name":"q"}`},
		{http.MethodGet, "/v1/queues/x", ""},
		{http.MethodPatch, "/v1/queues/x", `{"name":"q"}`},
		{http.MethodPut, "/v1/queues/x/settings", `{}`},
		{http.MethodDelete, "/v1/queues/x", ""},
		{http.MethodPost, "/v1/queues/x/start", ""},
		{http.MethodPost, "/v1/queues/x/stop", ""},
		{http.MethodPost, "/v1/queues/x/reorder", `{"position":0}`},
		{http.MethodGet, "/v1/queues/x/settings/effective", ""},
		{http.MethodGet, "/v1/settings", ""},
		{http.MethodPut, "/v1/settings", `{}`},
		{http.MethodPatch, "/v1/settings", `{}`},
		{http.MethodGet, "/v1/settings/defaults", ""},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := do(t, s, request{method: tc.method, path: tc.path, body: tc.body, headers: bearer(testToken)})
			assertProblem(t, w, http.StatusServiceUnavailable)
		})
	}
}

func TestTaskLifecycle(t *testing.T) {
	f := newOpsFixture(t)
	task := f.createTask(t, `{"url":"`+f.fileURL()+`","name":"a.bin","priority":"high","labels":{"k":"v"}}`)
	if task.Status != domain.StatusQueued || task.QueueID != domain.MainQueueID || task.Priority != domain.PriorityHigh ||
		task.Name != "a.bin" || task.Labels["k"] != "v" || task.Source.CurrentURL != f.fileURL() {
		t.Fatalf("created task = %+v", task)
	}

	var got domain.Task
	f.call(t, http.MethodGet, "/v1/tasks/"+task.ID, "", http.StatusOK, &got)
	if got.ID != task.ID {
		t.Fatalf("get = %+v", got)
	}

	// Merge patch through HTTP: set, then null unsets only that field.
	f.call(t, http.MethodPatch, "/v1/tasks/"+task.ID,
		`{"priority":"low","settings":{"speedLimit":1024,"retry":{"maxAttempts":2}}}`, http.StatusOK, &got)
	if got.Priority != domain.PriorityLow || got.Settings.SpeedLimit.Value() != 1024 || got.Settings.Retry.MaxAttempts.Value() != 2 {
		t.Fatalf("after patch: %+v", got)
	}
	callServer(t, f.s, http.MethodPatch, "/v1/tasks/"+task.ID, `{"settings":{"speedLimit":null}}`,
		mergePatchContentType, http.StatusOK, &got)
	if got.Settings.SpeedLimit.IsSet() || got.Settings.Retry.MaxAttempts.Value() != 2 {
		t.Fatalf("after null patch: speedLimit set=%v retry=%v", got.Settings.SpeedLimit.IsSet(), got.Settings.Retry.MaxAttempts)
	}
	// settings: null is rejected (use field nulls).
	assertProblem(t, do(t, f.s, request{
		method: http.MethodPatch, path: "/v1/tasks/" + task.ID, body: `{"settings":null}`, headers: bearer(testToken),
	}), http.StatusUnprocessableEntity)

	var eff settings.Effective
	f.call(t, http.MethodGet, "/v1/tasks/"+task.ID+"/settings/effective", "", http.StatusOK, &eff)
	if eff.Sources["retry.maxAttempts"] != settings.LevelTask || eff.Sources["maxConnections"] != settings.LevelDefault {
		t.Fatalf("sources = %v", eff.Sources)
	}

	q := f.createQueue(t, "other")
	f.call(t, http.MethodPost, "/v1/tasks/"+task.ID+"/move", `{"queueId":"`+q.ID+`"}`, http.StatusOK, &got)
	if got.QueueID != q.ID {
		t.Fatalf("moved to %q", got.QueueID)
	}
	second := f.createTask(t, `{"url":"`+f.fileURL()+`","queueId":"`+q.ID+`"}`)
	f.call(t, http.MethodPost, "/v1/tasks/"+second.ID+"/reorder", `{"position":0}`, http.StatusOK, &got)
	if got.Position != 0 {
		t.Fatalf("reordered position = %d", got.Position)
	}

	var res struct {
		Results []struct {
			ID    string       `json:"id"`
			OK    bool         `json:"ok"`
			Task  *domain.Task `json:"task"`
			Error *problemBody `json:"error"`
		} `json:"results"`
	}
	f.call(t, http.MethodPost, "/v1/tasks/pause", `{"ids":["`+task.ID+`","missing"]}`, http.StatusOK, &res)
	if len(res.Results) != 2 || !res.Results[0].OK || res.Results[0].Task.Status != domain.StatusPaused ||
		res.Results[1].OK || res.Results[1].Error == nil || res.Results[1].Error.Status != http.StatusNotFound {
		t.Fatalf("pause results = %+v", res.Results)
	}
	f.call(t, http.MethodPost, "/v1/tasks/resume", `{"filter":{"status":["paused"]}}`, http.StatusOK, &res)
	if len(res.Results) != 1 || res.Results[0].ID != task.ID || res.Results[0].Task.Status != domain.StatusQueued {
		t.Fatalf("resume results = %+v", res.Results)
	}

	// Restarting a queued task is a conflict.
	p := decodeProblem(t, do(t, f.s, request{method: http.MethodPost, path: "/v1/tasks/" + task.ID + "/restart", headers: bearer(testToken)}),
		http.StatusConflict)
	if !strings.Contains(p.Detail, "restart") {
		t.Errorf("detail = %q", p.Detail)
	}

	f.call(t, http.MethodDelete, "/v1/tasks/"+task.ID+"?files=true", "", http.StatusNoContent, nil)
	decodeProblem(t, do(t, f.s, request{method: http.MethodGet, path: "/v1/tasks/" + task.ID, headers: bearer(testToken)}),
		http.StatusNotFound)
	decodeProblem(t, do(t, f.s, request{method: http.MethodDelete, path: "/v1/tasks/" + task.ID, headers: bearer(testToken)}),
		http.StatusNotFound)
}

func TestResolveAndCreateFromResolveID(t *testing.T) {
	f := newOpsFixture(t)
	var res service.ResolveResult
	f.call(t, http.MethodPost, "/v1/resolve", `{"url":"`+f.fileURL()+`"}`, http.StatusOK, &res)
	if res.ResolveID == "" || res.Resource.Size != int64(len(fileContent)) || res.Name != "file.bin" {
		t.Fatalf("resolve = %+v", res)
	}
	task := f.createTask(t, `{"resolveId":"`+res.ResolveID+`"}`)
	if task.Name != "file.bin" || task.Size != int64(len(fileContent)) {
		t.Fatalf("task = %+v", task)
	}
	// A resolve ID is single-use.
	decodeProblem(t, do(t, f.s, request{
		method: http.MethodPost, path: "/v1/tasks", body: `{"resolveId":"` + res.ResolveID + `"}`,
		headers: bearer(testToken),
	}), http.StatusNotFound)
}

func TestResolveOriginFailureIsBadGateway(t *testing.T) {
	f := newOpsFixture(t)
	w := do(t, f.s, request{
		method: http.MethodPost, path: "/v1/resolve", body: `{"url":"` + f.origin.URL + `/broken"}`,
		headers: bearer(testToken),
	})
	p := decodeProblem(t, w, http.StatusBadGateway)
	if !strings.Contains(p.Detail, "["+domain.ErrorCodeHTTPStatus+"]") {
		t.Errorf("detail = %q, want the engine error code", p.Detail)
	}
}

func TestErrorMapping(t *testing.T) {
	f := newOpsFixture(t)
	auth := bearer(testToken)

	t.Run("404 unknown queue", func(t *testing.T) {
		decodeProblem(t, do(t, f.s, request{method: http.MethodGet, path: "/v1/queues/nope", headers: auth}), http.StatusNotFound)
		decodeProblem(t, do(t, f.s, request{
			method: http.MethodPost, path: "/v1/tasks",
			body: `{"url":"` + f.fileURL() + `","queueId":"nope"}`, headers: auth,
		}), http.StatusNotFound)
	})
	t.Run("409 duplicate queue name and default queue", func(t *testing.T) {
		f.createQueue(t, "dup")
		decodeProblem(t, do(t, f.s, request{method: http.MethodPost, path: "/v1/queues", body: `{"name":"DUP"}`, headers: auth}),
			http.StatusConflict)
		decodeProblem(t, do(t, f.s, request{method: http.MethodDelete, path: "/v1/queues/main", headers: auth}), http.StatusConflict)
	})
	t.Run("422 settings field errors", func(t *testing.T) {
		p := decodeProblem(t, do(t, f.s, request{
			method: http.MethodPost, path: "/v1/tasks",
			body: `{"url":"` + f.fileURL() + `","settings":{"maxConcurrentDownloads":2,"maxConnections":0}}`, headers: auth,
		}),
			http.StatusUnprocessableEntity)
		locs := errorLocations(p)
		for _, want := range []string{"body.settings.maxConcurrentDownloads", "body.settings.maxConnections"} {
			if !slices.Contains(locs, want) {
				t.Errorf("locations %v lack %q", locs, want)
			}
		}
		p = decodeProblem(t, do(t, f.s, request{
			method: http.MethodPut, path: "/v1/settings",
			body: `{"checksum":{"algorithm":"md5","expected":"00"}}`, headers: auth,
		}), http.StatusUnprocessableEntity)
		if locs := errorLocations(p); !slices.Contains(locs, "body.checksum") {
			t.Errorf("locations = %v", locs)
		}
	})
	t.Run("422 input validation", func(t *testing.T) {
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodPost, "/v1/tasks", `{"url":"` + f.fileURL() + `","source":{"currentUrl":"` + f.fileURL() + `"}}`},
			{http.MethodPost, "/v1/tasks", `{"name":"x"}`},
			{http.MethodPost, "/v1/tasks", `{"url":"not a url"}`},
			{http.MethodPost, "/v1/tasks", `{"url":"` + f.fileURL() + `","priority":"urgent"}`},
			{http.MethodGet, "/v1/tasks?limit=501", ""},
			{http.MethodGet, "/v1/tasks?status=bogus", ""},
			{http.MethodGet, "/v1/tasks?cursor=garbage", ""},
			{http.MethodPost, "/v1/tasks/pause", `{}`},
			{http.MethodPost, "/v1/tasks/pause", `{"ids":["a"],"filter":{}}`},
			{http.MethodPatch, "/v1/settings", `{"noSuchSetting":1}`},
			{http.MethodPatch, "/v1/settings", `[1]`},
			{http.MethodPost, "/v1/queues/main/reorder", `{"position":-1}`},
		} {
			w := do(t, f.s, request{method: tc.method, path: tc.path, body: tc.body, headers: auth})
			if w.Code != http.StatusUnprocessableEntity && w.Code != http.StatusBadRequest {
				t.Errorf("%s %s %s: status %d, want 422; body: %s", tc.method, tc.path, tc.body, w.Code, w.Body)
			}
		}
	})
	t.Run("internal errors are not leaked", func(t *testing.T) {
		p := f.s.problemFor(context.Background(), "op", errTest("secret detail"), locBody)
		if p.Status != http.StatusInternalServerError || strings.Contains(p.Detail, "secret") {
			t.Errorf("problem = %+v", p)
		}
		p = f.s.problemFor(context.Background(), "op", context.Canceled, locBody)
		if p.Status != http.StatusServiceUnavailable {
			t.Errorf("canceled: %+v", p)
		}
	})
}

type errTest string

func (e errTest) Error() string { return string(e) }

func errorLocations(p problemBody) []string {
	out := make([]string, 0, len(p.Errors))
	for _, e := range p.Errors {
		out = append(out, e.Location)
	}
	return out
}

func TestCreateTasksBatch(t *testing.T) {
	f := newOpsFixture(t)
	var res struct {
		Results []struct {
			ID    string       `json:"id"`
			OK    bool         `json:"ok"`
			Task  *domain.Task `json:"task"`
			Error *problemBody `json:"error"`
		} `json:"results"`
	}
	f.call(t, http.MethodPost, "/v1/tasks/batch",
		`{"items":[{"url":"`+f.fileURL()+`"},{"url":"`+f.fileURL()+`","source":{"currentUrl":"http://x/"}},{"url":"`+f.fileURL()+`","queueId":"nope"}]}`,
		http.StatusOK, &res)
	if len(res.Results) != 3 || !res.Results[0].OK || res.Results[0].ID != res.Results[0].Task.ID ||
		res.Results[1].OK || res.Results[1].Error.Status != http.StatusUnprocessableEntity ||
		res.Results[2].OK || res.Results[2].Error.Status != http.StatusNotFound {
		t.Fatalf("results = %+v", res.Results)
	}
}

func TestListTasksPaginationAndFilters(t *testing.T) {
	f := newOpsFixture(t)
	q := f.createQueue(t, "q")
	want := map[string]bool{}
	for range 5 {
		want[f.createTask(t, `{"url":"`+f.fileURL()+`","queueId":"`+q.ID+`"}`).ID] = true
	}
	f.createTask(t, `{"url":"`+f.fileURL()+`","paused":true}`) // other queue

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		var page TaskList
		path := "/v1/tasks?limit=2&queueId=" + q.ID
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		f.call(t, http.MethodGet, path, "", http.StatusOK, &page)
		pages++
		for _, task := range page.Tasks {
			if seen[task.ID] || !want[task.ID] {
				t.Fatalf("unexpected or duplicate task %s", task.ID)
			}
			seen[task.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("saw %d tasks in %d pages", len(seen), pages)
	}

	var page TaskList
	f.call(t, http.MethodGet, "/v1/tasks?status=paused&sort=created", "", http.StatusOK, &page)
	if len(page.Tasks) != 1 || page.Tasks[0].Status != domain.StatusPaused {
		t.Fatalf("paused filter = %+v", page.Tasks)
	}
	f.call(t, http.MethodGet, "/v1/tasks?status=queued,paused", "", http.StatusOK, &page)
	if len(page.Tasks) != 6 {
		t.Fatalf("multi-status filter returned %d tasks", len(page.Tasks))
	}
}

func TestBulkDeleteWithFilter(t *testing.T) {
	f := newOpsFixture(t)
	q := f.createQueue(t, "q")
	for range 3 {
		f.createTask(t, `{"url":"`+f.fileURL()+`","queueId":"`+q.ID+`"}`)
	}
	keep := f.createTask(t, `{"url":"`+f.fileURL()+`"}`)
	var res struct {
		Results []BatchItem `json:"results"`
	}
	f.call(t, http.MethodPost, "/v1/tasks/delete", `{"filter":{"queueId":"`+q.ID+`"},"files":true}`, http.StatusOK, &res)
	if len(res.Results) != 3 {
		t.Fatalf("deleted %d tasks", len(res.Results))
	}
	for _, r := range res.Results {
		if !r.OK {
			t.Errorf("item %+v failed", r)
		}
	}
	var page TaskList
	f.call(t, http.MethodGet, "/v1/tasks", "", http.StatusOK, &page)
	if len(page.Tasks) != 1 || page.Tasks[0].ID != keep.ID {
		t.Fatalf("remaining = %+v", page.Tasks)
	}
}

func TestQueueOperations(t *testing.T) {
	f := newOpsFixture(t)
	var q domain.Queue
	f.call(t, http.MethodPost, "/v1/queues", `{"name":"fast","settings":{"maxConcurrentDownloads":2},"state":"stopped"}`,
		http.StatusCreated, &q)
	if q.State != domain.QueueStopped || q.Settings.MaxConcurrentDownloads.Value() != 2 || q.Position != 1 {
		t.Fatalf("created = %+v", q)
	}
	var list QueueList
	f.call(t, http.MethodGet, "/v1/queues", "", http.StatusOK, &list)
	if len(list.Queues) != 2 || list.Queues[0].ID != domain.MainQueueID {
		t.Fatalf("list = %+v", list.Queues)
	}
	f.call(t, http.MethodPatch, "/v1/queues/"+q.ID, `{"name":"faster","settings":{"speedLimit":100,"maxConcurrentDownloads":null}}`,
		http.StatusOK, &q)
	if q.Name != "faster" || q.Settings.MaxConcurrentDownloads.IsSet() || q.Settings.SpeedLimit.Value() != 100 {
		t.Fatalf("updated = %+v", q)
	}
	f.call(t, http.MethodPut, "/v1/queues/"+q.ID+"/settings", `{"maxConnections":4}`, http.StatusOK, &q)
	if q.Settings.SpeedLimit.IsSet() || q.Settings.MaxConnections.Value() != 4 {
		t.Fatalf("replaced = %+v", q.Settings)
	}
	f.call(t, http.MethodPost, "/v1/queues/"+q.ID+"/start", "", http.StatusOK, &q)
	if q.State != domain.QueueRunning {
		t.Fatalf("start: %s", q.State)
	}
	f.call(t, http.MethodPost, "/v1/queues/"+q.ID+"/stop", "", http.StatusOK, &q)
	if q.State != domain.QueueStopped {
		t.Fatalf("stop: %s", q.State)
	}
	f.call(t, http.MethodPost, "/v1/queues/"+q.ID+"/reorder", `{"position":0}`, http.StatusOK, &q)
	if q.Position != 0 {
		t.Fatalf("reorder: %d", q.Position)
	}
	var get domain.Queue
	f.call(t, http.MethodGet, "/v1/queues/"+q.ID, "", http.StatusOK, &get)
	if get.Position != 0 {
		t.Fatalf("get after reorder: %+v", get)
	}
	var eff settings.Effective
	f.call(t, http.MethodGet, "/v1/queues/"+q.ID+"/settings/effective", "", http.StatusOK, &eff)
	if eff.Sources["maxConnections"] != settings.LevelQueue || eff.Settings.MaxConnections.Value() != 4 {
		t.Fatalf("effective = %+v", eff)
	}

	task := f.createTask(t, `{"url":"`+f.fileURL()+`","queueId":"`+q.ID+`"}`)
	decodeProblem(t, do(t, f.s, request{method: http.MethodDelete, path: "/v1/queues/" + q.ID, headers: bearer(testToken)}),
		http.StatusConflict)
	f.call(t, http.MethodDelete, "/v1/queues/"+q.ID+"?moveTasksTo=main", "", http.StatusNoContent, nil)
	var moved domain.Task
	f.call(t, http.MethodGet, "/v1/tasks/"+task.ID, "", http.StatusOK, &moved)
	if moved.QueueID != domain.MainQueueID {
		t.Fatalf("task queue = %q", moved.QueueID)
	}
}

func TestSettingsOperations(t *testing.T) {
	f := newOpsFixture(t)
	var g settings.Settings
	f.call(t, http.MethodGet, "/v1/settings", "", http.StatusOK, &g)
	if len(settings.SetPaths(g)) != 0 {
		t.Fatalf("initial global = %v", settings.SetPaths(g))
	}
	callServer(t, f.s, http.MethodPatch, "/v1/settings", `{"maxConnections":8,"retry":{"maxAttempts":3}}`,
		mergePatchContentType, http.StatusOK, &g)
	if g.MaxConnections.Value() != 8 || g.Retry.MaxAttempts.Value() != 3 {
		t.Fatalf("patched = %v", settings.SetPaths(g))
	}
	f.call(t, http.MethodPatch, "/v1/settings", `{"retry":{"maxAttempts":null}}`, http.StatusOK, &g)
	if g.Retry.MaxAttempts.IsSet() || g.MaxConnections.Value() != 8 {
		t.Fatalf("after null = %v", settings.SetPaths(g))
	}
	f.call(t, http.MethodGet, "/v1/settings", "", http.StatusOK, &g)
	if got := settings.SetPaths(g); !slices.Equal(got, []string{"maxConnections"}) {
		t.Fatalf("stored = %v", got)
	}
	f.call(t, http.MethodPut, "/v1/settings", `{"userAgent":"ua","proxy":null}`, http.StatusOK, &g)
	if got := settings.SetPaths(g); !slices.Equal(got, []string{"userAgent"}) {
		t.Fatalf("replaced = %v", got)
	}
	var defs settings.Settings
	f.call(t, http.MethodGet, "/v1/settings/defaults", "", http.StatusOK, &defs)
	if defs.DownloadDir.Value() != f.dir || defs.MaxConnections.Value() != 16 {
		t.Fatalf("defaults = %+v", defs)
	}
}

func TestMergePatchMediaTypeDocumented(t *testing.T) {
	s := newTestServer(t)
	paths := s.API().OpenAPI().Paths
	for _, p := range []string{"/v1/settings", "/v1/tasks/{id}", "/v1/queues/{id}"} {
		if paths[p].Patch.RequestBody.Content[mergePatchContentType] == nil {
			t.Errorf("PATCH %s does not document %s", p, mergePatchContentType)
		}
	}
}
