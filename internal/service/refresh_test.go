package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/settings"
)

// refreshEnv is a service fixture plus an origin whose links expire: /click
// redirects to /file?token=<valid token>, and /file serves only the valid
// token (403 otherwise). The served file's ETag can be changed to simulate a
// different file behind a fresh link.
type refreshEnv struct {
	*svcFixture
	origin *httptest.Server
	data   []byte
	etag   atomic.Value // string
	token  atomic.Value // string
	name   atomic.Value // string: the Content-Disposition file name, when set
	hits   atomic.Int32 // requests to /file
}

func newRefreshEnv(t *testing.T) *refreshEnv {
	t.Helper()
	e := &refreshEnv{svcFixture: newSvcFixture(t), data: bytes.Repeat([]byte("0123456789abcdef"), 4096)} // 64 KiB
	e.etag.Store(`"v1"`)
	e.token.Store("new")
	mux := http.NewServeMux()
	mux.HandleFunc("/click", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/file?token="+load(&e.token), http.StatusFound)
	})
	mux.HandleFunc("/file", func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		if r.URL.Query().Get("token") != load(&e.token) {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		w.Header().Set("ETag", load(&e.etag))
		w.Header().Set("Content-Type", "application/octet-stream")
		if n, _ := e.name.Load().(string); n != "" {
			w.Header().Set("Content-Disposition", `attachment; filename="`+n+`"`)
		}
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(e.data))
	})
	e.origin = httptest.NewServer(mux)
	t.Cleanup(e.origin.Close)
	return e
}

// load reads a string stored in an atomic.Value.
func load(v *atomic.Value) string {
	s, _ := v.Load().(string)
	return s
}

func (e *refreshEnv) oldLink() string { return e.origin.URL + "/file?token=old" }

