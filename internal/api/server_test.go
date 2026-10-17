package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/config"
)

const (
	testToken   = "test-token-0123456789"
	testOrigin  = "chrome-extension://abcdefghijklmnop"
	testVersion = "1.2.3-test"
)

type echoInput struct {
	Body struct {
		Message string `json:"message"`
	}
}

type echoOutput struct {
	Body struct {
		Message string `json:"message"`
	}
}

// newTestServer builds a server with a protected echo operation and a
// panicking operation, both test-only.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Server.AllowedOrigins = []string{testOrigin}
	s, err := New(&cfg, Deps{Version: testVersion, Token: testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	Register(s, huma.Operation{
		OperationID: "test-echo",
		Method:      http.MethodPost,
		Path:        "/v1/test/echo",
		Tags:        []string{"test"},
	}, func(_ context.Context, in *echoInput) (*echoOutput, error) {
		out := &echoOutput{}
		out.Body.Message = in.Body.Message
		return out, nil
	})
	Register(s, huma.Operation{
		OperationID: "test-panic",
		Method:      http.MethodGet,
		Path:        "/v1/test/panic",
		Tags:        []string{"test"},
	}, func(context.Context, *struct{}) (*struct{}, error) {
		panic("boom")
	})
	return s
}

type request struct {
	method  string
	path    string
	body    string
	headers map[string]string
}

func do(t *testing.T, s *Server, req request) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if req.body != "" {
		body = strings.NewReader(req.body)
	}
	r := httptest.NewRequest(req.method, req.path, body)
	if req.body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range req.headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func assertProblem(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, status, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var p struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v; body: %s", err, w.Body)
	}
	if p.Status != status || p.Title == "" {
		t.Fatalf("problem = %+v, want status %d and a title", p, status)
	}
}

func TestNewValidation(t *testing.T) {
	cfg := config.Default()
	if _, err := New(&cfg, Deps{}); err == nil {
		t.Error("New with empty token: want error")
	}
	if _, err := New(nil, Deps{Token: "x"}); err == nil {
		t.Error("New with nil config: want error")
	}
	cfg.Server.AllowedOrigins = []string{"chrome-extension://id/with/path"}
	if _, err := New(&cfg, Deps{Token: "x"}); err == nil {
		t.Error("New with invalid allowed origin: want error")
	}
}

