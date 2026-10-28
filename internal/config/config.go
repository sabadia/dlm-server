// Package config loads the daemon's boot configuration (listen address,
// directories, logging) from defaults, an optional YAML file, environment
// variables and command-line flags, in increasing order of precedence.
//
// Runtime download settings (queues, speed limits, ...) are not part of the
// boot configuration; they live in the database and are managed through the
// settings package.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/platform"
)

// AppName is used for directory names, the environment prefix and logs.
const AppName = "dlm"

// DefaultAddress is the loopback address the daemon listens on by default.
const DefaultAddress = "127.0.0.1:17171"

// Config is the boot configuration of the daemon.
type Config struct {
	// Home, when set, places every directory below it (portable mode).
	Home   string       `koanf:"home"`
	Server ServerConfig `koanf:"server"`
	Paths  PathsConfig  `koanf:"paths"`
	Log    LogConfig    `koanf:"log"`
	// Browser configures the internal headless browser.
	Browser BrowserConfig `koanf:"browser"`
	// FFmpeg configures the ffmpeg the stream engines use.
	FFmpeg FFmpegConfig `koanf:"ffmpeg"`
	// Torrent configures the BitTorrent client shared by all torrent tasks.
	Torrent TorrentConfig `koanf:"torrent"`
	// ED2K configures the eDonkey2000 client shared by all ed2k tasks.
	ED2K ED2KConfig `koanf:"ed2k"`
	// Ytdlp configures the yt-dlp the ytdlp engine runs.
	Ytdlp YtdlpConfig `koanf:"ytdlp"`
	// HTTP configures the HTTP clients every engine and the browser use.
	HTTP HTTPConfig `koanf:"http"`
}

// ServerConfig controls the HTTP listener.
type ServerConfig struct {
	// Address is the host:port to listen on.
	Address string `koanf:"address"`
	// AllowRemote must be true to bind a non-loopback address. Remote mode
	// additionally requires TLSCert and TLSKey.
	AllowRemote bool   `koanf:"allow_remote"`
	TLSCert     string `koanf:"tls_cert"`
	TLSKey      string `koanf:"tls_key"`
	// AllowedOrigins lists browser origins (e.g. extension origins such as
	// chrome-extension://<id>) permitted to call the API cross-origin.
	AllowedOrigins []string `koanf:"allowed_origins"`
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration `koanf:"shutdown_timeout"`
}

// PathsConfig holds the directories used by the daemon. Empty values are
// filled with OS-appropriate defaults by Load.
type PathsConfig struct {
	ConfigDir string `koanf:"config_dir"`
	DataDir   string `koanf:"data_dir"`
	CacheDir  string `koanf:"cache_dir"`
	LogDir    string `koanf:"log_dir"`
}

// LogConfig controls structured logging.
type LogConfig struct {
	Level      string `koanf:"level"`  // debug, info, warn, error
	Format     string `koanf:"format"` // json, text
	Stderr     bool   `koanf:"stderr"` // also log to stderr
	File       bool   `koanf:"file"`   // log to a rotating file in Paths.LogDir
	MaxSizeMB  int    `koanf:"max_size_mb"`
	MaxBackups int    `koanf:"max_backups"`
	MaxAgeDays int    `koanf:"max_age_days"`
}

// BrowserConfig configures the internal headless browser (see package
// browser). It is started on demand, so these settings cost nothing until a
// page is opened.
type BrowserConfig struct {
	// Path is the browser executable. Empty means: the first of Chrome,
	// Chromium, Edge or Brave found on the machine, else a downloaded
	// chrome-headless-shell.
	Path string `koanf:"path"`
	// AutoDownload lets dlm download chrome-headless-shell (from the Chrome
	// for Testing endpoints, into the cache directory) when no browser is
	// installed.
	AutoDownload bool `koanf:"auto_download"`
	// MaxBrowsers bounds the browser processes running at once; there is one
	// per site group, each with its own profile.
	MaxBrowsers int `koanf:"max_browsers"`
	// IdleTimeout is how long a browser without open pages stays alive.
	IdleTimeout time.Duration `koanf:"idle_timeout"`
	// NoSandbox starts browsers with --no-sandbox, for containers that run as
	// root.
	NoSandbox bool `koanf:"no_sandbox"`
}

// FFmpegConfig configures where dlm gets ffmpeg, which remuxes and merges
// streams (HLS, DASH). It is run as a separate program.
type FFmpegConfig struct {
	// Path is the ffmpeg executable. Empty means: ffmpeg on PATH, else a
	// downloaded static build.
	Path string `koanf:"path"`
	// AutoDownload lets dlm download a pinned static ffmpeg build into the
	// cache directory when none is installed.
	AutoDownload bool `koanf:"auto_download"`
}

