package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func loadWithFile(t *testing.T, fileContent string, env []string, flags ...string) (*Config, string) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, "config", "config.yaml")
	if fileContent != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fileContent), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load(LoadOptions{Environ: environ(append([]string{"DLM_HOME=" + home}, env...)...), Flags: newFlags(t, flags...)})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

func entry(t *testing.T, v *View, key string) Entry {
	t.Helper()
	for _, e := range v.Entries {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("no entry %q", key)
	return Entry{}
}

func TestDescribeTellsTheSourceOfEveryValue(t *testing.T) {
	cfg, path := loadWithFile(t, "log:\n  level: debug\n  stderr: true\nserver:\n  shutdown_timeout: 30s\n",
		[]string{"DLM_LOG_STDERR=false", "DLM_SERVER_FS_ROOTS=" + os.TempDir()}, "--log-stderr=true")
	v, err := cfg.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if v.File != path || !v.FileExists || v.FileError != "" || v.RestartPending {
		t.Errorf("view = %+v", v)
	}
	for key, want := range map[string]Source{
		"log.level":               SourceFile,
		"log.stderr":              SourceFlag, // the flag beats the environment and the file
		"server.fs_roots":         SourceEnv,
		"server.shutdown_timeout": SourceFile,
		"server.address":          SourceDefault,
		"torrent.dht":             SourceDefault,
		"http.tls_impersonation":  SourceDefault,
	} {
		if got := entry(t, v, key).Source; got != want {
			t.Errorf("%s: source %q, want %q", key, got, want)
		}
	}
	if e := entry(t, v, "server.shutdown_timeout"); e.Type != TypeDuration || e.Value != "30s" || e.Default != "10s" || !e.RestartRequired {
		t.Errorf("shutdown_timeout = %+v", e)
	}
	if e := entry(t, v, "server.fs_roots"); e.Type != TypeList {
		t.Errorf("fs_roots = %+v", e)
	}
	if e := entry(t, v, "log.max_size_mb"); e.Type != TypeInt || e.Value != int64(20) {
		t.Errorf("max_size_mb = %+v", e)
	}
	if e := entry(t, v, "home"); !e.ReadOnly {
		t.Errorf("home = %+v", e)
	}
}

func TestPatchWritesTheFileAndDescribeShowsWhatIsPending(t *testing.T) {
	cfg, path := loadWithFile(t, "# my config\nlog:\n  level: debug\nserver:\n  allowed_origins:\n    - chrome-extension://abc\n", []string{"DLM_LOG_FORMAT=text"})

	err := cfg.Patch(map[string]any{
		"log.level":               "warn",
		"log.format":              "json", // an environment variable overrides it, but the file is written all the same
		"torrent.dht":             false,
		"server.shutdown_timeout": "45s",
		"server.fs_roots":         []any{os.TempDir()},
		"log.max_size_mb":         float64(50),
		"server.allowed_origins":  nil, // back to the default
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"level: warn", "shutdown_timeout: 45s", "max_size_mb: 50", "dht: false"} {
		if !strings.Contains(text, want) {
			t.Errorf("file lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "chrome-extension") {
		t.Errorf("a removed key is still in the file:\n%s", text)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode())
	}

	v, err := cfg.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if !v.RestartPending {
		t.Error("a change was written but nothing is pending")
	}
	if e := entry(t, v, "log.level"); e.Value != "debug" || e.Pending != "warn" {
		t.Errorf("log.level = %+v: the running value stays, the file's is pending", e)
	}
	if e := entry(t, v, "log.format"); e.Source != SourceEnv || e.Pending != nil {
		t.Errorf("log.format = %+v: an environment variable overrides the file, so nothing is pending", e)
	}
	if pending, ok := entry(t, v, "server.allowed_origins").Pending.([]string); !ok || len(pending) != 0 {
		t.Errorf("allowed_origins = %+v: removing the key makes the default pending", entry(t, v, "server.allowed_origins"))
	}
	if e := entry(t, v, "server.shutdown_timeout"); e.Pending != "45s" {
		t.Errorf("shutdown_timeout = %+v", e)
	}

	// The next start picks it all up.
	again, err := Load(LoadOptions{Environ: environ("DLM_HOME=" + cfg.Home)})
	if err != nil {
		t.Fatal(err)
	}
	if again.Log.Level != "warn" || again.Torrent.DHT || again.Log.MaxSizeMB != 50 || len(again.Server.AllowedOrigins) != 0 {
		t.Errorf("reloaded: %+v", again)
	}
}

func TestPatchCreatesTheFile(t *testing.T) {
	cfg, path := loadWithFile(t, "", nil)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the file exists before the patch")
	}
	if err := cfg.Patch(map[string]any{"log.level": "error"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "level: error") {
		t.Errorf("file = %q, %v", data, err)
	}
}

func TestPatchRefusesWhatIsWrongAndWritesNothing(t *testing.T) {
	original := "log:\n  level: debug\n"
	cfg, path := loadWithFile(t, original, nil)
	for name, tc := range map[string]struct {
		changes map[string]any
		key     string
		code    string
	}{
		"unknown key":         {map[string]any{"no.such": "x"}, "no.such", "config.unknown_key"},
		"read only":           {map[string]any{"paths.config_dir": "/elsewhere"}, "paths.config_dir", "config.read_only"},
		"home":                {map[string]any{"home": "/x"}, "home", "config.read_only"},
		"string for a bool":   {map[string]any{"torrent.dht": "yes"}, "torrent.dht", "config.bad_value"},
		"fraction for an int": {map[string]any{"log.max_size_mb": 1.5}, "log.max_size_mb", "config.bad_value"},
		"bad duration":        {map[string]any{"server.shutdown_timeout": "soon"}, "server.shutdown_timeout", "config.bad_value"},
		"list of numbers":     {map[string]any{"server.fs_roots": []any{1}}, "server.fs_roots", "config.bad_value"},
		"invalid as a whole":  {map[string]any{"log.level": "loud"}, "", "config.invalid"},
		"relative fs root":    {map[string]any{"server.fs_roots": []any{"relative"}}, "", "config.invalid"},
		"bad timeout":         {map[string]any{"server.shutdown_timeout": "-5s"}, "", "config.invalid"},
	} {
		err := cfg.Patch(tc.changes)
		var pe *PatchError
		if !errors.As(err, &pe) || pe.Code != tc.code || pe.Key != tc.key {
			t.Errorf("%s: err = %v, want code %q key %q", name, err, tc.code, tc.key)
		}
		if data, _ := os.ReadFile(path); string(data) != original {
			t.Errorf("%s: a refused patch changed the file:\n%s", name, data)
		}
	}
}

func TestDescribeReportsAnInvalidFile(t *testing.T) {
	cfg, path := loadWithFile(t, "log:\n  level: debug\n", nil)
	if err := os.WriteFile(path, []byte("log:\n  level: loud\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := cfg.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if v.FileError == "" || v.RestartPending {
		t.Errorf("view = %+v", v)
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	secretKeys["server.address"] = true
	t.Cleanup(func() { delete(secretKeys, "server.address") })
	cfg, _ := loadWithFile(t, "", nil)
	v, err := cfg.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if e := entry(t, v, "server.address"); !e.Secret || e.Value != "***" || e.Default != "***" {
		t.Errorf("secret entry = %+v", e)
	}
}
