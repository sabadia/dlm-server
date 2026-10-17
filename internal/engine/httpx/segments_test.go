package httpx

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/domain"
)

func TestValidateSegments(t *testing.T) {
	tests := []struct {
		name string
		segs []domain.Segment
		size int64
		ok   bool
	}{
		{"empty", nil, 10, false},
		{"single", []domain.Segment{{Start: 0, End: 10, Done: 3}}, 10, true},
		{"unordered", []domain.Segment{{Index: 7, Start: 5, End: 10}, {Index: 9, Start: 0, End: 5, Done: 5}}, 10, true},
		{"gap", []domain.Segment{{Start: 0, End: 4}, {Start: 5, End: 10}}, 10, false},
		{"overlap", []domain.Segment{{Start: 0, End: 6}, {Start: 5, End: 10}}, 10, false},
		{"short", []domain.Segment{{Start: 0, End: 9}}, 10, false},
		{"not from zero", []domain.Segment{{Start: 1, End: 10}}, 10, false},
		{"done too large", []domain.Segment{{Start: 0, End: 10, Done: 11}}, 10, false},
		{"unknown size", []domain.Segment{{Start: 0, End: 10}}, -1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := validateSegments(tt.segs, tt.size)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			for i, s := range got {
				if s.Index != i {
					t.Errorf("segment %d has index %d", i, s.Index)
				}
			}
		})
	}
}

func TestSegTableSplitAndSteal(t *testing.T) {
	tab := newSegTable(nil, 1000, 100)
	a := tab.acquire()
	if a == nil || a.start != 0 || a.end != 1000 {
		t.Fatalf("first acquire = %+v", a)
	}
	// A second connection splits the remaining [0, 1000) in half.
	b := tab.acquire()
	if b == nil || b.start != 500 || b.end != 1000 {
		t.Fatalf("split = %+v", b)
	}
	if _, end := tab.span(a); end != 500 {
		t.Fatalf("owner end = %d, want 500", end)
	}
	// The owner writes 300 bytes; a split now starts above them.
	pos, w, atEnd := tab.reserve(a, 300)
	if pos != 0 || w != 300 || atEnd {
		t.Fatalf("reserve = %d %d %v", pos, w, atEnd)
	}
	tab.commit(a, w)
	// Largest remaining: b (500) beats a (200).
	c := tab.acquire()
	if c == nil || c.start != 750 || c.end != 1000 {
		t.Fatalf("steal = %+v", c)
	}
	// A write crossing the shrunken end is truncated to it.
	pos, w, atEnd = tab.reserve(a, 1000)
	if pos != 300 || w != 200 || !atEnd {
		t.Fatalf("reserve at end = %d %d %v", pos, w, atEnd)
	}
	tab.commit(a, w)
	tab.release(a)
	// Remaining: b [500,750) = 250, c [750,1000) = 250; both >= 100.
	d := tab.acquire()
	if d == nil || d.end-d.start != 125 {
		t.Fatalf("second steal = %+v", d)
	}
	segs, done := tab.snapshot()
	if done != 500 {
		t.Errorf("done = %d, want 500", done)
	}
	if _, ok := validateSegments(segs, 1000); !ok {
		t.Errorf("snapshot does not cover the file: %+v", segs)
	}
}

func TestSegTableMinSplit(t *testing.T) {
	tab := newSegTable(nil, 150, 100)
	if tab.acquire() == nil {
		t.Fatal("no first segment")
	}
	if s := tab.acquire(); s == nil {
		t.Fatal("150 >= 100 should split")
	}
	if s := tab.acquire(); s != nil {
		t.Fatalf("75-byte remainder split: %+v", s)
	}
}

func TestSegTableRestoredFirst(t *testing.T) {
	segs := []domain.Segment{{Start: 0, End: 100, Done: 100}, {Start: 100, End: 200, Done: 40}, {Start: 200, End: 300}}
	tab := newSegTable(segs, 300, 10)
	var got [][2]int64
	for range 2 {
		s := tab.acquire()
		got = append(got, [2]int64{s.start, s.next})
	}
	want := [][2]int64{{100, 140}, {200, 200}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("acquire order (-want +got):\n%s", diff)
	}
	if tab.complete() {
		t.Error("complete with pending bytes")
	}
}
