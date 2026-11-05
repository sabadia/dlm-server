package hostgate

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireCapsRequestsInFlight(t *testing.T) {
	g := New(nil)
	var inFlight, peak atomic.Int32
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := g.Acquire(t.Context(), "h", 5)
			if err != nil {
				t.Error(err)
				return
			}
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inFlight.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if peak.Load() > 5 {
		t.Errorf("peak %d requests in flight, cap is 5", peak.Load())
	}
}

func TestHostsAreIndependentAndZeroMeansNoCap(t *testing.T) {
	g := New(nil)
	var releases []func()
	for range 20 {
		r, err := g.Acquire(t.Context(), "a", 0)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, r)
	}
	one, err := g.Acquire(t.Context(), "b", 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.Acquire(ctx, "b", 1); err == nil {
		t.Error("a second request to a host capped at 1 was let in")
	}
	one()
	for _, r := range releases {
		r()
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	g := New(nil)
	r, _ := g.Acquire(t.Context(), "h", 1)
	r()
	r()
	if _, err := g.Acquire(t.Context(), "h", 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.Acquire(ctx, "h", 1); err == nil {
		t.Error("a double release freed a second slot")
	}
}

func TestTroubleBurstMakesTheHostRestAndGrowsThenResets(t *testing.T) {
	now := time.Unix(1000, 0)
	g := New(func() time.Time { return now })

	for range burst - 1 {
		g.Trouble("h")
	}
	if r, err := g.Acquire(t.Context(), "h", 0); err != nil {
		t.Fatal(err)
	} else {
		r()
	}

	g.Trouble("h") // the burst
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.Acquire(ctx, "h", 0); err == nil {
		t.Fatal("a host in cooldown was let through")
	}

	now = now.Add(firstCooldown + time.Second)
	for range burst {
		g.Trouble("h")
	}
	g.mu.Lock()
	got := g.hosts["h"].cooldown
	g.mu.Unlock()
	if got != 2*firstCooldown {
		t.Errorf("second cooldown = %v, want %v", got, 2*firstCooldown)
	}

	for range calmAfter {
		g.Success("h")
	}
	g.mu.Lock()
	got = g.hosts["h"].cooldown
	g.mu.Unlock()
	if got != 0 {
		t.Errorf("cooldown after a calm stretch = %v, want 0", got)
	}
}

func TestTroubleOutsideTheWindowIsForgotten(t *testing.T) {
	now := time.Unix(1000, 0)
	g := New(func() time.Time { return now })
	for range burst - 1 {
		g.Trouble("h")
	}
	now = now.Add(window + time.Second)
	g.Trouble("h")
	g.mu.Lock()
	rest := g.hosts["h"].until
	g.mu.Unlock()
	if !rest.IsZero() {
		t.Errorf("old trouble counted towards a burst")
	}
}
