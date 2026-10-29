// Package transfer measures what the daemon transfers: the speed of the whole
// daemon, published once a second as system.speed events while anything
// moves, and the bytes per hour, queue and engine that the statistics
// timeline is made of.
package transfer

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/storage"
)

// Defaults of Options.
const (
	DefaultSpeedInterval = time.Second
	DefaultFlushInterval = 30 * time.Second
)

// Options configure a Meter.
type Options struct {
	// Store keeps the history; nil keeps none.
	Store storage.Store
	// Bus receives system.speed events; nil publishes none.
	Bus    *events.Bus
	Logger *slog.Logger
	// Clock defaults to time.Now.
	Clock func() time.Time
	// SpeedInterval is how often the speed is measured and published, and
	// FlushInterval how often the history is written.
	SpeedInterval, FlushInterval time.Duration
}

// Meter counts transferred bytes. The runner reports them with Record; Run
// publishes the speed and keeps the history. It is safe for concurrent use.
type Meter struct {
	o Options

	mu         sync.Mutex
	down, up   int64 // bytes since the last speed measurement
	lastTick   time.Time
	wasMoving  bool
	pending    map[bucketKey]*bucketBytes
	flushedAt  time.Time
	totalCount int64 // samples recorded, for tests
}

type bucketKey struct {
	hour          int64 // Unix seconds of the start of the hour
	queue, engine string
}

type bucketBytes struct{ down, up int64 }

// New creates a Meter.
func New(o Options) *Meter {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.SpeedInterval <= 0 {
		o.SpeedInterval = DefaultSpeedInterval
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = DefaultFlushInterval
	}
	now := o.Clock()
	return &Meter{o: o, lastTick: now, flushedAt: now, pending: make(map[bucketKey]*bucketBytes)}
}

// Record counts bytes a task of queueID (running on engine) downloaded and
// uploaded since its previous report.
func (m *Meter) Record(_, queueID, engine string, down, up int64) {
	if down <= 0 && up <= 0 {
		return
	}
	now := m.o.Clock()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down += max(down, 0)
	m.up += max(up, 0)
	m.totalCount++
	if m.o.Store == nil {
		return
	}
	k := bucketKey{hour: now.UTC().Truncate(time.Hour).Unix(), queue: queueID, engine: engine}
	b := m.pending[k]
	if b == nil {
		b = &bucketBytes{}
		m.pending[k] = b
	}
	b.down += max(down, 0)
	b.up += max(up, 0)
}

// Run measures and publishes the speed and writes the history until ctx is
// done; it writes what is pending once more before it returns.
func (m *Meter) Run(ctx context.Context) {
	t := time.NewTicker(m.o.SpeedInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.Flush(context.WithoutCancel(ctx))
			return
		case <-t.C:
			m.Tick(ctx)
		}
	}
}

// Tick measures the speed over the time since the previous tick, publishes it
// when anything moved (or just stopped moving) and writes the history when
// it is due.
func (m *Meter) Tick(ctx context.Context) {
	now := m.o.Clock()
	m.mu.Lock()
	dt := now.Sub(m.lastTick).Seconds()
	down, up := m.down, m.up
	m.down, m.up, m.lastTick = 0, 0, now
	moving := down > 0 || up > 0
	publish := moving || m.wasMoving
	m.wasMoving = moving
	flush := now.Sub(m.flushedAt) >= m.o.FlushInterval
	m.mu.Unlock()

	if publish && m.o.Bus != nil && dt > 0 {
		data := events.SpeedData{Down: int64(float64(down) / dt), Up: int64(float64(up) / dt)}
		if m.o.Store != nil {
			if counts, err := m.o.Store.Tasks().CountByStatus(ctx, ""); err == nil {
				data.Active = counts[domain.StatusRunning] + counts[domain.StatusRefreshing]
				data.Queued = counts[domain.StatusQueued]
			}
		}
		m.o.Bus.Publish(events.Event{Type: events.TypeSystemSpeed, Data: data})
	}
	if flush {
		m.Flush(ctx)
	}
}

// Flush writes the pending history. Rows that cannot be written are kept for
// the next flush.
func (m *Meter) Flush(ctx context.Context) {
	if m.o.Store == nil {
		return
	}
	m.mu.Lock()
	pending := m.pending
	m.pending = make(map[bucketKey]*bucketBytes)
	m.flushedAt = m.o.Clock()
	m.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	rows := make([]storage.TransferRow, 0, len(pending))
	for k, b := range pending {
		rows = append(rows, storage.TransferRow{
			Bucket: time.Unix(k.hour, 0).UTC(), QueueID: k.queue, Engine: k.engine, Down: b.down, Up: b.up,
		})
	}
	if err := m.o.Store.Transfer().Add(ctx, rows); err != nil {
		m.o.Logger.WarnContext(ctx, "transfer: cannot write the history; keeping it for the next time", "error", err)
		m.mu.Lock()
		for k, b := range pending {
			if cur := m.pending[k]; cur != nil {
				cur.down += b.down
				cur.up += b.up
			} else {
				m.pending[k] = b
			}
		}
		m.mu.Unlock()
	}
}
