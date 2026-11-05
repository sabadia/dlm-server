// Package hostgate limits how many requests are in flight to one host across
// all downloads, and lets a host that is refusing or resetting connections
// rest for a while.
//
// Sites that serve many small files (HLS and DASH segments) allow only so many
// connections from one address. Thirty tasks with sixteen workers each ask for
// far more, and the answer is resets, handshake timeouts and 429s. The gate
// makes them take turns instead.
package hostgate

import (
	"context"
	"sync"
	"time"
)

const (
	// burst is how many trouble reports within window start a cooldown.
	burst  = 3
	window = 10 * time.Second
	// firstCooldown, maxCooldown bound the rest a host gets; it doubles each
	// time trouble comes back before the host has been calm for calmAfter
	// successful requests.
	firstCooldown = 2 * time.Second
	maxCooldown   = 2 * time.Minute
	calmAfter     = 20
)

// Gate is safe for concurrent use. The zero value is not ready; use New.
type Gate struct {
	now func() time.Time

	mu    sync.Mutex
	hosts map[string]*host
}

type host struct {
	active   int
	wake     chan struct{} // closed, and replaced, whenever a slot is freed
	trouble  []time.Time
	cooldown time.Duration
	until    time.Time
	calm     int
}

// New returns a Gate. now defaults to time.Now.
func New(now func() time.Time) *Gate {
	if now == nil {
		now = time.Now
	}
	return &Gate{now: now, hosts: map[string]*host{}}
}

// Default is the gate every download shares.
var Default = New(nil)

func (g *Gate) host(name string) *host {
	h := g.hosts[name]
	if h == nil {
		h = &host{wake: make(chan struct{})}
		g.hosts[name] = h
	}
	return h
}

// Acquire waits until fewer than limit requests to name are in flight and the
// host is not resting, then counts one more. The returned function gives the
// slot back; call it exactly once. A limit of 0 or less means no cap, but a
// resting host is still waited for.
func (g *Gate) Acquire(ctx context.Context, name string, limit int) (release func(), err error) {
	for {
		g.mu.Lock()
		h := g.host(name)
		var rest time.Duration
		if now := g.now(); now.Before(h.until) {
			rest = h.until.Sub(now)
		}
		if rest == 0 && (limit <= 0 || h.active < limit) {
			h.active++
			g.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { g.release(name) }) }, nil
		}
		wake := h.wake
		g.mu.Unlock()

		if rest > 0 {
			t := time.NewTimer(rest)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err() //nolint:wrapcheck // cancellation is reported as is
			}
			continue
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		}
	}
}

func (g *Gate) release(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.host(name)
	h.active--
	close(h.wake)
	h.wake = make(chan struct{})
}

// Trouble reports that a request to name was refused, reset or timed out. A
// few of them close together make the host rest, longer each time.
func (g *Gate) Trouble(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.host(name)
	now := g.now()
	h.calm = 0
	recent := make([]time.Time, 0, burst)
	for _, t := range h.trouble {
		if now.Sub(t) < window {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	h.trouble = recent
	if len(recent) < burst {
		return
	}
	h.trouble = nil
	if h.cooldown == 0 {
		h.cooldown = firstCooldown
	} else {
		h.cooldown = min(h.cooldown*2, maxCooldown)
	}
	h.until = now.Add(h.cooldown)
}

// Success reports a request that worked. After a calm stretch the host's
// cooldown starts over from the shortest.
func (g *Gate) Success(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.host(name)
	h.calm++
	if h.calm >= calmAfter {
		h.cooldown = 0
		h.calm = 0
	}
}
