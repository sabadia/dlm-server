package events

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Default option values used by New when the corresponding field is zero.
const (
	DefaultReplaySize       = 1024
	DefaultSubscriberBuffer = 256
	DefaultProgressInterval = 500 * time.Millisecond
)

// Options configures a Bus. Zero values select the defaults.
type Options struct {
	// Clock returns the current time; defaults to time.Now. Event times are
	// stored in UTC.
	Clock func() time.Time
	// ReplaySize is the number of recent events kept for Since.
	ReplaySize int
	// SubscriberBuffer is the channel capacity of each subscription.
	SubscriberBuffer int
	// ProgressInterval is how often coalesced progress events are flushed.
	ProgressInterval time.Duration
	// Logger receives diagnostic messages; nil discards them.
	Logger *slog.Logger
}

// tickerFunc creates a ticker; it returns the tick channel and a stop function.
// It exists so tests can drive the flush loop without real time.
type tickerFunc func(d time.Duration) (<-chan time.Time, func())

func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Bus is a multi-subscriber, non-blocking event bus with a replay buffer and
// per-task coalescing of progress events.
//
// Publish never blocks: events are written into each matching subscriber's
// buffered channel while the bus lock is held, and when a subscriber's buffer
// is full its oldest queued event is dropped to make room.
//
// Coalesced progress events are flushed by Run (every ProgressInterval), by
// any non-progress event for the same task, and by Close.
type Bus struct {
	clock            func() time.Time
	subBuffer        int
	progressInterval time.Duration
	log              *slog.Logger
	newTicker        tickerFunc

	mu      sync.Mutex
	closed  bool
	lastID  uint64
	ring    []Event // circular replay buffer, capacity = ReplaySize
	head    int     // index of the oldest event in ring
	size    int     // number of valid events in ring
	evicted uint64  // highest ID evicted from ring (0 = none)
	subs    map[*Subscription]struct{}
	pending map[string]Event // latest undelivered progress per TaskID
	order   []string         // TaskIDs in pending, in first-seen order

	done chan struct{} // closed by Close; stops Run
}

// New creates a Bus. Call Run to enable periodic progress flushing and Close
// to release subscribers.
func New(opts Options) *Bus {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.ReplaySize <= 0 {
		opts.ReplaySize = DefaultReplaySize
	}
	if opts.SubscriberBuffer <= 0 {
		opts.SubscriberBuffer = DefaultSubscriberBuffer
	}
	if opts.ProgressInterval <= 0 {
		opts.ProgressInterval = DefaultProgressInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Bus{
		clock:            opts.Clock,
		subBuffer:        opts.SubscriberBuffer,
		progressInterval: opts.ProgressInterval,
		log:              opts.Logger,
		newTicker:        realTicker,
		ring:             make([]Event, opts.ReplaySize),
		subs:             make(map[*Subscription]struct{}),
		pending:          make(map[string]Event),
		done:             make(chan struct{}),
	}
}

// Run flushes coalesced progress events every ProgressInterval until ctx is
// done or the bus is closed. It returns nil on either condition. Run must be
// called by an owner that waits for it to return.
func (b *Bus) Run(ctx context.Context) error {
	tick, stop := b.newTicker(b.progressInterval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-b.done:
			return nil
		case <-tick:
			b.flush()
		}
	}
}

// Close flushes pending progress events, closes every subscription channel
// and stops Run. It is idempotent. Publish after Close is a no-op.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.flushLocked()
	b.closed = true
	for s := range b.subs {
		b.removeLocked(s)
	}
	close(b.done)
}

// Publish assigns the event an ID and timestamp, stores it in the replay
// buffer and delivers it to all matching subscribers, returning the stored
// event. It never blocks on subscribers.
//
// Events of type TypeTaskProgress are coalesced instead: only the latest one
// per TaskID is kept and delivered on the next flush, and Publish returns the
// zero Event (the ID is assigned at flush time). Publish on a closed bus
// returns the zero Event.
func (b *Bus) Publish(e Event) Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return Event{}
	}
	if e.Type == TypeTaskProgress {
		if _, ok := b.pending[e.TaskID]; !ok {
			b.order = append(b.order, e.TaskID)
		}
		b.pending[e.TaskID] = e
		return Event{}
	}
	if e.TaskID != "" {
		// Keep per-task ordering sane: progress precedes the status change.
		if p, ok := b.pending[e.TaskID]; ok {
			b.removePendingLocked(e.TaskID)
			b.deliverLocked(p)
		}
	}
	return b.deliverLocked(e)
}

