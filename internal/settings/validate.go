package settings

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
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
	MaxSeedRatio                = 1000
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

	if r, ok := s.Torrent.SeedRatio.Get(); ok && (r < 0 || r > MaxSeedRatio || math.IsNaN(r)) {
		v.add("torrent.seedRatio", fmt.Sprintf("must be between 0 and %d", MaxSeedRatio))
	}
	if d, ok := s.Torrent.SeedTime.Get(); ok && d < 0 {
		v.add("torrent.seedTime", "must not be negative")
	}
	if trackers, ok := s.Torrent.Trackers.Get(); ok {
		for _, raw := range trackers {
			if u, err := url.Parse(raw); err != nil || u.Host == "" || !slices.Contains([]string{"http", "https", "udp", "wss"}, u.Scheme) {
				v.add("torrent.trackers", fmt.Sprintf("%q is not an http, https, udp or wss tracker URL", raw))
			}
		}
	}

	enum(v, "browserUsage", s.BrowserUsage, BrowserNever, BrowserAuto, BrowserAlways)
	if actions, ok := s.PostActions.Get(); ok {
		for i, a := range actions {
			v.postAction(fmt.Sprintf("postActions[%d]", i), a)
		}
	}
	if sch, ok := s.Schedule.Get(); ok {
		v.windows("schedule", sch.Timezone, sch.Windows)
	}
	if profiles, ok := s.SpeedProfiles.Get(); ok {
		for i, p := range profiles {
			prefix := fmt.Sprintf("speedProfiles[%d]", i)
			v.windows(prefix, p.Timezone, p.Windows)
			if p.SpeedLimit < 0 {
				v.add(prefix+".speedLimit", "must be >= 0 (0 = unlimited)")
			}
		}
	}
	if q, ok := s.Quota.Get(); ok {
		if q.Bytes <= 0 {
			v.add("quota.bytes", "must be > 0")
		}
		if q.Period.Std() < MinQuotaPeriod {
			v.add("quota.period", "must be at least "+MinQuotaPeriod.String())
		}
	}
	if a, ok := s.OnQueueComplete.Get(); ok {
		v.queueAction(a)
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

// windows validates a time zone and its windows under prefix (for example
// "schedule" or "speedProfiles[0]").
func (v *validator) windows(prefix, timezone string, windows []TimeWindow) {
	if timezone != "" {
		if _, err := time.LoadLocation(timezone); err != nil {
			v.add(prefix+".timezone", fmt.Sprintf("unknown time zone %q", timezone))
		}
	}
	if len(windows) == 0 {
		v.add(prefix+".windows", "at least one window is required")
	}
	for i, w := range windows {
		p := fmt.Sprintf("%s.windows[%d]", prefix, i)
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

func (v *validator) queueAction(a QueueCompleteAction) {
	const p = "onQueueComplete"
	switch a.Action {
	case QueueActionCommand:
		if a.Command == "" || !filepath.IsAbs(a.Command) {
			v.add(p+".command", "must be the absolute path of an executable")
		}
		if strings.ContainsRune(a.Command, 0) || slices.ContainsFunc(a.Args, func(s string) bool { return strings.ContainsRune(s, 0) }) {
			v.add(p+".args", "must not contain NUL characters")
		}
	case QueueActionNone, QueueActionStop, QueueActionSleep, QueueActionShutdown:
		if a.Command != "" || len(a.Args) > 0 {
			v.add(p+".command", fmt.Sprintf("command and args are only allowed for action %q", QueueActionCommand))
		}
	default:
		v.add(p+".action", "must be one of none, stop, sleep, shutdown, command")
	}
	if a.Delay < 0 || a.Delay.Std() > MaxQueueActionDelay {
		v.add(p+".delay", "must be between 0 and "+MaxQueueActionDelay.String()+" (0 = default "+DefaultQueueActionDelay.String()+")")
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

// postAction validates one post action: a known kind with known parameters.
func (v *validator) postAction(path string, a PostAction) {
	if strings.TrimSpace(a.Kind) == "" {
		v.add(path+".kind", "is required")
		return
	}
	known, ok := postActionParams[a.Kind]
	if !ok {
		v.add(path+".kind", fmt.Sprintf("unknown post action %q", a.Kind))
		return
	}
	for k, val := range a.Params {
		if !slices.Contains(known, k) {
			v.add(path+".params."+k, fmt.Sprintf("is not a parameter of %s", a.Kind))
		}
		if k == "delete" && val != "true" && val != "false" {
			v.add(path+".params.delete", `must be "true" or "false"`)
		}
		if k == "dir" && val == "" {
			v.add(path+".params.dir", "must not be empty")
		}
	}
}
