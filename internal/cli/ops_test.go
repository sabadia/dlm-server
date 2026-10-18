package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/client"
	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/daemon"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
	"github.com/sabadia/dlm-server/internal/webhook"
)

// e2e is a CLI test environment: a real API server with real services on a
// temp SQLite store, and a local origin server for download URLs.
type e2e struct {
	base   string
	origin string
	home   string
	// browser is the fake headless browser behind `dlm browser`.
	browser *fakeBrowser
}

func newE2E(t *testing.T) *e2e {
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
	cipher, err := cookies.NewCipher(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	fb := &fakeBrowser{}
	svc := daemon.NewServices(service.Deps{
		Store: store, Engines: reg, Resolves: resolves,
		Defaults: func() settings.Settings { return defaults },
	})
	jar := cookies.NewStore(store.Cookies(), cipher, cookies.Options{})
	base := startAPIWith(t, api.Deps{
		Token: testToken, Version: testSrvVersion,
		Settings: svc.Settings, Queues: svc.Queues, Tasks: svc.Tasks,
		Cookies: service.NewCookieService(jar), Capture: service.NewCaptureService(svc.Tasks, jar, svc.Refresh), Refresh: svc.Refresh, Browser: service.NewBrowserService(fb, svc.Tasks), Categories: svc.Categories,
		Webhooks:    service.NewWebhookService(service.Deps{Store: store}, cipher, webhook.New(nil, nil, webhook.Options{})),
		Credentials: service.NewCredentialService(service.Deps{Store: store}, cipher),
	})
	content := bytes.Repeat([]byte("x"), 2048)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Unix(1_700_000_000, 0), bytes.NewReader(content))
	}))
	t.Cleanup(origin.Close)
	return &e2e{base: base, origin: origin.URL, home: t.TempDir(), browser: fb}
}

// run runs the CLI against the environment's server.
func (e *e2e) run(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), testCLIVersion, append([]string{"--server", e.base, "--token", testToken}, args...),
		WithIO(&stdout, &stderr),
		WithStdin(strings.NewReader(stdin)),
		WithEnviron(func() []string { return []string{"DLM_HOME=" + e.home} }),
	)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// ok runs the CLI, requires exit 0 and decodes JSON output into out.
func (e *e2e) ok(t *testing.T, out any, args ...string) string {
	t.Helper()
	r := e.run(t, "", args...)
	if r.code != ExitOK {
		t.Fatalf("dlm %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(r.stdout), out); err != nil {
			t.Fatalf("dlm %s: output is not JSON: %v\n%s", strings.Join(args, " "), err, r.stdout)
		}
	}
	return r.stdout
}

func (e *e2e) url(name string) string { return e.origin + "/" + name }

func settingValue(t *testing.T, raw json.RawMessage, path string) string {
	t.Helper()
	v, ok := lookupPath(raw, path)
	if !ok {
		return "<unset>"
	}
	return compactJSON(v)
}

