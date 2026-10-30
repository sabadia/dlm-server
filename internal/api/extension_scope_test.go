package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

func extensionToken(t *testing.T, f *opsFixture) map[string]string {
	t.Helper()
	secret, _, err := f.s.tokens.CreateExtension(t.Context(), "Chrome extension", auth.ExtensionInfo{Browser: "chrome"})
	if err != nil {
		t.Fatal(err)
	}
	return bearer(secret)
}

// An extension token must not be able to run a program, write outside the
// download directory or reach the machine of the daemon.
func TestExtensionTokenCannotEscapeTheDownloadDirectory(t *testing.T) {
	f := newOpsFixture(t)
	h := extensionToken(t, f)
	const remote = "http://203.0.113.5/file.bin"
	cases := map[string]struct{ path, body string }{
		"capture runs a command": {"/v1/capture", `{"url":"` + remote + `","settings":{"postActions":[{"kind":"command","params":{"cmd":"/bin/sh"}}]}}`},
		"task runs a command":    {"/v1/tasks", `{"url":"` + remote + `","settings":{"postActions":[{"kind":"command","params":{"cmd":"/bin/sh"}}]}}`},
		"batch runs a command":   {"/v1/tasks/batch", `{"items":[{"url":"` + remote + `","settings":{"postActions":[{"kind":"command","params":{"cmd":"/bin/sh"}}]}}]}`},
		"capture sets a proxy":   {"/v1/capture", `{"url":"` + remote + `","settings":{"proxy":{"mode":"custom","url":"http://evil:8080"}}}`},
		"capture downloadDir":    {"/v1/capture", `{"url":"` + remote + `","settings":{"downloadDir":"/etc"}}`},
		"capture dir":            {"/v1/capture", `{"url":"` + remote + `","dir":"/Users/someone/Library/LaunchAgents"}`},
		"task dir":               {"/v1/tasks", `{"url":"` + remote + `","dir":"/etc/cron.d"}`},
		"dir with dots":          {"/v1/tasks", `{"url":"` + remote + `","dir":"` + f.dir + `/../../etc"}`},
		"loopback":               {"/v1/capture", `{"url":"http://127.0.0.1:8080/x"}`},
		"localhost":              {"/v1/tasks", `{"url":"http://localhost/x"}`},
		"metadata address":       {"/v1/capture", `{"url":"http://169.254.169.254/latest/meta-data"}`},
		"mirror on loopback":     {"/v1/capture", `{"url":"` + remote + `","mirrors":["http://[::1]/x"]}`},
		"candidate on loopback":  {"/v1/capture", `{"mediaCandidates":[{"url":"http://127.0.0.1/x.mp4"}]}`},
		"resolve loopback":       {"/v1/resolve", `{"url":"http://127.0.0.1/x"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := do(t, f.s, request{method: http.MethodPost, path: c.path, body: c.body, headers: h})
			assertProblem(t, w, http.StatusForbidden)
			if !strings.Contains(w.Body.String(), "auth.extension_scope") {
				t.Errorf("body = %s", w.Body)
			}
		})
	}
	// The same requests are fine for an admin token (it reaches the validation of the service).
	w := do(t, f.s, request{method: http.MethodPost, path: "/v1/capture", body: cases["capture dir"].body, headers: bearer(testToken)})
	if w.Code == http.StatusForbidden {
		t.Errorf("admin was refused: %s", w.Body)
	}
}

func TestExtensionTokenMayUseSafeSettingsAndTheDownloadDirectory(t *testing.T) {
	f := newOpsFixture(t)
	h := extensionToken(t, f)
	body := `{"url":"` + f.fileURL() + `","paused":true}`
	// The test origin is on loopback, which an extension token may not fetch, so only the guard is exercised.
	w := do(t, f.s, request{method: http.MethodPost, path: "/v1/capture", body: body, headers: h})
	assertProblem(t, w, http.StatusForbidden)

	if err := checkExtensionSettings(extensionSafeSettingsFromJSON(t, `{"speedLimit":1000,"maxConnections":4,"checksum":{"algorithm":"sha256","expected":"ab"},"retry":{"maxAttempts":2}}`)); err != nil {
		t.Errorf("safe settings refused: %v", err)
	}
	if err := f.s.checkExtensionDir(t.Context(), f.dir+"/sub/dir"); err != nil {
		t.Errorf("directory below the download dir refused: %v", err)
	}
}

func TestExtensionBatchIsCappedAfterExpansion(t *testing.T) {
	f := newOpsFixture(t)
	h := extensionToken(t, f)
	w := do(t, f.s, request{
		method: http.MethodPost, path: "/v1/tasks/batch", headers: h,
		body: `{"expandPatterns":true,"items":[{"url":"http://203.0.113.5/f[1-300].bin"}]}`,
	})
	assertProblem(t, w, http.StatusUnprocessableEntity)
	if !strings.Contains(w.Body.String(), "batch.too_large") {
		t.Errorf("body = %s", w.Body)
	}
}

func TestSecretsAreHiddenFromReadAndExtensionTokens(t *testing.T) {
	f := newOpsFixture(t)
	var out CaptureOutput
	f.call(t, http.MethodPost, "/v1/capture", `{"url":"`+f.fileURL()+`","paused":true,"headers":{"Authorization":"Bearer s3cret"},"method":"POST","body":"c2VjcmV0"}`, http.StatusCreated, &out)
	id := out.Task.ID
	if out.Task.Source.Headers["Authorization"] == "" {
		t.Fatalf("admin does not see the captured header: %+v", out.Task.Source)
	}
	read, _, err := f.s.tokens.Create(t.Context(), "dash", auth.ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []map[string]string{bearer(read), extensionToken(t, f)} {
		for _, path := range []string{"/v1/tasks/" + id, "/v1/tasks"} {
			w := do(t, f.s, request{method: http.MethodGet, path: path, headers: h})
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body)
			}
			if strings.Contains(w.Body.String(), "s3cret") || strings.Contains(w.Body.String(), "c2VjcmV0") {
				t.Errorf("GET %s leaks the captured secrets: %s", path, w.Body)
			}
		}
	}
	// The stored task keeps them: redaction works on a copy.
	var again domain.Task
	f.call(t, http.MethodGet, "/v1/tasks/"+id, "", http.StatusOK, &again)
	if again.Source.Headers["Authorization"] == "" {
		t.Errorf("redaction changed the stored task")
	}
}

func TestProxyCredentialsAreMaskedForReadTokens(t *testing.T) {
	f := newOpsFixture(t)
	f.call(t, http.MethodPatch, "/v1/settings", `{"proxy":{"mode":"custom","url":"http://user:pw123@proxy.test:3128"}}`, http.StatusOK, nil)
	read, _, err := f.s.tokens.Create(t.Context(), "dash", auth.ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	w := do(t, f.s, request{method: http.MethodGet, path: "/v1/settings", headers: bearer(read)})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "pw123") || !strings.Contains(w.Body.String(), "proxy.test") {
		t.Errorf("read token: %d %s", w.Code, w.Body)
	}
	w = do(t, f.s, request{method: http.MethodGet, path: "/v1/settings", headers: bearer(testToken)})
	if !strings.Contains(w.Body.String(), "pw123") {
		t.Errorf("admin should see the proxy credentials: %s", w.Body)
	}
}

func extensionSafeSettingsFromJSON(t *testing.T, raw string) (s settings.Settings) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	return s
}
