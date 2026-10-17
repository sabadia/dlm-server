package service

import (
	"errors"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

func TestQueueReferencedByName(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	fast, err := f.queues.Create(ctx, CreateQueueInput{Name: "Fast"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.queues.Create(ctx, CreateQueueInput{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}

	// Get/Stop/EffectiveForQueue by case-insensitive name.
	if q, err := f.queues.Get(ctx, "fast"); err != nil || q.ID != fast.ID {
		t.Fatalf("Get by name = %+v, %v", q, err)
	}
	if q, err := f.queues.Stop(ctx, "FAST"); err != nil || q.ID != fast.ID || q.State != domain.QueueStopped {
		t.Fatalf("Stop by name = %+v, %v", q, err)
	}
	if _, err := f.settings.EffectiveForQueue(ctx, "Fast"); err != nil {
		t.Fatalf("EffectiveForQueue by name: %v", err)
	}
	// IDs still work and win over names.
	if q, err := f.queues.Get(ctx, fast.ID); err != nil || q.ID != fast.ID {
		t.Fatalf("Get by id = %+v, %v", q, err)
	}
	if _, err := f.queues.Get(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown name: err = %v", err)
	}

	// Tasks: create, list filter and move by queue name.
	task, err := f.tasks.Create(ctx, CreateTaskInput{Source: domain.Source{CurrentURL: "http://127.0.0.1:1/a.bin"}, QueueID: "fast", Paused: true})
	if err != nil || task.QueueID != fast.ID {
		t.Fatalf("Create with queue name = %+v, %v", task, err)
	}
	page, err := f.tasks.List(ctx, storage.TaskFilter{QueueID: "Fast"})
	if err != nil || len(page.Tasks) != 1 {
		t.Fatalf("List by queue name = %d tasks, %v", len(page.Tasks), err)
	}
	if moved, err := f.tasks.Move(ctx, task.ID, "other"); err != nil || moved.QueueID != other.ID {
		t.Fatalf("Move by name = %+v, %v", moved, err)
	}
	if _, err := f.tasks.Create(ctx, CreateTaskInput{Source: domain.Source{CurrentURL: "http://127.0.0.1:1/b.bin"}, QueueID: "missing"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown queue name on create: err = %v", err)
	}

	// Delete with MoveTasksTo by name.
	if err := f.queues.Delete(ctx, "other", DeleteQueueOptions{MoveTasksTo: "main"}); err != nil {
		t.Fatalf("Delete by name: %v", err)
	}
	if got, err := f.tasks.Get(ctx, task.ID); err != nil || got.QueueID != domain.MainQueueID {
		t.Fatalf("task after queue delete = %+v, %v", got, err)
	}
}
