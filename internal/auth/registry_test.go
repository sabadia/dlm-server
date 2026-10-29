package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

func newRegistry(t *testing.T) (*Registry, storage.Store) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "dlm.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	r := NewRegistry(store.Tokens())
	if err := r.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	return r, store
}

func TestCreateAuthenticateAndRevoke(t *testing.T) {
	r, store := newRegistry(t)
	token, info, err := r.Create(t.Context(), "  dashboard ", ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, TokenPrefix) || len(token) < 40 || info.Name != "dashboard" || info.Scope != ScopeRead || info.ID == "" {
		t.Fatalf("token %q info %+v", token, info)
	}
	p, ok := r.Authenticate(t.Context(), token)
	if !ok || p.Name != "dashboard" || p.Scope != ScopeRead || p.Primary || p.ID != info.ID {
		t.Fatalf("principal = %+v, %v", p, ok)
	}
	for _, bad := range []string{"", token + "x", "dlmt_" + strings.Repeat("A", 43), strings.TrimPrefix(token, TokenPrefix)} {
		if _, ok := r.Authenticate(t.Context(), bad); ok {
			t.Errorf("%q authenticated", bad)
		}
	}

	// Only a hash is stored, never the token.
	recs, _ := store.Tokens().List(t.Context())
	if len(recs) != 1 || recs[0].Hash != HashToken(token) || strings.Contains(recs[0].Hash, token) {
		t.Fatalf("stored = %+v", recs)
	}

	list, err := r.List(t.Context())
	if err != nil || len(list) != 1 || list[0].Name != "dashboard" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := r.Delete(t.Context(), info.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Authenticate(t.Context(), token); ok {
		t.Error("a revoked token still authenticates")
	}
	if err := r.Delete(t.Context(), info.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
}

func TestCreateRefusals(t *testing.T) {
	r, _ := newRegistry(t)
	if _, _, err := r.Create(t.Context(), "ci", ScopeAdmin); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		name  string
		scope Scope
		kind  error
	}{
		"duplicate (case-insensitive)": {"CI", ScopeRead, domain.ErrAlreadyExists},
		"empty name":                   {"  ", ScopeRead, domain.ErrInvalidArgument},
		"long name":                    {strings.Repeat("x", 65), ScopeRead, domain.ErrInvalidArgument},
		"control character":            {"a\nb", ScopeRead, domain.ErrInvalidArgument},
		"unknown scope":                {"other", "root", domain.ErrInvalidArgument},
	} {
		if _, _, err := r.Create(t.Context(), tc.name, tc.scope); !errors.Is(err, tc.kind) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.kind)
		}
	}
}

func TestTokensSurviveARestartAndLastUseIsRecordedOncePerMinute(t *testing.T) {
	r, store := newRegistry(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	r.clock = func() time.Time { return now }
	token, info, err := r.Create(t.Context(), "app", ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	r.Authenticate(t.Context(), token)
	now = now.Add(10 * time.Second)
	r.Authenticate(t.Context(), token) // within the minute: not written again
	recs, _ := store.Tokens().List(t.Context())
	if recs[0].LastUsedAt == nil || !recs[0].LastUsedAt.Equal(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("last used = %v, want the first use", recs[0].LastUsedAt)
	}
	now = now.Add(2 * time.Minute)
	r.Authenticate(t.Context(), token)
	recs, _ = store.Tokens().List(t.Context())
	if recs[0].LastUsedAt == nil || !recs[0].LastUsedAt.Equal(now) {
		t.Errorf("last used = %v, want %v", recs[0].LastUsedAt, now)
	}

	again := NewRegistry(store.Tokens())
	if err := again.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p, ok := again.Authenticate(t.Context(), token); !ok || p.ID != info.ID {
		t.Errorf("after a restart: %+v, %v", p, ok)
	}
	var nilRegistry *Registry
	if _, ok := nilRegistry.Authenticate(t.Context(), token); ok {
		t.Error("a nil registry authenticated")
	}
}

func TestExtensionTokenMetadataSurvivesAReload(t *testing.T) {
	r, store := newRegistry(t)
	secret, info, err := r.CreateExtension(t.Context(), "Firefox extension", ExtensionInfo{Browser: "firefox", ID: "dlm-extension@sabadia.github.io", Version: "0.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Scope != ScopeExtension || info.Kind != KindExtension {
		t.Fatalf("info = %+v", info)
	}
	again := NewRegistry(store.Tokens())
	if err := again.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p, ok := again.Authenticate(t.Context(), secret); !ok || p.Scope != ScopeExtension || p.Name != "Firefox extension" {
		t.Fatalf("principal = %+v, %v", p, ok)
	}
	list, _ := again.List(t.Context())
	if len(list) != 1 || list[0].Browser != "firefox" || list[0].ClientID != "dlm-extension@sabadia.github.io" || list[0].ClientVersion != "0.2.0" {
		t.Fatalf("listing = %+v", list)
	}
	// An extension token cannot be made by hand through Create.
	if _, _, err := r.Create(t.Context(), "sneaky", ScopeExtension); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Create(extension) = %v, want invalid argument", err)
	}
}
