package ratelimit

import (
	"context"

	"github.com/sabadia/dlm-server/internal/engine"
)

// chainChunk caps the amount a chain requests from its buckets at a time.
const chainChunk = 64 << 10

// Chain returns a limiter that makes every byte pass each non-nil bucket,
// in order (conventionally task, queue, global). The effective rate is the
// minimum of the bucket limits; unlimited buckets are skipped at almost no
// cost. Limits are read at wait time, so live SetLimit calls apply.
//
// Buckets are waited on sequentially, one step at a time, rather than
// reserved atomically across all of them. A step is at most the smallest
// burst of the limited buckets (and at most 64 KiB), so every bucket refills
// while the chain waits on the others and the waits overlap: the rate
// converges to the minimum limit instead of degrading towards the harmonic
// sum of the limits. The trade-off is that tokens taken from an earlier
// bucket are held while waiting on a later one. This never blocks other
// users of the later bucket (they compete for it normally); it only lets a
// shared earlier bucket hand out at most one step per waiter ahead of the
// actual transfer.
func Chain(buckets ...*Bucket) engine.Limiter {
	c := make(chain, 0, len(buckets))
	for _, b := range buckets {
		if b != nil {
			c = append(c, b)
		}
	}
	return c
}

type chain []*Bucket

// WaitN implements engine.Limiter.
func (c chain) WaitN(ctx context.Context, n int) error {
	for n > 0 {
		step, limited := c.step()
		if !limited {
			return nil
		}
		step = min(n, step)
		for _, b := range c {
			if err := b.WaitN(ctx, step); err != nil {
				return err
			}
		}
		n -= step
	}
	return nil
}

// step returns the largest request that fits every limited bucket's burst
// (capped at chainChunk) and whether any bucket is limited at all.
func (c chain) step() (int, bool) {
	step, limited := chainChunk, false
	for _, b := range c {
		// A concurrent SetLimit only makes this a stale size hint, which
		// Bucket.WaitN tolerates by splitting.
		if burst := b.burst(); burst > 0 {
			limited = true
			step = min(step, burst)
		}
	}
	return step, limited
}

var (
	_ engine.Limiter = (*Bucket)(nil)
	_ engine.Limiter = chain(nil)
)
