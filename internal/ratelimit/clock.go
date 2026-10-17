package ratelimit

import "time"

// clock abstracts time so the token-bucket logic can be tested
// deterministically. Production code uses realClock.
type clock interface {
	now() time.Time
	// newTimer returns a channel that receives once d has elapsed and a
	// function that stops the timer.
	newTimer(d time.Duration) (<-chan time.Time, func())
}

type realClock struct{}

func (realClock) now() time.Time { return time.Now() }

func (realClock) newTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}
