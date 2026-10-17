package httpx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
)

var fixedNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func statusErr(code int, retryAfter string) *StatusError {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &StatusError{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)), Header: h, URL: "https://example.com/f"}
}

func TestClassifyStatus(t *testing.T) {
	type want struct {
		kind engine.Kind
		code string
	}
	transient := want{engine.KindTransient, domain.ErrorCodeHTTPStatus}
	expired := want{engine.KindLinkExpired, domain.ErrorCodeLinkExpired}
	fatal := want{engine.KindFatal, domain.ErrorCodeHTTPStatus}
	tests := map[int]want{
		408: transient, 425: transient, 429: transient,
		500: transient, 501: transient, 502: transient, 503: transient, 504: transient, 507: transient,
		520: transient, 521: transient, 522: transient, 523: transient, 524: transient, 599: transient,
		401: expired, 403: expired, 404: expired, 410: expired,
		416: fatal, 400: fatal, 405: fatal, 409: fatal, 451: fatal, 499: fatal,
		200: fatal, 302: fatal,
	}
	for code, w := range tests {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			se := statusErr(code, "")
			err := Classify(fmt.Errorf("probe: %w", se))
			var e *engine.Error
			if !errors.As(err, &e) {
				t.Fatalf("Classify returned %T, want *engine.Error", err)
			}
			if e.Kind != w.kind || e.Code != w.code || e.StatusCode != code {
				t.Errorf("got kind=%v code=%q status=%d, want kind=%v code=%q status=%d", e.Kind, e.Code, e.StatusCode, w.kind, w.code, code)
			}
			var got *StatusError
			if !errors.As(err, &got) || got != se {
				t.Error("classified error must still wrap the *StatusError")
			}
		})
	}
}

func TestClassifyRetryAfter(t *testing.T) {
	c := Classifier{Now: func() time.Time { return fixedNow }}
	tests := []struct {
		name   string
		code   int
		header string
		want   time.Duration
	}{
		{"seconds", 503, "120", 2 * time.Minute},
		{"seconds with spaces", 429, " 7 ", 7 * time.Second},
		{"http date", 429, fixedNow.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"past date", 503, fixedNow.Add(-time.Hour).Format(http.TimeFormat), 0},
		{"negative", 503, "-5", 0},
		{"zero", 503, "0", 0},
		{"garbage", 503, "soon", 0},
		{"absent", 503, "", 0},
		{"huge", 503, "99999999999999999", time.Duration(1<<63-1) / time.Second * time.Second},
		{"ignored on link expired", 403, "60", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e *engine.Error
			if !errors.As(c.Classify(statusErr(tt.code, tt.header)), &e) {
				t.Fatal("not an *engine.Error")
			}
			if e.RetryAfter != tt.want {
				t.Errorf("RetryAfter = %v, want %v", e.RetryAfter, tt.want)
			}
		})
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

type plainNetErr struct{}

func (plainNetErr) Error() string   { return "not a timeout" }
func (plainNetErr) Timeout() bool   { return false }
func (plainNetErr) Temporary() bool { return false }

func TestClassifyErrors(t *testing.T) {
	refused := connRefusedErr(t)
	tests := []struct {
		name string
		err  error
		kind engine.Kind
		code string
	}{
		{"timeout net.Error", &url.Error{Op: "Get", URL: "https://x", Err: timeoutErr{}}, engine.KindTransient, domain.ErrorCodeNetwork},
		{"non-timeout net.Error", plainNetErr{}, engine.KindFatal, domain.ErrorCodeFatal},
		{"os deadline", fmt.Errorf("read: %w", os.ErrDeadlineExceeded), engine.KindTransient, domain.ErrorCodeNetwork},
		{"request deadline", &url.Error{Op: "Get", URL: "https://x", Err: context.DeadlineExceeded}, engine.KindTransient, domain.ErrorCodeNetwork},
		{"connection refused (real)", refused, engine.KindTransient, domain.ErrorCodeNetwork},
		{"conn reset", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, engine.KindTransient, domain.ErrorCodeNetwork},
		{"conn aborted", fmt.Errorf("x: %w", syscall.ECONNABORTED), engine.KindTransient, domain.ErrorCodeNetwork},
		{"broken pipe", fmt.Errorf("x: %w", syscall.EPIPE), engine.KindTransient, domain.ErrorCodeNetwork},
		{"eof", fmt.Errorf("body: %w", io.EOF), engine.KindTransient, domain.ErrorCodeNetwork},
		{"unexpected eof", fmt.Errorf("body: %w", io.ErrUnexpectedEOF), engine.KindTransient, domain.ErrorCodeNetwork},
		{"dns not found", &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}, engine.KindTransient, domain.ErrorCodeNetwork},
		{"dns temporary", &net.DNSError{Err: "server misbehaving", Name: "x", IsTemporary: true}, engine.KindTransient, domain.ErrorCodeNetwork},
		{"http2 goaway", errors.New("http2: server sent GOAWAY and closed the connection; LastStreamID=1"), engine.KindTransient, domain.ErrorCodeNetwork},
		{"http2 stream error", errors.New("stream error: stream ID 3; INTERNAL_ERROR"), engine.KindTransient, domain.ErrorCodeNetwork},
		{"unknown authority", &url.Error{Op: "Get", URL: "https://x", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, engine.KindFatal, domain.ErrorCodeFatal},
		{"x509 unknown authority", x509.UnknownAuthorityError{}, engine.KindFatal, domain.ErrorCodeFatal},
		{"x509 invalid", x509.CertificateInvalidError{Reason: x509.Expired}, engine.KindFatal, domain.ErrorCodeFatal},
		{"x509 hostname", fmt.Errorf("tls: %w", x509.HostnameError{Host: "x"}), engine.KindFatal, domain.ErrorCodeFatal},
		{"cert error inside OpError", &net.OpError{Op: "remote error", Err: &tls.CertificateVerificationError{Err: x509.HostnameError{}}}, engine.KindFatal, domain.ErrorCodeFatal},
		{"disk full", &os.PathError{Op: "write", Path: "f.part", Err: diskFullErrno()}, engine.KindFatal, domain.ErrorCodeDiskFull},
		{"other", errors.New("something odd"), engine.KindFatal, domain.ErrorCodeFatal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Classify(tt.err)
			var e *engine.Error
			if !errors.As(err, &e) {
				t.Fatalf("Classify returned %T, want *engine.Error", err)
			}
			if e.Kind != tt.kind || e.Code != tt.code {
				t.Errorf("got kind=%v code=%q, want kind=%v code=%q", e.Kind, e.Code, tt.kind, tt.code)
			}
			if !errors.Is(err, tt.err) {
				t.Error("classified error must wrap the original")
			}
		})
	}
}

func TestClassifyRealTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)

	client := &http.Client{Timeout: 20 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected a timeout")
	}
	if k := engine.Classify(Classify(err)); k != engine.KindTransient {
		t.Errorf("kind = %v, want transient (err %v)", k, err)
	}
}

func TestClassifyPassthrough(t *testing.T) {
	already := engine.LinkExpired(errors.New("html"))
	wrapped := fmt.Errorf("task: %w", already)
	canceled := fmt.Errorf("get: %w", context.Canceled)
	for name, err := range map[string]error{"engine error": already, "wrapped engine error": wrapped, "canceled": canceled} {
		t.Run(name, func(t *testing.T) {
			if got := Classify(err); got != err { //nolint:errorlint // identity is the point
				t.Errorf("Classify changed %v into %v", err, got)
			}
		})
	}
	if Classify(nil) != nil {
		t.Error("Classify(nil) != nil")
	}
	if k := engine.Classify(Classify(canceled)); k != engine.KindCanceled {
		t.Errorf("canceled kind = %v", k)
	}
}

func connRefusedErr(t *testing.T) error {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	var d net.Dialer
	conn, err := d.DialContext(context.Background(), "tcp", addr)
	if err == nil {
		_ = conn.Close()
		t.Skip("port unexpectedly reused")
	}
	return err
}

func TestIsHTMLInsteadOfFile(t *testing.T) {
	tests := []struct {
		name, got, expected string
		want                bool
	}{
		{"html for zip", "text/html; charset=utf-8", "application/zip", true},
		{"xhtml for video", "application/xhtml+xml", "video/mp4", true},
		{"html no expectation", "text/html", "", true},
		{"html expected", "text/html", "text/html; charset=utf-8", false},
		{"binary", "application/octet-stream", "application/zip", false},
		{"no content type", "", "application/zip", false},
		{"malformed", "text/html; =", "application/zip", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if tt.got != "" {
				resp.Header.Set("Content-Type", tt.got)
			}
			if got := IsHTMLInsteadOfFile(resp, tt.expected); got != tt.want {
				t.Errorf("IsHTMLInsteadOfFile = %v, want %v", got, tt.want)
			}
		})
	}
	if IsHTMLInsteadOfFile(nil, "") {
		t.Error("nil response must be false")
	}
}
