package settings

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // embed zone data so schedules validate on every OS
)

// Limits for numeric settings.
const (
	MaxConnectionsLimit         = 128
	MaxConcurrentDownloadsLimit = 100
	MaxRetryAttemptsLimit       = 100
	MaxRefreshAttemptsLimit     = 20
)

// FieldError describes one invalid setting.
type FieldError struct {
	Path    string // JSON path, e.g. "retry.maxAttempts"
	Message string
}

func (e *FieldError) Error() string { return e.Path + ": " + e.Message }

// ValidationError lists every invalid field found by Validate.
type ValidationError struct {
	Level  Level
	Fields []*FieldError
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		msgs[i] = f.Error()
	}
	return fmt.Sprintf("settings: invalid %s settings: %s", e.Level, strings.Join(msgs, "; "))
}

// Validate checks that s may be stored at level: every set field must be
// allowed at that level and hold a valid value. It returns a
// *ValidationError listing all problems, or nil.
func (s *Settings) Validate(level Level) error {
	v := &validator{}

	sv := reflect.ValueOf(*s)
	for _, f := range fields() {
		if !f.allowed(level) && sv.FieldByIndex(f.index).Interface().(optionalValue).isSet() { //nolint:errcheck,forcetypeassert // fields() only yields optionalValue types
			v.add(f.path, fmt.Sprintf("cannot be set at %s level", level))
		}
	}

	if d, ok := s.DownloadDir.Get(); ok && !filepath.IsAbs(d) {
		v.add("downloadDir", "must be an absolute path")
	}
	if t, ok := s.FilenameTemplate.Get(); ok && (strings.TrimSpace(t) == "" || strings.ContainsAny(t, `/\`)) {
		v.add("filenameTemplate", "must be non-empty and must not contain path separators")
	}
	v.intRange("maxConnections", s.MaxConnections, 1, MaxConnectionsLimit)
	v.intRange("maxConcurrentDownloads", s.MaxConcurrentDownloads, 1, MaxConcurrentDownloadsLimit)
	v.intRange("maxActiveDownloadsTotal", s.MaxActiveDownloadsTotal, 0, 10_000)
	if n, ok := s.SpeedLimit.Get(); ok && n < 0 {
		v.add("speedLimit", "must be >= 0 (0 = unlimited)")
	}

	v.intRange("retry.maxAttempts", s.Retry.MaxAttempts, 0, MaxRetryAttemptsLimit)
	v.positiveDuration("retry.initialBackoff", s.Retry.InitialBackoff)
	v.positiveDuration("retry.maxBackoff", s.Retry.MaxBackoff)
	if lo, ok := s.Retry.InitialBackoff.Get(); ok {
		if hi, ok := s.Retry.MaxBackoff.Get(); ok && hi < lo {
			v.add("retry.maxBackoff", "must be >= retry.initialBackoff")
		}
	}

	if p, ok := s.Proxy.Get(); ok {
		v.proxy("proxy", p)
	}
	if rules, ok := s.ProxyRules.Get(); ok {
		for i, r := range rules {
			prefix := fmt.Sprintf("proxyRules[%d]", i)
			if len(r.Hosts) == 0 {
				v.add(prefix+".hosts", "at least one host pattern is required")
			}
			for _, h := range r.Hosts {
				if _, err := path.Match(h, ""); err != nil || strings.TrimSpace(h) == "" || strings.ContainsAny(h, "/ ") {
					v.add(prefix+".hosts", fmt.Sprintf("invalid host pattern %q", h))
				}
			}
			v.proxy(prefix+".proxy", r.Proxy)
		}
	}
	if ua, ok := s.UserAgent.Get(); ok && strings.ContainsAny(ua, "\r\n") {
		v.add("userAgent", "must not contain line breaks")
	}
	if h, ok := s.Headers.Get(); ok {
		for name, val := range h {
			if !isToken(name) {
				v.add("headers", fmt.Sprintf("invalid header name %q", name))
			}
			if strings.ContainsAny(val, "\r\n") {
				v.add("headers", fmt.Sprintf("header %q value must not contain line breaks", name))
			}
		}
	}

	enum(v, "fileExistsPolicy", s.FileExistsPolicy, FileExistsRename, FileExistsOverwrite, FileExistsSkip, FileExistsResume, FileExistsAsk)
	if c, ok := s.Checksum.Get(); ok {
		v.checksum(c)
	}

	v.intRange("refresh.maxAttempts", s.Refresh.MaxAttempts, 0, MaxRefreshAttemptsLimit)
	v.positiveDuration("refresh.awaitTimeout", s.Refresh.AwaitTimeout)
	enum(v, "refresh.onMismatch", s.Refresh.OnMismatch, MismatchRestart, MismatchFail, MismatchAsk)
	if strategies, ok := s.Refresh.Strategies.Get(); ok {
		known := []RefreshStrategy{RefreshOriginRedirect, RefreshPageReload, RefreshExtensionCapture, RefreshPlugin, RefreshManual}
		seen := map[RefreshStrategy]bool{}
		for _, st := range strategies {
			if !slices.Contains(known, st) {
				v.add("refresh.strategies", fmt.Sprintf("unknown strategy %q", st))
			}
			if seen[st] {
				v.add("refresh.strategies", fmt.Sprintf("duplicate strategy %q", st))
			}
			seen[st] = true
		}
	}

	enum(v, "browserUsage", s.BrowserUsage, BrowserNever, BrowserAuto, BrowserAlways)
	if actions, ok := s.PostActions.Get(); ok {
		for i, a := range actions {
			if strings.TrimSpace(a.Kind) == "" {
				v.add(fmt.Sprintf("postActions[%d].kind", i), "is required")
			}
		}
	}
	if sch, ok := s.Schedule.Get(); ok {
		v.schedule(sch)
	}

	if len(v.errs) == 0 {
		return nil
	}
	return &ValidationError{Level: level, Fields: v.errs}
}

type validator struct{ errs []*FieldError }

func (v *validator) add(path, msg string) {
	v.errs = append(v.errs, &FieldError{Path: path, Message: msg})
}

func (v *validator) intRange(path string, o Optional[int], lo, hi int) {
	if n, ok := o.Get(); ok && (n < lo || n > hi) {
		v.add(path, fmt.Sprintf("must be between %d and %d", lo, hi))
	}
}

func (v *validator) positiveDuration(path string, o Optional[Duration]) {
	if d, ok := o.Get(); ok && d <= 0 {
		v.add(path, "must be a positive duration")
	}
}

func (v *validator) proxy(path string, p Proxy) {
	switch p.Mode {
	case ProxyNone, ProxySystem:
		if p.URL != "" {
			v.add(path+".url", fmt.Sprintf("must be empty for mode %q", p.Mode))
		}
	case ProxyCustom:
		u, err := url.Parse(p.URL)
		if err != nil || u.Host == "" || !slices.Contains([]string{"http", "https", "socks5", "socks5h"}, u.Scheme) {
			v.add(path+".url", "must be an http, https, socks5 or socks5h URL with a host")
		}
	default:
		v.add(path+".mode", fmt.Sprintf("unknown mode %q", p.Mode))
	}
}

func (v *validator) checksum(c Checksum) {
	sizes := map[ChecksumAlgorithm]int{ChecksumMD5: 16, ChecksumSHA1: 20, ChecksumSHA256: 32, ChecksumBLAKE3: 32}
	size, ok := sizes[c.Algorithm]
	if !ok {
		v.add("checksum.algorithm", fmt.Sprintf("unknown algorithm %q", c.Algorithm))
		return
	}
	b, err := hex.DecodeString(c.Expected)
	if err != nil || len(b) != size {
		v.add("checksum.expected", fmt.Sprintf("must be a %d-character hex string", size*2))
	}
}

func (v *validator) schedule(s Schedule) {
	if s.Timezone != "" {
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			v.add("schedule.timezone", fmt.Sprintf("unknown time zone %q", s.Timezone))
		}
	}
	if len(s.Windows) == 0 {
		v.add("schedule.windows", "at least one window is required")
	}
	for i, w := range s.Windows {
		p := fmt.Sprintf("schedule.windows[%d]", i)
		if len(w.Days) == 0 {
			v.add(p+".days", "at least one day is required")
		}
		for _, d := range w.Days {
			if d < 0 || d > 6 {
				v.add(p+".days", fmt.Sprintf("invalid weekday %d", int(d)))
			}
		}
		start, errS := ParseClock(w.Start)
		end, errE := ParseClock(w.End)
		if errS != nil {
			v.add(p+".start", errS.Error())
		}
		if errE != nil {
			v.add(p+".end", errE.Error())
		}
		if errS == nil && errE == nil && start == end {
			v.add(p, "start and end must differ")
		}
	}
}

// ParseClock parses "HH:MM" (24h) and returns minutes since midnight.
func ParseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil || len(s) != 5 {
		return 0, errors.New("must be HH:MM (24h)")
	}
	return t.Hour()*60 + t.Minute(), nil
}

// enum validates a string-kinded Optional against the allowed values.
func enum[T ~string](v *validator, path string, o Optional[T], allowed ...T) {
	val, ok := o.Get()
	if !ok || slices.Contains(allowed, val) {
		return
	}
	names := make([]string, len(allowed))
	for i, a := range allowed {
		names[i] = string(a)
	}
	v.add(path, "must be one of "+strings.Join(names, ", "))
}

// isToken reports whether s is a valid HTTP header field name (RFC 9110).
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isTokenChar(r) {
			return false
		}
	}
	return true
}

func isTokenChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	default:
		return strings.ContainsRune("!#$%&'*+-.^_`|~", r)
	}
}
