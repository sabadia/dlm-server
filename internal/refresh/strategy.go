package refresh

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/settings"
)

// ErrNotApplicable is returned by a Strategy that cannot work for a task
// (for example origin-redirect without an original URL). The chain records a
// skip and moves on to the next strategy.
var ErrNotApplicable = errors.New("refresh: strategy not applicable")

// Request is the input of Strategy.Refresh.
type Request struct {
	// Task is a snapshot of the task; strategies must not modify it.
	Task *domain.Task
	// Settings are the effective settings of the task.
	Settings settings.Settings
}

// Candidate is a new link a strategy found, before it was validated.
type Candidate struct {
	// Source is the task's source with the new link and context (URL,
	// headers, referrer…). Its Fingerprint is the task's old one: a
	// candidate is checked against what was downloaded so far.
	Source domain.Source
	// Probe, when set, is an open probe of Source, which saves validation a
	// second request (one-time links). The receiver of the Candidate owns it.
	Probe *engine.Probe
}

// Strategy obtains a new link for a task whose link expired. The built-in
// strategies are origin-redirect here; page-reload (browser) and plugin
// register themselves in later phases. extension-capture and manual need an
// external event and are handled by the service, not by a Strategy.
type Strategy interface {
	// Name is the settings.RefreshStrategy the strategy implements.
	Name() settings.RefreshStrategy
	// Refresh returns a candidate link, ErrNotApplicable, or an error (a
	// classified *engine.Error when the origin answered).
	Refresh(ctx context.Context, req Request) (Candidate, error)
}

// OriginRedirect re-requests the task's original URL, which usually
// redirects to a fresh signed link. The probe's final URL becomes the new
// link.
type OriginRedirect struct {
	Engines *engine.Registry
}

// Name implements Strategy.
func (OriginRedirect) Name() settings.RefreshStrategy { return settings.RefreshOriginRedirect }

// Refresh implements Strategy.
func (o OriginRedirect) Refresh(ctx context.Context, req Request) (Candidate, error) {
	src := req.Task.Source
	if src.OriginalURL == "" {
		return Candidate{}, fmt.Errorf("%w: the task has no original URL", ErrNotApplicable)
	}
	probeSrc := src
	probeSrc.CurrentURL, probeSrc.RedirectURL = src.OriginalURL, ""
	eng, err := o.engineFor(req.Task, probeSrc)
	if err != nil {
		return Candidate{}, err
	}
	probe, err := eng.Resolve(ctx, engine.Request{Source: probeSrc, Settings: req.Settings})
	if err != nil {
		return Candidate{}, fmt.Errorf("origin-redirect: %w", err)
	}
	cand := src
	cand.CurrentURL, cand.RedirectURL = src.OriginalURL, ""
	if probe.Source.RedirectURL != "" {
		cand.CurrentURL = probe.Source.RedirectURL
	}
	return Candidate{Source: cand, Probe: probe}, nil
}

func (o OriginRedirect) engineFor(task *domain.Task, src domain.Source) (engine.Engine, error) {
	if task.Engine != "" {
		e, err := o.Engines.Get(task.Engine)
		if err != nil {
			return nil, fmt.Errorf("origin-redirect: %w", err)
		}
		return e, nil
	}
	e, err := o.Engines.Select(src)
	if err != nil {
		return nil, fmt.Errorf("origin-redirect: %w", err)
	}
	return e, nil
}

// withoutURLName clears the fingerprint's file name when it is just the last
// path segment of one of urls. Such a name comes from the link, not from the
// file (no Content-Disposition), and a refreshed link usually has another
// path, so comparing it would report a different file where there is none.
func withoutURLName(fp domain.Fingerprint, urls ...string) domain.Fingerprint {
	if fp.Filename == "" {
		return fp
	}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if name, err := url.PathUnescape(path.Base(u.Path)); err == nil && strings.EqualFold(name, fp.Filename) {
			fp.Filename = ""
			break
		}
	}
	return fp
}

// Validation is the outcome of checking a candidate link.
type Validation struct {
	// Probe is the open probe of the candidate; the caller must close it.
	Probe *engine.Probe
	// Match is true when the candidate serves the content downloaded so far
	// (or nothing contradicts it).
	Match bool
	// Reason explains a mismatch.
	Reason string
}

// Validate probes the candidate (unless it carries a probe) and compares the
// fingerprint of what it serves (size, strong ETag, filename) with the
// task's. A probe failure is returned as an error and nothing is left open.
func Validate(ctx context.Context, eng engine.Engine, task *domain.Task, c Candidate, eff settings.Settings) (Validation, error) {
	probe := c.Probe
	if probe == nil {
		var err error
		if probe, err = eng.Resolve(ctx, engine.Request{Source: c.Source, Settings: eff}); err != nil {
			return Validation{}, fmt.Errorf("validate new link: %w", err)
		}
	}
	have := withoutURLName(task.Source.Fingerprint, task.Source.CurrentURL, task.Source.RedirectURL, task.Source.OriginalURL)
	learnt := withoutURLName(probe.Source.Fingerprint, probe.Source.CurrentURL, probe.Source.RedirectURL, c.Source.CurrentURL)
	match, reason := have.Matches(learnt)
	if match && task.Downloaded > 0 && task.Size > 0 && probe.Resource.Size > 0 && probe.Resource.Size != task.Size {
		match, reason = false, fmt.Sprintf("size differs: %d != %d", task.Size, probe.Resource.Size)
	}
	return Validation{Probe: probe, Match: match, Reason: reason}, nil
}
