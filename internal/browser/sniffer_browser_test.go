//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/sabadia/dlm-server/internal/domain"
)

const mediaPage = `<!doctype html><html><head><title>media page</title></head><body>
<video src="/v.mp4" preload="auto" muted></video>
<img src="/i.png">
<script>
fetch('/master.m3u8').then(r => r.text());
fetch('/manifest.mpd').then(r => r.text());
fetch('/seg1.ts').then(r => r.arrayBuffer());
fetch('/api/data').then(r => r.json());
window.addEventListener('load', () => {
  const a = document.createElement('a');
  a.href = '/files/archive.zip'; a.download = ''; document.body.appendChild(a); a.click();
  setTimeout(() => {
    const b = document.createElement('a');
    b.href = URL.createObjectURL(new Blob(['blob-content'], {type: 'text/plain'}));
    b.download = 'made-in-js.txt'; document.body.appendChild(b); b.click();
  }, 200);
});
</script></body></html>`

func mediaServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	typed := func(path, ctype, body string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ctype)
			_, _ = fmt.Fprint(w, body)
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, mediaPage)
	})
	typed("/v.mp4", "video/mp4", strings.Repeat("0123456789", 100))
	typed("/master.m3u8", "application/vnd.apple.mpegurl", "#EXTM3U\n")
	typed("/manifest.mpd", "application/dash+xml", "<MPD/>")
	typed("/seg1.ts", "video/mp2t", "segment")
	typed("/i.png", "image/png", "png")
	typed("/api/data", "application/json", "{}")
	mux.HandleFunc("/files/archive.zip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="archive.zip"`)
		_, _ = fmt.Fprint(w, "PK-fake-archive")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSnifferFindsMediaAndDownloads(t *testing.T) {
	m := testManager(t, nil)
	srv := mediaServer(t)
	ctx := timeout(t)
	sess, err := m.Acquire(ctx, "sniff")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sn, err := StartSniffer(sess.Ctx, SnifferOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Close()
	dir := sn.dir

	if err := chromedp.Do(sess.Ctx, chromedp.Navigate(srv.URL+"/")); err != nil {
		t.Fatal(err)
	}
	cands := sn.Wait(ctx, 1500*time.Millisecond, 20*time.Second)

	byName := map[string]Candidate{}
	for _, c := range cands {
		if strings.HasPrefix(c.URL, "blob:") {
			byName["blob"] = c
			continue
		}
		u, _ := url.Parse(c.URL)
		byName[strings.TrimPrefix(u.Path, "/")] = c
	}
	for path, kind := range map[string]Kind{"master.m3u8": KindHLS, "manifest.mpd": KindDASH, "v.mp4": KindVideo, "files/archive.zip": KindFile, "blob": KindFile} {
		if got, ok := byName[path]; !ok || got.Kind != kind {
			t.Errorf("candidate %q = %+v (found: %v), want kind %s", path, got, ok, kind)
		}
	}
	for _, noise := range []string{"seg1.ts", "i.png", "api/data", ""} {
		if c, ok := byName[noise]; ok {
			t.Errorf("noise %q reported as a candidate: %+v", noise, c)
		}
	}

	hls := byName["master.m3u8"]
	if hls.Source != "network" || hls.MIME != "application/vnd.apple.mpegurl" || hls.Size != int64(len("#EXTM3U\n")) || hls.Headers["Referer"] != srv.URL+"/" {
		t.Errorf("hls candidate = %+v", hls)
	}
	zip := byName["files/archive.zip"]
	if zip.Source != "download" || zip.Filename != "archive.zip" || zip.File == "" {
		t.Fatalf("archive candidate = %+v, want a completed browser download", zip)
	}
	if got, err := os.ReadFile(zip.File); err != nil || string(got) != "PK-fake-archive" {
		t.Errorf("downloaded archive = %q, %v", got, err)
	}
	blob := byName["blob"]
	if blob.Source != "blob" || blob.Filename != "made-in-js.txt" || blob.File == "" {
		t.Fatalf("blob candidate = %+v", blob)
	}
	if got, err := os.ReadFile(blob.File); err != nil || string(got) != "blob-content" {
		t.Errorf("blob file = %q, %v", got, err)
	}

	// Close removes the sniffer's temporary directory.
	sn.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temporary download directory still exists: %v", err)
	}
}

// memStore is an in-memory CookieStore.
type memStore struct {
	mu      sync.Mutex
	cookies []*domain.Cookie
}

func (s *memStore) ForSite(_ context.Context, site string) ([]*domain.Cookie, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*domain.Cookie
	for _, c := range s.cookies {
		if c.Domain == site || strings.HasSuffix(c.Domain, "."+site) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *memStore) Put(_ context.Context, c *domain.Cookie) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cookies = slices.DeleteFunc(s.cookies, func(o *domain.Cookie) bool { return o.Domain == c.Domain && o.Name == c.Name && o.Path == c.Path })
	s.cookies = append(s.cookies, c)
	return nil
}

func (s *memStore) find(name string) *domain.Cookie {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestCookiesSyncBothWays(t *testing.T) {
	m := testManager(t, nil)
	ctx := timeout(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<html><head><title>cookie:%s</title></head></html>", r.Header.Get("Cookie"))
	})
	mux.HandleFunc("/set", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "srv", Value: "from-server", Path: "/", MaxAge: 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		http.SetCookie(w, &http.Cookie{Name: "sess", Value: "session-only", Path: "/"})
		fmt.Fprint(w, "<html><head><title>set</title></head></html>")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(strings.Split(srv.URL, ":")[1], "//") // 127.0.0.1

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	store := &memStore{cookies: []*domain.Cookie{
		{Domain: host, Name: "login", Value: "abc", Path: "/", HostOnly: true, Secure: false, ExpiresAt: &exp},
		{Domain: host, Name: "stale", Value: "x", Path: "/", HostOnly: true, ExpiresAt: ptr(time.Now().Add(-time.Hour))},
	}}
	sess, err := m.Acquire(ctx, "cookie-sync")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Store -> browser: the page is opened with the stored login (and the expired cookie is not sent).
	n, err := PushCookies(ctx, sess.Ctx, store, host)
	if err != nil || n != 1 {
		t.Fatalf("PushCookies = %d, %v; want 1", n, err)
	}
	if got := title(t, sess, srv.URL+"/whoami"); got != "cookie:login=abc" {
		t.Errorf("the server saw %q, want the stored login", got)
	}

	// Browser -> store: what the page set comes back; third-party cookies stay out.
	if _, err := InjectCookies(sess.Ctx, []*domain.Cookie{{Domain: "tracker.test", Name: "t", Value: "1", Path: "/", HostOnly: true}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	title(t, sess, srv.URL+"/set")
	n, err = PullCookies(ctx, sess.Ctx, store, func(d string) bool { return d == host })
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 { // login, srv, sess
		t.Errorf("PullCookies stored %d cookies, want 3", n)
	}
	srvCookie := store.find("srv")
	if srvCookie == nil || srvCookie.Value != "from-server" || !srvCookie.HTTPOnly || srvCookie.SameSite != "lax" || srvCookie.ExpiresAt == nil ||
		time.Until(*srvCookie.ExpiresAt) < 50*time.Minute || !srvCookie.HostOnly || srvCookie.Domain != host {
		t.Errorf("srv cookie = %+v", srvCookie)
	}
	if sc := store.find("sess"); sc == nil || sc.ExpiresAt != nil || sc.Value != "session-only" {
		t.Errorf("session cookie = %+v, want no expiry", sc)
	}
	if store.find("t") != nil {
		t.Error("the third-party cookie was copied to the store")
	}
	if login := store.find("login"); login == nil || login.ExpiresAt == nil || login.ExpiresAt.Sub(exp).Abs() > time.Second {
		t.Errorf("login cookie after the round trip = %+v", login)
	}
}

func ptr[T any](v T) *T { return &v }
