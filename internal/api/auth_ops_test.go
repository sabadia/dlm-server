package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

type authFixture struct {
	s         *Server
	tokenFile string
}

func newAuthFixture(t *testing.T, withRegistry, withFile bool) *authFixture {
	t.Helper()
	cfg := config.Default()
	deps := Deps{Token: testToken, Version: testVersion}
	f := &authFixture{}
	if withRegistry {
		store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "dlm.db"), sqlite.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		deps.Tokens = auth.NewRegistry(store.Tokens())
	}
	if withFile {
		f.tokenFile = filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(f.tokenFile, []byte(testToken+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		deps.TokenFile = f.tokenFile
	}
	s, err := New(&cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	return f
}

// as sends a request with the given bearer token.
func (f *authFixture) as(t *testing.T, token, method, path, body string, want int, out any) {
	t.Helper()
	w := do(t, f.s, request{method: method, path: path, body: body, headers: bearer(token)})
	if w.Code != want {
		t.Fatalf("%s %s as %.12s…: status = %d, want %d; body: %s", method, path, token, w.Code, want, w.Body)
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decode: %v; body %s", err, w.Body)
		}
	}
}

func TestWhoAmIAndNamedTokens(t *testing.T) {
	f := newAuthFixture(t, true, true)

	var me WhoAmI
	f.as(t, testToken, http.MethodGet, "/v1/auth/whoami", "", http.StatusOK, &me)
	if me.Name != "primary" || me.Scope != auth.ScopeAdmin || !me.Primary {
		t.Errorf("whoami = %+v", me)
	}
	do401 := do(t, f.s, request{method: http.MethodGet, path: "/v1/auth/whoami"})
	if do401.Code != http.StatusUnauthorized {
		t.Errorf("whoami without a token = %d", do401.Code)
	}

	var made CreatedToken
	f.as(t, testToken, http.MethodPost, "/v1/auth/tokens", `{"name":"dashboard","scope":"read"}`, http.StatusCreated, &made)
	if !strings.HasPrefix(made.Token, auth.TokenPrefix) || made.Info == nil || made.Info.Name != "dashboard" || made.Info.Scope != auth.ScopeRead {
		t.Fatalf("created = %+v", made)
	}
	var list TokenList
	f.as(t, testToken, http.MethodGet, "/v1/auth/tokens", "", http.StatusOK, &list)
	if len(list.Tokens) != 1 || list.Tokens[0].Name != "dashboard" {
		t.Errorf("list = %+v", list)
	}
	if raw := do(t, f.s, request{method: http.MethodGet, path: "/v1/auth/tokens", headers: bearer(testToken)}); strings.Contains(raw.Body.String(), made.Token) {
		t.Error("the token list contains a secret")
	}

	f.as(t, made.Token, http.MethodGet, "/v1/auth/whoami", "", http.StatusOK, &me)
	if me.Name != "dashboard" || me.Scope != auth.ScopeRead || me.Primary {
		t.Errorf("whoami as the read token = %+v", me)
	}

	// Errors.
	f.as(t, testToken, http.MethodPost, "/v1/auth/tokens", `{"name":"DASHBOARD","scope":"admin"}`, http.StatusConflict, nil)
	f.as(t, testToken, http.MethodPost, "/v1/auth/tokens", `{"name":"x","scope":"root"}`, http.StatusUnprocessableEntity, nil)
	f.as(t, testToken, http.MethodPost, "/v1/auth/tokens", `{"name":"","scope":"read"}`, http.StatusUnprocessableEntity, nil)
	f.as(t, testToken, http.MethodDelete, "/v1/auth/tokens/nope", "", http.StatusNotFound, nil)

	f.as(t, testToken, http.MethodDelete, "/v1/auth/tokens/"+made.Info.ID, "", http.StatusNoContent, nil)
	f.as(t, made.Token, http.MethodGet, "/v1/auth/whoami", "", http.StatusUnauthorized, nil)
}

func TestReadOnlyTokensCanOnlyRead(t *testing.T) {
	f := newAuthFixture(t, true, true)
	var read, admin CreatedToken
	f.as(t, testToken, http.MethodPost, "/v1/auth/tokens", `{"name":"viewer","scope":"read"}`, http.StatusCreated, &read)
	f.as(t, testToken, http.MethodPost, "/v1/auth/tokens", `{"name":"helper","scope":"admin"}`, http.StatusCreated, &admin)

	// What a read-only token may read.
	f.as(t, read.Token, http.MethodGet, "/v1/health", "", http.StatusOK, nil)
	f.as(t, read.Token, http.MethodGet, "/v1/auth/whoami", "", http.StatusOK, nil)

	// What it may not: anything but GET, and the places that hand out secrets or manage access.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/tasks/expand", `{"pattern":"http://h/a[1-2]"}`},
		{http.MethodPost, "/v1/limits/override", `{"speedLimit":0,"minutes":5}`},
		{http.MethodDelete, "/v1/tasks/x", ""},
		{http.MethodPatch, "/v1/config", `{"values":{"log.level":"debug"}}`},
		{http.MethodPost, "/v1/auth/token/rotate", ""},
		{http.MethodGet, "/v1/auth/tokens", ""},
		{http.MethodGet, "/v1/cookies/export", ""},
	} {
		w := do(t, f.s, request{method: tc.method, path: tc.path, body: tc.body, headers: bearer(read.Token)})
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"auth.read_only"`) {
			t.Errorf("%s %s as a read-only token = %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}

	// An admin named token can do what the primary can, except that it needs
	// no special care: it may create tokens and rotate.
	f.as(t, admin.Token, http.MethodGet, "/v1/auth/tokens", "", http.StatusOK, nil)
	f.as(t, admin.Token, http.MethodPost, "/v1/tasks/expand", `{"pattern":"http://h/a[1-2]"}`, http.StatusOK, nil)
}

func TestRotatingThePrimaryToken(t *testing.T) {
	f := newAuthFixture(t, true, true)
	var named CreatedToken
	f.as(t, testToken, http.MethodPost, "/v1/auth/tokens", `{"name":"keeps working","scope":"read"}`, http.StatusCreated, &named)

	var rotated CreatedToken
	f.as(t, testToken, http.MethodPost, "/v1/auth/token/rotate", "", http.StatusOK, &rotated)
	if rotated.Token == "" || rotated.Token == testToken {
		t.Fatalf("rotated = %+v", rotated)
	}
	if data, err := os.ReadFile(f.tokenFile); err != nil || strings.TrimSpace(string(data)) != rotated.Token {
		t.Errorf("token file = %q, %v", data, err)
	}
	f.as(t, testToken, http.MethodGet, "/v1/auth/whoami", "", http.StatusUnauthorized, nil)
	var me WhoAmI
	f.as(t, rotated.Token, http.MethodGet, "/v1/auth/whoami", "", http.StatusOK, &me)
	if !me.Primary {
		t.Errorf("whoami = %+v", me)
	}
	f.as(t, named.Token, http.MethodGet, "/v1/auth/whoami", "", http.StatusOK, nil)
}

func TestTokenOperationsWithoutTheirDependencies(t *testing.T) {
	bare := newAuthFixture(t, false, false)
	bare.as(t, testToken, http.MethodGet, "/v1/auth/whoami", "", http.StatusOK, nil)
	bare.as(t, testToken, http.MethodGet, "/v1/auth/tokens", "", http.StatusServiceUnavailable, nil)
	bare.as(t, testToken, http.MethodPost, "/v1/auth/tokens", `{"name":"x","scope":"read"}`, http.StatusServiceUnavailable, nil)
	bare.as(t, testToken, http.MethodPost, "/v1/auth/token/rotate", "", http.StatusServiceUnavailable, nil)
	// A named-token header with no registry is just an unknown token.
	bare.as(t, auth.TokenPrefix+"whatever", http.MethodGet, "/v1/auth/whoami", "", http.StatusUnauthorized, nil)
}
