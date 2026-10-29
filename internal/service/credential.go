package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sabadia/dlm-server/internal/events"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/storage"
)

// CredentialService manages the stored site logins that answer HTTP Basic and
// Digest challenges. Passwords are sealed at rest and never returned. It also
// is the netclient.CredentialSource the HTTP clients look logins up in.
type CredentialService struct {
	d       Deps
	sealer  Sealer
	version atomic.Uint64

	mu     sync.Mutex
	loaded bool
	at     uint64
	exact  map[string]netclient.Login
	suffix map[string]netclient.Login // "example.com" for "*.example.com"
}

// NewCredentialService creates a CredentialService.
func NewCredentialService(d Deps, sealer Sealer) *CredentialService {
	return &CredentialService{d: d.withDefaults(), sealer: sealer}
}

// CreateCredentialInput are the fields of a new login.
type CreateCredentialInput struct {
	Host     string
	Scheme   string // default auto
	Username string
	Password string
}

// UpdateCredentialInput changes a login; nil fields are left unchanged.
type UpdateCredentialInput struct {
	Host     *string
	Scheme   *string
	Username *string
	Password *string
}

func credentialEntity(r *storage.CredentialRecord) *domain.Credential {
	return &domain.Credential{
		ID: r.ID, Host: r.Host, Scheme: r.Scheme, Username: r.Username, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (s *CredentialService) seal(id, password string) ([]byte, error) {
	sealed, err := s.sealer.Seal([]byte(password), []byte(id))
	if err != nil {
		return nil, fmt.Errorf("service: seal credential password: %w", err)
	}
	return sealed, nil
}

// List returns every login, ordered by host.
func (s *CredentialService) List(ctx context.Context) ([]*domain.Credential, error) {
	rs, err := s.d.Store.Credentials().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: list credentials: %w", err)
	}
	out := make([]*domain.Credential, 0, len(rs))
	for _, r := range rs {
		out = append(out, credentialEntity(r))
	}
	return out, nil
}

// Get returns one login.
func (s *CredentialService) Get(ctx context.Context, id string) (*domain.Credential, error) {
	r, err := s.d.Store.Credentials().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("service: get credential: %w", err)
	}
	return credentialEntity(r), nil
}

// Create adds a login.
func (s *CredentialService) Create(ctx context.Context, in CreateCredentialInput) (*domain.Credential, error) {
	if in.Scheme == "" {
		in.Scheme = domain.CredentialAuto
	}
	now := s.d.now()
	rec := &storage.CredentialRecord{
		ID: s.d.NewID(), Host: strings.ToLower(strings.TrimSpace(in.Host)), Scheme: in.Scheme, Username: in.Username,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := credentialEntity(rec).Validate(); err != nil {
		return nil, fmt.Errorf("service: create credential: %w", err)
	}
	if in.Password == "" {
		return nil, fmt.Errorf("service: create credential: %w", domain.InvalidArgument("password is required"))
	}
	var err error
	if rec.SealedPassword, err = s.seal(rec.ID, in.Password); err != nil {
		return nil, err
	}
	if err := s.d.Store.Credentials().Create(ctx, rec); err != nil {
		return nil, fmt.Errorf("service: create credential: %w", err)
	}
	s.version.Add(1)
	publishResource(s.d.Bus, events.ResourceCredential, rec.ID, events.OpCreated)
	return credentialEntity(rec), nil
}

// Update changes a login.
func (s *CredentialService) Update(ctx context.Context, id string, in UpdateCredentialInput) (*domain.Credential, error) {
	var rec *storage.CredentialRecord
	err := s.d.Store.WithTx(ctx, func(tx storage.Store) error {
		var err error
		if rec, err = tx.Credentials().Get(ctx, id); err != nil {
			return err //nolint:wrapcheck // wrapped below
		}
		if in.Host != nil {
			rec.Host = strings.ToLower(strings.TrimSpace(*in.Host))
		}
		if in.Scheme != nil {
			rec.Scheme = *in.Scheme
		}
		if in.Username != nil {
			rec.Username = *in.Username
		}
		if err := credentialEntity(rec).Validate(); err != nil {
			return err //nolint:wrapcheck // wrapped below
		}
		if in.Password != nil {
			if *in.Password == "" {
				return domain.InvalidArgument("password must not be empty")
			}
			if rec.SealedPassword, err = s.seal(id, *in.Password); err != nil {
				return err
			}
		}
		rec.UpdatedAt = s.d.now()
		return tx.Credentials().Update(ctx, rec) //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return nil, fmt.Errorf("service: update credential: %w", err)
	}
	s.version.Add(1)
	publishResource(s.d.Bus, events.ResourceCredential, id, events.OpUpdated)
	return credentialEntity(rec), nil
}

// Delete removes a login.
func (s *CredentialService) Delete(ctx context.Context, id string) error {
	if err := s.d.Store.Credentials().Delete(ctx, id); err != nil {
		return fmt.Errorf("service: delete credential: %w", err)
	}
	s.version.Add(1)
	publishResource(s.d.Bus, events.ResourceCredential, id, events.OpDeleted)
	return nil
}

// Lookup implements netclient.CredentialSource: the login for host, an exact
// host before a wildcard, the most specific wildcard first. The logins are
// loaded once and again after every change.
func (s *CredentialService) Lookup(host string) (netclient.Login, bool) {
	host = strings.ToLower(host)
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.version.Load(); !s.loaded || v != s.at {
		s.reload(v)
	}
	if l, ok := s.exact[host]; ok {
		return l, true
	}
	for rest := host; ; {
		_, after, ok := strings.Cut(rest, ".")
		if !ok {
			return netclient.Login{}, false
		}
		if l, ok := s.suffix[after]; ok {
			return l, true
		}
		rest = after
	}
}

// reload rebuilds the lookup tables; the caller holds s.mu. A login whose
// password cannot be opened is left out.
func (s *CredentialService) reload(version uint64) {
	ctx := context.Background()
	rs, err := s.d.Store.Credentials().List(ctx)
	if err != nil {
		s.d.Logger.ErrorContext(ctx, "load credentials", "error", err)
		return // keep the old tables and try again on the next request
	}
	exact := make(map[string]netclient.Login, len(rs))
	suffix := map[string]netclient.Login{}
	for _, r := range rs {
		pw, err := s.sealer.Open(r.SealedPassword, []byte(r.ID))
		if err != nil {
			s.d.Logger.WarnContext(ctx, "skipping credential", "host", r.Host, "error", err)
			continue
		}
		l := netclient.Login{Username: r.Username, Password: string(pw), Scheme: r.Scheme}
		if name, ok := strings.CutPrefix(r.Host, "*."); ok {
			suffix[name] = l
		} else {
			exact[r.Host] = l
		}
	}
	s.exact, s.suffix, s.at, s.loaded = exact, suffix, version, true
}