// task creates a task for the origin's file, as left behind by a download
// that got `downloaded` bytes in before the link expired, in the given state
// (running, awaiting_link, error, paused or queued).
func (e *refreshEnv) task(t *testing.T, status domain.TaskStatus, downloaded int64) *domain.Task {
	t.Helper()
	size := int64(len(e.data))
	in := CreateTaskInput{
		Name: "file.bin", Dir: e.dir,
		Source: domain.Source{
			CurrentURL: e.oldLink(), OriginalURL: e.origin.URL + "/click",
			Fingerprint: domain.Fingerprint{Size: size, ETag: `"v1"`, Filename: "file.bin", ContentType: "application/octet-stream"},
		},
	}
	task := e.create(t, in)
	task.Size, task.Downloaded = size, downloaded
	task.Resource = &domain.Resource{Name: "file.bin", Size: size, RangeSupported: true, Files: []domain.File{{Index: 0, Name: "file.bin", Size: size, Selected: true}}}
	switch status {
	case domain.StatusQueued:
	case domain.StatusPaused:
		mustTransition(t, task, domain.StatusPaused)
	case domain.StatusRunning:
		mustTransition(t, task, domain.StatusRunning)
	case domain.StatusAwaitingLink:
		mustTransition(t, task, domain.StatusRunning, domain.StatusAwaitingLink)
	case domain.StatusError:
		mustTransition(t, task, domain.StatusRunning)
		if err := task.SetError(domain.TaskError{Code: domain.ErrorCodeLinkExpired, Message: "403"}, time.Now()); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported status %s", status)
	}
	if err := e.store.Tasks().Update(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if downloaded > 0 {
		segs := []domain.Segment{{Index: 0, Start: 0, End: size, Done: downloaded}}
		if err := e.store.Segments().Replace(t.Context(), task.ID, segs); err != nil {
			t.Fatal(err)
		}
		if err := e.store.EngineState().Put(t.Context(), task.ID, []byte("state")); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

func mustTransition(t *testing.T, task *domain.Task, path ...domain.TaskStatus) {
	t.Helper()
	for _, to := range path {
		if err := task.TransitionTo(to, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

// setSettings stores task-level settings overrides.
func (e *refreshEnv) setSettings(t *testing.T, id string, s settings.Settings) {
	t.Helper()
	task := e.get(t, id)
	task.Settings = s
	if err := e.store.Tasks().Update(t.Context(), task); err != nil {
		t.Fatal(err)
	}
}

func (e *refreshEnv) history(t *testing.T, id string) []string {
	t.Helper()
	h, err := e.refresher.History(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(h))
	for i, a := range h {
		out[i] = a.Strategy + ":" + string(a.Outcome)
	}
	return out
}

func wantHistory(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
}

func refreshSettings(r settings.RefreshSettings) settings.Settings {
	return settings.Settings{Refresh: r}
}

func TestHandleExpiredResumesWithTheNewLink(t *testing.T) {
	e := newRefreshEnv(t)
	task := e.task(t, domain.StatusRunning, 20000)
	sub := e.bus.Subscribe(t.Context(), events.Filter{Types: []events.Type{events.TypeTaskStatus, events.TypeTaskRefresh}})

	if err := e.refresher.HandleExpired(t.Context(), task.ID, engine.LinkExpired(errors.New("403"))); err != nil {
		t.Fatalf("HandleExpired: %v", err)
	}
	got := e.get(t, task.ID)
	if got.Status != domain.StatusQueued {
		t.Fatalf("status = %s, want queued", got.Status)
	}
	if want := e.origin.URL + "/file?token=new"; got.Source.CurrentURL != want || got.Source.RedirectURL != "" ||
		got.Source.OriginalURL != e.origin.URL+"/click" {
		t.Errorf("source = %+v, want the redirect target as current link and the original kept", got.Source)
	}
	if fp := got.Source.Fingerprint; fp.ETag != `"v1"` || fp.Size != int64(len(e.data)) {
		t.Errorf("fingerprint = %+v, want the old one kept", fp)
	}
	if got.Downloaded != 20000 {
		t.Errorf("downloaded = %d, want the progress kept", got.Downloaded)
	}
	if segs, err := e.store.Segments().List(t.Context(), task.ID); err != nil || len(segs) != 1 || segs[0].Done != 20000 {
		t.Errorf("segments = %+v, %v; want them kept", segs, err)
	}
	if _, err := e.store.EngineState().Get(t.Context(), task.ID); err != nil {
		t.Errorf("engine state lost: %v", err)
	}
	wantHistory(t, e.history(t, task.ID), "origin-redirect:success")
	if e.sched.wakes.Load() == 0 {
		t.Error("the scheduler was not woken for the requeued task")
	}

	var statuses []string
	var refreshes int
	for _, ev := range collect(sub) {
		switch d := ev.Data.(type) {
		case StatusData:
			statuses = append(statuses, string(d.From)+">"+string(d.To))
		case *domain.RefreshAttempt:
			refreshes++
			if d.Outcome != domain.RefreshSuccess || d.OldURL != e.oldLink() || d.NewURL != got.Source.CurrentURL || d.Downloaded != 20000 {
				t.Errorf("refresh event = %+v", d)
			}
		}
	}
	if want := []string{"running>refreshing", "refreshing>queued"}; !slices.Equal(statuses, want) || refreshes != 1 {
		t.Errorf("status events = %v, refresh events = %d; want %v and 1", statuses, refreshes, want)
	}
}

func TestHandleExpiredDisabled(t *testing.T) {
	e := newRefreshEnv(t)
	task := e.task(t, domain.StatusRunning, 0)
	e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{Auto: settings.Some(false)}))
	if err := e.refresher.HandleExpired(t.Context(), task.ID, nil); !errors.Is(err, ErrRefreshDisabled) {
		t.Fatalf("HandleExpired = %v, want ErrRefreshDisabled", err)
	}
	if got := e.get(t, task.ID); got.Status != domain.StatusRunning {
		t.Errorf("status = %s, want the task untouched", got.Status)
	}
	wantHistory(t, e.history(t, task.ID))
}

func TestMismatchPolicies(t *testing.T) {
	t.Run("fail then wait for a capture (default chain)", func(t *testing.T) {
		e := newRefreshEnv(t)
		e.etag.Store(`"v2"`) // the new link serves another file
		task := e.task(t, domain.StatusRunning, 20000)
		if err := e.refresher.HandleExpired(t.Context(), task.ID, engine.LinkExpired(errors.New("403"))); err != nil {
			t.Fatal(err)
		}
		got := e.get(t, task.ID)
		if got.Status != domain.StatusAwaitingLink || got.Source.CurrentURL != e.oldLink() || got.Downloaded != 20000 {
			t.Errorf("task = %s %s downloaded %d; want awaiting_link, old link, progress kept", got.Status, got.Source.CurrentURL, got.Downloaded)
		}
		wantHistory(t, e.history(t, task.ID), "origin-redirect:mismatch", "page-reload:skipped", "extension-capture:waiting")
		h, _ := e.refresher.History(t.Context(), task.ID)
		if !strings.Contains(h[0].Error, "etag differs") {
			t.Errorf("mismatch reason = %q", h[0].Error)
		}
		want := e.clock.Now().Add(10 * time.Minute) // the default awaitTimeout
		if h[2].Deadline == nil || h[2].Deadline.Sub(want).Abs() > time.Second {
			t.Errorf("deadline = %v, want about %v", h[2].Deadline, want)
		}
	})
	t.Run("fail only: the task fails", func(t *testing.T) {
		e := newRefreshEnv(t)
		e.etag.Store(`"v2"`)
		task := e.task(t, domain.StatusRunning, 20000)
		e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{
			Strategies: settings.Some([]settings.RefreshStrategy{settings.RefreshOriginRedirect}),
		}))
		cause := engine.LinkExpired(errors.New("http 403"))
		if err := e.refresher.HandleExpired(t.Context(), task.ID, cause); err != nil {
			t.Fatal(err)
		}
		got := e.get(t, task.ID)
		if got.Status != domain.StatusError || got.Error == nil || got.Error.Code != domain.ErrorCodeLinkExpired || !got.Error.Retryable ||
			!strings.Contains(got.Error.Message, "http 403") || !strings.Contains(got.Error.Message, "link refresh failed: origin-redirect") {
			t.Errorf("task = %s %+v", got.Status, got.Error)
		}
	})
	t.Run("restart", func(t *testing.T) {
		e := newRefreshEnv(t)
		e.etag.Store(`"v2"`)
		task := e.task(t, domain.StatusRunning, 20000)
		part := filepath.Join(e.dir, "file.bin.part")
		if err := os.WriteFile(part, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{OnMismatch: settings.Some(settings.MismatchRestart)}))
		if err := e.refresher.HandleExpired(t.Context(), task.ID, nil); err != nil {
			t.Fatal(err)
		}
		got := e.get(t, task.ID)
		if got.Status != domain.StatusQueued || got.Downloaded != 0 || got.Source.Fingerprint.ETag != `"v2"` {
			t.Errorf("task = %s downloaded %d fingerprint %+v; want queued from scratch with the new fingerprint", got.Status, got.Downloaded, got.Source.Fingerprint)
		}
		if segs, _ := e.store.Segments().List(t.Context(), task.ID); len(segs) != 0 {
			t.Errorf("segments = %+v, want none", segs)
		}
		if _, err := e.store.EngineState().Get(t.Context(), task.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("engine state = %v, want removed", err)
		}
		if _, err := os.Stat(part); !errors.Is(err, os.ErrNotExist) {
			t.Errorf(".part file still exists: %v", err)
		}
		wantHistory(t, e.history(t, task.ID), "origin-redirect:restarted")
	})
	t.Run("ask", func(t *testing.T) {
		e := newRefreshEnv(t)
		e.etag.Store(`"v2"`)
		task := e.task(t, domain.StatusRunning, 20000)
		e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{OnMismatch: settings.Some(settings.MismatchAsk)}))
		if err := e.refresher.HandleExpired(t.Context(), task.ID, nil); err != nil {
			t.Fatal(err)
		}
		got := e.get(t, task.ID)
		if got.Status != domain.StatusAwaitingLink || got.Downloaded != 20000 {
			t.Errorf("task = %s downloaded %d; want awaiting_link with the progress kept", got.Status, got.Downloaded)
		}
		wantHistory(t, e.history(t, task.ID), "origin-redirect:mismatch", "manual:waiting")
		h, _ := e.refresher.History(t.Context(), task.ID)
		if h[1].Deadline != nil {
			t.Errorf("a wait for a decision has a deadline: %v", h[1].Deadline)
		}
		// The user decides: use the new link and start over.
		fixed, err := e.refresher.SetSource(t.Context(), task.ID, SetSourceInput{
			Source:     domain.Source{CurrentURL: e.origin.URL + "/file?token=new"},
			OnMismatch: settings.MismatchRestart,
		})
		if err != nil || fixed.Status != domain.StatusQueued || fixed.Downloaded != 0 {
			t.Fatalf("SetSource = %+v, %v", fixed, err)
		}
	})
}

