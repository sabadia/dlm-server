package browser

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
)

func TestRequirePageRefusesFiles(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html></html>"))
	})
	mux.HandleFunc("/file.zip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write([]byte("PK"))
	})
	mux.HandleFunc("/attachment", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Disposition", `attachment; filename="a.html"`)
		_, _ = w.Write([]byte("x"))
	})
	mux.HandleFunc("/nohead", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("x"))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for path, wantFile := range map[string]bool{
		"/page": false, "/file.zip": true, "/attachment": true, "/nohead": true, "/missing": false,
	} {
		err := requirePage(t.Context(), srv.URL+path)
		if wantFile != (err != nil) {
			t.Errorf("%s: err = %v, want file = %v", path, err, wantFile)
		}
		if wantFile && !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: error %v does not match ErrInvalidArgument", path, err)
		}
	}
	// An unreachable host is not proof of a file: the browser reports it.
	if err := requirePage(t.Context(), "http://127.0.0.1:1/x"); err != nil {
		t.Errorf("unreachable: %v", err)
	}
}
