package hls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/engine/retry"
	"github.com/sabadia/dlm-server/internal/ffmpeg"
	"github.com/sabadia/dlm-server/internal/settings"
)

// EngineName is the name of the HLS engine, stored on tasks.
const EngineName = "hls"

const (
	// maxPlaylistBytes bounds a playlist; maxKeyBytes a decryption key.
	maxPlaylistBytes = 8 << 20
	maxKeyBytes      = 4 << 10
	// maxSegmentBytes bounds one segment held in memory for decryption.
	maxSegmentBytes = 256 << 20

	defaultWorkers            = 8
	maxWorkers                = 16
	defaultCheckpointInterval = time.Second
	minLiveReload             = time.Second
)

// FFmpeg provides the ffmpeg executable (ffmpeg.Locator).
type FFmpeg interface {
	Locate(ctx context.Context) (ffmpeg.Binary, error)
}

// Exec runs an external program to completion; it returns an error carrying
// the program's complaint when it fails.
type Exec func(ctx context.Context, bin string, args []string) error

type options struct {
	exec     Exec
	sleep    retry.Sleeper
	ckptIntv time.Duration
	// liveWait overrides how long a live download waits before reloading the
	// playlist (tests).
	liveWait func(target time.Duration) time.Duration
}

// Option configures an Engine.
type Option func(*options)

// WithExec replaces the way ffmpeg is run (tests).
func WithExec(e Exec) Option { return func(o *options) { o.exec = e } }

// WithRetrySleeper replaces the retry back-off sleeper (tests).
func WithRetrySleeper(s retry.Sleeper) Option { return func(o *options) { o.sleep = s } }

// WithCheckpointInterval sets how often progress is checkpointed (default 1s).
func WithCheckpointInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.ckptIntv = d
		}
	}
}

// WithLiveWait overrides the wait before a live playlist is reloaded
// (default: half the target duration, at least a second).
func WithLiveWait(f func(target time.Duration) time.Duration) Option {
	return func(o *options) { o.liveWait = f }
}

// Engine downloads HLS streams: it picks a variant, fetches every segment
// (decrypting AES-128), and merges them with ffmpeg into one file.
type Engine struct {
	env  engine.Env
	ff   FFmpeg
	opts options
}

var _ engine.Engine = (*Engine)(nil)

// New returns an HLS engine. ff may be nil, which only allows MPEG-TS
// streams saved as .ts (they are concatenated without ffmpeg).
func New(env engine.Env, ff FFmpeg, opts ...Option) *Engine {
	o := options{exec: runExec, sleep: retry.Sleep, ckptIntv: defaultCheckpointInterval}
	for _, opt := range opts {
		opt(&o)
	}
	if env.Clock == nil {
		env.Clock = time.Now
	}
	return &Engine{env: env, ff: ff, opts: o}
}

// Name implements engine.Engine.
func (*Engine) Name() string { return EngineName }

// Match implements engine.Engine: http(s) URLs whose path ends in .m3u8.
func (*Engine) Match(src domain.Source) bool {
	u, err := url.Parse(strings.TrimSpace(src.CurrentURL))
	if err != nil || u.Host == "" {
		return false
	}
	s := strings.ToLower(u.Scheme)
	return (s == "http" || s == "https") && strings.HasSuffix(strings.ToLower(u.Path), ".m3u8")
}

// Resolve implements engine.Engine: it reads the playlist(s), selects the
// variant (Source.MediaHint) and describes the result.
func (e *Engine) Resolve(ctx context.Context, req engine.Request) (*engine.Probe, error) {
	client, err := e.client(req.Settings)
	if err != nil {
		return nil, err
	}
	if _, err := e.buildPlan(ctx, client, req.Source, req.Settings); err != nil {
		return nil, err
	}
	name := suggestName(req.Source.CurrentURL)
	src := req.Source
	return &engine.Probe{
		Engine: EngineName, Source: src, Suggested: name,
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
	return newDownload(e, spec), nil
}

func (e *Engine) client(s settings.Settings) (*http.Client, error) {
	if e.env.Clients == nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, errors.New("hls: no client factory"))
	}
	c, err := e.env.Clients.Client(s)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("hls: client: %w", err))
	}
	return c, nil
}

