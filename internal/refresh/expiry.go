package refresh

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
)

// Timestamps outside this window are not Unix times (a "page=2" style value
// must never look like an expiry). Millisecond timestamps are scaled down
// first.
var (
	minUnix = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	maxUnix = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
)

// SignedURLExpiry reports when a URL carrying an expiry in its query stops
// working, so a refresh can happen before the link breaks. It understands
//
//   - AWS Signature V4 (X-Amz-Date + X-Amz-Expires) and the same scheme of
//     Google Cloud (X-Goog-Date + X-Goog-Expires), also used by many CDNs;
//   - Azure SAS (se, with sig);
//   - S3 Signature V2, CloudFront and GCS V2 (Expires with Signature,
//     AWSAccessKeyId, Key-Pair-Id or GoogleAccessId);
//   - generic Unix or RFC 3339 expiries (exp, expires, expiry, expire,
//     expiration) next to a signature-like parameter (sig, signature, token,
//     hmac, hash, policy, auth…).
//
// A bare expires=… without such a parameter is ignored: it may mean anything
// and a false positive would refresh a perfectly good link. With several
// expiries the earliest wins. ok is false when none was found.
func SignedURLExpiry(raw string) (expiresAt time.Time, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return time.Time{}, false
	}
	q := lowerKeys(u.Query())

	var found []time.Time
	add := func(t time.Time, ok bool) {
		if ok {
			found = append(found, t)
		}
	}
	add(relativeExpiry(q, "x-amz-date", "x-amz-expires"))
	add(relativeExpiry(q, "x-goog-date", "x-goog-expires"))
	if _, signed := q["sig"]; signed {
		add(parseTime(first(q, "se")))
	}
	vendorSigned := hasAny(q, "awsaccesskeyid", "signature", "key-pair-id", "googleaccessid")
	genericSigned := vendorSigned || hasSignatureLike(q)
	if vendorSigned {
		add(parseTime(first(q, "expires")))
	}
	if genericSigned {
		for _, k := range []string{"exp", "expires", "expiry", "expire", "expiration"} {
			add(parseTime(first(q, k)))
		}
	}
	if len(found) == 0 {
		return time.Time{}, false
	}
	earliest := found[0]
	for _, t := range found[1:] {
		if t.Before(earliest) {
			earliest = t
		}
	}
	return earliest, true
}

// SourceExpiry is SignedURLExpiry for a task source: the URL actually
// requested (the redirect target when known) and the current URL, whichever
// expires first.
func SourceExpiry(src domain.Source) (expiresAt time.Time, ok bool) {
	for _, raw := range []string{src.RedirectURL, src.CurrentURL} {
		if raw == "" {
			continue
		}
		if t, found := SignedURLExpiry(raw); found && (!ok || t.Before(expiresAt)) {
			expiresAt, ok = t, true
		}
	}
	return expiresAt, ok
}

// ExpiresWithin reports whether the source's link has expired or expires
// within margin of now. It is false when the link carries no expiry.
func ExpiresWithin(src domain.Source, now time.Time, margin time.Duration) bool {
	t, ok := SourceExpiry(src)
	return ok && !t.After(now.Add(margin))
}

func lowerKeys(v url.Values) map[string][]string {
	out := make(map[string][]string, len(v))
	for k, vals := range v {
		out[strings.ToLower(k)] = vals
	}
	return out
}

func first(q map[string][]string, key string) string {
	if v := q[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func hasAny(q map[string][]string, keys ...string) bool {
	for _, k := range keys {
		if _, ok := q[k]; ok {
			return true
		}
	}
	return false
}

func hasSignatureLike(q map[string][]string) bool {
	for k := range q {
		for _, frag := range []string{"sig", "token", "hmac", "hash", "policy", "auth"} {
			if strings.Contains(k, frag) {
				return true
			}
		}
	}
	return false
}

// relativeExpiry handles "<date>" + "<seconds>" signing: the link is valid
// for seconds after the signing date (basic ISO 8601, e.g. 20260102T030405Z).
func relativeExpiry(q map[string][]string, dateKey, secondsKey string) (time.Time, bool) {
	date, err := time.Parse("20060102T150405Z", first(q, dateKey))
	if err != nil {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(first(q, secondsKey), 10, 64)
	if err != nil || secs < 0 || secs > int64(10*365*24*time.Hour/time.Second) {
		return time.Time{}, false
	}
	return date.Add(time.Duration(secs) * time.Second), true
}

// parseTime reads a Unix timestamp in seconds or milliseconds, or an
// RFC 3339 time.
func parseTime(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n > maxUnix*10 { // milliseconds
			n /= 1000
		}
		if n < minUnix || n > maxUnix {
			return time.Time{}, false
		}
		return time.Unix(n, 0).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}
