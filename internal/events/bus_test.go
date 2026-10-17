package events

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("X", 3600))

func newTestBus(opts Options) *Bus {
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return t0 }
	}
	return New(opts)
}

// drain returns all events currently queued on s without blocking.
func drain(s *Subscription) []Event {
	var out []Event
	for {
		select {
		case e, ok := <-s.C():
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

func ids(evs []Event) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

func equalIDs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// waitClosed waits (bounded) for s's channel to be closed, discarding events.
func waitClosed(t *testing.T, s *Subscription) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-s.C():
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("subscription channel not closed")
		}
	}
}

func TestFilterMatch(t *testing.T) {
	e := Event{Type: TypeTaskStatus, TaskID: "t1", QueueID: "q1"}
	tests := []struct {
		name string
		f    Filter
		want bool
	}{
		{"zero matches all", Filter{}, true},
		{"type match", Filter{Types: []Type{TypeTaskCreated, TypeTaskStatus}}, true},
		{"type mismatch", Filter{Types: []Type{TypeTaskCreated}}, false},
		{"task match", Filter{TaskID: "t1"}, true},
		{"task mismatch", Filter{TaskID: "t2"}, false},
		{"queue match", Filter{QueueID: "q1"}, true},
		{"queue mismatch", Filter{QueueID: "q2"}, false},
		{"all match", Filter{Types: []Type{TypeTaskStatus}, TaskID: "t1", QueueID: "q1"}, true},
		{"one field mismatch", Filter{Types: []Type{TypeTaskStatus}, TaskID: "t1", QueueID: "q2"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.Match(e); got != tt.want {
				t.Errorf("Match = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEventJSON(t *testing.T) {
	tests := []struct {
		name string
		e    Event
		want string
	}{
		{
			"all fields",
			Event{ID: 7, Type: TypeTaskStatus, Time: t0.UTC(), TaskID: "t", QueueID: "q", Data: map[string]int{"n": 1}},
			`{"id":7,"type":"task.status","time":"2026-01-02T02:04:05Z","taskId":"t","queueId":"q","data":{"n":1}}`,
		},
		{
			"empty ids omitted",
			Event{ID: 1, Type: TypeSettingsUpdated, Time: t0.UTC()},
			`{"id":1,"type":"settings.updated","time":"2026-01-02T02:04:05Z"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.e)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tt.want {
				t.Errorf("got %s\nwant %s", b, tt.want)
			}
		})
	}
}

func TestPublishAssignsIDAndUTCTime(t *testing.T) {
	b := newTestBus(Options{})
	defer b.Close()
	var prev uint64
	for i := range 5 {
		got := b.Publish(Event{ID: 999, Type: TypeTaskCreated, TaskID: "t", Time: time.Unix(1, 0)})
		if got.ID != prev+1 {
			t.Fatalf("publish %d: ID = %d, want %d", i, got.ID, prev+1)
		}
		prev = got.ID
		if !got.Time.Equal(t0) || got.Time.Location() != time.UTC {
			t.Fatalf("Time = %v, want %v in UTC", got.Time, t0)
		}
	}
}

func TestFanOutAndFilters(t *testing.T) {
	b := newTestBus(Options{})
	defer b.Close()
	ctx := context.Background()

	all := b.Subscribe(ctx, Filter{})
	all2 := b.Subscribe(ctx, Filter{})
	byType := b.Subscribe(ctx, Filter{Types: []Type{TypeQueueCreated}})
	byTask := b.Subscribe(ctx, Filter{TaskID: "t1"})
	byQueue := b.Subscribe(ctx, Filter{QueueID: "q2"})

	b.Publish(Event{Type: TypeTaskCreated, TaskID: "t1", QueueID: "q1"}) // 1
	b.Publish(Event{Type: TypeQueueCreated, QueueID: "q2"})              // 2
	b.Publish(Event{Type: TypeTaskCreated, TaskID: "t2", QueueID: "q2"}) // 3
	b.Publish(Event{Type: TypeSettingsUpdated})                          // 4

	tests := []struct {
		name string
		sub  *Subscription
		want []uint64
	}{
		{"all", all, []uint64{1, 2, 3, 4}},
		{"all2", all2, []uint64{1, 2, 3, 4}},
		{"type", byType, []uint64{2}},
		{"task", byTask, []uint64{1}},
		{"queue", byQueue, []uint64{2, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ids(drain(tt.sub)); !equalIDs(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPublishNeverBlocksDropsOldest(t *testing.T) {
	b := newTestBus(Options{SubscriberBuffer: 3})
	defer b.Close()
	slow := b.Subscribe(context.Background(), Filter{})
	other := b.Subscribe(context.Background(), Filter{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 10 {
			b.Publish(Event{Type: TypeTaskUpdated, TaskID: "t"})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}

	if got, want := ids(drain(slow)), []uint64{8, 9, 10}; !equalIDs(got, want) {
		t.Errorf("slow got %v, want newest %v", got, want)
	}
	if got := slow.Dropped(); got != 7 {
		t.Errorf("Dropped = %d, want 7", got)
	}
	if got := other.Dropped(); got != 7 {
		t.Errorf("other Dropped = %d, want 7 (independent counter)", got)
	}
}

func TestProgressCoalescing(t *testing.T) {
	b := newTestBus(Options{})
	defer b.Close()
	s := b.Subscribe(context.Background(), Filter{})

	for i := range 5 {
		if got := b.Publish(Event{Type: TypeTaskProgress, TaskID: "a", Data: i}); got.ID != 0 {
			t.Fatalf("coalesced Publish returned ID %d, want 0", got.ID)
		}
	}
	b.Publish(Event{Type: TypeTaskProgress, TaskID: "b", Data: 100})
	b.Publish(Event{Type: TypeTaskProgress, TaskID: "a", Data: 5})

	if got := drain(s); len(got) != 0 {
		t.Fatalf("progress delivered before flush: %v", got)
	}
	if got, _ := b.Since(0, Filter{}); len(got) != 0 {
		t.Fatalf("pending progress in replay buffer: %v", got)
	}

	b.flush()
	got := drain(s)
	if len(got) != 2 {
		t.Fatalf("got %d events after flush, want 2: %v", len(got), got)
	}
	want := []struct {
		task string
		data int
	}{{"a", 5}, {"b", 100}}
	for i, w := range want {
		if got[i].TaskID != w.task || got[i].Data != w.data || got[i].Type != TypeTaskProgress {
			t.Errorf("event %d = %+v, want task %s data %d", i, got[i], w.task, w.data)
		}
		if got[i].ID != uint64(i+1) {
			t.Errorf("event %d ID = %d, want %d", i, got[i].ID, i+1)
		}
	}

	b.flush()
	if got := drain(s); len(got) != 0 {
		t.Errorf("second flush delivered %v, want nothing", got)
	}
}

func TestStatusFlushesPendingProgressFirst(t *testing.T) {
	b := newTestBus(Options{})
	defer b.Close()
	s := b.Subscribe(context.Background(), Filter{})

	b.Publish(Event{Type: TypeTaskProgress, TaskID: "a", Data: 1})
	b.Publish(Event{Type: TypeTaskProgress, TaskID: "a", Data: 2})
	b.Publish(Event{Type: TypeTaskProgress, TaskID: "b", Data: 9})
	st := b.Publish(Event{Type: TypeTaskStatus, TaskID: "a", Data: "completed"})

	got := drain(s)
	if len(got) != 2 {
		t.Fatalf("got %v, want progress(a) then status(a)", got)
	}
	if got[0].Type != TypeTaskProgress || got[0].TaskID != "a" || got[0].Data != 2 {
		t.Errorf("first = %+v, want latest progress for a", got[0])
	}
	if got[1] != st || got[0].ID >= st.ID {
		t.Errorf("second = %+v, want status %+v after progress", got[1], st)
	}

	// b's progress is still pending and only appears on flush.
	b.flush()
	got = drain(s)
	if len(got) != 1 || got[0].TaskID != "b" || got[0].Data != 9 {
		t.Errorf("after flush got %v, want progress(b)", got)
	}
}

type fakeTicker struct {
	c       chan time.Time
	stopped chan struct{}
}

func (f *fakeTicker) new(time.Duration) (<-chan time.Time, func()) {
	return f.c, func() { close(f.stopped) }
}

func TestRunFlushesOnTickAndStops(t *testing.T) {
	tests := []struct {
		name string
		stop func(cancel context.CancelFunc, b *Bus)
	}{
		{"ctx cancel", func(cancel context.CancelFunc, _ *Bus) { cancel() }},
		{"bus close", func(_ context.CancelFunc, b *Bus) { b.Close() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestBus(Options{})
			defer b.Close()
			ft := &fakeTicker{c: make(chan time.Time), stopped: make(chan struct{})}
			b.newTicker = ft.new
			s := b.Subscribe(context.Background(), Filter{})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runErr := make(chan error, 1)
			go func() { runErr <- b.Run(ctx) }()

			b.Publish(Event{Type: TypeTaskProgress, TaskID: "a", Data: 1})
			ft.c <- time.Time{} // unbuffered: Run has received the tick
			// The flush happens right after the receive; wait for delivery.
			select {
			case e := <-s.C():
				if e.Type != TypeTaskProgress || e.Data != 1 {
					t.Fatalf("got %+v, want progress", e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("tick did not flush progress")
			}

			tt.stop(cancel, b)
			select {
			case err := <-runErr:
				if err != nil {
					t.Errorf("Run = %v, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return")
			}
			<-ft.stopped
		})
	}
}

func TestSinceReplay(t *testing.T) {
	b := newTestBus(Options{ReplaySize: 4})
	defer b.Close()

	got, complete := b.Since(0, Filter{})
	if len(got) != 0 || !complete {
		t.Fatalf("empty bus: got %v complete=%v, want none, true", got, complete)
	}

	for i := range 6 { // IDs 1..6; ring keeps 3..6
		task := "a"
		if i%2 == 1 {
			task = "b"
		}
		b.Publish(Event{Type: TypeTaskUpdated, TaskID: task})
	}

	tests := []struct {
		name         string
		after        uint64
		f            Filter
		want         []uint64
		wantComplete bool
	}{
		{"from zero has gap", 0, Filter{}, []uint64{3, 4, 5, 6}, false},
		{"just evicted has gap", 1, Filter{}, []uint64{3, 4, 5, 6}, false},
		{"last evicted is complete", 2, Filter{}, []uint64{3, 4, 5, 6}, true},
		{"middle", 4, Filter{}, []uint64{5, 6}, true},
		{"up to date", 6, Filter{}, nil, true},
		{"filtered", 2, Filter{TaskID: "b"}, []uint64{4, 6}, true},
		{"filtered with gap", 0, Filter{TaskID: "a"}, []uint64{3, 5}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evs, ok := b.Since(tt.after, tt.f)
			if !equalIDs(ids(evs), tt.want) || ok != tt.wantComplete {
				t.Errorf("Since(%d) = %v, %v; want %v, %v", tt.after, ids(evs), ok, tt.want, tt.wantComplete)
			}
		})
	}

	// Returned slice is a copy.
	got, _ = b.Since(0, Filter{})
	got[0].ID = 1000
	if again, _ := b.Since(0, Filter{}); again[0].ID != 3 {
		t.Error("Since returned internal storage")
	}
}

func TestSubscriptionEnds(t *testing.T) {
	t.Run("ctx cancel", func(t *testing.T) {
		b := newTestBus(Options{})
		defer b.Close()
		ctx, cancel := context.WithCancel(context.Background())
		s := b.Subscribe(ctx, Filter{})
		cancel()
		waitClosed(t, s)
		b.mu.Lock()
		n := len(b.subs)
		b.mu.Unlock()
		if n != 0 {
			t.Errorf("subs = %d after cancel, want 0", n)
		}
		b.Publish(Event{Type: TypeTaskCreated}) // must not panic
	})

	t.Run("already cancelled ctx", func(t *testing.T) {
		b := newTestBus(Options{})
		defer b.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		waitClosed(t, b.Subscribe(ctx, Filter{}))
	})

	t.Run("unsubscribe idempotent", func(t *testing.T) {
		b := newTestBus(Options{})
		defer b.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := b.Subscribe(ctx, Filter{})
		other := b.Subscribe(ctx, Filter{})
		s.Unsubscribe()
		s.Unsubscribe()
		waitClosed(t, s)
		cancel() // ctx callback after Unsubscribe must be harmless
		waitClosed(t, other)
	})

	t.Run("bus close", func(t *testing.T) {
		b := newTestBus(Options{})
		s := b.Subscribe(context.Background(), Filter{})
		b.Close()
		waitClosed(t, s)
		s.Unsubscribe() // after close: no panic
	})
}

func TestCloseSemantics(t *testing.T) {
	b := newTestBus(Options{})
	s := b.Subscribe(context.Background(), Filter{})
	b.Publish(Event{Type: TypeTaskProgress, TaskID: "a", Data: 1})

	b.Close()
	b.Close() // idempotent

	got := drain(s)
	if len(got) != 1 || got[0].Type != TypeTaskProgress {
		t.Fatalf("Close did not flush pending progress: %v", got)
	}
	select {
	case _, ok := <-s.C():
		if ok {
			t.Fatal("channel still open after Close")
		}
	default:
		t.Fatal("channel not closed after Close")
	}

	if e := b.Publish(Event{Type: TypeTaskCreated}); e != (Event{}) {
		t.Errorf("Publish after Close = %+v, want zero", e)
	}
	b.flush() // no-op, no panic

	late := b.Subscribe(context.Background(), Filter{})
	if _, ok := <-late.C(); ok {
		t.Error("Subscribe after Close returned open channel")
	}
	late.Unsubscribe()

	if err := b.Run(context.Background()); err != nil {
		t.Errorf("Run after Close = %v, want nil", err)
	}
}

func TestLastID(t *testing.T) {
	b := newTestBus(Options{})
	defer b.Close()
	if got := b.LastID(); got != 0 {
		t.Fatalf("LastID on empty bus = %d, want 0", got)
	}
	b.Publish(Event{Type: TypeTaskCreated, TaskID: "a"})
	b.Publish(Event{Type: TypeTaskProgress, TaskID: "a"}) // pending, no ID yet
	if got := b.LastID(); got != 1 {
		t.Fatalf("LastID with pending progress = %d, want 1", got)
	}
	b.flush()
	if got := b.LastID(); got != 2 {
		t.Fatalf("LastID after flush = %d, want 2", got)
	}
}

func TestConcurrentPublishersAndSubscribers(t *testing.T) {
	b := newTestBus(Options{SubscriberBuffer: 16, ReplaySize: 64})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var runWG sync.WaitGroup
	runWG.Go(func() { _ = b.Run(ctx) })

	const publishers, perPublisher, subscribers = 8, 200, 8
	var subWG sync.WaitGroup
	for range subscribers {
		s := b.Subscribe(ctx, Filter{})
		subWG.Go(func() {
			var last uint64
			for e := range s.C() {
				if e.ID <= last {
					t.Errorf("non-monotonic IDs: %d after %d", e.ID, last)
					return
				}
				last = e.ID
			}
		})
	}
	// Churning subscribers.
	var churnWG sync.WaitGroup
	for range 4 {
		churnWG.Go(func() {
			for range 50 {
				sctx, scancel := context.WithCancel(ctx)
				s := b.Subscribe(sctx, Filter{Types: []Type{TypeTaskStatus}})
				drain(s)
				_ = s.Dropped()
				scancel()
				s.Unsubscribe()
			}
		})
	}

	var pubWG sync.WaitGroup
	for p := range publishers {
		pubWG.Go(func() {
			for i := range perPublisher {
				typ := TypeTaskStatus
				if i%3 == 0 {
					typ = TypeTaskProgress
				}
				b.Publish(Event{Type: typ, TaskID: string(rune('a' + p))})
				if i%50 == 0 {
					b.flush()
					b.Since(uint64(i), Filter{})
				}
			}
		})
	}
	pubWG.Wait()
	churnWG.Wait()
	b.Close()
	subWG.Wait()
	runWG.Wait()

	evs, _ := b.Since(0, Filter{})
	for i := 1; i < len(evs); i++ {
		if evs[i].ID != evs[i-1].ID+1 {
			t.Fatalf("replay IDs not contiguous: %d then %d", evs[i-1].ID, evs[i].ID)
		}
	}
}
