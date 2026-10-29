package service

import (
	"errors"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/settings"
)

type fakeQuotas map[string]QuotaUsage

func (f fakeQuotas) Usage(queueID string) (QuotaUsage, bool) {
	u, ok := f[queueID]
	return u, ok
}

func limitsFixture(t *testing.T, quotas QuotaSource) (*LimitsService, *svcFixture, *ratelimit.Manager, *events.Subscription) {
	t.Helper()
	f := newSvcFixture(t)
	mgr := ratelimit.NewManager(0)
	d := f.tasks.d
	d.Limits = mgr
	sub := f.bus.Subscribe(t.Context(), events.Filter{Types: []events.Type{events.TypeLimitsChanged}})
	return NewLimitsService(d, quotas), f, mgr, sub
}

func TestLimitsViewShowsLimitsProfilesAndQuotas(t *testing.T) {
	windowStart := time.Now().Add(-10 * time.Minute)
	quotas := fakeQuotas{
		"":     {Quota: settings.Quota{Bytes: 1000, Period: settings.Duration(time.Hour)}, Used: 400, WindowStart: windowStart},
		"main": {Quota: settings.Quota{Bytes: 50, Period: settings.Duration(time.Minute)}, Used: 50, Exhausted: true},
	}
	svc, f, mgr, _ := limitsFixture(t, quotas)
	allDay := []settings.TimeWindow{{Days: []settings.Weekday{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", End: "23:59"}}
	global := settings.Settings{
		SpeedLimit:    settings.Some(int64(5000)),
		SpeedProfiles: settings.Some([]settings.SpeedProfile{{Windows: allDay, SpeedLimit: 1234}}),
	}
	if err := f.store.Settings().SaveGlobal(t.Context(), global); err != nil {
		t.Fatal(err)
	}
	mgr.SetGlobal(1234)

	v, err := svc.Limits(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if v.Global.SpeedLimit != 1234 || v.Global.ConfiguredSpeedLimit != 1234 || v.Global.ActiveProfile == nil || v.Global.ActiveProfile.Index != 0 {
		t.Errorf("global = %+v", v.Global)
	}
	if q := v.Global.Quota; q == nil || q.Used != 400 || q.ResetsAt == nil || !q.ResetsAt.Equal(windowStart.Add(time.Hour)) || q.Exhausted {
		t.Errorf("global quota = %+v", v.Global.Quota)
	}
	if v.Override != nil {
		t.Errorf("override = %+v", v.Override)
	}
	var main *ScopeLimits
	for i := range v.Queues {
		if v.Queues[i].QueueID == domain.MainQueueID {
			main = &v.Queues[i]
		}
	}
	if main == nil || main.Quota == nil || !main.Quota.Exhausted || main.Quota.WindowStart != nil {
		t.Errorf("main queue = %+v", main)
	}
}

func TestOverrideTurboAndItsEvents(t *testing.T) {
	svc, f, mgr, sub := limitsFixture(t, nil)
	if err := f.store.Settings().SaveGlobal(t.Context(), settings.Settings{SpeedLimit: settings.Some(int64(2000))}); err != nil {
		t.Fatal(err)
	}
	mgr.SetGlobal(2000)

	v, err := svc.SetOverride(t.Context(), 0, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v.Override == nil || v.Override.SpeedLimit != 0 || v.Global.SpeedLimit != 0 || v.Global.ConfiguredSpeedLimit != 2000 {
		t.Fatalf("view during turbo = %+v override %+v", v.Global, v.Override)
	}
	evs := collect(sub)
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	if d, ok := evs[0].Data.(events.LimitsChangedData); !ok || d.SpeedLimit != 0 || d.Override == nil {
		t.Errorf("event = %+v", evs[0].Data)
	}

	v, err = svc.ClearOverride(t.Context())
	if err != nil || v.Override != nil || v.Global.SpeedLimit != 2000 {
		t.Fatalf("after clear: %+v, %v", v, err)
	}
	if evs = collect(sub); len(evs) != 1 {
		t.Errorf("clear published %d events", len(evs))
	}
	if _, err := svc.ClearOverride(t.Context()); err != nil || len(collect(sub)) != 0 {
		t.Errorf("clearing nothing must be quiet: %v", err)
	}
}

func TestOverrideBoundsAndExpiry(t *testing.T) {
	svc, f, mgr, sub := limitsFixture(t, nil)
	for name, tc := range map[string]struct {
		limit int64
		d     time.Duration
		code  string
	}{
		"too short": {100, 10 * time.Second, "limits.bad_duration"},
		"too long":  {100, 25 * time.Hour, "limits.bad_duration"},
		"negative":  {-1, time.Hour, "limits.bad_limit"},
	} {
		_, err := svc.SetOverride(t.Context(), tc.limit, tc.d)
		if !errors.Is(err, domain.ErrInvalidArgument) || code(err) != tc.code {
			t.Errorf("%s: err = %v (code %q)", name, err, code(err))
		}
	}
	if mgr.Global().Limit() != 0 {
		t.Error("a refused override changed the limit")
	}

	if _, err := svc.SetOverride(t.Context(), 777, time.Minute); err != nil {
		t.Fatal(err)
	}
	collect(sub)
	// The scheduler's controller ends an override that ran out and says so.
	ctl := NewLimitsController(Deps{Store: f.store, Limits: mgr, Bus: f.bus, Defaults: func() settings.Settings { return settings.Settings{} }})
	ctl.Apply(settings.Settings{}, nil, time.Now())
	if mgr.Global().Limit() != 777 || len(collect(sub)) != 0 {
		t.Fatal("the override ended before its time")
	}
	ctl.Apply(settings.Settings{}, nil, time.Now().Add(2*time.Minute))
	if mgr.Global().Limit() != 0 {
		t.Errorf("limit after expiry = %d", mgr.Global().Limit())
	}
	evs := collect(sub)
	if len(evs) != 1 {
		t.Fatalf("expiry published %d events", len(evs))
	}
	if d, ok := evs[0].Data.(events.LimitsChangedData); !ok || d.Override != nil {
		t.Errorf("expiry event = %+v", evs[0].Data)
	}
}
