package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
)

type fakeEngine struct {
	name   string
	scheme string
}

func (f fakeEngine) Name() string { return f.name }
func (f fakeEngine) Match(src domain.Source) bool {
	return strings.HasPrefix(src.CurrentURL, f.scheme+"://") || strings.HasSuffix(src.CurrentURL, "."+f.scheme)
}
func (fakeEngine) Resolve(context.Context, Request) (*Probe, error) { return &Probe{}, nil }
func (fakeEngine) NewDownload(Spec) (Download, error)               { return nil, errors.New("unused") }

type closer struct{ n *atomic.Int32 }

func (c closer) Close() error { c.n.Add(1); return nil }

func TestRegistrySelectOrder(t *testing.T) {
	r, err := NewRegistry(fakeEngine{"hls", "m3u8"}, fakeEngine{"http", "https"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"https://x/video.m3u8": "hls", // first match wins
		"https://x/file.zip":   "http",
	}
	for url, want := range cases {
		e, err := r.Select(domain.Source{CurrentURL: url})
		if err != nil || e.Name() != want {
			t.Errorf("Select(%s) = %v, %v; want %s", url, e, err, want)
		}
	}
	if _, err := r.Select(domain.Source{CurrentURL: "gopher://x"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("unsupported source: err = %v", err)
	}
	if e, err := r.Get("http"); err != nil || e.Name() != "http" {
		t.Errorf("Get(http) = %v, %v", e, err)
	}
	if _, err := r.Get("ftp"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get(ftp) err = %v", err)
	}
	if got := r.Names(); !slices.Equal(got, []string{"hls", "http"}) {
		t.Errorf("Names = %v", got)
	}
}

func TestRegistryRejectsBadNames(t *testing.T) {
	if _, err := NewRegistry(fakeEngine{"", "x"}); err == nil {
		t.Error("empty name accepted")
	}
	if _, err := NewRegistry(fakeEngine{"a", "x"}, fakeEngine{"a", "y"}); err == nil {
		t.Error("duplicate name accepted")
	}
}

func TestClassifyAndTaskError(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	base := errors.New("boom")
	tests := []struct {
		name      string
		err       error
		kind      Kind
		code      string
		retryable bool
	}{
		{"transient", Transient(domain.ErrorCodeNetwork, base), KindTransient, domain.ErrorCodeNetwork, true},
		{"wrapped transient", fmt.Errorf("ctx: %w", Transient(domain.ErrorCodeNetwork, base)), KindTransient, domain.ErrorCodeNetwork, true},
		{"link expired", LinkExpired(base), KindLinkExpired, domain.ErrorCodeLinkExpired, true},
		{"fatal", Fatal(domain.ErrorCodeDiskFull, base), KindFatal, domain.ErrorCodeDiskFull, false},
		{"plain error is fatal", base, KindFatal, domain.ErrorCodeFatal, false},
		{"canceled", fmt.Errorf("run: %w", context.Canceled), KindCanceled, domain.ErrorCodeFatal, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.kind {
				t.Errorf("Classify = %v, want %v", got, tt.kind)
			}
			te := TaskError(tt.err, now)
			if te.Code != tt.code || te.Retryable != tt.retryable || !te.At.Equal(now) || te.Message == "" {
				t.Errorf("TaskError = %+v", te)
			}
		})
	}
	if Classify(nil) != KindFatal {
		t.Error("Classify(nil) should be fatal")
	}
	e := &Error{Kind: KindTransient, StatusCode: 503, Err: base}
	if !strings.Contains(e.Error(), "status 503") || !errors.Is(e, base) {
		t.Errorf("Error() = %q", e.Error())
	}
	if KindLinkExpired.String() != "link_expired" || Kind(99).String() != "kind(99)" {
		t.Error("Kind.String mismatch")
	}
}

func TestProbeAttachment(t *testing.T) {
	var n atomic.Int32
	p := &Probe{}
	p.Attach(closer{&n})
	p.Attach(closer{&n}) // replaces and closes the first
	if n.Load() != 1 {
		t.Fatalf("replaced attachment not closed: %d", n.Load())
	}
	c := p.TakeAttachment()
	if c == nil || p.TakeAttachment() != nil {
		t.Fatal("TakeAttachment should hand over exactly once")
	}
	if err := p.Close(); err != nil || n.Load() != 1 {
		t.Fatalf("Close after take must not close the taken attachment (n=%d)", n.Load())
	}
	p.Attach(closer{&n}) // attach after close: released immediately
	if n.Load() != 2 {
		t.Fatalf("attachment after close not released: %d", n.Load())
	}
	var nilProbe *Probe
	if err := nilProbe.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveCache(t *testing.T) {
	now := time.Unix(1000, 0)
	var ids atomic.Int32
	c := NewResolveCache(CacheOptions{
		TTL:        time.Minute,
		MaxEntries: 2,
		Clock:      func() time.Time { return now },
		NewID:      func() string { return fmt.Sprint(ids.Add(1)) },
	})
	var closed atomic.Int32
	newProbe := func() *Probe { p := &Probe{}; p.Attach(closer{&closed}); return p }

	a := c.Put(newProbe())
	b := c.Put(newProbe())
	if _, err := c.Peek(a); err != nil {
		t.Fatalf("Peek: %v", err)
	}
	c.Put(newProbe()) // evicts a (oldest)
	if closed.Load() != 1 {
		t.Fatalf("evicted probe not closed: %d", closed.Load())
	}
	if _, err := c.Take(a); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("evicted id still present: %v", err)
	}
	p, err := c.Take(b)
	if err != nil || p == nil {
		t.Fatalf("Take(b) = %v, %v", p, err)
	}
	if closed.Load() != 1 {
		t.Error("taken probe must not be closed by the cache")
	}
	if _, err := c.Take(b); !errors.Is(err, domain.ErrNotFound) {
		t.Error("Take must remove the entry")
	}

	now = now.Add(2 * time.Minute) // expire the remaining entry
	if c.Len() != 0 || closed.Load() != 2 {
		t.Errorf("expired entry not swept: len=%d closed=%d", c.Len(), closed.Load())
	}
	c.Put(newProbe())
	c.Close()
	if c.Len() != 0 || closed.Load() != 3 {
		t.Errorf("Close: len=%d closed=%d", c.Len(), closed.Load())
	}
}

func TestResolveCacheRunStops(t *testing.T) {
	c := NewResolveCache(CacheOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, time.Millisecond) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
