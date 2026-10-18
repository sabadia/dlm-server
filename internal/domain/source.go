package domain

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ResolverKind names how a task's link was obtained and therefore how it can
// be refreshed.
type ResolverKind string

// Resolver kinds.
const (
	ResolverDirect  ResolverKind = "direct"  // a plain URL given by the user or extension
	ResolverBrowser ResolverKind = "browser" // sniffed by the internal headless browser
	ResolverYtdlp   ResolverKind = "ytdlp"   // extracted by yt-dlp
	ResolverPlugin  ResolverKind = "plugin"  // produced by a JS plugin
)

// Valid reports whether k is a known resolver kind.
func (k ResolverKind) Valid() bool {
	switch k {
	case ResolverDirect, ResolverBrowser, ResolverYtdlp, ResolverPlugin:
		return true
	default:
		return false
	}
}

// Source is everything needed to (re)obtain a task's download link: the link
// itself plus the origin references used by the refresh strategy chain.
type Source struct {
	// CurrentURL is the link the engine downloads from. Required.
	CurrentURL string `json:"currentUrl"`
	// OriginalURL is the first link the task was created with; the
	// origin-redirect refresh strategy re-requests it.
	OriginalURL string `json:"originalUrl,omitempty"`
	// RedirectURL is the final URL after redirects at the last probe.
	RedirectURL string `json:"redirectUrl,omitempty"`
	// PageURL is the web page the download originated from (page-reload strategy).
	PageURL  string `json:"pageUrl,omitempty"`
	Referrer string `json:"referrer,omitempty"`
	// Method is the HTTP method; empty means GET.
	Method string `json:"method,omitempty"`
	// Body is the request body (base64 in JSON).
	Body          []byte            `json:"body,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	UserAgent     string            `json:"userAgent,omitempty"`
	CookieDomains []string          `json:"cookieDomains,omitempty"`
	// MediaHint identifies the stream to pick (manifest pattern / variant).
	MediaHint string `json:"mediaHint,omitempty"`
	// Mirrors are other http(s) links to the same file. A ranged download
	// spreads its connections over the ones that serve a file of the same
	// size; the others are ignored.
	Mirrors []string `json:"mirrors,omitempty"`
	// ResolverKind defaults to direct when empty.
	ResolverKind ResolverKind `json:"resolverKind,omitempty"`
	Fingerprint  Fingerprint  `json:"fingerprint"`
}

// applyDefaults fills OriginalURL, Method and ResolverKind when empty.
func (s *Source) applyDefaults() {
	if s.OriginalURL == "" {
		s.OriginalURL = s.CurrentURL
	}
	if s.Method == "" {
		s.Method = http.MethodGet
	}
	if s.ResolverKind == "" {
		s.ResolverKind = ResolverDirect
	}
}

// Validate checks the source. CurrentURL is required; every URL that is set
// must be absolute (have a scheme). Method, when set, must be a valid HTTP
// token. It returns an error matching ErrInvalidArgument.
func (s *Source) Validate() error {
	if err := s.validate(); err != nil {
		return InvalidArgument("%w", err)
	}
	return nil
}

// validate returns a plain error so callers can add their own context.
func (s *Source) validate() error {
	if strings.TrimSpace(s.CurrentURL) == "" {
		return errors.New("source: currentUrl is required")
	}
	for _, f := range []struct{ name, val string }{
		{"currentUrl", s.CurrentURL},
		{"originalUrl", s.OriginalURL},
		{"redirectUrl", s.RedirectURL},
		{"pageUrl", s.PageURL},
		{"referrer", s.Referrer},
	} {
		if f.val == "" {
			continue
		}
		if err := checkAbsURL(f.val); err != nil {
			return fmt.Errorf("source: %s: %w", f.name, err)
		}
	}
	if len(s.Mirrors) > MaxMirrors {
		return fmt.Errorf("source: at most %d mirrors are allowed", MaxMirrors)
	}
	for _, m := range s.Mirrors {
		if err := checkMirror(m); err != nil {
			return fmt.Errorf("source: mirror %q: %w", m, err)
		}
	}
	if s.Method != "" && !isToken(s.Method) {
		return fmt.Errorf("source: invalid method %q", s.Method)
	}
	for name, val := range s.Headers {
		if !isToken(name) {
			return fmt.Errorf("source: invalid header name %q", name)
		}
		if strings.ContainsAny(val, "\r\n") {
			return fmt.Errorf("source: header %q value must not contain line breaks", name)
		}
	}
	if strings.ContainsAny(s.UserAgent, "\r\n") {
		return errors.New("source: userAgent must not contain line breaks")
	}
	for _, d := range s.CookieDomains {
		if strings.TrimSpace(d) == "" {
			return errors.New("source: cookieDomains must not contain empty entries")
		}
	}
	if s.ResolverKind != "" && !s.ResolverKind.Valid() {
		return fmt.Errorf("source: unknown resolverKind %q", s.ResolverKind)
	}
	if s.Fingerprint.Size < -1 {
		return errors.New("source: fingerprint size must be >= -1")
	}
	return nil
}

// MaxMirrors bounds Source.Mirrors.
const MaxMirrors = 16

// checkMirror requires an absolute http or https URL with a host.
func checkMirror(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return errors.New("a mirror must be an http or https URL")
	}
	if u.Hostname() == "" {
		return errors.New("a mirror needs a host")
	}
	return nil
}

func checkAbsURL(raw string) error {
	// An ed2k link is not a URL ("|" is not allowed in a host name); its
	// engine checks the rest.
	if len(raw) > len("ed2k://|") && strings.EqualFold(raw[:len("ed2k://|")], "ed2k://|") {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if !u.IsAbs() {
		return fmt.Errorf("URL %q must be absolute with a scheme", raw)
	}
	return nil
}

// Fingerprint identifies the file behind a link so that a refreshed link can
// be checked to point at the same content.
type Fingerprint struct {
	// Size in bytes; values <= 0 mean unknown (0 is treated as unknown so a
	// zero-value Fingerprint carries no information).
	Size int64 `json:"size"`
	// ETag holds a strong entity tag only; weak tags (W/"…") are ignored.
	ETag         string     `json:"etag,omitempty"`
	LastModified *time.Time `json:"lastModified,omitempty"`
	Filename     string     `json:"filename,omitempty"`
	ContentType  string     `json:"contentType,omitempty"`
}

// Matches reports whether other plausibly identifies the same file as f.
// When it does not, reason explains the first mismatch. Rules:
//   - if both sizes are known they must be equal;
//   - if both have strong ETags they must be equal;
//   - if both have filenames they must be equal, ignoring case;
//   - LastModified and ContentType are not compared.
func (f Fingerprint) Matches(other Fingerprint) (ok bool, reason string) {
	if f.Size > 0 && other.Size > 0 && f.Size != other.Size {
		return false, fmt.Sprintf("size differs: %d != %d", f.Size, other.Size)
	}
	if isStrongETag(f.ETag) && isStrongETag(other.ETag) && f.ETag != other.ETag {
		return false, fmt.Sprintf("etag differs: %s != %s", f.ETag, other.ETag)
	}
	if f.Filename != "" && other.Filename != "" && !strings.EqualFold(f.Filename, other.Filename) {
		return false, fmt.Sprintf("filename differs: %q != %q", f.Filename, other.Filename)
	}
	return true, ""
}

func isStrongETag(tag string) bool {
	return tag != "" && !strings.HasPrefix(tag, "W/")
}

// isToken reports whether s is a valid HTTP token (RFC 9110 §5.6.2).
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}