func TestTaskCommandsEndToEnd(t *testing.T) {
	e := newE2E(t)

	var q client.Queue
	e.ok(t, &q, "queue", "add", "fast", "--max-concurrent", "2", "--speed-limit", "1MB", "-o", "json")
	if q.Name != "fast" || settingValue(t, q.Settings, "maxConcurrentDownloads") != "2" ||
		settingValue(t, q.Settings, "speedLimit") != "1000000" {
		t.Fatalf("queue = %+v settings %s", q, q.Settings)
	}

	var task client.Task
	e.ok(t, &task, "task", "add", e.url("a.bin"), "--queue", q.ID, "--name", "a.bin", "--priority", "high",
		"--speed-limit", "512K", "--connections", "4", "--header", "X-Test=1", "--set", "retry.maxAttempts=3", "-o", "json")
	if task.QueueID != q.ID || task.Name != "a.bin" || task.Priority != "high" || task.Status != "queued" ||
		settingValue(t, task.Settings, "speedLimit") != "524288" || settingValue(t, task.Settings, "maxConnections") != "4" ||
		settingValue(t, task.Settings, "headers") != `{"X-Test":"1"}` || settingValue(t, task.Settings, "retry.maxAttempts") != "3" {
		t.Fatalf("task = %+v settings %s", task, task.Settings)
	}

	// Several URLs (args + file from stdin) use create-tasks.
	r := e.run(t, "# list\n"+e.url("c.bin")+"\n\n", "task", "add", e.url("b.bin"), "-f", "-", "-o", "json")
	if r.code != ExitOK {
		t.Fatalf("batch add: exit %d: %s", r.code, r.stderr)
	}
	var items []client.BatchItem
	if err := json.Unmarshal([]byte(r.stdout), &items); err != nil || len(items) != 2 || !items[0].OK || !items[1].OK {
		t.Fatalf("batch items = %+v (%v)", items, err)
	}

	// list: table with short IDs, --all follows cursors.
	out := e.ok(t, nil, "task", "list", "--queue", q.ID)
	if !strings.Contains(out, shortID(task.ID)) || strings.Contains(out, task.ID) || !strings.Contains(out, "a.bin") ||
		!strings.HasPrefix(strings.Join(strings.Fields(out), " "), "ID NAME STATUS PROGRESS SIZE QUEUE") {
		t.Errorf("list table:\n%s", out)
	}
	r = e.run(t, "", "task", "list", "--limit", "1")
	if r.code != ExitOK || !strings.Contains(r.stderr, "more tasks available") {
		t.Errorf("paged list: exit %d stderr %q", r.code, r.stderr)
	}
	var page client.TaskPage
	e.ok(t, &page, "task", "list", "--limit", "1", "--all", "-o", "json")
	if len(page.Tasks) != 3 || page.NextCursor != "" {
		t.Fatalf("--all returned %d tasks, cursor %q", len(page.Tasks), page.NextCursor)
	}

	var got client.Task
	e.ok(t, &got, "task", "get", task.ID, "-o", "json")
	if got.ID != task.ID {
		t.Fatalf("get = %+v", got)
	}

	e.ok(t, &got, "task", "set", task.ID, "--set", "speedLimit=2048", "--unset", "retry.maxAttempts", "--priority", "low",
		"--label", "k=v", "-o", "json")
	if got.Priority != "low" || got.Labels["k"] != "v" || settingValue(t, got.Settings, "speedLimit") != "2048" ||
		settingValue(t, got.Settings, "retry.maxAttempts") != "<unset>" || settingValue(t, got.Settings, "maxConnections") != "4" {
		t.Fatalf("after set: %+v settings %s", got, got.Settings)
	}

	out = e.ok(t, nil, "task", "settings", task.ID)
	if !strings.Contains(out, "speedLimit") || !containsRow(out, "speedLimit 2048 task") || !containsRow(out, "maxConcurrentDownloads 2 queue") {
		t.Errorf("effective settings table:\n%s", out)
	}

	out = e.ok(t, nil, "task", "pause", task.ID)
	if !containsRow(out, task.ID+" ok paused") {
		t.Errorf("pause table:\n%s", out)
	}
	e.ok(t, &items, "task", "pause", "--queue", "main", "-o", "json")
	if len(items) != 2 {
		t.Fatalf("pause --queue main: %+v", items)
	}
	e.ok(t, &items, "task", "resume", "--all", "--status", "paused", "-o", "json")
	if len(items) != 3 || items[0].Task.Status != "queued" {
		t.Fatalf("resume --all: %+v", items)
	}

	// A failed item exits 1 but still prints every result.
	r = e.run(t, "", "task", "pause", task.ID, "missing")
	if r.code != ExitError || !containsRow(r.stdout, task.ID+" ok paused") || !strings.Contains(r.stdout, "failed") ||
		!strings.Contains(r.stderr, "1 of 2 items failed") {
		t.Errorf("partial failure: exit %d stdout %s stderr %s", r.code, r.stdout, r.stderr)
	}

	out = e.ok(t, nil, "task", "rm", task.ID, "--files")
	if strings.TrimSpace(out) != "deleted "+task.ID {
		t.Errorf("rm output %q", out)
	}
	if r := e.run(t, "", "task", "get", task.ID); r.code != ExitError || !strings.Contains(r.stderr, "Not Found") {
		t.Errorf("get deleted: exit %d stderr %s", r.code, r.stderr)
	}
	e.ok(t, &items, "task", "rm", "--all", "-o", "json")
	if len(items) != 2 {
		t.Fatalf("rm --all: %+v", items)
	}
}

func containsRow(table, row string) bool {
	for line := range strings.Lines(table) {
		if strings.Join(strings.Fields(line), " ") == row {
			return true
		}
	}
	return false
}

func TestTaskCommandsMoveReorderRestart(t *testing.T) {
	e := newE2E(t)
	var q client.Queue
	e.ok(t, &q, "queue", "add", "other", "-o", "json")
	var a, b client.Task
	e.ok(t, &a, "task", "add", e.url("a"), "-o", "json")
	e.ok(t, &b, "task", "add", e.url("b"), "-o", "json")
	var got client.Task
	e.ok(t, &got, "task", "reorder", b.ID, "0", "-o", "json")
	if got.Position != 0 {
		t.Errorf("reorder position %d", got.Position)
	}
	e.ok(t, &got, "task", "move", a.ID, q.ID, "-o", "json")
	if got.QueueID != q.ID {
		t.Errorf("move queue %q", got.QueueID)
	}
	if r := e.run(t, "", "task", "restart", a.ID); r.code != ExitError || !strings.Contains(r.stderr, "Conflict") {
		t.Errorf("restart queued: exit %d stderr %s", r.code, r.stderr)
	}
	var res client.ResolveResult
	e.ok(t, &res, "resolve", e.url("r.bin"), "-o", "json")
	if res.ResolveID == "" || res.Resource.Size != 2048 {
		t.Errorf("resolve = %+v", res)
	}
}

