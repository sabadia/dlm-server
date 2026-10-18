package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

func TestCredentialRepository(t *testing.T) {
	ctx := t.Context()
	repo := openStore(t).Credentials()
	now := time.UnixMilli(1_700_000_000_000).UTC()

	a := &storage.CredentialRecord{ID: "c1", Host: "files.example.com", Scheme: "digest", Username: "ann", SealedPassword: []byte{9, 9}, CreatedAt: now, UpdatedAt: now}
	b := &storage.CredentialRecord{ID: "c2", Host: "*.alpha.example", Scheme: "auto", Username: "bob", SealedPassword: []byte{1}, CreatedAt: now, UpdatedAt: now}
	for _, c := range []*storage.CredentialRecord{a, b} {
		if err := repo.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Create(ctx, &storage.CredentialRecord{ID: "c3", Host: "files.example.com", Scheme: "basic", Username: "x", SealedPassword: []byte{1}, CreatedAt: now, UpdatedAt: now}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("duplicate host: %v", err)
	}
	if err := repo.Create(ctx, &storage.CredentialRecord{ID: "c1", Host: "other.example", Scheme: "basic", Username: "x", SealedPassword: []byte{1}, CreatedAt: now, UpdatedAt: now}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("duplicate id: %v", err)
	}
	got, err := repo.Get(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(a, got); diff != "" {
		t.Errorf("Get differs (-want +got):\n%s", diff)
	}
	list, err := repo.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "c2" || list[1].ID != "c1" {
		t.Fatalf("List = %v, %v; want ordered by host (*.alpha.example first)", list, err)
	}
	a.Username, a.Scheme, a.SealedPassword, a.UpdatedAt = "ann2", "basic", []byte{7}, now.Add(time.Hour)
	if err := repo.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, _ = repo.Get(ctx, "c1"); got.Username != "ann2" || got.Scheme != "basic" || got.SealedPassword[0] != 7 {
		t.Errorf("after Update = %+v", got)
	}
	a.Host = "*.alpha.example"
	if err := repo.Update(ctx, a); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("update to a taken host: %v", err)
	}
	if err := repo.Update(ctx, &storage.CredentialRecord{ID: "nope", Host: "x.example", Scheme: "auto", Username: "u"}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}
	if _, err := repo.Get(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get missing: %v", err)
	}
	if err := repo.Delete(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, "c1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}