func TestMaxAttemptsCountOnlyAttemptsWithoutProgress(t *testing.T) {
	e := newRefreshEnv(t)
	task := e.task(t, domain.StatusRunning, 100)
	e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{
		Strategies:  settings.Some([]settings.RefreshStrategy{settings.RefreshOriginRedirect}),
		MaxAttempts: settings.Some(1),
	}))
	// Every refresh attempt fails: the fresh link serves a different file.
	e.etag.Store(`"v2"`)

	expire := func() {
		t.Helper()
		cur := e.get(t, task.ID)
		if cur.Status != domain.StatusRunning {
			mustTransition(t, cur, domain.StatusQueued, domain.StatusRunning)
			if err := e.store.Tasks().Update(t.Context(), cur); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.refresher.HandleExpired(t.Context(), task.ID, engine.LinkExpired(errors.New("403"))); err != nil {
			t.Fatal(err)
		}
	}
	expire()
	wantHistory(t, e.history(t, task.ID), "origin-redirect:mismatch")
	// Same progress, budget used up: no further attempt is made.
	expire()
	wantHistory(t, e.history(t, task.ID), "origin-redirect:mismatch")
	if got := e.get(t, task.ID); got.Status != domain.StatusError || !strings.Contains(got.Error.Message, "maxAttempts (1)") {
		t.Errorf("task = %s %+v, want an error naming refresh.maxAttempts", got.Status, got.Error)
	}
	// After progress the budget is fresh.
	cur := e.get(t, task.ID)
	cur.Downloaded = 5000
	if err := e.store.Tasks().Update(t.Context(), cur); err != nil {
		t.Fatal(err)
	}
	expire()
	wantHistory(t, e.history(t, task.ID), "origin-redirect:mismatch", "origin-redirect:mismatch")
}

func TestOriginRedirectFailureAndSkip(t *testing.T) {
	e := newRefreshEnv(t)
	task := e.task(t, domain.StatusRunning, 0)
	e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{
		Strategies: settings.Some([]settings.RefreshStrategy{settings.RefreshOriginRedirect, settings.RefreshExtensionCapture}),
	}))
	// The click URL itself is gone.
	gone := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(gone.Close)
	cur := e.get(t, task.ID)
	cur.Source.OriginalURL = gone.URL + "/click"
	if err := e.store.Tasks().Update(t.Context(), cur); err != nil {
		t.Fatal(err)
	}
	if err := e.refresher.HandleExpired(t.Context(), task.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, task.ID); got.Status != domain.StatusAwaitingLink {
		t.Errorf("status = %s, want awaiting_link after the failed strategy", got.Status)
	}
	h, _ := e.refresher.History(t.Context(), task.ID)
	if len(h) != 2 || h[0].Outcome != domain.RefreshFailed || !strings.Contains(h[0].Error, "404") || h[1].Outcome != domain.RefreshWaiting {
		t.Errorf("history = %+v", h)
	}
}

