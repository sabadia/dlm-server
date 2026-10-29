package logging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/config"
)

func ids(recs []Record) string {
	parts := make([]string, len(recs))
	for i, r := range recs {
		parts[i] = fmt.Sprint(r.ID)
	}
	return strings.Join(parts, ",")
}

func fill(r *Ring, n int) {
	for i := 1; i <= n; i++ {
		lvl := "info"
		if i%3 == 0 {
			lvl = "error"
		}
		r.Add(Record{Time: time.Unix(int64(1000+i), 0), Level: lvl, Message: fmt.Sprintf("message %d", i)})
	}
}

func TestRingKeepsTheNewestAndPages(t *testing.T) {
	r := NewRing(5)
	fill(r, 8) // records 4..8 are kept
	got, more := r.Query(Filter{}, 0, 100)
	if ids(got) != "4,5,6,7,8" || more {
		t.Errorf("all = %s more %v", ids(got), more)
	}
	got, more = r.Query(Filter{}, 0, 2)
	if ids(got) != "7,8" || !more {
		t.Errorf("newest two = %s more %v", ids(got), more)
	}
	got, more = r.Query(Filter{}, 7, 2) // older than 7
	if ids(got) != "5,6" || !more {
		t.Errorf("before 7 = %s more %v", ids(got), more)
	}
	got, more = r.Query(Filter{}, 5, 10)
	if ids(got) != "4" || more {
		t.Errorf("before 5 = %s more %v", ids(got), more)
	}
	if r.LastID() != 8 {
		t.Errorf("last id = %d", r.LastID())
	}
}

func TestFilters(t *testing.T) {
	r := NewRing(50)
	fill(r, 9)
	r.Add(Record{Time: time.Unix(2000, 0), Level: "warn", Message: "disk", Attrs: map[string]any{"path": "/Data/Downloads"}})

	got, _ := r.Query(Filter{MinLevel: levelPtr(slog.LevelError)}, 0, 100)
	if ids(got) != "3,6,9" {
		t.Errorf("errors = %s", ids(got))
	}
	got, _ = r.Query(Filter{MinLevel: levelPtr(slog.LevelWarn)}, 0, 100)
	if ids(got) != "3,6,9,10" {
		t.Errorf("warnings and up = %s", ids(got))
	}
	got, _ = r.Query(Filter{Query: "MESSAGE 4"}, 0, 100)
	if ids(got) != "4" {
		t.Errorf("message search = %s", ids(got))
	}
	got, _ = r.Query(Filter{Query: "downloads"}, 0, 100)
	if ids(got) != "10" {
		t.Errorf("attribute search = %s", ids(got))
	}
	got, _ = r.Query(Filter{Since: time.Unix(1003, 0), Until: time.Unix(1005, 0)}, 0, 100)
	if ids(got) != "3,4,5" {
		t.Errorf("time range = %s", ids(got))
	}
}

func TestSinceAndItsGaps(t *testing.T) {
	r := NewRing(5)
	if recs, complete := r.Since(0, Filter{}); len(recs) != 0 || !complete {
		t.Errorf("empty ring: %v %v", recs, complete)
	}
	fill(r, 8) // 4..8 kept
	recs, complete := r.Since(5, Filter{})
	if ids(recs) != "6,7,8" || !complete {
		t.Errorf("since 5 = %s complete %v", ids(recs), complete)
	}
	recs, complete = r.Since(3, Filter{})
	if ids(recs) != "4,5,6,7,8" || !complete {
		t.Errorf("since 3 (the oldest kept follows) = %s complete %v", ids(recs), complete)
	}
	recs, complete = r.Since(1, Filter{})
	if ids(recs) != "4,5,6,7,8" || complete {
		t.Errorf("since 1 = %s complete %v: records 2 and 3 are gone", ids(recs), complete)
	}
	if recs, complete = r.Since(8, Filter{}); len(recs) != 0 || !complete {
		t.Errorf("since the newest = %v %v", recs, complete)
	}
}

func TestSubscribersGetNewMatchingRecordsAndLoseWhenSlow(t *testing.T) {
	r := NewRing(10)
	ctx, cancel := context.WithCancel(t.Context())
	sub := r.Subscribe(ctx, Filter{MinLevel: levelPtr(slog.LevelError)})
	r.Add(Record{Level: "info", Message: "quiet"})
	r.Add(Record{Level: "error", Message: "loud"})
	select {
	case rec := <-sub.C():
		if rec.Message != "loud" || rec.ID != 2 {
			t.Errorf("record = %+v", rec)
		}
	case <-time.After(time.Second):
		t.Fatal("no record delivered")
	}
	select {
	case rec := <-sub.C():
		t.Errorf("an unmatched record was delivered: %+v", rec)
	default:
	}
	for range 300 { // more than the buffer holds
		r.Add(Record{Level: "error", Message: "flood"})
	}
	if sub.Dropped() == 0 {
		t.Error("a slow subscriber lost nothing")
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		n := len(r.subs)
		r.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the subscription was not removed after its context ended")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTeeHandlerKeepsWhatTheInnerHandlerAccepts(t *testing.T) {
	r := NewRing(10)
	var out strings.Builder
	h := NewTeeHandler(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}), r)
	l := slog.New(h).With("component", "scheduler").WithGroup("task")

	l.Debug("below the level")
	l.Info("started", "id", "t1", "attempt", 2, slog.Group("src", "url", "http://x"), "err", errors.New("boom"), "took", 1500*time.Millisecond)

	recs, _ := r.Query(Filter{}, 0, 10)
	if len(recs) != 1 {
		t.Fatalf("records = %+v", recs)
	}
	got := recs[0]
	if got.Message != "started" || got.Level != "info" || got.Time.IsZero() {
		t.Errorf("record = %+v", got)
	}
	want := map[string]any{
		"component": "scheduler", "task.id": "t1", "task.attempt": int64(2), "task.src.url": "http://x",
		"task.err": "boom", "task.took": "1.5s",
	}
	for k, v := range want {
		if got.Attrs[k] != v {
			t.Errorf("attr %s = %v (%T), want %v", k, got.Attrs[k], got.Attrs[k], v)
		}
	}
	if !strings.Contains(out.String(), "started") || strings.Contains(out.String(), "below the level") {
		t.Errorf("the inner handler saw %q", out.String())
	}
}

func TestLoggerNewKeepsRecentRecords(t *testing.T) {
	cfg := configDefaultLog()
	cfg.File = false
	l, err := New(cfg, t.TempDir(), &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello", "k", "v")
	l.Debug("hidden") // below the configured level
	recs, _ := l.Ring.Query(Filter{}, 0, 10)
	if len(recs) != 1 || recs[0].Message != "hello" || recs[0].Attrs["k"] != "v" {
		t.Errorf("records = %+v", recs)
	}
	l.Level.Set(slog.LevelDebug)
	l.Debug("now visible")
	if recs, _ = l.Ring.Query(Filter{}, 0, 10); len(recs) != 2 {
		t.Errorf("after lowering the level: %+v", recs)
	}
}

func configDefaultLog() config.LogConfig { return config.Default().Log }

func levelPtr(l slog.Level) *slog.Level { return &l }
