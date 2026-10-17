package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/client"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/daemon"
)

const (
	testToken      = "cli-test-token"
	testCLIVersion = "1.0.0-cli"
	testSrvVersion = "2.0.0-srv"
)

// startAPI serves buildServer's API on a random loopback port and returns its
// base URL. The server stops when the test ends.
func startAPI(t *testing.T) string {
	t.Helper()
	cfg := config.Default()
	cfg.Home = t.TempDir()
	srv, err := daemon.BuildServer(&cfg, api.Deps{Token: testToken, Version: testSrvVersion})
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
	return "http://" + ln.Addr().String()
}

type result struct {
	code           int
	stdout, stderr string
}

// runCLI runs the CLI hermetically: DLM_HOME points at home (a fresh temp
// dir when empty) and no other environment is visible.
func runCLI(ctx context.Context, t *testing.T, home string, args ...string) result {
	t.Helper()
	if home == "" {
		home = t.TempDir()
	}
	var stdout, stderr bytes.Buffer
	code := run(ctx, testCLIVersion, args,
		WithIO(&stdout, &stderr),
		WithEnviron(func() []string { return []string{"DLM_HOME=" + home} }),
	)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestHealthJSON(t *testing.T) {
	base := startAPI(t)
	r := runCLI(context.Background(), t, "", "--server", base, "--token", testToken, "health", "-o", "json")
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	var h client.Health
	if err := json.Unmarshal([]byte(r.stdout), &h); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, r.stdout)
	}
	if h != (client.Health{Status: "ok", Version: testSrvVersion, Authenticated: true}) {
		t.Errorf("health = %+v", h)
	}
}

func TestHealthTableAndYAML(t *testing.T) {
	base := startAPI(t)
	r := runCLI(context.Background(), t, "", "--server", base, "--token", testToken, "health")
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "STATUS VERSION AUTHENTICATED" ||
		strings.Join(strings.Fields(lines[1]), " ") != "ok "+testSrvVersion+" true" {
		t.Errorf("table output:\n%s", r.stdout)
	}

	r = runCLI(context.Background(), t, "", "--server", base, "--token", testToken, "health", "--output", "yaml")
	if r.code != ExitOK || !strings.Contains(r.stdout, "version: "+testSrvVersion) || !strings.Contains(r.stdout, "authenticated: true") {
		t.Errorf("yaml exit %d output:\n%s", r.code, r.stdout)
	}
}

func TestHealthReadsTokenFileFromConfig(t *testing.T) {
	base := startAPI(t)
	home := t.TempDir()
	tokenFile := filepath.Join(home, "config", "token")
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	address := strings.TrimPrefix(base, "http://")
	r := runCLI(context.Background(), t, "", "--home", home, "--address", address, "health", "-o", "json")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"authenticated": true`) {
		t.Fatalf("exit %d, stdout: %s, stderr: %s", r.code, r.stdout, r.stderr)
	}
}

func TestHealthBadTokenFails(t *testing.T) {
	base := startAPI(t)
	r := runCLI(context.Background(), t, "", "--server", base, "--token", "wrong", "health")
	if r.code != ExitError {
		t.Fatalf("exit %d, want %d; stderr: %s", r.code, ExitError, r.stderr)
	}
	if !strings.HasPrefix(r.stderr, "error: ") || !strings.Contains(r.stderr, "rejected the API token") {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestHealthWithoutTokenFileSucceeds(t *testing.T) {
	base := startAPI(t)
	r := runCLI(context.Background(), t, "", "--server", base, "health", "-o", "json")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"authenticated": false`) {
		t.Fatalf("exit %d, stdout: %s, stderr: %s", r.code, r.stdout, r.stderr)
	}
}

