package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

type recordingLimiter struct {
	mu  sync.Mutex
	ns  []int
	err error
}

func (r *recordingLimiter) WaitN(_ context.Context, n int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ns = append(r.ns, n)
	return r.err
}

func TestReaderNilLimiter(t *testing.T) {
	src := strings.NewReader("x")
	if got := Reader(t.Context(), src, nil); got != io.Reader(src) {
		t.Fatal("nil limiter should return the reader unchanged")
	}
}

func TestReaderChargesBytesRead(t *testing.T) {
	data := strings.Repeat("abcdefghij", 10)
	l := &recordingLimiter{}
	// HalfReader returns short reads; each must be charged exactly.
	r := Reader(t.Context(), iotest.HalfReader(strings.NewReader(data)), l)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != data {
		t.Fatal("data mismatch")
	}
	sum := 0
	for _, n := range l.ns {
		if n <= 0 {
			t.Fatalf("charged %d bytes", n)
		}
		sum += n
	}
	if sum != len(data) {
		t.Fatalf("charged %d bytes, read %d", sum, len(data))
	}
	if err := iotest.TestReader(Reader(t.Context(), strings.NewReader(data), &recordingLimiter{}), []byte(data)); err != nil {
		t.Fatal(err)
	}
}

func TestReaderWaitError(t *testing.T) {
	werr := fmt.Errorf("ratelimit: wait: %w", context.Canceled)
	r := Reader(t.Context(), strings.NewReader("hello"), &recordingLimiter{err: werr})
	buf := make([]byte, 8)
	n, err := r.Read(buf)
	if n != 5 || !errors.Is(err, context.Canceled) {
		t.Fatalf("Read = %d, %v; want 5, context.Canceled", n, err)
	}
}

// Throughput tests use the real clock. Timers never fire early, so the
// lower bound is tight; the upper bound is generous for slow CI runners.

func TestThroughputBucket(t *testing.T) {
	const limit = 256 << 10 // burst 64 KiB
	b := NewBucket(limit)
	data := make([]byte, 64<<10+limit/2) // burst + 0.5s worth
	start := time.Now()
	n, err := io.Copy(io.Discard, Reader(t.Context(), bytes.NewReader(data), b))
	if err != nil || n != int64(len(data)) {
		t.Fatalf("copy = %d, %v", n, err)
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("elapsed = %v, want ~500ms", elapsed)
	}
}

func TestThroughputSharedQueue(t *testing.T) {
	m := NewManager(1 << 20)
	m.SetQueue("q", 256<<10) // burst 64 KiB; the slowest level
	const workers = 4
	per := (64<<10 + 128<<10) / workers // burst + 0.5s worth, shared
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		wg.Go(func() {
			r := Reader(t.Context(), bytes.NewReader(make([]byte, per)), m.ForTask(fmt.Sprint("t", w), "q"))
			if _, err := io.Copy(io.Discard, r); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("elapsed = %v, want ~500ms", elapsed)
	}
}