func TestQueueCommandsEndToEnd(t *testing.T) {
	e := newE2E(t)
	var q client.Queue
	e.ok(t, &q, "queue", "add", "q1", "--speed-limit", "2MiB", "--set", "maxConnections=3", "-o", "json")
	if settingValue(t, q.Settings, "speedLimit") != "2097152" {
		t.Fatalf("queue settings %s", q.Settings)
	}
	var task client.Task
	e.ok(t, &task, "task", "add", e.url("f"), "--queue", q.ID, "-o", "json")

	e.ok(t, &q, "queue", "set", q.ID, "--name", "renamed", "--unset", "speedLimit", "--max-concurrent", "5", "-o", "json")
	if q.Name != "renamed" || settingValue(t, q.Settings, "speedLimit") != "<unset>" ||
		settingValue(t, q.Settings, "maxConcurrentDownloads") != "5" || settingValue(t, q.Settings, "maxConnections") != "3" {
		t.Fatalf("after set: %+v %s", q, q.Settings)
	}
	out := e.ok(t, nil, "queue", "list")
	if !containsRow(out, "main Main running 0 -") || !containsRow(out, q.ID+" renamed running 1 5") {
		t.Errorf("queue table:\n%s", out)
	}
	e.ok(t, &q, "queue", "stop", q.ID, "-o", "json")
	if q.State != "stopped" {
		t.Errorf("stop: %s", q.State)
	}
	e.ok(t, &q, "queue", "start", q.ID, "-o", "json")
	if q.State != "running" {
		t.Errorf("start: %s", q.State)
	}
	e.ok(t, &q, "queue", "reorder", q.ID, "0", "-o", "json")
	if q.Position != 0 {
		t.Errorf("reorder: %d", q.Position)
	}
	e.ok(t, &q, "queue", "get", q.ID, "-o", "json")
	if q.Name != "renamed" {
		t.Errorf("get: %+v", q)
	}

	file := filepath.Join(t.TempDir(), "q.yaml")
	if err := os.WriteFile(file, []byte("maxConnections: 7\nretry:\n  maxAttempts: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.ok(t, &q, "queue", "replace-settings", q.ID, "-f", file, "-o", "json")
	if settingValue(t, q.Settings, "maxConnections") != "7" || settingValue(t, q.Settings, "maxConcurrentDownloads") != "<unset>" {
		t.Fatalf("replaced: %s", q.Settings)
	}
	out = e.ok(t, nil, "queue", "settings", q.ID)
	if !containsRow(out, "maxConnections 7 queue") || !containsRow(out, "retry.maxAttempts 2 queue") {
		t.Errorf("queue effective:\n%s", out)
	}

	if r := e.run(t, "", "queue", "rm", q.ID); r.code != ExitError || !strings.Contains(r.stderr, "Conflict") {
		t.Errorf("rm non-empty queue: exit %d stderr %s", r.code, r.stderr)
	}
	out = e.ok(t, nil, "queue", "rm", q.ID, "--move-tasks-to", "main")
	if strings.TrimSpace(out) != "deleted "+q.ID {
		t.Errorf("rm output %q", out)
	}
	var got client.Task
	e.ok(t, &got, "task", "get", task.ID, "-o", "json")
	if got.QueueID != "main" {
		t.Errorf("task moved to %q", got.QueueID)
	}
}

func TestSettingsCommandsEndToEnd(t *testing.T) {
	e := newE2E(t)
	var layer json.RawMessage
	e.ok(t, &layer, "settings", "set", "maxConnections=8", "retry.maxAttempts=2", "userAgent=my agent", "-o", "json")
	if settingValue(t, layer, "maxConnections") != "8" || settingValue(t, layer, "retry.maxAttempts") != "2" ||
		settingValue(t, layer, "userAgent") != "my agent" {
		t.Fatalf("after set: %s", layer)
	}
	e.ok(t, &layer, "settings", "unset", "retry.maxAttempts", "userAgent", "-o", "json")
	e.ok(t, &layer, "settings", "get", "-o", "json")
	if string(layer) != `{"maxConnections":8}` && strings.Join(strings.Fields(string(layer)), "") != `{"maxConnections":8}` {
		t.Fatalf("after unset: %s", layer)
	}
	out := e.ok(t, nil, "settings", "get")
	if !containsRow(out, "maxConnections 8") {
		t.Errorf("settings table:\n%s", out)
	}
	r := e.run(t, `{"tlsVerify": false}`, "settings", "replace", "-f", "-", "-o", "json")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"tlsVerify": false`) || strings.Contains(r.stdout, "maxConnections") {
		t.Errorf("replace: exit %d stdout %s stderr %s", r.code, r.stdout, r.stderr)
	}
	out = e.ok(t, nil, "settings", "defaults")
	if !containsRow(out, "maxConnections 16") {
		t.Errorf("defaults table:\n%s", out)
	}
	// Invalid values are reported with their field.
	r = e.run(t, "", "settings", "set", "maxConnections=0")
	if r.code != ExitError || !strings.Contains(r.stderr, "maxConnections") {
		t.Errorf("invalid set: exit %d stderr %s", r.code, r.stderr)
	}
}

func TestOperationUsageErrors(t *testing.T) {
	e := newE2E(t)
	for _, args := range [][]string{
		{"task", "pause"},
		{"task", "pause", "a", "--all"},
		{"task", "rm"},
		{"task", "add"},
		{"task", "add", "http://x/1", "http://x/2", "--name", "n"},
		{"task", "add", "http://x/1", "--speed-limit", "fast"},
		{"task", "set", "id"},
		{"task", "set", "id", "--label", "novalue"},
		{"task", "reorder", "id", "-1"},
		{"queue", "set", "id"},
		{"queue", "replace-settings", "id"},
		{"settings", "set"},
		{"settings", "set", "novalue"},
		{"settings", "set", "a..b=1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if r := e.run(t, "", args...); r.code != ExitUsage {
				t.Errorf("exit %d, want %d; stderr: %s", r.code, ExitUsage, r.stderr)
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true},
		{"1024", 1024, true},
		{"512K", 512 << 10, true},
		{"512k", 512 << 10, true},
		{"1KB", 1000, true},
		{"1.5MB", 1_500_000, true},
		{"1.5M", 3 << 19, true},
		{"2MiB", 2 << 20, true},
		{"1 GiB", 1 << 30, true},
		{"1G", 1 << 30, true},
		{"10B", 10, true},
		{" 3TB ", 3_000_000_000_000, true},
		{"", 0, false},
		{"-1", 0, false},
		{"MB", 0, false},
		{"1.2.3", 0, false},
		{"5XB", 0, false},
		{"1e3", 0, false},
		{"99999999999TiB", 0, false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSize(tc.in)
			if (err == nil) != tc.ok || got != tc.want {
				t.Errorf("parseSize(%q) = %d, %v; want %d ok=%v", tc.in, got, err, tc.want, tc.ok)
			}
		})
	}
}

func TestFormatSize(t *testing.T) {
	for in, want := range map[int64]string{-1: "-", 0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 30: "5.0 GiB"} {
		if got := formatSize(in); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestSettingsDoc(t *testing.T) {
	doc := settingsDoc{}
	if err := doc.apply([]string{"a.b=1", "a.c=true", "s=hello", "q=\"7\"", "o={\"x\":[1]}"}, []string{"a.d", "n"}); err != nil {
		t.Fatal(err)
	}
	raw, err := doc.raw()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":{"b":1,"c":true,"d":null},"n":null,"o":{"x":[1]},"q":"7","s":"hello"}`
	if string(raw) != want {
		t.Errorf("doc = %s, want %s", raw, want)
	}
	if err := doc.assign("s.x=1"); err == nil {
		t.Error("path through a scalar: want error")
	}
	for _, bad := range []string{"=1", ".a=1", "a.=1", "noequals"} {
		if err := (settingsDoc{}).assign(bad); err == nil {
			t.Errorf("assign(%q): want error", bad)
		}
	}
	if raw, _ := (settingsDoc{}).raw(); raw != nil {
		t.Errorf("empty doc = %s, want nil", raw)
	}
}

func TestCookieCommandsEndToEnd(t *testing.T) {
	e := newE2E(t)
	jsonData := `[{"domain":"example.com","name":"sid","value":"top-secret","path":"/","secure":true,"httpOnly":true,"hostOnly":true},
	  {"domain":".example.org","name":"pref","value":"dark","path":"/"}]`

	// Import from stdin (format detected).
	r := e.run(t, jsonData, "cookies", "import", "-", "-o", "json")
	if r.code != ExitOK {
		t.Fatalf("import: exit %d: %s", r.code, r.stderr)
	}
	var res client.ImportCookiesResult
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil || res.Imported != 2 {
		t.Fatalf("import result = %+v (%v)", res, err)
	}

	// List shows metadata, never values.
	out := e.ok(t, nil, "cookies", "list")
	if !strings.Contains(out, "example.com") || !strings.Contains(out, "sid") || strings.Contains(out, "top-secret") ||
		!strings.HasPrefix(strings.Join(strings.Fields(out), " "), "ID DOMAIN PATH NAME SECURE HTTPONLY EXPIRES") {
		t.Errorf("list table:\n%s", out)
	}
	var cs []client.Cookie
	e.ok(t, &cs, "cookies", "list", "--domain", "example.org", "-o", "json")
	if len(cs) != 1 || cs[0].Name != "pref" {
		t.Fatalf("filtered list = %+v", cs)
	}

	// Export to stdout (bare file) and to a 0600 file.
	out = e.ok(t, nil, "cookies", "export", "--format", "netscape", "--domain", "example.com")
	if !strings.Contains(out, "#HttpOnly_example.com\tFALSE\t/\tTRUE\t0\tsid\ttop-secret\n") {
		t.Errorf("netscape export:\n%s", out)
	}
	file := filepath.Join(t.TempDir(), "cookies.json")
	out = e.ok(t, nil, "cookies", "export", "-f", file)
	data, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(data), "top-secret") || !strings.Contains(out, "exported 2 cookies") {
		t.Errorf("file export: %q (%v), output %q", data, err, out)
	}
	if info, err := os.Stat(file); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Errorf("export file mode: %v (%v)", info, err)
	}

	// The exported file imports back from a file argument.
	if out = e.ok(t, nil, "cookies", "import", file, "--format", "json"); !strings.Contains(out, "imported 2, skipped 0 expired, rejected 0") {
		t.Errorf("import output %q", out)
	}
	r = e.run(t, "", "cookies", "import", "-")
	if r.code != ExitError || !strings.Contains(r.stderr, "Unprocessable") {
		t.Errorf("import of empty input: exit %d stderr %s", r.code, r.stderr)
	}
	r = e.run(t, "nope", "cookies", "import", "-", "--format", "json")
	if r.code != ExitError || !strings.Contains(r.stderr, "invalid JSON") {
		t.Errorf("import of garbage: exit %d stderr %s", r.code, r.stderr)
	}

	// Delete one by ID, the rest by domain.
	out = e.ok(t, nil, "cookies", "rm", cs[0].ID)
	if strings.TrimSpace(out) != "deleted "+cs[0].ID {
		t.Errorf("rm output %q", out)
	}
	if r := e.run(t, "", "cookies", "rm", cs[0].ID); r.code != ExitError || !strings.Contains(r.stderr, "Not Found") {
		t.Errorf("rm again: exit %d stderr %s", r.code, r.stderr)
	}
	if r := e.run(t, "", "cookies", "clear"); r.code != ExitUsage || !strings.Contains(r.stderr, "--domain is required") {
		t.Errorf("clear without domain: exit %d stderr %s", r.code, r.stderr)
	}
	out = e.ok(t, nil, "cookies", "clear", "--domain", "example.com")
	if strings.TrimSpace(out) != "deleted 1 cookies of example.com" {
		t.Errorf("clear output %q", out)
	}
	if out = e.ok(t, nil, "cookies", "list", "-o", "json"); strings.TrimSpace(out) != "[]" {
		t.Errorf("list after clear = %q, want []", out)
	}
}

