package service

import (
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// A task that cannot reach its host keeps its place: it is marked as waiting
// for a retry, is started again when that is due, and gives up after the
// configured number of retries.
func TestFailedTaskRetriesByItselfThenGivesUp(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	task, err := domain.NewTask(domain.NewID(), time.Now(), domain.NewTaskParams{
		Source: domain.Source{CurrentURL: "http://127.0.0.1:1/unreachable"},
		Settings: settings.Settings{Retry: settings.RetrySettings{
			MaxAttempts:  settings.Some(0),
			TaskAttempts: settings.Some(2),
			TaskBackoff:  settings.Some(settings.Duration(time.Minute)),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Tasks().Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	retrier := NewAutoRetrier(Deps{Store: f.store}, nil)
	retrier.tasks = &TaskService{d: retrier.d}

	for attempt := 1; attempt <= 2; attempt++ {
		if err := f.runner.Run(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		got := f.task(t, task.ID)
		if got.Status != domain.StatusError || got.Error == nil || got.Error.Retry == nil {
			t.Fatalf("attempt %d: task = %s %+v, want an error waiting for a retry", attempt, got.Status, got.Error)
		}
		if r := got.Error.Retry; r.Attempt != attempt || r.Max != 2 || !r.NextAt.After(time.Now()) {
			t.Fatalf("attempt %d: retry = %+v", attempt, r)
		}
		if n, err := retrier.RunOnce(ctx); err != nil || n != 0 {
			t.Fatalf("a retry that is not due was started: %d, %v", n, err)
		}
		// Make it due.
		got.Error.Retry.NextAt = time.Now().Add(-time.Second)
		if err := f.store.Tasks().Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		if n, err := retrier.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("RunOnce = %d, %v; want the due task started", n, err)
		}
		if got := f.task(t, task.ID); got.Status != domain.StatusQueued {
			t.Fatalf("status after the retry is due = %s, want queued", got.Status)
		}
	}

	if err := f.runner.Run(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	got := f.task(t, task.ID)
	if got.Status != domain.StatusError || got.Error == nil || got.Error.Retry != nil {
		t.Fatalf("after the last retry: %s %+v, want an error that has given up", got.Status, got.Error)
	}
	if n, _ := retrier.RunOnce(ctx); n != 0 {
		t.Errorf("a task that gave up was started again")
	}
}
