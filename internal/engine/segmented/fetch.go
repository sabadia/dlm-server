package segmented

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/settings"
)

const (
	// MaxDocumentBytes bounds a playlist or manifest; maxKeyBytes a decryption
	// key; maxSegmentBytes one segment held in memory for decryption.
	MaxDocumentBytes = 8 << 20
	maxKeyBytes      = 4 << 10
	maxSegmentBytes  = 256 << 20
)

// ErrNoSegments is returned when a stream lists no segments.
var ErrNoSegments = errors.New("segmented: the stream has no segments")

// Client is what a Frontend needs to read the stream description: the HTTP
// client of the task and the source and settings every request carries.
type Client struct {
	HTTP     *http.Client
	Source   domain.Source
	Settings settings.Settings
}

// Meta is what a response tells besides its body.
type Meta struct {
	URL         *url.URL // the final URL after redirects
	ContentType string
	Status      int
}

// Fetch GETs rawURL (the part rng of it, when set) and returns its body, at
// most limit bytes. Errors are classified (httpx.Classify).
func (c Client) Fetch(ctx context.Context, rawURL string, rng *ByteRange, limit int64) ([]byte, Meta, error) {
	return fetch(ctx, c, rawURL, rng, limit, nil)
}

func newRequest(ctx context.Context, src domain.Source, s settings.Settings, rawURL string, rng *ByteRange) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("segmented: build request: %w", err)
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
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", rng.Offset, rng.Offset+rng.Length-1))
	}
	return req, nil
}

// fetch is Client.Fetch reading through wrap (a rate limiter; nil reads
// directly).
func fetch(ctx context.Context, c Client, rawURL string, rng *ByteRange, limit int64, wrap func(io.Reader) io.Reader) ([]byte, Meta, error) {
	req, err := newRequest(ctx, c.Source, c.Settings, rawURL, rng)
	if err != nil {
		return nil, Meta{}, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, Meta{}, httpx.Classify(fmt.Errorf("segmented: GET %s: %w", Redact(rawURL), err))
	}
	defer func() { _ = resp.Body.Close() }() // fully read below
	m := Meta{URL: resp.Request.URL, ContentType: resp.Header.Get("Content-Type"), Status: resp.StatusCode}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, m, httpx.Classify(httpx.NewStatusError(resp))
	}
	var body io.Reader = resp.Body
	if rng != nil && resp.StatusCode == http.StatusOK {
		// The server ignored the range and sent the whole resource.
		if _, err := io.CopyN(io.Discard, body, rng.Offset); err != nil {
			return nil, m, httpx.Classify(fmt.Errorf("segmented: skip to the byte range of %s: %w", Redact(rawURL), err))
		}
		body = io.LimitReader(body, rng.Length)
	}
	if wrap != nil {
		body = wrap(body)
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, m, ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		}
		return nil, m, httpx.Classify(fmt.Errorf("segmented: read %s: %w", Redact(rawURL), err))
	}
	if int64(len(data)) > limit {
		return nil, m, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("segmented: %s is larger than %d bytes", Redact(rawURL), limit))
	}
	return data, m, nil
}

// Redact hides the query of a URL in error messages (it often carries tokens).
func Redact(raw string) string {
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