func TestCaptureCommandEndToEnd(t *testing.T) {
	e := newE2E(t)
	request := `{
	  "url": "` + e.url("dl?id=1") + `",
	  "finalUrl": "` + e.url("final.bin") + `",
	  "pageUrl": "https://example.com/page",
	  "cookies": [{"domain": "127.0.0.1", "name": "sid", "value": "s3cret", "hostOnly": true}],
	  "mediaCandidates": [{"url": "https://example.com/m.m3u8", "kind": "hls"}]
	}`

	var res client.CaptureResult
	r := e.run(t, request, "capture", "-r", "-", "--paused", "--header", "X-Test=1", "--filename", "my/file.bin", "--size", "2KiB", "-o", "json")
	if r.code != ExitOK {
		t.Fatalf("capture: exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, r.stdout)
	}
	src := res.Task.Source
	if res.Task.Status != "paused" || res.Task.Name != "file.bin" || src.OriginalURL != e.url("dl?id=1") || src.CurrentURL != e.url("final.bin") ||
		src.PageURL != "https://example.com/page" || src.Headers["X-Test"] != "1" || src.Fingerprint.Size != 2048 || res.Cookies.Imported != 1 {
		t.Fatalf("result = %+v source %+v", res, src)
	}
	if out := e.ok(t, nil, "cookies", "list", "--domain", "127.0.0.1"); !strings.Contains(out, "sid") {
		t.Errorf("captured cookie not stored:\n%s", out)
	}

	// A URL argument with flags only; the table shows the task.
	out := e.ok(t, nil, "capture", e.url("plain.bin"), "--page-url", "https://example.com/p", "--referrer", "https://ref.example/")
	if !strings.Contains(out, "plain.bin") && !strings.Contains(out, e.url("plain.bin")) {
		t.Errorf("table output:\n%s", out)
	}

	// Usage and server errors.
	if r := e.run(t, "not json", "capture", "-r", "-"); r.code != ExitUsage || !strings.Contains(r.stderr, "not a capture request") {
		t.Errorf("bad request file: exit %d stderr %s", r.code, r.stderr)
	}
	if r := e.run(t, `{"url":"x","bogus":1}`, "capture", "-r", "-"); r.code != ExitUsage {
		t.Errorf("unknown field: exit %d stderr %s", r.code, r.stderr)
	}
	if r := e.run(t, "", "capture", e.url("a.bin"), "--size", "lots"); r.code != ExitUsage {
		t.Errorf("bad size: exit %d stderr %s", r.code, r.stderr)
	}
	if r := e.run(t, "", "capture"); r.code != ExitError || !strings.Contains(r.stderr, "Unprocessable") {
		t.Errorf("nothing to capture: exit %d stderr %s", r.code, r.stderr)
	}
}

func TestRefreshCommandsEndToEnd(t *testing.T) {
	e := newE2E(t)
	var task client.Task
	e.ok(t, &task, "task", "add", e.url("a.bin"), "--paused", "-o", "json")

	var got client.Task
	e.ok(t, &got, "task", "set-url", task.ID, e.url("fresh.bin"), "--referrer", "https://example.com/", "--header", "X-A=1",
		"--on-mismatch", "restart", "-o", "json")
	if got.Status != "paused" || got.Source.CurrentURL != e.url("fresh.bin") || got.Source.Referrer != "https://example.com/" ||
		got.Source.Headers["X-A"] != "1" || got.Source.OriginalURL != e.url("a.bin") {
		t.Fatalf("after set-url: %s %+v", got.Status, got.Source)
	}

	e.ok(t, &got, "task", "refresh", task.ID, "-o", "json")
	if got.Status != "paused" {
		t.Errorf("status after refresh = %s, want paused", got.Status)
	}

	var history []client.RefreshAttempt
	e.ok(t, &history, "task", "refresh-history", task.ID, "-o", "json")
	if len(history) != 2 || history[0].Cause != "source" || history[0].Outcome != "success" ||
		history[1].Strategy != "origin-redirect" || history[1].Cause != "manual" || history[1].NewURL != e.url("a.bin") {
		t.Errorf("history = %+v", history)
	}
	out := e.ok(t, nil, "task", "refresh-history", task.ID)
	if !strings.HasPrefix(strings.Join(strings.Fields(out), " "), "TIME CAUSE STRATEGY OUTCOME DETAIL") ||
		!strings.Contains(out, "source") || !strings.Contains(out, "manual") || !strings.Contains(out, "origin-redirect") || !strings.Contains(out, "success") {
		t.Errorf("history table:\n%s", out)
	}

	// Errors are reported with the right exit codes.
	if r := e.run(t, "", "task", "await-link", task.ID); r.code != ExitError || !strings.Contains(r.stderr, "only a failed or awaiting_link task") {
		t.Errorf("await-link of a paused task: exit %d stderr %s", r.code, r.stderr)
	}
	if r := e.run(t, "", "task", "refresh", "missing"); r.code != ExitError || !strings.Contains(r.stderr, "Not Found") {
		t.Errorf("refresh of a missing task: exit %d stderr %s", r.code, r.stderr)
	}
	if r := e.run(t, "", "task", "set-url", task.ID); r.code != ExitUsage {
		t.Errorf("set-url without a URL: exit %d", r.code)
	}
	if r := e.run(t, "", "task", "set-url", task.ID, "not a url"); r.code != ExitError {
		t.Errorf("set-url with a bad URL: exit %d stderr %s", r.code, r.stderr)
	}
}

// TestRefreshSettingsPerLevel: the refresh settings can be set globally, per
// queue and per task, and the effective view says where each value comes from.
func TestRefreshSettingsPerLevel(t *testing.T) {
	e := newE2E(t)
	e.ok(t, nil, "settings", "set", `refresh.strategies=["origin-redirect","manual"]`, "refresh.awaitTimeout=5m", "refresh.maxAttempts=7")
	var q client.Queue
	e.ok(t, &q, "queue", "add", "slow", "--set", "refresh.auto=false", "--set", "refresh.maxAttempts=2", "-o", "json")
	var task client.Task
	e.ok(t, &task, "task", "add", e.url("a.bin"), "--queue", q.ID, "--paused", "--set", "refresh.onMismatch=restart", "-o", "json")

	out := e.ok(t, nil, "task", "settings", task.ID)
	for _, row := range []string{
		"refresh.auto false queue",
		"refresh.maxAttempts 2 queue",
		"refresh.onMismatch restart task",
		"refresh.awaitTimeout 5m0s global",
	} {
		if !containsRow(out, row) {
			t.Errorf("effective settings lack the row %q:\n%s", row, out)
		}
	}
	if !strings.Contains(out, "refresh.strategies") || !strings.Contains(out, "origin-redirect") || !strings.Contains(out, "manual") {
		t.Errorf("effective settings lack the global strategies:\n%s", out)
	}

	// A task level override wins; an unknown strategy is rejected.
	e.ok(t, nil, "task", "set", task.ID, "--set", `refresh.strategies=["extension-capture"]`)
	if r := e.run(t, "", "task", "set", task.ID, "--set", `refresh.strategies=["teleport"]`); r.code != ExitError || !strings.Contains(r.stderr, "unknown strategy") {
		t.Errorf("unknown strategy: exit %d stderr %s", r.code, r.stderr)
	}
	if r := e.run(t, "", "settings", "set", "refresh.onMismatch=sometimes"); r.code != ExitError {
		t.Errorf("bad onMismatch: exit %d stderr %s", r.code, r.stderr)
	}
}

func TestTaskAddYtdlpAndFormatFlags(t *testing.T) {
	e := newE2E(t)
	// --ytdlp and --format become the resolver and the media hint of the source.
	var yt client.Task
	e.ok(t, &yt, "task", "add", "https://video.example/watch?v=1", "--ytdlp", "--format", "bv*+ba/b", "--paused", "-o", "json")
	if yt.Source.ResolverKind != "ytdlp" || yt.Source.MediaHint != "bv*+ba/b" || yt.Source.CurrentURL != "https://video.example/watch?v=1" {
		t.Fatalf("yt-dlp task source = %+v", yt.Source)
	}
	// --format alone is a hint for a stream, not a resolver choice.
	var hls client.Task
	e.ok(t, &hls, "task", "add", e.url("x.m3u8"), "--format", "720", "--paused", "-o", "json")
	if hls.Source.ResolverKind != "direct" || hls.Source.MediaHint != "720" {
		t.Fatalf("stream task source = %+v", hls.Source)
	}
}

func TestTaskAddChecksumFlag(t *testing.T) {
	e := newE2E(t)
	sum := strings.Repeat("ab", 32)
	var task client.Task
	e.ok(t, &task, "task", "add", e.url("a.bin"), "--checksum", "SHA256:"+strings.ToUpper(sum), "--paused", "-o", "json")
	if got := settingValue(t, task.Settings, "checksum"); got != `{"algorithm":"sha256","expected":"`+sum+`"}` {
		t.Errorf("checksum setting = %s", got)
	}
	for _, bad := range []string{"sha256", "sha256:", ":abcd", "sha256:xyz"} {
		if r := e.run(t, "", "task", "add", e.url("b.bin"), "--checksum", bad); r.code == ExitOK {
			t.Errorf("--checksum %q was accepted", bad)
		}
	}
}

func TestCategoryCommands(t *testing.T) {
	e := newE2E(t)
	var cat client.Category
	e.ok(t, &cat, "category", "add", "Videos", "--dir", "Videos", "--ext", "mp4", "--ext", ".MKV", "--mime", "video/*", "-o", "json")
	if cat.Name != "Videos" || cat.Dir != "Videos" || strings.Join(cat.Extensions, ",") != "mp4,mkv" || strings.Join(cat.MIMETypes, ",") != "video/*" {
		t.Fatalf("category = %+v", cat)
	}
	if r := e.run(t, "", "category", "add", "NoDir", "--ext", "a"); r.code == ExitOK {
		t.Error("category add without --dir succeeded")
	}
	if r := e.run(t, "", "category", "add", "NoRules", "--dir", "x"); r.code == ExitOK {
		t.Error("category add without rules succeeded")
	}
	var list []client.Category
	e.ok(t, &list, "category", "list", "-o", "json")
	if len(list) != 1 || list[0].ID != cat.ID {
		t.Fatalf("list = %+v", list)
	}
	var got client.Category
	e.ok(t, &got, "category", "get", cat.ID, "-o", "json")
	if got.ID != cat.ID {
		t.Errorf("get = %+v", got)
	}
	e.ok(t, &got, "category", "set", cat.ID, "--name", "Movies", "--ext", "avi", "--position", "3", "-o", "json")
	if got.Name != "Movies" || strings.Join(got.Extensions, ",") != "avi" || got.Position != 3 || got.Dir != "Videos" {
		t.Errorf("after set = %+v", got)
	}
	if r := e.run(t, "", "category", "set", cat.ID); r.code != ExitUsage {
		t.Errorf("set with nothing to change: exit %d, want usage", r.code)
	}
	r := e.run(t, "", "category", "match", "--name", "clip.AVI")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Movies") {
		t.Errorf("match: exit %d, stdout %q", r.code, r.stdout)
	}
	r = e.run(t, "", "category", "match", "--name", "data.bin")
	if r.code != ExitOK || !strings.Contains(r.stdout, "no category matches") {
		t.Errorf("match without a result: exit %d, stdout %q", r.code, r.stdout)
	}
	if r := e.run(t, "", "category", "match"); r.code != ExitUsage {
		t.Errorf("match without input: exit %d", r.code)
	}
	r = e.run(t, "", "category", "rm", cat.ID)
	if r.code != ExitOK || !strings.Contains(r.stdout, "deleted") {
		t.Errorf("rm: exit %d %q", r.code, r.stdout)
	}
	if r := e.run(t, "", "category", "get", cat.ID); r.code == ExitOK {
		t.Error("the deleted category is still there")
	}
}

