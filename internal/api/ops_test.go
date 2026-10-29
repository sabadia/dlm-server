package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"github.com/sabadia/dlm-server/internal/ratelimit"
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
	limitsDeps := d
	limitsDeps.Limits = ratelimit.NewManager(0)
	cfg := config.Default()
	cfg.Server.AllowedOrigins = []string{testOrigin}
	s, err := New(&cfg, Deps{
		Token: testToken, Version: testVersion,
		Settings: service.NewSettingsService(d), Queues: service.NewQueueService(d), Tasks: tasks,
		Cookies: service.NewCookieService(jar), Capture: service.NewCaptureService(tasks, jar, refreshSvc), Refresh: refreshSvc,
		Browser: service.NewBrowserService(fb, tasks), Categories: service.NewCategoryService(d),
		Webhooks: service.NewWebhookService(d, cipher, webhook.New(nil, nil, webhook.Options{})), Credentials: service.NewCredentialService(d, cipher),
		Plugins: service.NewPluginService(plugins),
		Limits:  service.NewLimitsService(limitsDeps, nil),
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
				ref := reg.SchemaFromRef(p.Ref)
				// A referenced object without properties is the schema huma
				// makes of an Optional[T] that has no alias: it rejects every
				// real value (dlm-server#128).
				if ref.Type == huma.TypeObject && len(ref.Properties) == 0 && ref.AdditionalProperties != true {
					t.Errorf("setting %s: %s is an empty object; add an alias in registerSettingsSchemas", path, p.Ref)
					continue
				}
				walk(path+".", ref)
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

// A queue accepts the settings the app and CLI send: the three values that
// were once refused because their schema was empty (dlm-server#128).
func TestQueueSettingsAcceptQuotaProfilesAndAction(t *testing.T) {
	f := newOpsFixture(t)
	q := f.createQueue(t, "night")
	// The daemon wants an absolute path, which looks different per platform.
	program, err := json.Marshal(filepath.Join(t.TempDir(), "notify"))
	if err != nil {
		t.Fatal(err)
	}
	body := `{
		"speedLimit": 2000000,
		"torrent": {"seedRatio": 1.5, "trackers": ["udp://tracker.example:80"]},
		"schedule": {"windows": [{"days": ["mon"], "start": "22:00", "end": "00:00"}]},
		"speedProfiles": [{"windows": [{"days": ["mon"], "start": "10:00", "end": "12:00"}], "speedLimit": 5}],
		"quota": {"bytes": 1000, "period": "24h0m0s"},
		"onQueueComplete": {"action": "command", "command": ` + string(program) + `, "args": ["a"], "delay": "2m0s"}
	}`
	var got domain.Queue
	f.call(t, http.MethodPut, "/v1/queues/"+q.ID+"/settings", body, http.StatusOK, &got)
	if v, ok := got.Settings.Quota.Get(); !ok || v.Bytes != 1000 {
		t.Errorf("quota = %+v (set %v)", v, ok)
	}
	if v, ok := got.Settings.OnQueueComplete.Get(); !ok || v.Action != settings.QueueActionCommand {
		t.Errorf("onQueueComplete = %+v (set %v)", v, ok)
	}
	if v, ok := got.Settings.SpeedProfiles.Get(); !ok || len(v) != 1 || v[0].SpeedLimit != 5 {
		t.Errorf("speedProfiles = %+v (set %v)", v, ok)
	}

	// Null unsets each of them again, as the app does when a switch is off.
	f.call(t, http.MethodPut, "/v1/queues/"+q.ID+"/settings",
		`{"quota": null, "onQueueComplete": null, "speedProfiles": null}`, http.StatusOK, &got)
	if got.Settings.Quota.IsSet() || got.Settings.OnQueueComplete.IsSet() || got.Settings.SpeedProfiles.IsSet() {
		t.Errorf("settings still set after null: %+v", got.Settings)
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

// The task detail operations: segments, the choice of files and the torrent
// description.
func TestTaskDetailOperations(t *testing.T) {
	f := newOpsFixture(t)
	task := f.createTask(t, `{"url":"`+f.fileURL()+`","paused":true}`)

	var segs struct {
		Segments []domain.Segment `json:"segments"`
	}
	f.call(t, http.MethodGet, "/v1/tasks/"+task.ID+"/segments", "", http.StatusOK, &segs)
	if segs.Segments == nil || len(segs.Segments) != 0 {
		t.Errorf("a task without segments lists []: %#v", segs.Segments)
	}
	f.call(t, http.MethodGet, "/v1/tasks/nope/segments", "", http.StatusNotFound, nil)

	// No file list yet.
	f.call(t, http.MethodPatch, "/v1/tasks/"+task.ID+"/files", `{"selected":[0]}`, http.StatusUnprocessableEntity, nil)

	stored, err := f.store.Tasks().Get(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Resource = &domain.Resource{Name: "pack", Size: 30, Files: []domain.File{
		{Index: 0, Name: "a", Size: 10, Selected: true}, {Index: 1, Name: "b", Size: 20, Selected: true},
	}}
	stored.Size = 30
	if err := f.store.Tasks().Update(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	var got domain.Task
	f.call(t, http.MethodPatch, "/v1/tasks/"+task.ID+"/files", `{"selected":[1]}`, http.StatusOK, &got)
	if got.Size != 20 || got.Resource.Files[0].Selected || !got.Resource.Files[1].Selected {
		t.Errorf("selection not applied: size %d, files %+v", got.Size, got.Resource.Files)
	}
	f.call(t, http.MethodPatch, "/v1/tasks/"+task.ID+"/files", `{"selected":[9]}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPatch, "/v1/tasks/"+task.ID+"/files", `{"selected":[]}`, http.StatusUnprocessableEntity, nil)

	// Not a torrent.
	f.call(t, http.MethodGet, "/v1/tasks/"+task.ID+"/torrent", "", http.StatusNotFound, nil)
}

// The listing filters, sorts and total, the per-queue counts and the bulk
// operations (P13.5).
func TestListPowerAndBulkOperations(t *testing.T) {
	f := newOpsFixture(t)
	mk := func(name, labels string) *domain.Task {
		return f.createTask(t, `{"url":"`+f.fileURL()+`","name":"`+name+`","paused":true,"labels":`+labels+`}`)
	}
	a := mk("alpha.iso", `{"project":"os"}`)
	b := mk("Bravo.zip", `{"project":"docs","owner":"ann"}`)
	c := mk("charlie.bin", `{}`)

	var cat domain.Category
	f.call(t, http.MethodPost, "/v1/categories", `{"name":"Archives","dir":"Archives","extensions":["zip","iso"]}`, http.StatusCreated, &cat)

	list := func(query string) (ids []string, total *int64) {
		var out struct {
			Tasks []*domain.Task `json:"tasks"`
			Total *int64         `json:"total"`
		}
		f.call(t, http.MethodGet, "/v1/tasks?"+query, "", http.StatusOK, &out)
		for _, tk := range out.Tasks {
			ids = append(ids, tk.Name)
		}
		return ids, out.Total
	}
	eq := func(what string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	got, _ := list("label=project")
	eq("label key", got, "alpha.iso", "Bravo.zip")
	got, _ = list("label=owner%3Dann")
	eq("label key=value", got, "Bravo.zip")
	got, _ = list("category=archives")
	eq("category by name", got, "alpha.iso", "Bravo.zip")
	got, _ = list("category=" + cat.ID + "&sort=name&order=desc")
	eq("category by id, name desc", got, "Bravo.zip", "alpha.iso")
	got, total := list("sort=name&limit=2&total=true")
	eq("name sort page", got, "alpha.iso", "Bravo.zip")
	if total == nil || *total != 3 {
		t.Errorf("total = %v, want 3", total)
	}
	if _, total = list("limit=2"); total != nil {
		t.Errorf("total = %d without total=true", *total)
	}
	f.call(t, http.MethodGet, "/v1/tasks?category=nope", "", http.StatusNotFound, nil)
	f.call(t, http.MethodGet, "/v1/tasks?order=desc", "", http.StatusUnprocessableEntity, nil)

	// Queue counts.
	var queues struct {
		Counts map[string]map[string]int `json:"counts"`
	}
	f.call(t, http.MethodGet, "/v1/queues", "", http.StatusOK, &queues)
	if queues.Counts["main"]["paused"] != 3 {
		t.Errorf("counts = %v, want 3 paused in main", queues.Counts)
	}

	// Bulk update: priority, labels added and removed, settings patch.
	var batch struct {
		Results []struct {
			OK   bool         `json:"ok"`
			Task *domain.Task `json:"task"`
		} `json:"results"`
	}
	body := `{"ids":["` + a.ID + `","` + b.ID + `"],"priority":"high","labels":{"team":"x"},"removeLabels":["project"],"settings":{"speedLimit":1000}}`
	f.call(t, http.MethodPost, "/v1/tasks/update", body, http.StatusOK, &batch)
	if len(batch.Results) != 2 || !batch.Results[0].OK || !batch.Results[1].OK {
		t.Fatalf("update results = %+v", batch.Results)
	}
	got1 := batch.Results[1].Task
	if got1.Priority != domain.PriorityHigh || got1.Labels["team"] != "x" || got1.Labels["owner"] != "ann" || got1.Labels["project"] != "" {
		t.Errorf("task after update = priority %v labels %v", got1.Priority, got1.Labels)
	}
	if v, ok := got1.Settings.SpeedLimit.Get(); !ok || v != 1000 {
		t.Errorf("settings patch not applied: %+v", got1.Settings.SpeedLimit)
	}

	// Bulk move by filter; one bad queue is one error per task.
	q := f.createQueue(t, "other")
	f.call(t, http.MethodPost, "/v1/tasks/move", `{"filter":{"status":["paused"]},"queueId":"`+q.ID+`"}`, http.StatusOK, &batch)
	if len(batch.Results) != 3 {
		t.Fatalf("move results = %d", len(batch.Results))
	}
	for _, r := range batch.Results {
		if !r.OK || r.Task.QueueID != q.ID {
			t.Errorf("move result = %+v", r)
		}
	}
	f.call(t, http.MethodPost, "/v1/tasks/move", `{"ids":["`+c.ID+`"],"queueId":"nope"}`, http.StatusOK, &batch)
	if len(batch.Results) != 1 || batch.Results[0].OK {
		t.Errorf("moving to an unknown queue = %+v", batch.Results)
	}
	// Restart and refresh answer per task as well (a paused task is not
	// finished, so restarting it is refused).
	f.call(t, http.MethodPost, "/v1/tasks/restart", `{"ids":["`+a.ID+`"]}`, http.StatusOK, &batch)
	if len(batch.Results) != 1 {
		t.Errorf("restart results = %+v", batch.Results)
	}
	f.call(t, http.MethodPost, "/v1/tasks/refresh", `{"ids":["`+a.ID+`"]}`, http.StatusOK, &batch)
	if len(batch.Results) != 1 {
		t.Errorf("refresh results = %+v", batch.Results)
	}
}

func TestLimitsAndTimelineOperations(t *testing.T) {
	f := newOpsFixture(t)

	var l service.LimitsView
	f.call(t, http.MethodGet, "/v1/limits", "", http.StatusOK, &l)
	if l.Global.SpeedLimit != 0 || l.Override != nil || len(l.Queues) != 1 || l.Queues[0].QueueID != "main" {
		t.Errorf("limits = %+v", l)
	}
	f.call(t, http.MethodPost, "/v1/limits/override", `{"speedLimit":0,"minutes":30}`, http.StatusOK, &l)
	if l.Override == nil || l.Override.SpeedLimit != 0 || time.Until(l.Override.Until) < 29*time.Minute {
		t.Errorf("override = %+v", l.Override)
	}
	f.call(t, http.MethodPost, "/v1/limits/override", `{"speedLimit":500000,"minutes":5}`, http.StatusOK, &l)
	if l.Override == nil || l.Override.SpeedLimit != 500000 || l.Global.SpeedLimit != 500000 {
		t.Errorf("a new override replaces the old one: %+v", l)
	}
	f.call(t, http.MethodPost, "/v1/limits/override", `{"speedLimit":1,"minutes":1441}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPost, "/v1/limits/override", `{"speedLimit":-1,"minutes":5}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodDelete, "/v1/limits/override", "", http.StatusOK, &l)
	if l.Override != nil || l.Global.SpeedLimit != 0 {
		t.Errorf("after clearing: %+v", l)
	}

	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	err := f.store.Transfer().Add(t.Context(), []storage.TransferRow{
		{Bucket: day.Add(10 * time.Hour), QueueID: "main", Engine: "http", Down: 100, Up: 1},
		{Bucket: day.Add(11 * time.Hour), QueueID: "main", Engine: "bt", Down: 50, Up: 9},
		{Bucket: day.Add(30 * time.Hour), QueueID: "main", Engine: "http", Down: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	rfc := func(t time.Time) string { return url.QueryEscape(t.Format(time.RFC3339)) }
	var tl Timeline
	q := "from=" + rfc(day) + "&to=" + rfc(day.Add(48*time.Hour))
	f.call(t, http.MethodGet, "/v1/stats/timeline?bucket=day&groupBy=engine&"+q, "", http.StatusOK, &tl)
	if len(tl.Points) != 3 || tl.Points[0].Group != "bt" || tl.Points[0].Up != 9 || tl.Points[1].Group != "http" || tl.Points[1].Down != 100 || tl.Points[2].Down != 7 {
		t.Errorf("daily timeline by engine = %+v", tl.Points)
	}
	f.call(t, http.MethodGet, "/v1/stats/timeline?"+q, "", http.StatusOK, &tl)
	if len(tl.Points) != 3 || tl.Bucket != "hour" || tl.Points[0].Down != 100 {
		t.Errorf("hourly timeline = %+v", tl)
	}
	f.call(t, http.MethodGet, "/v1/stats/timeline", "", http.StatusOK, &tl)
	if len(tl.Points) != 0 || !tl.To.After(tl.From) {
		t.Errorf("default range = %+v", tl)
	}
	f.call(t, http.MethodGet, "/v1/stats/timeline?from="+rfc(day.AddDate(-5, 0, 0))+"&to="+rfc(day), "", http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodGet, "/v1/stats/timeline?from="+rfc(day)+"&to="+rfc(day), "", http.StatusUnprocessableEntity, nil)
}
