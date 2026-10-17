package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"golang.org/x/time/rate"
)

const (
	// smallBurst is the burst used for mid-range limits; it fits a couple of
	// typical 32 KiB read buffers so ordinary reads are not split.
	smallBurst = 64 << 10
	// maxBurst caps the burst of very fast limits so a long idle period
	// never releases more than this many bytes at once.
	maxBurst = 4 << 20
)

// burstFor returns the bucket size used for a limit of bytesPerSec (> 0):
//
//   - limit <= 64 KiB/s: one second worth of bytes (the limit itself), so
//     slow limits stay accurate;
//   - otherwise max(limit/4, 64 KiB), i.e. at most a quarter second worth
//     once the limit exceeds 256 KiB/s, capped at 4 MiB.
//
// The burst bounds how far a transfer may run ahead of the configured rate
// after being idle and is also the largest amount reserved in one step;
// larger WaitN requests are split into burst-sized chunks.
func burstFor(bytesPerSec int64) int {
	switch {
	case bytesPerSec <= smallBurst:
		return int(max(bytesPerSec, 1))
	case bytesPerSec/4 <= smallBurst:
		return smallBurst
	default:
		return int(min(bytesPerSec/4, maxBurst))
	}
}

// Bucket is a token bucket limiting one scope (a task, a queue or the
// whole daemon) to a number of bytes per second. The zero value is not
// usable; create buckets with NewBucket. A Bucket is safe for concurrent
// use and implements engine.Limiter.
type Bucket struct {
	clk clock

	// fast mirrors limit for a lock-free unlimited fast path.
	fast atomic.Int64

	// lim is the token bucket of the current limited period. It is
	// replaced (never mutated back) when the bucket goes from unlimited to
	// limited, so it may be loaded without holding mu; nil while unlimited
	// since creation.
	lim atomic.Pointer[rate.Limiter]

	mu    sync.Mutex
	limit int64 // bytes per second, 0 = unlimited
	// changed is closed and replaced whenever the limit changes so that
	// blocked waiters re-evaluate their reservation.
	changed chan struct{}
}

// NewBucket returns a bucket limited to bytesPerSec; 0 (or a negative
// value) means unlimited. The bucket starts full.
func NewBucket(bytesPerSec int64) *Bucket {
	return newBucket(bytesPerSec, realClock{})
}

func newBucket(bytesPerSec int64, clk clock) *Bucket {
	bytesPerSec = max(bytesPerSec, 0)
	b := &Bucket{
		clk:     clk,
		limit:   bytesPerSec,
		changed: make(chan struct{}),
	}
	if bytesPerSec > 0 {
		b.lim.Store(rate.NewLimiter(rate.Limit(bytesPerSec), burstFor(bytesPerSec)))
	}
	b.fast.Store(bytesPerSec)
	return b
}

// Limit returns the current limit in bytes per second; 0 means unlimited.
func (b *Bucket) Limit() int64 {
	return b.fast.Load()
}

// SetLimit changes the limit to bytesPerSec (0 or negative = unlimited).
// The change applies immediately, including to goroutines already blocked
// in WaitN: they give back their pending reservation and re-evaluate it
// under the new limit, so raising or removing a limit releases them
// promptly. Changing between two limits keeps the tokens accumulated so far
// (capped at the new burst); a previously unlimited bucket starts full,
// like a new one.
func (b *Bucket) SetLimit(bytesPerSec int64) {
	bytesPerSec = max(bytesPerSec, 0)
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytesPerSec == b.limit {
		return
	}
	switch {
	case bytesPerSec == 0:
		// Keep lim: waiters cancel their reservations against it.
	case b.limit == 0:
		b.lim.Store(rate.NewLimiter(rate.Limit(bytesPerSec), burstFor(bytesPerSec)))
	default:
		now := b.clk.now()
		lim := b.lim.Load()
		lim.SetLimitAt(now, rate.Limit(bytesPerSec))
		lim.SetBurstAt(now, burstFor(bytesPerSec))
	}
	b.limit = bytesPerSec
	b.fast.Store(bytesPerSec)
	close(b.changed)
	b.changed = make(chan struct{})
}

// WaitN blocks until n bytes may be transferred, or ctx is done. Requests
// larger than the burst are split into burst-sized chunks; chunks already
// granted when ctx is cancelled are not given back. An unlimited bucket
// returns immediately without consulting ctx.
func (b *Bucket) WaitN(ctx context.Context, n int) error {
	for n > 0 {
		if b.fast.Load() == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("ratelimit: wait: %w", err)
		}
		granted, err := b.waitChunk(ctx, n)
		if err != nil {
			return err
		}
		n -= granted
	}
	return nil
}

// burst returns the current burst, or 0 while the bucket is unlimited.
func (b *Bucket) burst() int {
	if b.fast.Load() == 0 {
		return 0
	}
	if lim := b.lim.Load(); lim != nil {
		return lim.Burst()
	}
	return 0
}

// waitChunk reserves and waits for up to n bytes (at most one burst) and
// returns how many bytes were granted. It returns 0 without error when the
// limit changed while waiting, so the caller retries under the new limit.
func (b *Bucket) waitChunk(ctx context.Context, n int) (int, error) {
	b.mu.Lock()
	if b.limit == 0 {
		b.mu.Unlock()
		return n, nil
	}
	// Holding mu keeps limit, burst and the reservation consistent with
	// each other: chunk <= burst guarantees the reservation is OK.
	lim := b.lim.Load()
	chunk := min(n, lim.Burst())
	now := b.clk.now()
	r := lim.ReserveN(now, chunk)
	changed := b.changed
	b.mu.Unlock()

	delay := r.DelayFrom(now)
	if delay <= 0 {
		return chunk, nil
	}
	timer, stop := b.clk.newTimer(delay)
	defer stop()
	select {
	case <-timer:
		return chunk, nil
	case <-changed:
		r.CancelAt(b.clk.now())
		return 0, nil
	case <-ctx.Done():
		r.CancelAt(b.clk.now())
		return 0, fmt.Errorf("ratelimit: wait: %w", ctx.Err())
	}
}
