package api

import (
	"net/http"
	"testing"
)

func TestSystemOperationsAnswer503WithoutServices(t *testing.T) {
	f := newOpsFixture(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/system/info"},
		{http.MethodPost, "/v1/system/shutdown"},
		{http.MethodPost, "/v1/system/restart"},
	} {
		p := problemOf(t, f, tc.method, tc.path, "", http.StatusServiceUnavailable)
		if p.Code != "service.unavailable" {
			t.Errorf("%s %s: code %q", tc.method, tc.path, p.Code)
		}
	}
}

func TestSystemOperationsRequireTheToken(t *testing.T) {
	f := newOpsFixture(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/system/info"},
		{http.MethodPost, "/v1/system/shutdown"},
		{http.MethodPost, "/v1/system/restart"},
	} {
		w := do(t, f.s, request{method: tc.method, path: tc.path})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d", tc.method, tc.path, w.Code)
		}
	}
}

func TestIsLoopbackRemote(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:5000": true, "[::1]:5000": true, "127.0.0.1": true,
		"192.0.2.7:5000": false, "[2001:db8::1]:80": false, "": false, "garbage": false,
	} {
		if got := isLoopbackRemote(addr); got != want {
			t.Errorf("isLoopbackRemote(%q) = %v, want %v", addr, got, want)
		}
	}
}
