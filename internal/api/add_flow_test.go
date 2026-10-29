package api

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/domain"
)

func TestExpandTasks(t *testing.T) {
	f := newOpsFixture(t)
	var out ExpandedPattern
	f.call(t, http.MethodPost, "/v1/tasks/expand", `{"pattern":"http://h/img[001-100].jpg","preview":3}`, http.StatusOK, &out)
	if out.Count != 100 || len(out.URLs) != 3 || out.URLs[0] != "http://h/img001.jpg" || out.URLs[2] != "http://h/img003.jpg" {
		t.Errorf("expanded = %+v", out)
	}
	f.call(t, http.MethodPost, "/v1/tasks/expand", `{"pattern":"http://h/plain.bin"}`, http.StatusOK, &out)
	if out.Count != 1 || len(out.URLs) != 1 {
		t.Errorf("a pattern without ranges = %+v", out)
	}
	f.call(t, http.MethodPost, "/v1/tasks/expand", `{"pattern":"http://h/[1-99999]/[1-99999]"}`, http.StatusUnprocessableEntity, nil)
}

type resolveBatchOut struct {
	Results []struct {
		OK     bool `json:"ok"`
		Result *struct {
			ResolveID   string `json:"resolveId"`
			Name        string `json:"name"`
			DuplicateOf *struct {
				TaskID string `json:"taskId"`
			} `json:"duplicateOf"`
		} `json:"result"`
		Error *Problem `json:"error"`
	} `json:"results"`
}

func TestResolveBatchAndDuplicateHints(t *testing.T) {
	f := newOpsFixture(t)
	body := `{"items":[{"url":"` + f.fileURL() + `"},{"url":"` + f.origin.URL + `/broken"},{}]}`
	var out resolveBatchOut
	f.call(t, http.MethodPost, "/v1/resolve/batch", body, http.StatusOK, &out)
	if len(out.Results) != 3 {
		t.Fatalf("results = %d", len(out.Results))
	}
	if r := out.Results[0]; !r.OK || r.Result == nil || r.Result.ResolveID == "" || r.Result.DuplicateOf != nil {
		t.Errorf("first = %+v", r)
	}
	if r := out.Results[1]; r.OK || r.Error == nil {
		t.Errorf("a failing probe must be an error item: %+v", r)
	}
	if r := out.Results[2]; r.OK || r.Error == nil || r.Error.Status != http.StatusUnprocessableEntity {
		t.Errorf("an item without a source = %+v", r)
	}

	// Once a task has the link, resolving it again says so.
	task := f.createTask(t, `{"url":"`+f.fileURL()+`","paused":true}`)
	f.call(t, http.MethodPost, "/v1/resolve/batch", `{"items":[{"url":"`+f.fileURL()+`"}]}`, http.StatusOK, &out)
	if r := out.Results[0]; r.Result == nil || r.Result.DuplicateOf == nil || r.Result.DuplicateOf.TaskID != task.ID {
		t.Errorf("duplicate hint = %+v", r)
	}
	var single struct {
		DuplicateOf *struct {
			TaskID string `json:"taskId"`
		} `json:"duplicateOf"`
	}
	f.call(t, http.MethodPost, "/v1/resolve", `{"url":"`+f.fileURL()+`"}`, http.StatusOK, &single)
	if single.DuplicateOf == nil || single.DuplicateOf.TaskID != task.ID {
		t.Errorf("resolve-task duplicate hint = %+v", single.DuplicateOf)
	}
}

