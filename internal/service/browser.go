package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/browser"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/browserx"
	"github.com/sabadia/dlm-server/internal/settings"
)

// ErrBrowserUnavailable is returned when no headless browser can be used: none
// is installed and downloading chrome-headless-shell is not possible or not
// allowed.
var ErrBrowserUnavailable = errors.New("service: the headless browser is not available")

// BrowserBackend is the headless browser as the service sees it
// (browser.Resolver).
type BrowserBackend interface {
	ResolvePage(ctx context.Context, pageURL string, opts browser.ResolveOptions) (*browser.PageResult, error)
	Candidate(id string, index int) (browser.Candidate, error)
	Status() browser.Status
	Profiles() ([]string, error)
	DeleteProfile(group string) error
}

// BrowserService opens pages in the headless browser and creates tasks from
// the downloads found on them.
type BrowserService struct {
	b     BrowserBackend
	tasks *TaskService
}

// NewBrowserService creates a BrowserService.
func NewBrowserService(b BrowserBackend, tasks *TaskService) *BrowserService {
	return &BrowserService{b: b, tasks: tasks}
}

// ResolvePageInput is the request of BrowserService.ResolvePage.
type ResolvePageInput struct {
	URL string
	// Wait bounds how long the page may keep producing downloads; zero uses
	// the default.
	Wait time.Duration
}

// ResolvePage opens a page in the headless browser, with the stored cookies
// of its site, and lists the downloads it led to. The result (and the files
// the browser downloaded itself) is kept for a while: pass its ID and a
// candidate index to Download.
func (s *BrowserService) ResolvePage(ctx context.Context, in ResolvePageInput) (*browser.PageResult, error) {
	res, err := s.b.ResolvePage(ctx, in.URL, browser.ResolveOptions{Wait: in.Wait})
	if err != nil {
		return nil, browserError(err)
	}
	return res, nil
}

// BrowserDownloadInput is the request of BrowserService.Download.
type BrowserDownloadInput struct {
	// ResultID and Index name a candidate of a ResolvePage result.
	ResultID string
	Index    int

	QueueID  string
	Name     string
	Dir      string
	Settings settings.Settings
	Priority domain.Priority
	Labels   map[string]string
	Paused   bool
}

// Download creates a task for a candidate found on a page. A file the
// browser downloaded itself (a blob: download, an attachment) is imported
// from its staging place; anything else is downloaded from its URL by the
// engine that matches it, with the page as referrer and origin. HLS
// playlists (URLs ending in .m3u8) go to the HLS engine; DASH is refused
// until its engine exists.
func (s *BrowserService) Download(ctx context.Context, in BrowserDownloadInput) (*domain.Task, error) {
	res, err := s.b.Candidate(in.ResultID, in.Index)
	if err != nil {
		return nil, browserError(err)
	}
	src, err := s.sourceFor(in, res)
	if err != nil {
		return nil, err
	}
	name := in.Name
	if name == "" {
		name = captureName(res.Filename)
	}
	return s.tasks.Create(ctx, CreateTaskInput{
		Source: src, QueueID: in.QueueID, Name: name, Dir: in.Dir, Settings: in.Settings,
		Priority: in.Priority, Labels: in.Labels, Paused: in.Paused,
	})
}

func (s *BrowserService) sourceFor(in BrowserDownloadInput, c browser.Candidate) (domain.Source, error) {
	fp := domain.Fingerprint{Filename: captureName(c.Filename), ContentType: c.MIME}
	if c.Size > 0 {
		fp.Size = c.Size
	}
	if c.File != "" {
		// The browser has the file already; importing it is instant and does
		// not need the (possibly one-time) URL again.
		return domain.Source{
			CurrentURL: browserx.URL(in.ResultID, in.Index), OriginalURL: c.URL, PageURL: c.Page,
			ResolverKind: domain.ResolverBrowser, Fingerprint: fp,
		}, nil
	}
	switch c.Kind {
	case browser.KindDASH:
		return domain.Source{}, domain.InvalidArgument("service: DASH streams cannot be downloaded yet (the DASH engine arrives in a later release)")
	case browser.KindHLS:
		if u, err := url.Parse(c.URL); err != nil || !strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") {
			return domain.Source{}, domain.InvalidArgument("service: the HLS playlist %q has no .m3u8 extension, so no engine can tell it is one", c.URL)
		}
	default:
	}
	src := domain.Source{
		CurrentURL: c.URL, OriginalURL: c.URL, PageURL: c.Page, Referrer: c.Headers["Referer"],
		ResolverKind: domain.ResolverBrowser, Fingerprint: fp,
	}
	if origin := c.Headers["Origin"]; origin != "" {
		src.Headers = map[string]string{"Origin": origin}
	}
	if u, err := url.Parse(c.URL); err == nil && u.Hostname() != "" {
		src.CookieDomains = []string{strings.ToLower(u.Hostname())}
	}
	return src, nil
}

// BrowserStatus is the response of BrowserService.Status.
type BrowserStatus = browser.Status

// Status reports the browser in use (empty until it was first needed) and the
// running pool.
func (s *BrowserService) Status() BrowserStatus { return s.b.Status() }

// Profiles lists the site groups that have a browser profile.
func (s *BrowserService) Profiles() ([]string, error) {
	p, err := s.b.Profiles()
	if err != nil {
		return nil, fmt.Errorf("service: list browser profiles: %w", err)
	}
	return p, nil
}

// DeleteProfile removes the profile (cookies, storage) of a site group; a
// group whose browser is running conflicts, a missing profile is not found.
func (s *BrowserService) DeleteProfile(group string) error {
	if err := s.b.DeleteProfile(group); err != nil {
		return fmt.Errorf("service: delete browser profile: %w", err)
	}
	return nil
}

// browserError maps errors of the browser package to the service's:
// domain errors pass through, a missing browser becomes
// ErrBrowserUnavailable, an expired result is not found, and a page that
// could not be opened is a (fatal, "browser") origin error.
func browserError(err error) error {
	switch {
	case errors.Is(err, domain.ErrInvalidArgument), errors.Is(err, domain.ErrNotFound),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, browser.ErrNoBrowser), errors.Is(err, browser.ErrClosed):
		return fmt.Errorf("%w: %w", ErrBrowserUnavailable, err)
	case errors.Is(err, browser.ErrResultGone):
		return domain.NotFound("browser result", err.Error())
	default:
		return engine.Fatal(domain.ErrorCodeBrowser, err)
	}
}
