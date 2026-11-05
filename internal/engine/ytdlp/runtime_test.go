package ytdlp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fakeTool(t *testing.T, dir, name string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
}

func TestToolPathKeepsPathFirstAndAddsExistingDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	deno := filepath.Join(home, ".deno", "bin")
	if err := os.MkdirAll(deno, 0o755); err != nil { //nolint:gosec // test directory
		t.Fatal(err)
	}
	got := filepath.SplitList(toolPath("/first"))
	if got[0] != "/first" {
		t.Fatalf("PATH order changed: %v", got)
	}
	if !strings.Contains(strings.Join(got, "|"), deno) {
		t.Errorf("%s missing from %v", deno, got)
	}
	if strings.Contains(strings.Join(got, "|"), filepath.Join(home, ".bun")) {
		t.Errorf("a directory that does not exist was added: %v", got)
	}
}

func TestRuntimeArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-ins")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, ".bun", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil { //nolint:gosec // test directory
		t.Fatal(err)
	}
	t.Setenv("PATH", "/nonexistent")
	if got := runtimeArgs("2026.10.26"); got != nil && !hasDeno() {
		t.Fatalf("no runtime installed, got %v", got)
	}
	fakeTool(t, bin, "bun")
	if hasDeno() {
		t.Skip("a real deno is installed in a tool directory")
	}
	got := runtimeArgs("2026.10.26")
	if len(got) != 1 || got[0] != "--js-runtimes=bun:"+filepath.Join(bin, "bun") {
		t.Fatalf("got %v", got)
	}
	if got := runtimeArgs("2025.01.01"); got != nil {
		t.Errorf("an old yt-dlp does not know --js-runtimes, got %v", got)
	}
	if got := runtimeArgs(""); got != nil {
		t.Errorf("unknown version: got %v", got)
	}
	fakeTool(t, bin, "deno")
	if got := runtimeArgs("2026.10.26"); got != nil {
		t.Errorf("deno is yt-dlp's default, got %v", got)
	}
}

func hasDeno() bool {
	for _, d := range filepath.SplitList(toolPath("")) {
		if _, err := os.Stat(filepath.Join(d, "deno")); err == nil {
			return true
		}
	}
	return false
}

func TestClassifyJavaScriptRuntimeHint(t *testing.T) {
	err := classifyFailure("WARNING: [youtube] No supported JavaScript runtime could be found.\nERROR: [youtube] x: Requested format is not available.\n", os.ErrInvalid)
	if err == nil || !strings.Contains(err.Error(), "install deno") {
		t.Fatalf("got %v", err)
	}
}
