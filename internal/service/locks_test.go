package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestTaskLocksMutualExclusion(t *testing.T) {
	l := NewTaskLocks()
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		inside  = map[string]int{}
		maxSeen = map[string]int{}
	)
	for i := range 50 {
		id := []string{"a", "b"}[i%2]
		wg.Go(func() {
			unlock, err := l.Lock(context.Background(), id)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			inside[id]++
			maxSeen[id] = max(maxSeen[id], inside[id])
			mu.Unlock()
			time.Sleep(100 * time.Microsecond)
			mu.Lock()
			inside[id]--
			mu.Unlock()
			unlock()
		})
	}
	wg.Wait()
	if maxSeen["a"] != 1 || maxSeen["b"] != 1 {
		t.Fatalf("holders at once = %v, want 1 per key", maxSeen)
	}
	if n := l.size(); n != 0 {
		t.Fatalf("entries after all unlocks = %d, want 0", n)
	}
}

func TestTaskLocksContextCancel(t *testing.T) {
	l := NewTaskLocks()
	unlock, err := l.Lock(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := l.Lock(ctx, "x"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock while held = %v, want DeadlineExceeded", err)
	}
	// Other keys are independent.
	unlockY, err := l.Lock(context.Background(), "y")
	if err != nil {
		t.Fatal(err)
	}
	unlockY()
	unlock()
	if n := l.size(); n != 0 {
		t.Fatalf("entries = %d, want 0", n)
	}
	unlock, err = l.Lock(context.Background(), "x")
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	unlock()
}
