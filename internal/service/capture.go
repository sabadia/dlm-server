package service

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// MediaKind classifies a media candidate found on a page.
type MediaKind string

// Media kinds, from most to least preferred when the service must pick one.
const (
	MediaHLS   MediaKind = "hls"
	MediaDASH  MediaKind = "dash"
	MediaVideo MediaKind = "video"
	MediaAudio MediaKind = "audio"
	MediaFile  MediaKind = "file"
)

var mediaPreference = []MediaKind{MediaHLS, MediaDASH, MediaVideo, MediaAudio, MediaFile}

// Valid reports whether k is a known media kind.
func (k MediaKind) Valid() bool { return slices.Contains(mediaPreference, k) }

// MediaCandidate is a downloadable media resource the extension found on the
// page.
type MediaCandidate struct {
	URL  string
	Kind MediaKind
	MIME string
	// Size in bytes; <= 0 means unknown.
	Size int64
	// Variant identifies the stream variant (for example "1080p"); it becomes
	// the task's media hint.
	Variant string
}

// CaptureInput is a download captured by a browser extension, with the
// context needed to download it and to obtain the link again later.
type CaptureInput struct {
	// URL is the link the user clicked. It may be empty when MediaCandidates
	// is not: the best candidate is then downloaded.
	URL string
	// FinalURL is the URL after the browser followed redirects; it is the link
	// that is downloaded, while URL stays the task's original URL.
	FinalURL string
	// PageURL is the page the link was found on; Referrer is the Referer the
	// browser sent (it defaults to PageURL).
	PageURL  string
	Referrer string
	// Filename, MIME and Size are what the browser knows about the file; they
	// seed the task's fingerprint.
	Filename string
	MIME     string
	Size     int64
	// Cookies are stored in the cookie store before the task is queued, so the
	// first request already carries them.
	Cookies   []*domain.Cookie
	UserAgent string
	// Headers are request headers to repeat; Cookie and Host are dropped.
	Headers         map[string]string
	MediaCandidates []MediaCandidate

	QueueID  string
	Dir      string
	Settings settings.Settings
	Priority domain.Priority
	Labels   map[string]string
	Paused   bool
	// AllowDuplicate adds the task although another one has the same link.
	AllowDuplicate bool
}

// CaptureResult is the outcome of CaptureService.Capture.
type CaptureResult struct {
	Task *domain.Task
	// Cookies reports the import of CaptureInput.Cookies.
	Cookies cookies.ImportResult
	// Refreshed is true when Task is an existing task that was waiting for a
	// new link (refresh strategy extension-capture) and received the
	// captured one instead of a new task being created.
	Refreshed bool
}

// CaptureService creates tasks from downloads captured by the browser
// extension.
type CaptureService struct {
	tasks   *TaskService
	cookies *cookies.Store
	refresh *RefreshService
}

// NewCaptureService creates a CaptureService. refresh may be nil, which
// disables attaching captures to tasks that wait for a new link.
func NewCaptureService(tasks *TaskService, store *cookies.Store, refresh *RefreshService) *CaptureService {
	return &CaptureService{tasks: tasks, cookies: store, refresh: refresh}
}

// Capture stores the captured cookies and either gives the captured link to a
// matching task that waits for one (see RefreshService.AttachCapture) or
// creates a task whose Source holds
// the full capture context (original and final URL, page, referrer, headers,
// user agent, cookie domains, media hint, fingerprint seed). Invalid input is
// rejected (domain.ErrInvalidArgument) before anything is stored. Cookies
// that were stored stay stored when the task cannot be created afterwards.
func (s *CaptureService) Capture(ctx context.Context, in CaptureInput) (*CaptureResult, error) {
	src, err := captureSource(in)
	if err != nil {
		return nil, err
	}
	res, err := s.cookies.ImportCookies(ctx, in.Cookies)
	if err != nil {
		return nil, fmt.Errorf("service: capture: %w", err)
	}
	src.CookieDomains = cookieDomains(in.Cookies, src.CurrentURL)
	if s.refresh != nil {
		// A task waiting for a new link (extension-capture) that this
		// capture matches gets the link instead of a duplicate being created.
		waiting, err := s.refresh.AttachCapture(ctx, src)
		if err != nil {
			return nil, err
		}
		if waiting != nil {
			return &CaptureResult{Task: waiting, Cookies: res, Refreshed: true}, nil
		}
	}
	t, err := s.tasks.Create(ctx, CreateTaskInput{
		Source: src, QueueID: in.QueueID, Name: captureName(in.Filename), Dir: in.Dir,
		Settings: in.Settings, Priority: in.Priority, Labels: in.Labels, Paused: in.Paused, AllowDuplicate: in.AllowDuplicate,
	})
	if err != nil {
		return nil, err
	}
	return &CaptureResult{Task: t, Cookies: res}, nil
}

