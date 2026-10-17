package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/browser"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine/browserx"
)

// fakeBrowser is a service.BrowserBackend with canned answers.
type fakeBrowser struct {
	result   *browser.PageResult
	err      error
	cands    map[string]browser.Candidate
	profiles []string
	deleted  string
	gotURL   string
}

func (f *fakeBrowser) ResolvePage(_ context.Context, pageURL string, _ browser.ResolveOptions) (*browser.PageResult, error) {
	f.gotURL = pageURL
	return f.result, f.err
}

func (f *fakeBrowser) Candidate(id string, index int) (browser.Candidate, error) {
	c, ok := f.cands[browserx.URL(id, index)]
	if !ok {
		return browser.Candidate{}, browser.ErrResultGone
	}
	return c, nil
}

func (f *fakeBrowser) Status() browser.Status {
	return browser.Status{Browser: &browser.Installation{Name: "chrome", Path: "/x/chrome"}, Running: []browser.Running{{Group: "a.com", Sessions: 1}}, Max: 2}
}
func (f *fakeBrowser) Profiles() ([]string, error)      { return f.profiles, f.err }
func (f *fakeBrowser) DeleteProfile(group string) error { f.deleted = group; return f.err }

func TestBrowserOperations(t *testing.T) {
	f := newOpsFixture(t)
	f.browser.result = &browser.PageResult{
		ID: "r1", URL: "https://example.com/watch", FinalURL: "https://example.com/watch", Title: "Watch", Group: "example.com",
		Candidates: []browser.Candidate{{Index: 0, URL: f.fileURL(), Kind: browser.KindFile, Size: int64(len(fileContent)), Filename: "file.bin", Source: "network"}},
	}
	f.browser.cands = map[string]browser.Candidate{browserx.URL("r1", 0): f.browser.result.Candidates[0]}
	f.browser.cands[browserx.URL("r1", 0)] = func() browser.Candidate {
		c := f.browser.result.Candidates[0]
		c.Page = "https://example.com/watch"
		return c
	}()

	var res browser.PageResult
	f.call(t, http.MethodPost, "/v1/browser/resolve", `{"url":"https://example.com/watch","waitSeconds":5}`, http.StatusOK, &res)
	if res.ID != "r1" || res.Title != "Watch" || len(res.Candidates) != 1 || res.Candidates[0].Kind != browser.KindFile || f.browser.gotURL != "https://example.com/watch" {
		t.Errorf("resolve-page = %+v", res)
	}

	var task domain.Task
	f.call(t, http.MethodPost, "/v1/browser/results/r1/candidates/0/download", `{"paused":true,"priority":"high","name":"mine.bin"}`, http.StatusCreated, &task)
	if task.Status != domain.StatusPaused || task.Name != "mine.bin" || task.Priority != domain.PriorityHigh || task.Source.CurrentURL != f.fileURL() ||
		task.Source.PageURL != "https://example.com/watch" || task.Source.ResolverKind != domain.ResolverBrowser {
		t.Errorf("task = %+v", task)
	}

	var st browser.Status
	f.call(t, http.MethodGet, "/v1/browser", "", http.StatusOK, &st)
	if st.Browser == nil || st.Browser.Name != "chrome" || len(st.Running) != 1 || st.Max != 2 {
		t.Errorf("status = %+v", st)
	}

	f.browser.profiles = []string{"a.com", "b.com"}
	var profiles BrowserProfiles
	f.call(t, http.MethodGet, "/v1/browser/profiles", "", http.StatusOK, &profiles)
	if len(profiles.Profiles) != 2 {
		t.Errorf("profiles = %v", profiles.Profiles)
	}
	f.call(t, http.MethodDelete, "/v1/browser/profiles/a.com", "", http.StatusNoContent, nil)
	if f.browser.deleted != "a.com" {
		t.Errorf("deleted %q", f.browser.deleted)
	}
}

func TestBrowserOperationErrors(t *testing.T) {
	f := newOpsFixture(t)
	req := func(method, path, body string, want int) string {
		t.Helper()
		w := do(t, f.s, request{method: method, path: path, body: body, headers: bearer(testToken)})
		assertProblem(t, w, want)
		return w.Body.String()
	}
	// Invalid requests.
	req(http.MethodPost, "/v1/browser/resolve", `{}`, http.StatusUnprocessableEntity)
	req(http.MethodPost, "/v1/browser/resolve", `{"url":"x","waitSeconds":500}`, http.StatusUnprocessableEntity)
	req(http.MethodPost, "/v1/browser/results/r1/candidates/-1/download", `{}`, http.StatusUnprocessableEntity)
	// No browser installed: 503 with the reason.
	f.browser.err = browser.ErrNoBrowser
	if body := req(http.MethodPost, "/v1/browser/resolve", `{"url":"https://example.com/"}`, http.StatusServiceUnavailable); !strings.Contains(body, "browser") {
		t.Errorf("503 body = %s", body)
	}
	// A page that cannot be opened: bad gateway with the browser code.
	f.browser.err = errString("net::ERR_NAME_NOT_RESOLVED")
	if body := req(http.MethodPost, "/v1/browser/resolve", `{"url":"https://nope.invalid/"}`, http.StatusBadGateway); !strings.Contains(body, "[browser]") {
		t.Errorf("502 body = %s", body)
	}
	// Expired result, missing profile, running browser.
	f.browser.err = nil
	req(http.MethodPost, "/v1/browser/results/gone/candidates/0/download", `{}`, http.StatusNotFound)
	f.browser.err = domain.NotFound("browser profile", "x")
	req(http.MethodDelete, "/v1/browser/profiles/x", "", http.StatusNotFound)
	f.browser.err = domain.Conflict("running")
	req(http.MethodDelete, "/v1/browser/profiles/x", "", http.StatusConflict)
	// A stream no engine can recognise is refused with 422.
	f.browser.err = nil
	f.browser.cands = map[string]browser.Candidate{browserx.URL("r", 0): {URL: "https://x/manifest?id=7", Kind: browser.KindDASH}}
	req(http.MethodPost, "/v1/browser/results/r/candidates/0/download", `{}`, http.StatusUnprocessableEntity)
	// Authentication.
	assertProblem(t, do(t, f.s, request{method: http.MethodGet, path: "/v1/browser"}), http.StatusUnauthorized)
}

type errString string

func (e errString) Error() string { return string(e) }