func TestAwaitTimeoutContinuesTheChain(t *testing.T) {
	e := newRefreshEnv(t)
	task := e.task(t, domain.StatusRunning, 0)
	e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{
		Strategies:   settings.Some([]settings.RefreshStrategy{settings.RefreshExtensionCapture}),
		AwaitTimeout: settings.Some(settings.Duration(time.Minute)),
	}))
	if err := e.refresher.HandleExpired(t.Context(), task.ID, engine.LinkExpired(errors.New("403"))); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, task.ID); got.Status != domain.StatusAwaitingLink {
		t.Fatalf("status = %s", got.Status)
	}
	next := e.refresher.sweepOnce(t.Context())
	if want := e.clock.Now().Add(time.Minute); next.Sub(want).Abs() > time.Second {
		t.Errorf("next deadline = %v, want about %v", next, want)
	}
	if got := e.get(t, task.ID); got.Status != domain.StatusAwaitingLink {
		t.Fatalf("status = %s before the deadline", got.Status)
	}

	e.clock.Advance(2 * time.Minute)
	if next := e.refresher.sweepOnce(t.Context()); !next.IsZero() {
		t.Errorf("next deadline after expiry = %v, want none", next)
	}
	got := e.get(t, task.ID)
	if got.Status != domain.StatusError || got.Error == nil || got.Error.Code != domain.ErrorCodeLinkExpired {
		t.Errorf("task = %s %+v, want a link_expired error after the wait timed out", got.Status, got.Error)
	}
	wantHistory(t, e.history(t, task.ID), "extension-capture:waiting", "extension-capture:timeout")
}

func TestAwaitTimeoutFallsThroughToLaterStrategies(t *testing.T) {
	e := newRefreshEnv(t)
	task := e.task(t, domain.StatusRunning, 0)
	e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{
		Strategies:   settings.Some([]settings.RefreshStrategy{settings.RefreshExtensionCapture, settings.RefreshOriginRedirect}),
		AwaitTimeout: settings.Some(settings.Duration(time.Minute)),
	}))
	if err := e.refresher.HandleExpired(t.Context(), task.ID, nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Minute)
	e.refresher.sweepOnce(t.Context())
	if got := e.get(t, task.ID); got.Status != domain.StatusQueued || !strings.Contains(got.Source.CurrentURL, "token=new") {
		t.Errorf("task = %s %s, want queued with the refreshed link", got.Status, got.Source.CurrentURL)
	}
	wantHistory(t, e.history(t, task.ID), "extension-capture:waiting", "extension-capture:timeout", "origin-redirect:success")
}

func TestAttachCapture(t *testing.T) {
	e := newRefreshEnv(t)
	task := e.task(t, domain.StatusRunning, 20000)
	e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{
		Strategies: settings.Some([]settings.RefreshStrategy{settings.RefreshExtensionCapture}),
	}))
	if err := e.refresher.HandleExpired(t.Context(), task.ID, nil); err != nil {
		t.Fatal(err)
	}
	newLink := e.origin.URL + "/file?token=new"
	capture := func(fp domain.Fingerprint) domain.Source {
		return domain.Source{CurrentURL: newLink, PageURL: "https://example.com/p", Referrer: "https://example.com/", UserAgent: "UA", Fingerprint: fp}
	}

	// A capture of another file does not match.
	for name, fp := range map[string]domain.Fingerprint{
		"other size":      {Size: 1, Filename: "file.bin"},
		"other name":      {Size: int64(len(e.data)), Filename: "other.bin"},
		"nothing known":   {},
		"only a mime":     {ContentType: "application/octet-stream"},
		"different etag":  {ETag: `"zzz"`},
		"name only other": {Filename: "x.zip"},
	} {
		got, err := e.refresher.AttachCapture(t.Context(), capture(fp))
		if err != nil || got != nil {
			t.Errorf("%s: AttachCapture = %+v, %v; want no match", name, got, err)
		}
	}
	if got := e.get(t, task.ID); got.Status != domain.StatusAwaitingLink {
		t.Fatalf("status = %s after non-matching captures", got.Status)
	}

	// The matching capture (same size and file name) becomes the new link.
	got, err := e.refresher.AttachCapture(t.Context(), capture(domain.Fingerprint{Size: int64(len(e.data)), Filename: "FILE.bin"}))
	if err != nil || got == nil {
		t.Fatalf("AttachCapture = %+v, %v; want the task", got, err)
	}
	if got.ID != task.ID || got.Status != domain.StatusQueued || got.Source.CurrentURL != newLink || got.Source.PageURL != "https://example.com/p" ||
		got.Source.UserAgent != "UA" || got.Downloaded != 20000 || got.Source.OriginalURL != e.origin.URL+"/click" {
		t.Errorf("task = %+v", got)
	}
	h, _ := e.refresher.History(t.Context(), task.ID)
	if last := h[len(h)-1]; last.Cause != domain.RefreshCauseCapture || last.Outcome != domain.RefreshSuccess || last.Strategy != "extension-capture" {
		t.Errorf("last history entry = %+v", last)
	}
	// Nobody waits any more.
	if again, err := e.refresher.AttachCapture(t.Context(), capture(domain.Fingerprint{Size: int64(len(e.data))})); err != nil || again != nil {
		t.Errorf("second capture matched %+v, %v", again, err)
	}
}

