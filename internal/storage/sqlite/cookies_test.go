package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

func TestCookieRepository(t *testing.T) {
	ctx := t.Context()
	s := openStore(t)
	repo := s.Cookies()

	exp := baseTime.Add(time.Hour).Truncate(time.Millisecond)
	c1 := &storage.CookieRecord{
		ID:           domain.NewID(),
		Domain:       "example.com",
		Name:         "sess",
		Path:         "/",
		PartitionKey: "",
		EncryptedVal: []byte("cipher1"),
		Secure:       true,
		HTTPOnly:     true,
		SameSite:     "lax",
		HostOnly:     false,
		ExpiresAt:    &exp,
		CreatedAt:    baseTime.Truncate(time.Millisecond),
		UpdatedAt:    baseTime.Truncate(time.Millisecond),
	}

	// 1. Upsert
	if err := repo.Upsert(ctx, c1); err != nil {
		t.Fatalf("Upsert c1: %v", err)
	}

	// 2. Get
	got, err := repo.Get(ctx, c1.ID)
	if err != nil {
		t.Fatalf("Get c1: %v", err)
	}
	if diff := cmp.Diff(c1, got); diff != "" {
		t.Fatalf("Get mismatch (-want +got):\n%s", diff)
	}

	// 3. Upsert update (conflict on domain, name, path, partition_key)
	updatedTime := baseTime.Add(time.Minute).Truncate(time.Millisecond)
	c1Updated := &storage.CookieRecord{
		ID:           domain.NewID(), // different ID in upsert, but conflict will update existing
		Domain:       "example.com",
		Name:         "sess",
		Path:         "/",
		PartitionKey: "",
		EncryptedVal: []byte("cipher2"),
		Secure:       false,
		HTTPOnly:     true,
		SameSite:     "strict",
		HostOnly:     true,
		ExpiresAt:    nil,
		CreatedAt:    updatedTime,
		UpdatedAt:    updatedTime,
	}
	if err := repo.Upsert(ctx, c1Updated); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	// The original row's ID should remain
	gotUpdated, err := repo.Get(ctx, c1.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if string(gotUpdated.EncryptedVal) != "cipher2" {
		t.Errorf("EncryptedVal = %s, want cipher2", gotUpdated.EncryptedVal)
	}
	if gotUpdated.Secure {
		t.Error("expected Secure = false")
	}
	if gotUpdated.SameSite != "strict" {
		t.Errorf("SameSite = %s, want strict", gotUpdated.SameSite)
	}
	if gotUpdated.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil", gotUpdated.ExpiresAt)
	}

	// 4. Insert another cookie with partition key
	c2 := &storage.CookieRecord{
		ID:           domain.NewID(),
		Domain:       "sub.example.com",
		Name:         "token",
		Path:         "/api",
		PartitionKey: "site1",
		EncryptedVal: []byte("cipher3"),
		Secure:       true,
		CreatedAt:    baseTime.Truncate(time.Millisecond),
		UpdatedAt:    baseTime.Truncate(time.Millisecond),
	}
	if err := repo.Upsert(ctx, c2); err != nil {
		t.Fatalf("Upsert c2: %v", err)
	}

	// 5. List all
	all, err := repo.List(ctx, storage.CookieFilter{})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List all count = %d, want 2", len(all))
	}

	// 6. List with filter
	byDomain, err := repo.List(ctx, storage.CookieFilter{Domain: "example.com"})
	if err != nil {
		t.Fatalf("List by domain: %v", err)
	}
	if len(byDomain) != 1 || byDomain[0].Name != "sess" {
		t.Fatalf("List by domain got %d results, want 1 (sess)", len(byDomain))
	}

	byPartition, err := repo.List(ctx, storage.CookieFilter{PartitionKey: "site1"})
	if err != nil {
		t.Fatalf("List by partition: %v", err)
	}
	if len(byPartition) != 1 || byPartition[0].Name != "token" {
		t.Fatalf("List by partition got %d results, want 1 (token)", len(byPartition))
	}

	// 7. Delete
	if err := repo.Delete(ctx, c2.ID); err != nil {
		t.Fatalf("Delete c2: %v", err)
	}
	if _, err := repo.Get(ctx, c2.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get deleted c2: want ErrNotFound, got %v", err)
	}
	if err := repo.Delete(ctx, c2.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Delete non-existent: want ErrNotFound, got %v", err)
	}

	// 8. DeleteExpired
	past := baseTime.Add(-time.Hour).Truncate(time.Millisecond)
	cExpired := &storage.CookieRecord{
		ID:           domain.NewID(),
		Domain:       "expired.com",
		Name:         "old",
		Path:         "/",
		EncryptedVal: []byte("expired"),
		ExpiresAt:    &past,
		CreatedAt:    baseTime.Truncate(time.Millisecond),
		UpdatedAt:    baseTime.Truncate(time.Millisecond),
	}
	if err := repo.Upsert(ctx, cExpired); err != nil {
		t.Fatalf("Upsert expired: %v", err)
	}
	deleted, err := repo.DeleteExpired(ctx, baseTime)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if deleted != 1 {
		t.Errorf("DeleteExpired count = %d, want 1", deleted)
	}
	if _, err := repo.Get(ctx, cExpired.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get deleted expired cookie: want ErrNotFound, got %v", err)
	}
}
