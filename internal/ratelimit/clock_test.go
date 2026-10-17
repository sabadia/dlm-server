package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock. Timers fire when Advance moves
// the clock past their deadline.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	timers map[*fakeTimer]struct{}
}

type fakeTimer struct {
	at time.Time
	d  time.Duration
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		t:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		timers: make(map[*fakeTimer]struct{}),
	}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) newTimer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ft := &fakeTimer{at: c.t.Add(d), d: d, ch: make(chan time.Time, 1)}
	if d <= 0 {
		ft.ch <- c.t
		return ft.ch, func() {}
	}
	c.timers[ft] = struct{}{}
	return ft.ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.timers, ft)
	}
}

// Advance moves the clock forward by d and fires due timers.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	for ft := range c.timers {
		if !ft.at.After(c.t) {
			ft.ch <- c.t
			delete(c.timers, ft)
		}
	}
}

// AdvanceToNext moves the clock to the earliest pending timer deadline and
// fires it. It reports false when no timer is pending.
func (c *fakeClock) AdvanceToNext() bool {
	c.mu.Lock()
	var next time.Time
	for ft := range c.timers {
		if next.IsZero() || ft.at.Before(next) {
			next = ft.at
		}
	}
	d := next.Sub(c.t)
	c.mu.Unlock()
	if next.IsZero() {
		return false
	}
	c.Advance(d)
	return true
}

// Pending returns the durations the pending timers were created with.
func (c *fakeClock) Pending() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, 0, len(c.timers))
	for ft := range c.timers {
		out = append(out, ft.d)
	}
	return out
}

// waitPending blocks until exactly n timers are pending, failing the test
// after a generous real-time deadline. This is synchronisation with the
// goroutine under test, not a correctness sleep.
func (c *fakeClock) waitPending(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if len(c.Pending()) == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d pending timers, have %d", n, len(c.Pending()))
		}
		time.Sleep(time.Millisecond)
	}
}
