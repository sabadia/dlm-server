// Package retry implements the bounded retry policy used by the download
// engines: exponential backoff with full jitter, optional honouring of
// server-requested delays (Retry-After) and a hard limit on attempts.
//
// Only errors classified as engine.KindTransient are retried. Fatal errors
// fail immediately, link-expired errors are handed to the link-refresh layer
// and cancellation (pause / shutdown) stops at once.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/settings"
)

// Defaults used when a Policy field (or the corresponding setting) is unset.
const (
	DefaultMaxAttempts    = 5
	DefaultInitialBackoff = time.Second
	DefaultMaxBackoff     = time.Minute
	DefaultMaxRetryAfter  = 10 * time.Minute
)

// Policy describes how failures are retried. The zero value is usable: it
// never retries (MaxAttempts 0) and uses the default backoff bounds.
type Policy struct {
	// MaxAttempts is the number of retries after the first failure.
	MaxAttempts int
	// InitialBackoff is the backoff ceiling before the first retry; it
	// doubles for every following retry.
	InitialBackoff time.Duration
	// MaxBackoff caps the exponential backoff.
	MaxBackoff time.Duration
	// RespectRetryAfter makes Delay honour a server-requested delay carried
	// by the error (engine.Error.RetryAfter).
	RespectRetryAfter bool
	// MaxRetryAfter caps server-requested delays so a hostile or broken
	// server cannot stall a task indefinitely.
	MaxRetryAfter time.Duration
	// Rand returns a pseudo-random number in [0, 1) used for jitter. Nil
	// uses math/rand/v2. Tests inject a deterministic source.
	Rand func() float64
}

// FromSettings builds a Policy from resolved retry settings, filling the
// package defaults for unset fields.
func FromSettings(s settings.RetrySettings) Policy {
	return Policy{
		MaxAttempts:       s.MaxAttempts.OrElse(DefaultMaxAttempts),
		InitialBackoff:    s.InitialBackoff.OrElse(settings.Duration(DefaultInitialBackoff)).Std(),
		MaxBackoff:        s.MaxBackoff.OrElse(settings.Duration(DefaultMaxBackoff)).Std(),
		RespectRetryAfter: s.RespectRetryAfter.OrElse(true),
		MaxRetryAfter:     DefaultMaxRetryAfter,
	}
}

// normalized returns p with non-positive bounds replaced by defaults.
func (p Policy) normalized() Policy {
	if p.MaxAttempts < 0 {
		p.MaxAttempts = 0
	}
	if p.InitialBackoff <= 0 {
		p.InitialBackoff = DefaultInitialBackoff
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = DefaultMaxBackoff
	}
	if p.MaxBackoff < p.InitialBackoff {
		p.MaxBackoff = p.InitialBackoff
	}
	if p.MaxRetryAfter <= 0 {
		p.MaxRetryAfter = DefaultMaxRetryAfter
	}
	if p.Rand == nil {
		p.Rand = rand.Float64 //nolint:gosec // jitter does not need a cryptographic source
	}
	return p
}

// ceiling returns min(InitialBackoff * 2^(attempt-1), MaxBackoff) without
// overflowing for large attempts. p must be normalized.
func (p Policy) ceiling(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	// 2^62 already exceeds any representable backoff; also guards the shift.
	if shift >= 62 || p.InitialBackoff > p.MaxBackoff>>uint(shift) {
		return p.MaxBackoff
	}
	return p.InitialBackoff << uint(shift)
}

// Backoff returns the delay before retry number attempt (1-based): a random
// duration in [0, min(InitialBackoff*2^(attempt-1), MaxBackoff)] ("full
// jitter"). Attempts below 1 are treated as 1.
func (p Policy) Backoff(attempt int) time.Duration {
	p = p.normalized()
	d := p.ceiling(attempt)
	r := p.Rand()
	if r < 0 || math.IsNaN(r) {
		r = 0
	}
	if r > 1 {
		r = 1
	}
	return time.Duration(float64(d) * r)
}

