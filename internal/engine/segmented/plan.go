package segmented

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
)

// Track is one stream of the download: the video (or muxed) stream, or a
// separate audio rendition. Its segments are keyed by sequence number, so a
// reloaded description can replace URLs (fresh tokens) and a live one can add
// segments.
type Track struct {
	Index int
	Kind  string // "video" or "audio"
	// URL identifies the track's description (an HLS media playlist URL, a DASH
	// representation id); the Frontend uses it to reload the track.
	URL string

	mu     sync.RWMutex
	segs   map[uint64]Segment
	order  []uint64
	closed bool
	target float64

	reloadMu     sync.Mutex
	lastReload   time.Time
	lastReloadOK bool
}

// NewTrack creates a track holding the segments of m.
func NewTrack(index int, kind, url string, m *Media) *Track {
	t := &Track{Index: index, Kind: kind, URL: url, segs: map[uint64]Segment{}}
	t.update(m)
	return t
}

// update merges a (re)loaded segment list: known sequence numbers get their
// new URLs, unknown ones are appended in order. It returns how many segments
// were new.
func (t *Track) update(m *Media) (added int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range m.Segments {
		if _, known := t.segs[s.Seq]; !known {
			t.order = append(t.order, s.Seq)
			added++
		}
		t.segs[s.Seq] = s
	}
	t.closed = m.Closed
	t.target = m.Target
	return added
}

func (t *Track) get(seq uint64) (Segment, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.segs[seq]
	return s, ok
}

// snapshot returns the segments in order.
func (t *Track) snapshot() []Segment {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Segment, 0, len(t.order))
	for _, seq := range t.order {
		out = append(out, t.segs[seq])
	}
	return out
}

func (t *Track) isClosed() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.closed
}

func (t *Track) targetDuration() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return time.Duration(t.target * float64(time.Second))
}

func (t *Track) totalDuration() float64 {
	var d float64
	for _, s := range t.snapshot() {
		d += s.Duration
	}
	return d
}

// Plan is what a stream consists of.
type Plan struct {
	Tracks []*Track
	// Bandwidth of the chosen variant in bits per second, for size estimates;
	// 0 when unknown.
	Bandwidth int64
}

func (p *Plan) live() bool {
	for _, t := range p.Tracks {
		if !t.isClosed() {
			return true
		}
	}
	return false
}

// reload reads the track's description again through the Frontend and merges
// it in. It is used to learn fresh segment and key URLs after one expired, and
// to follow a live stream. Concurrent callers share one reload (one less than
// a second old is reused unless forced); it reports whether the description
// could be reloaded and how many segments were new.
func (d *Download) reload(ctx context.Context, t *Track, force bool) (bool, int, error) {
	t.reloadMu.Lock()
	defer t.reloadMu.Unlock()
	if !force && !t.lastReload.IsZero() && d.r.env.Clock().Sub(t.lastReload) < time.Second {
		return t.lastReloadOK, 0, nil
	}
	m, err := d.fe.Reload(ctx, d.client, t)
	t.lastReload = d.r.env.Clock()
	if err != nil {
		t.lastReloadOK = false
		return false, 0, err
	}
	if m == nil || len(m.Segments) == 0 {
		t.lastReloadOK = false
		return false, 0, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w (reloading %s)", ErrNoSegments, Redact(t.URL)))
	}
	t.lastReloadOK = true
	return true, t.update(m), nil
}

// sortedSeqs returns the keys of a done map in ascending order.
func sortedSeqs(m map[uint64]int64) []uint64 {
	out := make([]uint64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
