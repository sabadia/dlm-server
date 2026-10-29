package api

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/auth"
)

// Limits of an extension token (auth.ScopeExtension). A browser extension makes
// a handful of requests per download, so these are far above what it needs and
// far below what a leaked token could use to flood the daemon.
const (
	// ExtensionWriteRate is the sustained requests per second of an extension
	// token that change something (everything but GET and HEAD); ExtensionWriteBurst
	// is how many it may send at once.
	ExtensionWriteRate  = 10.0
	ExtensionWriteBurst = 30.0
	// ExtensionReadRate and ExtensionReadBurst are the same for reading.
	ExtensionReadRate  = 50.0
	ExtensionReadBurst = 100.0

	// Caps on one capture from an extension token.
	MaxExtensionCookies         = 200
	MaxExtensionCookieBytes     = 8 << 10   // one cookie's name and value
	MaxExtensionCookiesTotal    = 256 << 10 // all cookies of one capture
	MaxExtensionHeaders         = 64
	MaxExtensionHeadersTotal    = 32 << 10
	MaxExtensionMediaCandidates = 100
	MaxExtensionBatchItems      = 100
)

// bucket is one token bucket.
type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter keeps a token bucket per key and kind of request. It is safe for
// concurrent use.
type rateLimiter struct {
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{now: time.Now, buckets: map[string]*bucket{}}
}

// allow takes one token from the bucket of key. When none is left it returns
// how long until one is.
func (l *rateLimiter) allow(key string, rate, burst float64) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / rate * float64(time.Second))
}

// forget drops the buckets of a token (it was revoked).
func (l *rateLimiter) forget(tokenID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, "r:"+tokenID)
	delete(l.buckets, "w:"+tokenID)
}

// limitExtension applies the rate limit to a request of an extension token. It
// answers 429 with Retry-After and returns false when the token is over it.
func (s *Server) limitExtension(w http.ResponseWriter, r *http.Request, p auth.Principal) bool {
	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	key, rate, burst := "r:"+p.ID, ExtensionReadRate, ExtensionReadBurst
	if write {
		key, rate, burst = "w:"+p.ID, ExtensionWriteRate, ExtensionWriteBurst
	}
	ok, wait := s.limiter.allow(key, rate, burst)
	if ok {
		return true
	}
	secs := int(math.Ceil(wait.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
	writeCodedProblem(w, http.StatusTooManyRequests, "rate_limited", "too many requests from this token; retry after "+strconv.Itoa(max(secs, 1))+" s")
	return false
}

// checkExtensionCapture refuses a capture from an extension token that is
// larger than anything a browser sends (cookies, headers, media candidates).
func checkExtensionCapture(b *CaptureBody) error {
	if len(b.Cookies) > MaxExtensionCookies {
		return captureTooBig("cookies", MaxExtensionCookies)
	}
	total := 0
	for _, c := range b.Cookies {
		n := len(c.Name) + len(c.Value)
		if n > MaxExtensionCookieBytes {
			return captureTooBig("a cookie's name and value", MaxExtensionCookieBytes)
		}
		total += n + len(c.Domain) + len(c.Path)
	}
	if total > MaxExtensionCookiesTotal {
		return captureTooBig("the cookies together", MaxExtensionCookiesTotal)
	}
	if len(b.Headers) > MaxExtensionHeaders {
		return captureTooBig("headers", MaxExtensionHeaders)
	}
	total = 0
	for k, v := range b.Headers {
		total += len(k) + len(v)
	}
	if total > MaxExtensionHeadersTotal {
		return captureTooBig("the headers together", MaxExtensionHeadersTotal)
	}
	if len(b.MediaCandidates) > MaxExtensionMediaCandidates {
		return captureTooBig("media candidates", MaxExtensionMediaCandidates)
	}
	return nil
}

func captureTooBig(what string, limit int) error {
	return newProblem(http.StatusUnprocessableEntity, "a capture from a browser extension may carry at most "+strconv.Itoa(limit)+" ("+what+")").
		withCode("capture.too_large", map[string]string{"what": what, "limit": strconv.Itoa(limit)})
}
