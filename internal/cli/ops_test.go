package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/client"
	"github.com/sabadia/dlm-server/internal/daemon"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

// e2e is a CLI test environment: a real API server with real services on a
// temp SQLite store, and a local origin server for download URLs.
type e2e struct {
	base   string
	origin string
	home   string
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
	svc := daemon.NewServices(service.Deps{
		Store: store, Engines: reg, Resolves: resolves,
		Defaults: func() settings.Settings { return defaults },
	})
	base := startAPIWith(t, api.Deps{
		Token: testToken, Version: testSrvVersion,
		Settings: svc.Settings, Queues: svc.Queues, Tasks: svc.Tasks,
	})
	content := bytes.Repeat([]byte("x"), 2048)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Unix(1_700_000_000, 0), bytes.NewReader(content))
	}))
	t.Cleanup(origin.Close)
	return &e2e{base: base, origin: origin.URL, home: t.TempDir()}
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
