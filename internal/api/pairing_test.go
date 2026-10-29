package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

type pairingFixture struct {
	s      *Server
	events []auth.PairingEvent
}

func newPairingFixture(t *testing.T) *pairingFixture {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "dlm.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reg := auth.NewRegistry(store.Tokens())
	f := &pairingFixture{}
	cfg := config.Default()
	s, err := New(&cfg, Deps{
		Token: testToken, Version: testVersion, Tokens: reg,
		Pairings: auth.NewPairings(reg, func(e auth.PairingEvent) { f.events = append(f.events, e) }),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	return f
}

// call sends a request as token from the machine the daemon runs on (local) or from elsewhere.
func (f *pairingFixture) call(t *testing.T, local bool, token, method, path, body string, want int, out any) {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if local {
		r.RemoteAddr = net.JoinHostPort("127.0.0.1", "4321")
	}
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: status = %d, want %d; body: %s", method, path, w.Code, want, w.Body)
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decode: %v; body %s", err, w.Body)
		}
	}
}

func TestPairingFlowOverHTTP(t *testing.T) {
	f := newPairingFixture(t)

	var started auth.Pairing
	f.call(t, true, testToken, http.MethodPost, "/v1/pairing", `{"browser":"chrome","clientId":"abc","version":"0.1.0"}`, http.StatusCreated, &started)
	if started.Status != auth.PairingPending || len(started.Code) != 6 {
		t.Fatalf("started = %+v", started)
	}
	if len(f.events) != 1 || !f.events[0].Requested {
		t.Fatalf("events = %+v", f.events)
	}

	var list PairingList
	f.call(t, true, testToken, http.MethodGet, "/v1/pairing", "", http.StatusOK, &list)
	if len(list.Pairings) != 1 || list.Pairings[0].ID != started.ID {
		t.Fatalf("list = %+v", list)
	}

	var st PairingState
	f.call(t, true, testToken, http.MethodGet, "/v1/pairing/"+started.ID, "", http.StatusOK, &st)
	if st.Status != auth.PairingPending || st.Token != "" {
		t.Fatalf("pending read = %+v", st)
	}
	f.call(t, true, testToken, http.MethodPost, "/v1/pairing/"+started.ID+"/approve", "", http.StatusOK, nil)
	f.call(t, true, testToken, http.MethodGet, "/v1/pairing/"+started.ID, "", http.StatusOK, &st)
	if st.Status != auth.PairingApproved || !strings.HasPrefix(st.Token, auth.TokenPrefix) {
		t.Fatalf("approved read = %+v", st)
	}
	token := st.Token
	var again PairingState
	f.call(t, true, testToken, http.MethodGet, "/v1/pairing/"+started.ID, "", http.StatusOK, &again)
	if again.Token != "" {
		t.Error("the token was returned twice")
	}

	// The extension token knows who it is, and shows up in the token list with what it is.
	var me WhoAmI
	f.call(t, true, token, http.MethodGet, "/v1/auth/whoami", "", http.StatusOK, &me)
	if me.Scope != auth.ScopeExtension || me.Primary {
		t.Fatalf("whoami = %+v", me)
	}
	var tokens TokenList
	f.call(t, true, testToken, http.MethodGet, "/v1/auth/tokens", "", http.StatusOK, &tokens)
	if len(tokens.Tokens) != 1 || tokens.Tokens[0].Kind != "extension" || tokens.Tokens[0].Browser != "chrome" || tokens.Tokens[0].ClientVersion != "0.1.0" {
		t.Fatalf("tokens = %+v", tokens)
	}

	// Revoking the token ends the extension's access at once.
	f.call(t, true, testToken, http.MethodDelete, "/v1/auth/tokens/"+tokens.Tokens[0].ID, "", http.StatusNoContent, nil)
	f.call(t, true, token, http.MethodGet, "/v1/auth/whoami", "", http.StatusUnauthorized, nil)
}

func TestPairingDenyAndErrors(t *testing.T) {
	f := newPairingFixture(t)
	var p auth.Pairing
	f.call(t, true, testToken, http.MethodPost, "/v1/pairing", `{"browser":"firefox"}`, http.StatusCreated, &p)
	f.call(t, true, testToken, http.MethodPost, "/v1/pairing/"+p.ID+"/deny", "", http.StatusOK, nil)
	f.call(t, true, testToken, http.MethodPost, "/v1/pairing/"+p.ID+"/approve", "", http.StatusConflict, nil)
	f.call(t, true, testToken, http.MethodGet, "/v1/pairing/nope", "", http.StatusNotFound, nil)
	f.call(t, true, testToken, http.MethodPost, "/v1/pairing", `{"browser":"netscape"}`, http.StatusUnprocessableEntity, nil)
	for range auth.MaxPendingPairings {
		f.call(t, true, testToken, http.MethodPost, "/v1/pairing", `{"browser":"chrome"}`, http.StatusCreated, nil)
	}
	f.call(t, true, testToken, http.MethodPost, "/v1/pairing", `{"browser":"chrome"}`, http.StatusConflict, nil)
}

