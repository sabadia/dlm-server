package cli

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/client"
	"github.com/sabadia/dlm-server/internal/instance"
)

type fakeSystem struct{}

func (fakeSystem) SystemInfo(context.Context) api.SystemInfo {
	return api.SystemInfo{
		Version: "fake", APIVersion: api.APIVersion, MinClientAPI: api.MinClientAPI, GoVersion: "go-test", OS: "testos", Arch: "testarch",
		PID: 99, StartedAt: time.Now().Add(-time.Minute), Address: "127.0.0.1:1", Engines: []string{"http", "hls"},
		Paths: api.SystemPaths{Config: "/cfg", Database: "/data/dlm.db", Downloads: "/dl"}, DatabaseBytes: 4096,
		Tools: []api.ToolStatus{
			{Name: "ffmpeg", Available: true, Path: "/usr/bin/ffmpeg", Version: "7.0", Source: "path"},
			{Name: "yt-dlp", Error: "yt-dlp was not found"},
		},
	}
}

type fakeLifecycle struct {
	mu    sync.Mutex
	calls []bool
}

func (l *fakeLifecycle) RequestShutdown(restart bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, restart)
}

func (l *fakeLifecycle) requests() []bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]bool(nil), l.calls...)
}

func TestSystemInfoCommand(t *testing.T) {
	e := newE2E(t)
	var info client.SystemInfo
	e.ok(t, &info, "system", "info", "-o", "json")
	if info.Version != "fake" || info.PID != 99 || !info.Local || len(info.Tools) != 2 || info.DatabaseBytes != 4096 || info.UptimeSeconds < 59 {
		t.Errorf("info = %+v", info)
	}
	table := e.ok(t, nil, "system", "info")
	for _, want := range []string{"fake", "testos/testarch", "/data/dlm.db (4096 bytes)", "http, hls", "7.0 path /usr/bin/ffmpeg", "missing: yt-dlp was not found"} {
		if !strings.Contains(table, want) {
			t.Errorf("table lacks %q:\n%s", want, table)
		}
	}
}

func TestSystemStopAndRestartCommands(t *testing.T) {
	e := newE2E(t)
	var res struct{ Status string }
	e.ok(t, &res, "system", "stop", "-o", "json")
	if res.Status != "shutting_down" {
		t.Errorf("stop status = %q", res.Status)
	}
	e.ok(t, &res, "system", "restart", "-o", "json")
	if res.Status != "restarting" {
		t.Errorf("restart status = %q", res.Status)
	}
	if got := e.life.requests(); len(got) != 2 || got[0] || !got[1] {
		t.Errorf("shutdown requests = %v, want [false true]", got)
	}
}

// publishedDaemon makes dir look like the config directory of a daemon at base.
func publishedDaemon(t *testing.T, dir, base string) {
	t.Helper()
	lock, err := instance.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	if err := instance.Write(dir, instance.Info{PID: 1, Address: strings.TrimPrefix(base, "http://"), URL: base, Version: "t"}); err != nil {
		t.Fatal(err)
	}
}

func TestClientFindsTheDaemonThroughRuntimeFile(t *testing.T) {
	base := startAPI(t)
	home := t.TempDir()
	cfgDir := home + "/config"
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	publishedDaemon(t, cfgDir, base)

	// No --server and no --address: the runtime file says where the daemon is.
	r := runCLI(context.Background(), t, "", "--home", home, "--token", testToken, "health", "-o", "json")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"authenticated": true`) {
		t.Fatalf("exit %d, stdout: %s, stderr: %s", r.code, r.stdout, r.stderr)
	}

	// An explicit --address wins over the runtime file.
	r = runCLI(context.Background(), t, "", "--home", home, "--address", freeAddress(t), "--token", testToken, "health")
	if r.code != ExitError || !strings.Contains(r.stderr, "not running") {
		t.Errorf("explicit --address: exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestClientIgnoresAStaleRuntimeFile(t *testing.T) {
	base := startAPI(t)
	home := t.TempDir()
	cfgDir := home + "/config"
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A runtime file without a lock holder is what a crashed daemon leaves.
	if err := instance.Write(cfgDir, instance.Info{PID: 1, Address: strings.TrimPrefix(base, "http://"), URL: base}); err != nil {
		t.Fatal(err)
	}
	r := runCLI(context.Background(), t, "", "--home", home, "--token", testToken, "health")
	if r.code != ExitError {
		t.Fatalf("a stale runtime file must not be trusted: exit %d, stdout %s", r.code, r.stdout)
	}
}
