package refresh

import (
	"context"
	"errors"
	"testing"

	"github.com/sabadia/dlm-server/internal/browser"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

type fakePage struct {
	url    string
	result *browser.PageResult
	err    error
}

func (f *fakePage) ResolvePage(_ context.Context, pageURL string, _ browser.ResolveOptions) (*browser.PageResult, error) {
	f.url = pageURL
	return f.result, f.err
}

func cand(url string, kind browser.Kind, size int64, mutate ...func(*browser.Candidate)) browser.Candidate {
	c := browser.Candidate{URL: url, Kind: kind, Size: size, Source: "network"}
	for _, m := range mutate {
		m(&c)
	}
	return c
}

func TestPickCandidate(t *testing.T) {
	video := domain.Source{
		CurrentURL:  "https://cdn.example/v/clip.mp4?token=old",
		Fingerprint: domain.Fingerprint{Size: 5000, Filename: "clip.mp4", ContentType: "video/mp4"},
	}
	tests := []struct {
		name  string
		src   domain.Source
		cands []browser.Candidate
		want  string // "" means no match
	}{
		{"same size", video, []browser.Candidate{cand("https://cdn.example/other.mp4", browser.KindVideo, 100), cand("https://cdn2.example/x/abc.mp4?t=new", browser.KindVideo, 5000)}, "https://cdn2.example/x/abc.mp4?t=new"},
		{"same path new token", domain.Source{CurrentURL: "https://cdn.example/v/clip.mp4?token=old"}, []browser.Candidate{cand("https://cdn.example/v/clip.mp4?token=new", browser.KindVideo, -1)}, ""}, // path (2) alone is not enough
		{
			"same path and kind",
			domain.Source{CurrentURL: "https://cdn.example/v/clip.mp4?token=old", Fingerprint: domain.Fingerprint{ContentType: "video/mp4"}},
			[]browser.Candidate{cand("https://cdn.example/v/clip.mp4?token=new", browser.KindVideo, -1)},
			"https://cdn.example/v/clip.mp4?token=new",
		},
		{"file name", video, []browser.Candidate{cand("https://x/dl/clip.mp4?sig=1", browser.KindVideo, -1)}, "https://x/dl/clip.mp4?sig=1"},
		{
			"announced file name",
			domain.Source{Fingerprint: domain.Fingerprint{Filename: "report.pdf"}},
			[]browser.Candidate{cand("https://x/get?id=7", browser.KindFile, -1, func(c *browser.Candidate) { c.Filename = "Report.PDF" })},
			"https://x/get?id=7",
		},
		{"size contradicts", video, []browser.Candidate{cand("https://cdn.example/v/clip.mp4?token=new", browser.KindVideo, 9999)}, ""},
		{"media hint substring", domain.Source{MediaHint: "1080p"}, []browser.Candidate{cand("https://x/360p/index.m3u8", browser.KindHLS, -1), cand("https://x/1080p/index.m3u8", browser.KindHLS, -1)}, "https://x/1080p/index.m3u8"},
		{"media hint regexp", domain.Source{MediaHint: `master_\d+\.m3u8`}, []browser.Candidate{cand("https://x/master_7.m3u8?a=1", browser.KindHLS, -1)}, "https://x/master_7.m3u8?a=1"},
		{"media hint is not a valid regexp", domain.Source{MediaHint: "q[1080"}, []browser.Candidate{cand("https://x/q[1080/a.m3u8", browser.KindHLS, -1)}, "https://x/q[1080/a.m3u8"},
		{"best of several", video, []browser.Candidate{
			cand("https://x/a.mp4", browser.KindVideo, 5000),    // size: 3 (+1 kind)
			cand("https://x/clip.mp4", browser.KindVideo, 5000), // size and name: 6 (+1)
			cand("https://x/b.mp4", browser.KindVideo, -1),
		}, "https://x/clip.mp4"},
		{"first of equals", video, []browser.Candidate{cand("https://x/1.mp4", browser.KindVideo, 5000), cand("https://x/2.mp4", browser.KindVideo, 5000)}, "https://x/1.mp4"},
		{"blobs are never picked", domain.Source{Fingerprint: domain.Fingerprint{Size: 7}}, []browser.Candidate{cand("blob:https://x/1", browser.KindFile, 7)}, ""},
		{"nothing known about the task", domain.Source{CurrentURL: "https://x/a"}, []browser.Candidate{cand("https://x/b.mp4", browser.KindVideo, 5)}, ""},
		{"no candidates", video, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := PickCandidate(tt.src, tt.cands)
			if ok != (tt.want != "") || got.URL != tt.want {
				t.Errorf("PickCandidate = %q, %v; want %q", got.URL, ok, tt.want)
			}
		})
	}
}

