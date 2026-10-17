package scheduler

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
)

// fakeClock is a settable clock shared by the scheduler and the tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// friday0800 is Friday 2026-10-16 08:00 UTC.
var friday0800 = time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// gateHarness is a harness whose scheduler uses a fake clock, a bus and a
// store that counts admission passes.
type gateHarness struct {
	*harness
	clock  *fakeClock
	bus    *events.Bus
	counts *countingStore
}

func newGateHarness(t *testing.T, o Options) *gateHarness {
	t.Helper()
	counts := &countingStore{Store: openStore(t)}
	clock := newFakeClock(friday0800)
	bus := events.New(events.Options{})
	t.Cleanup(bus.Close)
	o.Clock, o.Bus = clock.Now, bus
	h := newHarness(t, counts, o)
	h.store = counts
	return &gateHarness{harness: h, clock: clock, bus: bus, counts: counts}
}

// sync makes sure a complete admission pass ran after the call: it wakes
// the scheduler, waits until a pass started, then wakes it again and waits
// for the next pass to start, which happens only after the first one ended.
func (h *gateHarness) sync(t *testing.T) {
	t.Helper()
	before := h.counts.lists.Load()
	h.sched.Wake()
	waitFor(t, "a pass to start", func() bool { return h.counts.lists.Load() > before })
	mid := h.counts.lists.Load()
	h.sched.Wake()
	waitFor(t, "the next pass to start", func() bool { return h.counts.lists.Load() > mid })
}

// subscribe returns a subscription to the given event types.
func (h *gateHarness) subscribe(t *testing.T, types ...events.Type) *events.Subscription {
	t.Helper()
	sub := h.bus.Subscribe(t.Context(), events.Filter{Types: types})
	t.Cleanup(sub.Unsubscribe)
	return sub
}

// nextEvent waits for the next event of sub.
func nextEvent(t *testing.T, sub *events.Subscription) events.Event {
	t.Helper()
	select {
	case e := <-sub.C():
		return e
	case <-time.After(waitTimeout):
		t.Fatal("no event")
		return events.Event{}
	}
}

// waitRequeued waits until the run of id was cancelled with ErrRequeued and
// the task is queued again.
func (h *gateHarness) waitRequeued(t *testing.T, id string) {
	t.Helper()
	waitFor(t, "task requeued", func() bool {
		return errors.Is(h.runner.cause(id), service.ErrRequeued) && h.status(t, id) == domain.StatusQueued &&
			!slices.Contains(h.sched.Running(), id)
	})
}

func fridayWindow(start, end string) []settings.TimeWindow {
	return []settings.TimeWindow{{Days: []settings.Weekday{settings.Weekday(time.Friday)}, Start: start, End: end}}
}

