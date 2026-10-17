package ratelimit

import (
	"fmt"
	"sync"
	"testing"
)

func TestManagerBuckets(t *testing.T) {
	m := NewManager(1000)
	if got := m.Global().Limit(); got != 1000 {
		t.Fatalf("global limit = %d, want 1000", got)
	}
	m.SetGlobal(0)
	if got := m.Global().Limit(); got != 0 {
		t.Fatalf("global limit = %d, want 0", got)
	}

	q := m.Queue("q")
	if q.Limit() != 0 {
		t.Fatalf("new queue limit = %d, want 0", q.Limit())
	}
	if m.Queue("q") != q {
		t.Fatal("Queue returned a different bucket for the same id")
	}
	m.SetQueue("q", 500)
	if q.Limit() != 500 {
		t.Fatalf("queue limit = %d, want 500", q.Limit())
	}

	m.SetTask("t", 200) // creates on demand
	tb := m.Task("t")
	if tb.Limit() != 200 {
		t.Fatalf("task limit = %d, want 200", tb.Limit())
	}
	if m.Task("other") == tb {
		t.Fatal("different task ids share a bucket")
	}

	m.RemoveTask("t")
	m.RemoveTask("other")
	m.RemoveTask("missing") // no-op
	m.RemoveQueue("q")
	if len(m.tasks) != 0 || len(m.queues) != 0 {
		t.Fatalf("buckets leaked: %d tasks, %d queues", len(m.tasks), len(m.queues))
	}
	if fresh := m.Task("t"); fresh == tb || fresh.Limit() != 0 {
		t.Fatal("Task after RemoveTask did not create a fresh unlimited bucket")
	}
}

func TestManagerForTask(t *testing.T) {
	m := NewManager(0)
	tests := []struct {
		name          string
		task, queue   string
		wantBucketsFn func() []*Bucket
	}{
		{"task queue global", "t", "q", func() []*Bucket { return []*Bucket{m.Task("t"), m.Queue("q"), m.Global()} }},
		{"no queue", "t", "", func() []*Bucket { return []*Bucket{m.Task("t"), m.Global()} }},
		{"global only", "", "", func() []*Bucket { return []*Bucket{m.Global()} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, ok := m.ForTask(tt.task, tt.queue).(chain)
			if !ok {
				t.Fatalf("ForTask returned %T, want chain", m.ForTask(tt.task, tt.queue))
			}
			want := tt.wantBucketsFn()
			if len(c) != len(want) {
				t.Fatalf("chain has %d buckets, want %d", len(c), len(want))
			}
			for i := range want {
				if c[i] != want[i] {
					t.Fatalf("bucket %d differs from the expected level", i)
				}
			}
		})
	}
}

func TestManagerConcurrent(t *testing.T) {
	m := NewManager(1 << 30)
	const (
		workers = 32
		iters   = 200
	)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Mutator: live limit changes and bucket churn while transfers run.
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			limit := int64(0)
			if i%2 == 0 {
				limit = 1 << 30
			}
			m.SetGlobal(limit)
			m.SetQueue(fmt.Sprint("q", i%3), limit)
			m.SetTask(fmt.Sprint("t", i%10), limit)
			m.RemoveTask(fmt.Sprint("t", (i+5)%10))
			m.RemoveQueue(fmt.Sprint("q", (i+1)%3))
			_ = m.Global().Limit()
		}
	})

	var workersWG sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		workersWG.Go(func() {
			for i := range iters {
				l := m.ForTask(fmt.Sprint("t", (w+i)%10), fmt.Sprint("q", w%3))
				if err := l.WaitN(t.Context(), 4096); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	workersWG.Wait()
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