func TestDaemonNotRunning(t *testing.T) {
	addr := freeAddress(t)
	r := runCLI(context.Background(), t, "", "--address", addr, "health")
	if r.code != ExitError {
		t.Fatalf("exit %d, want %d", r.code, ExitError)
	}
	if !strings.Contains(r.stderr, "error: dlm daemon is not running at http://"+addr) {
		t.Errorf("stderr = %q", r.stderr)
	}
	if n := strings.Count(strings.TrimSpace(r.stderr), "\n"); n != 0 {
		t.Errorf("error is not a single line: %q", r.stderr)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"no-such-command"},
		{"health", "--no-such-flag"},
		{"health", "extra-arg"},
		{"-o", "xml", "version"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := runCLI(context.Background(), t, "", args...)
			if r.code != ExitUsage {
				t.Errorf("exit %d, want %d; stderr: %s", r.code, ExitUsage, r.stderr)
			}
			if !strings.HasPrefix(r.stderr, "error: ") {
				t.Errorf("stderr = %q", r.stderr)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	base := startAPI(t)
	r := runCLI(context.Background(), t, "", "--server", base, "--token", testToken, "version", "-o", "json")
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	var v versionInfo
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatal(err)
	}
	if v != (versionInfo{Client: testCLIVersion, Server: testSrvVersion}) {
		t.Errorf("version = %+v", v)
	}

	// Daemon down: still succeeds, with a note.
	r = runCLI(context.Background(), t, "", "--address", freeAddress(t), "version")
	if r.code != ExitOK || !strings.Contains(r.stdout, testCLIVersion) || strings.Contains(r.stdout, "server") {
		t.Errorf("exit %d stdout: %s", r.code, r.stdout)
	}
	if !strings.Contains(r.stderr, "note: daemon version unavailable") {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestTokenShow(t *testing.T) {
	home := t.TempDir()
	tokenFile := filepath.Join(home, "config", "token")

	r := runCLI(context.Background(), t, home, "token", "show", "-o", "json")
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	var info tokenInfo
	if err := json.Unmarshal([]byte(r.stdout), &info); err != nil {
		t.Fatal(err)
	}
	if info != (tokenInfo{Path: tokenFile}) {
		t.Errorf("info = %+v", info)
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token show created the token file (stat err = %v)", err)
	}
	if r := runCLI(context.Background(), t, home, "token", "show", "--reveal"); r.code != ExitError {
		t.Errorf("--reveal without token: exit %d", r.code)
	}

	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("sekret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runCLI(context.Background(), t, home, "token", "show")
	if r.code != ExitOK || strings.Contains(r.stdout, "sekret") || !strings.Contains(r.stdout, tokenFile) {
		t.Errorf("without --reveal: exit %d stdout: %s", r.code, r.stdout)
	}
	r = runCLI(context.Background(), t, home, "token", "show", "--reveal", "-o", "json")
	if err := json.Unmarshal([]byte(r.stdout), &info); err != nil {
		t.Fatal(err)
	}
	if info != (tokenInfo{Path: tokenFile, Exists: true, Token: "sekret"}) {
		t.Errorf("info = %+v", info)
	}
}

func TestServeEndToEnd(t *testing.T) {
	home := t.TempDir()
	addr := freeAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan result, 1)
	go func() { done <- runCLI(ctx, t, home, "serve", "--home", home, "--address", addr) }()

	tokenFile := filepath.Join(home, "config", "token")
	waitHealthy(t, "http://"+addr, done)

	if _, err := os.Stat(tokenFile); err != nil {
		t.Fatalf("token file not created: %v", err)
	}
	// The client finds the daemon and its token through the same config.
	r := runCLI(context.Background(), t, home, "--home", home, "--address", addr, "health", "-o", "json")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"authenticated": true`) {
		t.Errorf("health via config: exit %d stdout: %s stderr: %s", r.code, r.stdout, r.stderr)
	}

	cancel()
	select {
	case r := <-done:
		if r.code != ExitOK {
			t.Fatalf("serve exit %d, stderr: %s", r.code, r.stderr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop after cancellation")
	}
	if _, err := os.Stat(filepath.Join(home, "data", "logs", "dlm.log")); err != nil {
		t.Errorf("log file not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "data", "dlm.db")); err != nil {
		t.Errorf("database not created: %v", err)
	}
}

// waitHealthy polls get-health until the daemon answers, failing early if
// serve exits.
func waitHealthy(t *testing.T, base string, done <-chan result) {
	t.Helper()
	c, err := client.New(base, "", client.WithTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case r := <-done:
			t.Fatalf("serve exited early with %d: %s", r.code, r.stderr)
		default:
		}
		if _, err := c.Health(context.Background()); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("daemon not healthy: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// freeAddress returns a loopback address with no listener.
func freeAddress(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