// TorrentConfig configures the one BitTorrent client the daemon runs for all
// torrent tasks. The client starts when the first torrent is resolved or
// downloaded. Per-task behaviour (seeding, extra trackers) is in the torrent
// group of the settings.
type TorrentConfig struct {
	// Enabled turns the BitTorrent engine on; magnet links and .torrent
	// files are refused when it is off.
	Enabled bool `koanf:"enabled"`
	// ListenPort is the port for incoming peer connections (TCP and uTP, and
	// the DHT's UDP). 0 picks a random free port; set a fixed one to forward
	// it on the router.
	ListenPort int `koanf:"listen_port"`
	// ListenHost binds the client to one local address; empty listens on all
	// addresses.
	ListenHost string `koanf:"listen_host"`
	// DHT enables the distributed hash table for finding peers without a
	// tracker; PEX enables peer exchange between connected peers.
	DHT bool `koanf:"dht"`
	PEX bool `koanf:"pex"`
	// PortForwarding lets the client ask the router (UPnP, NAT-PMP) to
	// forward the listen port.
	PortForwarding bool `koanf:"port_forwarding"`
	// UploadLimit caps the upload speed of all torrents together, in bytes
	// per second; 0 means unlimited.
	UploadLimit int64 `koanf:"upload_limit"`
	// MetadataTimeout bounds how long a magnet link may take to deliver its
	// torrent metadata.
	MetadataTimeout time.Duration `koanf:"metadata_timeout"`
}

// ED2KConfig configures the one eDonkey2000 client the daemon runs for all
// ed2k:// tasks. The client starts when the first task needs it. Without
// Servers, ServerMet or Kad a download only finds the sources its link names.
type ED2KConfig struct {
	// Enabled turns the ed2k engine on; ed2k links are refused when it is off.
	Enabled bool `koanf:"enabled"`
	// ListenPort is the TCP port for incoming connections (UDP uses the next
	// one); 0 picks free ports.
	ListenPort int `koanf:"listen_port"`
	// Servers are ed2k servers (host:port) used to find sources.
	Servers []string `koanf:"servers"`
	// ServerMet is the path of a server.met file whose servers are used too.
	ServerMet string `koanf:"server_met"`
	// Kad turns on the Kad network for finding sources without a server.
	Kad bool `koanf:"kad"`
	// UPnP asks the router to forward the listen port.
	UPnP bool `koanf:"upnp"`
	// UploadLimitKB caps the upload in kilobytes per second; 0 is unlimited.
	UploadLimitKB int `koanf:"upload_limit_kb"`
}

// YtdlpConfig configures where dlm gets yt-dlp, which downloads video and
// audio pages (tasks created with the ytdlp resolver). It is run as a
// separate program.
type YtdlpConfig struct {
	// Path is the yt-dlp executable. Empty means: yt-dlp on PATH, else a
	// downloaded build. Sites change often, so a yt-dlp you keep up to date
	// is better than the pinned download.
	Path string `koanf:"path"`
	// AutoDownload lets dlm download a pinned yt-dlp build into the cache
	// directory when none is installed.
	AutoDownload bool `koanf:"auto_download"`
}

// HTTPConfig configures the HTTP clients.
type HTTPConfig struct {
	// TLSImpersonation retries a request through a Chrome-like TLS fingerprint
	// when a site refuses Go's (a handshake failure, or a Cloudflare challenge
	// page), and remembers which hosts needed it. Off by default: it opens an
	// extra connection to such sites, and some sites would rather see an honest
	// client. JavaScript challenges are not solved either way.
	TLSImpersonation bool `koanf:"tls_impersonation"`
	// ImpersonationTTL is how long a host that needed impersonation is
	// remembered before the normal fingerprint is tried again.
	ImpersonationTTL time.Duration `koanf:"impersonation_ttl"`
}

// Default returns the configuration used when nothing else is specified.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Address:         DefaultAddress,
			ShutdownTimeout: 10 * time.Second,
		},
		Log: LogConfig{
			Level:      "info",
			Format:     "json",
			Stderr:     false,
			File:       true,
			MaxSizeMB:  20,
			MaxBackups: 5,
			MaxAgeDays: 30,
		},
		Browser: BrowserConfig{AutoDownload: true, MaxBrowsers: 2, IdleTimeout: 2 * time.Minute},
		FFmpeg:  FFmpegConfig{AutoDownload: true},
		Torrent: TorrentConfig{
			Enabled: true, DHT: true, PEX: true, PortForwarding: true,
			MetadataTimeout: 2 * time.Minute,
		},
		ED2K:  ED2KConfig{Enabled: true},
		Ytdlp: YtdlpConfig{AutoDownload: true},
		HTTP:  HTTPConfig{ImpersonationTTL: time.Hour},
	}
}

// TokenFile is the path of the API bearer-token file.
func (c *Config) TokenFile() string { return filepath.Join(c.Paths.ConfigDir, "token") }

// DatabaseFile is the path of the SQLite database.
func (c *Config) DatabaseFile() string { return filepath.Join(c.Paths.DataDir, AppName+".db") }

