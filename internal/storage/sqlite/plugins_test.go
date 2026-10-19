package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

func TestPluginRepository(t *testing.T) {
	ctx := t.Context()
	repo := openStore(t).Plugins()
	now := time.UnixMilli(1_700_000_000_000).UTC()
	b := &storage.PluginRecord{Name: "beta", Version: "1.0", Title: "Beta", Description: "d", Source: "https://example.com/b.git", Enabled: true, InstalledAt: now, UpdatedAt: now}
	a := &storage.PluginRecord{Name: "alpha", Version: "2.0", Source: "/plugins/alpha", InstalledAt: now, UpdatedAt: now}
	for _, p := range []*storage.PluginRecord{b, a} {
		if err := repo.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Create(ctx, &storage.PluginRecord{Name: "alpha", Version: "3", Source: "x", InstalledAt: now, UpdatedAt: now}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("duplicate: %v", err)
	}
	got, err := repo.Get(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(b, got); diff != "" {
		t.Errorf("Get differs (-want +got):\n%s", diff)
	}
	list, err := repo.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "beta" {
		t.Fatalf("List = %v, %v", list, err)
	}
	a.Version, a.Enabled, a.UpdatedAt = "2.1", true, now.Add(time.Hour)
	if err := repo.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, _ = repo.Get(ctx, "alpha"); got.Version != "2.1" || !got.Enabled || !got.InstalledAt.Equal(now) {
		t.Errorf("after Update = %+v (installedAt is kept)", got)
	}
	if err := repo.Update(ctx, &storage.PluginRecord{Name: "nope"}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}
	if _, err := repo.Get(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get missing: %v", err)
	}
	if err := repo.Delete(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, "alpha"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}
