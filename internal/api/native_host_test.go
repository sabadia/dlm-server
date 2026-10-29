package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/nativehost"
)

type noRegistry struct{}

func (noRegistry) Set(string, string) error { return nil }
func (noRegistry) Delete(string) error      { return nil }

// newNativeHostServer builds a server whose installer works in a temporary
// home, never in the real browser directories.
func newNativeHostServer(t *testing.T, origins ...string) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	cfg := config.Default()
	cfg.Server.AllowedOrigins = origins
	s, err := New(&cfg, Deps{
		Token: testToken, Version: testVersion,
		NativeHost: func() (*nativehost.Installer, error) {
			return &nativehost.Installer{
				Executable: filepath.Join(home, "bin", "dlm"), ConfigDir: filepath.Join(home, "cfg"), DataDir: filepath.Join(home, "cfg"),
				ServerURL: "http://127.0.0.1:1", GOOS: "linux", Home: home, UserConfigDir: filepath.Join(home, ".config"), Registry: noRegistry{},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, home
}

// localCall sends a request that comes from the loopback address.
func localCall(t *testing.T, s *Server, method, body string, want int, out any) {
	t.Helper()
	const path = "/v1/native-host"
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = net.JoinHostPort("127.0.0.1", "4321")
	req.Header.Set("Authorization", "Bearer "+testToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != want {
		t.Fatalf("%s %s = %d, want %d; body: %s", method, path, w.Code, want, w.Body)
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decode: %v; body %s", err, w.Body)
		}
	}
}

func TestNativeHostStatusInstallUninstall(t *testing.T) {
	s, home := newNativeHostServer(t, "chrome-extension://aaaabbbbccccddddeeeeffffgggghhhh")

	var st NativeHostStatus
	localCall(t, s, http.MethodGet, "", http.StatusOK, &st)
	if st.LauncherExists || len(st.Browsers) == 0 || len(st.AllowedOrigins) != 1 {
		t.Fatalf("before = %+v", st)
	}
	for _, b := range st.Browsers {
		if b.Status != nativehost.StatusAbsent {
			t.Errorf("%s = %s before installing", b.Browser, b.Status)
		}
	}

	// The extension ID comes from server.allowed_origins when not given.
	var res []nativehost.Result
	localCall(t, s, http.MethodPost, `{"action":"install","browsers":["chrome"]}`, http.StatusOK, &res)
	if len(res) != 1 || res[0].Status != nativehost.StatusInstalled || !strings.HasPrefix(res[0].Manifest, home) {
		t.Fatalf("install = %+v", res)
	}
	data, err := os.ReadFile(res[0].Manifest)
	if err != nil || !strings.Contains(string(data), "chrome-extension://aaaabbbbccccddddeeeeffffgggghhhh/") {
		t.Errorf("manifest = %q, %v", data, err)
	}
	localCall(t, s, http.MethodGet, "", http.StatusOK, &st)
	if !st.LauncherExists {
		t.Error("the launcher was not written")
	}
	installed := 0
	for _, b := range st.Browsers {
		if b.Status == nativehost.StatusInstalled {
			installed++
		}
	}
	if installed != 1 {
		t.Errorf("%d browsers installed, want 1: %+v", installed, st.Browsers)
	}

	localCall(t, s, http.MethodPost, `{"action":"uninstall"}`, http.StatusOK, &res)
	if _, err := os.Stat(st.Launcher); err == nil {
		t.Error("uninstalling every browser kept the launcher")
	}
	localCall(t, s, http.MethodGet, "", http.StatusOK, &st)
	for _, b := range st.Browsers {
		if b.Status != nativehost.StatusAbsent {
			t.Errorf("%s = %s after uninstalling", b.Browser, b.Status)
		}
	}
}

func TestNativeHostRefusals(t *testing.T) {
	s, _ := newNativeHostServer(t) // no allowed origins

	// A request from another machine is refused.
	remote := do(t, s, request{method: http.MethodGet, path: "/v1/native-host", headers: bearer(testToken)})
	if remote.Code != http.StatusForbidden || !strings.Contains(remote.Body.String(), `"code":"native_host.local_only"`) {
		t.Errorf("remote get = %d %s", remote.Code, remote.Body)
	}
	remote = do(t, s, request{method: http.MethodPost, path: "/v1/native-host", body: `{"action":"uninstall"}`, headers: bearer(testToken)})
	if remote.Code != http.StatusForbidden {
		t.Errorf("remote post = %d", remote.Code)
	}

	localCall(t, s, http.MethodPost, `{"action":"install","extensionIds":["not an id"]}`, http.StatusUnprocessableEntity, nil)
	localCall(t, s, http.MethodPost, `{"action":"install","browsers":["netscape"],"extensionIds":["aaaabbbbccccddddeeeeffffgggghhhh"]}`, http.StatusUnprocessableEntity, nil)
	localCall(t, s, http.MethodPost, `{"action":"explode"}`, http.StatusUnprocessableEntity, nil)
}
