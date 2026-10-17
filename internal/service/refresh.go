package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/refresh"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
)

// ErrRefreshDisabled is returned by RefreshService.HandleExpired when the
// task's effective settings turn automatic refresh off (refresh.auto=false):
// the caller fails the task as it would without a refresh layer.
var ErrRefreshDisabled = errors.New("service: automatic link refresh is disabled")

// defaultAwaitSweep is how often Run looks for waits that timed out when no
// deadline is closer.
const defaultAwaitSweep = 5 * time.Second

// RefreshService obtains a new link for tasks whose link expired. It runs the
// configured strategy chain, validates the new link against the task's
// fingerprint, applies it (so the download resumes from its segments), keeps
// the per-attempt history, and handles the two strategies that wait for the
// outside world: extension-capture (a matching browser capture arrives) and
// manual (the user sets a link).
type RefreshService struct {
	d          Deps
	strategies map[settings.RefreshStrategy]refresh.Strategy
	sweep      time.Duration
	kick       chan struct{}
}

// NewRefreshService creates a RefreshService with the given active
// strategies (origin-redirect, and later page-reload and plugin). Strategies
// named in refresh.strategies but not registered are skipped with a history
// entry.
func NewRefreshService(d Deps, strategies ...refresh.Strategy) *RefreshService {
	s := &RefreshService{
		d:          d.withDefaults(),
		strategies: make(map[settings.RefreshStrategy]refresh.Strategy, len(strategies)),
		sweep:      defaultAwaitSweep,
		kick:       make(chan struct{}, 1),
	}
	for _, st := range strategies {
		s.strategies[st.Name()] = st
	}
	return s
}

// SetSourceInput is the request of RefreshService.SetSource.
type SetSourceInput struct {
	// Source holds the new link (CurrentURL is required) and any context to
	// change: PageURL, Referrer, Method, Body, Headers, UserAgent,
	// CookieDomains, MediaHint, ResolverKind. Fields left empty keep the
	// task's values; OriginalURL is kept unless set.
	Source domain.Source
	// OnMismatch overrides refresh.onMismatch for this request; empty uses the
	// task's effective setting. "ask" counts as "fail" here: the caller is the
	// one being asked.
	OnMismatch settings.MismatchPolicy
}

// chainResult is how a refresh chain ended.
type chainResult int

const (
	chainRefreshed chainResult = iota // a new link was applied and the task is queued again
	chainAwaiting                     // the task waits for a capture or a manual link
	chainExhausted                    // no strategy produced a usable link
)

// ---- entry points ----

// HandleExpired is called by the Runner when a download of taskID (running)
// failed with an expired link. It runs the chain: the task is refreshing
// while strategies run, and ends queued (refreshed), awaiting_link (waiting)
// or error (exhausted). It returns ErrRefreshDisabled when refresh.auto is
// off, in which case the task is untouched. A cancelled ctx (pause, shutdown)
// is returned as is, leaving the task refreshing for the Runner to settle.
func (s *RefreshService) HandleExpired(ctx context.Context, taskID string, cause error) error {
	t, eff, err := s.load(ctx, taskID)
	if err != nil {
		return err
	}
	if !eff.Settings.Refresh.Auto.OrElse(true) {
		return ErrRefreshDisabled
	}
	_, err = s.runChain(ctx, t, eff, domain.RefreshCauseExpired, "", cause)
	return err
}