func TestAttachCaptureIgnoresManualWaitsAndBrokenLinks(t *testing.T) {
	e := newRefreshEnv(t)
	manual := e.task(t, domain.StatusRunning, 0)
	e.setSettings(t, manual.ID, refreshSettings(settings.RefreshSettings{
		Strategies: settings.Some([]settings.RefreshStrategy{settings.RefreshManual}),
	}))
	if err := e.refresher.HandleExpired(t.Context(), manual.ID, nil); err != nil {
		t.Fatal(err)
	}
	fp := domain.Fingerprint{Size: int64(len(e.data)), Filename: "file.bin"}
	if got, err := e.refresher.AttachCapture(t.Context(), domain.Source{CurrentURL: e.origin.URL + "/file?token=new", Fingerprint: fp}); err != nil || got != nil {
		t.Errorf("a capture was attached to a task that waits for a manual link: %+v, %v", got, err)
	}

	waiting := e.task(t, domain.StatusRunning, 0)
	e.setSettings(t, waiting.ID, refreshSettings(settings.RefreshSettings{
		Strategies: settings.Some([]settings.RefreshStrategy{settings.RefreshExtensionCapture}),
	}))
	if err := e.refresher.HandleExpired(t.Context(), waiting.ID, nil); err != nil {
		t.Fatal(err)
	}
	// The captured link itself does not work: the task keeps waiting.
	got, err := e.refresher.AttachCapture(t.Context(), domain.Source{CurrentURL: e.oldLink(), Fingerprint: fp})
	if err != nil || got != nil {
		t.Errorf("a broken captured link was attached: %+v, %v", got, err)
	}
	if cur := e.get(t, waiting.ID); cur.Status != domain.StatusAwaitingLink {
		t.Errorf("status = %s, want still awaiting_link", cur.Status)
	}
	h, _ := e.refresher.History(t.Context(), waiting.ID)
	if last := h[len(h)-1]; last.Cause != domain.RefreshCauseCapture || last.Outcome != domain.RefreshFailed {
		t.Errorf("last history entry = %+v, want a failed capture attempt", last)
	}
}

func TestSetSource(t *testing.T) {
	newLink := func(e *refreshEnv) domain.Source { return domain.Source{CurrentURL: e.origin.URL + "/file?token=new"} }

	t.Run("failed task resumes", func(t *testing.T) {
		e := newRefreshEnv(t)
		task := e.task(t, domain.StatusError, 20000)
		in := newLink(e)
		in.Referrer, in.Headers = "https://example.com/ref", map[string]string{"X-Token": "1"}
		got, err := e.refresher.SetSource(t.Context(), task.ID, SetSourceInput{Source: in})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.StatusQueued || got.Error != nil || got.Downloaded != 20000 || got.Source.CurrentURL != in.CurrentURL ||
			got.Source.Referrer != "https://example.com/ref" || got.Source.Headers["X-Token"] != "1" ||
			got.Source.OriginalURL != e.origin.URL+"/click" || got.Source.Fingerprint.ETag != `"v1"` {
			t.Errorf("task = %+v", got)
		}
		wantHistory(t, e.history(t, task.ID), "manual:success")
		h, _ := e.refresher.History(t.Context(), task.ID)
		if h[0].Cause != domain.RefreshCauseSource {
			t.Errorf("cause = %q", h[0].Cause)
		}
	})
	t.Run("awaiting task is queued", func(t *testing.T) {
		e := newRefreshEnv(t)
		task := e.task(t, domain.StatusAwaitingLink, 0)
		got, err := e.refresher.SetSource(t.Context(), task.ID, SetSourceInput{Source: newLink(e)})
		if err != nil || got.Status != domain.StatusQueued {
			t.Fatalf("SetSource = %+v, %v", got, err)
		}
	})
	t.Run("paused task stays paused", func(t *testing.T) {
		e := newRefreshEnv(t)
		task := e.task(t, domain.StatusPaused, 20000)
		got, err := e.refresher.SetSource(t.Context(), task.ID, SetSourceInput{Source: newLink(e)})
		if err != nil || got.Status != domain.StatusPaused || got.Source.CurrentURL != newLink(e).CurrentURL {
			t.Fatalf("SetSource = %+v, %v", got, err)
		}
	})
	t.Run("mismatch is refused by default", func(t *testing.T) {
		e := newRefreshEnv(t)
		e.etag.Store(`"v2"`)
		task := e.task(t, domain.StatusError, 20000)
		_, err := e.refresher.SetSource(t.Context(), task.ID, SetSourceInput{Source: newLink(e)})
		wantErr(t, err, domain.ErrConflict)
		got := e.get(t, task.ID)
		if got.Status != domain.StatusError || got.Source.CurrentURL != e.oldLink() || got.Downloaded != 20000 {
			t.Errorf("task changed by a refused link: %s %s %d", got.Status, got.Source.CurrentURL, got.Downloaded)
		}
		wantHistory(t, e.history(t, task.ID), "manual:mismatch")
	})
	t.Run("mismatch with restart", func(t *testing.T) {
		e := newRefreshEnv(t)
		e.etag.Store(`"v2"`)
		task := e.task(t, domain.StatusError, 20000)
		got, err := e.refresher.SetSource(t.Context(), task.ID, SetSourceInput{Source: newLink(e), OnMismatch: settings.MismatchRestart})
		if err != nil || got.Status != domain.StatusQueued || got.Downloaded != 0 || got.Source.Fingerprint.ETag != `"v2"` {
			t.Fatalf("SetSource = %+v, %v", got, err)
		}
		wantHistory(t, e.history(t, task.ID), "manual:restarted")
	})
	t.Run("link that does not work", func(t *testing.T) {
		e := newRefreshEnv(t)
		task := e.task(t, domain.StatusError, 20000)
		_, err := e.refresher.SetSource(t.Context(), task.ID, SetSourceInput{Source: domain.Source{CurrentURL: e.oldLink()}})
		if engine.Classify(err) != engine.KindLinkExpired {
			t.Errorf("err = %v, want a link-expired error", err)
		}
		wantHistory(t, e.history(t, task.ID), "manual:failed")
	})
	t.Run("rejected input and states", func(t *testing.T) {
		e := newRefreshEnv(t)
		task := e.task(t, domain.StatusError, 0)
		for name, in := range map[string]SetSourceInput{
			"no url":       {},
			"relative url": {Source: domain.Source{CurrentURL: "/x"}},
			"bad policy":   {Source: newLink(e), OnMismatch: "sometimes"},
		} {
			if _, err := e.refresher.SetSource(t.Context(), task.ID, in); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
			}
		}
		running := e.task(t, domain.StatusRunning, 0)
		_, err := e.refresher.SetSource(t.Context(), running.ID, SetSourceInput{Source: newLink(e)})
		wantErr(t, err, domain.ErrConflict)
		if _, err := e.refresher.SetSource(t.Context(), "missing", SetSourceInput{Source: newLink(e)}); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("missing task: %v", err)
		}
	})
}