func TestImportDryRun(t *testing.T) {
	f := newOpsFixture(t)
	existing := f.createTask(t, `{"url":"http://example.test/have.bin","paused":true}`)
	content := "http://example.test/have.bin\nhttp://example.test/part[1-3].bin\nnot a url\n"
	var out struct {
		Results []struct {
			OK    bool     `json:"ok"`
			URL   string   `json:"url"`
			Error *Problem `json:"error"`
		} `json:"results"`
	}
	body := `{"format":"urls","content":` + quoteJSON(content) + `,"expandPatterns":true,"dryRun":true}`
	f.call(t, http.MethodPost, "/v1/tasks/import", body, http.StatusOK, &out)
	if len(out.Results) != 5 {
		t.Fatalf("results = %+v", out.Results)
	}
	if r := out.Results[0]; r.OK || r.URL != "http://example.test/have.bin" || r.Error == nil || r.Error.Code != "task.duplicate" {
		t.Errorf("the existing link = %+v", r)
	}
	for i, want := range []string{"part1", "part2", "part3"} {
		if r := out.Results[i+1]; !r.OK || !strings.HasSuffix(r.URL, want+".bin") {
			t.Errorf("expanded entry %d = %+v", i, r)
		}
	}
	if r := out.Results[4]; r.OK || r.Error == nil {
		t.Errorf("an invalid entry = %+v", r)
	}
	// Nothing was created.
	var list struct {
		Tasks []*domain.Task `json:"tasks"`
	}
	f.call(t, http.MethodGet, "/v1/tasks", "", http.StatusOK, &list)
	if len(list.Tasks) != 1 || list.Tasks[0].ID != existing.ID {
		t.Errorf("a dry run created tasks: %d", len(list.Tasks))
	}
	// The same list for real adds the new entries and reports their links.
	body = strings.Replace(body, `"dryRun":true`, `"dryRun":false`, 1)
	f.call(t, http.MethodPost, "/v1/tasks/import", body, http.StatusOK, &out)
	if r := out.Results[1]; !r.OK || !strings.HasSuffix(r.URL, "part1.bin") {
		t.Errorf("real import entry = %+v", r)
	}
}

func newFSServer(t *testing.T, roots ...string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Server.FSRoots = roots
	s, err := New(&cfg, Deps{Token: testToken, Version: testVersion})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFSBrowseAndFree(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"movies/2026", "music", "docs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := newFSServer(t, root)
	q := func(path string) string { return "?path=" + url.QueryEscape(path) }

	var l FSListing
	callServer(t, s, http.MethodGet, "/v1/fs/browse", "", "", http.StatusOK, &l)
	if len(l.Entries) != 1 || l.Entries[0].Path != filepath.Clean(root) {
		t.Errorf("roots = %+v", l)
	}
	callServer(t, s, http.MethodGet, "/v1/fs/browse"+q(root), "", "", http.StatusOK, &l)
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "docs,movies,music" || l.Parent != "" {
		t.Errorf("listing = %+v", l)
	}
	callServer(t, s, http.MethodGet, "/v1/fs/browse"+q(filepath.Join(root, "movies")), "", "", http.StatusOK, &l)
	if len(l.Entries) != 1 || l.Entries[0].Name != "2026" || l.Parent != filepath.Clean(root) {
		t.Errorf("sub listing = %+v", l)
	}
	var free FSFree
	callServer(t, s, http.MethodGet, "/v1/fs/free"+q(root), "", "", http.StatusOK, &free)
	if free.FreeBytes == 0 {
		t.Errorf("free = %+v", free)
	}

	// Outside the roots, relative, missing, a file.
	for _, tc := range []struct {
		path   string
		status int
	}{
		{filepath.Dir(root), http.StatusUnprocessableEntity},
		{filepath.Join(root, "..", "x"), http.StatusUnprocessableEntity},
		{"relative", http.StatusUnprocessableEntity},
		{filepath.Join(root, "nope"), http.StatusNotFound},
		{filepath.Join(root, "file.txt"), http.StatusUnprocessableEntity},
	} {
		callServer(t, s, http.MethodGet, "/v1/fs/browse"+q(tc.path), "", "", tc.status, nil)
		callServer(t, s, http.MethodGet, "/v1/fs/free"+q(tc.path), "", "", tc.status, nil)
	}

	// Without roots the feature is off.
	off := newFSServer(t)
	callServer(t, off, http.MethodGet, "/v1/fs/browse", "", "", http.StatusNotFound, nil)
	callServer(t, off, http.MethodGet, "/v1/fs/free"+q(root), "", "", http.StatusNotFound, nil)
}
