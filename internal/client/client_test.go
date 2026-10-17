package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/config"
)

func TestHealthSuccessSendsAuthAndUserAgent(t *testing.T) {
	var gotAuth, gotUA, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotUA, gotPath = r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","version":"1.2.3","authenticated":true}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL+"/", "secret", WithUserAgent("dlm-cli/1.2.3"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if diff := cmp.Diff(&Health{Status: "ok", Version: "1.2.3", Authenticated: true}, h); diff != "" {
		t.Errorf("Health mismatch (-want +got):\n%s", diff)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret")
	}
	if gotUA != "dlm-cli/1.2.3" {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if gotPath != "/v1/health" {
		t.Errorf("path = %q, want /v1/health", gotPath)
	}
}

func TestNoTokenSendsNoAuthorization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v, ok := r.Header["Authorization"]; ok {
			t.Errorf("Authorization header sent without token: %q", v)
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProblemErrors(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		want        *APIError
		notFound    bool
		unauth      bool
	}{
		{
			name:        "unauthorized",
			status:      http.StatusUnauthorized,
			contentType: "application/problem+json",
			body:        `{"title":"Unauthorized","status":401,"detail":"missing or invalid bearer token"}`,
			want:        &APIError{Status: 401, Title: "Unauthorized", Detail: "missing or invalid bearer token"},
			unauth:      true,
		},
		{
			name:        "not found with details",
			status:      http.StatusNotFound,
			contentType: "application/problem+json; charset=utf-8",
			body:        `{"title":"Not Found","status":404,"detail":"no task","errors":[{"message":"unknown id","location":"path.id","value":"x"}]}`,
			want: &APIError{
				Status: 404, Title: "Not Found", Detail: "no task",
				Errors: []ErrorDetail{{Message: "unknown id", Location: "path.id", Value: "x"}},
			},
			notFound: true,
		},
		{
			name:        "non-json body falls back to status text",
			status:      http.StatusBadGateway,
			contentType: "text/plain",
			body:        "upstream down",
			want:        &APIError{Status: 502, Title: "Bad Gateway"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			c, err := New(srv.URL, "tok")
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Health(context.Background())
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error %v (%T) is not *APIError", err, err)
			}
			if diff := cmp.Diff(tt.want, apiErr); diff != "" {
				t.Errorf("APIError mismatch (-want +got):\n%s", diff)
			}
			if IsNotFound(err) != tt.notFound || IsUnauthorized(err) != tt.unauth {
				t.Errorf("IsNotFound=%v IsUnauthorized=%v", IsNotFound(err), IsUnauthorized(err))
			}
		})
	}
}

func TestAPIErrorMessage(t *testing.T) {
	if got := (&APIError{Title: "Unauthorized", Detail: "bad token"}).Error(); got != "Unauthorized: bad token" {
		t.Errorf("Error() = %q", got)
	}
	if got := (&APIError{Title: "Not Found"}).Error(); got != "Not Found" {
		t.Errorf("Error() = %q", got)
	}
}

func TestConnectionRefused(t *testing.T) {
	// Reserve a port, then close it so nothing listens there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	base := "http://" + addr
	c, err := New(base, "tok")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Health(context.Background())
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("error = %v, want ErrDaemonUnreachable", err)
	}
	for _, want := range []string{"dlm daemon is not running at " + base, "dlm serve"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestNewRejectsInvalidBaseURL(t *testing.T) {
	for _, u := range []string{"", "127.0.0.1:17171", "ftp://host", "http://", "://bad"} {
		if _, err := New(u, ""); err == nil {
			t.Errorf("New(%q) succeeded, want error", u)
		}
	}
}

func TestBaseURL(t *testing.T) {
	tests := []struct {
		address string
		tls     bool
		want    string
	}{
		{"127.0.0.1:17171", false, "http://127.0.0.1:17171"},
		{"localhost:80", false, "http://localhost:80"},
		{"0.0.0.0:9000", true, "https://127.0.0.1:9000"},
		{":9000", false, "http://127.0.0.1:9000"},
		{"[::]:9000", false, "http://[::1]:9000"},
		{"[::1]:9000", false, "http://[::1]:9000"},
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			cfg := config.Default()
			cfg.Server.Address = tt.address
			if tt.tls {
				cfg.Server.TLSCert, cfg.Server.TLSKey = "cert.pem", "key.pem"
			}
			got, err := BaseURL(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("BaseURL = %q, want %q", got, tt.want)
			}
		})
	}
	cfg := config.Default()
	cfg.Server.Address = "no-port"
	if _, err := BaseURL(&cfg); err == nil {
		t.Error("BaseURL accepted an address without port")
	}
}
