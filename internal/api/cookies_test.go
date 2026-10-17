package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type cookieListBody struct {
	Cookies []map[string]any `json:"cookies"`
}

const importJSON = `[
  {"domain": "example.com", "name": "sid", "value": "top-secret", "path": "/", "secure": true, "httpOnly": true, "hostOnly": true},
  {"domain": ".example.org", "name": "pref", "value": "dark", "path": "/", "sameSite": "lax"}
]`

func importBody(t *testing.T, format, data string) string {
	t.Helper()
	m := map[string]string{"data": data}
	if format != "" {
		m["format"] = format
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCookieOperations(t *testing.T) {
	f := newOpsFixture(t)

	var res ImportCookiesResult
	f.call(t, http.MethodPost, "/v1/cookies/import", importBody(t, "", importJSON), http.StatusOK, &res)
	if res.Imported != 2 || res.Skipped != 0 || len(res.Errors) != 0 {
		t.Fatalf("import = %+v, want 2 imported", res)
	}

	// Listing never contains values.
	var list cookieListBody
	f.call(t, http.MethodGet, "/v1/cookies", "", http.StatusOK, &list)
	if len(list.Cookies) != 2 {
		t.Fatalf("list = %v, want 2 cookies", list.Cookies)
	}
	for _, c := range list.Cookies {
		if _, has := c["value"]; has {
			t.Errorf("list leaks a value: %v", c)
		}
	}
	w := do(t, f.s, request{method: http.MethodGet, path: "/v1/cookies", headers: bearer(testToken)})
	if strings.Contains(w.Body.String(), "top-secret") {
		t.Error("list body contains a cookie value")
	}

	var filtered cookieListBody
	f.call(t, http.MethodGet, "/v1/cookies?domain=EXAMPLE.org&name=pref", "", http.StatusOK, &filtered)
	if len(filtered.Cookies) != 1 || filtered.Cookies[0]["name"] != "pref" {
		t.Errorf("filtered list = %v", filtered.Cookies)
	}

	// Export carries the values, in either format.
	var exp ExportCookiesResult
	f.call(t, http.MethodGet, "/v1/cookies/export?domain=example.com", "", http.StatusOK, &exp)
	if exp.Format != "json" || exp.Count != 1 || !strings.Contains(exp.Data, "top-secret") {
		t.Errorf("json export = %+v", exp)
	}
	f.call(t, http.MethodGet, "/v1/cookies/export?format=netscape", "", http.StatusOK, &exp)
	if exp.Format != "netscape" || exp.Count != 2 ||
		!strings.Contains(exp.Data, "#HttpOnly_example.com\tFALSE\t/\tTRUE\t0\tsid\ttop-secret\n") ||
		!strings.Contains(exp.Data, ".example.org\tTRUE\t/\tFALSE\t0\tpref\tdark\n") {
		t.Errorf("netscape export = %+v", exp)
	}

	// Delete one by ID, the rest by domain.
	id, _ := list.Cookies[0]["id"].(string)
	f.call(t, http.MethodDelete, "/v1/cookies/"+url.PathEscape(id), "", http.StatusNoContent, nil)
	assertProblem(t, do(t, f.s, request{method: http.MethodDelete, path: "/v1/cookies/" + id, headers: bearer(testToken)}), http.StatusNotFound)
	var del DeleteCookiesResult
	f.call(t, http.MethodDelete, "/v1/cookies?domain=example.org", "", http.StatusOK, &del)
	if del.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", del.Deleted)
	}
	f.call(t, http.MethodGet, "/v1/cookies", "", http.StatusOK, &list)
	if len(list.Cookies) != 0 {
		t.Errorf("list after deletes = %v", list.Cookies)
	}
}

func TestCookieImportNetscapeAndProblems(t *testing.T) {
	f := newOpsFixture(t)
	data := "example.com\tTRUE\t/\tFALSE\t0\ta\t1\ncom\tTRUE\t/\tFALSE\t0\ttld\t1\n"
	var res ImportCookiesResult
	f.call(t, http.MethodPost, "/v1/cookies/import", importBody(t, "netscape", data), http.StatusOK, &res)
	if res.Imported != 1 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "public suffix") {
		t.Errorf("import = %+v, want 1 imported and a public suffix rejection", res)
	}
	// An unreadable document and an unknown format are 422.
	for name, body := range map[string]string{
		"not json":       importBody(t, "json", "nope"),
		"unknown format": `{"format":"xml","data":"x"}`,
		"empty data":     `{"data":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			assertProblem(t, do(t, f.s, request{method: http.MethodPost, path: "/v1/cookies/import", body: body, headers: bearer(testToken)}), http.StatusUnprocessableEntity)
		})
	}
	for _, path := range []string{"/v1/cookies?domain=", "/v1/cookies", "/v1/cookies/export?format=xml"} {
		method := http.MethodDelete
		if strings.Contains(path, "export") {
			method = http.MethodGet
		}
		assertProblem(t, do(t, f.s, request{method: method, path: path, headers: bearer(testToken)}), http.StatusUnprocessableEntity)
	}
}

func TestCookieOperationsRequireToken(t *testing.T) {
	f := newOpsFixture(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/cookies"},
		{http.MethodGet, "/v1/cookies/export"},
		{http.MethodDelete, "/v1/cookies?domain=example.com"},
	} {
		assertProblem(t, do(t, f.s, request{method: tc.method, path: tc.path}), http.StatusUnauthorized)
	}
}
