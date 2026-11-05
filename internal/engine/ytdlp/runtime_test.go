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
	dir := t.TempDir()
	if got := runtimeArgsIn("2026.10.26", dir); got != nil {
		t.Fatalf("no runtime installed, got %v", got)
	}
	fakeTool(t, dir, "bun")
	got := runtimeArgsIn("2026.10.26", dir)
	if len(got) != 1 || got[0] != "--js-runtimes=bun:"+filepath.Join(dir, "bun") {
		t.Fatalf("got %v", got)
	}
	if got := runtimeArgsIn("2025.01.01", dir); got != nil {
		t.Errorf("an old yt-dlp does not know --js-runtimes, got %v", got)
	}
	if got := runtimeArgsIn("", dir); got != nil {
		t.Errorf("unknown version: got %v", got)
	}
	fakeTool(t, dir, "deno")
	if got := runtimeArgsIn("2026.10.26", dir); got != nil {
		t.Errorf("deno is yt-dlp's default, got %v", got)
	}
}

func TestClassifyJavaScriptRuntimeHint(t *testing.T) {
	err := classifyFailure("WARNING: [youtube] No supported JavaScript runtime could be found.\nERROR: [youtube] x: Requested format is not available.\n", os.ErrInvalid)
	if err == nil || !strings.Contains(err.Error(), "install deno") {
		t.Fatalf("got %v", err)
	}
}
