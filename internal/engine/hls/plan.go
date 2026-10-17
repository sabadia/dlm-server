package hls

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/settings"
)

// track is one media playlist being downloaded: the video (or muxed) stream,
// or a separate audio rendition. Its segments are keyed by media sequence
// number, so a reloaded playlist can replace URLs (fresh tokens) and a live
// playlist can add segments.
type track struct {
	idx  int
	kind string // "video" or "audio"
	url  string // the media playlist

	mu     sync.RWMutex
	segs   map[uint64]segment
	order  []uint64
	closed bool
	target float64

	reloadMu     sync.Mutex
	lastReload   time.Time
	lastReloadOK bool
}

func newTrack(idx int, kind, rawURL string, m *media) *track {
	t := &track{idx: idx, kind: kind, url: rawURL, segs: map[uint64]segment{}}
	t.update(m)
	return t
}

// update merges a (re)loaded playlist: known sequence numbers get their new
// URLs, unknown ones are appended in playlist order. It returns how many
// segments were new.
func (t *track) update(m *media) (added int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range m.segments {
		if _, known := t.segs[s.seq]; !known {
			t.order = append(t.order, s.seq)
			added++
		}
		t.segs[s.seq] = s
	}
	t.closed = m.closed
	t.target = m.target
	return added
}

func (t *track) get(seq uint64) (segment, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.segs[seq]
	return s, ok
}

// snapshot returns the segments in playlist order.
func (t *track) snapshot() []segment {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]segment, 0, len(t.order))
	for _, seq := range t.order {
		out = append(out, t.segs[seq])
	}
	return out
}

func (t *track) isClosed() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.closed
}

func (t *track) targetDuration() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return time.Duration(t.target * float64(time.Second))
}

func (t *track) totalDuration() float64 {
	var d float64
	for _, s := range t.snapshot() {
		d += s.duration
	}
	return d
}

// plan is what a stream consists of.
type plan struct {
	tracks    []*track
	bandwidth int64 // of the chosen variant, for size estimates; 0 unknown
}

func (p *plan) live() bool {
	for _, t := range p.tracks {
		if !t.isClosed() {
			return true
		}
	}
	return false
}

// ErrNoSegments is returned when a media playlist lists no segments.
var ErrNoSegments = errors.New("hls: the playlist has no segments")

// buildPlan reads the playlist at src.CurrentURL. A master playlist is
// resolved to one variant (Source.MediaHint) and, when that variant plays with
// a separate audio rendition, to its audio playlist too.
func (e *Engine) buildPlan(ctx context.Context, client *http.Client, src domain.Source, s settings.Settings) (*plan, error) {
	first, err := loadPlaylist(ctx, client, src, s, src.CurrentURL)
	if err != nil {
		return nil, err
	}
	p := &plan{}
	load := func(idx int, kind, rawURL string, m *media) error {
		if m == nil {
			pl, err := loadPlaylist(ctx, client, src, s, rawURL)
			if err != nil {
				return err
			}
			if pl.media == nil {
				return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("hls: %s is a master playlist where a media playlist was expected", redact(rawURL)))
			}
			m = pl.media
		}
		if len(m.segments) == 0 {
			return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w (%s)", ErrNoSegments, redact(rawURL)))
		}
		p.tracks = append(p.tracks, newTrack(idx, kind, rawURL, m))
		return nil
	}
	if first.media != nil {
		return p, load(0, "video", src.CurrentURL, first.media)
	}
	v, err := selectVariant(first.master.variants, src.MediaHint)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	p.bandwidth = v.bandwidth
	if err := load(0, "video", v.url, nil); err != nil {
		return nil, err
	}
	if a, ok := pickAudio(first.master, v); ok {
		if err := load(1, "audio", a.url, nil); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// reload fetches the track's playlist again and merges it in. It is used to
// learn fresh segment and key URLs after one expired, and to follow a live
// stream. Concurrent callers share one reload (a reload less than a second old
// is reused); it reports whether the playlist could be reloaded and how many
// segments were new.
func (d *Download) reload(ctx context.Context, t *track, force bool) (bool, int, error) {
	t.reloadMu.Lock()
	defer t.reloadMu.Unlock()
	if !force && !t.lastReload.IsZero() && d.e.env.Clock().Sub(t.lastReload) < time.Second {
		return t.lastReloadOK, 0, nil
	}
	pl, err := loadPlaylist(ctx, d.client, d.spec.Source, d.spec.Settings, t.url)
	t.lastReload = d.e.env.Clock()
	if err != nil {
		t.lastReloadOK = false
		return false, 0, err
	}
	if pl.media == nil {
		t.lastReloadOK = false
		return false, 0, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("hls: %s no longer is a media playlist", redact(t.url)))
	}
	t.lastReloadOK = true
	return true, t.update(pl.media), nil
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
