//go:build browser

package browser

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
)

func testResolver(t *testing.T, store CookieStore, ttl time.Duration, clock func() time.Time) *Resolver {
	t.Helper()
	m := testManager(t, nil)
	return NewResolver(ResolverOptions{Manager: m, Cookies: store, Dir: filepath.Join(t.TempDir(), "stage"), TTL: ttl, Clock: clock})
}

func TestResolvePage(t *testing.T) {
	srv := mediaServer(t)
	store := &memStore{}
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()) }
	r := testResolver(t, store, time.Minute, clock)
	ctx := timeout(t)

	res, err := r.ResolvePage(ctx, srv.URL+"/", ResolveOptions{Quiet: time.Second, Wait: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "media page" || res.FinalURL != srv.URL+"/" || res.URL != srv.URL+"/" || res.ID == "" || res.Group != "127.0.0.1" {
		t.Errorf("result = %+v", res)
	}
	kinds := map[Kind]int{}
	for i, c := range res.Candidates {
		kinds[c.Kind]++
		if c.Index != i {
			t.Errorf("candidate %d has index %d", i, c.Index)
		}
	}
	if kinds[KindHLS] != 1 || kinds[KindDASH] != 1 || kinds[KindVideo] != 1 || kinds[KindFile] != 2 {
		t.Errorf("kinds = %v, want hls, dash, video and two files (zip, blob)", kinds)
	}

	// The staged files survive the browser session and are served by Candidate.
	var blob *Candidate
	for i := range res.Candidates {
		if res.Candidates[i].Source == "blob" {
			blob = &res.Candidates[i]
		}
	}
	if blob == nil || !strings.HasPrefix(blob.File, filepath.Join(r.opts.Dir, res.ID)) {
		t.Fatalf("blob candidate = %+v, want a staged file below %s", blob, r.opts.Dir)
	}
	if got, err := os.ReadFile(blob.File); err != nil || string(got) != "blob-content" {
		t.Errorf("staged blob = %q, %v", got, err)
	}
	if c, err := r.Candidate(res.ID, blob.Index); err != nil || c.File != blob.File {
		t.Errorf("Candidate = %+v, %v", c, err)
	}
	if _, err := r.Candidate(res.ID, 99); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Candidate(99) = %v, want ErrNotFound", err)
	}

	// Release deletes the staged files.
	r.Release(res.ID)
	if _, err := os.Stat(blob.File); !os.IsNotExist(err) {
		t.Errorf("staged file still exists after Release: %v", err)
	}
	if _, err := r.Result(res.ID); !errors.Is(err, ErrResultGone) {
		t.Errorf("Result after Release = %v, want ErrResultGone", err)
	}

	// Results expire.
	res2, err := r.ResolvePage(ctx, srv.URL+"/", ResolveOptions{Quiet: 500 * time.Millisecond, Wait: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Result(res2.ID); err != nil {
		t.Fatal(err)
	}
	now.Add(int64(2 * time.Minute))
	if _, err := r.Result(res2.ID); !errors.Is(err, ErrResultGone) {
		t.Errorf("Result after the TTL = %v, want ErrResultGone", err)
	}
	r.sweep()
	if entries, _ := os.ReadDir(r.opts.Dir); len(entries) != 0 {
		t.Errorf("stage directory not cleaned: %v", entries)
	}
}

func TestResolvePageSyncsCookies(t *testing.T) {
	var seen atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: "refreshed", Value: "yes", Path: "/", MaxAge: 3600})
		_, _ = w.Write([]byte("<html><head><title>t</title></head></html>"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(strings.Split(srv.URL, ":")[1], "//")
	exp := time.Now().Add(time.Hour)
	store := &memStore{cookies: []*domain.Cookie{{Domain: host, Name: "login", Value: "abc", Path: "/", HostOnly: true, ExpiresAt: &exp}}}
	r := testResolver(t, store, time.Minute, nil)
	if _, err := r.ResolvePage(timeout(t), srv.URL+"/", ResolveOptions{Quiet: 300 * time.Millisecond, Wait: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	// The first request carried the stored login; later ones (the favicon
	// hits the same handler) also carry what the first response set.
	if got, _ := seen.Load().(string); !strings.Contains(got, "login=abc") {
		t.Errorf("the page saw cookies %q, want the stored login", got)
	}
	if c := store.find("refreshed"); c == nil || c.Value != "yes" {
		t.Errorf("the cookie the page set was not stored: %+v", c)
	}
}

func TestResolvePageErrors(t *testing.T) {
	r := testResolver(t, nil, time.Minute, nil)
	ctx := timeout(t)
	for _, bad := range []string{"", "ftp://x/y", "/relative", "http://"} {
		if _, err := r.ResolvePage(ctx, bad, ResolveOptions{}); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ResolvePage(%q) = %v, want ErrInvalidArgument", bad, err)
		}
	}
	// A page that cannot be loaded (nothing listens) is an error and leaves no staged files.
	if _, err := r.ResolvePage(ctx, "http://127.0.0.1:1/", ResolveOptions{Quiet: 200 * time.Millisecond, Wait: 5 * time.Second}); err == nil {
		t.Error("ResolvePage of an unreachable page succeeded")
	}
	if entries, _ := os.ReadDir(r.opts.Dir); len(entries) != 0 {
		t.Errorf("failed resolve left staged directories: %v", entries)
	}
	if _, err := r.Result("nope"); !errors.Is(err, ErrResultGone) {
		t.Errorf("Result(nope) = %v", err)
	}
}
