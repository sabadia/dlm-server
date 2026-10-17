package cookies

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

// jarTimeout bounds one jar operation: http.CookieJar methods carry no
// context.
const jarTimeout = 5 * time.Second

// Options configures a Store. Zero values select the defaults.
type Options struct {
	// Clock defaults to time.Now.
	Clock func() time.Time
	// Logger defaults to a discarding logger. Cookie values are never logged.
	Logger *slog.Logger
}

// Store keeps cookies in a storage.CookieRepository with the values
// encrypted at rest. It implements http.CookieJar (RFC 6265, public-suffix
// aware) for the download engines, and offers management and import/export
// to the services. It is safe for concurrent use.
type Store struct {
	repo   storage.CookieRepository
	cipher *Cipher
	now    func() time.Time
	log    *slog.Logger
}

var _ http.CookieJar = (*Store)(nil)

// NewStore returns a Store persisting to repo and encrypting with c.
func NewStore(repo storage.CookieRepository, c *Cipher, opts Options) *Store {
	s := &Store{repo: repo, cipher: c, now: opts.Clock, log: opts.Logger}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s
}

// aad binds a ciphertext to the identity of its cookie.
func aad(domainName, name, path, partitionKey string) []byte {
	return []byte(strings.Join([]string{domainName, name, path, partitionKey}, "\x00"))
}

// Put stores c, replacing the cookie with the same domain, name, path and
// partition key (which keeps that cookie's ID and creation time). A missing
// ID is generated and zero timestamps are set to the current time.
func (s *Store) Put(ctx context.Context, c *domain.Cookie) error {
	now := s.now().UTC().Truncate(time.Millisecond)
	cc := *c
	cc.Domain = domain.CanonicalDomain(cc.Domain)
	if err := cc.Validate(); err != nil {
		return err
	}
	if cc.ID == "" {
		cc.ID = domain.NewID()
	}
	if cc.CreatedAt.IsZero() {
		cc.CreatedAt = now
	}
	if cc.UpdatedAt.IsZero() {
		cc.UpdatedAt = now
	}
	sealed, err := s.cipher.Seal([]byte(cc.Value), aad(cc.Domain, cc.Name, cc.Path, cc.PartitionKey))
	if err != nil {
		return err
	}
	rec := &storage.CookieRecord{
		ID:           cc.ID,
		Domain:       cc.Domain,
		Name:         cc.Name,
		Path:         cc.Path,
		PartitionKey: cc.PartitionKey,
		EncryptedVal: sealed,
		Secure:       cc.Secure,
		HTTPOnly:     cc.HTTPOnly,
		SameSite:     strings.ToLower(cc.SameSite),
		HostOnly:     cc.HostOnly,
		ExpiresAt:    cc.ExpiresAt,
		CreatedAt:    cc.CreatedAt,
		UpdatedAt:    cc.UpdatedAt,
	}
	if err := s.repo.Upsert(ctx, rec); err != nil {
		return fmt.Errorf("cookies: store cookie %q: %w", cc.Name, err)
	}
	return nil
}

