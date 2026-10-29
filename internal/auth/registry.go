package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

// Scope says what a token may do.
type Scope string

// Token scopes.
const (
	// ScopeAdmin may do everything the API offers.
	ScopeAdmin Scope = "admin"
	// ScopeRead may only read: GET requests, and not what exposes secrets.
	ScopeRead Scope = "read"
)

// Valid reports whether s is a known scope.
func (s Scope) Valid() bool { return s == ScopeAdmin || s == ScopeRead }

// TokenPrefix starts every generated named token, so a leaked one is easy to
// recognise.
const TokenPrefix = "dlmt_"

// touchInterval bounds how often a token's last-used time is written.
const touchInterval = time.Minute

// MaxNameLength bounds the name of a token.
const MaxNameLength = 64

// Principal is who made a request.
type Principal struct {
	// ID is the token's ID; empty for the primary token.
	ID string
	// Name is the token's name, "primary" for the primary token.
	Name  string
	Scope Scope
	// Primary is true for the token in the token file.
	Primary bool
}

// Token is a named token as it is listed: never the secret itself.
type Token struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scope      Scope      `json:"scope"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

// HashToken returns the hex SHA-256 of a token, which is all that is stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewToken generates a random token (TokenPrefix and 32 random bytes in
// base64url).
func NewToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Registry keeps the named tokens. It looks tokens up in memory and writes
// last-used times back at most once a minute per token. It is safe for
// concurrent use.
type Registry struct {
	repo  storage.TokenRepository
	clock func() time.Time
	newID func() string

	mu      sync.RWMutex
	byHash  map[string]*storage.TokenRecord
	touched map[string]time.Time
}

// NewRegistry creates a Registry over repo; call Load before use.
func NewRegistry(repo storage.TokenRepository) *Registry {
	return &Registry{repo: repo, clock: time.Now, newID: domain.NewID, byHash: map[string]*storage.TokenRecord{}, touched: map[string]time.Time{}}
}

// Load reads the stored tokens.
func (r *Registry) Load(ctx context.Context) error {
	recs, err := r.repo.List(ctx)
	if err != nil {
		return fmt.Errorf("auth: load tokens: %w", err)
	}
	m := make(map[string]*storage.TokenRecord, len(recs))
	for _, rec := range recs {
		m[rec.Hash] = rec
	}
	r.mu.Lock()
	r.byHash = m
	r.mu.Unlock()
	return nil
}

func tokenOf(rec *storage.TokenRecord) Token {
	return Token{ID: rec.ID, Name: rec.Name, Scope: Scope(rec.Scope), CreatedAt: rec.CreatedAt, LastUsedAt: rec.LastUsedAt}
}

// List returns the named tokens, oldest first.
func (r *Registry) List(ctx context.Context) ([]Token, error) {
	recs, err := r.repo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list tokens: %w", err)
	}
	out := make([]Token, len(recs))
	for i, rec := range recs {
		out[i] = tokenOf(rec)
	}
	return out, nil
}

// ValidateName checks the name of a token: 1 to MaxNameLength characters
// without control characters. It returns an error matching
// domain.ErrInvalidArgument.
func ValidateName(name string) error {
	if name == "" || len(name) > MaxNameLength {
		return domain.InvalidArgumentCode("token.bad_name", nil, "auth: a token name has 1 to %d characters", MaxNameLength)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return domain.InvalidArgumentCode("token.bad_name", nil, "auth: a token name must not contain control characters")
		}
	}
	return nil
}

// Create makes a token and returns it together with its listing. The secret
// is not stored and cannot be shown again. A name that is taken (ignoring
// case) matches domain.ErrAlreadyExists.
func (r *Registry) Create(ctx context.Context, name string, scope Scope) (string, Token, error) {
	name = strings.TrimSpace(name)
	if err := ValidateName(name); err != nil {
		return "", Token{}, err
	}
	if !scope.Valid() {
		return "", Token{}, domain.InvalidArgumentCode("token.bad_scope", nil, "auth: scope must be admin or read")
	}
	token, err := NewToken()
	if err != nil {
		return "", Token{}, err
	}
	rec := &storage.TokenRecord{ID: r.newID(), Name: name, Scope: string(scope), Hash: HashToken(token), CreatedAt: r.clock().UTC()}
	if err := r.repo.Create(ctx, rec); err != nil {
		return "", Token{}, fmt.Errorf("auth: create token: %w", err)
	}
	r.mu.Lock()
	r.byHash[rec.Hash] = rec
	r.mu.Unlock()
	return token, tokenOf(rec), nil
}

// Delete revokes a token at once. An unknown ID matches domain.ErrNotFound.
func (r *Registry) Delete(ctx context.Context, id string) error {
	if err := r.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("auth: delete token: %w", err)
	}
	r.mu.Lock()
	for h, rec := range r.byHash {
		if rec.ID == id {
			delete(r.byHash, h)
		}
	}
	delete(r.touched, id)
	r.mu.Unlock()
	return nil
}

// Authenticate looks a presented token up; ok is false for an unknown one.
func (r *Registry) Authenticate(ctx context.Context, token string) (p Principal, ok bool) {
	if r == nil || !strings.HasPrefix(token, TokenPrefix) {
		return Principal{}, false
	}
	hash := HashToken(token)
	r.mu.RLock()
	rec := r.byHash[hash]
	r.mu.RUnlock()
	if rec == nil {
		return Principal{}, false
	}
	now := r.clock()
	r.mu.Lock()
	due := now.Sub(r.touched[rec.ID]) >= touchInterval
	if due {
		r.touched[rec.ID] = now
	}
	r.mu.Unlock()
	if due {
		_ = r.repo.Touch(ctx, rec.ID, now.UTC()) // best effort: the last-used time is informational
	}
	return Principal{ID: rec.ID, Name: rec.Name, Scope: Scope(rec.Scope)}, true
}