// captureSource builds and validates the Source of a capture, except for its
// cookie domains.
func captureSource(in CaptureInput) (domain.Source, error) {
	primary, hint, mime, size := in.URL, "", in.MIME, in.Size
	for i, c := range in.MediaCandidates {
		if c.URL == "" {
			return domain.Source{}, domain.InvalidArgument("service: capture: mediaCandidates[%d]: url is required", i)
		}
		if c.Kind != "" && !c.Kind.Valid() {
			return domain.Source{}, domain.InvalidArgument("service: capture: mediaCandidates[%d]: unknown kind %q", i, c.Kind)
		}
	}
	var chosen MediaCandidate
	var found bool
	if primary == "" {
		if chosen, found = pickCandidate(in.MediaCandidates); !found {
			return domain.Source{}, domain.InvalidArgument("service: capture: url or mediaCandidates is required")
		}
		primary = chosen.URL
	} else if i := slices.IndexFunc(in.MediaCandidates, func(c MediaCandidate) bool { return c.URL == primary }); i >= 0 {
		chosen, found = in.MediaCandidates[i], true // the clicked link is a known candidate
	}
	if found {
		hint, mime = chosen.Variant, cmpOr(mime, chosen.MIME)
		if size <= 0 {
			size = chosen.Size
		}
	}
	current := cmpOr(in.FinalURL, primary)
	src := domain.Source{
		CurrentURL:   current,
		OriginalURL:  primary,
		PageURL:      in.PageURL,
		Referrer:     cmpOr(in.Referrer, in.PageURL),
		Headers:      captureHeaders(in.Headers),
		UserAgent:    in.UserAgent,
		MediaHint:    hint,
		ResolverKind: domain.ResolverDirect,
		Fingerprint: domain.Fingerprint{
			Size:        max(size, 0),
			Filename:    captureName(in.Filename),
			ContentType: mime,
		},
	}
	if in.FinalURL != "" && in.FinalURL != primary {
		src.RedirectURL = in.FinalURL
	}
	if err := src.Validate(); err != nil {
		return domain.Source{}, fmt.Errorf("service: capture: %w", err)
	}
	for _, u := range []string{src.CurrentURL, src.OriginalURL} {
		if p, err := url.Parse(u); err != nil || p.Host == "" {
			return domain.Source{}, domain.InvalidArgument("service: capture: %q is not an absolute URL with a host", u)
		}
	}
	return src, nil
}

// pickCandidate returns the preferred candidate: by kind (see
// mediaPreference; an unset kind ranks as a plain file), then the largest
// known size, then the first.
func pickCandidate(cs []MediaCandidate) (MediaCandidate, bool) {
	rank := func(k MediaKind) int {
		if i := slices.Index(mediaPreference, k); i >= 0 {
			return i
		}
		return slices.Index(mediaPreference, MediaFile)
	}
	best := -1
	for i, c := range cs {
		switch {
		case best < 0, rank(c.Kind) < rank(cs[best].Kind),
			rank(c.Kind) == rank(cs[best].Kind) && c.Size > cs[best].Size:
			best = i
		}
	}
	if best < 0 {
		return MediaCandidate{}, false
	}
	return cs[best], true
}

// captureHeaders copies headers without the ones the download must not
// replay: Cookie (the cookie store supplies fresh values) and Host.
func captureHeaders(h map[string]string) map[string]string {
	var out map[string]string
	for k, v := range h {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "cookie", "host", "":
			continue
		}
		if out == nil {
			out = make(map[string]string, len(h))
		}
		out[k] = v
	}
	return out
}

// captureName reduces a browser-supplied file name to its base name.
func captureName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// cookieDomains lists the distinct cookie domains of cs, or the host of
// fallbackURL when there are none.
func cookieDomains(cs []*domain.Cookie, fallbackURL string) []string {
	var out []string
	for _, c := range cs {
		d := domain.CanonicalDomain(c.Domain)
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		if u, err := url.Parse(fallbackURL); err == nil && u.Hostname() != "" {
			out = []string{domain.CanonicalDomain(u.Hostname())}
		}
	}
	return out
}