// Refresh runs the strategy chain for a task now (POST …/refresh). The task
// must not be running; an awaiting_link task goes back to waiting when no
// strategy helps, a failed, paused or queued task keeps its state. A refresh
// that found no usable link returns a link-expired *engine.Error with the
// reasons.
func (s *RefreshService) Refresh(ctx context.Context, taskID string) (*domain.Task, error) {
	t, eff, err := s.load(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if err := refreshable(t); err != nil {
		return nil, err
	}
	res, err := s.runChain(ctx, t, eff, domain.RefreshCauseManual, "", nil)
	if err != nil {
		return nil, err
	}
	out, gerr := s.d.Store.Tasks().Get(ctx, taskID)
	if gerr != nil {
		return nil, fmt.Errorf("service: refresh task: %w", gerr)
	}
	if res == chainExhausted {
		return out, engine.LinkExpired(errors.New("service: no refresh strategy produced a usable link; see the refresh history"))
	}
	return out, nil
}

// Await puts a failed or awaiting_link task into the awaiting_link state to
// wait for a matching browser capture, with refresh.awaitTimeout as the
// deadline (POST …/refresh/await). On an awaiting_link task it restarts the
// wait. Running tasks conflict: the wait starts by itself when their link
// expires.
func (s *RefreshService) Await(ctx context.Context, taskID string) (*domain.Task, error) {
	t, eff, err := s.load(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t.Status != domain.StatusError && t.Status != domain.StatusAwaitingLink {
		return nil, domain.Conflict("service: task %q is %s; only a failed or awaiting_link task can wait for a new link", taskID, t.Status)
	}
	if err := s.parkFrom(ctx, t, eff, domain.RefreshCauseManual, settings.RefreshExtensionCapture); err != nil {
		return nil, err
	}
	return s.d.Store.Tasks().Get(ctx, taskID) //nolint:wrapcheck // plain store read
}

// SetSource replaces a task's link by hand (PATCH …/source). The new link is
// probed and compared with the task's fingerprint; on a match the download
// resumes from its segments, on a mismatch refresh.onMismatch (or
// in.OnMismatch) decides: restart discards the partial data, fail rejects the
// link (domain.ErrConflict). The task must not be running or completed.
func (s *RefreshService) SetSource(ctx context.Context, taskID string, in SetSourceInput) (*domain.Task, error) {
	t, eff, err := s.load(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if err := refreshable(t); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Source.CurrentURL) == "" {
		return nil, domain.InvalidArgument("service: set source: currentUrl is required")
	}
	switch in.OnMismatch {
	case "", settings.MismatchRestart, settings.MismatchFail, settings.MismatchAsk:
	default:
		return nil, domain.InvalidArgument("service: set source: unknown onMismatch %q", in.OnMismatch)
	}
	cand := mergeSource(t.Source, in.Source)
	if err := cand.Validate(); err != nil {
		return nil, fmt.Errorf("service: set source: %w", err)
	}
	policy := cmp.Or(in.OnMismatch, eff.Settings.Refresh.OnMismatch.OrElse(settings.MismatchFail))
	if policy == settings.MismatchAsk {
		policy = settings.MismatchFail
	}
	applied, err := s.tryCandidate(ctx, t, eff, refresh.Candidate{Source: cand}, domain.RefreshCauseSource, settings.RefreshManual, policy)
	if err != nil {
		return nil, err
	}
	if !applied.ok {
		return nil, applied.err
	}
	return s.d.Store.Tasks().Get(ctx, taskID) //nolint:wrapcheck // plain store read
}

// AttachCapture offers a browser capture to the tasks waiting for a new link
// through extension-capture. The first task whose fingerprint the capture
// matches (same size, strong ETag or file name, and nothing contradicting)
// and whose probe of the captured link validates gets the new link and is
// queued again. It returns that task, or nil when no waiting task matched
// (the caller then creates a new task). src is the captured Source; its
// Fingerprint carries what the browser knows.
func (s *RefreshService) AttachCapture(ctx context.Context, src domain.Source) (*domain.Task, error) {
	waiting, err := s.awaitingCapture(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range waiting {
		if !captureMatches(t, src.Fingerprint) {
			continue
		}
		t, eff, err := s.load(ctx, t.ID)
		if err != nil || t.Status != domain.StatusAwaitingLink {
			continue // deleted or settled meanwhile
		}
		policy := eff.Settings.Refresh.OnMismatch.OrElse(settings.MismatchFail)
		if policy == settings.MismatchAsk {
			policy = settings.MismatchFail // a capture is not a decision; never discard data unasked
		}
		cand := mergeSource(t.Source, src)
		applied, err := s.tryCandidate(ctx, t, eff, refresh.Candidate{Source: cand}, domain.RefreshCauseCapture, settings.RefreshExtensionCapture, policy)
		if err != nil {
			return nil, err
		}
		if applied.ok {
			return s.d.Store.Tasks().Get(ctx, t.ID) //nolint:wrapcheck // plain store read
		}
	}
	return nil, nil //nolint:nilnil // no waiting task matched: not an error
}

// History returns a task's refresh history, oldest first.
func (s *RefreshService) History(ctx context.Context, taskID string) ([]*domain.RefreshAttempt, error) {
	if _, err := s.d.Store.Tasks().Get(ctx, taskID); err != nil {
		return nil, fmt.Errorf("service: refresh history: %w", err)
	}
	h, err := s.d.Store.RefreshHistory().List(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("service: refresh history: %w", err)
	}
	return h, nil
}

// Run ends the waits that timed out until ctx is done: a task whose
// extension-capture wait passed its deadline continues with the strategies
// after it, and fails when there are none.
func (s *RefreshService) Run(ctx context.Context) error {
	for {
		next := s.sweepOnce(ctx)
		wait := s.sweep
		if !next.IsZero() {
			if d := next.Sub(s.d.now()); d < wait {
				wait = max(d, time.Millisecond)
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-s.kick:
			t.Stop()
		case <-t.C:
		}
	}
}

// sweepOnce continues every timed-out wait and returns the earliest deadline
// still pending (zero when none).
func (s *RefreshService) sweepOnce(ctx context.Context) time.Time {
	pending, err := s.d.Store.RefreshHistory().PendingAwaits(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.d.Logger.WarnContext(ctx, "refresh: cannot list pending waits", "error", err)
		}
		return time.Time{}
	}
	var next time.Time
	now := s.d.now()
	for _, w := range pending {
		if w.Deadline.After(now) {
			if next.IsZero() || w.Deadline.Before(next) {
				next = *w.Deadline
			}
			continue
		}
		if err := s.expireWait(ctx, w); err != nil && ctx.Err() == nil {
			s.d.Logger.WarnContext(ctx, "refresh: cannot continue after a timed-out wait", "task_id", w.TaskID, "error", err)
		}
	}
	return next
}

// expireWait records the timeout of wait w and continues the chain after its
// strategy.
func (s *RefreshService) expireWait(ctx context.Context, w *domain.RefreshAttempt) error {
	t, eff, err := s.load(ctx, w.TaskID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}
	if t.Status != domain.StatusAwaitingLink {
		return nil
	}
	s.record(ctx, t, w.Strategy, domain.RefreshCauseTimeout, domain.RefreshTimeout, "no matching link arrived before the deadline", "")
	_, err = s.runChain(ctx, t, eff, domain.RefreshCauseTimeout, settings.RefreshStrategy(w.Strategy), nil)
	return err
}

// ---- the chain ----

// runChain tries the effective strategies in order, starting after `after`
// (empty: from the start). cause is the original download error, kept as the
// task error when the chain is exhausted.
func (s *RefreshService) runChain(ctx context.Context, t *domain.Task, eff settings.Effective, why string,
	after settings.RefreshStrategy, cause error,
) (chainResult, error) {
	strategies := eff.Settings.Refresh.Strategies.OrElse(nil)
	if i := slices.Index(strategies, after); after != "" && i >= 0 {
		strategies = strategies[i+1:]
	}
	startStatus := t.Status
	if err := s.enterRefreshing(ctx, t); err != nil {
		return chainExhausted, err
	}
	maxAttempts := eff.Settings.Refresh.MaxAttempts.OrElse(3)
	attempts, err := s.d.Store.RefreshHistory().CountAt(ctx, t.ID, t.Downloaded)
	if err != nil {
		return chainExhausted, fmt.Errorf("service: refresh: %w", err)
	}
	prevWait := settings.RefreshStrategy("")
	if startStatus == domain.StatusAwaitingLink && why == domain.RefreshCauseManual {
		if last, err := s.d.Store.RefreshHistory().Latest(ctx, t.ID); err == nil && last.Outcome == domain.RefreshWaiting {
			prevWait = settings.RefreshStrategy(last.Strategy)
		}
	}
	var failures []string
	for _, name := range strategies {
		if err := ctx.Err(); err != nil {
			return chainExhausted, err //nolint:wrapcheck // cancellation is reported as is
		}
		if name == settings.RefreshExtensionCapture || name == settings.RefreshManual {
			if !canPark(t.Status) {
				s.record(ctx, t, string(name), why, domain.RefreshSkipped, fmt.Sprintf("a %s task cannot wait for a new link", t.Status), "")
				continue
			}
			if err := s.parkFrom(ctx, t, eff, why, name); err != nil {
				return chainExhausted, err
			}
			return chainAwaiting, nil
		}
		if attempts >= maxAttempts {
			failures = append(failures, fmt.Sprintf("refresh.maxAttempts (%d) reached without download progress", maxAttempts))
			break
		}
		st := s.strategies[name]
		if st == nil {
			s.record(ctx, t, string(name), why, domain.RefreshSkipped, "strategy is not available", "")
			continue
		}
		cand, err := st.Refresh(ctx, refresh.Request{Task: t, Settings: eff.Settings})
		switch {
		case errors.Is(err, refresh.ErrNotApplicable):
			s.record(ctx, t, string(name), why, domain.RefreshSkipped, err.Error(), "")
			continue
		case ctx.Err() != nil:
			_ = cand.Probe.Close()           // Close is nil-safe; the refresh was cancelled
			return chainExhausted, ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		case err != nil:
			attempts++
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			s.record(ctx, t, string(name), why, domain.RefreshFailed, err.Error(), "")
			continue
		}
		policy := eff.Settings.Refresh.OnMismatch.OrElse(settings.MismatchFail)
		applied, err := s.tryCandidate(ctx, t, eff, cand, why, name, policy)
		if err != nil {
			return chainExhausted, err
		}
		if applied.ok {
			return chainRefreshed, nil
		}
		attempts++
		failures = append(failures, fmt.Sprintf("%s: %v", name, applied.err))
		if applied.parked {
			return chainAwaiting, nil
		}
	}
	if prevWait != "" {
		// A manual refresh of a waiting task that found nothing: back to
		// waiting, with a fresh record so the wait stays visible.
		return chainAwaiting, s.parkFrom(ctx, t, eff, why, prevWait)
	}
	return chainExhausted, s.exhausted(ctx, t, startStatus, cause, failures)
}

// enterRefreshing marks a running or awaiting_link task as refreshing.
// Other states are left as they are (the state machine has no refreshing
// state for them).
func (s *RefreshService) enterRefreshing(ctx context.Context, t *domain.Task) error {
	if t.Status != domain.StatusRunning && t.Status != domain.StatusAwaitingLink {
		return nil
	}
	return s.mutate(ctx, t, func(fresh *domain.Task) error {
		if fresh.Status != t.Status {
			return domain.Conflict("service: refresh task %q: status changed to %s", fresh.ID, fresh.Status)
		}
		return fresh.TransitionTo(domain.StatusRefreshing, s.d.now()) //nolint:wrapcheck // domain error
	})
}

// exhausted ends a chain that found no link. A task that was running (or
// awaiting a link) fails with its original link-expired error, annotated
// with what was tried; a task in any other state is left alone.
func (s *RefreshService) exhausted(ctx context.Context, t *domain.Task, startStatus domain.TaskStatus, cause error, failures []string) error {
	if startStatus != domain.StatusRunning && startStatus != domain.StatusAwaitingLink {
		return nil
	}
	if cause == nil {
		cause = engine.LinkExpired(errors.New("the link expired"))
	}
	te := engine.TaskError(cause, s.d.now())
	if te.Code == domain.ErrorCodeFatal || te.Code == "" {
		te.Code = domain.ErrorCodeLinkExpired
	}
	te.Retryable = true
	if len(failures) > 0 {
		te.Message += "; link refresh failed: " + strings.Join(failures, "; ")
	} else {
		te.Message += "; link refresh found no usable link"
	}
	return s.mutate(ctx, t, func(fresh *domain.Task) error {
		return fresh.SetError(te, s.d.now()) //nolint:wrapcheck // domain error
	})
}

// ---- candidates ----

// applyOutcome is the result of tryCandidate.
type applyOutcome struct {
	ok     bool
	parked bool  // refresh.onMismatch=ask: the task now waits for a decision
	err    error // why the candidate was not applied
}

// tryCandidate validates a candidate link and applies it according to the
// mismatch policy. A candidate that cannot be used is recorded in the
// history and reported in the outcome; an error return means the refresh
// itself must stop (cancellation, storage failure).
func (s *RefreshService) tryCandidate(ctx context.Context, t *domain.Task, eff settings.Effective, cand refresh.Candidate,
	why string, name settings.RefreshStrategy, policy settings.MismatchPolicy,
) (applyOutcome, error) {
	oldURL := t.Source.CurrentURL
	eng, err := s.engineFor(t, cand.Source)
	if err != nil {
		_ = cand.Probe.Close() // nil-safe
		return applyOutcome{}, err
	}
	val, err := refresh.Validate(ctx, eng, t, cand, eff.Settings)
	if err != nil {
		_ = cand.Probe.Close() // nil-safe; a probe error leaves nothing open
		if ctx.Err() != nil {
			return applyOutcome{}, ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		}
		s.record(ctx, t, string(name), why, domain.RefreshFailed, err.Error(), cand.Source.CurrentURL)
		return applyOutcome{err: err}, nil
	}
	defer func() { _ = val.Probe.Close() }() // the probe only validated the link
	if val.Match {
		if err := s.apply(ctx, t, cand, val, false, why, name, domain.RefreshSuccess, oldURL); err != nil {
			return applyOutcome{}, err
		}
		return applyOutcome{ok: true}, nil
	}
	msg := "the new link serves different content: " + val.Reason
	switch policy {
	case settings.MismatchRestart:
		if err := s.apply(ctx, t, cand, val, true, why, name, domain.RefreshRestarted, oldURL); err != nil {
			return applyOutcome{}, err
		}
		return applyOutcome{ok: true}, nil
	case settings.MismatchAsk:
		s.record(ctx, t, string(name), why, domain.RefreshMismatch, msg, cand.Source.CurrentURL)
		if !canPark(t.Status) {
			return applyOutcome{err: domain.Conflict("service: %s", msg)}, nil
		}
		if err := s.parkFrom(ctx, t, eff, why, settings.RefreshManual); err != nil {
			return applyOutcome{}, err
		}
		return applyOutcome{parked: true, err: errors.New(msg)}, nil
	default:
		s.record(ctx, t, string(name), why, domain.RefreshMismatch, msg, cand.Source.CurrentURL)
		return applyOutcome{err: domain.Conflict("service: %s", msg)}, nil
	}
}

// apply stores the validated link on the task: with restart=false the
// download resumes from its segments, with restart=true the partial data is
// discarded and the new content is downloaded from the start. A refreshing,
// awaiting_link or failed task is queued again.
func (s *RefreshService) apply(ctx context.Context, t *domain.Task, cand refresh.Candidate, val refresh.Validation, restart bool,
	why string, name settings.RefreshStrategy, outcome domain.RefreshOutcome, oldURL string,
) error {
	probe := val.Probe
	expect := t.Status
	var (
		saved *domain.Task
		from  domain.TaskStatus
		row   *domain.RefreshAttempt
	)
	err := s.d.withTaskLock(ctx, t.ID, func() error {
		return s.d.Store.WithTx(ctx, func(tx storage.Store) error {
			fresh, err := tx.Tasks().Get(ctx, t.ID)
			if err != nil {
				return fmt.Errorf("service: refresh task: %w", err)
			}
			if fresh.Status != expect {
				return domain.Conflict("service: refresh task %q: status changed from %s to %s", t.ID, expect, fresh.Status)
			}
			from = fresh.Status
			downloaded := fresh.Downloaded
			newSrc := newSource(fresh.Source, cand.Source, probe, restart)
			if restart {
				if _, part, ok := taskPaths(fresh); ok { // the final file is never touched
					if err := removeFile(part); err != nil {
						return fmt.Errorf("service: refresh task %q: %w", t.ID, err)
					}
				}
				res := probe.Resource
				res.Files = slices.Clone(res.Files)
				fresh.Resource, fresh.Size, fresh.Downloaded = &res, res.Size, 0
				if err := tx.Segments().Replace(ctx, t.ID, nil); err != nil {
					return fmt.Errorf("service: refresh task %q: %w", t.ID, err)
				}
				if err := tx.EngineState().Delete(ctx, t.ID); err != nil {
					return fmt.Errorf("service: refresh task %q: %w", t.ID, err)
				}
			}
			fresh.Source = newSrc
			if fresh.Status == domain.StatusRefreshing || fresh.Status == domain.StatusAwaitingLink || fresh.Status == domain.StatusError {
				if err := fresh.TransitionTo(domain.StatusQueued, s.d.now()); err != nil {
					return fmt.Errorf("service: refresh task %q: %w", t.ID, err)
				}
			}
			fresh.UpdatedAt = s.d.now()
			if err := tx.Tasks().Update(ctx, fresh); err != nil {
				return fmt.Errorf("service: save task %q: %w", t.ID, err)
			}
			row = s.attempt(fresh, string(name), why, outcome, "", oldURL, newSrc.CurrentURL, downloaded)
			if err := tx.RefreshHistory().Add(ctx, row); err != nil {
				return fmt.Errorf("service: refresh task %q: %w", t.ID, err)
			}
			saved = fresh
			return nil
		})
	})
	if err != nil {
		return err
	}
	*t = *saved
	s.d.publish(events.TypeTaskRefresh, t.ID, t.QueueID, row)
	if t.Status != from {
		s.d.publishStatus(t, from)
	}
	s.d.publishTask(events.TypeTaskUpdated, t)
	if t.Status == domain.StatusQueued {
		s.d.Scheduler.Wake()
	}
	return nil
}

// newSource builds the source stored after a refresh: the candidate's link
// and context plus the redirect the probe found. When the download resumes
// the old fingerprint stays (so If-Range keeps guarding the partial data)
// and only gaps are filled from the probe; a restart adopts the probe's.
func newSource(old, cand domain.Source, probe *engine.Probe, restart bool) domain.Source {
	src := cand
	src.RedirectURL = probe.Source.RedirectURL
	if src.RedirectURL == src.CurrentURL {
		src.RedirectURL = ""
	}
	src.OriginalURL = cmpOr(old.OriginalURL, src.OriginalURL)
	if restart {
		src.Fingerprint = probe.Source.Fingerprint
	} else {
		src.Fingerprint = mergeFingerprint(old.Fingerprint, probe.Source.Fingerprint)
	}
	return src
}

// mergeFingerprint keeps old and fills what it does not know from learnt.
func mergeFingerprint(old, learnt domain.Fingerprint) domain.Fingerprint {
	out := old
	if out.Size <= 0 {
		out.Size = learnt.Size
	}
	if out.ETag == "" || strings.HasPrefix(out.ETag, "W/") {
		out.ETag = learnt.ETag
	}
	if out.LastModified == nil {
		out.LastModified = learnt.LastModified
	}
	out.Filename = cmpOr(out.Filename, learnt.Filename)
	out.ContentType = cmpOr(out.ContentType, learnt.ContentType)
	return out
}

// mergeSource overlays the fields set in in onto base (see SetSourceInput).
func mergeSource(base, in domain.Source) domain.Source {
	out := base
	out.CurrentURL, out.RedirectURL = in.CurrentURL, ""
	out.OriginalURL = cmpOr(in.OriginalURL, base.OriginalURL)
	out.PageURL = cmpOr(in.PageURL, base.PageURL)
	out.Referrer = cmpOr(in.Referrer, base.Referrer)
	out.Method = cmpOr(in.Method, base.Method)
	if len(in.Body) > 0 {
		out.Body = in.Body
	}
	if len(in.Headers) > 0 {
		out.Headers = in.Headers
	}
	out.UserAgent = cmpOr(in.UserAgent, base.UserAgent)
	if len(in.CookieDomains) > 0 {
		out.CookieDomains = in.CookieDomains
	}
	out.MediaHint = cmpOr(in.MediaHint, base.MediaHint)
	out.ResolverKind = cmp.Or(in.ResolverKind, base.ResolverKind)
	// The fingerprint stays the task's: the new link is checked against it.
	out.Fingerprint = base.Fingerprint
	return out
}

// ---- waiting ----

// canPark reports whether the state machine lets a task in status wait in
// awaiting_link: running and refreshing tasks (their link expired), failed
// tasks and tasks already waiting.
func canPark(status domain.TaskStatus) bool {
	return status == domain.StatusAwaitingLink || domain.CanTransition(status, domain.StatusAwaitingLink)
}

// parkFrom moves the task to awaiting_link (from running, refreshing, failed
// or already awaiting) and records the wait: extension-capture waits get the
// awaitTimeout deadline, manual waits none.
func (s *RefreshService) parkFrom(ctx context.Context, t *domain.Task, eff settings.Effective, why string, name settings.RefreshStrategy) error {
	var deadline *time.Time
	if name == settings.RefreshExtensionCapture {
		if d := time.Duration(eff.Settings.Refresh.AwaitTimeout.OrElse(0)); d > 0 {
			at := s.d.now().Add(d)
			deadline = &at
		}
	}
	msg := "waiting for a matching browser capture"
	if name == settings.RefreshManual {
		msg = "waiting for a new link to be set"
	}
	var from domain.TaskStatus
	var row *domain.RefreshAttempt
	err := s.d.withTaskLock(ctx, t.ID, func() error {
		return s.d.Store.WithTx(ctx, func(tx storage.Store) error {
			fresh, err := tx.Tasks().Get(ctx, t.ID)
			if err != nil {
				return fmt.Errorf("service: refresh task: %w", err)
			}
			from = fresh.Status
			if fresh.Status != domain.StatusAwaitingLink {
				if err := fresh.TransitionTo(domain.StatusAwaitingLink, s.d.now()); err != nil {
					return fmt.Errorf("service: refresh task %q: %w", t.ID, err)
				}
				fresh.Error = nil
			}
			fresh.UpdatedAt = s.d.now()
			if err := tx.Tasks().Update(ctx, fresh); err != nil {
				return fmt.Errorf("service: save task %q: %w", t.ID, err)
			}
			row = s.attempt(fresh, string(name), why, domain.RefreshWaiting, msg, fresh.Source.CurrentURL, "", fresh.Downloaded)
			row.Deadline = deadline
			if err := tx.RefreshHistory().Add(ctx, row); err != nil {
				return fmt.Errorf("service: refresh task %q: %w", t.ID, err)
			}
			*t = *fresh
			return nil
		})
	})
	if err != nil {
		return err
	}
	s.d.publish(events.TypeTaskRefresh, t.ID, t.QueueID, row)
	if t.Status != from {
		s.d.publishStatus(t, from)
	}
	if deadline != nil {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	return nil
}

// awaitingCapture lists the awaiting_link tasks whose newest history entry is
// an extension-capture wait, oldest first.
func (s *RefreshService) awaitingCapture(ctx context.Context) ([]*domain.Task, error) {
	filter := storage.TaskFilter{Statuses: []domain.TaskStatus{domain.StatusAwaitingLink}, Limit: storage.MaxTaskLimit}
	var out []*domain.Task
	for {
		page, err := s.d.Store.Tasks().List(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("service: refresh: list waiting tasks: %w", err)
		}
		for _, t := range page.Tasks {
			last, err := s.d.Store.RefreshHistory().Latest(ctx, t.ID)
			if err != nil {
				if errors.Is(err, domain.ErrNotFound) {
					continue
				}
				return nil, fmt.Errorf("service: refresh: %w", err)
			}
			if last.Outcome == domain.RefreshWaiting && last.Strategy == string(settings.RefreshExtensionCapture) {
				out = append(out, t)
			}
		}
		if page.NextCursor == "" {
			break
		}
		filter.Cursor = page.NextCursor
	}
	slices.SortStableFunc(out, func(a, b *domain.Task) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// captureMatches reports whether a capture with fingerprint fp plausibly is
// the file of task t: nothing contradicts it and at least one property
// (size, strong ETag, file name) is known on both sides and equal.
func captureMatches(t *domain.Task, fp domain.Fingerprint) bool {
	have := t.Source.Fingerprint
	if ok, _ := have.Matches(fp); !ok {
		return false
	}
	if have.Size > 0 && have.Size == fp.Size {
		return true
	}
	if have.ETag != "" && !strings.HasPrefix(have.ETag, "W/") && have.ETag == fp.ETag {
		return true
	}
	for _, name := range []string{have.Filename, t.Name} {
		if name != "" && fp.Filename != "" && strings.EqualFold(name, fp.Filename) {
			return true
		}
	}
	return false
}

// ---- helpers ----

// load reads a task with its effective settings.
func (s *RefreshService) load(ctx context.Context, taskID string) (*domain.Task, settings.Effective, error) {
	t, err := s.d.Store.Tasks().Get(ctx, taskID)
	if err != nil {
		return nil, settings.Effective{}, fmt.Errorf("service: refresh: %w", err)
	}
	eff, err := effective(ctx, s.d, t.QueueID, t.Settings)
	if err != nil {
		return nil, settings.Effective{}, err
	}
	return t, eff, nil
}

func (s *RefreshService) engineFor(t *domain.Task, src domain.Source) (engine.Engine, error) {
	if t.Engine != "" {
		e, err := s.d.Engines.Get(t.Engine)
		if err != nil {
			return nil, fmt.Errorf("service: refresh: %w", err)
		}
		return e, nil
	}
	e, err := s.d.Engines.Select(src)
	if err != nil {
		return nil, fmt.Errorf("service: refresh: %w", err)
	}
	return e, nil
}

// refreshable checks that a task can have its link replaced from outside.
func refreshable(t *domain.Task) error {
	switch t.Status {
	case domain.StatusRunning, domain.StatusRefreshing, domain.StatusPostprocessing:
		return domain.Conflict("service: task %q is %s; pause it first", t.ID, t.Status)
	case domain.StatusCompleted:
		return domain.Conflict("service: task %q is completed", t.ID)
	default:
		return nil
	}
}

// mutate re-reads the task under its lock, applies fn, persists it and
// publishes a status event for a status change. On success *t is the saved
// task.
func (s *RefreshService) mutate(ctx context.Context, t *domain.Task, fn func(fresh *domain.Task) error) error {
	var (
		saved *domain.Task
		from  domain.TaskStatus
	)
	err := s.d.withTaskLock(ctx, t.ID, func() error {
		return s.d.Store.WithTx(ctx, func(tx storage.Store) error {
			fresh, err := tx.Tasks().Get(ctx, t.ID)
			if err != nil {
				return fmt.Errorf("service: refresh task: %w", err)
			}
			from = fresh.Status
			if err := fn(fresh); err != nil {
				return err
			}
			if err := tx.Tasks().Update(ctx, fresh); err != nil {
				return fmt.Errorf("service: save task %q: %w", t.ID, err)
			}
			saved = fresh
			return nil
		})
	})
	if err != nil {
		return err
	}
	*t = *saved
	if t.Status != from {
		s.d.publishStatus(t, from)
	}
	return nil
}

// attempt builds a history entry for t.
func (s *RefreshService) attempt(t *domain.Task, strategy, cause string, outcome domain.RefreshOutcome,
	msg, oldURL, newURL string, downloaded int64,
) *domain.RefreshAttempt {
	return &domain.RefreshAttempt{
		ID: s.d.NewID(), TaskID: t.ID, Strategy: strategy, Cause: cause, Outcome: outcome, Error: msg,
		OldURL: oldURL, NewURL: newURL, Downloaded: downloaded, CreatedAt: s.d.now().Truncate(time.Millisecond),
	}
}

// record appends a history entry outside of a state change and publishes it.
// A storage failure is logged: the history must never break a refresh.
func (s *RefreshService) record(ctx context.Context, t *domain.Task, strategy, cause string, outcome domain.RefreshOutcome, msg, newURL string) {
	row := s.attempt(t, strategy, cause, outcome, msg, t.Source.CurrentURL, newURL, t.Downloaded)
	if err := s.d.Store.RefreshHistory().Add(ctx, row); err != nil {
		s.d.Logger.WarnContext(ctx, "refresh: cannot record history", "task_id", t.ID, "error", err)
		return
	}
	s.d.publish(events.TypeTaskRefresh, t.ID, t.QueueID, row)
}
