package service

import (
	"context"

	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

// CookieService manages the stored cookies: listing (without values),
// deleting, and importing and exporting cookie files.
type CookieService struct {
	store *cookies.Store
}

// NewCookieService creates a CookieService over store.
func NewCookieService(store *cookies.Store) *CookieService {
	return &CookieService{store: store}
}

// CookieFilter selects cookies; empty fields match anything. Domain matches
// exactly (case-insensitively), without subdomains.
type CookieFilter struct {
	Domain string
	Name   string
}

// CookieExport is an exported cookie file.
type CookieExport struct {
	Format cookies.Format
	// Count is the number of cookies written to Data.
	Count int
	// Data is the file content (it contains cookie values: secrets).
	Data string
}

// List returns the stored cookies without their values, ordered by domain,
// path and name.
func (s *CookieService) List(ctx context.Context, f CookieFilter) ([]*domain.Cookie, error) {
	return s.store.ListMetadata(ctx, storage.CookieFilter{Domain: f.Domain, Name: f.Name}) //nolint:wrapcheck // the store already adds context
}

// Delete removes the cookie with the given ID (domain.ErrNotFound if none).
func (s *CookieService) Delete(ctx context.Context, id string) error {
	return s.store.Delete(ctx, id) //nolint:wrapcheck // the store already adds context
}

// DeleteDomain removes every cookie of exactly this domain and returns how
// many were removed. An empty domain is domain.ErrInvalidArgument.
func (s *CookieService) DeleteDomain(ctx context.Context, domainName string) (int, error) {
	return s.store.DeleteDomain(ctx, domainName) //nolint:wrapcheck // the store already adds context
}

// Import stores the cookies in data. format is "json", "netscape" or ""
// (detected from the content). Rejected cookies are reported in the result,
// not as an error; an unreadable document is domain.ErrInvalidArgument.
func (s *CookieService) Import(ctx context.Context, format, data string) (cookies.ImportResult, error) {
	f, err := parseOptionalFormat(format)
	if err != nil {
		return cookies.ImportResult{}, err
	}
	return s.store.Import(ctx, f, []byte(data)) //nolint:wrapcheck // the store already adds context
}

// Export returns the unexpired cookies matching f as a file in format
// ("json" or "netscape"; "" means json).
func (s *CookieService) Export(ctx context.Context, format string, f CookieFilter) (CookieExport, error) {
	cf, err := cookies.ParseFormat(format)
	if err != nil {
		return CookieExport{}, err //nolint:wrapcheck // a domain.ErrInvalidArgument
	}
	data, n, err := s.store.Export(ctx, cf, storage.CookieFilter{Domain: f.Domain, Name: f.Name})
	if err != nil {
		return CookieExport{}, err //nolint:wrapcheck // the store already adds context
	}
	return CookieExport{Format: cf, Count: n, Data: string(data)}, nil
}

// parseOptionalFormat is ParseFormat except that "" stays "" (detect).
func parseOptionalFormat(s string) (cookies.Format, error) {
	if s == "" {
		return "", nil
	}
	return cookies.ParseFormat(s) //nolint:wrapcheck // a domain.ErrInvalidArgument
}
