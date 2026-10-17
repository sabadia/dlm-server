package service

import (
	"errors"
	"slices"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/settings"
)

func TestSettingsServiceReplaceGlobal(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	sub := f.bus.Subscribe(ctx, events.Filter{Types: []events.Type{events.TypeSettingsUpdated}})

	g := settings.Settings{SpeedLimit: settings.Some[int64](4096), MaxConcurrentDownloads: settings.Some(2)}
	got, err := f.settings.ReplaceGlobal(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if got.SpeedLimit.Value() != 4096 {
		t.Errorf("returned %+v", got)
	}
	stored, err := f.settings.Global(ctx)
	if err != nil || stored.MaxConcurrentDownloads.Value() != 2 {
		t.Fatalf("Global = %+v, %v", stored, err)
	}
	if l := f.limits.Global().Limit(); l != 4096 {
		t.Errorf("global limit = %d, want 4096", l)
	}
	if f.sched.wakes.Load() != 1 {
		t.Errorf("wakes = %d, want 1", f.sched.wakes.Load())
	}
	evs := collect(sub)
	if len(evs) != 1 {
		t.Fatalf("events = %v", eventTypes(evs))
	}
	if d, ok := evs[0].Data.(settings.Settings); !ok || d.SpeedLimit.Value() != 4096 {
		t.Errorf("event payload = %#v", evs[0].Data)
	}

	// Task-only fields are rejected at the global level.
	_, err = f.settings.ReplaceGlobal(ctx, settings.Settings{Checksum: settings.Some(settings.Checksum{Algorithm: settings.ChecksumMD5, Expected: "00"})})
	wantErr(t, err, domain.ErrInvalidArgument)
	var ve *settings.ValidationError
	if !errors.As(err, &ve) {
		t.Errorf("err %v does not unwrap to *settings.ValidationError", err)
	}
	if len(collect(sub)) != 0 {
		t.Error("rejected change published an event")
	}
}

func TestSettingsServicePatchGlobal(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	if _, err := f.settings.ReplaceGlobal(ctx, settings.Settings{
		SpeedLimit: settings.Some[int64](100),
		Retry:      settings.RetrySettings{MaxAttempts: settings.Some(2), RespectRetryAfter: settings.Some(false)},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := f.settings.PatchGlobal(ctx, []byte(`{"speedLimit":null,"retry":{"maxAttempts":7},"userAgent":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.SpeedLimit.IsSet() || got.Retry.MaxAttempts.Value() != 7 || got.Retry.RespectRetryAfter.Value() || got.UserAgent.Value() != "x" {
		t.Errorf("patched = %+v", got)
	}
	// Unset global speed limit falls back to the default (unlimited).
	if l := f.limits.Global().Limit(); l != 0 {
		t.Errorf("global limit = %d, want 0", l)
	}
	for _, patch := range []string{`{"nope":1}`, `[]`, `{"schedule":{"windows":[]}}`, `{"maxConnections":-1}`} {
		_, err := f.settings.PatchGlobal(ctx, []byte(patch))
		wantErr(t, err, domain.ErrInvalidArgument)
	}
	stored, err := f.settings.Global(ctx)
	if err != nil || stored.Retry.MaxAttempts.Value() != 7 {
		t.Errorf("failed patches changed the stored layer: %+v, %v", stored, err)
	}
}

func TestSettingsServiceEffective(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	if _, err := f.settings.ReplaceGlobal(ctx, settings.Settings{MaxConnections: settings.Some(4), UserAgent: settings.Some("global")}); err != nil {
		t.Fatal(err)
	}
	q, err := f.queues.Create(ctx, CreateQueueInput{Name: "fast", Settings: settings.Settings{MaxConnections: settings.Some(8)}})
	if err != nil {
		t.Fatal(err)
	}
	task := f.create(t, CreateTaskInput{
		Source: domain.Source{CurrentURL: "http://example.com/a"}, QueueID: q.ID,
		Settings: settings.Settings{UserAgent: settings.Some("task")},
	})

	eq, err := f.settings.EffectiveForQueue(ctx, q.ID)
	if err != nil {
		t.Fatal(err)
	}
	if eq.Settings.MaxConnections.Value() != 8 || eq.Sources["maxConnections"] != settings.LevelQueue ||
		eq.Settings.UserAgent.Value() != "global" || eq.Sources["userAgent"] != settings.LevelGlobal ||
		eq.Sources["retry.maxAttempts"] != settings.LevelDefault {
		t.Errorf("queue effective = %+v", eq)
	}
	et, err := f.settings.EffectiveForTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if et.Settings.UserAgent.Value() != "task" || et.Sources["userAgent"] != settings.LevelTask || et.Sources["maxConnections"] != settings.LevelQueue {
		t.Errorf("task effective = %+v", et)
	}
	_, err = f.settings.EffectiveForTask(ctx, "nope")
	wantErr(t, err, domain.ErrNotFound)
	_, err = f.settings.EffectiveForQueue(ctx, "nope")
	wantErr(t, err, domain.ErrNotFound)
	if d := f.settings.Defaults(); d.DownloadDir.Value() != f.dir {
		t.Errorf("Defaults = %+v", d)
	}
}

func TestSettingsServiceSyncLimits(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	q, err := f.queues.Create(ctx, CreateQueueInput{Name: "slow", Settings: settings.Settings{SpeedLimit: settings.Some[int64](2048)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Settings().SaveGlobal(ctx, settings.Settings{SpeedLimit: settings.Some[int64](999)}); err != nil {
		t.Fatal(err)
	}
	f.limits.SetGlobal(0)
	f.limits.RemoveQueue(q.ID)
	if err := f.settings.SyncLimits(ctx); err != nil {
		t.Fatal(err)
	}
	if f.limits.Global().Limit() != 999 || f.limits.Queue(q.ID).Limit() != 2048 {
		t.Errorf("limits global=%d queue=%d", f.limits.Global().Limit(), f.limits.Queue(q.ID).Limit())
	}
	if !slices.Contains(settings.SetPaths(q.Settings), "speedLimit") {
		t.Error("queue settings lost")
	}
}
