package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

func environ(kv ...string) func() []string { return func() []string { return kv } }

func newFlags(t *testing.T, args ...string) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return fs
}

func TestLoadDefaultsWithHome(t *testing.T) {
	home := t.TempDir()
	cfg, err := Load(LoadOptions{Environ: environ("DLM_HOME=" + home)})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Address != DefaultAddress {
		t.Errorf("address = %q, want %q", cfg.Server.Address, DefaultAddress)
	}
	if got, want := cfg.Paths.ConfigDir, filepath.Join(home, "config"); got != want {
		t.Errorf("config dir = %q, want %q", got, want)
	}
	if got, want := cfg.Paths.LogDir, filepath.Join(home, "data", "logs"); got != want {
		t.Errorf("log dir = %q, want %q", got, want)
	}
	if got, want := cfg.DatabaseFile(), filepath.Join(home, "data", "dlm.db"); got != want {
		t.Errorf("db file = %q, want %q", got, want)
	}
	if cfg.Server.ShutdownTimeout != 10*time.Second {
		t.Errorf("shutdown timeout = %v", cfg.Server.ShutdownTimeout)
	}
}

func TestLoadPrecedence(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := "server:\n  address: 127.0.0.1:1000\n  shutdown_timeout: 3s\nlog:\n  level: debug\n  format: text\n  max_size_mb: 7\n"
	if err := os.WriteFile(filepath.Join(home, "config", "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(LoadOptions{
		Environ: environ(
			"DLM_HOME="+home,
			"DLM_SERVER_ADDRESS=127.0.0.1:2000",
			"DLM_LOG_LEVEL=warn",
			"DLM_SERVER_ALLOWED_ORIGINS=chrome-extension://a,moz-extension://b",
			"DLM_UNKNOWN_KEY=ignored",
		),
		Flags: newFlags(t, "--address", "127.0.0.1:3000"),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Address != "127.0.0.1:3000" {
		t.Errorf("flag should win: address = %q", cfg.Server.Address)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("env should beat file: level = %q", cfg.Log.Level)
	}
	if cfg.Log.Format != "text" || cfg.Log.MaxSizeMB != 7 || cfg.Server.ShutdownTimeout != 3*time.Second {
		t.Errorf("file values not applied: %+v %+v", cfg.Log, cfg.Server)
	}
	if got := strings.Join(cfg.Server.AllowedOrigins, "|"); got != "chrome-extension://a|moz-extension://b" {
		t.Errorf("allowed origins = %q", got)
	}
}

func TestUnsetFlagDoesNotOverride(t *testing.T) {
	home := t.TempDir()
	cfg, err := Load(LoadOptions{
		Environ: environ("DLM_HOME="+home, "DLM_LOG_LEVEL=error"),
		Flags:   newFlags(t), // --log-level not set; its default must not win
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "error" {
		t.Errorf("level = %q, want env value", cfg.Log.Level)
	}
}

func TestExplicitConfigFileMustExist(t *testing.T) {
	_, err := Load(LoadOptions{
		ConfigFile: filepath.Join(t.TempDir(), "missing.yaml"),
		Environ:    environ("DLM_HOME=" + t.TempDir()),
	})
	if err == nil {
		t.Fatal("expected error for missing explicit config file")
	}
}

func TestConfigFileFromFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("log:\n  level: debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{
		Environ: environ("DLM_HOME=" + t.TempDir()),
		Flags:   newFlags(t, "--config", path),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("level = %q", cfg.Log.Level)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"default ok", func(*Config) {}, ""},
		{"localhost ok", func(c *Config) { c.Server.Address = "localhost:80" }, ""},
		{"ipv6 loopback ok", func(c *Config) { c.Server.Address = "[::1]:80" }, ""},
		{"bad address", func(c *Config) { c.Server.Address = "nope" }, "server.address"},
		{"missing port", func(c *Config) { c.Server.Address = "127.0.0.1:" }, "missing port"},
		{"all interfaces rejected", func(c *Config) { c.Server.Address = ":80" }, "allow_remote"},
		{"remote without tls", func(c *Config) {
			c.Server.Address = "0.0.0.0:80"
			c.Server.AllowRemote = true
		}, "requires server.tls_cert"},
		{"remote with tls", func(c *Config) {
			c.Server.Address = "0.0.0.0:80"
			c.Server.AllowRemote = true
			c.Server.TLSCert, c.Server.TLSKey = "c", "k"
		}, ""},
		{"half tls", func(c *Config) { c.Server.TLSCert = "c" }, "set together"},
		{"bad level", func(c *Config) { c.Log.Level = "loud" }, "log.level"},
		{"bad format", func(c *Config) { c.Log.Format = "xml" }, "log.format"},
		{"bad rotation", func(c *Config) { c.Log.MaxSizeMB = 0 }, "rotation"},
		{"bad shutdown", func(c *Config) { c.Server.ShutdownTimeout = 0 }, "shutdown_timeout"},
		{"no browsers", func(c *Config) { c.Browser.MaxBrowsers = 0 }, "browser.max_browsers"},
		{"bad browser idle timeout", func(c *Config) { c.Browser.IdleTimeout = 0 }, "browser.idle_timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestEnsureDirs(t *testing.T) {
	cfg, err := Load(LoadOptions{Environ: environ("DLM_HOME=" + t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	for _, dir := range []string{cfg.Paths.ConfigDir, cfg.Paths.DataDir, cfg.Paths.CacheDir, cfg.Paths.LogDir} {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			t.Errorf("%s not created: %v", dir, err)
		}
	}
}

func TestBrowserConfig(t *testing.T) {
	d := Default().Browser
	if !d.AutoDownload || d.MaxBrowsers != 2 || d.IdleTimeout != 2*time.Minute || d.Path != "" || d.NoSandbox {
		t.Errorf("default browser config = %+v", d)
	}
	cfg, err := Load(LoadOptions{Environ: func() []string {
		return []string{"DLM_HOME=" + t.TempDir(), "DLM_BROWSER_PATH=/opt/chrome", "DLM_BROWSER_MAX_BROWSERS=4", "DLM_BROWSER_AUTO_DOWNLOAD=false", "DLM_BROWSER_IDLE_TIMEOUT=30s"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Browser
	if b.Path != "/opt/chrome" || b.MaxBrowsers != 4 || b.AutoDownload || b.IdleTimeout != 30*time.Second {
		t.Errorf("browser config from the environment = %+v", b)
	}
	if got, want := cfg.BrowserProfilesDir(), filepath.Join(cfg.Paths.DataDir, "browser-profiles"); got != want {
		t.Errorf("BrowserProfilesDir = %q, want %q", got, want)
	}
	if got, want := cfg.BrowserStagingDir(), filepath.Join(cfg.Paths.CacheDir, "browser-staging"); got != want {
		t.Errorf("BrowserStagingDir = %q, want %q", got, want)
	}
}

func TestFFmpegConfig(t *testing.T) {
	if d := Default().FFmpeg; !d.AutoDownload || d.Path != "" {
		t.Errorf("default ffmpeg config = %+v", d)
	}
	cfg, err := Load(LoadOptions{Environ: func() []string {
		return []string{"DLM_HOME=" + t.TempDir(), "DLM_FFMPEG_PATH=/opt/ffmpeg", "DLM_FFMPEG_AUTO_DOWNLOAD=false"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FFmpeg.Path != "/opt/ffmpeg" || cfg.FFmpeg.AutoDownload {
		t.Errorf("ffmpeg config from the environment = %+v", cfg.FFmpeg)
	}
}

func TestTorrentConfig(t *testing.T) {
	d := Default().Torrent
	if !d.Enabled || !d.DHT || !d.PEX || !d.PortForwarding || d.ListenPort != 0 || d.UploadLimit != 0 || d.MetadataTimeout != 2*time.Minute {
		t.Errorf("default torrent config = %+v", d)
	}
	cfg, err := Load(LoadOptions{Environ: func() []string {
		return []string{"DLM_HOME=" + t.TempDir(), "DLM_TORRENT_ENABLED=false", "DLM_TORRENT_LISTEN_PORT=51413", "DLM_TORRENT_DHT=false", "DLM_TORRENT_UPLOAD_LIMIT=1048576", "DLM_TORRENT_METADATA_TIMEOUT=30s"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Torrent
	if got.Enabled || got.ListenPort != 51413 || got.DHT || !got.PEX || got.UploadLimit != 1<<20 || got.MetadataTimeout != 30*time.Second {
		t.Errorf("torrent config from the environment = %+v", got)
	}
}

func TestED2KConfig(t *testing.T) {
	if d := Default().ED2K; !d.Enabled || d.Kad || d.UPnP || len(d.Servers) != 0 || d.ServerMet != "" || d.UploadLimitKB != 0 {
		t.Errorf("default ed2k config = %+v", d)
	}
	cfg, err := Load(LoadOptions{Environ: func() []string {
		return []string{"DLM_HOME=" + t.TempDir(), "DLM_ED2K_ENABLED=false", "DLM_ED2K_LISTEN_PORT=4662", "DLM_ED2K_KAD=true", "DLM_ED2K_UPLOAD_LIMIT_KB=50", "DLM_ED2K_SERVER_MET=/etc/server.met"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.ED2K
	if got.Enabled || got.ListenPort != 4662 || !got.Kad || got.UploadLimitKB != 50 || got.ServerMet != "/etc/server.met" {
		t.Errorf("ed2k config from the environment = %+v", got)
	}
}
