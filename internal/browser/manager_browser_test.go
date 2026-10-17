//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// These tests drive a real browser: they need Chrome, Chromium, Edge or Brave
// (or CHROMEDP_TEST_RUNNER) and run with `go test -tags browser`.

func testManager(t *testing.T, mutate func(*Options)) *Manager {
	t.Helper()
	opts := Options{ProfilesDir: t.TempDir(), IdleTimeout: time.Minute, NoSandbox: os.Geteuid() == 0 || os.Getenv("CI") != "", Path: os.Getenv("CHROMEDP_TEST_RUNNER")}
	if mutate != nil {
		mutate(&opts)
	}
	m := NewManager(opts)
	t.Cleanup(func() { _ = m.Close() })
	if _, err := m.Resolve(context.Background()); err != nil {
		t.Skipf("no browser available: %v", err)
	}
	return m
}

func pageServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<html><head><title>page %s</title></head><body>hello</body></html>", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func timeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func title(t *testing.T, s *Session, url string) string {
	t.Helper()
	if err := chromedp.Do(s.Ctx, chromedp.Navigate(url)); err != nil {
		t.Fatalf("navigate %s: %v", url, err)
	}
	got, err := chromedp.Run(s.Ctx, chromedp.Title())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func cookies(t *testing.T, s *Session) string {
	t.Helper()
	got, err := chromedp.Run(s.Ctx, chromedp.Evaluate[string]("document.cookie"))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen in time", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSessionsOfAGroupShareOneBrowser(t *testing.T) {
	m := testManager(t, nil)
	srv := pageServer(t)
	ctx := timeout(t)
	a, err := m.Acquire(ctx, "site-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Acquire(ctx, "site-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := title(t, a, srv.URL+"/one"); got != "page /one" {
		t.Errorf("title = %q", got)
	}
	if got := title(t, b, srv.URL+"/two"); got != "page /two" {
		t.Errorf("title = %q", got)
	}
	if st := m.Status(); len(st.Running) != 1 || st.Running[0].Group != "site-a" || st.Running[0].Sessions != 2 {
		t.Errorf("Status = %+v, want one browser with two sessions", st.Running)
	}
	a.Close()
	a.Close() // idempotent
	if st := m.Status(); st.Running[0].Sessions != 1 {
		t.Errorf("sessions after closing one = %d, want 1", st.Running[0].Sessions)
	}
	b.Close()
}

func TestProfilesAreIsolatedAndPersist(t *testing.T) {
	m := testManager(t, func(o *Options) { o.IdleTimeout = 100 * time.Millisecond })
	srv := pageServer(t)
	ctx := timeout(t)

	a, err := m.Acquire(ctx, "group-a")
	if err != nil {
		t.Fatal(err)
	}
	title(t, a, srv.URL+"/")
	if err := chromedp.Do(a.Ctx, chromedp.Evaluate[chromedp.Void]("document.cookie = 'k=v; max-age=3600; path=/'")); err != nil {
		t.Fatal(err)
	}
	if got := cookies(t, a); got != "k=v" {
		t.Fatalf("cookies in group-a = %q", got)
	}
	b, err := m.Acquire(ctx, "group-b")
	if err != nil {
		t.Fatal(err)
	}
	title(t, b, srv.URL+"/")
	if got := cookies(t, b); got != "" {
		t.Errorf("group-b sees the cookies of group-a: %q", got)
	}
	a.Close()
	b.Close()

	// Both browsers are shut down after the idle timeout; the profile stays.
	eventually(t, "idle shutdown", func() bool { return len(m.Status().Running) == 0 })
	a2, err := m.Acquire(ctx, "group-a")
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	title(t, a2, srv.URL+"/")
	if got := cookies(t, a2); got != "k=v" {
		t.Errorf("cookies after restarting group-a's browser = %q, want the persisted k=v", got)
	}
	if profiles, err := m.Profiles(); err != nil || len(profiles) != 2 {
		t.Errorf("Profiles = %v, %v", profiles, err)
	}
}

func TestPoolIsBounded(t *testing.T) {
	m := testManager(t, func(o *Options) { o.MaxBrowsers = 1 })
	srv := pageServer(t)
	ctx := timeout(t)
	a, err := m.Acquire(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	title(t, a, srv.URL+"/")

	// The only browser is busy: another group must wait.
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := m.Acquire(short, "two"); err == nil {
		t.Fatal("a second browser started beyond MaxBrowsers")
	}

	// Once the first group is idle it is evicted for the second.
	done := make(chan *Session, 1)
	go func() {
		s, err := m.Acquire(ctx, "two")
		if err != nil {
			t.Error(err)
		}
		done <- s
	}()
	time.Sleep(100 * time.Millisecond)
	a.Close()
	b := <-done
	if b == nil {
		t.FailNow()
	}
	defer b.Close()
	if got := title(t, b, srv.URL+"/x"); got != "page /x" {
		t.Errorf("title = %q", got)
	}
	if st := m.Status(); len(st.Running) != 1 || st.Running[0].Group != "two" {
		t.Errorf("Status = %+v, want only group two", st.Running)
	}
}

func TestDeadBrowserIsReplacedAndCloseStopsEverything(t *testing.T) {
	m := testManager(t, nil)
	srv := pageServer(t)
	ctx := timeout(t)
	a, err := m.Acquire(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	title(t, a, srv.URL+"/")
	if err := m.DeleteProfile("g"); err == nil {
		t.Error("DeleteProfile succeeded while the browser runs")
	}
	// Kill the browser behind the manager's back.
	m.mu.Lock()
	inst := m.instances["g"]
	m.mu.Unlock()
	inst.cancel()
	a.Close()
	eventually(t, "dead browser detection", func() bool { return inst.dead() })
	b, err := m.Acquire(ctx, "g")
	if err != nil {
		t.Fatalf("Acquire after the browser died: %v", err)
	}
	if got := title(t, b, srv.URL+"/again"); got != "page /again" {
		t.Errorf("title = %q", got)
	}
	_ = m.Close()
	if st := m.Status(); len(st.Running) != 0 {
		t.Errorf("Status after Close = %+v", st.Running)
	}
}
