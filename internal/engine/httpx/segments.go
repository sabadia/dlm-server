package httpx

import (
	"cmp"
	"slices"
	"sync"

	"github.com/sabadia/dlm-server/internal/domain"
)

// segment is one byte range [start, end) of the file, written by at most
// one connection at a time. All fields except idx and start are guarded by
// segTable.mu.
type segment struct {
	idx   int
	start int64
	// end is exclusive. It only ever shrinks, when another connection
	// splits off the tail of the segment.
	end int64
	// done counts the bytes durably written from start.
	done int64
	// next is the offset of the next byte the owner will write: start+done
	// plus the size of an in-flight write. Splits never hand out bytes
	// below next.
	next int64
	// active means a connection owns the segment.
	active bool
}

// segTable is the set of segments of a ranged download. It implements the
// split and work-stealing rules; it is safe for concurrent use. Only
// bookkeeping happens under the lock, never file or network I/O.
type segTable struct {
	minSplit int64

	mu      sync.Mutex
	segs    []*segment
	nextIdx int
}

// newSegTable builds a table from validated segments (see
// validateSegments), or a single segment [0, size) when segs is empty.
func newSegTable(segs []domain.Segment, size, minSplit int64) *segTable {
	t := &segTable{minSplit: max(minSplit, 1)}
	if len(segs) == 0 {
		segs = []domain.Segment{{Start: 0, End: size}}
	}
	for i, s := range segs {
		t.segs = append(t.segs, &segment{idx: i, start: s.Start, end: s.End, done: s.Done, next: s.Start + s.Done})
	}
	t.nextIdx = len(t.segs)
	return t
}

// validateSegments reports whether segs (in any order) are valid and
// exactly cover [0, size) without gaps or overlaps. It returns them sorted
// by start with indices renumbered 0..n-1.
func validateSegments(segs []domain.Segment, size int64) ([]domain.Segment, bool) {
	if len(segs) == 0 || size <= 0 {
		return nil, false
	}
	out := slices.Clone(segs)
	slices.SortFunc(out, byStart)
	var pos int64
	for i := range out {
		if out[i].Validate() != nil || out[i].Start != pos {
			return nil, false
		}
		out[i].Index = i
		pos = out[i].End
	}
	if pos != size {
		return nil, false
	}
	return out, true
}

// acquire hands a connection its next piece of work:
//  1. the first incomplete segment nobody owns (restored segments), else
//  2. the second half of the owned segment with the most bytes left, when
//     that remainder is at least minSplit bytes. The owner's end shrinks to
//     the split point; the owner notices before its next write.
//
// It returns nil when there is nothing left to hand out.
func (t *segTable) acquire() *segment {
	t.mu.Lock()
	defer t.mu.Unlock()
	var victim *segment
	for _, s := range t.segs {
		if s.next >= s.end {
			continue
		}
		if !s.active {
			s.active = true
			return s
		}
		if victim == nil || s.end-s.next > victim.end-victim.next {
			victim = s
		}
	}
	if victim == nil {
		return nil
	}
	rem := victim.end - victim.next
	if rem < t.minSplit || rem < 2 {
		return nil
	}
	mid := victim.next + rem/2
	ns := &segment{idx: t.nextIdx, start: mid, end: victim.end, next: mid, active: true}
	t.nextIdx++
	victim.end = mid
	t.segs = append(t.segs, ns)
	return ns
}

// span returns the next offset to write and the current end of s.
func (t *segTable) span(s *segment) (next, end int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return s.next, s.end
}

// reserve claims up to n bytes at the current position of s for a write.
// It returns the file offset, the number of bytes to write (0 when the
// segment is already finished) and whether the write reaches the end of the
// segment, which may have shrunk since the data was requested.
func (t *segTable) reserve(s *segment, n int64) (pos, w int64, atEnd bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	pos = s.next
	w = max(min(n, s.end-pos), 0)
	s.next = pos + w
	return pos, w, s.next >= s.end
}

// commit records w bytes written by the reservation at pos.
func (t *segTable) commit(s *segment, w int64) {
	t.mu.Lock()
	s.done += w
	t.mu.Unlock()
}

// rollback undoes a reservation whose write failed.
func (t *segTable) rollback(s *segment, pos int64) {
	t.mu.Lock()
	s.next = pos
	t.mu.Unlock()
}

// release marks s as no longer owned (its connection finished it or gave
// up), so acquire may hand out its remainder again.
func (t *segTable) release(s *segment) {
	t.mu.Lock()
	s.active = false
	t.mu.Unlock()
}

// complete reports whether every byte has been written.
func (t *segTable) complete() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.segs {
		if s.start+s.done < s.end {
			return false
		}
	}
	return true
}

// snapshot returns a copy of the segments sorted by start and the total
// number of bytes written, taken atomically.
func (t *segTable) snapshot() ([]domain.Segment, int64) {
	t.mu.Lock()
	out := make([]domain.Segment, len(t.segs))
	var total int64
	for i, s := range t.segs {
		out[i] = domain.Segment{Index: s.idx, Start: s.start, End: s.end, Done: s.done}
		total += s.done
	}
	t.mu.Unlock()
	slices.SortFunc(out, byStart)
	return out, total
}

func byStart(a, b domain.Segment) int { return cmp.Compare(a.Start, b.Start) }
