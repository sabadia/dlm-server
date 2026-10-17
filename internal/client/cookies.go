package client

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Cookie mirrors a stored cookie as listed by the daemon (no value).
type Cookie struct {
	ID           string     `json:"id"`
	Domain       string     `json:"domain"`
	Name         string     `json:"name"`
	Path         string     `json:"path"`
	PartitionKey string     `json:"partitionKey,omitempty"`
	Secure       bool       `json:"secure"`
	HTTPOnly     bool       `json:"httpOnly"`
	SameSite     string     `json:"sameSite,omitempty"`
	HostOnly     bool       `json:"hostOnly"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// CookieFilter selects cookies; empty fields match anything.
type CookieFilter struct {
	// Domain matches exactly, without subdomains.
	Domain string
	Name   string
}

func (f CookieFilter) query(extra url.Values) string {
	q := extra
	if q == nil {
		q = url.Values{}
	}
	if f.Domain != "" {
		q.Set("domain", f.Domain)
	}
	if f.Name != "" {
		q.Set("name", f.Name)
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// ImportCookiesResult is the outcome of ImportCookies.
type ImportCookiesResult struct {
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors"`
}

// ExportedCookies is a cookie file returned by ExportCookies.
type ExportedCookies struct {
	Format string `json:"format"`
	Count  int    `json:"count"`
	Data   string `json:"data"`
}

// ListCookies calls list-cookies (GET /v1/cookies).
func (c *Client) ListCookies(ctx context.Context, f CookieFilter) ([]Cookie, error) {
	var out struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/cookies"+f.query(nil), nil, &out); err != nil {
		return nil, err
	}
	return out.Cookies, nil
}

// DeleteCookies calls delete-cookies (DELETE /v1/cookies?domain=) and returns
// how many cookies of exactly that domain were deleted.
func (c *Client) DeleteCookies(ctx context.Context, domain string) (int, error) {
	var out struct {
		Deleted int `json:"deleted"`
	}
	path := "/v1/cookies?" + url.Values{"domain": {domain}}.Encode()
	if err := c.do(ctx, http.MethodDelete, path, nil, &out); err != nil {
		return 0, err
	}
	return out.Deleted, nil
}

// DeleteCookie calls delete-cookie (DELETE /v1/cookies/{id}).
func (c *Client) DeleteCookie(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/cookies/"+url.PathEscape(id), nil, nil)
}

// ImportCookies calls import-cookies (POST /v1/cookies/import). format is
// "json", "netscape" or "" to let the daemon detect it.
func (c *Client) ImportCookies(ctx context.Context, format, data string) (*ImportCookiesResult, error) {
	body := struct {
		Format string `json:"format,omitempty"`
		Data   string `json:"data"`
	}{Format: format, Data: data}
	var out ImportCookiesResult
	if err := c.do(ctx, http.MethodPost, "/v1/cookies/import", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExportCookies calls export-cookies (GET /v1/cookies/export). format is
// "json" or "netscape" ("" means json).
func (c *Client) ExportCookies(ctx context.Context, format string, f CookieFilter) (*ExportedCookies, error) {
	extra := url.Values{}
	if format != "" {
		extra.Set("format", format)
	}
	var out ExportedCookies
	if err := c.do(ctx, http.MethodGet, "/v1/cookies/export"+f.query(extra), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
