package settings

import (
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
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
		{"negative seed ratio", Settings{Torrent: TorrentSettings{SeedRatio: Some(-0.5)}}, "torrent.seedRatio"},
		{"huge seed ratio", Settings{Torrent: TorrentSettings{SeedRatio: Some(float64(MaxSeedRatio) + 1)}}, "torrent.seedRatio"},
		{"nan seed ratio", Settings{Torrent: TorrentSettings{SeedRatio: Some(math.NaN())}}, "torrent.seedRatio"},
		{"negative seed time", Settings{Torrent: TorrentSettings{SeedTime: Some(Duration(-time.Second))}}, "torrent.seedTime"},
		{"bad tracker scheme", Settings{Torrent: TorrentSettings{Trackers: Some([]string{"ftp://t.example/announce"})}}, "torrent.trackers"},
		{"tracker without host", Settings{Torrent: TorrentSettings{Trackers: Some([]string{"udp://"})}}, "torrent.trackers"},
		{"torrent settings ok", Settings{Torrent: TorrentSettings{SeedKeep: Some(true), SeedRatio: Some(2.5), SeedTime: Some(Duration(0)), Trackers: Some([]string{"udp://t.example:6969/announce", "https://t.example/announce", "wss://t.example"})}}, ""},
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

func TestProxyRules(t *testing.T) {
	video := Proxy{Mode: ProxyCustom, URL: "socks5h://127.0.0.1:1080"}
	global := Settings{
		Proxy: Some(Proxy{Mode: ProxySystem}),
		ProxyRules: Some([]ProxyRule{
			{Hosts: []string{"*.corp.local", "10.*"}, Proxy: Proxy{Mode: ProxyNone}},
			{Hosts: []string{"*.Video-Site.com"}, Proxy: video},
		}),
	}
	if err := global.Validate(LevelGlobal); err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}
	cases := map[string]ProxyMode{
		"a.corp.local":       ProxyNone,
		"a.b.corp.local":     ProxyNone, // * spans dots
		"10.1.2.3":           ProxyNone,
		"cdn.video-site.com": ProxyCustom, // case-insensitive
		"example.com":        ProxySystem,
	}
	for host, want := range cases {
		if got := global.ProxyFor(host).Mode; got != want {
			t.Errorf("ProxyFor(%s) = %s, want %s", host, got, want)
		}
	}

	// An explicit queue or task proxy disables the global rules.
	for _, layers := range []Layers{
		{Global: global, Queue: Settings{Proxy: Some(Proxy{Mode: ProxyNone})}},
		{Global: global, Task: Settings{Proxy: Some(Proxy{Mode: ProxyNone})}},
	} {
		eff := Resolve(layers)
		if eff.Settings.ProxyRules.IsSet() {
			t.Error("rules must not apply when queue/task sets proxy")
		}
		if _, ok := eff.Sources["proxyRules"]; ok {
			t.Error("proxyRules source must be removed")
		}
	}
	if eff := Resolve(Layers{Global: global}); !eff.Settings.ProxyRules.IsSet() {
		t.Error("rules must apply when only global sets proxy")
	}

	bad := []Settings{
		{ProxyRules: Some([]ProxyRule{{Proxy: Proxy{Mode: ProxyNone}}})},                             // no hosts
		{ProxyRules: Some([]ProxyRule{{Hosts: []string{"[bad"}, Proxy: Proxy{Mode: ProxyNone}}})},    // malformed glob
		{ProxyRules: Some([]ProxyRule{{Hosts: []string{"a.com/x"}, Proxy: Proxy{Mode: ProxyNone}}})}, // path char
		{ProxyRules: Some([]ProxyRule{{Hosts: []string{"a.com"}, Proxy: Proxy{Mode: ProxyCustom}}})}, // custom w/o url
	}
	for i, s := range bad {
		var ve *ValidationError
		if err := s.Validate(LevelGlobal); !errors.As(err, &ve) || !strings.HasPrefix(ve.Fields[0].Path, "proxyRules[0]") {
			t.Errorf("bad[%d]: err = %v", i, err)
		}
	}
	if err := global.Validate(LevelQueue); err == nil {
		t.Error("proxyRules must be global-only")
	}
}

