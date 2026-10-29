package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sabadia/dlm-server/internal/platform"
)

func TestServiceInstallStatusUninstall(t *testing.T) {
	cfgDir := t.TempDir()
	var mu sync.Mutex
	var calls []string
	old := autostart
	autostart = &platform.Autostart{
		GOOS: "linux", Home: t.TempDir(), UserConfigDir: cfgDir,
		Run: func(_ context.Context, c platform.Command) error {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, c.String())
			return nil
		},
	}
	t.Cleanup(func() { autostart = old })
	home := t.TempDir()

	var st platform.AutostartStatus
	r := runCLI(t.Context(), t, home, "service", "status", "-o", "json")
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &st) != nil || st.Installed {
		t.Fatalf("status before: exit %d %s", r.code, r.stdout)
	}

	r = runCLI(t.Context(), t, home, "service", "install", "-o", "json")
	if r.code != ExitOK {
		t.Fatalf("install: exit %d, stderr %s", r.code, r.stderr)
	}
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil || !st.Installed || !st.Active || st.Kind != "systemd" {
		t.Fatalf("install output %s (%v)", r.stdout, err)
	}
	unit, err := os.ReadFile(filepath.Join(cfgDir, "systemd", "user", "dlm.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), " serve --home "+home) {
		t.Errorf("the unit does not carry --home:\n%s", unit)
	}

	r = runCLI(t.Context(), t, home, "service", "uninstall", "-o", "json")
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &st) != nil || st.Installed {
		t.Fatalf("uninstall: exit %d %s %s", r.code, r.stdout, r.stderr)
	}
}
