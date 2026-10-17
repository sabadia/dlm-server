package settings

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestWindowsActive(t *testing.T) {
	ny := mustZone(t, "America/New_York")
	tokyo := mustZone(t, "Asia/Tokyo")
	// 2026-10-16 is a Friday.
	at := func(loc *time.Location, day, hour, minute int) time.Time {
		return time.Date(2026, 10, day, hour, minute, 0, 0, loc)
	}
	office := []TimeWindow{{Days: []Weekday{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00"}}
	overnight := []TimeWindow{{Days: []Weekday{5}, Start: "22:00", End: "02:00"}} // Friday night
	untilMidnight := []TimeWindow{{Days: []Weekday{5}, Start: "22:00", End: "00:00"}}
	multi := []TimeWindow{
		{Days: []Weekday{6}, Start: "08:00", End: "10:00"},
		{Days: []Weekday{0}, Start: "20:00", End: "21:30"},
	}
	tests := []struct {
		name    string
		tz      string
		windows []TimeWindow
		t       time.Time
		want    bool
	}{
		{"office start inclusive", "UTC", office, at(time.UTC, 16, 9, 0), true},
		{"office end exclusive", "UTC", office, at(time.UTC, 16, 17, 0), false},
		{"office before", "UTC", office, at(time.UTC, 16, 8, 59), false},
		{"office weekend", "UTC", office, at(time.UTC, 17, 12, 0), false},
		{"overnight evening of start day", "UTC", overnight, at(time.UTC, 16, 23, 0), true},
		{"overnight morning after start day", "UTC", overnight, at(time.UTC, 17, 1, 59), true},
		{"overnight end exclusive", "UTC", overnight, at(time.UTC, 17, 2, 0), false},
		{"overnight morning of start day is not in window", "UTC", overnight, at(time.UTC, 16, 1, 0), false},
		{"overnight evening of next day", "UTC", overnight, at(time.UTC, 17, 23, 0), false},
		{"until midnight 23:59", "UTC", untilMidnight, at(time.UTC, 16, 23, 59), true},
		{"until midnight 00:00 next day", "UTC", untilMidnight, at(time.UTC, 17, 0, 0), false},
		{"multi first window", "UTC", multi, at(time.UTC, 17, 9, 0), true},
		{"multi second window", "UTC", multi, at(time.UTC, 18, 21, 0), true},
		{"multi gap", "UTC", multi, at(time.UTC, 17, 12, 0), false},
		// 14:00 UTC Friday is 10:00 in New York (EDT) and 23:00 in Tokyo.
		{"zone converts instant ny", "America/New_York", office, at(time.UTC, 16, 14, 0), true},
		{"zone converts instant tokyo", "Asia/Tokyo", office, at(time.UTC, 16, 14, 0), false},
		{"zone tokyo overnight", "Asia/Tokyo", overnight, at(tokyo, 17, 0, 30), true},
		// US DST ends 2026-11-01 02:00 EDT -> 01:00 EST; 01:30 happens twice.
		{"dst repeated hour first", "America/New_York", []TimeWindow{{Days: []Weekday{0}, Start: "01:00", End: "02:00"}}, time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), true},
		{"dst repeated hour second", "America/New_York", []TimeWindow{{Days: []Weekday{0}, Start: "01:00", End: "02:00"}}, time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), true},
		{"dst after repeated hour", "America/New_York", []TimeWindow{{Days: []Weekday{0}, Start: "01:00", End: "02:00"}}, time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC), false},
		// US DST starts 2026-03-08: 02:00 EST jumps to 03:00 EDT, so a
		// 02:00-03:00 window never happens that night.
		{"dst skipped hour never active", "America/New_York", []TimeWindow{{Days: []Weekday{0}, Start: "02:00", End: "03:00"}}, time.Date(2026, 3, 8, 2, 30, 0, 0, ny), false},
		{"empty windows", "UTC", nil, at(time.UTC, 16, 12, 0), false},
		{"bad clock never active", "UTC", []TimeWindow{{Days: []Weekday{5}, Start: "9am", End: "17:00"}}, at(time.UTC, 16, 12, 0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WindowsActive(tt.tz, tt.windows, tt.t); got != tt.want {
				t.Errorf("WindowsActive(%s, %v) = %v, want %v", tt.tz, tt.t, got, tt.want)
			}
		})
	}
}

