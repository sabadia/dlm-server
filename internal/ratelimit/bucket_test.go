package ratelimit

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// wait for a result from done, failing after a generous real-time deadline.
func recvErr(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for WaitN to return")
		return nil
	}
}

// drive runs fn in a goroutine and advances clk to each pending timer
// until fn returns. It returns fn's error and the fake time elapsed.
func drive(t *testing.T, clk *fakeClock, fn func() error) (time.Duration, error) {
	t.Helper()
	start := clk.now()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case err := <-done:
			return clk.now().Sub(start), err
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out driving fake clock")
		}
		if !clk.AdvanceToNext() {
			time.Sleep(100 * time.Microsecond)
		}
	}
}

func TestBurstFor(t *testing.T) {
	tests := []struct {
		limit int64
		want  int
	}{
		{1, 1},
		{1000, 1000},
		{64 << 10, 64 << 10},
		{64<<10 + 1, 64 << 10},
		{256 << 10, 64 << 10},
		{1 << 20, 256 << 10},
		{16 << 20, 4 << 20},
		{1 << 40, 4 << 20},
	}
	for _, tt := range tests {
		if got := burstFor(tt.limit); got != tt.want {
			t.Errorf("burstFor(%d) = %d, want %d", tt.limit, got, tt.want)
		}
	}
}

func TestUnlimitedNeverBlocks(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, b := range []*Bucket{NewBucket(0), NewBucket(-5)} {
		if b.Limit() != 0 {
			t.Fatalf("Limit() = %d, want 0", b.Limit())
		}
		if err := b.WaitN(t.Context(), math.MaxInt); err != nil {
			t.Fatalf("WaitN: %v", err)
		}
		if err := b.WaitN(cancelled, 1<<30); err != nil {
			t.Fatalf("WaitN with cancelled ctx on unlimited bucket: %v", err)
		}
	}
	c := Chain(NewBucket(0), nil, NewBucket(0))
	if err := c.WaitN(t.Context(), math.MaxInt); err != nil {
		t.Fatalf("chain WaitN: %v", err)
	}
	if err := Chain().WaitN(t.Context(), 1<<30); err != nil {
		t.Fatalf("empty chain WaitN: %v", err)
	}
}

func TestBucketWaitsForTokens(t *testing.T) {
	clk := newFakeClock()
	b := newBucket(1000, clk)

	// The bucket starts full: one burst passes at once.
	if err := b.WaitN(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	if p := clk.Pending(); len(p) != 0 {
		t.Fatalf("full bucket blocked: pending %v", p)
	}

	done := make(chan error, 1)
	go func() { done <- b.WaitN(t.Context(), 500) }()
	clk.waitPending(t, 1)
	if p := clk.Pending(); p[0] != 500*time.Millisecond {
		t.Fatalf("delay = %v, want 500ms", p[0])
	}
	clk.Advance(499 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("WaitN returned early: %v", err)
	default:
	}
	clk.Advance(time.Millisecond)
	if err := recvErr(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBucketLargerThanBurst(t *testing.T) {
	clk := newFakeClock()
	b := newBucket(100, clk) // burst 100
	elapsed, err := drive(t, clk, func() error { return b.WaitN(t.Context(), 350) })
	if err != nil {
		t.Fatal(err)
	}
	// 100 from the full bucket, then 250 at 100 B/s.
	if want := 2500 * time.Millisecond; elapsed < want-time.Millisecond || elapsed > want+10*time.Millisecond {
		t.Fatalf("elapsed = %v, want ~%v", elapsed, want)
	}
}

func TestSetLimitReleasesWaiters(t *testing.T) {
	tests := []struct {
		name     string
		newLimit int64
	}{
		{"removed", 0},
		{"raised", 1 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := newFakeClock()
			b := newBucket(10, clk)
			if err := b.WaitN(t.Context(), 10); err != nil { // drain
				t.Fatal(err)
			}
			const waiters = 3
			done := make(chan error, waiters)
			for range waiters {
				go func() { done <- b.WaitN(t.Context(), 10) }()
			}
			clk.waitPending(t, waiters) // delays of 1s, 2s and 3s

			start := clk.now()
			b.SetLimit(tt.newLimit)
			if got := b.Limit(); got != tt.newLimit {
				t.Fatalf("Limit() = %d, want %d", got, tt.newLimit)
			}
			for i := 0; i < waiters; {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
					i++
					continue
				case <-time.After(time.Millisecond):
				}
				// A raised limit may leave microsecond waits; never seconds.
				for _, d := range clk.Pending() {
					if d > time.Millisecond {
						t.Fatalf("waiter still blocked for %v after SetLimit", d)
					}
				}
				clk.AdvanceToNext()
			}
			if elapsed := clk.now().Sub(start); elapsed > 10*time.Millisecond {
				t.Fatalf("waiters released after %v of fake time", elapsed)
			}
		})
	}
}

func TestSetLimitLowered(t *testing.T) {
	clk := newFakeClock()
	b := newBucket(1000, clk)
	b.SetLimit(100)
	if b.Limit() != 100 {
		t.Fatalf("Limit() = %d", b.Limit())
	}
	// Burst shrinks with the limit, so 100 passes, then 100 B/s applies.
	elapsed, err := drive(t, clk, func() error { return b.WaitN(t.Context(), 300) })
	if err != nil {
		t.Fatal(err)
	}
	if want := 2 * time.Second; elapsed < want-time.Millisecond || elapsed > want+10*time.Millisecond {
		t.Fatalf("elapsed = %v, want ~%v", elapsed, want)
	}
	b.SetLimit(100) // no-op
	b.SetLimit(-1)
	if b.Limit() != 0 {
		t.Fatalf("negative limit: Limit() = %d, want 0", b.Limit())
	}
}

func TestWaitNContextCancel(t *testing.T) {
	clk := newFakeClock()
	b := newBucket(1, clk)
	if err := b.WaitN(t.Context(), 1); err != nil { // drain
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- b.WaitN(ctx, 1) }()
	clk.waitPending(t, 1)
	cancel()
	if err := recvErr(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	clk.waitPending(t, 0) // timer stopped

	// An already-cancelled context fails fast even with tokens available.
	b2 := newBucket(1000, clk)
	if err := b2.WaitN(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// The cancelled reservation was given back: the next byte is due one
	// second after the drain, not two.
	elapsed, err := drive(t, clk, func() error { return b.WaitN(t.Context(), 1) })
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > time.Second+10*time.Millisecond {
		t.Fatalf("elapsed = %v, want <= 1s (reservation not returned)", elapsed)
	}
}

func TestWaitNDeadline(t *testing.T) {
	b := NewBucket(1)
	if err := b.WaitN(t.Context(), 1); err != nil { // drain
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := b.WaitN(ctx, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("WaitN returned after %v, want prompt return on deadline", elapsed)
	}
}

func TestZeroAndNegativeN(t *testing.T) {
	clk := newFakeClock()
	b := newBucket(1, clk)
	for _, n := range []int{0, -1} {
		if err := b.WaitN(t.Context(), n); err != nil {
			t.Fatalf("WaitN(%d): %v", n, err)
		}
	}
	if len(clk.Pending()) != 0 {
		t.Fatal("WaitN(<=0) blocked")
	}
}