func TestRefreshNow(t *testing.T) {
	t.Run("failed task is queued", func(t *testing.T) {
		e := newRefreshEnv(t)
		task := e.task(t, domain.StatusError, 20000)
		got, err := e.refresher.Refresh(t.Context(), task.ID)
		if err != nil || got.Status != domain.StatusQueued || !strings.Contains(got.Source.CurrentURL, "token=new") {
			t.Fatalf("Refresh = %+v, %v", got, err)
		}
		h, _ := e.refresher.History(t.Context(), task.ID)
		if len(h) == 0 || h[0].Cause != domain.RefreshCauseManual {
			t.Errorf("history = %+v", h)
		}
	})
	t.Run("awaiting task is refreshed", func(t *testing.T) {
		e := newRefreshEnv(t)
		task := e.task(t, domain.StatusAwaitingLink, 0)
		got, err := e.refresher.Refresh(t.Context(), task.ID)
		if err != nil || got.Status != domain.StatusQueued {
			t.Fatalf("Refresh = %+v, %v", got, err)
		}
	})
	t.Run("no strategy helps: failed task stays failed", func(t *testing.T) {
		e := newRefreshEnv(t)
		e.etag.Store(`"v2"`)
		task := e.task(t, domain.StatusError, 20000)
		e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{
			Strategies: settings.Some([]settings.RefreshStrategy{settings.RefreshOriginRedirect}),
		}))
		got, err := e.refresher.Refresh(t.Context(), task.ID)
		if engine.Classify(err) != engine.KindLinkExpired {
			t.Errorf("err = %v, want a link-expired error", err)
		}
		if got == nil || got.Status != domain.StatusError {
			t.Errorf("task = %+v, want it left failed", got)
		}
	})
	t.Run("no strategy helps: awaiting task keeps waiting", func(t *testing.T) {
		e := newRefreshEnv(t)
		e.etag.Store(`"v2"`)
		task := e.task(t, domain.StatusRunning, 20000)
		e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{
			Strategies: settings.Some([]settings.RefreshStrategy{settings.RefreshExtensionCapture, settings.RefreshOriginRedirect}),
		}))
		if err := e.refresher.HandleExpired(t.Context(), task.ID, nil); err != nil {
			t.Fatal(err)
		}
		// Refresh restarts from the first strategy: the capture wait, so it just waits again.
		got, err := e.refresher.Refresh(t.Context(), task.ID)
		if err != nil || got.Status != domain.StatusAwaitingLink {
			t.Fatalf("Refresh = %+v, %v; want the task waiting", got, err)
		}
		// And it can still receive a capture.
		e.etag.Store(`"v1"`)
		att, err := e.refresher.AttachCapture(t.Context(), domain.Source{
			CurrentURL:  e.origin.URL + "/file?token=new",
			Fingerprint: domain.Fingerprint{ETag: `"v1"`, Size: int64(len(e.data))},
		})
		if err != nil || att == nil || att.Status != domain.StatusQueued {
			t.Errorf("AttachCapture after Refresh = %+v, %v; want the task queued", att, err)
		}
	})
	t.Run("a paused task cannot wait: waiting strategies are skipped", func(t *testing.T) {
		e := newRefreshEnv(t)
		e.etag.Store(`"v2"`) // the origin link yields another file, so only the waiting strategy is left
		task := e.task(t, domain.StatusPaused, 20000)
		got, err := e.refresher.Refresh(t.Context(), task.ID)
		if engine.Classify(err) != engine.KindLinkExpired || got.Status != domain.StatusPaused {
			t.Fatalf("Refresh = %+v, %v; want a link-expired error and the task left paused", got, err)
		}
		wantHistory(t, e.history(t, task.ID), "origin-redirect:mismatch", "page-reload:skipped", "extension-capture:skipped")
	})
	t.Run("running tasks conflict", func(t *testing.T) {
		e := newRefreshEnv(t)
		task := e.task(t, domain.StatusRunning, 0)
		_, err := e.refresher.Refresh(t.Context(), task.ID)
		wantErr(t, err, domain.ErrConflict)
	})
}

