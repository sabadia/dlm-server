package daemon

import (
	"context"
	"errors"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/browser"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/ffmpeg"
	"github.com/sabadia/dlm-server/internal/ytdlp"
)

// toolProbeTimeout bounds each tool probe of get-system-info (they may run the
// tool to read its version).
const toolProbeTimeout = 10 * time.Second

// ErrRestart is returned by Run after a clean shutdown that was requested as
// a restart: the caller starts the daemon again.
var ErrRestart = errors.New("daemon: restart requested")

// lifecycle implements api.Lifecycle on top of the daemon's run context.
type lifecycle struct {
	cancel  context.CancelFunc
	restart atomic.Bool
}

// RequestShutdown implements api.Lifecycle.
func (l *lifecycle) RequestShutdown(restart bool) {
	if restart {
		l.restart.Store(true)
	}
	l.cancel()
}

// systemSource implements api.SystemSource.
type systemSource struct {
	cfg         *config.Config
	version     string
	startedAt   time.Time
	downloadDir string
	engines     []string
	ffmpeg      *ffmpeg.Locator
	ytdlp       *ytdlp.Locator
	browsers    *browser.Manager
	// address is the host:port the listener really bound, set once it is.
	address atomic.Value
}

// SystemInfo implements api.SystemSource.
func (s *systemSource) SystemInfo(ctx context.Context) api.SystemInfo {
	addr, _ := s.address.Load().(string)
	if addr == "" {
		addr = s.cfg.Server.Address
	}
	info := api.SystemInfo{
		Version:      s.version,
		APIVersion:   api.APIVersion,
		MinClientAPI: api.MinClientAPI,
		Commit:       buildCommit(),
		GoVersion:    runtime.Version(),
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		PID:          pid(),
		StartedAt:    s.startedAt.UTC(),
		Address:      addr,
		TLS:          s.cfg.TLSEnabled(),
		Paths: api.SystemPaths{
			Config: s.cfg.Paths.ConfigDir, Data: s.cfg.Paths.DataDir, Cache: s.cfg.Paths.CacheDir, Log: s.cfg.Paths.LogDir,
			Database: s.cfg.DatabaseFile(), Downloads: s.downloadDir,
		},
		Engines:       append([]string{}, s.engines...),
		DatabaseBytes: databaseSize(s.cfg.DatabaseFile()),
	}
	info.Tools = []api.ToolStatus{s.ffmpegStatus(ctx), s.ytdlpStatus(ctx), s.browserStatus(ctx)}
	return info
}

func (s *systemSource) ffmpegStatus(ctx context.Context) api.ToolStatus {
	ctx, cancel := context.WithTimeout(ctx, toolProbeTimeout)
	defer cancel()
	st := api.ToolStatus{Name: "ffmpeg", AutoDownload: s.cfg.FFmpeg.AutoDownload}
	b, err := s.ffmpeg.Probe(ctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Available, st.Path, st.Version, st.Source = true, b.Path, b.Version, b.Source
	return st
}

func (s *systemSource) ytdlpStatus(ctx context.Context) api.ToolStatus {
	ctx, cancel := context.WithTimeout(ctx, toolProbeTimeout)
	defer cancel()
	st := api.ToolStatus{Name: "yt-dlp", AutoDownload: s.cfg.Ytdlp.AutoDownload}
	b, err := s.ytdlp.Probe(ctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Available, st.Path, st.Version, st.Source = true, b.Path, b.Version, b.Source
	return st
}

func (s *systemSource) browserStatus(ctx context.Context) api.ToolStatus {
	ctx, cancel := context.WithTimeout(ctx, toolProbeTimeout)
	defer cancel()
	st := api.ToolStatus{Name: "browser", AutoDownload: s.cfg.Browser.AutoDownload}
	inst, source, err := s.browsers.Probe(ctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Available, st.Path, st.Version, st.Source = true, inst.Path, inst.Version, source
	return st
}

// buildCommit returns the source revision the binary was built from (with
// "-dirty" for a modified tree), or "" when the build has no VCS data.
func buildCommit() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	var dirty bool
	for _, kv := range bi.Settings {
		switch kv.Key {
		case "vcs.revision":
			rev = kv.Value
		case "vcs.modified":
			dirty = kv.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" && dirty {
		rev += "-dirty"
	}
	return rev
}
