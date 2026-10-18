package netclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type staticLogins map[string]Login

func (s staticLogins) Lookup(host string) (Login, bool) {
	l, ok := s[host]
	return l, ok
}

func TestDigestRFC2617Vector(t *testing.T) {
	ch, ok := parseDigest(`Digest realm="testrealm@host.com", qop="auth,auth-int", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`)
	if !ok {
		t.Fatal("challenge not parsed")
	}
	got := ch.authorizeWith(Login{Username: "Mufasa", Password: "Circle Of Life"}, "GET", "/dir/index.html", 1, "0a4f113b")
	for _, want := range []string{
		`response="6629fae49393a05397450978507c4ef1"`, `username="Mufasa"`, `nc=00000001`, `cnonce="0a4f113b"`, `qop=auth`,
		`opaque="5ccc069c403ebaf9f0171e9517f40e41"`, `uri="/dir/index.html"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("header lacks %s:\n%s", want, got)
		}
	}
}

func TestParseDigestRefusals(t *testing.T) {
	for _, h := range []string{
		`Basic realm="x"`,
		`Digest realm="x"`, // no nonce
		`Digest realm="x", nonce="n", algorithm=SHA-512-256`,
		`Digest realm="x", nonce="n", qop="auth-int"`,
	} {
		if _, ok := parseDigest(h); ok {
			t.Errorf("parseDigest(%q) accepted", h)
		}
	}
	if ch, ok := parseDigest(`Digest realm="a, b", nonce="n", algorithm=sha-256-sess`); !ok || ch.realm != "a, b" || !ch.sess || ch.algorithm != "SHA-256" {
		t.Errorf("parsed = %+v, %v", ch, ok)
	}
}

// digestServer answers 401 with a Digest challenge until the request carries a
// response that matches the reference computation.
func digestServer(t *testing.T, alg string, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	const nonce = "abc123nonce"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		challenge := `Digest realm="files", nonce="` + nonce + `", qop="auth", algorithm=` + alg + `, opaque="op"`
		h := r.Header.Get("Authorization")
		if h == "" {
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p := parseAuthParams(strings.TrimPrefix(h, "Digest "))
		ch, _ := parseDigest(challenge)
		var nc uint64
		for _, c := range p["nc"] {
			nc = nc*16 + uint64(strings.IndexRune("0123456789abcdef", c))
		}
		want := ch.authorizeWith(Login{Username: "ann", Password: "pw"}, r.Method, r.URL.RequestURI(), uint32(nc), p["cnonce"])
		if p["username"] != "ann" || !strings.Contains(want, `response="`+p["response"]+`"`) {
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "secret")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fetch(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestDigestLoginAnswersTheChallengeAndRemembersIt(t *testing.T) {
	for _, alg := range []string{"MD5", "SHA-256", "MD5-sess"} {
		t.Run(alg, func(t *testing.T) {
			var n atomic.Int32
			srv := digestServer(t, alg, &n)
			host := strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[0]
			c := &http.Client{Transport: newAuthTransport(http.DefaultTransport, staticLogins{host: {Username: "ann", Password: "pw"}})}
			if code, body := fetch(t, c, srv.URL+"/a?x=1"); code != 200 || body != "secret" {
				t.Fatalf("first: %d %q", code, body)
			}
			if n.Load() != 2 {
				t.Errorf("requests = %d, want the challenge and the answer", n.Load())
			}
			if code, _ := fetch(t, c, srv.URL+"/b"); code != 200 {
				t.Fatalf("second: %d", code)
			}
			if n.Load() != 3 {
				t.Errorf("requests = %d, want the learnt challenge to be answered without asking again", n.Load())
			}
		})
	}
}

func TestWrongDigestPasswordGivesTheChallengeBack(t *testing.T) {
	var n atomic.Int32
	srv := digestServer(t, "MD5", &n)
	host := strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[0]
	c := &http.Client{Transport: newAuthTransport(http.DefaultTransport, staticLogins{host: {Username: "ann", Password: "wrong"}})}
	if code, _ := fetch(t, c, srv.URL); code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
	if n.Load() != 2 {
		t.Errorf("requests = %d, want exactly one retry", n.Load())
	}
}

func basicServer(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if u, p, ok := r.BasicAuth(); !ok || u != "bob" || p != "s3" {
			w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBasicLogins(t *testing.T) {
	login := Login{Username: "bob", Password: "s3"}
	host := func(u string) string { return strings.Split(strings.TrimPrefix(u, "http://"), ":")[0] }

	t.Run("basic is sent with the first request", func(t *testing.T) {
		var n atomic.Int32
		srv := basicServer(t, &n)
		l := login
		l.Scheme = AuthBasic
		c := &http.Client{Transport: newAuthTransport(http.DefaultTransport, staticLogins{host(srv.URL): l})}
		if code, _ := fetch(t, c, srv.URL); code != 200 || n.Load() != 1 {
			t.Errorf("status %d after %d requests, want 200 after 1", code, n.Load())
		}
	})
	t.Run("auto waits for the challenge, then remembers", func(t *testing.T) {
		var n atomic.Int32
		srv := basicServer(t, &n)
		c := &http.Client{Transport: newAuthTransport(http.DefaultTransport, staticLogins{host(srv.URL): login})}
		if code, _ := fetch(t, c, srv.URL); code != 200 || n.Load() != 2 {
			t.Errorf("status %d after %d requests, want 200 after 2", code, n.Load())
		}
		if code, _ := fetch(t, c, srv.URL); code != 200 || n.Load() != 3 {
			t.Errorf("status %d after %d requests, want 200 after 3", code, n.Load())
		}
	})
	t.Run("digest only does not answer a Basic challenge", func(t *testing.T) {
		var n atomic.Int32
		srv := basicServer(t, &n)
		l := login
		l.Scheme = AuthDigest
		c := &http.Client{Transport: newAuthTransport(http.DefaultTransport, staticLogins{host(srv.URL): l})}
		if code, _ := fetch(t, c, srv.URL); code != http.StatusUnauthorized || n.Load() != 1 {
			t.Errorf("status %d after %d requests", code, n.Load())
		}
	})
	t.Run("hosts without a login are untouched", func(t *testing.T) {
		var n atomic.Int32
		srv := basicServer(t, &n)
		c := &http.Client{Transport: newAuthTransport(http.DefaultTransport, staticLogins{"other.example": login})}
		if code, _ := fetch(t, c, srv.URL); code != http.StatusUnauthorized || n.Load() != 1 {
			t.Errorf("status %d after %d requests", code, n.Load())
		}
	})
	t.Run("an Authorization header is never replaced", func(t *testing.T) {
		var seen string
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.Header.Get("Authorization") }))
		t.Cleanup(srv.Close)
		l := login
		l.Scheme = AuthBasic
		c := &http.Client{Transport: newAuthTransport(http.DefaultTransport, staticLogins{host(srv.URL): l})}
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.Header.Set("Authorization", "Bearer abc")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if seen != "Bearer abc" {
			t.Errorf("Authorization = %q", seen)
		}
	})
	t.Run("credentials do not follow a redirect to a host without a login", func(t *testing.T) {
		var seen string
		target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.Header.Get("Authorization") }))
		t.Cleanup(target.Close)
		redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, strings.Replace(target.URL, "127.0.0.1", "localhost", 1), http.StatusFound)
		}))
		t.Cleanup(redir.Close)
		l := login
		l.Scheme = AuthBasic
		c := &http.Client{Transport: newAuthTransport(http.DefaultTransport, staticLogins{host(redir.URL): l})}
		resp, err := c.Get(redir.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if seen != "" {
			t.Errorf("the redirect target received %q", seen)
		}
	})
}
