package cookies

import (
	"context"
	"fmt"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

// ImportResult summarises an import.
type ImportResult struct {
	// Imported cookies were stored (new or replacing an existing one).
	Imported int `json:"imported"`
	// Skipped cookies were valid but already expired.
	Skipped int `json:"skipped"`
	// Errors describes each rejected cookie.
	Errors []string `json:"errors"`
}

// Import decodes data (format "" detects it) and stores every acceptable
// cookie. A rejected cookie does not stop the import; it is reported in
// ImportResult.Errors. Cookies are stored as host-only or domain cookies as
// the file says; a domain cookie for a public suffix is rejected.
func (s *Store) Import(ctx context.Context, format Format, data []byte) (ImportResult, error) {
	if format == "" {
		format = DetectFormat(data)
	}
	parsed, err := Decode(format, data)
	if err != nil {
		return ImportResult{}, err
	}
	return s.ImportParsed(ctx, parsed)
}

// ImportCookies stores already decoded cookies like Import does; positions in
// the result's errors are 1-based indexes into cs.
func (s *Store) ImportCookies(ctx context.Context, cs []*domain.Cookie) (ImportResult, error) {
	parsed := make([]Parsed, len(cs))
	for i, c := range cs {
		parsed[i] = Parsed{Index: i + 1, Cookie: c}
	}
	return s.ImportParsed(ctx, parsed)
}

// ImportParsed stores the cookies of a Decode result; see Import.
func (s *Store) ImportParsed(ctx context.Context, parsed []Parsed) (ImportResult, error) {
	now := s.now()
	res := ImportResult{Errors: []string{}}
	for _, p := range parsed {
		c, err := p.Cookie, p.Err
		if err == nil {
			err = prepareImported(c)
		}
		if err != nil {
			res.Errors = append(res.Errors, describe(p, err))
			continue
		}
		if c.IsExpired(now) {
			res.Skipped++
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("cookies: import: %w", err)
		}
		if err := s.Put(ctx, c); err != nil {
			res.Errors = append(res.Errors, describe(p, err))
			continue
		}
		res.Imported++
	}
	return res, nil
}

func prepareImported(c *domain.Cookie) error {
	c.Domain = domain.CanonicalDomain(c.Domain)
	if err := c.Validate(); err != nil {
		return err
	}
	if !c.HostOnly && domain.IsPublicSuffix(c.Domain) {
		return domain.InvalidArgument("cookie: domain %q is a public suffix", c.Domain)
	}
	return nil
}

func describe(p Parsed, err error) string {
	name := ""
	if p.Cookie != nil {
		name = " (" + p.Cookie.Name + ")"
	}
	return fmt.Sprintf("#%d%s: %v", p.Index, name, err)
}

// Export returns the unexpired cookies matching filter, values included, in
// the given format, and how many were written (cookies that a format cannot
// represent are not counted; see EncodeNetscape).
func (s *Store) Export(ctx context.Context, format Format, filter storage.CookieFilter) (data []byte, count int, err error) {
	all, err := s.List(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	now := s.now()
	live := all[:0]
	for _, c := range all {
		if !c.IsExpired(now) {
			live = append(live, c)
		}
	}
	data, err = Encode(format, live)
	if err != nil {
		return nil, 0, err
	}
	return data, len(live), nil
}