func TestAwait(t *testing.T) {
	e := newRefreshEnv(t)
	failed := e.task(t, domain.StatusError, 0)
	got, err := e.refresher.Await(t.Context(), failed.ID)
	if err != nil || got.Status != domain.StatusAwaitingLink || got.Error != nil {
		t.Fatalf("Await = %+v, %v", got, err)
	}
	h, _ := e.refresher.History(t.Context(), failed.ID)
	if len(h) != 1 || h[0].Strategy != "extension-capture" || h[0].Outcome != domain.RefreshWaiting || h[0].Deadline == nil {
		t.Errorf("history = %+v", h)
	}
	// Awaiting again restarts the wait.
	if _, err := e.refresher.Await(t.Context(), failed.ID); err != nil {
		t.Fatal(err)
	}
	wantHistory(t, e.history(t, failed.ID), "extension-capture:waiting", "extension-capture:waiting")

	for _, status := range []domain.TaskStatus{domain.StatusRunning, domain.StatusPaused, domain.StatusQueued} {
		task := e.task(t, status, 0)
		_, err := e.refresher.Await(t.Context(), task.ID)
		wantErr(t, err, domain.ErrConflict)
	}
	if _, err := e.refresher.Await(t.Context(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing task: %v", err)
	}
}

func TestHistoryOfMissingTask(t *testing.T) {
	e := newRefreshEnv(t)
	if _, err := e.refresher.History(t.Context(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("History = %v, want ErrNotFound", err)
	}
}

// TestRunnerRefreshesAnExpiredLink runs the whole path: the download hits a
// 403, the runner hands the failure to the refresh service, the link is
// renewed through the click URL and the second run completes the file.
func TestRunnerRefreshesAnExpiredLink(t *testing.T) {
	e := newRefreshEnv(t)
	e.autoRefresh.Store(true)
	task := e.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: e.oldLink(), OriginalURL: e.origin.URL + "/click"}})

	if err := e.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatalf("first run: %v", err)
	}
	got := e.get(t, task.ID)
	if got.Status != domain.StatusQueued || !strings.Contains(got.Source.CurrentURL, "token=new") {
		t.Fatalf("after the first run: %s %s (error %+v); want queued with the refreshed link", got.Status, got.Source.CurrentURL, got.Error)
	}
	wantHistory(t, e.history(t, task.ID), "origin-redirect:success")

	if err := e.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatalf("second run: %v", err)
	}
	done := e.get(t, task.ID)
	if done.Status != domain.StatusCompleted {
		t.Fatalf("after the second run: %s (error %+v)", done.Status, done.Error)
	}
	if diff := cmp.Diff(e.data, mustRead(t, filepath.Join(done.Dir, done.Name))); diff != "" {
		t.Errorf("file content differs (-want +got), %d bytes", len(diff))
	}
}

func TestRunnerKeepsFailingWhenRefreshIsOff(t *testing.T) {
	e := newRefreshEnv(t) // autoRefresh stays off
	task := e.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: e.oldLink(), OriginalURL: e.origin.URL + "/click"}})
	if err := e.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	got := e.get(t, task.ID)
	if got.Status != domain.StatusError || got.Error == nil || got.Error.Code != domain.ErrorCodeLinkExpired {
		t.Errorf("task = %s %+v, want the link_expired error", got.Status, got.Error)
	}
}

