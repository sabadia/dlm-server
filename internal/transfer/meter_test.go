package transfer

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newMeter(t *testing.T) (*Meter, *clock, *events.Subscription, storage.Store) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "dlm.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bus := events.New(events.Options{})
	t.Cleanup(bus.Close)
	sub := bus.Subscribe(t.Context(), events.Filter{Types: []events.Type{events.TypeSystemSpeed}})
	clk := &clock{now: time.Date(2026, 10, 29, 10, 59, 30, 0, time.UTC)}
	return New(Options{Store: store, Bus: bus, Clock: clk.Now, FlushInterval: time.Minute}), clk, sub, store
}

func next(t *testing.T, sub *events.Subscription) (events.SpeedData, bool) {
	t.Helper()
	select {
	case e := <-sub.C():
		d, ok := e.Data.(events.SpeedData)
		if !ok {
			t.Fatalf("payload %T", e.Data)
		}
		return d, true
	default:
		return events.SpeedData{}, false
	}
}

func TestSpeedIsPublishedWhileMovingAndOnceMoreWhenItStops(t *testing.T) {
	m, clk, sub, _ := newMeter(t)

	clk.Advance(time.Second)
	m.Tick(t.Context())
	if _, ok := next(t, sub); ok {
		t.Fatal("an idle daemon published a speed")
	}

	m.Record("t1", "main", "http", 3000, 0)
	m.Record("t2", "main", "bt", 1000, 500)
	clk.Advance(2 * time.Second) // two seconds since the last measurement
	m.Tick(t.Context())
	d, ok := next(t, sub)
	if !ok || d.Down != 2000 || d.Up != 250 {
		t.Fatalf("speed = %+v, %v; want 2000 down, 250 up (bytes per second over two seconds)", d, ok)
	}

	clk.Advance(time.Second)
	m.Tick(t.Context())
	d, ok = next(t, sub)
	if !ok || d.Down != 0 || d.Up != 0 {
		t.Fatalf("after the transfer stopped: %+v, %v; want one event of zeros", d, ok)
	}
	clk.Advance(time.Second)
	m.Tick(t.Context())
	if _, ok := next(t, sub); ok {
		t.Error("an idle daemon went on publishing")
	}
}

func TestHistoryIsKeptPerHourQueueAndEngine(t *testing.T) {
	m, clk, _, store := newMeter(t)
	m.Record("t1", "main", "http", 100, 0) // 10:59:30
	clk.Advance(time.Minute)               // 11:00:30: the next hour
	m.Record("t1", "main", "http", 10, 0)
	m.Record("t2", "q1", "bt", 5, 7)
	m.Record("t3", "main", "http", 0, 0) // nothing to count
	m.Flush(t.Context())

	pts, err := store.Transfer().Timeline(t.Context(), time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC),
		time.Hour, storage.TransferGroupNone)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 || pts[0].Down != 100 || pts[1].Down != 15 || pts[1].Up != 7 {
		t.Fatalf("points = %+v", pts)
	}
	m.Flush(t.Context()) // nothing pending: nothing doubled
	pts, _ = store.Transfer().Timeline(t.Context(), time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC),
		24*time.Hour, storage.TransferGroupNone)
	if len(pts) != 1 || pts[0].Down != 115 {
		t.Fatalf("day = %+v", pts)
	}
}

func TestTickFlushesWhenDue(t *testing.T) {
	m, clk, _, store := newMeter(t)
	m.Record("t1", "main", "http", 42, 0)
	clk.Advance(30 * time.Second)
	m.Tick(t.Context())
	day := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	if pts, _ := store.Transfer().Timeline(t.Context(), day, day.Add(24*time.Hour), 24*time.Hour, storage.TransferGroupNone); len(pts) != 0 {
		t.Fatalf("flushed too early: %+v", pts)
	}
	clk.Advance(time.Minute)
	m.Tick(t.Context())
	if pts, _ := store.Transfer().Timeline(t.Context(), day, day.Add(24*time.Hour), 24*time.Hour, storage.TransferGroupNone); len(pts) != 1 || pts[0].Down != 42 {
		t.Fatalf("not flushed when due: %+v", pts)
	}
}

func TestRunWritesTheHistoryWhenStopped(t *testing.T) {
	m, _, _, store := newMeter(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	m.Record("t1", "main", "http", 9, 0)
	cancel()
	<-done
	day := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	if pts, _ := store.Transfer().Timeline(t.Context(), day, day.Add(24*time.Hour), 24*time.Hour, storage.TransferGroupNone); len(pts) != 1 || pts[0].Down != 9 {
		t.Fatalf("history after stopping: %+v", pts)
	}
}
