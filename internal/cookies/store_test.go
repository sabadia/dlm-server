package cookies_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// fakeClock is a settable clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	store *cookies.Store
	repo  storage.CookieRepository
	clock *fakeClock
	key   []byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "dlm.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	key := bytes.Repeat([]byte{3}, 32)
	c, err := cookies.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: t0}
	return &fixture{
		store: cookies.NewStore(db.Cookies(), c, cookies.Options{Clock: clock.Now}),
		repo:  db.Cookies(),
		clock: clock,
		key:   key,
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func names(cs []*http.Cookie) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name + "=" + c.Value
	}
	return out
}

func TestJarRoundTripOverHTTP(t *testing.T) {
	f := newFixture(t)
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "s3cret-value", Path: "/", HttpOnly: true, MaxAge: 3600})
			return
		}
		gotCookie = r.Header.Get("Cookie")
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{Jar: f.store}
	for _, p := range []string{"/login", "/files/a.bin"} {
		resp, err := client.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if gotCookie != "sid=s3cret-value" {
		t.Errorf("second request Cookie = %q, want sid=s3cret-value", gotCookie)
	}
}

func TestValuesAreEncryptedAtRest(t *testing.T) {
	f := newFixture(t)
	f.store.SetCookies(mustURL(t, "https://example.com/"), []*http.Cookie{{Name: "sid", Value: "plain-text-secret"}})
	recs, err := f.repo.List(context.Background(), storage.CookieFilter{})
	if err != nil || len(recs) != 1 {
		t.Fatalf("List = %v, %v", recs, err)
	}
	if bytes.Contains(recs[0].EncryptedVal, []byte("plain-text-secret")) {
		t.Fatal("cookie value is stored in plain text")
	}
	// A store with another key cannot read it.
	other, _ := cookies.NewCipher(bytes.Repeat([]byte{4}, 32))
	s2 := cookies.NewStore(f.repo, other, cookies.Options{Clock: f.clock.Now})
	if _, err := s2.List(context.Background(), storage.CookieFilter{}); !errors.Is(err, cookies.ErrDecrypt) {
		t.Errorf("List with the wrong key: %v, want ErrDecrypt", err)
	}
	if got := s2.Cookies(mustURL(t, "https://example.com/")); len(got) != 0 {
		t.Errorf("jar with the wrong key returned %v, want nothing", names(got))
	}
}

func TestJarScoping(t *testing.T) {
	f := newFixture(t)
	f.store.SetCookies(mustURL(t, "https://www.example.com/a/b"), []*http.Cookie{
		{Name: "host", Value: "1"},                                  // host-only, default path /a
		{Name: "dom", Value: "2", Domain: "example.com", Path: "/"}, // all of example.com
		{Name: "deep", Value: "3", Path: "/a/b/c"},                  // deeper than the request
		{Name: "sec", Value: "4", Secure: true, Path: "/a"},         // https only
		{Name: "alien", Value: "5", Domain: "other.org"},            // wrong domain: rejected
		{Name: "tld", Value: "6", Domain: "com"},                    // public suffix: rejected
	})
	tests := []struct {
		url  string
		want []string
	}{
		{"https://www.example.com/a/b", []string{"host=1", "sec=4", "dom=2"}},
		{"https://www.example.com/a", []string{"host=1", "sec=4", "dom=2"}},
		{"https://www.example.com/other", []string{"dom=2"}},
		{"http://www.example.com/a/b", []string{"host=1", "dom=2"}},
		{"https://shop.example.com/a", []string{"dom=2"}},
		{"https://example.com/", []string{"dom=2"}},
		{"https://example.org/a", nil},
		{"https://www.example.com/a/b/c/d", []string{"deep=3", "host=1", "sec=4", "dom=2"}},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := names(f.store.Cookies(mustURL(t, tt.url)))
			slices.Sort(got)
			want := slices.Clone(tt.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("Cookies(%s) = %v, want %v", tt.url, got, want)
			}
		})
	}
}

