package netclient

import (
	"crypto/md5" //nolint:gosec // HTTP Digest authentication (RFC 7616) defines MD5; servers choose it
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Authentication schemes of a stored login.
const (
	// AuthAuto answers whatever the server asks for: Digest when offered,
	// else Basic.
	AuthAuto = "auto"
	// AuthBasic sends Basic credentials with the first request.
	AuthBasic = "basic"
	// AuthDigest answers a Digest challenge.
	AuthDigest = "digest"
)

// Login is a stored username and password for a host.
type Login struct {
	Username string
	Password string
	// Scheme is AuthAuto, AuthBasic or AuthDigest; empty means AuthAuto.
	Scheme string
}

// CredentialSource finds the login for a host name (no port). Lookup is called
// for every request, so it must be cheap.
type CredentialSource interface {
	Lookup(host string) (Login, bool)
}

// authTransport adds stored credentials to requests: Basic preemptively when
// the login says so, otherwise after the server's 401 challenge (Basic or
// Digest). A request that already has an Authorization header is left alone.
// Redirects are looked up per hop, so credentials never follow a redirect to a
// host that has none of its own.
type authTransport struct {
	base http.RoundTripper
	src  CredentialSource

	mu    sync.Mutex
	state map[string]*authState // by lower-case host
}

// authState is what was learnt about a host's authentication.
type authState struct {
	basic  bool // send Basic preemptively
	digest *digestChallenge
	nc     uint32
}

func newAuthTransport(base http.RoundTripper, src CredentialSource) *authTransport {
	return &authTransport{base: base, src: src, state: make(map[string]*authState)}
}

// CloseIdleConnections forwards to the wrapped transport.
func (t *authTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// RoundTrip implements http.RoundTripper. It never mutates req.
func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" {
		return t.base.RoundTrip(req) //nolint:wrapcheck // transport errors pass through unchanged
	}
	host := strings.ToLower(req.URL.Hostname())
	login, ok := t.src.Lookup(host)
	if !ok {
		return t.base.RoundTrip(req) //nolint:wrapcheck // transport errors pass through unchanged
	}
	scheme := login.Scheme
	if scheme == "" {
		scheme = AuthAuto
	}

	first := req
	if h := t.preemptive(host, scheme, login, req); h != "" {
		first = req.Clone(req.Context())
		first.Header.Set("Authorization", h)
	}
	resp, err := t.base.RoundTrip(first)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err //nolint:wrapcheck // transport errors pass through unchanged
	}
	h := t.answer(host, scheme, login, req, resp.Header.Values("WWW-Authenticate"))
	if h == "" || (req.Body != nil && req.Body != http.NoBody && req.GetBody == nil) {
		return resp, nil // nothing to answer with, or the body cannot be sent again
	}
	retry := req.Clone(req.Context())
	if req.GetBody != nil && req.Body != nil && req.Body != http.NoBody {
		body, err := req.GetBody()
		if err != nil {
			return resp, nil //nolint:nilerr // the 401 is the useful answer
		}
		retry.Body = body
	}
	retry.Header.Set("Authorization", h)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	return t.base.RoundTrip(retry) //nolint:wrapcheck // transport errors pass through unchanged
}

// preemptive returns the Authorization value to send with the first request,
// or "".
func (t *authTransport) preemptive(host, scheme string, l Login, req *http.Request) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state[host]
	switch {
	case scheme == AuthBasic || (st != nil && st.basic && scheme == AuthAuto):
		return basicHeader(l)
	case st != nil && st.digest != nil && scheme != AuthBasic:
		st.nc++
		return st.digest.authorize(l, req.Method, req.URL.RequestURI(), st.nc)
	}
	return ""
}

// answer builds the Authorization value that answers a 401 and remembers what
// worked for the next request; "" when the server's challenges are not ones the
// login can answer.
func (t *authTransport) answer(host, scheme string, l Login, req *http.Request, challenges []string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state[host]
	if st == nil {
		st = &authState{}
		t.state[host] = st
	}
	if scheme != AuthBasic {
		for _, c := range challenges {
			if ch, ok := parseDigest(c); ok {
				st.digest, st.nc, st.basic = ch, 1, false
				return ch.authorize(l, req.Method, req.URL.RequestURI(), 1)
			}
		}
	}
	if scheme != AuthDigest {
		for _, c := range challenges {
			if strings.EqualFold(strings.Fields(c + " ")[0], "basic") {
				st.basic, st.digest = true, nil
				return basicHeader(l)
			}
		}
	}
	return ""
}

