package domain

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestMatchesDomain(t *testing.T) {
	tests := []struct {
		name         string
		cookieDomain string
		host         string
		hostOnly     bool
		want         bool
	}{
		{"exact match host-only", "example.com", "example.com", true, true},
		{"subdomain host-only rejected", "example.com", "sub.example.com", true, false},
		{"exact match non-host-only", "example.com", "example.com", false, true},
		{"subdomain non-host-only accepted", "example.com", "sub.example.com", false, true},
		{"nested subdomain non-host-only", "example.com", "a.b.sub.example.com", false, true},
		{"prefix match rejected", "example.com", "notexample.com", false, false},
		{"leading dot in cookie domain handled", ".example.com", "sub.example.com", false, true},
		{"case insensitive", "Example.COM", "sub.example.com", false, true},
		{"ip address exact match", "127.0.0.1", "127.0.0.1", false, true},
		{"ip address no subdomain matching", "127.0.0.1", "sub.127.0.0.1", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MatchesDomain(tt.cookieDomain, tt.host, tt.hostOnly); got != tt.want {
				t.Errorf("MatchesDomain(%q, %q, %v) = %v, want %v", tt.cookieDomain, tt.host, tt.hostOnly, got, tt.want)
			}
		})
	}
}

func TestPathMatch(t *testing.T) {
	tests := []struct {
		name       string
		cookiePath string
		reqPath    string
		want       bool
	}{
		{"root matches root", "/", "/", true},
		{"root matches subpath", "/", "/any/path", true},
		{"empty cookie path defaults to root", "", "/sub", true},
		{"exact subpath", "/docs", "/docs", true},
		{"subpath with trailing slash in cookie", "/docs/", "/docs/guide", true},
		{"subpath without trailing slash in cookie matches child", "/docs", "/docs/guide", true},
		{"prefix but not directory boundary rejected", "/doc", "/document", false},
		{"unrelated path rejected", "/docs", "/api/v1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PathMatch(tt.cookiePath, tt.reqPath); got != tt.want {
				t.Errorf("PathMatch(%q, %q) = %v, want %v", tt.cookiePath, tt.reqPath, got, tt.want)
			}
		})
	}
}

func TestDefaultPath(t *testing.T) {
	tests := []struct {
		reqPath string
		want    string
	}{
		{"", "/"},
		{"abc", "/"},
		{"/", "/"},
		{"/file.txt", "/"},
		{"/a/b/c", "/a/b"},
		{"/a/b/", "/a/b"},
	}
	for _, tt := range tests {
		if got := DefaultPath(tt.reqPath); got != tt.want {
			t.Errorf("DefaultPath(%q) = %q, want %q", tt.reqPath, got, tt.want)
		}
	}
}

func TestIsPublicSuffix(t *testing.T) {
	if !IsPublicSuffix("com") {
		t.Error("expected 'com' to be a public suffix")
	}
	if !IsPublicSuffix("co.uk") {
		t.Error("expected 'co.uk' to be a public suffix")
	}
	if IsPublicSuffix("example.com") {
		t.Error("expected 'example.com' to not be a public suffix")
	}
}

func TestCookieValidation(t *testing.T) {
	now := time.Now()
	c, err := NewCookie(NewID(), "session", "val123", "example.com", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Path != "/" {
		t.Errorf("path = %q, want /", c.Path)
	}

	// Invalid name
	if _, err := NewCookie(NewID(), "bad;name", "val", "example.com", now); err == nil {
		t.Error("expected error for semicolon in cookie name")
	}
	// Empty name
	if _, err := NewCookie(NewID(), "", "val", "example.com", now); err == nil {
		t.Error("expected error for empty cookie name")
	}
	// Invalid domain
	if _, err := NewCookie(NewID(), "name", "val", "example.com/bad", now); err == nil {
		t.Error("expected error for slash in domain")
	}
}

func TestFromHTTPCookie(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	u, _ := url.Parse("https://sub.example.com/api/v1/download")

	// Case 1: Host-only cookie (no domain specified)
	hc1 := &http.Cookie{
		Name:  "token",
		Value: "xyz",
	}
	c1, err := FromHTTPCookie(hc1, u, "", now)
	if err != nil {
		t.Fatalf("FromHTTPCookie error: %v", err)
	}
	if !c1.HostOnly {
		t.Error("expected HostOnly = true")
	}
	if c1.Domain != "sub.example.com" {
		t.Errorf("domain = %q, want sub.example.com", c1.Domain)
	}
	if c1.Path != "/api/v1" {
		t.Errorf("default path = %q, want /api/v1", c1.Path)
	}

	// Case 2: Explicit domain matching parent domain
	hc2 := &http.Cookie{
		Name:     "sess",
		Value:    "123",
		Domain:   ".example.com",
		Path:     "/",
		MaxAge:   3600,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	c2, err := FromHTTPCookie(hc2, u, "part1", now)
	if err != nil {
		t.Fatalf("FromHTTPCookie error: %v", err)
	}
	if c2.HostOnly {
		t.Error("expected HostOnly = false")
	}
	if c2.Domain != "example.com" {
		t.Errorf("domain = %q, want example.com", c2.Domain)
	}
	if c2.ExpiresAt == nil || !c2.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("expiresAt = %v, want %v", c2.ExpiresAt, now.Add(time.Hour))
	}
	if c2.SameSite != "lax" {
		t.Errorf("sameSite = %q, want lax", c2.SameSite)
	}

	// Case 3: Public suffix domain rejected
	hc3 := &http.Cookie{
		Name:   "cross",
		Value:  "evil",
		Domain: "com",
	}
	if _, err := FromHTTPCookie(hc3, u, "", now); err == nil {
		t.Error("expected error for public suffix domain")
	}

	// Case 4: Non-matching domain rejected
	hc4 := &http.Cookie{
		Name:   "cross",
		Value:  "evil",
		Domain: "other.com",
	}
	if _, err := FromHTTPCookie(hc4, u, "", now); err == nil {
		t.Error("expected error for non-matching domain")
	}
}

func TestCookieMatchesRequest(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	uHTTPS, _ := url.Parse("https://app.example.com/files/123")
	uHTTP, _ := url.Parse("http://app.example.com/files/123")
	uOther, _ := url.Parse("https://other.com/files/123")

	expFuture := now.Add(time.Hour)
	expPast := now.Add(-time.Hour)

	c := &Cookie{
		Domain:    "example.com",
		Name:      "auth",
		Value:     "pass",
		Path:      "/files",
		Secure:    true,
		HostOnly:  false,
		ExpiresAt: &expFuture,
	}

	if !c.MatchesRequest(uHTTPS, "", now) {
		t.Error("expected cookie to match HTTPS request")
	}
	if c.MatchesRequest(uHTTP, "", now) {
		t.Error("expected secure cookie to NOT match HTTP request")
	}
	if c.MatchesRequest(uOther, "", now) {
		t.Error("expected cookie to NOT match different domain")
	}

	// Expired cookie
	cExpired := *c
	cExpired.ExpiresAt = &expPast
	if cExpired.MatchesRequest(uHTTPS, "", now) {
		t.Error("expected expired cookie to NOT match")
	}

	// Partition key mismatch
	cPart := *c
	cPart.PartitionKey = "siteA"
	if cPart.MatchesRequest(uHTTPS, "siteB", now) {
		t.Error("expected partition mismatch to NOT match")
	}
	if !cPart.MatchesRequest(uHTTPS, "siteA", now) {
		t.Error("expected matching partition to match")
	}
}
