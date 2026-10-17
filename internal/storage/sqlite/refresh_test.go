package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/domain"
)

func attempt(taskID, id string, at time.Duration, outcome domain.RefreshOutcome, downloaded int64) *domain.RefreshAttempt {
	return &domain.RefreshAttempt{
		ID: id, TaskID: taskID, Strategy: "origin-redirect", Cause: domain.RefreshCauseExpired, Outcome: outcome,
		OldURL: "https://x/old", Downloaded: downloaded, CreatedAt: baseTime.Add(at).Truncate(time.Millisecond),
	}
}

func TestRefreshHistory(t *testing.T) {
	ctx := t.Context()
	s := openStore(t)
	repo := s.RefreshHistory()
	for _, id := range []string{"t1", "t2"} {
		if err := s.Tasks().Create(ctx, newTask(t, id, domain.MainQueueID, id+".bin", "https://x/"+id)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := repo.Latest(ctx, "t1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Latest of an empty history = %v, want ErrNotFound", err)
	}
	if err := repo.Add(ctx, attempt("missing", "a0", 0, domain.RefreshFailed, 0)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Add for a missing task = %v, want ErrNotFound", err)
	}

	deadline := baseTime.Add(time.Hour).Truncate(time.Millisecond)
	wait := attempt("t1", "a4", 4*time.Second, domain.RefreshWaiting, 100)
	wait.Strategy, wait.Deadline = "extension-capture", &deadline
	wait.Error = "waiting for a capture"
	wait.NewURL = "https://x/new"
	entries := []*domain.RefreshAttempt{
		attempt("t1", "a1", 1*time.Second, domain.RefreshFailed, 100),
		attempt("t1", "a2", 2*time.Second, domain.RefreshSkipped, 100),
		attempt("t1", "a3", 3*time.Second, domain.RefreshSuccess, 100),
		wait,
		attempt("t1", "a5", 5*time.Second, domain.RefreshMismatch, 200),
		attempt("t2", "b1", 1*time.Second, domain.RefreshFailed, 100),
	}
	for _, e := range entries {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatalf("Add %s: %v", e.ID, err)
		}
	}

	got, err := repo.List(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(entries[:5], got); diff != "" {
		t.Errorf("List (-want +got):\n%s", diff)
	}
	latest, err := repo.Latest(ctx, "t1")
	if err != nil || latest.ID != "a5" {
		t.Errorf("Latest = %+v, %v; want a5", latest, err)
	}

	// Failed, success and mismatch at 100 bytes count; the skip and the wait do not.
	for downloaded, want := range map[int64]int{100: 2, 200: 1, 300: 0} {
		if n, err := repo.CountAt(ctx, "t1", downloaded); err != nil || n != want {
			t.Errorf("CountAt(%d) = %d, %v; want %d", downloaded, n, err, want)
		}
	}

	// Pending awaits: only tasks that are awaiting_link and whose newest entry is a wait.
	if pending, err := repo.PendingAwaits(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("PendingAwaits for non-awaiting tasks = %d, %v; want 0", len(pending), err)
	}
	task, err := s.Tasks().Get(ctx, "t2")
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []domain.TaskStatus{domain.StatusRunning, domain.StatusAwaitingLink} {
		if err := task.TransitionTo(to, baseTime); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Tasks().Update(ctx, task); err != nil {
		t.Fatal(err)
	}
	w2 := attempt("t2", "b2", 2*time.Second, domain.RefreshWaiting, 100)
	w2.Deadline = &deadline
	noDeadline := attempt("t2", "b3", 3*time.Second, domain.RefreshWaiting, 100) // a wait without timeout is never pending
	for _, e := range []*domain.RefreshAttempt{w2} {
		if err := repo.Add(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := repo.PendingAwaits(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != "b2" {
		t.Fatalf("PendingAwaits = %+v, %v; want b2", pending, err)
	}
	if err := repo.Add(ctx, noDeadline); err != nil {
		t.Fatal(err)
	}
	if pending, err := repo.PendingAwaits(ctx); err != nil || len(pending) != 0 {
		t.Errorf("PendingAwaits after a newer wait without deadline = %d, %v; want 0", len(pending), err)
	}

	// Deleting the task deletes its history.
	if err := s.Tasks().Delete(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.List(ctx, "t1"); err != nil || len(got) != 0 {
		t.Errorf("List after task delete = %d, %v; want 0", len(got), err)
	}
}
