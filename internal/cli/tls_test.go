package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/instance"
)

func TestTLSInitCreatesFilesAndPrintsTheFingerprint(t *testing.T) {
	home := t.TempDir()
	r := runCLI(context.Background(), t, home, "tls", "init", "--host", "nas.local", "--apply", "-o", "json")
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	var out struct {
		Certificate, Key, Fingerprint string
		Names                         []string
		Applied                       bool
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	configDir := filepath.Join(home, "config")
	if out.Certificate != filepath.Join(configDir, "cert.pem") || out.Key != filepath.Join(configDir, "key.pem") || !out.Applied {
		t.Errorf("output = %+v", out)
	}
	if got, err := instance.Fingerprint(out.Certificate, out.Key); err != nil || got != out.Fingerprint {
		t.Errorf("fingerprint %q vs the certificate's %q (%v)", out.Fingerprint, got, err)
	}
	if cfg, err := os.ReadFile(filepath.Join(configDir, "config.yaml")); err != nil ||
		!strings.Contains(string(cfg), "tls_cert: "+out.Certificate) || !strings.Contains(string(cfg), "tls_key: "+out.Key) {
		t.Errorf("config file = %q, %v", cfg, err)
	}

	// A second run refuses to replace the certificate unless told to.
	if r := runCLI(context.Background(), t, home, "tls", "init"); r.code == ExitOK || !strings.Contains(r.stderr, "--force") {
		t.Errorf("second run: exit %d, stderr %q", r.code, r.stderr)
	}
	if r := runCLI(context.Background(), t, home, "tls", "init", "--force"); r.code != ExitOK {
		t.Errorf("--force: exit %d, stderr %q", r.code, r.stderr)
	}
}
