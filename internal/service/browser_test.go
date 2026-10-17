package service

import (
	"context"
	"errors"
	"testing"

	"github.com/sabadia/dlm-server/internal/browser"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/browserx"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/netclient"
)

type fakeBackend struct {
	result   *browser.PageResult
	err      error
	cands    map[string]browser.Candidate
	profiles []string
	deleted  string
	gotOpts  browser.ResolveOptions
}

func (f *fakeBackend) ResolvePage(_ context.Context, _ string, opts browser.ResolveOptions) (*browser.PageResult, error) {
	f.gotOpts = opts
	return f.result, f.err
}

func (f *fakeBackend) Candidate(id string, index int) (browser.Candidate, error) {
	c, ok := f.cands[browserx.URL(id, index)]
	if !ok {
		return browser.Candidate{}, browser.ErrResultGone
	}
	return c, nil
}
func (f *fakeBackend) Status() browser.Status           { return browser.Status{Max: 2} }
func (f *fakeBackend) Profiles() ([]string, error)      { return f.profiles, f.err }
func (f *fakeBackend) DeleteProfile(group string) error { f.deleted = group; return f.err }

func newBrowserService(t *testing.T, fb *fakeBackend) (*BrowserService, *svcFixture) {
	t.Helper()
	f := newSvcFixture(t)
	clients := netclient.New(netclient.Options{})
	t.Cleanup(func() { _ = clients.Close() })
	reg, err := engine.NewRegistry(browserx.New(fb), httpx.New(engine.Env{Clients: clients}))
	if err != nil {
		t.Fatal(err)
	}
	tasks := NewTaskService(Deps{Store: f.store, Engines: reg, Defaults: f.tasks.d.Defaults})
	return NewBrowserService(fb, tasks), f
}

func TestBrowserDownloadFromAURL(t *testing.T) {
	fb := &fakeBackend{cands: map[string]browser.Candidate{
		browserx.URL("r1", 0): {
			Index: 0, URL: "https://cdn.example/v/clip.mp4?t=1", Kind: browser.KindVideo, MIME: "video/mp4", Size: 5000,
			Filename: "dir/clip.mp4", Page: "https://example.com/watch", Headers: map[string]string{"Referer": "https://example.com/watch", "Origin": "https://example.com"},
		},
	}}
	s, f := newBrowserService(t, fb)
	task, err := s.Download(t.Context(), BrowserDownloadInput{ResultID: "r1", Index: 0, Paused: true, Priority: domain.PriorityHigh, Dir: f.dir})
	if err != nil {
		t.Fatal(err)
	}
	src := task.Source
	if src.CurrentURL != "https://cdn.example/v/clip.mp4?t=1" || src.OriginalURL != src.CurrentURL || src.PageURL != "https://example.com/watch" ||
		src.Referrer != "https://example.com/watch" || src.Headers["Origin"] != "https://example.com" || src.ResolverKind != domain.ResolverBrowser ||
		len(src.CookieDomains) != 1 || src.CookieDomains[0] != "cdn.example" ||
		src.Fingerprint.Size != 5000 || src.Fingerprint.Filename != "clip.mp4" || src.Fingerprint.ContentType != "video/mp4" {
		t.Errorf("source = %+v", src)
	}
	if task.Engine != "http" || task.Name != "clip.mp4" || task.Status != domain.StatusPaused || task.Priority != domain.PriorityHigh {
		t.Errorf("task = %+v", task)
	}
}

func TestBrowserDownloadOfAStagedFile(t *testing.T) {
	fb := &fakeBackend{cands: map[string]browser.Candidate{
		browserx.URL("r1", 2): {
			Index: 2, URL: "blob:https://example.com/abc", Kind: browser.KindFile, Source: "blob", Filename: "made.txt",
			Page: "https://example.com/p", File: "/stage/guid", Size: 12, MIME: "text/plain",
		},
	}}
	s, _ := newBrowserService(t, fb)
	task, err := s.Download(t.Context(), BrowserDownloadInput{ResultID: "r1", Index: 2, Name: "renamed.txt", Paused: true})
	if err != nil {
		t.Fatal(err)
	}
	if task.Engine != "browser" || task.Source.CurrentURL != browserx.URL("r1", 2) || task.Source.OriginalURL != "blob:https://example.com/abc" ||
		task.Name != "renamed.txt" || task.Source.Fingerprint.Size != 12 || task.Source.PageURL != "https://example.com/p" {
		t.Errorf("task = %+v", task)
	}
}

func TestBrowserDownloadErrors(t *testing.T) {
	fb := &fakeBackend{cands: map[string]browser.Candidate{
		browserx.URL("r1", 0): {URL: "https://x/master.m3u8", Kind: browser.KindHLS},
		browserx.URL("r1", 1): {URL: "https://x/m.mpd", Kind: browser.KindDASH},
	}}
	s, _ := newBrowserService(t, fb)
	for _, idx := range []int{0, 1} {
		if _, err := s.Download(t.Context(), BrowserDownloadInput{ResultID: "r1", Index: idx}); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("stream candidate %d: %v, want ErrInvalidArgument", idx, err)
		}
	}
	if _, err := s.Download(t.Context(), BrowserDownloadInput{ResultID: "expired", Index: 0}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("expired result: %v, want ErrNotFound", err)
	}
}

func TestBrowserResolvePageErrorMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want func(error) bool
	}{
		"no browser": {browser.ErrNoBrowser, func(e error) bool { return errors.Is(e, ErrBrowserUnavailable) }},
		"closed":     {browser.ErrClosed, func(e error) bool { return errors.Is(e, ErrBrowserUnavailable) }},
		"bad url":    {domain.InvalidArgument("nope"), func(e error) bool { return errors.Is(e, domain.ErrInvalidArgument) }},
		"cancelled":  {context.Canceled, func(e error) bool { return errors.Is(e, context.Canceled) }},
		"page failed": {errors.New("net::ERR_NAME_NOT_RESOLVED"), func(e error) bool {
			var ee *engine.Error
			return errors.As(e, &ee) && ee.Code == domain.ErrorCodeBrowser
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newBrowserService(t, &fakeBackend{err: tc.err})
			if _, err := s.ResolvePage(t.Context(), ResolvePageInput{URL: "https://x/"}); !tc.want(err) {
				t.Errorf("ResolvePage error = %v", err)
			}
		})
	}
	fb := &fakeBackend{result: &browser.PageResult{ID: "r", Title: "t"}}
	s, _ := newBrowserService(t, fb)
	res, err := s.ResolvePage(t.Context(), ResolvePageInput{URL: "https://x/", Wait: 7})
	if err != nil || res.ID != "r" || fb.gotOpts.Wait != 7 {
		t.Errorf("ResolvePage = %+v, %v (options %+v)", res, err, fb.gotOpts)
	}
}

func TestBrowserStatusAndProfiles(t *testing.T) {
	fb := &fakeBackend{profiles: []string{"a.com"}}
	s, _ := newBrowserService(t, fb)
	if st := s.Status(); st.Max != 2 {
		t.Errorf("Status = %+v", st)
	}
	if p, err := s.Profiles(); err != nil || len(p) != 1 {
		t.Errorf("Profiles = %v, %v", p, err)
	}
	if err := s.DeleteProfile("a.com"); err != nil || fb.deleted != "a.com" {
		t.Errorf("DeleteProfile: %v (deleted %q)", err, fb.deleted)
	}
	fb.err = domain.NotFound("browser profile", "x")
	if err := s.DeleteProfile("x"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("DeleteProfile of a missing profile = %v", err)
	}
}
