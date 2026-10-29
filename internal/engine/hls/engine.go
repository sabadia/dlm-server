package hls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/segmented"
)

// EngineName is the name of the HLS engine, stored on tasks.
const EngineName = "hls"

// Aliases of the shared downloader's types, so the engine's users and tests
// need not import it.
type (
	// FFmpeg provides the ffmpeg executable (ffmpeg.Locator).
	FFmpeg = segmented.FFmpeg
	// Exec runs an external program to completion.
	Exec = segmented.Exec
	// Option configures an Engine.
	Option = segmented.Option
)

// Errors of the shared downloader, as returned by this engine.
var (
	ErrDestinationExists = segmented.ErrDestinationExists
	ErrContentChanged    = segmented.ErrContentChanged
	ErrFFmpeg            = segmented.ErrFFmpeg
	ErrNoSegments        = segmented.ErrNoSegments
)

// Options of the shared downloader.
var (
	WithExec               = segmented.WithExec
	WithRetrySleeper       = segmented.WithRetrySleeper
	WithCheckpointInterval = segmented.WithCheckpointInterval
	WithLiveWait           = segmented.WithLiveWait
)

// Engine downloads HLS streams: it picks a variant, fetches every segment
// (decrypting AES-128), and merges them with ffmpeg into one file.
type Engine struct {
	r  *segmented.Runner
	fe frontend
}

var _ engine.Engine = (*Engine)(nil)

// New returns an HLS engine. ff may be nil, which only allows MPEG-TS
// streams saved as .ts (they are concatenated without ffmpeg).
func New(env engine.Env, ff FFmpeg, opts ...Option) *Engine {
	return &Engine{r: segmented.NewRunner(env, ff, opts...)}
}

// Name implements engine.Engine.
func (*Engine) Name() string { return EngineName }

// Match implements engine.Engine: http(s) URLs whose path ends in .m3u8, or
// that the source says are HLS (Source.StreamKind) although the path does not.
func (*Engine) Match(src domain.Source) bool {
	u, err := url.Parse(strings.TrimSpace(src.CurrentURL))
	if err != nil || u.Host == "" {
		return false
	}
	s := strings.ToLower(u.Scheme)
	if s != "http" && s != "https" {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") || (src.StreamKind == domain.StreamHLS && src.ResolverKind != domain.ResolverYtdlp && src.ResolverKind != domain.ResolverBrowser)
}

// Resolve implements engine.Engine: it reads the playlist(s), selects the
// variant (Source.MediaHint) and describes the result.
func (e *Engine) Resolve(ctx context.Context, req engine.Request) (*engine.Probe, error) {
	plan, err := e.r.Resolve(ctx, e.fe, req.Source, req.Settings)
	if err != nil {
		return nil, err //nolint:wrapcheck // engine errors are classified; kept intact
	}
	name := suggestName(req.Source.CurrentURL)
	return &engine.Probe{
		Engine: EngineName, Source: req.Source, Suggested: name, Formats: plan.Formats,
		Resource: domain.Resource{Name: name, Size: -1, Files: []domain.File{{Index: 0, Name: name, Size: -1, Selected: true}}},
	}, nil
}

// NewDownload implements engine.Engine. It takes ownership of spec.Probe.
func (e *Engine) NewDownload(spec engine.Spec) (engine.Download, error) {
	_ = spec.Probe.Close() // nil-safe; this engine keeps nothing open
	if spec.Path == "" || !filepath.IsAbs(spec.Path) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("hls: download path %q must be absolute", spec.Path))
	}
	if !e.Match(spec.Source) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("hls: %q is not an HLS playlist URL", spec.Source.CurrentURL))
	}
	return e.r.NewDownload(spec, e.fe), nil
}

// frontend reads HLS playlists for the shared downloader.
type frontend struct{}

var _ segmented.Frontend = frontend{}