func TestJarOrdersLongestPathFirst(t *testing.T) {
	f := newFixture(t)
	u := mustURL(t, "https://example.com/a/b/c")
	f.store.SetCookies(u, []*http.Cookie{{Name: "short", Value: "1", Path: "/"}})
	f.clock.Advance(time.Second)
	f.store.SetCookies(u, []*http.Cookie{{Name: "long", Value: "2", Path: "/a/b"}, {Name: "mid", Value: "3", Path: "/a"}})
	f.clock.Advance(time.Second)
	f.store.SetCookies(u, []*http.Cookie{{Name: "mid2", Value: "4", Path: "/a"}})
	got := names(f.store.Cookies(u))
	want := []string{"long=2", "mid=3", "mid2=4", "short=1"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestJarRejectsSecureCookieFromPlainHTTP(t *testing.T) {
	f := newFixture(t)
	f.store.SetCookies(mustURL(t, "http://example.com/"), []*http.Cookie{{Name: "sec", Value: "1", Secure: true}})
	if got := f.store.Cookies(mustURL(t, "https://example.com/")); len(got) != 0 {
		t.Errorf("Secure cookie set over HTTP was stored: %v", names(got))
	}
}

func TestJarIPHostAndLocalhost(t *testing.T) {
	f := newFixture(t)
	for _, host := range []string{"http://127.0.0.1:8080/", "http://localhost:8080/"} {
		f.store.SetCookies(mustURL(t, host), []*http.Cookie{{Name: "k", Value: "v"}})
		if got := names(f.store.Cookies(mustURL(t, host+"x"))); !slices.Equal(got, []string{"k=v"}) {
			t.Errorf("%s: cookies = %v, want [k=v]", host, got)
		}
	}
	if got := f.store.Cookies(mustURL(t, "http://127.0.0.2/")); len(got) != 0 {
		t.Errorf("cookie leaked to another IP: %v", names(got))
	}
}

func TestJarExpiry(t *testing.T) {
	f := newFixture(t)
	u := mustURL(t, "https://example.com/")
	f.store.SetCookies(u, []*http.Cookie{
		{Name: "short", Value: "1", MaxAge: 60},
		{Name: "long", Value: "2", Expires: t0.Add(24 * time.Hour)},
		{Name: "session", Value: "3"},
	})
	if got := len(f.store.Cookies(u)); got != 3 {
		t.Fatalf("cookies = %d, want 3", got)
	}
	f.clock.Advance(2 * time.Minute)
	if got := names(f.store.Cookies(u)); len(got) != 2 {
		t.Errorf("after expiry cookies = %v, want long and session", got)
	}
	// A deletion (Max-Age < 0) removes the stored cookie.
	f.store.SetCookies(u, []*http.Cookie{{Name: "long", MaxAge: -1}})
	if got := names(f.store.Cookies(u)); !slices.Equal(got, []string{"session=3"}) {
		t.Errorf("after delete cookies = %v, want [session=3]", got)
	}
	n, err := f.store.PurgeExpired(context.Background())
	if err != nil || n != 1 { // "short" is still stored
		t.Errorf("PurgeExpired = %d, %v; want 1", n, err)
	}
}

func TestJarReplacesSameCookie(t *testing.T) {
	f := newFixture(t)
	u := mustURL(t, "https://example.com/")
	f.store.SetCookies(u, []*http.Cookie{{Name: "sid", Value: "old"}})
	before, err := f.store.List(context.Background(), storage.CookieFilter{})
	if err != nil || len(before) != 1 {
		t.Fatalf("List = %v, %v", before, err)
	}
	f.clock.Advance(time.Hour)
	f.store.SetCookies(u, []*http.Cookie{{Name: "sid", Value: "new"}})
	after, err := f.store.List(context.Background(), storage.CookieFilter{})
	if err != nil || len(after) != 1 {
		t.Fatalf("List = %v, %v", after, err)
	}
	if after[0].Value != "new" || after[0].ID != before[0].ID || !after[0].CreatedAt.Equal(before[0].CreatedAt) {
		t.Errorf("replaced cookie = %+v, want value new with the original ID and creation time (%+v)", after[0], before[0])
	}
}

func TestPartitionedCookiesAreNotSentByTheJar(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	err := f.store.Put(ctx, &domain.Cookie{Domain: "example.com", Name: "chips", Value: "v", Path: "/", PartitionKey: "https://site.test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.store.Cookies(mustURL(t, "https://example.com/")); len(got) != 0 {
		t.Errorf("partitioned cookie sent by the unpartitioned jar: %v", names(got))
	}
}

func TestListDeleteAndMetadata(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.store.SetCookies(mustURL(t, "https://a.example.com/"), []*http.Cookie{{Name: "x", Value: "1"}, {Name: "y", Value: "2"}})
	f.store.SetCookies(mustURL(t, "https://b.example.com/"), []*http.Cookie{{Name: "x", Value: "3"}})

	all, err := f.store.List(ctx, storage.CookieFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("List = %d cookies, %v; want 3", len(all), err)
	}
	meta, err := f.store.ListMetadata(ctx, storage.CookieFilter{Domain: "A.Example.com"})
	if err != nil || len(meta) != 2 {
		t.Fatalf("ListMetadata = %d cookies, %v; want 2 (domain filter is case-insensitive)", len(meta), err)
	}
	for _, c := range meta {
		if c.Value != "" {
			t.Errorf("ListMetadata returned a value for %q", c.Name)
		}
	}
	if err := f.store.Delete(ctx, all[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(ctx, all[0].ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
	n, err := f.store.DeleteDomain(ctx, "b.example.com")
	if err != nil || n != 1 {
		t.Errorf("DeleteDomain = %d, %v; want 1", n, err)
	}
	if _, err := f.store.DeleteDomain(ctx, ""); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("DeleteDomain(\"\") = %v, want ErrInvalidArgument", err)
	}
}

func TestPutValidates(t *testing.T) {
	f := newFixture(t)
	for name, c := range map[string]*domain.Cookie{
		"no name":   {Domain: "example.com"},
		"no domain": {Name: "a"},
		"bad path":  {Name: "a", Domain: "example.com", Path: "x"},
	} {
		if err := f.store.Put(context.Background(), c); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: Put = %v, want ErrInvalidArgument", name, err)
		}
	}
}

func TestForURL(t *testing.T) {
	f := newFixture(t)
	f.store.SetCookies(mustURL(t, "https://example.com/"), []*http.Cookie{{Name: "a", Value: "1", Domain: "example.com"}})
	got, err := f.store.ForURL(context.Background(), mustURL(t, "https://www.example.com/x"))
	if err != nil || len(got) != 1 || got[0].Name != "a" || got[0].Value != "1" {
		t.Errorf("ForURL = %+v, %v", got, err)
	}
}

func TestRunPurgesExpired(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.store.SetCookies(mustURL(t, "https://example.com/"), []*http.Cookie{{Name: "a", Value: "1", MaxAge: 1}})
	f.clock.Advance(time.Minute)
	done := make(chan error, 1)
	go func() { done <- f.store.Run(ctx, time.Hour) }()
	deadline := time.After(5 * time.Second)
	for {
		recs, err := f.repo.List(ctx, storage.CookieFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("expired cookie was not purged")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil on cancel", err)
	}
}
