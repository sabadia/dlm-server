package cookies_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/settings"
)

// TestEngineClientsShareTheJar checks the wiring the daemon relies on: the
// clients built by netclient.Factory send imported cookies, store the
// cookies a response sets, and send them on later requests.
func TestEngineClientsShareTheJar(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gated":
			if c, err := r.Cookie("access"); err != nil || c.Value != "imported" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "issued", Path: "/", MaxAge: 600})
			_, _ = io.WriteString(w, "ok")
		case "/next":
			if c, err := r.Cookie("session"); err != nil || c.Value != "issued" {
				http.Error(w, "no session", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, "ok")
		}
	}))
	t.Cleanup(srv.Close)

	factory := netclient.New(netclient.Options{CookieJar: f.store})
	t.Cleanup(func() { _ = factory.Close() })
	client, err := factory.Client(settings.Settings{Proxy: settings.Some(settings.Proxy{Mode: settings.ProxyNone})})
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) int {
		t.Helper()
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if got := get("/gated"); got != http.StatusForbidden {
		t.Fatalf("gated without cookie = %d, want 403", got)
	}
	data := "127.0.0.1\tFALSE\t/\tFALSE\t0\taccess\timported\n"
	if _, err := f.store.Import(context.Background(), cookies.FormatNetscape, []byte(data)); err != nil {
		t.Fatal(err)
	}
	if got := get("/gated"); got != http.StatusOK {
		t.Fatalf("gated with imported cookie = %d, want 200", got)
	}
	if got := get("/next"); got != http.StatusOK {
		t.Fatalf("follow-up with the cookie set by the previous response = %d, want 200", got)
	}
}
