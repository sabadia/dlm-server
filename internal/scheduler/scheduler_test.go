package scheduler

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
)

func TestPerQueueSlotLimit(t *testing.T) {
	h := newHarness(t, nil, Options{})
	q := h.queue(t, "two", settings.Settings{MaxConcurrentDownloads: settings.Some(2)}, "")
	var tasks []*domain.Task
	for range 5 {
		tasks = append(tasks, h.task(t, q.ID, domain.PriorityNormal))
	}
	h.start(t)

	if got, want := h.runner.starts(t, 2), ids(tasks[0], tasks[1]); !cmp.Equal(got, want) {
		t.Fatalf("first starts = %v, want %v", got, want)
	}
	h.runner.noStart(t)
	if got := h.sched.Running(); !cmp.Equal(got, ids(tasks[0], tasks[1])) {
		t.Errorf("Running() = %v", got)
	}

	// Completion frees a slot and the next task in queue order starts.
	h.runner.release(tasks[0].ID)
	if got := h.runner.nextStart(t); got != tasks[2].ID {
		t.Fatalf("after completion started %s, want %s", got, tasks[2].ID)
	}
	h.runner.noStart(t)
	for _, task := range tasks[1:] {
		h.runner.release(task.ID)
	}
	h.runner.starts(t, 2)
	waitFor(t, "all tasks completed", func() bool {
		for _, task := range tasks {
			if h.status(t, task.ID) != domain.StatusCompleted {
				return false
			}
		}
		return true
	})
	if m := h.runner.max(); m > 2 {
		t.Errorf("max concurrent runs = %d, want <= 2", m)
	}
	waitFor(t, "running set empty", func() bool { return len(h.sched.Running()) == 0 })
}

func TestQueuesHaveIndependentSlots(t *testing.T) {
	h := newHarness(t, nil, Options{Defaults: defaultsWith(1)})
	q := h.queue(t, "other", settings.Settings{}, "")
	m1, m2 := h.task(t, domain.MainQueueID, 0), h.task(t, domain.MainQueueID, 0)
	o1, o2 := h.task(t, q.ID, 0), h.task(t, q.ID, 0)
	h.start(t)
	if got, want := h.runner.starts(t, 2), ids(m1, o1); !cmp.Equal(got, want) {
		t.Fatalf("starts = %v, want %v", got, want)
	}
	h.runner.noStart(t)
	h.runner.release(o1.ID)
	if got := h.runner.nextStart(t); got != o2.ID {
		t.Fatalf("started %s, want %s", got, o2.ID)
	}
	_ = m2
}

func TestGlobalCapDistributesFairly(t *testing.T) {
	h := newHarness(t, nil, Options{})
	ctx := context.Background()
	if err := h.store.Settings().SaveGlobal(ctx, settings.Settings{MaxActiveDownloadsTotal: settings.Some(3)}); err != nil {
		t.Fatal(err)
	}
	q2 := h.queue(t, "second", settings.Settings{}, "")
	q3 := h.queue(t, "third", settings.Settings{}, "")
	var main, second []*domain.Task
	for range 4 {
		main = append(main, h.task(t, domain.MainQueueID, 0))
		second = append(second, h.task(t, q2.ID, 0))
	}
	third := h.task(t, q3.ID, 0)
	h.start(t)

	// One per queue in queue order: Main, second, third.
	if got, want := h.runner.starts(t, 3), ids(main[0], second[0], third); !cmp.Equal(got, want) {
		t.Fatalf("starts = %v, want %v", got, want)
	}
	h.runner.noStart(t)

	// The freed global slot goes round-robin again from the first queue.
	h.runner.release(third.ID)
	if got := h.runner.nextStart(t); got != main[1].ID {
		t.Fatalf("started %s, want %s", got, main[1].ID)
	}
	h.runner.noStart(t)
}

