package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/nativehost"
)

const testExtID = "abcdefghijklmnopabcdefghijklmnop"

// runHost runs `dlm native-host args...` with the given raw requests on stdin
// and returns the decoded replies.
func (e *e2e) runHost(t *testing.T, env, requests []string, args ...string) []map[string]any {
	t.Helper()
	var in bytes.Buffer
	for _, r := range requests {
		if err := nativehost.WriteMessage(&in, []byte(r)); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	full := append([]string{"--server", e.base, "native-host"}, args...)
	code := run(context.Background(), testCLIVersion, full,
		WithIO(&stdout, &stderr),
		WithStdin(&in),
		WithEnviron(func() []string { return append([]string{"DLM_HOME=" + e.home}, env...) }),
	)
	if code != ExitOK {
		t.Fatalf("native-host: exit %d\nstderr: %s", code, stderr.String())
	}
	var out []map[string]any
	for {
		raw, err := nativehost.ReadMessage(&stdout, nativehost.MaxResponseSize)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("stdout is not a clean frame stream: %v (stderr %s)", err, stderr.String())
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
}

func (e *e2e) writeToken(t *testing.T) string {
	t.Helper()
	token, err := auth.LoadOrCreateToken(filepath.Join(e.home, "config", "token"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestNativeHostServesConnect(t *testing.T) {
	e := newE2E(t)

	// Before the daemon created a token there is nothing to hand out.
	got := e.runHost(t, nil, []string{`{"type":"connect"}`})
	if len(got) != 1 || got[0]["ok"] != false || got[0]["error"] != nativehost.CodeTokenUnavailable {
		t.Fatalf("without a token: %v", got)
	}

	token := e.writeToken(t)
	origin := "chrome-extension://" + testExtID + "/"
	allowed := []string{"DLM_SERVER_ALLOWED_ORIGINS=chrome-extension://" + testExtID}

	// Browser arguments: the caller origin, and Windows' --parent-window.
	got = e.runHost(t, allowed, []string{`{"type":"connect","id":1}`, `{"type":"ping"}`}, origin, "--parent-window=0")
	if len(got) != 2 {
		t.Fatalf("replies = %v", got)
	}
	r := got[0]
	if r["ok"] != true || r["token"] != token || r["baseUrl"] != e.base || r["running"] != true || r["version"] != testSrvVersion || r["id"] != float64(1) {
		t.Errorf("connect = %v, want the token, base URL %s and the running daemon's version", r, e.base)
	}
	if got[1]["ok"] != true || got[1]["type"] != "ping" {
		t.Errorf("ping = %v", got[1])
	}

	// An extension that is not allowed gets no token.
	got = e.runHost(t, nil, []string{`{"type":"connect"}`}, origin)
	if len(got) != 1 || got[0]["error"] != nativehost.CodeOriginNotAllowed || got[0]["token"] != nil {
		t.Errorf("unlisted origin: %v", got)
	}
	// Firefox passes the manifest path instead; the manifest restricts it.
	got = e.runHost(t, nil, []string{`{"type":"connect"}`}, filepath.Join(e.home, "m.json"), "dlm@example.org")
	if len(got) != 1 || got[0]["ok"] != true {
		t.Errorf("firefox-style arguments: %v", got)
	}
}

func TestNativeHostReportsDownDaemon(t *testing.T) {
	e := newE2E(t)
	e.writeToken(t)
	var in bytes.Buffer
	_ = nativehost.WriteMessage(&in, []byte(`{"type":"connect"}`))
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), testCLIVersion, []string{"--server", "http://127.0.0.1:1", "native-host"},
		WithIO(&stdout, &stderr), WithStdin(&in),
		WithEnviron(func() []string { return []string{"DLM_HOME=" + e.home} }))
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	raw, err := nativehost.ReadMessage(&stdout, nativehost.MaxResponseSize)
	if err != nil || !bytes.Contains(raw, []byte(`"running":false`)) || !bytes.Contains(raw, []byte(`"ok":true`)) {
		t.Errorf("reply = %s, %v; want ok with running=false", raw, err)
	}
}

func TestNativeHostInstallUninstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("installing on Windows writes the real registry; the registry path is covered with a fake in internal/nativehost")
	}
	e := newE2E(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	var results []nativehost.Result
	e.ok(t, &results, "native-host", "install", "--extension-id", testExtID, "--firefox-id", "dlm@example.org", "-o", "json")
	if len(results) != 5 {
		t.Fatalf("results = %+v", results)
	}
	launcher := filepath.Join(e.home, "config", "native-messaging", "dlm-native-host")
	script, err := os.ReadFile(launcher)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "native-host --config-dir '"+filepath.Join(e.home, "config")+"' --server '"+e.base+"'") {
		t.Errorf("launcher:\n%s", script)
	}
	for _, r := range results {
		if r.Status != nativehost.StatusInstalled || !strings.HasPrefix(r.Manifest, home) {
			t.Errorf("result = %+v", r)
			continue
		}
		data, err := os.ReadFile(r.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), launcher) {
			t.Errorf("%s manifest does not point to the launcher:\n%s", r.Browser, data)
		}
	}

	out := e.ok(t, nil, "native-host", "uninstall", "--browser", "firefox")
	if !strings.Contains(out, "firefox") || !strings.Contains(out, "removed") {
		t.Errorf("uninstall table:\n%s", out)
	}
	if _, err := os.Stat(launcher); err != nil {
		t.Errorf("launcher removed by a partial uninstall: %v", err)
	}
	e.ok(t, &results, "native-host", "uninstall", "-o", "json")
	if _, err := os.Stat(launcher); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("launcher not removed by a full uninstall: %v", err)
	}
}

func TestNativeHostInstallErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("see TestNativeHostInstallUninstall")
	}
	e := newE2E(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	if r := e.run(t, "", "native-host", "install", "--extension-id", "not-an-id"); r.code != ExitUsage || !strings.Contains(r.stderr, "not a Chromium extension ID") {
		t.Errorf("bad id: exit %d stderr %s", r.code, r.stderr)
	}
	if r := e.run(t, "", "native-host", "install", "--browser", "netscape", "--extension-id", testExtID); r.code != ExitUsage || !strings.Contains(r.stderr, "unknown browser") {
		t.Errorf("bad browser: exit %d stderr %s", r.code, r.stderr)
	}
	// Without any ID nothing is installed, which is an error.
	r := e.run(t, "", "native-host", "install")
	if r.code != ExitError || !strings.Contains(r.stderr, "nothing installed") || !strings.Contains(r.stdout, "skipped") {
		t.Errorf("no ids: exit %d stdout %s stderr %s", r.code, r.stdout, r.stderr)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Errorf("files written although nothing was installed: %v", entries)
	}
}