func TestTaskAddDuplicates(t *testing.T) {
	e := newE2E(t)
	var first client.Task
	e.ok(t, &first, "task", "add", e.url("dup.bin"), "--paused", "-o", "json")
	r := e.run(t, "", "task", "add", e.url("dup.bin"), "--paused")
	if r.code == ExitOK || !strings.Contains(r.stderr, first.ID) {
		t.Errorf("duplicate: exit %d, stderr %q (want a refusal naming the first task)", r.code, r.stderr)
	}
	var again client.Task
	e.ok(t, &again, "task", "add", e.url("dup.bin"), "--paused", "--allow-duplicate", "-o", "json")
	if again.ID == first.ID {
		t.Error("--allow-duplicate did not create a new task")
	}
}

func TestWebhookCommands(t *testing.T) {
	e := newE2E(t)
	var hook client.Webhook
	e.ok(t, &hook, "webhook", "add", e.url("hook"), "--event", "task.status", "--event", "queue.completed", "--secret", "s3cret", "-o", "json")
	if hook.ID == "" || !hook.HasSecret || !hook.Enabled || len(hook.Events) != 2 {
		t.Fatalf("webhook = %+v", hook)
	}
	if r := e.run(t, "", "webhook", "add", e.url("x")); r.code == ExitOK {
		t.Error("webhook add without --event succeeded")
	}
	if r := e.run(t, "", "webhook", "add", "ftp://x.example/", "--event", "task.status"); r.code == ExitOK {
		t.Error("webhook add with an ftp URL succeeded")
	}
	var list []client.Webhook
	e.ok(t, &list, "webhook", "list", "-o", "json")
	if len(list) != 1 || list[0].ID != hook.ID {
		t.Fatalf("list = %+v", list)
	}
	var got client.Webhook
	e.ok(t, &got, "webhook", "get", hook.ID, "-o", "json")
	if got.URL != hook.URL {
		t.Errorf("get = %+v", got)
	}
	e.ok(t, &got, "webhook", "set", hook.ID, "--event", "task.deleted", "--enabled=false", "--secret", "", "-o", "json")
	if len(got.Events) != 1 || got.Enabled || got.HasSecret {
		t.Errorf("set = %+v", got)
	}
	if r := e.run(t, "", "webhook", "set", hook.ID); r.code != ExitUsage {
		t.Errorf("set with nothing to change: exit %d, want usage error", r.code)
	}
	if r := e.run(t, "", "webhook", "test", hook.ID); r.code != ExitOK || !strings.Contains(r.stdout, "delivered") {
		t.Errorf("test: exit %d %q %q", r.code, r.stdout, r.stderr)
	}
	e.ok(t, nil, "webhook", "set", hook.ID, "--url", "http://127.0.0.1:1/dead")
	if r := e.run(t, "", "webhook", "test", hook.ID); r.code == ExitOK {
		t.Error("test of an unreachable endpoint succeeded")
	}
	if r := e.run(t, "", "webhook", "rm", hook.ID); r.code != ExitOK || !strings.Contains(r.stdout, "deleted") {
		t.Errorf("rm: exit %d %q", r.code, r.stdout)
	}
	if r := e.run(t, "", "webhook", "get", hook.ID); r.code == ExitOK {
		t.Error("get after rm succeeded")
	}
}

