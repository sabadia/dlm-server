package domain

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestSegment(t *testing.T) {
	tests := []struct {
		seg             Segment
		length, remain  int64
		complete, valid bool
	}{
		{Segment{Start: 0, End: 100, Done: 0}, 100, 100, false, true},
		{Segment{Start: 100, End: 200, Done: 40}, 100, 60, false, true},
		{Segment{Start: 100, End: 200, Done: 100}, 100, 0, true, true},
		{Segment{Start: 5, End: 5}, 0, 0, true, false},
		{Segment{Start: -1, End: 5}, 6, 6, false, false},
		{Segment{Start: 0, End: 10, Done: 11}, 10, -1, true, false},
		{Segment{Start: 0, End: 10, Done: -1}, 10, 11, false, false},
	}
	for _, tt := range tests {
		s := tt.seg
		if s.Len() != tt.length || s.Remaining() != tt.remain || s.Complete() != tt.complete {
			t.Errorf("%+v: Len=%d Remaining=%d Complete=%v", s, s.Len(), s.Remaining(), s.Complete())
		}
		err := s.Validate()
		if (err == nil) != tt.valid || (err != nil && !errors.Is(err, ErrInvalidArgument)) {
			t.Errorf("%+v: Validate = %v, valid %v", s, err, tt.valid)
		}
	}
}

func TestCategoryValidate(t *testing.T) {
	ok := Category{ID: "c", Name: "Video", Extensions: []string{"mp4", "tar.gz"}, MIMETypes: []string{"video/*"}, Dir: "Videos"}
	tests := []struct {
		name    string
		mutate  func(*Category)
		wantErr bool
	}{
		{"valid", func(*Category) {}, false},
		{"mime only", func(c *Category) { c.Extensions = nil }, false},
		{"no id", func(c *Category) { c.ID = "" }, true},
		{"no name", func(c *Category) { c.Name = " " }, true},
		{"no dir", func(c *Category) { c.Dir = "" }, true},
		{"no rules", func(c *Category) { c.Extensions, c.MIMETypes = nil, nil }, true},
		{"upper ext", func(c *Category) { c.Extensions = []string{"MP4"} }, true},
		{"dotted ext", func(c *Category) { c.Extensions = []string{".mp4"} }, true},
		{"empty ext", func(c *Category) { c.Extensions = []string{""} }, true},
		{"bad mime", func(c *Category) { c.MIMETypes = []string{"video"} }, true},
		{"star mime", func(c *Category) { c.MIMETypes = []string{"*/*"} }, true},
		{"relative dir escaping", func(c *Category) { c.Dir = "a/../../b" }, true},
		{"absolute dir", func(c *Category) { c.Dir = filepath.Join(string(filepath.Separator), "data", "videos") }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ok
			tt.mutate(&c)
			err := c.Validate()
			if (err != nil) != tt.wantErr || (err != nil && !errors.Is(err, ErrInvalidArgument)) {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCategoryMatches(t *testing.T) {
	c := Category{ID: "c", Name: "Mixed", Extensions: []string{"mp4", "tar.gz"}, MIMETypes: []string{"video/*", "application/pdf"}, Dir: "x"}
	tests := []struct {
		filename, mime string
		want           bool
	}{
		{"movie.MP4", "", true},
		{"backup.tar.gz", "", true},
		{"archive.gz", "", false},
		{"mp4", "", false},
		{"clip", "video/webm", true},
		{"doc", "Application/PDF; charset=binary", true},
		{"doc", "application/pdfx", false},
		{"page.html", "text/html", false},
		{"", "", false},
	}
	for _, tt := range tests {
		if got := c.Matches(tt.filename, tt.mime); got != tt.want {
			t.Errorf("Matches(%q, %q) = %v, want %v", tt.filename, tt.mime, got, tt.want)
		}
	}
}

func TestResourceSelectedSize(t *testing.T) {
	tests := []struct {
		name string
		r    Resource
		want int64
	}{
		{"single known", Resource{Size: 100}, 100},
		{"single unknown", Resource{Size: -1}, -1},
		{"files selected", Resource{Size: 60, Files: []File{{Size: 10, Selected: true}, {Size: 20}, {Size: 30, Selected: true}}}, 40},
		{"none selected", Resource{Files: []File{{Size: 10}}}, 0},
		{"selected unknown", Resource{Files: []File{{Size: 10, Selected: true}, {Size: -1, Selected: true}}}, -1},
		{"unselected unknown ignored", Resource{Files: []File{{Size: 10, Selected: true}, {Size: -1}}}, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.SelectedSize(); got != tt.want {
				t.Fatalf("SelectedSize = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNewID(t *testing.T) {
	const n = 10_000
	seen := make(map[string]struct{}, n)
	prev := ""
	for range n {
		id := NewID()
		u, err := uuid.Parse(id)
		if err != nil || u.Version() != 7 {
			t.Fatalf("NewID() = %q: not a UUIDv7 (%v)", id, err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = struct{}{}
		if id <= prev {
			t.Fatalf("ids not increasing: %q then %q", prev, id)
		}
		prev = id
	}
}

func TestCategoryResolveDir(t *testing.T) {
	base := t.TempDir()
	abs := filepath.Join(t.TempDir(), "videos")
	rel := Category{Dir: "Videos/Series"}
	if got := rel.ResolveDir(base); got != filepath.Join(base, "Videos", "Series") {
		t.Errorf("relative: %q", got)
	}
	if got := (&Category{Dir: abs}).ResolveDir(base); got != abs {
		t.Errorf("absolute: %q", got)
	}
}