func TestRunnerStoppedWhileRefreshing(t *testing.T) {
	e := newRefreshEnv(t)
	e.autoRefresh.Store(true)
	task := e.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: e.oldLink(), OriginalURL: e.origin.URL + "/click"}})
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(ErrPaused) // the pause arrives while the chain is about to start
	if err := e.runner.Run(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, task.ID); got.Status != domain.StatusPaused {
		t.Errorf("status = %s, want paused", got.Status)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A file name the server announced (Content-Disposition) identifies the file;
// one that is only the last path segment of the link does not.
func TestFilenameFromHeaderCountsForValidation(t *testing.T) {
	e := newRefreshEnv(t)
	e.name.Store("other.bin") // the fresh link names another file
	task := e.task(t, domain.StatusError, 20000)
	_, err := e.refresher.SetSource(t.Context(), task.ID, SetSourceInput{Source: domain.Source{CurrentURL: e.origin.URL + "/file?token=new"}})
	wantErr(t, err, domain.ErrConflict)
	if !strings.Contains(err.Error(), "filename differs") {
		t.Errorf("err = %v, want the file name mismatch", err)
	}
	e.name.Store("file.bin") // same announced name: fine
	if _, err := e.refresher.SetSource(t.Context(), task.ID, SetSourceInput{Source: domain.Source{CurrentURL: e.origin.URL + "/file?token=new"}}); err != nil {
		t.Errorf("same announced file name: %v", err)
	}
}

func TestWaitingForTheExtensionIsAnnounced(t *testing.T) {
	e := newRefreshEnv(t)
	task := e.task(t, domain.StatusRunning, 0)
	e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{
		Strategies:   settings.Some([]settings.RefreshStrategy{settings.RefreshExtensionCapture}),
		AwaitTimeout: settings.Some(settings.Duration(time.Minute)),
	}))
	sub := e.bus.Subscribe(t.Context(), events.Filter{Types: []events.Type{events.TypeTaskRefreshRequested}})
	if err := e.refresher.HandleExpired(t.Context(), task.ID, engine.LinkExpired(errors.New("403"))); err != nil {
		t.Fatal(err)
	}
	got := collect(sub)
	if len(got) != 1 || got[0].TaskID != task.ID {
		t.Fatalf("events = %+v, want one for the task", got)
	}
	d, ok := got[0].Data.(events.RefreshRequestedData)
	if !ok {
		t.Fatalf("data = %T", got[0].Data)
	}
	// The page defaults to the link the task started from.
	if d.Name != "file.bin" || d.PageURL != e.origin.URL+"/click" || d.OriginalURL != e.origin.URL+"/click" ||
		d.Deadline == nil || d.Deadline.Sub(e.clock.Now()).Abs() > time.Second+time.Minute || d.Fingerprint.Size != int64(len(e.data)) {
		t.Errorf("data = %+v", d)
	}

	// A manual wait is not for the extension.
	manual := e.task(t, domain.StatusRunning, 0)
	e.setSettings(t, manual.ID, refreshSettings(settings.RefreshSettings{
		Strategies: settings.Some([]settings.RefreshStrategy{settings.RefreshManual}),
	}))
	sub = e.bus.Subscribe(t.Context(), events.Filter{Types: []events.Type{events.TypeTaskRefreshRequested}})
	if err := e.refresher.HandleExpired(t.Context(), manual.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := collect(sub); len(got) != 0 {
		t.Errorf("a manual wait was announced to the extension: %+v", got)
	}
}

func TestCaptureOfTheSameClickedLinkMatchesWithoutAFileName(t *testing.T) {
	e := newRefreshEnv(t)
	task := e.task(t, domain.StatusRunning, 0)
	e.setSettings(t, task.ID, refreshSettings(settings.RefreshSettings{
		Strategies: settings.Some([]settings.RefreshStrategy{settings.RefreshExtensionCapture}),
	}))
	if err := e.refresher.HandleExpired(t.Context(), task.ID, nil); err != nil {
		t.Fatal(err)
	}
	newLink := e.origin.URL + "/file?token=new"

	// Another link, nothing known about the file: no match.
	if got, err := e.refresher.AttachCapture(t.Context(), domain.Source{CurrentURL: newLink, OriginalURL: "https://example.com/other"}); err != nil || got != nil {
		t.Errorf("a different link matched: %+v, %v", got, err)
	}
	// The same clicked link with a size that contradicts the task: no match.
	if got, err := e.refresher.AttachCapture(t.Context(), domain.Source{CurrentURL: newLink, OriginalURL: e.origin.URL + "/click", Fingerprint: domain.Fingerprint{Size: 7}}); err != nil || got != nil {
		t.Errorf("a contradicting capture matched: %+v, %v", got, err)
	}
	// The same clicked link, with no fingerprint at all (the browser knew nothing): the task gets it.
	got, err := e.refresher.AttachCapture(t.Context(), domain.Source{CurrentURL: newLink, OriginalURL: e.origin.URL + "/click"})
	if err != nil || got == nil || got.ID != task.ID || got.Source.CurrentURL != newLink {
		t.Fatalf("AttachCapture = %+v, %v; want the task with the new link", got, err)
	}
}

// A manual refresh of a task that waits for a link, cancelled half-way, must
// not leave it in refreshing: no runner settles a manual refresh, and
// refreshing blocks another refresh and ignores resume.
func TestCancelledManualRefreshReturnsToWaiting(t *testing.T) {
	e := newRefreshEnv(t)
	task := e.task(t, domain.StatusAwaitingLink, 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.refresher.Refresh(ctx, task.ID); err == nil {
		t.Fatal("a cancelled refresh reported success")
	}
	if got := e.get(t, task.ID); got.Status != domain.StatusAwaitingLink {
		t.Errorf("status after the cancelled refresh = %s, want awaiting_link", got.Status)
	}
	// And a refresh can be made again.
	if got, err := e.refresher.Refresh(t.Context(), task.ID); err != nil || got.Status != domain.StatusQueued {
		t.Errorf("the next refresh = %+v, %v", got, err)
	}
}
