package httpx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// newRequest builds a request for rawURL carrying the source's method,
// body, headers, User-Agent and Referer, shared by the probe and the
// downloader so every request of a task looks the same to the server:
//   - method src.Method (GET when empty) and body src.Body;
//   - headers: src.Headers first, then the settings Headers (explicit
//     settings win; names compare case-insensitively);
//   - User-Agent: src.UserAgent, else the settings UserAgent;
//   - Referer: src.Referrer, else src.PageURL, else a Referer from the
//     header maps;
//   - Accept-Encoding: identity, so sizes and offsets are byte-accurate.
//
// Range is never set here (a Range from the header maps is removed); the
// caller adds the range it needs.
func newRequest(ctx context.Context, src domain.Source, s settings.Settings, rawURL string) (*http.Request, error) {
	method := src.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(src.Body) > 0 {
		body = bytes.NewReader(src.Body) // NewRequest also sets GetBody for redirects
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
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
	req.Header.Del("If-Range")
	return req, nil
}