func TestScheduleWindowGatesAdmission(t *testing.T) {
	h := newGateHarness(t, Options{})
	q := h.queue(t, "office", settings.Settings{
		Schedule: settings.Some(settings.Schedule{Timezone: "UTC", Windows: fridayWindow("09:00", "17:00")}),
	}, "")
	task := h.task(t, q.ID, 0)
	other := h.task(t, domain.MainQueueID, 0) // Main has no schedule
	h.start(t)

	if got := h.runner.nextStart(t); got != other.ID {
		t.Fatalf("started %s, want the unscheduled queue's task %s", got, other.ID)
	}
	h.sync(t)
	h.runner.noStart(t) // 08:00: before the window

	h.clock.Set(friday0800.Add(time.Hour)) // 09:00: window opens
	h.sched.Wake()
	if got := h.runner.nextStart(t); got != task.ID {
		t.Fatalf("started %s, want %s", got, task.ID)
	}

	h.clock.Set(friday0800.Add(9 * time.Hour)) // 17:00: window ends
	h.sched.Wake()
	h.waitRequeued(t, task.ID)
	h.sync(t)
	h.runner.noStart(t)
	if slices.Contains(h.sched.Running(), task.ID) {
		t.Error("task still running after the window ended")
	}
	if !slices.Contains(h.sched.Running(), other.ID) {
		t.Error("the unscheduled queue's task was stopped")
	}
	// The schedule is a gate only: the stored queue state is untouched.
	got, err := h.store.Queues().Get(t.Context(), q.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.QueueRunning {
		t.Errorf("queue state = %s, want running", got.State)
	}

	// Next Friday 09:00 the task resumes.
	h.clock.Set(friday0800.Add(7*24*time.Hour + time.Hour))
	h.sched.Wake()
	if got := h.runner.nextStart(t); got != task.ID {
		t.Fatalf("started %s next week, want %s", got, task.ID)
	}
}

func TestScheduleOvernightWindowInZone(t *testing.T) {
	h := newGateHarness(t, Options{})
	// Friday 22:00-02:00 in Tokyo (UTC+9) is Friday 13:00-17:00 UTC.
	q := h.queue(t, "night", settings.Settings{
		Schedule: settings.Some(settings.Schedule{Timezone: "Asia/Tokyo", Windows: fridayWindow("22:00", "02:00")}),
	}, "")
	task := h.task(t, q.ID, 0)
	h.clock.Set(time.Date(2026, 10, 16, 12, 59, 0, 0, time.UTC))
	h.start(t)
	h.sync(t)
	h.runner.noStart(t)

	h.clock.Set(time.Date(2026, 10, 16, 16, 0, 0, 0, time.UTC)) // Saturday 01:00 in Tokyo
	h.sched.Wake()
	if got := h.runner.nextStart(t); got != task.ID {
		t.Fatalf("started %s, want %s", got, task.ID)
	}
	h.clock.Set(time.Date(2026, 10, 16, 17, 0, 0, 0, time.UTC)) // Saturday 02:00 in Tokyo
	h.sched.Wake()
	h.waitRequeued(t, task.ID)
}

func TestSpeedProfilesSwitchLimits(t *testing.T) {
	limits := ratelimit.NewManager(0)
	h := newGateHarness(t, Options{})
	h.sched.o.Limits = service.NewLimitsController(service.Deps{Store: h.store, Limits: limits, Defaults: h.sched.o.Defaults})
	ctx := t.Context()
	global := settings.Settings{
		SpeedLimit: settings.Some[int64](1000),
		SpeedProfiles: settings.Some([]settings.SpeedProfile{
			{Timezone: "UTC", Windows: fridayWindow("09:00", "17:00"), SpeedLimit: 5000},
		}),
	}
	if err := h.store.Settings().SaveGlobal(ctx, global); err != nil {
		t.Fatal(err)
	}
	q := h.queue(t, "q", settings.Settings{
		SpeedLimit: settings.Some[int64](100),
		SpeedProfiles: settings.Some([]settings.SpeedProfile{
			{Timezone: "UTC", Windows: fridayWindow("12:00", "13:00"), SpeedLimit: 0},
			{Timezone: "UTC", Windows: fridayWindow("12:00", "18:00"), SpeedLimit: 300}, // shadowed 12-13 by the first
		}),
	}, "")
	h.start(t)

	steps := []struct {
		at            time.Duration // after 08:00
		global, queue int64
	}{
		{0, 1000, 100},
		{time.Hour, 5000, 100},                  // 09:00
		{4*time.Hour + 30*time.Minute, 5000, 0}, // 12:30: first matching profile wins
		{5 * time.Hour, 5000, 300},              // 13:00
		{9 * time.Hour, 1000, 300},              // 17:00
		{10 * time.Hour, 1000, 100},             // 18:00
	}
	for _, st := range steps {
		h.clock.Set(friday0800.Add(st.at))
		h.sync(t)
		if g, qv := limits.Global().Limit(), limits.Queue(q.ID).Limit(); g != st.global || qv != st.queue {
			t.Errorf("at 08:00+%v: global=%d queue=%d, want %d/%d", st.at, g, qv, st.global, st.queue)
		}
	}
}

func TestQueueQuotaRequeuesAndBlocksUntilReset(t *testing.T) {
	h := newGateHarness(t, Options{})
	sub := h.subscribe(t, events.TypeQuotaExhausted, events.TypeQuotaReset)
	q := h.queue(t, "metered", settings.Settings{
		MaxConcurrentDownloads: settings.Some(1),
		Quota:                  settings.Some(settings.Quota{Bytes: 1000, Period: settings.Duration(time.Hour)}),
	}, "")
	a, b := h.task(t, q.ID, 0), h.task(t, q.ID, 0)
	other := h.task(t, domain.MainQueueID, 0)
	h.start(t)
	h.runner.starts(t, 2) // a and Main's task

	meter := h.sched.Quotas()
	meter.AddBytes(a.ID, q.ID, 600)
	h.sync(t)
	if got := h.status(t, a.ID); got != domain.StatusRunning {
		t.Fatalf("below quota: status %s", got)
	}
	start := h.clock.Now()
	h.clock.Advance(10 * time.Minute)
	meter.AddBytes(a.ID, q.ID, 400) // reaches the quota: wakes the scheduler
	h.waitRequeued(t, a.ID)
	e := nextEvent(t, sub)
	d, ok := e.Data.(QuotaData)
	if e.Type != events.TypeQuotaExhausted || !ok || e.QueueID != q.ID || d.Scope != QuotaScopeQueue || d.Used != 1000 ||
		!d.WindowStart.Equal(start) || !d.ResetsAt.Equal(start.Add(time.Hour)) {
		t.Fatalf("event = %s %+v", e.Type, e.Data)
	}
	h.sync(t)
	h.runner.noStart(t) // neither a nor b while exhausted
	if !slices.Contains(h.sched.Running(), other.ID) {
		t.Error("another queue's task was stopped by a queue quota")
	}
	if u, _ := meter.Usage(q.ID); !u.Exhausted || u.Used != 1000 {
		t.Errorf("usage = %+v", u)
	}

	h.clock.Set(start.Add(time.Hour)) // the window has elapsed
	h.sched.Wake()
	if got := h.runner.nextStart(t); got != a.ID {
		t.Fatalf("after reset started %s, want %s", got, a.ID)
	}
	if e := nextEvent(t, sub); e.Type != events.TypeQuotaReset || e.QueueID != q.ID {
		t.Fatalf("event = %s %+v", e.Type, e.Data)
	}
	if u, _ := meter.Usage(q.ID); u.Exhausted || u.Used != 0 || !u.WindowStart.IsZero() {
		t.Errorf("usage after reset = %+v", u)
	}
	_ = b
}

func TestGlobalQuotaRequeuesEveryQueue(t *testing.T) {
	h := newGateHarness(t, Options{})
	sub := h.subscribe(t, events.TypeQuotaExhausted, events.TypeQuotaReset)
	if err := h.store.Settings().SaveGlobal(t.Context(), settings.Settings{
		Quota: settings.Some(settings.Quota{Bytes: 500, Period: settings.Duration(time.Minute)}),
	}); err != nil {
		t.Fatal(err)
	}
	q := h.queue(t, "other", settings.Settings{}, "")
	m, o := h.task(t, domain.MainQueueID, 0), h.task(t, q.ID, 0)
	h.start(t)
	h.runner.starts(t, 2)

	h.sched.Quotas().AddBytes(m.ID, domain.MainQueueID, 300)
	h.sched.Quotas().AddBytes(o.ID, q.ID, 300) // 600 >= 500 across queues
	h.waitRequeued(t, m.ID)
	h.waitRequeued(t, o.ID)
	e := nextEvent(t, sub)
	d, ok := e.Data.(QuotaData)
	if e.Type != events.TypeQuotaExhausted || e.QueueID != "" || !ok || d.Scope != QuotaScopeGlobal {
		t.Fatalf("event = %s %+v", e.Type, e.Data)
	}
	h.sync(t)
	h.runner.noStart(t)

	h.clock.Advance(time.Minute)
	h.sched.Wake()
	if got, want := h.runner.starts(t, 2), ids(m, o); !slices.Equal(got, want) {
		t.Fatalf("after reset started %v, want %v", got, want)
	}
	if e := nextEvent(t, sub); e.Type != events.TypeQuotaReset {
		t.Fatalf("event = %s", e.Type)
	}
}

func TestQuotaRemovedWhileExhaustedResets(t *testing.T) {
	h := newGateHarness(t, Options{})
	sub := h.subscribe(t, events.TypeQuotaExhausted, events.TypeQuotaReset)
	q := h.queue(t, "metered", settings.Settings{
		Quota: settings.Some(settings.Quota{Bytes: 10, Period: settings.Duration(time.Hour)}),
	}, "")
	task := h.task(t, q.ID, 0)
	h.start(t)
	h.runner.nextStart(t)
	h.sched.Quotas().AddBytes(task.ID, q.ID, 10)
	h.waitRequeued(t, task.ID)
	nextEvent(t, sub) // exhausted

	q.Settings.Quota = settings.None[settings.Quota]()
	if err := h.store.Queues().Update(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	h.sched.Wake()
	if got := h.runner.nextStart(t); got != task.ID {
		t.Fatalf("started %s", got)
	}
	if e := nextEvent(t, sub); e.Type != events.TypeQuotaReset {
		t.Fatalf("event = %s", e.Type)
	}
	if _, ok := h.sched.Quotas().Usage(q.ID); ok {
		t.Error("scope kept after its quota was removed")
	}
}

func TestQuotaMeterWindows(t *testing.T) {
	clock := newFakeClock(friday0800)
	m := NewQuotaMeter(clock.Now)
	wakes := 0
	m.setWake(func() { wakes++ })
	quota := settings.Some(settings.Quota{Bytes: 100, Period: settings.Duration(time.Hour)})

	m.AddBytes("t", "q", 50) // no scope configured yet: not counted
	if _, ok := m.Usage("q"); ok {
		t.Fatal("unconfigured scope has usage")
	}
	if ex, ch := m.check("q", quota, clock.Now()); ex || ch != nil {
		t.Fatalf("fresh scope exhausted=%v change=%v", ex, ch)
	}
	clock.Advance(time.Minute)
	m.AddBytes("t", "q", 60) // opens the window at 08:01
	clock.Advance(30 * time.Minute)
	m.AddBytes("t", "q", 0) // ignored
	m.AddBytes("t", "q", 40)
	if wakes != 1 {
		t.Errorf("wakes = %d, want 1 (crossing)", wakes)
	}
	m.AddBytes("t", "q", 5) // already exhausted: no second wake
	if wakes != 1 {
		t.Errorf("wakes = %d after exceeding", wakes)
	}
	ex, ch := m.check("q", quota, clock.Now())
	if !ex || ch == nil || ch.typ != quotaExhausted || ch.data.Used != 105 || !ch.data.WindowStart.Equal(friday0800.Add(time.Minute)) {
		t.Fatalf("exhausted=%v change=%+v", ex, ch)
	}
	if ex, ch := m.check("q", quota, clock.Now()); !ex || ch != nil {
		t.Fatalf("second check exhausted=%v change=%+v (want no repeat event)", ex, ch)
	}
	// The window is fixed: it ends one period after its first byte.
	clock.Set(friday0800.Add(time.Hour + time.Minute - time.Nanosecond))
	if ex, _ := m.check("q", quota, clock.Now()); !ex {
		t.Fatal("window ended early")
	}
	clock.Set(friday0800.Add(time.Hour + time.Minute))
	ex, ch = m.check("q", quota, clock.Now())
	if ex || ch == nil || ch.typ != quotaReset {
		t.Fatalf("after the period exhausted=%v change=%+v", ex, ch)
	}
	// Raising the quota of an exhausted scope also resets it.
	m.AddBytes("t", "q", 100)
	if ex, _ := m.check("q", quota, clock.Now()); !ex {
		t.Fatal("not exhausted")
	}
	bigger := settings.Some(settings.Quota{Bytes: 1000, Period: settings.Duration(time.Hour)})
	if ex, ch := m.check("q", bigger, clock.Now()); ex || ch == nil || ch.typ != quotaReset {
		t.Fatalf("raised quota exhausted=%v change=%+v", ex, ch)
	}
	// The global scope counts every queue's bytes.
	if _, ch := m.check(globalScope, quota, clock.Now()); ch != nil {
		t.Fatal(ch)
	}
	m.AddBytes("t1", "a", 70)
	m.AddBytes("t2", "b", 30)
	if u, ok := m.Usage(globalScope); !ok || !u.Exhausted || u.Used != 100 {
		t.Errorf("global usage = %+v", u)
	}
	m.prune(map[string]bool{})
	if _, ok := m.Usage("q"); ok {
		t.Error("pruned scope kept")
	}
	if _, ok := m.Usage(globalScope); !ok {
		t.Error("global scope pruned")
	}
}

// fakeExecutor records calls without touching the real machine.
type fakeExecutor struct {
	mu        sync.Mutex
	sleeps    int
	shutdowns int
	commands  [][]string // each: [name, arg0, arg1, ...]
}

func (f *fakeExecutor) Sleep(context.Context) error {
	f.mu.Lock()
	f.sleeps++
	f.mu.Unlock()
	return nil
}

func (f *fakeExecutor) Shutdown(context.Context) error {
	f.mu.Lock()
	f.shutdowns++
	f.mu.Unlock()
	return nil
}

func (f *fakeExecutor) Command(_ context.Context, name string, args []string) ([]byte, error) {
	f.mu.Lock()
	f.commands = append(f.commands, append([]string{name}, args...))
	f.mu.Unlock()
	return []byte("ok"), nil
}

func (f *fakeExecutor) stats() (sleeps, shutdowns, cmds int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sleeps, f.shutdowns, len(f.commands)
}

func TestPostQueueActionCommandFires(t *testing.T) {
	exec := &fakeExecutor{}
	h := newGateHarness(t, Options{Actions: exec})
	sub := h.subscribe(t, events.TypeQueueCompleted)
	q := h.queue(t, "batch", settings.Settings{
		MaxConcurrentDownloads: settings.Some(1),
		OnQueueComplete: settings.Some(settings.QueueCompleteAction{
			Action:  settings.QueueActionCommand,
			Command: "/usr/bin/true",
			Args:    []string{"--done"},
		}),
	}, "")
	task := h.task(t, q.ID, 0)
	h.start(t)
	h.runner.nextStart(t)

	// Complete the task; wait for the run goroutine to finish so the
	// scheduler's completed map is incremented.
	h.runner.release(task.ID)
	waitFor(t, "task no longer running", func() bool { return !slices.Contains(h.sched.Running(), task.ID) })
	h.sync(t)              // pass schedules action
	e := nextEvent(t, sub) // queue.completed
	d, ok := e.Data.(QueueCompletedData)
	if !ok || d.Action != settings.QueueActionCommand || d.CompletedTasks != 1 {
		t.Fatalf("event data = %+v", e.Data)
	}

	// Advance clock past due time and sync to execute.
	h.clock.Set(d.ExecuteAt)
	h.sync(t)

	// Wait for the executor to receive the command.
	waitFor(t, "command executed", func() bool {
		_, _, cmds := exec.stats()
		return cmds > 0
	})
	exec.mu.Lock()
	got := exec.commands[0]
	exec.mu.Unlock()
	if got[0] != "/usr/bin/true" || len(got) != 2 || got[1] != "--done" {
		t.Errorf("command = %v", got)
	}
}

func TestPostQueueActionSleepFires(t *testing.T) {
	exec := &fakeExecutor{}
	h := newGateHarness(t, Options{Actions: exec})
	sub := h.subscribe(t, events.TypeQueueCompleted)
	q := h.queue(t, "batch", settings.Settings{
		MaxConcurrentDownloads: settings.Some(1),
		OnQueueComplete: settings.Some(settings.QueueCompleteAction{
			Action: settings.QueueActionSleep,
		}),
	}, "")
	task := h.task(t, q.ID, 0)
	h.start(t)
	h.runner.nextStart(t)
	h.runner.release(task.ID)
	waitFor(t, "task no longer running", func() bool { return !slices.Contains(h.sched.Running(), task.ID) })
	h.sync(t)
	e := nextEvent(t, sub)
	d, ok := e.Data.(QueueCompletedData)
	if !ok {
		t.Fatalf("event data = %+v", e.Data)
	}

	h.clock.Set(d.ExecuteAt)
	h.sync(t)
	waitFor(t, "sleep executed", func() bool {
		s, _, _ := exec.stats()
		return s > 0
	})
}

func TestPostQueueActionCancelledByNewWork(t *testing.T) {
	exec := &fakeExecutor{}
	h := newGateHarness(t, Options{Actions: exec})
	sub := h.subscribe(t, events.TypeQueueCompleted, events.TypeQueueActionCancelled)
	q := h.queue(t, "batch", settings.Settings{
		MaxConcurrentDownloads: settings.Some(1),
		OnQueueComplete: settings.Some(settings.QueueCompleteAction{
			Action: settings.QueueActionShutdown,
			Delay:  settings.Duration(time.Hour), // long delay
		}),
	}, "")
	task := h.task(t, q.ID, 0)
	h.start(t)
	h.runner.nextStart(t)
	h.runner.release(task.ID) // complete → pending action
	waitFor(t, "task no longer running", func() bool { return !slices.Contains(h.sched.Running(), task.ID) })
	h.sync(t)
	e := nextEvent(t, sub)
	if e.Type != events.TypeQueueCompleted {
		t.Fatalf("event = %s", e.Type)
	}

	// Add new work while the action is pending.
	h.task(t, q.ID, 0)
	h.sched.Wake()
	e = nextEvent(t, sub)
	if e.Type != events.TypeQueueActionCancelled {
		t.Fatalf("event = %s", e.Type)
	}
	d, ok := e.Data.(ActionCancelledData)
	if !ok || d.Reason != CancelNewWork || d.Action != settings.QueueActionShutdown {
		t.Errorf("data = %+v", e.Data)
	}
	// Executor never ran.
	if s, sh, _ := exec.stats(); s != 0 || sh != 0 {
		t.Errorf("executor fired: sleeps=%d shutdowns=%d", s, sh)
	}
}

func TestPostQueueActionCancelledByQueueStop(t *testing.T) {
	exec := &fakeExecutor{}
	stopped := make(chan string, 1)
	stopQueue := func(_ context.Context, id string) error {
		stopped <- id
		return nil
	}
	h := newGateHarness(t, Options{Actions: exec, StopQueue: stopQueue})
	sub := h.subscribe(t, events.TypeQueueCompleted, events.TypeQueueActionCancelled)
	q := h.queue(t, "batch", settings.Settings{
		MaxConcurrentDownloads: settings.Some(1),
		OnQueueComplete: settings.Some(settings.QueueCompleteAction{
			Action: settings.QueueActionSleep,
			Delay:  settings.Duration(time.Hour),
		}),
	}, "")
	task := h.task(t, q.ID, 0)
	h.start(t)
	h.runner.nextStart(t)
	h.runner.release(task.ID)
	waitFor(t, "task done", func() bool { return !slices.Contains(h.sched.Running(), task.ID) })
	h.sync(t)
	nextEvent(t, sub) // queue.completed

	// Stop the queue while the action is pending.
	q.State = domain.QueueStopped
	if err := h.store.Queues().Update(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	h.sched.Wake()
	e := nextEvent(t, sub)
	if e.Type != events.TypeQueueActionCancelled {
		t.Fatalf("event = %s", e.Type)
	}
	d, ok := e.Data.(ActionCancelledData)
	if !ok || d.Reason != CancelQueueStopped {
		t.Errorf("data = %+v", e.Data)
	}
}

func TestPostQueueActionStopQueue(t *testing.T) {
	stopped := make(chan string, 1)
	stopQueue := func(_ context.Context, id string) error {
		stopped <- id
		return nil
	}
	h := newGateHarness(t, Options{StopQueue: stopQueue})
	sub := h.subscribe(t, events.TypeQueueCompleted)
	q := h.queue(t, "batch", settings.Settings{
		MaxConcurrentDownloads: settings.Some(1),
		OnQueueComplete: settings.Some(settings.QueueCompleteAction{
			Action: settings.QueueActionStop,
		}),
	}, "")
	task := h.task(t, q.ID, 0)
	h.start(t)
	h.runner.nextStart(t)
	h.runner.release(task.ID)
	waitFor(t, "task no longer running", func() bool { return !slices.Contains(h.sched.Running(), task.ID) })
	h.sync(t)
	e := nextEvent(t, sub)
	d, ok := e.Data.(QueueCompletedData)
	if !ok {
		t.Fatalf("event data = %+v", e.Data)
	}

	h.clock.Set(d.ExecuteAt)
	h.sync(t)
	select {
	case id := <-stopped:
		if id != q.ID {
			t.Errorf("stopped queue %s, want %s", id, q.ID)
		}
	case <-time.After(waitTimeout):
		t.Fatal("stop not called")
	}
}

var _ = context.Background
