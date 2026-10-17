package ratelimit

import (
	"testing"
	"time"
)

func TestChainEnforcesMinimum(t *testing.T) {
	const total = 10000
	tests := []struct {
		name                string
		task, queue, global int64
		// want is the time the slowest bucket needs once its initial
		// burst (one second worth at these rates) is used up.
		want time.Duration
	}{
		{"queue slowest", 1000, 500, 2000, 19 * time.Second},
		{"task slowest", 500, 1000, 2000, 19 * time.Second},
		{"global slowest", 2000, 1000, 500, 19 * time.Second},
		{"equal", 1000, 1000, 1000, 9 * time.Second},
		{"task unlimited", 0, 1000, 2000, 9 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := newFakeClock()
			c := Chain(newBucket(tt.task, clk), newBucket(tt.queue, clk), newBucket(tt.global, clk))
			elapsed, err := drive(t, clk, func() error { return c.WaitN(t.Context(), total) })
			if err != nil {
				t.Fatal(err)
			}
			// Sequential waits may cost a little accuracy, never less time.
			if elapsed < tt.want-time.Millisecond || elapsed > tt.want+tt.want/20 {
				t.Fatalf("elapsed = %v, want ~%v", elapsed, tt.want)
			}
		})
	}
}

func TestChainLiveUpdate(t *testing.T) {
	clk := newFakeClock()
	task, global := newBucket(0, clk), newBucket(10, clk)
	c := Chain(task, nil, global)
	if err := c.WaitN(t.Context(), 10); err != nil { // drain global
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.WaitN(t.Context(), 10) }()
	clk.waitPending(t, 1)
	global.SetLimit(0)
	if err := recvErr(t, done); err != nil {
		t.Fatal(err)
	}

	// A bucket that becomes limited is enforced by an existing chain.
	task.SetLimit(100)
	elapsed, err := drive(t, clk, func() error { return c.WaitN(t.Context(), 300) })
	if err != nil {
		t.Fatal(err)
	}
	if want := 2 * time.Second; elapsed < want-time.Millisecond || elapsed > want+want/20 {
		t.Fatalf("elapsed = %v, want ~%v", elapsed, want)
	}
}
