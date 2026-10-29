package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// DefaultRingSize is how many records the daemon keeps in memory for the logs
// API.
const DefaultRingSize = 5000

// Record is one log record as the logs API shows it.
type Record struct {
	// ID counts records from 1 since the daemon started.
	ID      uint64    `json:"id"`
	Time    time.Time `json:"time"`
	Level   string    `json:"level" enum:"debug,info,warn,error"`
	Message string    `json:"message"`
	// Attrs are the record's attributes (groups joined with dots).
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Filter selects records. The zero Filter matches everything that was kept.
type Filter struct {
	// MinLevel, when set, keeps records of at least this level.
	MinLevel *slog.Level
	// Query keeps records whose message or attribute values contain it,
	// ignoring case.
	Query string
	// Since and Until bound the time (inclusive); zero means unbounded.
	Since, Until time.Time
}

// Match reports whether r satisfies the filter.
func (f Filter) Match(r *Record) bool {
	if f.MinLevel != nil && levelOf(r.Level) < *f.MinLevel {
		return false
	}
	if !f.Since.IsZero() && r.Time.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && r.Time.After(f.Until) {
		return false
	}
	if f.Query != "" {
		q := strings.ToLower(f.Query)
		if strings.Contains(strings.ToLower(r.Message), q) {
			return true
		}
		for k, v := range r.Attrs {
			if strings.Contains(strings.ToLower(k), q) || strings.Contains(strings.ToLower(fmt.Sprint(v)), q) {
				return true
			}
		}
		return false
	}
	return true
}

func levelOf(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

// Ring keeps the most recent log records in memory and hands new ones to
// subscribers. It is safe for concurrent use.
type Ring struct {
	mu   sync.Mutex
	recs []Record // circular
	size int
	next uint64 // ID of the next record
	subs map[*Subscription]struct{}
}

// NewRing returns a ring that keeps the last size records.
func NewRing(size int) *Ring {
	if size <= 0 {
		size = DefaultRingSize
	}
	return &Ring{size: size, next: 1, subs: make(map[*Subscription]struct{})}
}

// Add stores a record, gives it its ID and delivers it to the subscribers
// whose filter matches. It never blocks: a subscriber that cannot keep up
// loses records (see Subscription.Dropped).
func (r *Ring) Add(rec Record) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec.ID = r.next
	r.next++
	if len(r.recs) < r.size {
		r.recs = append(r.recs, rec)
	} else {
		r.recs[r.slot(rec.ID)] = rec
	}
	for s := range r.subs {
		if !s.filter.Match(&rec) {
			continue
		}
		select {
		case s.ch <- rec:
		default:
			s.dropped.Add(1)
		}
	}
	return rec.ID
}

// at returns the record with the given ID, which must be kept.
func (r *Ring) at(id uint64) Record { return r.recs[r.slot(id)] }

// slot is the position of record id in the circular buffer.
func (r *Ring) slot(id uint64) int { return int((id - 1) % uint64(r.size)) } //nolint:gosec // below r.size, an int

// oldest is the ID of the oldest kept record (next when empty).
func (r *Ring) oldest() uint64 {
	if kept := uint64(len(r.recs)); r.next-1 > kept {
		return r.next - kept
	}
	return 1
}

// Query returns up to limit of the newest records that match f and have an ID
// below before (0: no bound), oldest first, and whether older matching
// records exist.
func (r *Ring) Query(f Filter, before uint64, limit int) (recs []Record, more bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 {
		limit = 200
	}
	top := r.next - 1
	if before > 0 && before-1 < top {
		top = before - 1
	}
	lo := r.oldest()
	var rev []Record
	for id := top; id >= lo && id > 0; id-- {
		rec := r.at(id)
		if !f.Match(&rec) {
			continue
		}
		if len(rev) == limit {
			more = true
			break
		}
		rev = append(rev, rec)
	}
	for i := len(rev) - 1; i >= 0; i-- {
		recs = append(recs, rev[i])
	}
	return recs, more
}

// Since returns the matching records with an ID above after, oldest first;
// complete is false when records after that ID have already been dropped from
// the ring (a client resuming a stream then missed some).
func (r *Ring) Since(after uint64, f Filter) (recs []Record, complete bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lo := r.oldest()
	complete = after+1 >= lo || r.next == 1
	for id := max(after+1, lo); id < r.next; id++ {
		rec := r.at(id)
		if f.Match(&rec) {
			recs = append(recs, rec)
		}
	}
	return recs, complete
}

// LastID is the ID of the newest record (0 when there is none).
func (r *Ring) LastID() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next - 1
}

