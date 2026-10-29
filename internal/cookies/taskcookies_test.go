package cookies_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/storage"
)

func TestTaskCookiesAreScopedAndCapped(t *testing.T) {
	f := newFixture(t)
	far := t0.Add(365 * 24 * time.Hour)
	res, err := f.store.ImportForTask(t.Context(), "t1", []*domain.Cookie{
		{Domain: "example.com", Name: "a", Value: "1", Path: "/", ExpiresAt: &far, PartitionKey: "https://example.com"},
		{Domain: "example.com", Name: "b", Value: "2", Path: "/"},
	})
	if err != nil || res.Imported != 2 {
		t.Fatalf("import = %+v, %v", res, err)
	}
	recs, _ := f.repo.List(t.Context(), storage.CookieFilter{})
	for _, r := range recs {
		if r.PartitionKey != cookies.TaskPartition("t1") {
			t.Errorf("%s: partition %q", r.Name, r.PartitionKey)
		}
		// Whatever the cookie says, nothing stays longer than TaskCookieTTL.
		if r.ExpiresAt == nil || r.ExpiresAt.After(t0.Add(cookies.TaskCookieTTL)) {
			t.Errorf("%s: expires %v", r.Name, r.ExpiresAt)
		}
	}

	// Sent with requests, but never listed.
	got, err := f.store.ForURL(t.Context(), mustURL(t, "https://example.com/"))
	if err != nil || len(got) != 2 {
		t.Fatalf("ForURL = %+v, %v", got, err)
	}
	if l, _ := f.store.List(t.Context(), storage.CookieFilter{}); len(l) != 0 {
		t.Errorf("List shows the task's cookies: %+v", l)
	}
	if l, _ := f.store.ListMetadata(t.Context(), storage.CookieFilter{}); len(l) != 0 {
		t.Errorf("ListMetadata shows the task's cookies: %+v", l)
	}

	// Another task's cookies stay.
	if _, err := f.store.ImportForTask(t.Context(), "t2", []*domain.Cookie{{Domain: "example.com", Name: "c", Value: "3", Path: "/"}}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.store.DeleteTask(t.Context(), "t1"); err != nil || n != 2 {
		t.Fatalf("DeleteTask(t1) = %d, %v", n, err)
	}
	if got, _ := f.store.ForURL(t.Context(), mustURL(t, "https://example.com/")); len(got) != 1 || got[0].Name != "c" {
		t.Errorf("after deleting t1: %+v", got)
	}

	// The cap removes what nobody cleaned up.
	f.clock.Advance(cookies.TaskCookieTTL + time.Minute)
	if n, err := f.store.PurgeExpired(t.Context()); err != nil || n != 1 {
		t.Errorf("PurgeExpired = %d, %v", n, err)
	}
	if _, err := f.store.ImportForTask(t.Context(), "", nil); err == nil {
		t.Error("an empty task id was accepted")
	}
}

func TestTaskCookieBeatsTheUsersCookieOfTheSameName(t *testing.T) {
	f := newFixture(t)
	if err := f.store.Put(t.Context(), &domain.Cookie{Domain: "example.com", Name: "sid", Value: "mine", Path: "/"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ImportForTask(t.Context(), "t1", []*domain.Cookie{{Domain: "example.com", Name: "sid", Value: "captured", Path: "/"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.store.ForURL(t.Context(), mustURL(t, "https://example.com/"))
	if len(got) != 1 || got[0].Value != "captured" {
		t.Fatalf("ForURL = %+v", got)
	}
	// The user's own cookie was not replaced.
	l, _ := f.store.List(t.Context(), storage.CookieFilter{})
	if len(l) != 1 || l[0].Value != "mine" {
		t.Errorf("List = %+v", l)
	}
}

func TestRunTaskCleanupRemovesCookiesOfCompletedAndDeletedTasks(t *testing.T) {
	f := newFixture(t)
	bus := events.New(events.Options{})
	t.Cleanup(bus.Close)
	for _, id := range []string{"done", "gone", "failed", "running"} {
		if _, err := f.store.ImportForTask(t.Context(), id, []*domain.Cookie{{Domain: "example.com", Name: "c-" + id, Value: "v", Path: "/"}}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.store.RunTaskCleanup(ctx, bus) }()

	// The events are published until the cleaner (which subscribes in its own
	// goroutine) has seen them; deleting twice is harmless.
	publish := func() {
		bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: "done", Data: events.StatusData{From: domain.StatusRunning, To: domain.StatusCompleted}})
		bus.Publish(events.Event{Type: events.TypeTaskDeleted, TaskID: "gone", Data: domain.Task{ID: "gone"}})
		bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: "failed", Data: events.StatusData{From: domain.StatusRunning, To: domain.StatusError}})
		bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: "running", Data: events.StatusData{From: domain.StatusQueued, To: domain.StatusRunning}})
	}
	deadline := time.Now().Add(3 * time.Second)
	var left []string
	for time.Now().Before(deadline) {
		publish()
		time.Sleep(20 * time.Millisecond)
		left = left[:0]
		recs, _ := f.repo.List(t.Context(), storage.CookieFilter{})
		for _, r := range recs {
			left = append(left, r.Name)
		}
		if len(left) == 2 {
			break
		}
	}
	slices.Sort(left)
	if !slices.Equal(left, []string{"c-failed", "c-running"}) {
		t.Errorf("cookies left = %v, want those of the failed and the running task", left)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("RunTaskCleanup: %v", err)
	}
}
