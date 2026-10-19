package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
)

type batchBody struct {
	Results []struct {
		ID    string       `json:"id"`
		OK    bool         `json:"ok"`
		Task  *domain.Task `json:"task"`
		Error *struct {
			Status int `json:"status"`
		} `json:"error"`
	} `json:"results"`
}

func TestCreateTasksExpandsPatterns(t *testing.T) {
	f := newOpsFixture(t)
	var res batchBody
	base := f.origin.URL
	f.call(t, http.MethodPost, "/v1/tasks/batch",
		`{"expandPatterns":true,"items":[{"url":"`+base+`/img[08-10].jpg","paused":true},{"url":"`+base+`/n[1-3].jpg","name":"x.jpg"},{"url":"`+base+`/plain.jpg","paused":true}]}`,
		http.StatusOK, &res)
	if len(res.Results) != 5 {
		t.Fatalf("%d results: %+v", len(res.Results), res.Results)
	}
	for i, want := range []string{"img08.jpg", "img09.jpg", "img10.jpg"} {
		r := res.Results[i]
		if !r.OK || !strings.HasSuffix(r.Task.Source.CurrentURL, want) {
			t.Errorf("result %d = %+v", i, r)
		}
	}
	if r := res.Results[3]; r.OK || r.Error.Status != http.StatusUnprocessableEntity {
		t.Errorf("a name with a pattern: %+v", r)
	}
	if !res.Results[4].OK {
		t.Errorf("plain: %+v", res.Results[4])
	}

	// Without the flag the brackets stay in the link.
	f.call(t, http.MethodPost, "/v1/tasks/batch", `{"items":[{"url":"`+base+`/lit[1-2].jpg","paused":true}]}`, http.StatusOK, &res)
	if len(res.Results) != 1 || !res.Results[0].OK || !strings.Contains(res.Results[0].Task.Source.CurrentURL, "[1-2]") {
		t.Errorf("unexpanded = %+v", res.Results)
	}
	// An expansion over the limit is an item error, not a failed request.
	f.call(t, http.MethodPost, "/v1/tasks/batch", `{"expandPatterns":true,"items":[{"url":"`+base+`/f[1-99999].jpg"}]}`, http.StatusOK, &res)
	if len(res.Results) != 1 || res.Results[0].OK || res.Results[0].Error.Status != http.StatusUnprocessableEntity {
		t.Errorf("too many = %+v", res.Results)
	}
}

func TestTaskMirrorsAreStoredAndValidated(t *testing.T) {
	f := newOpsFixture(t)
	var task domain.Task
	f.call(t, http.MethodPost, "/v1/tasks", `{"source":{"currentUrl":"`+f.fileURL()+`","mirrors":["https://m1.example/f.bin","http://m2.example/f.bin"]},"paused":true}`, http.StatusCreated, &task)
	if len(task.Source.Mirrors) != 2 {
		t.Fatalf("mirrors = %v", task.Source.Mirrors)
	}
	var got domain.Task
	f.call(t, http.MethodGet, "/v1/tasks/"+task.ID, "", http.StatusOK, &got)
	if len(got.Source.Mirrors) != 2 || got.Source.Mirrors[1] != "http://m2.example/f.bin" {
		t.Errorf("stored mirrors = %v", got.Source.Mirrors)
	}
	f.call(t, http.MethodPost, "/v1/tasks", `{"source":{"currentUrl":"`+f.fileURL()+`","mirrors":["ftp://m.example/f"]},"allowDuplicate":true}`, http.StatusUnprocessableEntity, nil)
}