// List returns the cookies matching filter with their values decrypted,
// ordered by domain, path and name. An empty filter field matches anything.
// A value that cannot be decrypted fails the call (ErrDecrypt).
func (s *Store) List(ctx context.Context, filter storage.CookieFilter) ([]*domain.Cookie, error) {
	recs, err := s.repo.List(ctx, canonicalFilter(filter))
	if err != nil {
		return nil, fmt.Errorf("cookies: list: %w", err)
	}
	out := make([]*domain.Cookie, 0, len(recs))
	for _, r := range recs {
		c, err := s.decrypt(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// ListMetadata is List without decrypting: Value is empty.
func (s *Store) ListMetadata(ctx context.Context, filter storage.CookieFilter) ([]*domain.Cookie, error) {
	recs, err := s.repo.List(ctx, canonicalFilter(filter))
	if err != nil {
		return nil, fmt.Errorf("cookies: list: %w", err)
	}
	out := make([]*domain.Cookie, len(recs))
	for i, r := range recs {
		out[i] = metadata(r)
	}
	return out, nil
}

// Delete removes the cookie with the given ID (domain.ErrNotFound if none).
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("cookies: delete: %w", err)
	}
	return nil
}

// DeleteDomain removes every cookie stored for exactly this domain and
// returns how many were removed.
func (s *Store) DeleteDomain(ctx context.Context, domainName string) (int, error) {
	domainName = domain.CanonicalDomain(domainName)
	if domainName == "" {
		return 0, domain.InvalidArgument("cookies: domain must not be empty")
	}
	recs, err := s.repo.List(ctx, storage.CookieFilter{Domain: domainName})
	if err != nil {
		return 0, fmt.Errorf("cookies: list: %w", err)
	}
	n := 0
	for _, r := range recs {
		if err := s.repo.Delete(ctx, r.ID); err != nil {
			if errors.Is(err, domain.ErrNotFound) { // removed concurrently
				continue
			}
			return n, fmt.Errorf("cookies: delete: %w", err)
		}
		n++
	}
	return n, nil
}

// PurgeExpired deletes expired cookies and returns how many were removed.
func (s *Store) PurgeExpired(ctx context.Context) (int64, error) {
	n, err := s.repo.DeleteExpired(ctx, s.now())
	if err != nil {
		return 0, fmt.Errorf("cookies: purge expired: %w", err)
	}
	return n, nil
}

// Run purges expired cookies now and then every interval until ctx ends.
func (s *Store) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := s.PurgeExpired(ctx); err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // shutting down
			}
			s.log.Warn("cookies: purge failed", "error", err)
		} else if n > 0 {
			s.log.Debug("cookies: purged expired cookies", "count", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// ForURL returns the unpartitioned, unexpired cookies that a request to u
// would carry, in RFC 6265 order (longest path first, then oldest first),
// with their values decrypted. A cookie that cannot be decrypted is skipped
// and logged.
func (s *Store) ForURL(ctx context.Context, u *url.URL) ([]*domain.Cookie, error) {
	now := s.now()
	var matched []*storage.CookieRecord
	for _, d := range candidateDomains(u.Hostname()) {
		recs, err := s.repo.List(ctx, storage.CookieFilter{Domain: d})
		if err != nil {
			return nil, fmt.Errorf("cookies: list: %w", err)
		}
		for _, r := range recs {
			if r.PartitionKey == "" && metadata(r).MatchesRequest(u, "", now) {
				matched = append(matched, r)
			}
		}
	}
	slices.SortStableFunc(matched, func(a, b *storage.CookieRecord) int {
		if len(a.Path) != len(b.Path) {
			return len(b.Path) - len(a.Path)
		}
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	out := make([]*domain.Cookie, 0, len(matched))
	for _, r := range matched {
		c, err := s.decrypt(r)
		if err != nil {
			s.log.Warn("cookies: skipping undecryptable cookie", "domain", r.Domain, "name", r.Name, "error", err)
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// ForSite returns the unexpired cookies of a registrable domain and of its
// subdomains, values decrypted, for a browser to load. A cookie that cannot
// be decrypted is skipped and logged.
func (s *Store) ForSite(ctx context.Context, site string) ([]*domain.Cookie, error) {
	site = domain.CanonicalDomain(site)
	if site == "" {
		return nil, domain.InvalidArgument("cookies: site must not be empty")
	}
	recs, err := s.repo.List(ctx, storage.CookieFilter{})
	if err != nil {
		return nil, fmt.Errorf("cookies: list: %w", err)
	}
	now := s.now()
	var out []*domain.Cookie
	for _, r := range recs {
		if r.Domain != site && !strings.HasSuffix(r.Domain, "."+site) {
			continue
		}
		if metadata(r).IsExpired(now) {
			continue
		}
		c, err := s.decrypt(r)
		if err != nil {
			s.log.Warn("cookies: skipping undecryptable cookie", "domain", r.Domain, "name", r.Name, "error", err)
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// SetCookies implements http.CookieJar: it stores the cookies a response
// from u set. Invalid cookies (domain not matching u, public suffix, Secure
// over plain HTTP) are dropped; an already expired cookie deletes the stored
// one, as RFC 6265 requires.
func (s *Store) SetCookies(u *url.URL, cookies []*http.Cookie) {
	ctx, cancel := context.WithTimeout(context.Background(), jarTimeout)
	defer cancel()
	now := s.now()
	for _, hc := range cookies {
		c, err := domain.FromHTTPCookie(hc, u, "", now)
		if err != nil {
			s.log.Debug("cookies: rejected cookie", "host", u.Hostname(), "name", hc.Name, "error", err)
			continue
		}
		if c.Secure && u.Scheme != "https" {
			s.log.Debug("cookies: rejected Secure cookie from insecure origin", "host", u.Hostname(), "name", c.Name)
			continue
		}
		if c.IsExpired(now) {
			s.deleteExact(ctx, c)
			continue
		}
		if err := s.Put(ctx, c); err != nil {
			s.log.Warn("cookies: cannot store cookie", "host", u.Hostname(), "name", c.Name, "error", err)
		}
	}
}

// Cookies implements http.CookieJar: it returns the cookies to send with a
// request to u (name and value only).
func (s *Store) Cookies(u *url.URL) []*http.Cookie {
	ctx, cancel := context.WithTimeout(context.Background(), jarTimeout)
	defer cancel()
	cs, err := s.ForURL(ctx, u)
	if err != nil {
		s.log.Warn("cookies: cannot read cookies", "host", u.Hostname(), "error", err)
		return nil
	}
	out := make([]*http.Cookie, len(cs))
	for i, c := range cs {
		out[i] = &http.Cookie{Name: c.Name, Value: c.Value} //nolint:gosec // request-side cookie: only name and value are sent
	}
	return out
}

// deleteExact removes the stored cookie with c's domain, name, path and
// partition key, if any.
func (s *Store) deleteExact(ctx context.Context, c *domain.Cookie) {
	recs, err := s.repo.List(ctx, storage.CookieFilter{Domain: c.Domain, Name: c.Name})
	if err != nil {
		s.log.Warn("cookies: cannot look up expired cookie", "domain", c.Domain, "name", c.Name, "error", err)
		return
	}
	for _, r := range recs {
		if r.Path != c.Path || r.PartitionKey != c.PartitionKey {
			continue
		}
		if err := s.repo.Delete(ctx, r.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
			s.log.Warn("cookies: cannot delete expired cookie", "domain", c.Domain, "name", c.Name, "error", err)
		}
	}
}

func (s *Store) decrypt(r *storage.CookieRecord) (*domain.Cookie, error) {
	plain, err := s.cipher.Open(r.EncryptedVal, aad(r.Domain, r.Name, r.Path, r.PartitionKey))
	if err != nil {
		return nil, fmt.Errorf("cookies: cookie %q of %q: %w", r.Name, r.Domain, err)
	}
	c := metadata(r)
	c.Value = string(plain)
	return c, nil
}

// metadata converts a record without decrypting its value.
func metadata(r *storage.CookieRecord) *domain.Cookie {
	return &domain.Cookie{
		ID:           r.ID,
		Domain:       r.Domain,
		Name:         r.Name,
		Path:         r.Path,
		PartitionKey: r.PartitionKey,
		Secure:       r.Secure,
		HTTPOnly:     r.HTTPOnly,
		SameSite:     r.SameSite,
		HostOnly:     r.HostOnly,
		ExpiresAt:    r.ExpiresAt,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
	}
}

func canonicalFilter(f storage.CookieFilter) storage.CookieFilter {
	f.Domain = domain.CanonicalDomain(f.Domain)
	return f
}

// candidateDomains lists the domains whose cookies may match host: the host
// itself and, for names (not IP addresses), each parent domain.
func candidateDomains(host string) []string {
	host = domain.CanonicalDomain(host)
	if host == "" {
		return nil
	}
	out := []string{host}
	if net.ParseIP(host) != nil {
		return out
	}
	for rest := host; ; {
		_, after, ok := strings.Cut(rest, ".")
		if !ok || domain.IsPublicSuffix(after) {
			return out // a cookie cannot be set for a public suffix
		}
		out = append(out, after)
		rest = after
	}
}