func TestTorrentSettingsLayering(t *testing.T) {
	d := Defaults("/dl")
	if d.Torrent.SeedKeep.Value() || d.Torrent.SeedRatio.Value() != 1.0 || d.Torrent.SeedTime.Value().Std() != 2*time.Hour {
		t.Errorf("default torrent settings = %+v", d.Torrent)
	}
	task := Settings{Torrent: TorrentSettings{SeedKeep: Some(true)}}
	queue := Settings{Torrent: TorrentSettings{SeedRatio: Some(3.0)}}
	eff := Resolve(Layers{Defaults: d, Queue: queue, Task: task})
	if !eff.Settings.Torrent.SeedKeep.Value() || eff.Settings.Torrent.SeedRatio.Value() != 3.0 || eff.Settings.Torrent.SeedTime.Value().Std() != 2*time.Hour {
		t.Errorf("effective = %+v", eff.Settings.Torrent)
	}
	for path, want := range map[string]Level{"torrent.seedKeep": LevelTask, "torrent.seedRatio": LevelQueue, "torrent.seedTime": LevelDefault} {
		if got := eff.Sources[path]; got != want {
			t.Errorf("source of %s = %v, want %v", path, got, want)
		}
	}
	// A merge patch reaches into the group.
	got, err := MergePatch(Settings{}, []byte(`{"torrent":{"seedRatio":2,"seedTime":"30m","trackers":["udp://t.example/a"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Torrent.SeedRatio.Value() != 2 || got.Torrent.SeedTime.Value().Std() != 30*time.Minute || len(got.Torrent.Trackers.Value()) != 1 {
		t.Errorf("patched = %+v", got.Torrent)
	}
	got, err = MergePatch(got, []byte(`{"torrent":{"seedRatio":null}}`))
	if err != nil || got.Torrent.SeedRatio.IsSet() || !got.Torrent.SeedTime.IsSet() {
		t.Errorf("after unsetting seedRatio: %+v, %v", got.Torrent, err)
	}
}

func TestPostActionValidation(t *testing.T) {
	prog := filepath.Join(t.TempDir(), "scanner")
	for name, c := range map[string]struct {
		action PostAction
		want   string // the path of the expected error; empty means valid
	}{
		"extract":               {PostAction{Kind: PostActionExtract}, ""},
		"extract with params":   {PostAction{Kind: PostActionExtract, Params: map[string]string{"dir": ".", "delete": "true", "password": "pw"}}, ""},
		"add-torrent":           {PostAction{Kind: PostActionAddTorrent, Params: map[string]string{"dir": "/x", "delete": "false"}}, ""},
		"no kind":               {PostAction{}, "postActions[0].kind"},
		"unknown kind":          {PostAction{Kind: "format-disk"}, "postActions[0].kind"},
		"unknown parameter":     {PostAction{Kind: PostActionExtract, Params: map[string]string{"cmd": "rm"}}, "postActions[0].params.cmd"},
		"parameter of another":  {PostAction{Kind: PostActionAddTorrent, Params: map[string]string{"password": "x"}}, "postActions[0].params.password"},
		"delete is a boolean":   {PostAction{Kind: PostActionExtract, Params: map[string]string{"delete": "yes"}}, "postActions[0].params.delete"},
		"dir must not be empty": {PostAction{Kind: PostActionExtract, Params: map[string]string{"dir": ""}}, "postActions[0].params.dir"},

		"command":                     {PostAction{Kind: PostActionCommand, Params: map[string]string{"cmd": prog, "args": `["{path}","-x"]`, "timeout": "30s"}}, ""},
		"command needs cmd":           {PostAction{Kind: PostActionCommand}, "postActions[0].params.cmd"},
		"command cmd is absolute":     {PostAction{Kind: PostActionCommand, Params: map[string]string{"cmd": "rm"}}, "postActions[0].params.cmd"},
		"command args are JSON":       {PostAction{Kind: PostActionCommand, Params: map[string]string{"cmd": prog, "args": "-x {path}"}}, "postActions[0].params.args"},
		"command args are strings":    {PostAction{Kind: PostActionCommand, Params: map[string]string{"cmd": prog, "args": `[1]`}}, "postActions[0].params.args"},
		"command timeout is valid":    {PostAction{Kind: PostActionCommand, Params: map[string]string{"cmd": prog, "timeout": "-5s"}}, "postActions[0].params.timeout"},
		"command has no infectedExit": {PostAction{Kind: PostActionCommand, Params: map[string]string{"cmd": prog, "infectedExit": "1"}}, "postActions[0].params.infectedExit"},
		"av-scan":                     {PostAction{Kind: PostActionAVScan, Params: map[string]string{"cmd": prog, "infectedExit": "2", "onInfected": "keep"}}, ""},
		"av-scan infectedExit range":  {PostAction{Kind: PostActionAVScan, Params: map[string]string{"cmd": prog, "infectedExit": "0"}}, "postActions[0].params.infectedExit"},
		"av-scan onInfected":          {PostAction{Kind: PostActionAVScan, Params: map[string]string{"cmd": prog, "onInfected": "move"}}, "postActions[0].params.onInfected"},
	} {
		s := Settings{PostActions: Some([]PostAction{c.action})}
		err := s.Validate(LevelGlobal)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", name, err)
			}
			continue
		}
		var ve *ValidationError
		if !errors.As(err, &ve) || !slices.ContainsFunc(ve.Fields, func(f *FieldError) bool { return f.Path == c.want }) {
			t.Errorf("%s: err = %v, want a field error at %s", name, err, c.want)
		}
	}
}