func TestHealth(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		name    string
		headers map[string]string
		authed  bool
	}{
		{"no token", nil, false},
		{"wrong token", bearer("nope"), false},
		{"valid token", bearer(testToken), true},
		{"lower-case scheme", map[string]string{"Authorization": "bearer " + testToken}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, s, request{method: http.MethodGet, path: "/v1/health", headers: tc.headers})
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d; body: %s", w.Code, w.Body)
			}
			var got struct {
				Status        string `json:"status"`
				Version       string `json:"version"`
				Authenticated bool   `json:"authenticated"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != "ok" || got.Version != testVersion || got.Authenticated != tc.authed {
				t.Fatalf("health = %+v, want status ok, version %q, authenticated %v", got, testVersion, tc.authed)
			}
		})
	}
}

func TestProtectedRoute(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{"no token", "/v1/test/echo", nil, http.StatusUnauthorized},
		{"wrong token", "/v1/test/echo", bearer("wrong"), http.StatusUnauthorized},
		{"basic scheme", "/v1/test/echo", map[string]string{"Authorization": "Basic " + testToken}, http.StatusUnauthorized},
		{"valid token", "/v1/test/echo", bearer(testToken), http.StatusOK},
		{"unknown v1 path without token", "/v1/does-not-exist", nil, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, s, request{method: http.MethodPost, path: tc.path, body: `{"message":"hi"}`, headers: tc.headers})
			if tc.want == http.StatusUnauthorized {
				assertProblem(t, w, http.StatusUnauthorized)
				if w.Header().Get("WWW-Authenticate") == "" {
					t.Error("401 without WWW-Authenticate header")
				}
				return
			}
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tc.want, w.Body)
			}
			if !strings.Contains(w.Body.String(), `"message":"hi"`) {
				t.Fatalf("unexpected body: %s", w.Body)
			}
		})
	}
}

func TestPublicDocsRoutes(t *testing.T) {
	s := newTestServer(t)
	for _, path := range []string{"/openapi.json", "/openapi.yaml", "/docs"} {
		w := do(t, s, request{method: http.MethodGet, path: path})
		if w.Code != http.StatusOK {
			t.Errorf("GET %s without token: status = %d, want 200", path, w.Code)
		}
	}
}

func TestBodyTooLarge(t *testing.T) {
	s := newTestServer(t)
	big := `{"message":"` + strings.Repeat("a", int(MaxBodyBytes)) + `"}`
	w := do(t, s, request{method: http.MethodPost, path: "/v1/test/echo", body: big, headers: bearer(testToken)})
	assertProblem(t, w, http.StatusRequestEntityTooLarge)
}

func TestPanicRecovery(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, request{method: http.MethodGet, path: "/v1/test/panic", headers: bearer(testToken)})
	assertProblem(t, w, http.StatusInternalServerError)
}

func TestCORS(t *testing.T) {
	s := newTestServer(t)

	t.Run("allowed origin", func(t *testing.T) {
		w := do(t, s, request{method: http.MethodGet, path: "/v1/health", headers: map[string]string{"Origin": testOrigin}})
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != testOrigin {
			t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, testOrigin)
		}
	})
	t.Run("disallowed origin", func(t *testing.T) {
		w := do(t, s, request{method: http.MethodGet, path: "/v1/health", headers: map[string]string{"Origin": "https://evil.example"}})
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("Access-Control-Allow-Origin = %q for a disallowed origin", got)
		}
	})
	t.Run("no origin", func(t *testing.T) {
		w := do(t, s, request{method: http.MethodPost, path: "/v1/test/echo", body: `{"message":"cli"}`, headers: bearer(testToken)})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d; body: %s", w.Code, w.Body)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("Access-Control-Allow-Origin = %q without Origin", got)
		}
	})
	t.Run("preflight allowed", func(t *testing.T) {
		w := do(t, s, request{method: http.MethodOptions, path: "/v1/test/echo", headers: map[string]string{
			"Origin":                         testOrigin,
			"Access-Control-Request-Method":  http.MethodPost,
			"Access-Control-Request-Headers": "authorization, content-type",
		}})
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", w.Code)
		}
		h := w.Header()
		if h.Get("Access-Control-Allow-Origin") != testOrigin {
			t.Errorf("Access-Control-Allow-Origin = %q", h.Get("Access-Control-Allow-Origin"))
		}
		for _, want := range []string{"Authorization", "Content-Type", "Last-Event-ID"} {
			if !strings.Contains(h.Get("Access-Control-Allow-Headers"), want) {
				t.Errorf("Access-Control-Allow-Headers %q lacks %q", h.Get("Access-Control-Allow-Headers"), want)
			}
		}
		for _, want := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
			if !strings.Contains(h.Get("Access-Control-Allow-Methods"), want) {
				t.Errorf("Access-Control-Allow-Methods %q lacks %q", h.Get("Access-Control-Allow-Methods"), want)
			}
		}
		if h.Get("Access-Control-Max-Age") == "" {
			t.Error("missing Access-Control-Max-Age")
		}
	})
	t.Run("preflight disallowed", func(t *testing.T) {
		w := do(t, s, request{method: http.MethodOptions, path: "/v1/test/echo", headers: map[string]string{
			"Origin":                        "https://evil.example",
			"Access-Control-Request-Method": http.MethodPost,
		}})
		assertProblem(t, w, http.StatusForbidden)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("Access-Control-Allow-Origin = %q for a disallowed origin", got)
		}
	})
}

func TestCrossOriginProtection(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		name   string
		origin string
		want   int
	}{
		{"untrusted cross-site", "https://evil.example", http.StatusForbidden},
		{"trusted extension origin", testOrigin, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := bearer(testToken)
			headers["Origin"] = tc.origin
			headers["Sec-Fetch-Site"] = "cross-site"
			w := do(t, s, request{method: http.MethodPost, path: "/v1/test/echo", body: `{"message":"x"}`, headers: headers})
			if tc.want == http.StatusForbidden {
				assertProblem(t, w, http.StatusForbidden)
				return
			}
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tc.want, w.Body)
			}
		})
	}
}

func TestOpenAPIDocument(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, request{method: http.MethodGet, path: "/openapi.json"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var doc struct {
		OpenAPI    string `json:"openapi"`
		Info       struct{ Title, Version string }
		Security   []map[string][]string `json:"security"`
		Components struct {
			SecuritySchemes map[string]struct {
				Type   string `json:"type"`
				Scheme string `json:"scheme"`
			} `json:"securitySchemes"`
		} `json:"components"`
		Paths map[string]map[string]struct {
			OperationID string                 `json:"operationId"`
			Security    *[]map[string][]string `json:"security"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("openapi = %q, want 3.1.x", doc.OpenAPI)
	}
	if doc.Info.Title != Title || doc.Info.Version != testVersion {
		t.Errorf("info = %+v", doc.Info)
	}
	if sc := doc.Components.SecuritySchemes[SecurityScheme]; sc.Type != "http" || sc.Scheme != "bearer" {
		t.Errorf("security scheme %q = %+v, want http bearer", SecurityScheme, sc)
	}
	if len(doc.Security) != 1 || doc.Security[0][SecurityScheme] == nil {
		t.Errorf("document security = %v, want bearer by default", doc.Security)
	}
	health := doc.Paths["/v1/health"]["get"]
	if health.OperationID != "get-health" {
		t.Errorf("GET /v1/health operationId = %q, want get-health", health.OperationID)
	}
	if health.Security == nil || len(*health.Security) != 0 {
		t.Errorf("get-health security = %v, want explicit empty (public)", health.Security)
	}
	if echo := doc.Paths["/v1/test/echo"]["post"]; echo.Security != nil {
		t.Errorf("protected operation overrides security: %v", *echo.Security)
	}
}

func TestRegisterConventions(t *testing.T) {
	handler := func(context.Context, *struct{}) (*struct{}, error) { return nil, nil }
	for _, tc := range []struct {
		name string
		op   huma.Operation
	}{
		{"missing operationID", huma.Operation{Method: http.MethodGet, Path: "/v1/x", Tags: []string{"t"}}},
		{"camelCase operationID", huma.Operation{OperationID: "getX", Method: http.MethodGet, Path: "/v1/x", Tags: []string{"t"}}},
		{"no tags", huma.Operation{OperationID: "get-x", Method: http.MethodGet, Path: "/v1/x"}},
		{"outside v1", huma.Operation{OperationID: "get-x", Method: http.MethodGet, Path: "/x", Tags: []string{"t"}}},
		{"duplicate operationID", huma.Operation{OperationID: "get-health", Method: http.MethodGet, Path: "/v1/other", Tags: []string{"t"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			defer func() {
				if recover() == nil {
					t.Fatal("Register did not panic")
				}
			}()
			Register(s, tc.op, handler)
		})
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	s := newTestServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String()+"/v1/health", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

func TestRunListenError(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Address = "127.0.0.1:not-a-port"
	s, err := New(&cfg, Deps{Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Run(t.Context()); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("Run with bad address: err = %v, want listen error", err)
	}
}