func TestPairingIsLocalAndAdminOnly(t *testing.T) {
	f := newPairingFixture(t)
	f.call(t, false, testToken, http.MethodGet, "/v1/pairing", "", http.StatusForbidden, nil)
	f.call(t, false, testToken, http.MethodPost, "/v1/pairing", `{"browser":"chrome"}`, http.StatusForbidden, nil)

	read, _, err := f.s.tokens.Create(t.Context(), "dash", auth.ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	f.call(t, true, read, http.MethodGet, "/v1/pairing", "", http.StatusForbidden, nil)
	f.call(t, true, "", http.MethodGet, "/v1/pairing", "", http.StatusUnauthorized, nil)
}

func TestExtensionScope(t *testing.T) {
	f := newPairingFixture(t)
	secret, _, err := f.s.tokens.CreateExtension(t.Context(), "Chrome extension", auth.ExtensionInfo{Browser: "chrome"})
	if err != nil {
		t.Fatal(err)
	}
	denied := []struct{ method, path string }{
		{http.MethodGet, "/v1/settings"},
		{http.MethodPut, "/v1/settings"},
		{http.MethodPatch, "/v1/settings"},
		{http.MethodDelete, "/v1/tasks/x"},
		{http.MethodPost, "/v1/tasks/delete"},
		{http.MethodPost, "/v1/tasks/x/restart"},
		{http.MethodPatch, "/v1/tasks/x/source"},
		{http.MethodGet, "/v1/cookies"},
		{http.MethodGet, "/v1/cookies/export"},
		{http.MethodDelete, "/v1/cookies"},
		{http.MethodGet, "/v1/auth/tokens"},
		{http.MethodPost, "/v1/auth/tokens"},
		{http.MethodPost, "/v1/auth/token/rotate"},
		{http.MethodGet, "/v1/pairing"},
		{http.MethodPost, "/v1/pairing"},
		{http.MethodGet, "/v1/native-host"},
		{http.MethodPost, "/v1/native-host"},
		{http.MethodPost, "/v1/system/shutdown"},
		{http.MethodGet, "/v1/credentials"},
		{http.MethodGet, "/v1/webhooks"},
		{http.MethodGet, "/v1/plugins"},
		{http.MethodGet, "/v1/config"},
		{http.MethodGet, "/v1/logs"},
		{http.MethodPost, "/mcp"},
	}
	for _, c := range denied {
		w := do(t, f.s, request{method: c.method, path: c.path, headers: bearer(secret)})
		assertProblem(t, w, http.StatusForbidden)
		if !strings.Contains(w.Body.String(), "auth.extension_scope") {
			t.Errorf("%s %s: body %s", c.method, c.path, w.Body)
		}
	}
	// What the extension is for is not refused by the scope (the services are not wired here, so 503 is the answer).
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/capture"},
		{http.MethodPost, "/v1/tasks"},
		{http.MethodPost, "/v1/tasks/batch"},
		{http.MethodGet, "/v1/tasks"},
		{http.MethodGet, "/v1/tasks/abc"},
		{http.MethodPost, "/v1/tasks/pause"},
		{http.MethodPost, "/v1/tasks/abc/refresh/await"},
		{http.MethodPost, "/v1/resolve"},
		{http.MethodPost, "/v1/browser/resolve"},
		{http.MethodPost, "/v1/browser/results/r1/candidates/2/download"},
		{http.MethodPost, "/v1/cookies/import"},
		{http.MethodGet, "/v1/queues"},
		{http.MethodGet, "/v1/limits"},
		{http.MethodGet, "/v1/auth/whoami"},
	} {
		w := do(t, f.s, request{method: c.method, path: c.path, body: "{}", headers: bearer(secret)})
		if w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "auth.extension_scope") {
			t.Errorf("%s %s was refused by the extension scope", c.method, c.path)
		}
	}
}

func TestPathMatches(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"/v1/tasks", "/v1/tasks", true},
		{"/v1/tasks/{id}", "/v1/tasks/abc", true},
		{"/v1/tasks/{id}", "/v1/tasks/", false},
		{"/v1/tasks/{id}", "/v1/tasks/abc/segments", false},
		{"/v1/tasks/{id}/refresh", "/v1/tasks/abc/refresh", true},
		{"/v1/tasks/{id}/refresh", "/v1/tasks/abc/other", false},
		{"/v1/tasks", "/v1/tasks/", false},
	} {
		if got := pathMatches(tc.pattern, tc.path); got != tc.want {
			t.Errorf("pathMatches(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}
