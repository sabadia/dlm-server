package service

import (
	"context"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// completedTask makes a completed task that finished daysAgo days ago.
func (f *svcFixture) completedTask(t *testing.T, daysAgo int, size int64) *domain.Task {
	t.Helper()
	task := f.withStatus(t, domain.StatusRunning, domain.StatusPostprocessing, domain.StatusCompleted)
	done := f.clock.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour)
	task.CompletedAt, task.Size, task.Downloaded = &done, size, size
	if err := f.store.Tasks().Update(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestCleanupRemovesOldCompletedTasksOnly(t *testing.T) {
	f := newSvcFixture(t)
	old := f.completedTask(t, 40, 100)
	older := f.completedTask(t, 400, 50)
	recent := f.completedTask(t, 2, 10)
	paused := f.withStatus(t, domain.StatusPaused)

	res, err := f.tasks.Cleanup(t.Context(), CleanupInput{OlderThanDays: 30, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 2 || res.Bytes != 150 || res.Deleted != 0 {
		t.Errorf("dry run = %+v", res)
	}
	if _, err := f.store.Tasks().Get(t.Context(), old.ID); err != nil {
		t.Fatal("a dry run removed a task")
	}

	res, err = f.tasks.Cleanup(t.Context(), CleanupInput{OlderThanDays: 30})
	if err != nil || res.Matched != 2 || res.Deleted != 2 || res.Failed != 0 {
		t.Fatalf("cleanup = %+v, %v", res, err)
	}
	for _, gone := range []*domain.Task{old, older} {
		if _, err := f.store.Tasks().Get(t.Context(), gone.ID); err == nil {
			t.Errorf("%s was not removed", gone.ID)
		}
	}
	for _, kept := range []*domain.Task{recent, paused} {
		if _, err := f.store.Tasks().Get(t.Context(), kept.ID); err != nil {
			t.Errorf("%s was removed: %v", kept.ID, err)
		}
	}

	// 0 days means every completed task.
	res, err = f.tasks.Cleanup(t.Context(), CleanupInput{})
	if err != nil || res.Deleted != 1 {
		t.Errorf("all completed = %+v, %v", res, err)
	}
	if _, err := f.store.Tasks().Get(t.Context(), paused.ID); err != nil {
		t.Error("an unfinished task was removed")
	}
	if _, err := f.tasks.Cleanup(t.Context(), CleanupInput{OlderThanDays: -1}); code(err) != "cleanup.bad_age" {
		t.Errorf("negative age: %v", err)
	}
}

func TestCleanupCanDeleteFiles(t *testing.T) {
	f := newSvcFixture(t)
	task := f.completedTask(t, 5, 3)
	dir := t.TempDir()
	task.Dir, task.Name = dir, "done.bin"
	if err := f.store.Tasks().Update(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	path := dir + "/done.bin"
	writeFile(t, path)
	res, err := f.tasks.Cleanup(t.Context(), CleanupInput{Files: true})
	if err != nil || res.Deleted != 1 {
		t.Fatalf("cleanup = %+v, %v", res, err)
	}
	if exists(t, path) {
		t.Error("the file was not deleted")
	}
}

func TestHousekeeperAppliesRetentionDays(t *testing.T) {
	f := newSvcFixture(t)
	old := f.completedTask(t, 10, 1)
	recent := f.completedTask(t, 1, 1)
	hk := NewHousekeeper(f.tasks.d, f.tasks)

	// Retention is off by default: nothing is removed.
	if n, err := hk.RunOnce(t.Context()); err != nil || n != 0 {
		t.Fatalf("RunOnce with retention off = %d, %v", n, err)
	}
	if err := f.store.Settings().SaveGlobal(t.Context(), settings.Settings{RetentionDays: settings.Some(7)}); err != nil {
		t.Fatal(err)
	}
	if n, err := hk.RunOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	if _, err := f.store.Tasks().Get(t.Context(), old.ID); err == nil {
		t.Error("the old task stayed")
	}
	if _, err := f.store.Tasks().Get(t.Context(), recent.ID); err != nil {
		t.Errorf("the recent task was removed: %v", err)
	}
}
