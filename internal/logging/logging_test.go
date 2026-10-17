package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/config"
)

func TestFileJSONLogging(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default().Log
	l, err := New(cfg, dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("hello", "task", "t1")
	l.Debug("hidden")
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 line (debug filtered), got %d: %q", len(lines), data)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec["msg"] != "hello" || rec["task"] != "t1" {
		t.Errorf("unexpected record %v", rec)
	}
}

func TestStderrTextAndDynamicLevel(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.LogConfig{Level: "warn", Format: "text", File: false}
	l, err := New(cfg, t.TempDir(), &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("dropped")
	l.Level.Set(slog.LevelDebug)
	l.Debug("kept")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "dropped") || !strings.Contains(out, "msg=kept") {
		t.Errorf("unexpected output %q", out)
	}
}

func TestInvalidConfig(t *testing.T) {
	if _, err := New(config.LogConfig{Level: "nope", Format: "json"}, t.TempDir(), nil); err == nil {
		t.Error("expected invalid level error")
	}
	if _, err := New(config.LogConfig{Level: "info", Format: "xml"}, t.TempDir(), nil); err == nil {
		t.Error("expected invalid format error")
	}
}