func TestDistribute(t *testing.T) {
	plan := []queueSlots{
		{queue: &domain.Queue{ID: "a"}, candidates: []string{"a1", "a2", "a3"}},
		{queue: &domain.Queue{ID: "b"}, candidates: []string{"b1"}},
		{queue: &domain.Queue{ID: "c"}, candidates: []string{"c1", "c2"}},
	}
	flatten := func(p []queueSlots) []string {
		var out []string
		for _, qs := range p {
			out = append(out, qs.candidates...)
		}
		return out
	}
	tests := []struct {
		free int
		want []string
	}{
		{-1, []string{"a1", "a2", "a3", "b1", "c1", "c2"}},
		{1, []string{"a1"}},
		{2, []string{"a1", "b1"}},
		{4, []string{"a1", "a2", "b1", "c1"}},
		{5, []string{"a1", "a2", "b1", "c1", "c2"}},
		{100, []string{"a1", "a2", "a3", "b1", "c1", "c2"}},
	}
	for _, tt := range tests {
		if got := flatten(distribute(plan, tt.free)); !cmp.Equal(got, tt.want) {
			t.Errorf("distribute(free=%d) = %v, want %v", tt.free, got, tt.want)
		}
	}
}

func TestPriorityThenPositionOrdering(t *testing.T) {
	h := newHarness(t, nil, Options{Defaults: defaultsWith(1)})
	n1 := h.task(t, domain.MainQueueID, domain.PriorityNormal)
	high := h.task(t, domain.MainQueueID, domain.PriorityHigh)
	low := h.task(t, domain.MainQueueID, domain.PriorityLow)
	n2 := h.task(t, domain.MainQueueID, domain.PriorityNormal)
	h.start(t)
	var order []string
	for range 4 {
		id := h.runner.nextStart(t)
		order = append(order, id)
		h.runner.release(id)
	}
	if want := []string{high.ID, n1.ID, n2.ID, low.ID}; !cmp.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestStoppedQueueIsNotAdmitted(t *testing.T) {
	h := newHarness(t, nil, Options{})
	q := h.queue(t, "stopped", settings.Settings{}, domain.QueueStopped)
	task := h.task(t, q.ID, 0)
	h.start(t)
	h.runner.noStart(t)

	q.Start(time.Now())
	if err := h.store.Queues().Update(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	h.sched.Wake()
	if got := h.runner.nextStart(t); got != task.ID {
		t.Fatalf("started %s, want %s", got, task.ID)
	}
}

func TestPausedOrDeletedTaskIsNeverStarted(t *testing.T) {
	h := newHarness(t, nil, Options{Defaults: defaultsWith(1)})
	ctx := context.Background()
	paused := h.task(t, domain.MainQueueID, domain.PriorityHigh)
	deleted := h.task(t, domain.MainQueueID, domain.PriorityHigh)
	next := h.task(t, domain.MainQueueID, 0)
	if err := paused.TransitionTo(domain.StatusPaused, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Tasks().Update(ctx, paused); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Tasks().Delete(ctx, deleted.ID); err != nil {
		t.Fatal(err)
	}
	h.start(t)
	if got := h.runner.nextStart(t); got != next.ID {
		t.Fatalf("started %s, want %s", got, next.ID)
	}
	h.runner.release(next.ID)
	h.runner.noStart(t)
	if got := h.status(t, paused.ID); got != domain.StatusPaused {
		t.Errorf("paused task status = %s", got)
	}
}

func TestStopWaitsForRunToEnd(t *testing.T) {
	h := newHarness(t, nil, Options{})
	task := h.task(t, domain.MainQueueID, 0)
	h.start(t)
	h.runner.nextStart(t)

	if err := h.sched.Stop(t.Context(), task.ID, service.ErrPaused); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// The run has returned and persisted its final state.
	if got := h.status(t, task.ID); got != domain.StatusPaused {
		t.Errorf("status after Stop = %s, want paused", got)
	}
	if !errors.Is(h.runner.cause(task.ID), service.ErrPaused) {
		t.Errorf("cause = %v", h.runner.cause(task.ID))
	}
	if got := h.sched.Running(); len(got) != 0 {
		t.Errorf("Running() = %v", got)
	}
	// Not running: nil at once, also with an expired context.
	done, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.sched.Stop(done, task.ID, service.ErrPaused); err != nil {
		t.Errorf("Stop of a stopped task = %v", err)
	}
	if err := h.sched.Stop(t.Context(), "unknown", service.ErrRequeued); err != nil {
		t.Errorf("Stop of an unknown task = %v", err)
	}
	h.runner.noStart(t) // a paused task is not admitted again
}

func TestStopHonoursContext(t *testing.T) {
	h := newHarness(t, nil, Options{})
	task := h.task(t, domain.MainQueueID, 0)
	h.runner.stubborn[task.ID] = true
	h.start(t)
	h.runner.nextStart(t)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := h.sched.Stop(ctx, task.ID, service.ErrPaused); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v, want DeadlineExceeded", err)
	}
	h.runner.release(task.ID)
	waitFor(t, "run ended", func() bool { return len(h.sched.Running()) == 0 })
}

func TestCompletionWakesWithoutTick(t *testing.T) {
	h := newHarness(t, nil, Options{Defaults: defaultsWith(1)})
	a, b := h.task(t, domain.MainQueueID, 0), h.task(t, domain.MainQueueID, 0)
	h.start(t)
	if got := h.runner.nextStart(t); got != a.ID {
		t.Fatalf("started %s", got)
	}
	h.runner.release(a.ID)
	if got := h.runner.nextStart(t); got != b.ID {
		t.Fatalf("started %s, want %s", got, b.ID)
	}
}

func TestWakeCoalesces(t *testing.T) {
	store := &countingStore{Store: openStore(t), gate: make(chan struct{})}
	h := newHarness(t, store, Options{})
	h.start(t)
	waitFor(t, "first pass", func() bool { return store.lists.Load() == 1 })
	// The first pass is blocked; many wake-ups merge into one pending pass.
	for range 100 {
		h.sched.Wake()
	}
	close(store.gate)
	waitFor(t, "second pass", func() bool { return store.lists.Load() == 2 })
	time.Sleep(quiet)
	if n := store.lists.Load(); n != 2 {
		t.Fatalf("admission passes = %d, want 2", n)
	}
}

func TestTickReevaluates(t *testing.T) {
	h := newHarness(t, nil, Options{Tick: 5 * time.Millisecond})
	h.start(t)
	// Created behind the scheduler's back: no Wake.
	task := h.task(t, domain.MainQueueID, 0)
	if got := h.runner.nextStart(t); got != task.ID {
		t.Fatalf("started %s", got)
	}
}

func TestShutdownRequeuesAndWaits(t *testing.T) {
	h := newHarness(t, nil, Options{})
	a, b := h.task(t, domain.MainQueueID, 0), h.task(t, domain.MainQueueID, 0)
	h.start(t)
	h.runner.starts(t, 2)

	h.stop(t)
	// Run returned only after both runs persisted their final state.
	for _, id := range []string{a.ID, b.ID} {
		if got := h.status(t, id); got != domain.StatusQueued {
			t.Errorf("task %s status = %s, want queued", id, got)
		}
		if !errors.Is(h.runner.cause(id), service.ErrRequeued) {
			t.Errorf("task %s cause = %v, want ErrRequeued", id, h.runner.cause(id))
		}
	}
	if got := h.sched.Running(); len(got) != 0 {
		t.Errorf("Running() after shutdown = %v", got)
	}
	if err := h.sched.Run(t.Context()); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second Run = %v, want ErrAlreadyRunning", err)
	}
}

func TestLoweredLimitDoesNotPreempt(t *testing.T) {
	h := newHarness(t, nil, Options{})
	ctx := context.Background()
	q := h.queue(t, "q", settings.Settings{MaxConcurrentDownloads: settings.Some(2)}, "")
	a, b, c := h.task(t, q.ID, 0), h.task(t, q.ID, 0), h.task(t, q.ID, 0)
	h.start(t)
	h.runner.starts(t, 2)

	if err := q.SetSettings(settings.Settings{MaxConcurrentDownloads: settings.Some(1)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Queues().Update(ctx, q); err != nil {
		t.Fatal(err)
	}
	h.sched.Wake()
	h.runner.noStart(t)
	if got := h.sched.Running(); !cmp.Equal(got, ids(a, b)) {
		t.Fatalf("running = %v, want both kept", got)
	}
	// One completion leaves one running: still at the new limit.
	h.runner.release(a.ID)
	h.runner.noStart(t)
	h.runner.release(b.ID)
	if got := h.runner.nextStart(t); got != c.ID {
		t.Fatalf("started %s, want %s", got, c.ID)
	}
	if got := h.sched.Running(); !slices.Equal(got, []string{c.ID}) {
		t.Errorf("running = %v", got)
	}
}