// newRequest builds a request with the headers every request of the task
// carries: the source's and the settings' headers, User-Agent and Referer.
func newRequest(ctx context.Context, src domain.Source, s settings.Settings, rawURL string, rng *byteRange) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("hls: build request: %w", err)
	}
	for k, v := range src.Headers {
		req.Header.Set(k, v)
	}
	if h, ok := s.Headers.Get(); ok {
		for k, v := range h {
			req.Header.Set(k, v)
		}
	}
	if ua := src.UserAgent; ua != "" {
		req.Header.Set("User-Agent", ua)
	} else if ua, ok := s.UserAgent.Get(); ok && ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if ref := src.Referrer; ref != "" {
		req.Header.Set("Referer", ref)
	} else if src.PageURL != "" {
		req.Header.Set("Referer", src.PageURL)
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Del("Range")
	if rng != nil {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", rng.offset, rng.offset+rng.length-1))
	}
	return req, nil
}

// meta is what a response tells besides its body.
type meta struct {
	url         *url.URL // the final URL after redirects
	contentType string
	status      int
}

// fetch GETs rawURL (the part rng of it, when set) and returns its body, at
// most limit bytes, reading through rd (a rate limiter wrapper; nil reads
// directly). Errors are classified.
func fetch(ctx context.Context, client *http.Client, src domain.Source, s settings.Settings, rawURL string, rng *byteRange,
	limit int64, wrap func(io.Reader) io.Reader,
) ([]byte, meta, error) {
	req, err := newRequest(ctx, src, s, rawURL, rng)
	if err != nil {
		return nil, meta{}, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, meta{}, httpx.Classify(fmt.Errorf("hls: GET %s: %w", redact(rawURL), err))
	}
	defer func() { _ = resp.Body.Close() }() // fully read below
	m := meta{url: resp.Request.URL, contentType: resp.Header.Get("Content-Type"), status: resp.StatusCode}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, m, httpx.Classify(httpx.NewStatusError(resp))
	}
	var body io.Reader = resp.Body
	if rng != nil && resp.StatusCode == http.StatusOK {
		// The server ignored the range and sent the whole resource.
		if _, err := io.CopyN(io.Discard, body, rng.offset); err != nil {
			return nil, m, httpx.Classify(fmt.Errorf("hls: skip to the byte range of %s: %w", redact(rawURL), err))
		}
		body = io.LimitReader(body, rng.length)
	}
	if wrap != nil {
		body = wrap(body)
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, m, ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		}
		return nil, m, httpx.Classify(fmt.Errorf("hls: read %s: %w", redact(rawURL), err))
	}
	if int64(len(data)) > limit {
		return nil, m, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("hls: %s is larger than %d bytes", redact(rawURL), limit))
	}
	return data, m, nil
}

// redact hides the query of a URL in error messages (it often carries tokens).
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparsable URL)"
	}
	if u.RawQuery != "" {
		u.RawQuery = "…"
	}
	u.User = nil
	return u.String()
}

// loadPlaylist fetches and parses a playlist. A page of HTML where a playlist
// was expected is an expired link.
func loadPlaylist(ctx context.Context, client *http.Client, src domain.Source, s settings.Settings, rawURL string) (*playlist, error) {
	data, m, err := fetch(ctx, client, src, s, rawURL, nil, maxPlaylistBytes, nil)
	if err != nil {
		return nil, err
	}
	p, err := parse(data, m.url)
	if err != nil {
		if errors.Is(err, errSampleAES) {
			return nil, engine.Fatal(domain.ErrorCodeFatal, err)
		}
		if strings.Contains(strings.ToLower(m.contentType), "text/html") || bytes.HasPrefix(bytes.TrimSpace(data), []byte("<")) {
			e := engine.LinkExpired(fmt.Errorf("hls: %s is an HTML page, not a playlist", redact(rawURL)))
			e.StatusCode = m.status
			return nil, e
		}
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w (%s)", err, redact(rawURL)))
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

// runExec runs a program and returns its error with the tail of its stderr.
func runExec(ctx context.Context, bin string, args []string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // the user's or our own ffmpeg with arguments built here
	cmd.Stderr = &limitedBuffer{buf: &stderr, max: 8 << 10}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		}
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndex(msg, "\n"); i >= 0 && len(msg) > 400 {
			msg = msg[i+1:]
		}
		return fmt.Errorf("%s: %w: %s", filepath.Base(bin), err, msg)
	}
	return nil
}

// limitedBuffer keeps the first max bytes written and discards the rest.
type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
