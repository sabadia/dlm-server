package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/browser"
	"github.com/sabadia/dlm-server/internal/client"
	"github.com/sabadia/dlm-server/internal/engine/browserx"
)

// fakeBrowser is a service.BrowserBackend with canned answers.
type fakeBrowser struct {
	result   *browser.PageResult
	err      error
	cands    map[string]browser.Candidate
	profiles []string
	deleted  string
}

func (f *fakeBrowser) ResolvePage(context.Context, string, browser.ResolveOptions) (*browser.PageResult, error) {
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
	return browser.Status{Browser: &browser.Installation{Name: "chrome", Path: "/x/chrome", Version: "1.2"}, Running: []browser.Running{{Group: "a.com", Sessions: 2}}, Max: 2}
}
func (f *fakeBrowser) Profiles() ([]string, error)      { return f.profiles, f.err }
func (f *fakeBrowser) DeleteProfile(group string) error { f.deleted = group; return f.err }

func TestBrowserCommandsEndToEnd(t *testing.T) {
	e := newE2E(t)
	cand := browser.Candidate{Index: 0, URL: e.url("v.mp4"), Kind: browser.KindVideo, MIME: "video/mp4", Size: 2048, Source: "network", Page: "https://example.com/watch"}
	e.browser.result = &browser.PageResult{
		ID: "r1", URL: "https://example.com/watch", FinalURL: "https://example.com/watch", Title: "Watch me", Group: "example.com",
		Candidates: []browser.Candidate{cand, {Index: 1, URL: "blob:https://example.com/x", Kind: browser.KindFile, Size: -1, Source: "blob", Filename: "made.txt"}},
	}
	e.browser.cands = map[string]browser.Candidate{browserx.URL("r1", 0): cand}

	out := e.ok(t, nil, "browser", "resolve", "https://example.com/watch", "--wait", "5")
	if !strings.Contains(out, "result r1: Watch me (2 downloads") || !strings.Contains(out, "INDEX KIND SIZE SOURCE NAME URL") && !strings.Contains(strings.Join(strings.Fields(out), " "), "INDEX KIND SIZE SOURCE NAME URL") ||
		!strings.Contains(out, "video") || !strings.Contains(out, "made.txt") {
		t.Errorf("resolve table:\n%s", out)
	}
	var res client.PageResult
	e.ok(t, &res, "browser", "resolve", "https://example.com/watch", "-o", "json")
	if res.ID != "r1" || len(res.Candidates) != 2 || res.Candidates[1].Size != -1 {
		t.Errorf("resolve json = %+v", res)
	}

	var task client.Task
	e.ok(t, &task, "browser", "download", "r1", "0", "--paused", "--name", "mine.mp4", "--priority", "high", "-o", "json")
	if task.Status != "paused" || task.Name != "mine.mp4" || task.Priority != "high" || task.Source.CurrentURL != cand.URL || task.Source.ResolverKind != "browser" {
		t.Errorf("task = %+v source %+v", task, task.Source)
	}
	if r := e.run(t, "", "browser", "download", "r1", "x"); r.code != ExitUsage || !strings.Contains(r.stderr, "INDEX") {
		t.Errorf("bad index: exit %d stderr %s", r.code, r.stderr)
	}
	if r := e.run(t, "", "browser", "download", "gone", "0"); r.code != ExitError || !strings.Contains(r.stderr, "Not Found") {
		t.Errorf("expired result: exit %d stderr %s", r.code, r.stderr)
	}

	out = e.ok(t, nil, "browser", "status")
	if !strings.Contains(out, "chrome 1.2") || !strings.Contains(out, "a.com(2)") {
		t.Errorf("status table:\n%s", out)
	}
	e.browser.profiles = []string{"a.com", "b.com"}
	out = e.ok(t, nil, "browser", "profiles")
	if !strings.Contains(out, "a.com") || !strings.Contains(out, "b.com") {
		t.Errorf("profiles table:\n%s", out)
	}
	var profiles []string
	e.ok(t, &profiles, "browser", "profiles", "list", "-o", "json")
	if len(profiles) != 2 {
		t.Errorf("profiles json = %v", profiles)
	}
	if out = e.ok(t, nil, "browser", "profiles", "rm", "a.com"); strings.TrimSpace(out) != "deleted a.com" || e.browser.deleted != "a.com" {
		t.Errorf("profiles rm: %q (deleted %q)", out, e.browser.deleted)
	}

	e.browser.err = browser.ErrNoBrowser
	if r := e.run(t, "", "browser", "resolve", "https://example.com/"); r.code != ExitError || !strings.Contains(r.stderr, "Service Unavailable") {
		t.Errorf("no browser: exit %d stderr %s", r.code, r.stderr)
	}
}