func TestImportExportAndStats(t *testing.T) {
	f := newOpsFixture(t)
	var imp batchBody
	list := f.origin.URL + "/a.bin\n# comment\n" + f.origin.URL + "/b.bin\n"
	f.call(t, http.MethodPost, "/v1/tasks/import", `{"format":"urls","content":`+quoteJSON(list)+`,"paused":true}`, http.StatusOK, &imp)
	if len(imp.Results) != 2 || !imp.Results[0].OK || !imp.Results[1].OK {
		t.Fatalf("import = %+v", imp.Results)
	}
	f.call(t, http.MethodPost, "/v1/tasks/import", `{"format":"json","content":"{"}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPost, "/v1/tasks/import", `{"format":"xml","content":"x"}`, http.StatusUnprocessableEntity, nil)

	var exp ExportedTasks
	f.call(t, http.MethodGet, "/v1/tasks/export?format=urls", "", http.StatusOK, &exp)
	if exp.Count != 2 || exp.Format != "urls" || !strings.Contains(exp.Content, "/a.bin\n") {
		t.Errorf("export = %+v", exp)
	}
	f.call(t, http.MethodGet, "/v1/tasks/export?format=aria2&q=b.bin&status=paused", "", http.StatusOK, &exp)
	if exp.Count != 1 || !strings.Contains(exp.Content, "/b.bin") || strings.Contains(exp.Content, "/a.bin") {
		t.Errorf("filtered aria2 export = %+v", exp)
	}
	f.call(t, http.MethodGet, "/v1/tasks/export", "", http.StatusOK, &exp)
	if exp.Format != "json" || !strings.Contains(exp.Content, `"version": 1`) {
		t.Errorf("default format = %+v", exp)
	}
	f.call(t, http.MethodGet, "/v1/tasks/export?format=xml", "", http.StatusUnprocessableEntity, nil)

	var st StatsBody
	f.call(t, http.MethodGet, "/v1/stats", "", http.StatusOK, &st)
	if st.Tasks != 2 || st.ByStatus["paused"] != 2 || len(st.Completed) != 3 || st.ByEngine == nil {
		t.Errorf("stats = %+v", st)
	}
}

func quoteJSON(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func TestCredentialOperations(t *testing.T) {
	f := newOpsFixture(t)
	var list struct {
		Credentials []domain.Credential `json:"credentials"`
	}
	f.call(t, http.MethodGet, "/v1/credentials", "", http.StatusOK, &list)
	if list.Credentials == nil {
		t.Fatal("an empty list must be [], not null")
	}
	raw := do(t, f.s, request{
		method: http.MethodPost, path: "/v1/credentials", headers: bearer(testToken),
		body: `{"host":"Files.Example.com","username":"ann","password":"hunter2"}`,
	})
	if raw.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", raw.Code, raw.Body)
	}
	if strings.Contains(raw.Body.String(), "hunter2") || strings.Contains(strings.ToLower(raw.Body.String()), "password") {
		t.Errorf("the response mentions the password: %s", raw.Body)
	}
	var c domain.Credential
	f.call(t, http.MethodPost, "/v1/credentials", `{"host":"*.example.org","scheme":"digest","username":"bob","password":"p"}`, http.StatusCreated, &c)
	if c.Scheme != "digest" || c.Host != "*.example.org" {
		t.Errorf("created = %+v", c)
	}
	f.call(t, http.MethodGet, "/v1/credentials", "", http.StatusOK, &list)
	if len(list.Credentials) != 2 || list.Credentials[0].Host != "*.example.org" || list.Credentials[1].Host != "files.example.com" {
		t.Fatalf("list = %+v", list)
	}
	var got domain.Credential
	f.call(t, http.MethodGet, "/v1/credentials/"+c.ID, "", http.StatusOK, &got)
	f.call(t, http.MethodPatch, "/v1/credentials/"+c.ID, `{"username":"carol","password":"new"}`, http.StatusOK, &got)
	if got.Username != "carol" || got.Host != "*.example.org" {
		t.Errorf("patched = %+v", got)
	}

	f.call(t, http.MethodPost, "/v1/credentials", `{"host":"files.example.com","username":"x","password":"y"}`, http.StatusConflict, nil)
	f.call(t, http.MethodPost, "/v1/credentials", `{"host":"https://x.example","username":"x","password":"y"}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPost, "/v1/credentials", `{"host":"x.example","scheme":"ntlm","username":"x","password":"y"}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPost, "/v1/credentials", `{"host":"x.example","username":"x"}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPatch, "/v1/credentials/"+c.ID, `{"host":"files.example.com"}`, http.StatusConflict, nil)
	f.call(t, http.MethodGet, "/v1/credentials/nope", "", http.StatusNotFound, nil)
	f.call(t, http.MethodPatch, "/v1/credentials/nope", `{"username":"x"}`, http.StatusNotFound, nil)
	f.call(t, http.MethodDelete, "/v1/credentials/nope", "", http.StatusNotFound, nil)
	f.call(t, http.MethodDelete, "/v1/credentials/"+c.ID, "", http.StatusNoContent, nil)
	f.call(t, http.MethodGet, "/v1/credentials", "", http.StatusOK, &list)
	if len(list.Credentials) != 1 {
		t.Errorf("after delete: %+v", list)
	}
}

func TestNewOperationsUnavailableWithoutServices(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/webhooks", ""},
		{http.MethodPost, "/v1/webhooks", `{"url":"http://x/","events":["task.status"]}`},
		{http.MethodPost, "/v1/webhooks/x/test", ""},
		{http.MethodGet, "/v1/credentials", ""},
		{http.MethodPost, "/v1/credentials", `{"host":"x.example","username":"u","password":"p"}`},
		{http.MethodGet, "/v1/tasks/export", ""},
		{http.MethodPost, "/v1/tasks/import", `{"format":"urls","content":"http://x/"}`},
		{http.MethodGet, "/v1/stats", ""},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := do(t, s, request{method: tc.method, path: tc.path, body: tc.body, headers: bearer(testToken)})
			assertProblem(t, w, http.StatusServiceUnavailable)
		})
	}
}

func TestImportAcceptsListsOverTheDefaultBodyLimit(t *testing.T) {
	f := newOpsFixture(t)
	// About 1.2 MB of long links (a few of them, so the test is quick): over the 1 MiB limit of other operations.
	var list strings.Builder
	for i := range 100 {
		list.WriteString(f.origin.URL + "/f" + strings.Repeat("x", 12000) + "-" + itoaTest(i) + "\n")
	}
	if list.Len() <= int(MaxBodyBytes) {
		t.Fatalf("test list is only %d bytes", list.Len())
	}
	var res batchBody
	f.call(t, http.MethodPost, "/v1/tasks/import", `{"format":"urls","paused":true,"content":`+quoteJSON(list.String())+`}`, http.StatusOK, &res)
	if len(res.Results) != 100 || !res.Results[0].OK || !res.Results[99].OK {
		t.Fatalf("%d results; first %+v", len(res.Results), res.Results[0])
	}
	// Past the limit the request is refused before it is read.
	big := `{"format":"urls","content":"` + strings.Repeat("a", int(MaxImportBodyBytes)) + `"}`
	assertProblem(t, do(t, f.s, request{method: http.MethodPost, path: "/v1/tasks/import", body: big, headers: bearer(testToken)}), http.StatusRequestEntityTooLarge)
	// Other operations keep the default limit.
	small := `{"url":"` + strings.Repeat("a", int(MaxBodyBytes)) + `"}`
	assertProblem(t, do(t, f.s, request{method: http.MethodPost, path: "/v1/tasks", body: small, headers: bearer(testToken)}), http.StatusRequestEntityTooLarge)
}

func itoaTest(i int) string { return strconv.Itoa(i) }
