package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/instance"
)

// testConfig is a daemon configuration that listens on a free port and keeps
// everything under a temporary home.
func testConfig(t *testing.T, home string) *config.Config {
	t.Helper()
	env := []string{
		"DLM_HOME=" + home,
		"DLM_SERVER_ADDRESS=127.0.0.1:0",
		"DLM_SERVER_SHUTDOWN_TIMEOUT=5s",
		"DLM_LOG_FILE=false",
		"DLM_TORRENT_ENABLED=false",
		"DLM_ED2K_ENABLED=false",
	}
	cfg, err := config.Load(config.LoadOptions{Environ: func() []string { return env }})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type running struct {
	done chan error
	info instance.Info
}

// start runs a daemon until the test ends and waits for it to publish itself.
func start(t *testing.T, cfg *config.Config) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{done: make(chan error, 1)}
	go func() { r.done <- Run(ctx, cfg, "test", io.Discard) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done: //nolint:errcheck // the test body already checked what Run returned
		case <-time.After(15 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-r.done:
			t.Fatalf("the daemon stopped while starting: %v", err)
		default:
		}
		if info, err := instance.Discover(cfg.Paths.ConfigDir); err == nil {
			r.info = info
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the daemon did not publish runtime.json")
	return nil
}

func (r *running) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err // the cleanup waits for it too
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the daemon did not stop")
		return nil
	}
}

func call(t *testing.T, method, url, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestDaemonPublishesItsRealAddressAndIsExclusive(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	r := start(t, cfg)

	host, port, err := net.SplitHostPort(r.info.Address)
	if err != nil || host != "127.0.0.1" || port == "0" || port == "" {
		t.Fatalf("published address %q is not a real loopback address", r.info.Address)
	}
	if r.info.PID != os.Getpid() || r.info.Version != "test" || r.info.URL != "http://"+r.info.Address {
		t.Errorf("runtime info = %+v", r.info)
	}
	if status, body := call(t, http.MethodGet, r.info.URL+"/v1/health", ""); status != http.StatusOK {
		t.Fatalf("health through the published URL: %d %s", status, body)
	}

	// A second daemon for the same configuration directory is refused.
	err = Run(t.Context(), cfg, "second", io.Discard)
	if !errors.Is(err, instance.ErrLocked) {
		t.Errorf("second daemon error = %v, want ErrLocked", err)
	}
}

func TestSystemInfoReportsTheDaemon(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	r := start(t, cfg)
	token, err := auth.ReadToken(cfg.TokenFile())
	if err != nil {
		t.Fatal(err)
	}
	status, body := call(t, http.MethodGet, r.info.URL+"/v1/system/info", token)
	if status != http.StatusOK {
		t.Fatalf("system info: %d %s", status, body)
	}
	var info struct {
		Version, Address string
		PID              int
		Local, TLS       bool
		APIVersion       int
		Paths            struct{ Config, Database string }
		Engines          []string
		Tools            []struct {
			Name      string
			Available bool
			Error     string
		}
		DatabaseBytes int64
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if info.Version != "test" || info.PID != os.Getpid() || info.Address != r.info.Address || !info.Local || info.TLS || info.APIVersion < 1 {
		t.Errorf("info = %+v", info)
	}
	if info.Paths.Config != cfg.Paths.ConfigDir || info.Paths.Database != cfg.DatabaseFile() || info.DatabaseBytes <= 0 {
		t.Errorf("paths/database = %+v / %d", info.Paths, info.DatabaseBytes)
	}
	if len(info.Engines) == 0 || strings.Contains(strings.Join(info.Engines, ","), "bt") {
		t.Errorf("engines = %v (torrent is disabled)", info.Engines)
	}
	names := map[string]bool{}
	for _, tool := range info.Tools {
		names[tool.Name] = true
		if !tool.Available && tool.Error == "" {
			t.Errorf("tool %s is unavailable without a reason", tool.Name)
		}
	}
	for _, want := range []string{"ffmpeg", "yt-dlp", "browser"} {
		if !names[want] {
			t.Errorf("tool %s missing from %v", want, info.Tools)
		}
	}
	if status, _ := call(t, http.MethodGet, r.info.URL+"/v1/system/info", ""); status != http.StatusUnauthorized {
		t.Errorf("system info without a token: %d", status)
	}
}

func TestShutdownAndRestartThroughTheAPI(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	token := ""

	r := start(t, cfg)
	token, _ = auth.ReadToken(cfg.TokenFile())
	if status, body := call(t, http.MethodPost, r.info.URL+"/v1/system/restart", token); status != http.StatusAccepted {
		t.Fatalf("restart: %d %s", status, body)
	}
	if err := r.wait(t); !errors.Is(err, ErrRestart) {
		t.Fatalf("Run after restart-daemon = %v, want ErrRestart", err)
	}
	if _, err := instance.Discover(cfg.Paths.ConfigDir); !errors.Is(err, instance.ErrNotRunning) {
		t.Errorf("after the restart stop: %v, want ErrNotRunning", err)
	}
	if _, err := os.Stat(instance.RuntimePath(cfg.Paths.ConfigDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("runtime.json survived the shutdown: %v", err)
	}

	// The caller starts it again (as the CLI does); a plain shutdown then ends it for good.
	r = start(t, cfg)
	if status, body := call(t, http.MethodPost, r.info.URL+"/v1/system/shutdown", token); status != http.StatusAccepted {
		t.Fatalf("shutdown: %d %s", status, body)
	}
	if err := r.wait(t); err != nil {
		t.Errorf("Run after shutdown-daemon = %v, want nil", err)
	}
	if held, _ := instance.Held(cfg.Paths.ConfigDir); held {
		t.Error("the lock was not released")
	}
}