func basicHeader(l Login) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(l.Username+":"+l.Password))
}

// digestChallenge is a parsed "WWW-Authenticate: Digest ..." header.
type digestChallenge struct {
	realm, nonce, opaque string
	algorithm            string // as sent, without "-sess"
	sess                 bool
	qop                  bool // "auth" is offered
}

// parseDigest reads a Digest challenge; ok is false for other schemes and for
// challenges this implementation cannot answer (unknown algorithm, only
// qop=auth-int).
func parseDigest(header string) (*digestChallenge, bool) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(header), " ")
	if !strings.EqualFold(scheme, "digest") {
		return nil, false
	}
	params := parseAuthParams(rest)
	ch := &digestChallenge{realm: params["realm"], nonce: params["nonce"], opaque: params["opaque"]}
	if ch.nonce == "" {
		return nil, false
	}
	alg := strings.ToUpper(params["algorithm"])
	if alg == "" {
		alg = "MD5"
	}
	if base, ok := strings.CutSuffix(alg, "-SESS"); ok {
		alg, ch.sess = base, true
	}
	if alg != "MD5" && alg != "SHA-256" {
		return nil, false
	}
	ch.algorithm = alg
	if q := params["qop"]; q != "" {
		for _, v := range strings.Split(q, ",") {
			if strings.EqualFold(strings.TrimSpace(v), "auth") {
				ch.qop = true
			}
		}
		if !ch.qop {
			return nil, false // only auth-int
		}
	}
	return ch, true
}

// parseAuthParams reads key=value pairs where values may be quoted strings.
func parseAuthParams(s string) map[string]string {
	out := map[string]string{}
	for {
		s = strings.TrimLeft(s, ", \t")
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			return out
		}
		key = strings.ToLower(strings.TrimSpace(key))
		rest = strings.TrimLeft(rest, " \t")
		var val string
		if strings.HasPrefix(rest, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(rest) && rest[i] != '"'; i++ {
				if rest[i] == '\\' && i+1 < len(rest) {
					i++
				}
				b.WriteByte(rest[i])
			}
			val, s = b.String(), rest[min(i+1, len(rest)):]
		} else {
			val, s, _ = strings.Cut(rest, ",")
			val = strings.TrimSpace(val)
		}
		out[key] = val
	}
}

func (c *digestChallenge) hash() hash.Hash {
	if c.algorithm == "SHA-256" {
		return sha256.New()
	}
	return md5.New() //nolint:gosec // see the import
}

func (c *digestChallenge) h(s string) string {
	h := c.hash()
	_, _ = io.WriteString(h, s)
	return hex.EncodeToString(h.Sum(nil))
}

// authorize returns the Authorization header value for one request.
func (c *digestChallenge) authorize(l Login, method, uri string, nc uint32) string {
	return c.authorizeWith(l, method, uri, nc, randomHex(8))
}

func (c *digestChallenge) authorizeWith(l Login, method, uri string, nc uint32, cnonce string) string {
	ha1 := c.h(l.Username + ":" + c.realm + ":" + l.Password)
	if c.sess {
		ha1 = c.h(ha1 + ":" + c.nonce + ":" + cnonce)
	}
	ha2 := c.h(method + ":" + uri)
	ncText := fmt.Sprintf("%08x", nc)
	var response string
	if c.qop {
		response = c.h(ha1 + ":" + c.nonce + ":" + ncText + ":" + cnonce + ":auth:" + ha2)
	} else {
		response = c.h(ha1 + ":" + c.nonce + ":" + ha2)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `Digest username=%s, realm=%s, nonce=%s, uri=%s, algorithm=%s, response="%s"`,
		quote(l.Username), quote(c.realm), quote(c.nonce), quote(uri), c.algorithmName(), response)
	if c.opaque != "" {
		fmt.Fprintf(&b, ", opaque=%s", quote(c.opaque))
	}
	if c.qop {
		fmt.Fprintf(&b, `, qop=auth, nc=%s, cnonce="%s"`, ncText, cnonce)
	}
	return b.String()
}

func (c *digestChallenge) algorithmName() string {
	if c.sess {
		return c.algorithm + "-sess"
	}
	return c.algorithm
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