// Build implements segmented.Frontend: the playlist at the source URL. A master
// playlist is resolved to one variant (Source.MediaHint) and, when that variant
// plays with a separate audio rendition, to its audio playlist too.
func (frontend) Build(ctx context.Context, c segmented.Client) (*segmented.Plan, error) {
	src := c.Source
	first, err := loadPlaylist(ctx, c, src.CurrentURL)
	if err != nil {
		return nil, err
	}
	p := &segmented.Plan{}
	load := func(idx int, kind, rawURL string, m *segmented.Media) error {
		if m == nil {
			pl, err := loadPlaylist(ctx, c, rawURL)
			if err != nil {
				return err
			}
			if pl.media == nil {
				return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("hls: %s is a master playlist where a media playlist was expected", segmented.Redact(rawURL)))
			}
			m = pl.media
		}
		if len(m.Segments) == 0 {
			return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w (%s)", ErrNoSegments, segmented.Redact(rawURL)))
		}
		p.Tracks = append(p.Tracks, segmented.NewTrack(idx, kind, rawURL, m))
		return nil
	}
	if first.media != nil {
		return p, load(0, "video", src.CurrentURL, first.media)
	}
	p.Formats = formats(first.master.variants)
	v, err := selectVariant(first.master.variants, src.MediaHint)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	p.Bandwidth = v.bandwidth
	if err := load(0, "video", v.url, nil); err != nil {
		return nil, err
	}
	if a, ok := pickAudio(first.master, v); ok {
		if err := load(1, "audio", a.url, nil); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Reload implements segmented.Frontend: the track's media playlist again.
func (frontend) Reload(ctx context.Context, c segmented.Client, t *segmented.Track) (*segmented.Media, error) {
	pl, err := loadPlaylist(ctx, c, t.URL)
	if err != nil {
		return nil, err
	}
	if pl.media == nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("hls: %s no longer is a media playlist", segmented.Redact(t.URL)))
	}
	return pl.media, nil
}

// loadPlaylist fetches and parses a playlist. A page of HTML where a playlist
// was expected is an expired link.
func loadPlaylist(ctx context.Context, c segmented.Client, rawURL string) (*playlist, error) {
	data, m, err := c.Fetch(ctx, rawURL, nil, segmented.MaxDocumentBytes)
	if err != nil {
		return nil, err //nolint:wrapcheck // classified by Fetch
	}
	p, err := parse(data, m.URL)
	if err != nil {
		if errors.Is(err, errSampleAES) {
			return nil, engine.Fatal(domain.ErrorCodeFatal, err)
		}
		if strings.Contains(strings.ToLower(m.ContentType), "text/html") || bytes.HasPrefix(bytes.TrimSpace(data), []byte("<")) {
			e := engine.LinkExpired(fmt.Errorf("hls: %s is an HTML page, not a playlist", segmented.Redact(rawURL)))
			e.StatusCode = m.Status
			return nil, e
		}
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w (%s)", err, segmented.Redact(rawURL)))
	}
	return p, nil
}

// suggestName derives a file name from a playlist URL: the nearest path
// segment that is not generic, with .mp4.
func suggestName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "stream.mp4"
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	generic := map[string]bool{"index": true, "master": true, "playlist": true, "main": true, "stream": true, "chunklist": true, "hls": true, "manifest": true, "video": true, "media": true, "live": true, "prog_index": true}
	for i := len(segs) - 1; i >= 0; i-- {
		base := strings.TrimSuffix(segs[i], path.Ext(segs[i]))
		if decoded, err := url.PathUnescape(base); err == nil {
			base = decoded
		}
		base = sanitizeName(base)
		if base != "" && !generic[strings.ToLower(base)] {
			return base + ".mp4"
		}
	}
	if h := sanitizeName(u.Hostname()); h != "" {
		return h + ".mp4"
	}
	return "stream.mp4"
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20, strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Trim(strings.TrimSpace(b.String()), ".")
}
