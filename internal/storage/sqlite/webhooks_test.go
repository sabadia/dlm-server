package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

func TestWebhookRepository(t *testing.T) {
	ctx := t.Context()
	repo := openStore(t).Webhooks()
	now := time.UnixMilli(1_700_000_000_000).UTC()

	if list, err := repo.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("empty List = %v, %v", list, err)
	}
	a := &storage.WebhookRecord{ID: "w1", URL: "https://a.example/hook", Events: []string{"task.status", "queue.completed"}, Enabled: true, SealedSecret: []byte{1, 2, 3}, CreatedAt: now, UpdatedAt: now}
	b := &storage.WebhookRecord{ID: "w2", URL: "http://b.example/", Events: []string{"task.created"}, CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)}
	for _, w := range []*storage.WebhookRecord{b, a} {
		if err := repo.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Create(ctx, &storage.WebhookRecord{ID: "w1", URL: "http://x", Events: []string{"task.created"}, CreatedAt: now, UpdatedAt: now}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("duplicate ID: err = %v, want ErrAlreadyExists", err)
	}

	got, err := repo.Get(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(a, got); diff != "" {
		t.Errorf("Get differs (-want +got):\n%s", diff)
	}
	got, err = repo.Get(ctx, "w2")
	if err != nil || got.Enabled || len(got.SealedSecret) != 0 {
		t.Errorf("w2 = %+v, %v (disabled, unsigned)", got, err)
	}
	list, err := repo.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "w1" || list[1].ID != "w2" {
		t.Fatalf("List = %v, %v; want w1, w2 oldest first", list, err)
	}

	a.URL, a.Events, a.Enabled, a.SealedSecret, a.UpdatedAt = "https://c.example/", []string{"task.deleted"}, false, nil, now.Add(time.Minute)
	if err := repo.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, _ = repo.Get(ctx, "w1"); got.URL != "https://c.example/" || got.Enabled || len(got.SealedSecret) != 0 || len(got.Events) != 1 {
		t.Errorf("after Update = %+v", got)
	}

	if _, err := repo.Get(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get missing: %v", err)
	}
	if err := repo.Update(ctx, &storage.WebhookRecord{ID: "nope", URL: "http://x", Events: []string{"a"}}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Update missing: %v", err)
	}
	if err := repo.Delete(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, "w1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Delete twice: %v", err)
	}
}