func TestWindowsActiveLocalZone(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.Local) // Friday noon local
	s := Schedule{Windows: []TimeWindow{{Days: []Weekday{5}, Start: "11:00", End: "13:00"}}}
	if !s.ActiveAt(now) {
		t.Error("empty timezone must use the local zone")
	}
}

func TestEffectiveSpeedLimit(t *testing.T) {
	day := SpeedProfile{Timezone: "UTC", SpeedLimit: 1000, Windows: []TimeWindow{{Days: []Weekday{5}, Start: "09:00", End: "17:00"}}}
	lunch := SpeedProfile{Timezone: "UTC", SpeedLimit: 0, Windows: []TimeWindow{{Days: []Weekday{5}, Start: "12:00", End: "13:00"}}}
	profiles := []SpeedProfile{lunch, day}
	at := func(h int) time.Time { return time.Date(2026, 10, 16, h, 0, 0, 0, time.UTC) }
	for _, tt := range []struct {
		hour int
		want int64
	}{{8, 50}, {9, 1000}, {12, 0}, {13, 1000}, {17, 50}} {
		if got := EffectiveSpeedLimit(50, profiles, at(tt.hour)); got != tt.want {
			t.Errorf("at %02d:00 limit = %d, want %d", tt.hour, got, tt.want)
		}
	}
	if got := EffectiveSpeedLimit(7, nil, at(12)); got != 7 {
		t.Errorf("no profiles = %d, want base 7", got)
	}
}

func TestQueueCompleteActionDelay(t *testing.T) {
	if d := (QueueCompleteAction{}).EffectiveDelay(); d != DefaultQueueActionDelay {
		t.Errorf("default delay = %v", d)
	}
	if d := (QueueCompleteAction{Delay: Duration(5 * time.Second)}).EffectiveDelay(); d != 5*time.Second {
		t.Errorf("delay = %v", d)
	}
}