// Delay returns the wait before retry number attempt after err. When
// RespectRetryAfter is set and err carries a positive RetryAfter, the
// server-requested delay (capped at MaxRetryAfter) is used unless the
// jittered backoff is longer; otherwise it is Backoff(attempt).
func (p Policy) Delay(attempt int, err error) time.Duration {
	p = p.normalized()
	b := p.Backoff(attempt)
	if !p.RespectRetryAfter {
		return b
	}
	var e *engine.Error
	if !errors.As(err, &e) || e.RetryAfter <= 0 {
		return b
	}
	return max(min(e.RetryAfter, p.MaxRetryAfter), b)
}

// Sleeper waits for d or until ctx is done, returning ctx.Err() in the
// latter case.
type Sleeper func(ctx context.Context, d time.Duration) error

// Sleep is the default Sleeper, backed by a timer.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err() //nolint:wrapcheck // Sleeper contract: return ctx.Err() as is
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // Sleeper contract: return ctx.Err() as is
	case <-t.C:
		return nil
	}
}

type options struct {
	onRetry  func(attempt int, delay time.Duration, err error)
	sleep    Sleeper
	progress func() bool
}

// Option configures Do.
type Option func(*options)

// OnRetry registers a hook called before every wait, with the retry number
// (1-based, reset by ResetOnProgress), the delay and the error being retried.
// Use it for logging and events.
func OnRetry(fn func(attempt int, delay time.Duration, err error)) Option {
	return func(o *options) { o.onRetry = fn }
}

// WithSleeper replaces the timer-based wait (tests inject a fake).
func WithSleeper(s Sleeper) Option {
	return func(o *options) { o.sleep = s }
}

// ResetOnProgress makes long downloads tolerate occasional failures: after
// each transient failure Do calls progressed, and when it reports true
// (bytes were transferred since the previous failure) the retry counter and
// the backoff restart from the beginning. Progress.Moved is a ready-made
// implementation.
func ResetOnProgress(progressed func() bool) Option {
	return func(o *options) { o.progress = progressed }
}

// Do runs op until it succeeds, fails with a non-transient error, the retry
// budget (p.MaxAttempts retries after the first failure) is exhausted or ctx
// is done. attempt passed to op counts every call, starting at 1.
//
// The returned error wraps op's last error, so engine.Classify and errors.As
// still see the *engine.Error. If ctx is done (during op or while waiting),
// Do returns an error wrapping ctx.Err() only; the last op error is kept in
// the message. Callers should run op's errors through a classifier (e.g.
// httpx.Classify) so that retryable failures are marked transient.
func Do(ctx context.Context, p Policy, op func(ctx context.Context, attempt int) error, opts ...Option) error {
	p = p.normalized()
	o := options{sleep: Sleep}
	for _, opt := range opts {
		opt(&o)
	}

	retries := 0
	for attempt := 1; ; attempt++ {
		err := op(ctx, attempt)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("retry: attempt %d: %w (last error: %s)", attempt, ctxErr, err.Error())
		}
		if engine.Classify(err) != engine.KindTransient {
			return fmt.Errorf("retry: attempt %d: %w", attempt, err)
		}
		if o.progress != nil && o.progress() {
			retries = 0
		}
		if retries >= p.MaxAttempts {
			return fmt.Errorf("retry: giving up after %d attempts: %w", attempt, err)
		}
		retries++
		delay := p.Delay(retries, err)
		if o.onRetry != nil {
			o.onRetry(retries, delay, err)
		}
		if serr := o.sleep(ctx, delay); serr != nil {
			return fmt.Errorf("retry: waiting after attempt %d: %w (last error: %s)", attempt, serr, err.Error())
		}
	}
}

// Progress counts transferred bytes so a retry loop can tell whether an
// attempt made progress. It is safe for concurrent use; the zero value is
// ready.
type Progress struct {
	n, seen atomic.Int64
}

// Add records n transferred bytes.
func (p *Progress) Add(n int64) { p.n.Add(n) }

// Moved reports whether bytes were added since the previous call to Moved.
// Pass it to ResetOnProgress.
func (p *Progress) Moved() bool {
	n := p.n.Load()
	return p.seen.Swap(n) != n
}
