package settings

import (
	"slices"
	"time"
)

// Time windows are evaluated on the wall clock of their time zone: a
// window is active at instant t when t, converted to the zone, falls on one
// of the window's days between Start (inclusive) and End (exclusive). When
// End is earlier than Start the window spans midnight and ends on the next
// day; its Days name the day the window starts, so a "fri 22:00–02:00"
// window is active from Friday 22:00 to Saturday 02:00. Because the wall
// clock is used, a daylight-saving jump shortens or lengthens a window by
// the jump (a wall-clock time skipped by the jump is never reached, and a
// repeated one is active both times).

// ActiveAt reports whether t falls inside any of the schedule's windows.
func (s Schedule) ActiveAt(t time.Time) bool {
	return WindowsActive(s.Timezone, s.Windows, t)
}

// ActiveAt reports whether t falls inside any of the profile's windows.
func (p SpeedProfile) ActiveAt(t time.Time) bool {
	return WindowsActive(p.Timezone, p.Windows, t)
}

// WindowsActive reports whether t falls inside any of windows, evaluated in
// timezone (an IANA name; empty or unknown means the local zone). Windows
// with malformed clock times are never active.
func WindowsActive(timezone string, windows []TimeWindow, t time.Time) bool {
	if len(windows) == 0 {
		return false
	}
	local := t.In(zone(timezone))
	return slices.ContainsFunc(windows, func(w TimeWindow) bool { return w.activeAt(local) })
}

// EffectiveSpeedLimit returns the SpeedLimit of the first profile active at
// t, or base when none is.
func EffectiveSpeedLimit(base int64, profiles []SpeedProfile, t time.Time) int64 {
	for _, p := range profiles {
		if p.ActiveAt(t) {
			return p.SpeedLimit
		}
	}
	return base
}

// activeAt reports whether local (already converted to the window's zone)
// falls inside w.
func (w TimeWindow) activeAt(local time.Time) bool {
	start, err := ParseClock(w.Start)
	if err != nil {
		return false
	}
	end, err := ParseClock(w.End)
	if err != nil || start == end {
		return false
	}
	minute := local.Hour()*60 + local.Minute()
	today := Weekday(local.Weekday())
	if start < end {
		return slices.Contains(w.Days, today) && minute >= start && minute < end
	}
	// Spans midnight: the evening part belongs to today's window, the
	// morning part to the window that started yesterday.
	yesterday := Weekday((local.Weekday() + 6) % 7)
	return (slices.Contains(w.Days, today) && minute >= start) ||
		(slices.Contains(w.Days, yesterday) && minute < end)
}

// zone loads an IANA time zone, falling back to the local zone for an
// empty or unknown name (validation rejects unknown names).
func zone(name string) *time.Location {
	if name == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Local
	}
	return loc
}
