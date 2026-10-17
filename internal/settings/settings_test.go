package settings

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOptionalJSON(t *testing.T) {
	type wrapper struct {
		A Optional[int]    `json:"a,omitzero"`
		B Optional[string] `json:"b,omitzero"`
		C Optional[bool]   `json:"c,omitzero"`
	}
	b, err := json.Marshal(wrapper{A: Some(0), C: Some(false)})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"a":0,"c":false}` {
		t.Errorf("marshal = %s; zero values must be kept, unset omitted", got)
	}

	var w wrapper
	if err := json.Unmarshal([]byte(`{"a":5,"b":null}`), &w); err != nil {
		t.Fatal(err)
	}
	if v, ok := w.A.Get(); !ok || v != 5 {
		t.Errorf("A = %v,%v", v, ok)
	}
	if w.B.IsSet() || w.C.IsSet() {
		t.Error("null and absent must both be unset")
	}
	if err := json.Unmarshal([]byte(`{"a":"x"}`), &w); err == nil {
		t.Error("expected type error")
	}
	if got := w.B.OrElse("def"); got != "def" {
		t.Errorf("OrElse = %q", got)
	}
}

func TestSettingsJSONRoundTrip(t *testing.T) {
	in := Defaults("/downloads")
	in.Schedule = Some(Schedule{Timezone: "UTC", Windows: []TimeWindow{{Days: []Weekday{1, 5}, Start: "22:00", End: "06:00"}}})
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"retry":{"maxAttempts":5,"initialBackoff":"1s"`, `"days":["mon","fri"]`, `"proxy":{"mode":"system"}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON %s missing %s", b, want)
		}
	}
	var out Settings
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := SetPaths(out), SetPaths(in); !slices.Equal(got, want) {
		t.Errorf("round trip paths = %v, want %v", got, want)
	}
	if b, _ := json.Marshal(Settings{}); string(b) != "{}" {
		t.Errorf("empty settings marshal to %s, want {}", b)
	}
}

func TestResolvePrecedenceAndProvenance(t *testing.T) {
	defaults := Defaults("/dl")
	global := Settings{MaxConnections: Some(8), SpeedLimit: Some[int64](1000)}
	global.Retry.MaxAttempts = Some(9)
	queue := Settings{MaxConnections: Some(4), DownloadDir: Some("/queue")}
	task := Settings{MaxConnections: Some(2)}
	task.Retry.InitialBackoff = Some(Duration(5 * time.Second))

	eff := Resolve(Layers{Defaults: defaults, Global: global, Queue: queue, Task: task})

	checks := []struct {
		path  string
		level Level
		got   any
		want  any
	}{
		{"maxConnections", LevelTask, eff.Settings.MaxConnections.Value(), 2},
		{"downloadDir", LevelQueue, eff.Settings.DownloadDir.Value(), "/queue"},
		{"speedLimit", LevelGlobal, eff.Settings.SpeedLimit.Value(), int64(1000)},
		{"retry.maxAttempts", LevelGlobal, eff.Settings.Retry.MaxAttempts.Value(), 9},
		{"retry.initialBackoff", LevelTask, eff.Settings.Retry.InitialBackoff.Value(), Duration(5 * time.Second)},
		{"retry.maxBackoff", LevelDefault, eff.Settings.Retry.MaxBackoff.Value(), Duration(time.Minute)},
		{"userAgent", LevelDefault, eff.Settings.UserAgent.Value(), DefaultUserAgent},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.path, c.got, c.want)
		}
		if eff.Sources[c.path] != c.level {
			t.Errorf("source(%s) = %v, want %v", c.path, eff.Sources[c.path], c.level)
		}
	}
	if eff.Settings.Checksum.IsSet() {
		t.Error("checksum should stay unset")
	}
	if _, ok := eff.Sources["checksum"]; ok {
		t.Error("unset field must not have a source")
	}
}

func TestMergeAndSetPaths(t *testing.T) {
	base := Settings{MaxConnections: Some(4), UserAgent: Some("a")}
	over := Settings{UserAgent: Some("b"), TLSVerify: Some(false)}
	got := Merge(base, over)
	if got.MaxConnections.Value() != 4 || got.UserAgent.Value() != "b" || got.TLSVerify.Value() {
		t.Errorf("Merge = %+v", got)
	}
	if paths := SetPaths(got); !slices.Equal(paths, []string{"maxConnections", "tlsVerify", "userAgent"}) {
		t.Errorf("SetPaths = %v", paths)
	}
}

func TestDefaultsAreValid(t *testing.T) {
	d := Defaults(t.TempDir()) // OS-native absolute path
	if err := d.Validate(LevelDefault); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
}

func TestValidateLevels(t *testing.T) {
	tests := []struct {
		name  string
		s     Settings
		level Level
		ok    bool
	}{
		{"concurrency at task rejected", Settings{MaxConcurrentDownloads: Some(2)}, LevelTask, false},
		{"concurrency at queue ok", Settings{MaxConcurrentDownloads: Some(2)}, LevelQueue, true},
		{"total cap at queue rejected", Settings{MaxActiveDownloadsTotal: Some(2)}, LevelQueue, false},
		{"total cap at global ok", Settings{MaxActiveDownloadsTotal: Some(2)}, LevelGlobal, true},
		{"checksum at global rejected", Settings{Checksum: Some(Checksum{ChecksumMD5, strings.Repeat("a", 32)})}, LevelGlobal, false},
		{"checksum at task ok", Settings{Checksum: Some(Checksum{ChecksumMD5, strings.Repeat("a", 32)})}, LevelTask, true},
		{"schedule at task rejected", Settings{Schedule: Some(Schedule{Windows: []TimeWindow{{Days: []Weekday{0}, Start: "01:00", End: "02:00"}}})}, LevelTask, false},
		{"empty ok everywhere", Settings{}, LevelTask, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.s.Validate(tt.level)
			if (err == nil) != tt.ok {
				t.Fatalf("Validate(%v) = %v, want ok=%v", tt.level, err, tt.ok)
			}
		})
	}
}

func TestValidateValues(t *testing.T) {
	window := func(start, end string, days ...Weekday) Schedule {
		return Schedule{Windows: []TimeWindow{{Days: days, Start: start, End: end}}}
	}
	tests := []struct {
		name     string
		s        Settings
		wantPath string // empty means valid
	}{
		{"relative dir", Settings{DownloadDir: Some("rel/dir")}, "downloadDir"},
		{"template with slash", Settings{FilenameTemplate: Some("a/b")}, "filenameTemplate"},
		{"connections zero", Settings{MaxConnections: Some(0)}, "maxConnections"},
		{"connections too many", Settings{MaxConnections: Some(MaxConnectionsLimit + 1)}, "maxConnections"},
		{"negative speed", Settings{SpeedLimit: Some[int64](-1)}, "speedLimit"},
		{"backoff order", Settings{Retry: RetrySettings{InitialBackoff: Some(Duration(time.Minute)), MaxBackoff: Some(Duration(time.Second))}}, "retry.maxBackoff"},
		{"zero backoff", Settings{Retry: RetrySettings{InitialBackoff: Some(Duration(0))}}, "retry.initialBackoff"},
		{"custom proxy no url", Settings{Proxy: Some(Proxy{Mode: ProxyCustom})}, "proxy.url"},
		{"custom proxy bad scheme", Settings{Proxy: Some(Proxy{Mode: ProxyCustom, URL: "ftp://h:1"})}, "proxy.url"},
		{"custom proxy ok", Settings{Proxy: Some(Proxy{Mode: ProxyCustom, URL: "socks5h://user:pw@127.0.0.1:1080"})}, ""},
		{"none proxy with url", Settings{Proxy: Some(Proxy{Mode: ProxyNone, URL: "http://h"})}, "proxy.url"},
		{"unknown proxy mode", Settings{Proxy: Some(Proxy{Mode: "pac"})}, "proxy.mode"},
		{"ua newline", Settings{UserAgent: Some("a\r\nX: y")}, "userAgent"},
		{"bad header name", Settings{Headers: Some(map[string]string{"Bad Name": "v"})}, "headers"},
		{"header injection", Settings{Headers: Some(map[string]string{"X-A": "v\nX-B: w"})}, "headers"},
		{"headers ok", Settings{Headers: Some(map[string]string{"X-Token": "abc"})}, ""},
		{"bad file policy", Settings{FileExistsPolicy: Some(FileExistsPolicy("merge"))}, "fileExistsPolicy"},
		{"bad checksum algo", Settings{Checksum: Some(Checksum{Algorithm: "crc32", Expected: "00"})}, "checksum.algorithm"},
		{"bad checksum length", Settings{Checksum: Some(Checksum{Algorithm: ChecksumSHA256, Expected: "abcd"})}, "checksum.expected"},
		{"unknown strategy", Settings{Refresh: RefreshSettings{Strategies: Some([]RefreshStrategy{"magic"})}}, "refresh.strategies"},
		{"duplicate strategy", Settings{Refresh: RefreshSettings{Strategies: Some([]RefreshStrategy{RefreshManual, RefreshManual})}}, "refresh.strategies"},
		{"bad mismatch", Settings{Refresh: RefreshSettings{OnMismatch: Some(MismatchPolicy("x"))}}, "refresh.onMismatch"},
		{"bad browser usage", Settings{BrowserUsage: Some(BrowserUsage("sometimes"))}, "browserUsage"},
		{"post action without kind", Settings{PostActions: Some([]PostAction{{}})}, "postActions[0].kind"},
		{"schedule ok overnight", Settings{Schedule: Some(window("22:00", "06:00", 1, 2))}, ""},
		{"schedule bad clock", Settings{Schedule: Some(window("25:00", "06:00", 1))}, "schedule.windows[0].start"},
		{"schedule no days", Settings{Schedule: Some(window("01:00", "02:00"))}, "schedule.windows[0].days"},
		{"schedule equal times", Settings{Schedule: Some(window("01:00", "01:00", 1))}, "schedule.windows[0]"},
		{"schedule bad tz", Settings{Schedule: Some(Schedule{Timezone: "Mars/Base", Windows: window("01:00", "02:00", 1).Windows})}, "schedule.timezone"},
		{"schedule tz ok", Settings{Schedule: Some(Schedule{Timezone: "Asia/Dhaka", Windows: window("01:00", "02:00", 1).Windows})}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			level := LevelGlobal
			if tt.s.Checksum.IsSet() {
				level = LevelTask
			}
			if tt.s.Schedule.IsSet() {
				level = LevelQueue
			}
			err := tt.s.Validate(level)
			if tt.wantPath == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want *ValidationError, got %v", err)
			}
			if !slices.ContainsFunc(ve.Fields, func(f *FieldError) bool { return f.Path == tt.wantPath }) {
				t.Fatalf("errors %v do not include path %q", err, tt.wantPath)
			}
		})
	}
}

func TestLevelText(t *testing.T) {
	for _, l := range []Level{LevelDefault, LevelGlobal, LevelQueue, LevelTask} {
		b, err := l.MarshalText()
		if err != nil {
			t.Fatal(err)
		}
		var back Level
		if err := back.UnmarshalText(b); err != nil || back != l {
			t.Errorf("round trip %v -> %s -> %v (%v)", l, b, back, err)
		}
	}
	var l Level
	if err := l.UnmarshalText([]byte("system")); err == nil {
		t.Error("expected error for unknown level")
	}
}

func TestEveryLeafFieldIsCollected(t *testing.T) {
	// Guard: adding a non-Optional leaf field panics in collectFields; make
	// sure field collection works and covers nested groups.
	paths := make([]string, 0, len(fields()))
	for _, f := range fields() {
		paths = append(paths, f.path)
	}
	for _, want := range []string{"downloadDir", "retry.respectRetryAfter", "refresh.awaitTimeout", "schedule"} {
		if !slices.Contains(paths, want) {
			t.Errorf("field %q not collected (got %v)", want, paths)
		}
	}
}
