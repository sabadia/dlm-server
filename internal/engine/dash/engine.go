package dash

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

// EngineName is the name of the DASH engine, stored on tasks.
const EngineName = "dash"

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
)

// Engine downloads DASH streams: it picks the representations, fetches every
// segment, and muxes video and audio with ffmpeg into one file.
type Engine struct {
	r  *segmented.Runner
	fe frontend
}

var _ engine.Engine = (*Engine)(nil)

// New returns a DASH engine. DASH always needs ffmpeg to mux the segments.
func New(env engine.Env, ff FFmpeg, opts ...Option) *Engine {
	return &Engine{r: segmented.NewRunner(env, ff, opts...)}
}

// Name implements engine.Engine.
func (*Engine) Name() string { return EngineName }

// Match implements engine.Engine: http(s) URLs whose path ends in .mpd.
func (*Engine) Match(src domain.Source) bool {
	u, err := url.Parse(strings.TrimSpace(src.CurrentURL))
	if err != nil || u.Host == "" {
		return false
	}
	s := strings.ToLower(u.Scheme)
	return (s == "http" || s == "https") && strings.HasSuffix(strings.ToLower(u.Path), ".mpd")
}

// Resolve implements engine.Engine: it reads the manifest, selects the
// representations (Source.MediaHint) and describes the result.
func (e *Engine) Resolve(ctx context.Context, req engine.Request) (*engine.Probe, error) {
	if _, err := e.r.Resolve(ctx, e.fe, req.Source, req.Settings); err != nil {
		return nil, err //nolint:wrapcheck // engine errors are classified; kept intact
	}
	name := suggestName(req.Source.CurrentURL)
	return &engine.Probe{
		Engine: EngineName, Source: req.Source, Suggested: name,
		Resource: domain.Resource{Name: name, Size: -1, Files: []domain.File{{Index: 0, Name: name, Size: -1, Selected: true}}},
	}, nil
}

// NewDownload implements engine.Engine. It takes ownership of spec.Probe.
func (e *Engine) NewDownload(spec engine.Spec) (engine.Download, error) {
	_ = spec.Probe.Close() // nil-safe; this engine keeps nothing open
	if spec.Path == "" || !filepath.IsAbs(spec.Path) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("dash: download path %q must be absolute", spec.Path))
	}
	if !e.Match(spec.Source) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("dash: %q is not a DASH manifest URL", spec.Source.CurrentURL))
	}
	return e.r.NewDownload(spec, e.fe), nil
}

// frontend reads DASH manifests for the shared downloader.
type frontend struct{}

var _ segmented.Frontend = frontend{}

// Build implements segmented.Frontend: a video track (by Source.MediaHint) and
// an audio track.
func (frontend) Build(ctx context.Context, c segmented.Client) (*segmented.Plan, error) {
	tms, err := load(ctx, c, c.Source.CurrentURL)
	if err != nil {
		return nil, err
	}
	p := &segmented.Plan{}
	for i, tm := range tms {
		if len(tm.media.Segments) == 0 {
			return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w (%s)", ErrNoSegments, segmented.Redact(c.Source.CurrentURL)))
		}
		p.Bandwidth += int64(tm.bandwidth)
		p.Tracks = append(p.Tracks, segmented.NewTrack(i, tm.kind, c.Source.CurrentURL, tm.media))
	}
	if len(p.Tracks) == 0 {
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w (%s)", ErrNoSegments, segmented.Redact(c.Source.CurrentURL)))
	}
	return p, nil
}

// Reload implements segmented.Frontend: the manifest again, for fresh segment
// URLs.
func (frontend) Reload(ctx context.Context, c segmented.Client, t *segmented.Track) (*segmented.Media, error) {
	tms, err := load(ctx, c, t.URL)
	if err != nil {
		return nil, err
	}
	for _, tm := range tms {
		if tm.kind == t.Kind {
			return tm.media, nil
		}
	}
	return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("dash: the manifest no longer has a %s stream", t.Kind))
}

// load fetches the manifest and lists the tracks it describes. A page of HTML
// where a manifest was expected is an expired link.
func load(ctx context.Context, c segmented.Client, rawURL string) ([]trackMedia, error) {
	data, m, err := c.Fetch(ctx, rawURL, nil, segmented.MaxDocumentBytes)
	if err != nil {
		return nil, err //nolint:wrapcheck // classified by Fetch
	}
	mf, err := parse(data, m.URL)
	if err != nil {
		if errors.Is(err, errNotManifest) &&
			(strings.Contains(strings.ToLower(m.ContentType), "text/html") || bytes.HasPrefix(bytes.TrimSpace(data), []byte("<!"))) {
			e := engine.LinkExpired(fmt.Errorf("dash: %s is an HTML page, not a manifest", segmented.Redact(rawURL)))
			e.StatusCode = m.Status
			return nil, e
		}
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w (%s)", err, segmented.Redact(rawURL)))
	}
	tms, err := mf.tracks(c.Source.MediaHint)
	if err != nil {
		return nil, engineFatal(err)
	}
	return tms, nil
}

func engineFatal(err error) error { return engine.Fatal(domain.ErrorCodeFatal, err) }

// suggestName derives a file name from a manifest URL: the nearest path
// segment that is not generic, with .mp4.
func suggestName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "stream.mp4"
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	generic := map[string]bool{"index": true, "manifest": true, "stream": true, "dash": true, "video": true, "media": true, "main": true, "master": true, "playlist": true}
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
