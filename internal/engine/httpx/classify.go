package httpx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/platform"
)

// Classifier turns HTTP and network errors into classified *engine.Error
// values. Now is the clock used to resolve HTTP-date Retry-After values; nil
// means time.Now.
type Classifier struct {
	Now func() time.Time
}

// Classify classifies err with the real clock. See Classifier.Classify.
func Classify(err error) error {
	return Classifier{}.Classify(err)
}

// Classify wraps err into an *engine.Error whose Kind decides what the task
// runner does next. It returns err unchanged when it is nil, already carries
// an *engine.Error, or is a context cancellation.
//
// Status errors (*StatusError):
//
//	408, 425, 429, 500, 502, 503, 504, 520-524, 599, other 5xx → transient (http_status), RetryAfter from Retry-After
//	401, 403, 404, 410                                          → link_expired (link_expired)
//	416                                                         → fatal (http_status); the downloader handles the restart
//	other 4xx and anything else                                 → fatal (http_status)
//
// 401/403/404/410 are only a hint: the link-refresh layer makes the final
// decision (for example a 403 that persists after a refresh fails the task).
//
// Other errors, checked in this order:
//
//	disk full (platform.IsDiskFull)                → fatal (disk_full)
//	TLS certificate verification failures          → fatal (fatal)
//	timeouts (net.Error.Timeout, os.ErrDeadlineExceeded,
//	  context.DeadlineExceeded), *net.DNSError (including "no such host",
//	  since the network may simply be down), *net.OpError, connection
//	  refused/reset/aborted, broken pipe, io.EOF / io.ErrUnexpectedEOF,
//	  HTTP/2 stream and GOAWAY errors              → transient (network)
//	anything else                                  → fatal (fatal)
//
// context.DeadlineExceeded is treated as a per-request timeout. When the
// task's own context expired, retry.Do sees ctx.Err() and stops anyway.
func (c Classifier) Classify(err error) error {
	if err == nil {
		return nil
	}
	var ee *engine.Error
	if errors.As(err, &ee) || errors.Is(err, context.Canceled) {
		return err
	}
	var se *StatusError
	if errors.As(err, &se) {
		return c.classifyStatus(se, err)
	}
	switch {
	case platform.IsDiskFull(err):
		return engine.Fatal(domain.ErrorCodeDiskFull, err)
	case isCertificateError(err):
		return engine.Fatal(domain.ErrorCodeFatal, err)
	case isNetworkError(err):
		return engine.Transient(domain.ErrorCodeNetwork, err)
	default:
		return engine.Fatal(domain.ErrorCodeFatal, err)
	}
}

func (c Classifier) classifyStatus(se *StatusError, err error) *engine.Error {
	code := se.StatusCode
	switch {
	case isTransientStatus(code):
		e := engine.Transient(domain.ErrorCodeHTTPStatus, err)
		e.StatusCode = code
		e.RetryAfter = ParseRetryAfter(se.Header.Get("Retry-After"), c.now())
		return e
	case code == http.StatusUnauthorized, code == http.StatusForbidden,
		code == http.StatusNotFound, code == http.StatusGone:
		e := engine.LinkExpired(err)
		e.StatusCode = code
		return e
	default: // 416, other 4xx, unexpected 1xx/2xx/3xx
		e := engine.Fatal(domain.ErrorCodeHTTPStatus, err)
		e.StatusCode = code
		return e
	}
}

func (c Classifier) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func isTransientStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	// All 5xx, which covers 500/502/503/504, Cloudflare's 520-524 and 599.
	return code >= 500 && code <= 599
}

// ParseRetryAfter parses a Retry-After header value (delta-seconds or an
// HTTP-date relative to now). It returns 0 for an empty, malformed,
// negative or past value.
func ParseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		const maxSecs = int64((1<<63 - 1) / time.Second)
		if secs > maxSecs {
			secs = maxSecs
		}
		return time.Duration(secs) * time.Second
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	if d := t.Sub(now); d > 0 {
		return d
	}
	return 0
}

func isCertificateError(err error) bool {
	var (
		verr  *tls.CertificateVerificationError
		uerr  x509.UnknownAuthorityError
		ierr  x509.CertificateInvalidError
		herr  x509.HostnameError
		uerrp *x509.UnknownAuthorityError
		ierrp *x509.CertificateInvalidError
		herrp *x509.HostnameError
	)
	return errors.As(err, &verr) ||
		errors.As(err, &uerr) || errors.As(err, &ierr) || errors.As(err, &herr) ||
		errors.As(err, &uerrp) || errors.As(err, &ierrp) || errors.As(err, &herrp)
}

func isNetworkError(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed) {
		return true
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return isHTTP2TransportError(err)
}

// isHTTP2TransportError detects connection-level HTTP/2 failures (stream
// resets, GOAWAY, lost connections). net/http's bundled HTTP/2 error types
// are unexported, so this matches their stable message prefixes; it is
// deliberately narrow to avoid retrying unrelated failures.
func isHTTP2TransportError(err error) bool {
	msg := err.Error()
	for _, s := range []string{
		"http2: server sent GOAWAY",
		"http2: client connection lost",
		"http2: client connection force closed",
		"http2: stream closed",
		"stream error: stream ID",
		"http2: timeout awaiting response headers",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// IsHTMLInsteadOfFile reports whether resp is an HTML page although the
// expected (fingerprinted) content type is not HTML: a typical sign that a
// download link expired and the server answered with a login or error page.
// An empty expectedContentType counts as "not HTML".
func IsHTMLInsteadOfFile(resp *http.Response, expectedContentType string) bool {
	if resp == nil || !isHTML(resp.Header.Get("Content-Type")) {
		return false
	}
	return !isHTML(expectedContentType)
}

// expectedContentType returns the media type a response for src should have:
// the fingerprinted (or captured) content type, else the one the URL path's
// file extension implies. It is "" when there is no positive expectation, in
// which case a response is never held to be the wrong kind of content.
func expectedContentType(src domain.Source) string {
	if ct := src.Fingerprint.ContentType; ct != "" {
		return ct
	}
	raw := src.RedirectURL
	if raw == "" {
		raw = src.CurrentURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return mime.TypeByExtension(strings.ToLower(path.Ext(u.Path)))
}

// htmlInsteadOfFile returns a link-expired error when resp is an HTML page
// although src is expected to be something else, else nil.
func htmlInsteadOfFile(resp *http.Response, src domain.Source) error {
	exp := expectedContentType(src)
	if exp == "" || !IsHTMLInsteadOfFile(resp, exp) {
		return nil
	}
	e := engine.LinkExpired(fmt.Errorf("httpx: the server answered with an HTML page (%q) instead of the file (%s)",
		resp.Header.Get("Content-Type"), exp))
	e.StatusCode = resp.StatusCode
	return e
}

func isHTML(contentType string) bool {
	if contentType == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mt == "text/html" || mt == "application/xhtml+xml"
}
