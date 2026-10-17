package refresh

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/sabadia/dlm-server/internal/browser"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// PageResolver opens a page in the headless browser and reports the downloads
// it leads to (browser.Resolver).
type PageResolver interface {
	ResolvePage(ctx context.Context, pageURL string, opts browser.ResolveOptions) (*browser.PageResult, error)
}

// minMatchScore is the evidence a page download needs to be taken for the
// task's file: one strong sign (the media hint, the size or the file name),
// or the same path plus a compatible kind.
const minMatchScore = 3

// PageReload reopens the page the task's link came from, in the headless
// browser with the stored cookies, and takes the download on it that matches
// the task: the same media hint, size, file name or path. It is the strategy
// for links that only the page can hand out (tokens made by scripts).
type PageReload struct {
	Resolver PageResolver
}

// Name implements Strategy.
func (PageReload) Name() settings.RefreshStrategy { return settings.RefreshPageReload }

// Refresh implements Strategy.
func (p PageReload) Refresh(ctx context.Context, req Request) (Candidate, error) {
	src := req.Task.Source
	if req.Settings.BrowserUsage.OrElse(settings.BrowserAuto) == settings.BrowserNever {
		return Candidate{}, fmt.Errorf("%w: the browser is turned off for this task (browserUsage=never)", ErrNotApplicable)
	}
	if src.PageURL == "" {
		return Candidate{}, fmt.Errorf("%w: the task has no page URL", ErrNotApplicable)
	}
	res, err := p.Resolver.ResolvePage(ctx, src.PageURL, browser.ResolveOptions{})
	if err != nil {
		return Candidate{}, fmt.Errorf("page-reload: %w", err)
	}
	picked, ok := PickCandidate(src, res.Candidates)
	if !ok {
		return Candidate{}, fmt.Errorf("page-reload: none of the %d downloads on %s matches the task", len(res.Candidates), src.PageURL)
	}
	next := src
	next.CurrentURL, next.RedirectURL = picked.URL, ""
	next.ResolverKind = domain.ResolverBrowser
	if ref := picked.Headers["Referer"]; ref != "" {
		next.Referrer = ref
	}
	if origin := picked.Headers["Origin"]; origin != "" {
		if _, set := src.Headers["Origin"]; !set {
			next.Headers = withHeader(src.Headers, "Origin", origin)
		}
	}
	return Candidate{Source: next}, nil
}

func withHeader(h map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(h)+1)
	for hk, hv := range h {
		out[hk] = hv
	}
	out[k] = v
	return out
}

// PickCandidate chooses the download on a page that is the file of src, or
// reports false. Browser blobs cannot be fetched by URL and are never
// chosen; a candidate whose size contradicts the fingerprint is rejected.
func PickCandidate(src domain.Source, cands []browser.Candidate) (browser.Candidate, bool) {
	best, bestScore := -1, 0
	for i, c := range cands {
		if score, ok := scoreCandidate(src, c); ok && score >= minMatchScore && score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return browser.Candidate{}, false
	}
	return cands[best], true
}

func scoreCandidate(src domain.Source, c browser.Candidate) (int, bool) {
	if strings.HasPrefix(c.URL, "blob:") {
		return 0, false
	}
	fp := src.Fingerprint
	if fp.Size > 0 && c.Size > 0 && c.Size != fp.Size {
		return 0, false
	}
	cu, err := url.Parse(c.URL)
	if err != nil {
		return 0, false
	}
	score := 0
	if hint := src.MediaHint; hint != "" && hintMatches(hint, c) {
		score += 4
	}
	if fp.Size > 0 && c.Size == fp.Size {
		score += 3
	}
	base := path.Base(cu.Path)
	if fp.Filename != "" && (strings.EqualFold(base, fp.Filename) || strings.EqualFold(c.Filename, fp.Filename)) {
		score += 3
	}
	for _, old := range []string{src.CurrentURL, src.RedirectURL, src.OriginalURL} {
		ou, err := url.Parse(old)
		if err != nil || old == "" {
			continue
		}
		if ou.Path == cu.Path && ou.Path != "" && ou.Path != "/" {
			score += 2
			break
		}
	}
	if kindCompatible(fp.ContentType, c) {
		score++
	}
	return score, true
}

// hintMatches reports whether a media hint (a regular expression, else a
// plain substring) selects the candidate: it is tried against the URL and the
// file name.
func hintMatches(hint string, c browser.Candidate) bool {
	for _, s := range []string{c.URL, c.Filename} {
		if s == "" {
			continue
		}
		if re, err := regexp.Compile(hint); err == nil {
			if re.MatchString(s) {
				return true
			}
		} else if strings.Contains(s, hint) {
			return true
		}
	}
	return false
}

func kindCompatible(contentType string, c browser.Candidate) bool {
	ct := strings.ToLower(contentType)
	switch c.Kind {
	case browser.KindVideo:
		return strings.HasPrefix(ct, "video/")
	case browser.KindAudio:
		return strings.HasPrefix(ct, "audio/")
	case browser.KindHLS:
		return strings.Contains(ct, "mpegurl")
	case browser.KindDASH:
		return strings.Contains(ct, "dash")
	case browser.KindFile:
		return ct != "" && !strings.HasPrefix(ct, "video/") && !strings.HasPrefix(ct, "audio/") && !strings.HasPrefix(ct, "text/html")
	default:
		return false
	}
}
