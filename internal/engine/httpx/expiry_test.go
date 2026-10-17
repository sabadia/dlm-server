package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/settings"
)

func htmlPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<html><body>Please log in</body></html>"))
}

// The extension table comes from the OS (the Windows registry maps .zip to
// application/x-zip-compressed), so those cases only assert the kind of type.
const (
	anyFile = "<a non-HTML type>"
	anyHTML = "<an HTML type>"
)

func TestExpectedContentType(t *testing.T) {
	tests := []struct {
		name string
		src  domain.Source
		want string
	}{
		{"fingerprint wins", domain.Source{CurrentURL: "https://x/a.html", Fingerprint: domain.Fingerprint{ContentType: "application/zip"}}, "application/zip"},
		{"extension", domain.Source{CurrentURL: "https://x/dir/a.ZIP?sig=1"}, anyFile},
		{"redirect target preferred", domain.Source{CurrentURL: "https://x/get", RedirectURL: "https://cdn/f.pdf"}, anyFile},
		{"html extension", domain.Source{CurrentURL: "https://x/a.html"}, anyHTML},
		{"no extension", domain.Source{CurrentURL: "https://x/download"}, ""},
		{"unknown extension", domain.Source{CurrentURL: "https://x/a.qzxw"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expectedContentType(tt.src)
			switch tt.want {
			case anyFile:
				if got == "" || isHTML(got) {
					t.Errorf("expectedContentType = %q, want a non-HTML type", got)
				}
			case anyHTML:
				if !isHTML(got) {
					t.Errorf("expectedContentType = %q, want an HTML type", got)
				}
			default:
				if got != tt.want {
					t.Errorf("expectedContentType = %q, want %q", got, tt.want)
				}
			}
		})
	}
}

func TestProbeHTMLInsteadOfFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { htmlPage(w) }))
	t.Cleanup(srv.Close)
	tests := []struct {
		name    string
		src     domain.Source
		expired bool
	}{
		{"file extension", domain.Source{CurrentURL: srv.URL + "/files/data.zip"}, true},
		{"fingerprinted type", domain.Source{CurrentURL: srv.URL + "/get", Fingerprint: domain.Fingerprint{ContentType: "application/octet-stream"}}, true},
		{"captured html type", domain.Source{CurrentURL: srv.URL + "/get", Fingerprint: domain.Fingerprint{ContentType: "text/html"}}, false},
		{"html page by extension", domain.Source{CurrentURL: srv.URL + "/page.html"}, false},
		{"no expectation", domain.Source{CurrentURL: srv.URL + "/get"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Probe(context.Background(), srv.Client(), tt.src, settings.Settings{})
			if res != nil {
				t.Cleanup(func() { _ = res.Close() })
			}
			if !tt.expired {
				if err != nil {
					t.Fatalf("Probe: %v", err)
				}
				return
			}
			wantKind(t, err, engine.KindLinkExpired)
			var ee *engine.Error
			if !errors.As(err, &ee) || ee.Code != domain.ErrorCodeLinkExpired || ee.StatusCode != http.StatusOK {
				t.Errorf("error = %+v, want code link_expired with status 200", ee)
			}
		})
	}
}

func TestDownloadHTMLInsteadOfFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		noRange bool
	}{{"ranged", false}, {"sequential", true}} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fileServer{data: testData(1<<20, 7), noRange: tc.noRange}
			fs.hook = func(w http.ResponseWriter, _ *http.Request, _ int, _ reqInfo) bool {
				htmlPage(w)
				return true
			}
			h := newHarness(t, fs)
			spec := h.specNoProbe(nil)
			spec.Source.Fingerprint.ContentType = "application/octet-stream"
			_, err := h.run(context.Background(), spec)
			wantKind(t, err, engine.KindLinkExpired)
			if _, err := os.Stat(h.path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("destination exists after failure: %v", err)
			}
		})
	}
}

func TestSizeMismatchIsLinkExpired(t *testing.T) {
	fs := &fileServer{data: testData(1<<20, 8), noRange: true}
	h := newHarness(t, fs)
	h.set.Refresh.OnMismatch = settings.Some(settings.MismatchFail)
	spec := h.specNoProbe(nil)
	spec.Resource.Size += 10 // the file behind the link is not the one that was probed
	_, err := h.run(context.Background(), spec)
	wantKind(t, err, engine.KindLinkExpired)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Errorf("err = %v, want ErrSizeMismatch", err)
	}
}
