package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
)

// setState drives a task to a state directly in the store.
func (f *opsFixture) setState(t *testing.T, id string, path ...domain.TaskStatus) {
	t.Helper()
	ctx := context.Background()
	task, err := f.store.Tasks().Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range path {
		if to == domain.StatusError {
			err = task.SetError(domain.TaskError{Code: domain.ErrorCodeLinkExpired, Message: "403"}, time.Now())
		} else {
			err = task.TransitionTo(to, time.Now())
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Tasks().Update(ctx, task); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshOperations(t *testing.T) {
	f := newOpsFixture(t)
	task := f.createTask(t, `{"url":"`+f.fileURL()+`","paused":true}`)
	base := "/v1/tasks/" + task.ID

	// A paused task can have its link replaced; the status is kept.
	var got domain.Task
	newURL := f.fileURL() + "?fresh=1"
	f.call(t, http.MethodPatch, base+"/source", `{"currentUrl":"`+newURL+`","referrer":"https://example.com/","headers":{"X-A":"1"},"onMismatch":"restart"}`, http.StatusOK, &got)
	if got.Status != domain.StatusPaused || got.Source.CurrentURL != newURL || got.Source.Referrer != "https://example.com/" ||
		got.Source.Headers["X-A"] != "1" || got.Source.OriginalURL != f.fileURL() {
		t.Errorf("task after update-task-source = %+v", got.Source)
	}
	// Merge-patch media type is accepted too.
	callServer(t, f.s, http.MethodPatch, base+"/source", `{"currentUrl":"`+f.fileURL()+`"}`, mergePatchContentType, http.StatusOK, &got)

	// The origin URL still works, so a refresh succeeds without changing the state.
	f.call(t, http.MethodPost, base+"/refresh", "", http.StatusOK, &got)
	if got.Status != domain.StatusPaused {
		t.Errorf("status after refresh = %s, want paused", got.Status)
	}

	var hist RefreshHistory
	f.call(t, http.MethodGet, base+"/refresh/history", "", http.StatusOK, &hist)
	if len(hist.Attempts) != 3 || hist.Attempts[0].Outcome != domain.RefreshSuccess || hist.Attempts[0].Cause != domain.RefreshCauseSource ||
		hist.Attempts[2].Strategy != "origin-redirect" || hist.Attempts[2].Cause != domain.RefreshCauseManual {
		t.Errorf("history = %+v", hist.Attempts)
	}

	// Await needs a failed or awaiting task.
	assertProblem(t, do(t, f.s, request{method: http.MethodPost, path: base + "/refresh/await", headers: bearer(testToken)}), http.StatusConflict)
	f.setState(t, task.ID, domain.StatusQueued)
	f.setState(t, task.ID, domain.StatusRunning, domain.StatusError)
	f.call(t, http.MethodPost, base+"/refresh/await", "", http.StatusOK, &got)
	if got.Status != domain.StatusAwaitingLink || got.Error != nil {
		t.Errorf("task after await = %s %+v", got.Status, got.Error)
	}
	f.call(t, http.MethodGet, base+"/refresh/history", "", http.StatusOK, &hist)
	if last := hist.Attempts[len(hist.Attempts)-1]; last.Outcome != domain.RefreshWaiting || last.Deadline == nil {
		t.Errorf("last history entry = %+v", last)
	}
}

func TestRefreshOperationErrors(t *testing.T) {
	f := newOpsFixture(t)
	task := f.createTask(t, `{"url":"`+f.fileURL()+`","paused":true}`)
	base := "/v1/tasks/" + task.ID
	req := func(method, path, body string, want int) {
		t.Helper()
		assertProblem(t, do(t, f.s, request{method: method, path: path, body: body, headers: bearer(testToken)}), want)
	}
	// Unknown task.
	req(http.MethodPost, "/v1/tasks/missing/refresh", "", http.StatusNotFound)
	req(http.MethodPost, "/v1/tasks/missing/refresh/await", "", http.StatusNotFound)
	req(http.MethodPatch, "/v1/tasks/missing/source", `{"currentUrl":"http://x/y"}`, http.StatusNotFound)
	req(http.MethodGet, "/v1/tasks/missing/refresh/history", "", http.StatusNotFound)
	// Invalid bodies.
	req(http.MethodPatch, base+"/source", `{}`, http.StatusUnprocessableEntity)
	req(http.MethodPatch, base+"/source", `{"currentUrl":"/relative"}`, http.StatusUnprocessableEntity)
	req(http.MethodPatch, base+"/source", `{"currentUrl":"http://x/y","onMismatch":"ask"}`, http.StatusUnprocessableEntity)
	// A link that does not work: the origin answered 404, so it is a bad gateway.
	req(http.MethodPatch, base+"/source", `{"currentUrl":"`+f.origin.URL+`/gone"}`, http.StatusBadGateway)
	// Running tasks must be paused first.
	f.setState(t, task.ID, domain.StatusQueued, domain.StatusRunning)
	req(http.MethodPost, base+"/refresh", "", http.StatusConflict)
	req(http.MethodPatch, base+"/source", `{"currentUrl":"`+f.fileURL()+`"}`, http.StatusConflict)
	req(http.MethodPost, base+"/refresh/await", "", http.StatusConflict)
	// Authentication is required.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, base + "/refresh"},
		{http.MethodPost, base + "/refresh/await"},
		{http.MethodPatch, base + "/source"},
		{http.MethodGet, base + "/refresh/history"},
	} {
		assertProblem(t, do(t, f.s, request{method: tc.method, path: tc.path, body: "{}"}), http.StatusUnauthorized)
	}
}

func TestRefreshOfAnUnrecoverableLinkIsABadGateway(t *testing.T) {
	f := newOpsFixture(t)
	task := f.createTask(t, `{"url":"`+f.origin.URL+`/gone","paused":true}`)
	w := do(t, f.s, request{method: http.MethodPost, path: "/v1/tasks/" + task.ID + "/refresh", headers: bearer(testToken)})
	assertProblem(t, w, http.StatusBadGateway)
	if !strings.Contains(w.Body.String(), "link_expired") {
		t.Errorf("problem = %s, want the link_expired code", w.Body)
	}
}

// TestCaptureRefreshesAWaitingTask: a task that waits for a new link gets the
// link of the next matching capture instead of a duplicate task being created.
func TestCaptureRefreshesAWaitingTask(t *testing.T) {
	f := newOpsFixture(t)
	task := f.createTask(t, `{"url":"`+f.fileURL()+`","paused":true}`)
	base := "/v1/tasks/" + task.ID
	// The task knows its file (size, name) from an earlier download.
	stored, err := f.store.Tasks().Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Source.Fingerprint = domain.Fingerprint{Size: int64(len(fileContent)), Filename: "file.bin"}
	if err := f.store.Tasks().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	f.setState(t, task.ID, domain.StatusQueued, domain.StatusRunning, domain.StatusError)
	f.call(t, http.MethodPost, base+"/refresh/await", "", http.StatusOK, nil)

	var out CaptureOutput
	body := `{"url":"` + f.origin.URL + `/file.bin?fresh=1","filename":"file.bin","size":` + strconv.Itoa(len(fileContent)) + `}`
	f.call(t, http.MethodPost, "/v1/capture", body, http.StatusCreated, &out)
	if !out.Refreshed || out.Task == nil || out.Task.ID != task.ID || out.Task.Status != domain.StatusQueued ||
		!strings.Contains(out.Task.Source.CurrentURL, "fresh=1") {
		t.Fatalf("capture result = %+v", out)
	}
	var list TaskList
	f.call(t, http.MethodGet, "/v1/tasks", "", http.StatusOK, &list)
	if len(list.Tasks) != 1 {
		t.Errorf("%d tasks after the capture, want the one that was refreshed", len(list.Tasks))
	}
	// A second capture matches nothing: it creates a task as usual.
	f.call(t, http.MethodPost, "/v1/capture", body, http.StatusCreated, &out)
	if out.Refreshed {
		t.Errorf("a capture refreshed a task that no longer waits: %+v", out)
	}
}