// Subscription delivers new records.
type Subscription struct {
	ring    *Ring
	filter  Filter
	ch      chan Record
	dropped atomicCounter
}

// Subscribe returns a subscription for the records matching f that are added
// from now on, until ctx is done or Unsubscribe is called.
func (r *Ring) Subscribe(ctx context.Context, f Filter) *Subscription {
	s := &Subscription{ring: r, filter: f, ch: make(chan Record, 256)}
	r.mu.Lock()
	r.subs[s] = struct{}{}
	r.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.Unsubscribe()
	}()
	return s
}

// C is the channel of new records; it is not closed.
func (s *Subscription) C() <-chan Record { return s.ch }

// Dropped is how many records were lost because the reader was too slow.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Unsubscribe stops the delivery.
func (s *Subscription) Unsubscribe() {
	s.ring.mu.Lock()
	delete(s.ring.subs, s)
	s.ring.mu.Unlock()
}

// teeHandler passes records to inner and keeps them in the ring.
type teeHandler struct {
	inner  slog.Handler
	ring   *Ring
	attrs  []slog.Attr // attributes added with WithAttrs, already prefixed
	prefix string      // group path with a trailing dot
	// all keeps records of every level; inner still only gets those it accepts.
	all bool
}

// NewTeeHandler returns a handler that writes to inner and also keeps every
// record inner accepts in ring.
func NewTeeHandler(inner slog.Handler, ring *Ring) slog.Handler {
	return &teeHandler{inner: inner, ring: ring}
}

// NewCaptureHandler is like NewTeeHandler but keeps records of every level in
// ring, whatever level inner is set to (inner still only writes what it
// accepts). The plugin logs use it: a plugin author wants to see debug output
// without turning the whole daemon to debug.
func NewCaptureHandler(inner slog.Handler, ring *Ring) slog.Handler {
	return &teeHandler{inner: inner, ring: ring, all: true}
}

func (h *teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.all || h.inner.Enabled(ctx, l)
}

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := map[string]any{}
	for _, a := range h.attrs {
		addAttr(attrs, "", a)
	}
	r.Attrs(func(a slog.Attr) bool {
		addAttr(attrs, h.prefix, a)
		return true
	})
	rec := Record{Time: r.Time, Level: strings.ToLower(r.Level.String()), Message: r.Message}
	if len(attrs) > 0 {
		rec.Attrs = attrs
	}
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}
	h.ring.Add(rec)
	if h.all && !h.inner.Enabled(ctx, r.Level) {
		return nil
	}
	return h.inner.Handle(ctx, r) //nolint:wrapcheck // the inner handler's error is the handler's
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.inner = h.inner.WithAttrs(attrs)
	nh.attrs = append([]slog.Attr{}, h.attrs...)
	for _, a := range attrs {
		nh.attrs = append(nh.attrs, slog.Attr{Key: h.prefix + a.Key, Value: a.Value})
	}
	return &nh
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := *h
	nh.inner = h.inner.WithGroup(name)
	nh.prefix = h.prefix + name + "."
	return &nh
}

// addAttr flattens a (and its groups) into out under dotted keys, in a form
// that encodes as JSON.
func addAttr(out map[string]any, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			addAttr(out, p, ga)
		}
		return
	}
	out[prefix+a.Key] = jsonValue(a.Value)
}

func jsonValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339Nano)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return err.Error()
		}
		return fmt.Sprint(v.Any())
	default:
		return v.String()
	}
}