func TestCredentialCommands(t *testing.T) {
	e := newE2E(t)
	var cred client.Credential
	// The password comes from stdin, so it never shows in the process list.
	r := e.run(t, "s3cret\n", "credential", "add", "Files.Example.com", "--user", "ann", "--password-stdin", "-o", "json")
	if r.code != ExitOK {
		t.Fatalf("add: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if strings.Contains(r.stdout, "s3cret") {
		t.Errorf("the password was printed: %s", r.stdout)
	}
	if err := json.Unmarshal([]byte(r.stdout), &cred); err != nil || cred.Host != "files.example.com" || cred.Scheme != "auto" {
		t.Fatalf("credential = %+v, %v", cred, err)
	}
	if r := e.run(t, "", "credential", "add", "x.example", "--user", "u"); r.code != ExitUsage {
		t.Errorf("add without a password: exit %d, want usage error", r.code)
	}
	if r := e.run(t, "p\n", "credential", "add", "x.example", "--user", "u", "--password", "p", "--password-stdin"); r.code != ExitUsage {
		t.Errorf("both password flags: exit %d, want usage error", r.code)
	}
	if r := e.run(t, "", "credential", "add", "x.example", "--password", "p"); r.code == ExitOK {
		t.Error("add without --user succeeded")
	}
	if r := e.run(t, "", "credential", "add", "files.example.com", "--user", "u", "--password", "p"); r.code == ExitOK {
		t.Error("a second login for the same host was accepted")
	}
	var list []client.Credential
	e.ok(t, &list, "credential", "list", "-o", "json")
	if len(list) != 1 || list[0].ID != cred.ID {
		t.Fatalf("list = %+v", list)
	}
	var got client.Credential
	e.ok(t, &got, "credential", "get", cred.ID, "-o", "json")
	if got.Username != "ann" {
		t.Errorf("get = %+v", got)
	}
	r = e.run(t, "new-pass\n", "credential", "set", cred.ID, "--scheme", "digest", "--password-stdin", "-o", "json")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"digest"`) {
		t.Errorf("set: exit %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := e.run(t, "", "credential", "set", cred.ID); r.code != ExitUsage {
		t.Errorf("set with nothing to change: exit %d, want usage error", r.code)
	}
	if r := e.run(t, "", "credential", "rm", cred.ID); r.code != ExitOK || !strings.Contains(r.stdout, "deleted") {
		t.Errorf("rm: exit %d %q", r.code, r.stdout)
	}
	if r := e.run(t, "", "credential", "get", cred.ID); r.code == ExitOK {
		t.Error("get after rm succeeded")
	}
}

func TestTaskAddExpandAndMirror(t *testing.T) {
	e := newE2E(t)
	var items []client.BatchItem
	e.ok(t, &items, "task", "add", e.url("img[08-10].jpg"), "--expand", "--paused", "-o", "json")
	if len(items) != 3 || !items[0].OK || !strings.HasSuffix(items[2].Task.Source.CurrentURL, "img10.jpg") {
		t.Fatalf("items = %+v", items)
	}
	if r := e.run(t, "", "task", "add", e.url("n[1-2].jpg"), "--expand", "--name", "x.jpg"); r.code != ExitUsage {
		t.Errorf("--name with --expand: exit %d, want usage error", r.code)
	}
	var task client.Task
	e.ok(t, &task, "task", "add", e.url("m.bin"), "--mirror", "https://m1.example/m.bin", "--mirror", "https://m2.example/m.bin", "--paused", "-o", "json")
	if len(task.Source.Mirrors) != 2 {
		t.Errorf("mirrors = %v", task.Source.Mirrors)
	}
	if r := e.run(t, "", "task", "add", e.url("a"), e.url("b"), "--mirror", "https://m.example/x"); r.code != ExitUsage {
		t.Errorf("--mirror with two URLs: exit %d, want usage error", r.code)
	}
	if r := e.run(t, "", "task", "add", e.url("c.bin"), "--mirror", "ftp://m.example/x", "--allow-duplicate"); r.code == ExitOK {
		t.Error("an ftp mirror was accepted")
	}
}

func TestTaskExportImportAndStats(t *testing.T) {
	e := newE2E(t)
	e.ok(t, nil, "task", "add", e.url("one.bin"), e.url("two.bin"), "--paused")

	r := e.run(t, "", "task", "export", "--format", "urls")
	if r.code != ExitOK || strings.Count(r.stdout, "\n") != 2 || !strings.Contains(r.stdout, "one.bin") {
		t.Fatalf("export urls: exit %d %q", r.code, r.stdout)
	}
	file := filepath.Join(t.TempDir(), "backup.json")
	r = e.run(t, "", "task", "export", "-f", file)
	if r.code != ExitOK || !strings.Contains(r.stderr, "exported 2 tasks") {
		t.Fatalf("export to a file: exit %d %q", r.code, r.stderr)
	}
	data, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(data), `"version": 1`) {
		t.Fatalf("file = %q, %v (the format follows the extension)", data, err)
	}

	// The same links again are duplicates; --allow-duplicate adds them.
	var items []client.BatchItem
	r = e.run(t, "", "task", "import", file, "-o", "json")
	if r.code == ExitOK {
		t.Fatalf("an import where every task failed exited 0: %s", r.stdout)
	}
	if err := json.Unmarshal([]byte(r.stdout), &items); err != nil || len(items) != 2 || items[0].OK || items[1].OK {
		t.Fatalf("duplicate import = %+v, %v", items, err)
	}
	e.ok(t, &items, "task", "import", file, "--allow-duplicate", "--paused", "-o", "json")
	if len(items) != 2 || !items[0].OK || !items[1].OK {
		t.Fatalf("import = %+v", items)
	}
	r = e.run(t, e.url("three.bin")+"\n", "task", "import", "-", "--format", "urls", "--paused", "-o", "json")
	if r.code != ExitOK {
		t.Fatalf("import from stdin: exit %d %s", r.code, r.stderr)
	}
	if r := e.run(t, "x", "task", "import", "-"); r.code != ExitUsage {
		t.Errorf("import without a format: exit %d, want usage error", r.code)
	}
	if r := e.run(t, "{", "task", "import", "-", "--format", "json"); r.code == ExitOK {
		t.Error("an unreadable list was accepted")
	}

	var st client.Stats
	e.ok(t, &st, "stats", "-o", "json")
	if st.Tasks != 5 || st.ByStatus["paused"] != 5 || len(st.Completed) != 3 {
		t.Errorf("stats = %+v", st)
	}
	if r := e.run(t, "", "stats"); r.code != ExitOK || !strings.Contains(r.stdout, "all tasks") || !strings.Contains(r.stdout, "completed in 24h") {
		t.Errorf("stats table: exit %d %q", r.code, r.stdout)
	}
}