func TestSchedulingSettingsJSON(t *testing.T) {
	in := Settings{
		SpeedProfiles: Some([]SpeedProfile{{Timezone: "UTC", SpeedLimit: 1 << 20, Windows: []TimeWindow{{Days: []Weekday{1}, Start: "09:00", End: "17:00"}}}}),
		Quota:         Some(Quota{Bytes: 500 << 20, Period: Duration(time.Hour)}),
		OnQueueComplete: Some(QueueCompleteAction{
			Action: QueueActionCommand, Command: "/usr/local/bin/notify", Args: []string{"done", "x y"}, Delay: Duration(30 * time.Second),
		}),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"speedProfiles":[{"timezone":"UTC","windows":[{"days":["mon"],"start":"09:00","end":"17:00"}],"speedLimit":1048576}],` +
		`"quota":{"bytes":524288000,"period":"1h0m0s"},` +
		`"onQueueComplete":{"action":"command","command":"/usr/local/bin/notify","args":["done","x y"],"delay":"30s"}}`
	if string(b) != want {
		t.Errorf("json =\n%s\nwant\n%s", b, want)
	}
	var back Settings
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(in.SpeedProfiles.Value(), back.SpeedProfiles.Value()); diff != "" {
		t.Errorf("speedProfiles round trip (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(in.Quota.Value(), back.Quota.Value()); diff != "" {
		t.Errorf("quota round trip (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(in.OnQueueComplete.Value(), back.OnQueueComplete.Value()); diff != "" {
		t.Errorf("onQueueComplete round trip (-want +got):\n%s", diff)
	}
}

func TestValidateSchedulingSettings(t *testing.T) {
	win := []TimeWindow{{Days: []Weekday{1}, Start: "09:00", End: "17:00"}}
	exe, err := filepath.Abs(filepath.Join("bin", "notify"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		level    Level
		s        Settings
		wantPath string // empty means valid
	}{
		{"profiles ok global", LevelGlobal, Settings{SpeedProfiles: Some([]SpeedProfile{{Windows: win, SpeedLimit: 10}})}, ""},
		{"profiles ok queue", LevelQueue, Settings{SpeedProfiles: Some([]SpeedProfile{{Timezone: "Asia/Dhaka", Windows: win}})}, ""},
		{"profiles on task", LevelTask, Settings{SpeedProfiles: Some([]SpeedProfile{{Windows: win}})}, "speedProfiles"},
		{"profile no windows", LevelGlobal, Settings{SpeedProfiles: Some([]SpeedProfile{{SpeedLimit: 1}})}, "speedProfiles[0].windows"},
		{"profile bad window", LevelGlobal, Settings{SpeedProfiles: Some([]SpeedProfile{{Windows: []TimeWindow{{Days: []Weekday{1}, Start: "09:00", End: "24:30"}}}})}, "speedProfiles[0].windows[0].end"},
		{"profile no days", LevelGlobal, Settings{SpeedProfiles: Some([]SpeedProfile{{Windows: []TimeWindow{{Start: "09:00", End: "10:00"}}}})}, "speedProfiles[0].windows[0].days"},
		{"profile bad tz", LevelGlobal, Settings{SpeedProfiles: Some([]SpeedProfile{{Timezone: "Nowhere/City", Windows: win}})}, "speedProfiles[0].timezone"},
		{"profile negative limit", LevelGlobal, Settings{SpeedProfiles: Some([]SpeedProfile{{Windows: win, SpeedLimit: -1}})}, "speedProfiles[0].speedLimit"},
		{"quota ok", LevelGlobal, Settings{Quota: Some(Quota{Bytes: 1, Period: Duration(time.Minute)})}, ""},
		{"quota ok queue", LevelQueue, Settings{Quota: Some(Quota{Bytes: 1 << 30, Period: Duration(24 * time.Hour)})}, ""},
		{"quota on task", LevelTask, Settings{Quota: Some(Quota{Bytes: 1, Period: Duration(time.Hour)})}, "quota"},
		{"quota zero bytes", LevelGlobal, Settings{Quota: Some(Quota{Bytes: 0, Period: Duration(time.Hour)})}, "quota.bytes"},
		{"quota period too short", LevelGlobal, Settings{Quota: Some(Quota{Bytes: 1, Period: Duration(59 * time.Second)})}, "quota.period"},
		{"action stop ok", LevelQueue, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: QueueActionStop})}, ""},
		{"action shutdown ok", LevelQueue, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: QueueActionShutdown, Delay: Duration(time.Hour)})}, ""},
		{"action command ok", LevelQueue, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: QueueActionCommand, Command: exe, Args: []string{"a; rm -rf /"}})}, ""},
		{"action command no args ok", LevelQueue, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: QueueActionCommand, Command: exe})}, ""},
		{"action on global", LevelGlobal, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: QueueActionStop})}, "onQueueComplete"},
		{"action on task", LevelTask, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: QueueActionStop})}, "onQueueComplete"},
		{"unknown action", LevelQueue, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: "reboot"})}, "onQueueComplete.action"},
		{"command without command", LevelQueue, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: QueueActionCommand, Args: []string{"x"}})}, "onQueueComplete.command"},
		{"command relative", LevelQueue, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: QueueActionCommand, Command: "notify-send"})}, "onQueueComplete.command"},
		{"command nul arg", LevelQueue, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: QueueActionCommand, Command: exe, Args: []string{"a\x00b"}})}, "onQueueComplete.args"},
		{"args for sleep", LevelQueue, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: QueueActionSleep, Args: []string{"x"}})}, "onQueueComplete.command"},
		{"negative delay", LevelQueue, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: QueueActionStop, Delay: Duration(-time.Second)})}, "onQueueComplete.delay"},
		{"delay too long", LevelQueue, Settings{OnQueueComplete: Some(QueueCompleteAction{Action: QueueActionStop, Delay: Duration(25 * time.Hour)})}, "onQueueComplete.delay"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.s.Validate(tt.level)
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
