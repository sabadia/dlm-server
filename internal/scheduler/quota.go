package scheduler

import (
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
)

// Quota scopes, the Scope field of QuotaData.
const (
	QuotaScopeGlobal = "global"
	QuotaScopeQueue  = "queue"
)

// globalScope is the meter key of the global quota; queue scopes are keyed
// by queue ID (never empty).
const globalScope = ""

// QuotaData is the payload of quota.exhausted and quota.reset events.
// Events of a queue's quota also carry the queue ID in Event.QueueID.
type QuotaData struct {
	Scope   string            `json:"scope"` // QuotaScopeGlobal or QuotaScopeQueue
	QueueID string            `json:"queueId,omitempty"`
	Bytes   int64             `json:"bytes"`
	Period  settings.Duration `json:"period"`
	// Used is the bytes counted in the current window (0 after a reset).
	Used int64 `json:"used"`
	// WindowStart and ResetsAt bound the current window; both are omitted
	// after a reset (the next window starts with the next byte).
	WindowStart time.Time `json:"windowStart,omitzero"`
	ResetsAt    time.Time `json:"resetsAt,omitzero"`
}

// QuotaUsage is a snapshot of one quota scope.
type QuotaUsage struct {
	Quota       settings.Quota
	Used        int64
	WindowStart time.Time // zero when no window is open
	Exhausted   bool
}

// QuotaMeter counts downloaded bytes per quota scope (the global scope and
// each queue). It implements service.ByteCounter, so the runner reports
// every run's bytes to it; the scheduler configures the scopes from the
// effective settings on every admission pass.
//
// Accounting uses fixed windows: a scope's window opens with the first
// byte counted while no window is open and lasts one Period; when it has
// elapsed the usage drops to zero and the next byte opens a new window.
// Bytes are only counted for scopes that have a quota (configured by the
// scheduler); usage lives in memory, so restarting the daemon resets every
// quota.
type QuotaMeter struct {
	clock func() time.Time

	mu     sync.Mutex
	scopes map[string]*quotaScope
	wake   func() // called when a scope becomes exhausted; set by New
}

type quotaScope struct {
	quota    settings.Quota
	start    time.Time // zero: no open window
	used     int64
	notified bool // quota.exhausted published for the current window
}

var _ service.ByteCounter = (*QuotaMeter)(nil)

// NewQuotaMeter creates a meter. clock defaults to time.Now; it must be the
// scheduler's clock.
func NewQuotaMeter(clock func() time.Time) *QuotaMeter {
	if clock == nil {
		clock = time.Now
	}
	return &QuotaMeter{clock: clock, scopes: make(map[string]*quotaScope)}
}

// AddBytes counts n bytes downloaded by a task of queueID against the
// global quota and the queue's quota. When a scope reaches its quota the
// scheduler is woken so it requeues the scope's running tasks promptly.
func (m *QuotaMeter) AddBytes(_, queueID string, n int64) {
	if n <= 0 {
		return
	}
	now := m.clock()
	m.mu.Lock()
	crossed := m.addLocked(globalScope, n, now)
	if queueID != "" && m.addLocked(queueID, n, now) {
		crossed = true
	}
	wake := m.wake
	m.mu.Unlock()
	if crossed && wake != nil {
		wake()
	}
}

// addLocked counts n bytes in scope key and reports whether this crossed
// its quota.
func (m *QuotaMeter) addLocked(key string, n int64, now time.Time) bool {
	sc := m.scopes[key]
	if sc == nil {
		return false
	}
	sc.roll(now)
	if sc.start.IsZero() {
		sc.start = now
	}
	before := sc.used
	sc.used += n
	return before < sc.quota.Bytes && sc.used >= sc.quota.Bytes
}

// Usage returns the usage of the global scope (queueID "") or of a queue's
// scope, and false when that scope has no quota.
func (m *QuotaMeter) Usage(queueID string) (QuotaUsage, bool) {
	now := m.clock()
	m.mu.Lock()
	defer m.mu.Unlock()
	sc := m.scopes[queueID]
	if sc == nil {
		return QuotaUsage{}, false
	}
	sc.roll(now)
	return QuotaUsage{Quota: sc.quota, Used: sc.used, WindowStart: sc.start, Exhausted: sc.exhausted()}, true
}

func (m *QuotaMeter) setWake(wake func()) {
	m.mu.Lock()
	m.wake = wake
	m.mu.Unlock()
}

// Quota state changes (quotaChange.typ).
const (
	quotaExhausted = "exhausted"
	quotaReset     = "reset"
)

// quotaChange is a state change of a scope that is published as an event.
type quotaChange struct {
	typ  string // quotaExhausted or quotaReset
	data QuotaData
}

// check configures scope key with quota (unset removes the scope) and
// reports whether the scope is exhausted at now, plus the state change to
// publish, if any: "exhausted" the first time a window is used up, "reset"
// when an exhausted scope becomes available again (its window elapsed, the
// quota was raised or removed).
func (m *QuotaMeter) check(key string, quota settings.Optional[settings.Quota], now time.Time) (bool, *quotaChange) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sc := m.scopes[key]
	q, ok := quota.Get()
	if !ok {
		delete(m.scopes, key)
		if sc != nil && sc.notified {
			return false, &quotaChange{typ: quotaReset, data: quotaData(key, sc.quota, nil)}
		}
		return false, nil
	}
	if sc == nil {
		sc = &quotaScope{}
		m.scopes[key] = sc
	}
	sc.quota = q
	sc.roll(now)
	switch exhausted := sc.exhausted(); {
	case exhausted && !sc.notified:
		sc.notified = true
		return true, &quotaChange{typ: quotaExhausted, data: quotaData(key, q, sc)}
	case !exhausted && sc.notified:
		sc.notified = false
		return false, &quotaChange{typ: quotaReset, data: quotaData(key, q, nil)}
	default:
		return exhausted, nil
	}
}

// prune forgets the scopes of queues not in queues (deleted queues).
func (m *QuotaMeter) prune(queues map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.scopes {
		if key != globalScope && !queues[key] {
			delete(m.scopes, key)
		}
	}
}

// roll closes the window once its period has elapsed.
func (sc *quotaScope) roll(now time.Time) {
	if !sc.start.IsZero() && !now.Before(sc.start.Add(sc.quota.Period.Std())) {
		sc.start, sc.used = time.Time{}, 0
	}
}

func (sc *quotaScope) exhausted() bool {
	return !sc.start.IsZero() && sc.used >= sc.quota.Bytes
}

// quotaData builds an event payload; sc nil describes a reset scope.
func quotaData(key string, q settings.Quota, sc *quotaScope) QuotaData {
	d := QuotaData{Scope: QuotaScopeGlobal, Bytes: q.Bytes, Period: q.Period}
	if key != globalScope {
		d.Scope, d.QueueID = QuotaScopeQueue, key
	}
	if sc != nil {
		d.Used, d.WindowStart, d.ResetsAt = sc.used, sc.start.UTC(), sc.start.Add(q.Period.Std()).UTC()
	}
	return d
}
