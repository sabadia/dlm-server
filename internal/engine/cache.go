package engine

import (
	"context"
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
)

// Default resolve-cache limits.
const (
	DefaultResolveTTL        = 5 * time.Minute
	DefaultResolveMaxEntries = 256
)

// ResolveCache keeps recent probes so a client can resolve a URL, show the
// result to the user, and create the task from it without probing again
// (which matters for one-time URLs whose response is kept open). Expired or
// evicted probes are closed.
type ResolveCache struct {
	ttl   time.Duration
	max   int
	now   func() time.Time
	newID func() string

	mu      sync.Mutex
	entries map[string]*cacheEntry
	order   []string // insertion order, oldest first
}

type cacheEntry struct {
	probe   *Probe
	expires time.Time
}

// CacheOptions configures a ResolveCache; zero values select defaults.
type CacheOptions struct {
	TTL        time.Duration
	MaxEntries int
	Clock      func() time.Time
	// NewID generates resolve ids; defaults to domain.NewID.
	NewID func() string
}

// NewResolveCache creates an empty cache.
func NewResolveCache(opts CacheOptions) *ResolveCache {
	if opts.TTL <= 0 {
		opts.TTL = DefaultResolveTTL
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultResolveMaxEntries
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.NewID == nil {
		opts.NewID = domain.NewID
	}
	return &ResolveCache{
		ttl:     opts.TTL,
		max:     opts.MaxEntries,
		now:     opts.Clock,
		newID:   opts.NewID,
		entries: make(map[string]*cacheEntry),
	}
}

// Put stores p and returns its resolve id. When the cache is full the
// oldest entry is evicted and closed.
func (c *ResolveCache) Put(p *Probe) string {
	c.mu.Lock()
	var evicted []*Probe
	evicted = append(evicted, c.expireLocked()...)
	for len(c.entries) >= c.max && len(c.order) > 0 {
		evicted = append(evicted, c.removeLocked(c.order[0]))
	}
	id := c.newID()
	c.entries[id] = &cacheEntry{probe: p, expires: c.now().Add(c.ttl)}
	c.order = append(c.order, id)
	c.mu.Unlock()
	closeAll(evicted)
	return id
}

// Peek returns the probe for id without removing it, for showing resolve
// results again. It returns an error matching domain.ErrNotFound when the
// id is unknown or expired.
func (c *ResolveCache) Peek(id string) (*Probe, error) {
	c.mu.Lock()
	evicted := c.expireLocked()
	e, ok := c.entries[id]
	c.mu.Unlock()
	closeAll(evicted)
	if !ok {
		return nil, domain.NotFound("resolve result", id)
	}
	return e.probe, nil
}

// Take removes and returns the probe for id; the caller owns it. It returns
// an error matching domain.ErrNotFound when the id is unknown or expired.
func (c *ResolveCache) Take(id string) (*Probe, error) {
	c.mu.Lock()
	evicted := c.expireLocked()
	var p *Probe
	if _, ok := c.entries[id]; ok {
		p = c.removeLocked(id)
	}
	c.mu.Unlock()
	closeAll(evicted)
	if p == nil {
		return nil, domain.NotFound("resolve result", id)
	}
	return p, nil
}

// Len returns the number of live entries.
func (c *ResolveCache) Len() int {
	c.mu.Lock()
	evicted := c.expireLocked()
	n := len(c.entries)
	c.mu.Unlock()
	closeAll(evicted)
	return n
}

// Run sweeps expired entries every interval until ctx is done, so open
// attachments are released even when nobody touches the cache.
func (c *ResolveCache) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			c.mu.Lock()
			evicted := c.expireLocked()
			c.mu.Unlock()
			closeAll(evicted)
		}
	}
}

// Close closes and removes every entry.
func (c *ResolveCache) Close() {
	c.mu.Lock()
	var all []*Probe
	for _, e := range c.entries {
		all = append(all, e.probe)
	}
	clear(c.entries)
	c.order = nil
	c.mu.Unlock()
	closeAll(all)
}

func (c *ResolveCache) expireLocked() []*Probe {
	now := c.now()
	var evicted []*Probe
	for len(c.order) > 0 {
		id := c.order[0]
		if now.Before(c.entries[id].expires) {
			break // insertion order == expiry order (constant TTL)
		}
		evicted = append(evicted, c.removeLocked(id))
	}
	return evicted
}

func (c *ResolveCache) removeLocked(id string) *Probe {
	e := c.entries[id]
	delete(c.entries, id)
	for i, oid := range c.order {
		if oid == id {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	return e.probe
}

func closeAll(probes []*Probe) {
	for _, p := range probes {
		_ = p.Close() // best effort: releasing an abandoned probe
	}
}