func task(src domain.Source) *domain.Task { return &domain.Task{ID: "t1", Source: src} }

func TestPageReloadRefresh(t *testing.T) {
	ctx := context.Background()
	src := domain.Source{
		CurrentURL: "https://cdn.example/v/clip.mp4?token=old", PageURL: "https://example.com/watch?v=1",
		Headers: map[string]string{"X-A": "1"}, ResolverKind: domain.ResolverDirect,
		Fingerprint: domain.Fingerprint{Size: 5000, Filename: "clip.mp4", ContentType: "video/mp4"},
	}
	page := &fakePage{result: &browser.PageResult{Candidates: []browser.Candidate{
		cand("https://cdn.example/v/clip.mp4?token=new", browser.KindVideo, 5000, func(c *browser.Candidate) {
			c.Headers = map[string]string{"Referer": "https://example.com/watch?v=1", "Origin": "https://example.com"}
		}),
	}}}
	got, err := PageReload{Resolver: page}.Refresh(ctx, Request{Task: task(src)})
	if err != nil {
		t.Fatal(err)
	}
	if page.url != src.PageURL {
		t.Errorf("resolved %q, want the task's page", page.url)
	}
	s := got.Source
	if s.CurrentURL != "https://cdn.example/v/clip.mp4?token=new" || s.RedirectURL != "" || s.ResolverKind != domain.ResolverBrowser ||
		s.Referrer != "https://example.com/watch?v=1" || s.Headers["Origin"] != "https://example.com" || s.Headers["X-A"] != "1" ||
		s.Fingerprint != src.Fingerprint || s.PageURL != src.PageURL {
		t.Errorf("source = %+v", s)
	}
	if src.Headers["Origin"] != "" {
		t.Error("the task's header map was modified")
	}
	if got.Probe != nil {
		t.Error("page-reload returns no probe")
	}
}

func TestPageReloadSkipsAndFailures(t *testing.T) {
	ctx := context.Background()
	withPage := domain.Source{CurrentURL: "https://x/f.zip", PageURL: "https://x/page", Fingerprint: domain.Fingerprint{Size: 5}}
	page := &fakePage{result: &browser.PageResult{}}

	// Not applicable: no page, or the browser is off.
	if _, err := (PageReload{Resolver: page}).Refresh(ctx, Request{Task: task(domain.Source{CurrentURL: "https://x/f.zip"})}); !errors.Is(err, ErrNotApplicable) {
		t.Errorf("no page URL: %v, want ErrNotApplicable", err)
	}
	off := settings.Settings{BrowserUsage: settings.Some(settings.BrowserNever)}
	if _, err := (PageReload{Resolver: page}).Refresh(ctx, Request{Task: task(withPage), Settings: off}); !errors.Is(err, ErrNotApplicable) {
		t.Errorf("browserUsage=never: %v, want ErrNotApplicable", err)
	}
	if page.url != "" {
		t.Error("the browser was used although it is off")
	}
	// Failures: the page cannot be opened, or nothing on it matches.
	boom := errors.New("no browser")
	if _, err := (PageReload{Resolver: &fakePage{err: boom}}).Refresh(ctx, Request{Task: task(withPage)}); !errors.Is(err, boom) || errors.Is(err, ErrNotApplicable) {
		t.Errorf("resolver error: %v", err)
	}
	if _, err := (PageReload{Resolver: page}).Refresh(ctx, Request{Task: task(withPage)}); err == nil || errors.Is(err, ErrNotApplicable) {
		t.Errorf("no match: %v, want a failure", err)
	}
	if (PageReload{}).Name() != settings.RefreshPageReload {
		t.Error("wrong strategy name")
	}
}