// PluginsDir is where installed plugins live.
func (c *Config) PluginsDir() string { return filepath.Join(c.Paths.DataDir, "plugins") }

// BrowserProfilesDir holds the browser profiles (one per site group), which
// keep the logins of the pages the browser opened.
func (c *Config) BrowserProfilesDir() string {
	return filepath.Join(c.Paths.DataDir, "browser-profiles")
}

// BrowserStagingDir holds the files the browser downloaded until they are
// imported as tasks.
func (c *Config) BrowserStagingDir() string {
	return filepath.Join(c.Paths.CacheDir, "browser-staging")
}

// TLSEnabled reports whether the listener serves HTTPS.
func (c *Config) TLSEnabled() bool { return c.Server.TLSCert != "" && c.Server.TLSKey != "" }

// resolvePaths fills empty directories with defaults derived from Home or the
// OS conventions, and makes every path absolute.
func (c *Config) resolvePaths() error {
	p := &c.Paths
	if c.Home != "" {
		home, err := filepath.Abs(c.Home)
		if err != nil {
			return fmt.Errorf("config: resolve home: %w", err)
		}
		c.Home = home
		setIfEmpty(&p.ConfigDir, filepath.Join(home, "config"))
		setIfEmpty(&p.DataDir, filepath.Join(home, "data"))
		setIfEmpty(&p.CacheDir, filepath.Join(home, "cache"))
	}
	if p.ConfigDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("config: user config dir: %w", err)
		}
		p.ConfigDir = filepath.Join(dir, AppName)
	}
	if p.DataDir == "" {
		dir, err := platform.UserDataDir()
		if err != nil {
			return fmt.Errorf("config: user data dir: %w", err)
		}
		p.DataDir = filepath.Join(dir, AppName)
	}
	if p.CacheDir == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("config: user cache dir: %w", err)
		}
		p.CacheDir = filepath.Join(dir, AppName)
	}
	setIfEmpty(&p.LogDir, filepath.Join(p.DataDir, "logs"))

	for _, dir := range []*string{&p.ConfigDir, &p.DataDir, &p.CacheDir, &p.LogDir} {
		abs, err := filepath.Abs(*dir)
		if err != nil {
			return fmt.Errorf("config: resolve %q: %w", *dir, err)
		}
		*dir = abs
	}
	return nil
}

// EnsureDirs creates the configured directories with owner-only permissions.
func (c *Config) EnsureDirs() error {
	for _, dir := range []string{c.Paths.ConfigDir, c.Paths.DataDir, c.Paths.CacheDir, c.Paths.LogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("config: create %q: %w", dir, err)
		}
	}
	return nil
}

// Validate checks the configuration for consistency and safe defaults.
func (c *Config) Validate() error {
	var errs []error

	host, port, err := net.SplitHostPort(c.Server.Address)
	if err != nil {
		errs = append(errs, fmt.Errorf("server.address %q: %w", c.Server.Address, err))
	} else {
		if port == "" {
			errs = append(errs, fmt.Errorf("server.address %q: missing port", c.Server.Address))
		}
		if !isLoopbackHost(host) {
			if !c.Server.AllowRemote {
				errs = append(errs, fmt.Errorf("server.address %q is not a loopback address; set server.allow_remote to bind it", c.Server.Address))
			} else if !c.TLSEnabled() {
				errs = append(errs, errors.New("server.allow_remote requires server.tls_cert and server.tls_key"))
			}
		}
	}
	if (c.Server.TLSCert == "") != (c.Server.TLSKey == "") {
		errs = append(errs, errors.New("server.tls_cert and server.tls_key must be set together"))
	}
	if c.HTTP.ImpersonationTTL <= 0 {
		errs = append(errs, errors.New("http.impersonation_ttl must be positive"))
	}
	if c.Server.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("server.shutdown_timeout must be positive"))
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, strings.ToLower(c.Log.Level)) {
		errs = append(errs, fmt.Errorf("log.level %q: want debug, info, warn or error", c.Log.Level))
	}
	if !slices.Contains([]string{"json", "text"}, strings.ToLower(c.Log.Format)) {
		errs = append(errs, fmt.Errorf("log.format %q: want json or text", c.Log.Format))
	}
	if c.Browser.MaxBrowsers < 1 {
		errs = append(errs, errors.New("browser.max_browsers must be at least 1"))
	}
	if c.Browser.IdleTimeout <= 0 {
		errs = append(errs, errors.New("browser.idle_timeout must be positive"))
	}
	if c.Log.MaxSizeMB < 1 || c.Log.MaxBackups < 0 || c.Log.MaxAgeDays < 0 {
		errs = append(errs, errors.New("log rotation: max_size_mb must be >= 1, max_backups and max_age_days >= 0"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("config: invalid configuration: %w", err)
	}
	return nil
}

// isLoopbackHost reports whether host is "localhost" or a loopback IP.
// An empty host (all interfaces) is not loopback.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

func setIfEmpty(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}