// Subscribe registers a subscription receiving events matching f. It ends
// (its channel is closed) when ctx is done, Unsubscribe is called, or the bus
// is closed. Subscribing to a closed bus returns an already-closed
// subscription.
func (b *Bus) Subscribe(ctx context.Context, f Filter) *Subscription {
	f.Types = slices.Clone(f.Types) // the caller may reuse its slice
	s := &Subscription{
		bus:    b,
		filter: f,
		ch:     make(chan Event, b.subBuffer),
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(s.ch)
		return s
	}
	defer b.mu.Unlock()
	b.subs[s] = struct{}{}
	// AfterFunc starts no goroutine until ctx is done (if ctx is already done
	// its goroutine blocks on b.mu until we return). removeLocked stops it so
	// a long-lived ctx does not retain ended subscriptions.
	s.stopCtx = context.AfterFunc(ctx, s.Unsubscribe)
	return s
}

// Since returns the buffered events with ID greater than afterID that match
// f, oldest first, for SSE Last-Event-ID replay. complete is false when
// events after afterID have already been evicted from the replay buffer, i.e.
// the caller missed events that can no longer be replayed.
func (b *Bus) Since(afterID uint64, f Filter) (events []Event, complete bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.size {
		e := b.ring[(b.head+i)%len(b.ring)]
		if e.ID > afterID && f.Match(e) {
			events = append(events, e)
		}
	}
	return events, afterID >= b.evicted
}

// flush delivers all pending coalesced progress events.
func (b *Bus) flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.flushLocked()
}

func (b *Bus) flushLocked() {
	for _, id := range b.order {
		b.deliverLocked(b.pending[id])
	}
	clear(b.pending)
	b.order = b.order[:0]
}

func (b *Bus) removePendingLocked(taskID string) {
	delete(b.pending, taskID)
	for i, id := range b.order {
		if id == taskID {
			b.order = append(b.order[:i], b.order[i+1:]...)
			return
		}
	}
}

// deliverLocked stamps e, appends it to the replay ring and fans it out.
func (b *Bus) deliverLocked(e Event) Event {
	b.lastID++
	e.ID = b.lastID
	e.Time = b.clock().UTC()

	if b.size < len(b.ring) {
		b.ring[(b.head+b.size)%len(b.ring)] = e
		b.size++
	} else {
		b.evicted = b.ring[b.head].ID
		b.ring[b.head] = e
		b.head = (b.head + 1) % len(b.ring)
	}

	for s := range b.subs {
		if s.filter.Match(e) {
			b.sendLocked(s, e)
		}
	}
	return e
}

// sendLocked writes e into s's channel without blocking, dropping the oldest
// queued event when the buffer is full. Only the bus sends on s.ch and always
// under b.mu, so after a successful drain there is room for e.
func (b *Bus) sendLocked(s *Subscription, e Event) {
	select {
	case s.ch <- e:
		return
	default:
	}
	select {
	case <-s.ch:
		n := s.dropped.Add(1)
		b.log.Debug("events: subscriber buffer full, dropped oldest event",
			"dropped_total", n, "event_type", string(e.Type))
	default:
		// The consumer drained the buffer concurrently; room is available.
	}
	select {
	case s.ch <- e:
	default:
		// Unreachable: only this goroutine (holding b.mu) sends on s.ch.
		s.dropped.Add(1)
	}
}

// removeLocked unregisters s, closes its channel and detaches it from its
// context. It is a no-op if s is no longer registered.
func (b *Bus) removeLocked(s *Subscription) {
	if _, ok := b.subs[s]; !ok {
		return
	}
	delete(b.subs, s)
	close(s.ch)
	if s.stopCtx != nil {
		// The result is irrelevant: either the callback was deregistered or
		// it already ran (or is running) and will find s unregistered.
		_ = s.stopCtx()
		s.stopCtx = nil
	}
}

// Subscription is a live registration on a Bus.
type Subscription struct {
	bus     *Bus
	filter  Filter
	ch      chan Event
	dropped atomic.Uint64
	stopCtx func() bool // guarded by bus.mu
}

// C returns the channel on which events are delivered. It is closed when the
// subscription ends.
func (s *Subscription) C() <-chan Event { return s.ch }

// Dropped returns how many events were discarded because the subscriber's
// buffer was full.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Unsubscribe ends the subscription and closes its channel. It is idempotent
// and safe to call concurrently.
func (s *Subscription) Unsubscribe() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	s.bus.removeLocked(s)
}
