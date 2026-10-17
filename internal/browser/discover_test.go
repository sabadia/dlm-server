package browser

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sabadia/dlm-server/internal/platform"
)

func TestDiscoverPreferenceAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	chrome := filepath.Join(dir, "chrome")
	edge := filepath.Join(dir, "edge")
	for _, p := range []string{chrome, edge} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	candidates := []platform.BrowserCandidate{
		{Browser: platform.BrowserChrome, Path: filepath.Join(dir, "missing")},
		{Browser: platform.BrowserChrome, Path: "google-chrome", Lookup: true},
		{Browser: platform.BrowserChrome, Path: chrome},
		{Browser: platform.BrowserChromium, Path: "chromium", Lookup: true}, // not on PATH
		{Browser: platform.BrowserEdge, Path: edge},
	}
	lookPath := func(name string) (string, error) {
		if name == "google-chrome" {
			return chrome, nil // the same file as the next candidate
		}
		return "", errors.New("not found")
	}
	got := discover(candidates, lookPath, statFile)
	if len(got) != 2 || got[0].Name != "chrome" || got[1].Name != "edge" {
		t.Fatalf("discover = %+v, want chrome then edge, once each", got)
	}
	if want, _ := filepath.EvalSymlinks(chrome); got[0].Path != want {
		t.Errorf("path = %q, want %q", got[0].Path, want)
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "b")
	if err := os.WriteFile(file, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if inst, err := Validate(file); err != nil || inst.Path != file {
		t.Errorf("Validate(file) = %+v, %v", inst, err)
	}
	for _, p := range []string{filepath.Join(dir, "missing"), dir} {
		if _, err := Validate(p); err == nil {
			t.Errorf("Validate(%q) accepted", p)
		}
	}
}
