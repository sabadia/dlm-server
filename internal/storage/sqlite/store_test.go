package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

var baseTime = time.Date(2026, 10, 17, 12, 30, 45, 123456789, time.UTC)

// allowUnexported lets cmp look into settings.Optional's unexported fields.
var allowUnexported = cmp.Exporter(func(reflect.Type) bool { return true })

func fixedClock() time.Time { return baseTime }

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dlm.db")
	s, err := sqlite.Open(t.Context(), path, sqlite.Options{Clock: fixedClock})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func newQueue(t *testing.T, id, name string, pos int64) *domain.Queue {
	t.Helper()
	q, err := domain.NewQueue(id, name, baseTime, settings.Settings{
		MaxConcurrentDownloads: settings.Some(2),
		SpeedLimit:             settings.Some[int64](1 << 20),
	})
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	q.Position = pos
	return q
}

func newTask(t *testing.T, id, queueID, name, url string) *domain.Task {
	t.Helper()
	task, err := domain.NewTask(id, baseTime, domain.NewTaskParams{
		QueueID: queueID,
		Name:    name,
		Source:  domain.Source{CurrentURL: url},
	})
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	return task
}

// fullTask returns a task with every optional field populated.
func fullTask(t *testing.T, id string) *domain.Task {
	t.Helper()
	lm := baseTime.Add(-time.Hour)
	task, err := domain.NewTask(id, baseTime, domain.NewTaskParams{
		Name:   "file.iso",
		Engine: "http",
		Source: domain.Source{
			CurrentURL:    "https://example.com/file.iso?sig=1",
			PageURL:       "https://example.com/page",
			Referrer:      "https://example.com/",
			Body:          []byte("a=b"),
			Headers:       map[string]string{"X-Test": "1"},
			UserAgent:     "ua",
			CookieDomains: []string{"example.com"},
			MediaHint:     "720p",
			Fingerprint: domain.Fingerprint{
				Size: 1000, ETag: `"abc"`, LastModified: &lm, Filename: "file.iso", ContentType: "application/octet-stream",
			},
		},
		Settings: settings.Settings{
			DownloadDir:    settings.Some(filepath.Join(t.TempDir(), "dl")),
			MaxConnections: settings.Some(8),
			Headers:        settings.Some(map[string]string{"A": "b"}),
			Checksum:       settings.Some(settings.Checksum{Algorithm: settings.ChecksumSHA256, Expected: strings.Repeat("ab", 32)}),
			Retry:          settings.RetrySettings{MaxAttempts: settings.Some(3)},
		},
		Priority: domain.PriorityHigh,
		Labels:   map[string]string{"k": "v"},
	})
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	task.Dir = "/tmp/dl"
	task.Size = 1000
	task.Downloaded = 10
	task.Resource = &domain.Resource{
		Name: "file.iso", Size: 1000, RangeSupported: true,
		Files: []domain.File{{Index: 0, Name: "file.iso", Size: 1000, Selected: true}},
	}
	if err := task.TransitionTo(domain.StatusRunning, baseTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := task.SetError(domain.TaskError{Code: domain.ErrorCodeNetwork, Message: "boom", Retryable: true}, baseTime.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	return task
}

// truncTask returns the task as storage returns it: top-level timestamps
// truncated to the millisecond (JSON-embedded times keep full precision).
func truncTask(t *domain.Task) *domain.Task {
	c := *t
	c.CreatedAt = c.CreatedAt.Truncate(time.Millisecond)
	c.UpdatedAt = c.UpdatedAt.Truncate(time.Millisecond)
	if c.StartedAt != nil {
		v := c.StartedAt.Truncate(time.Millisecond)
		c.StartedAt = &v
	}
	if c.CompletedAt != nil {
		v := c.CompletedAt.Truncate(time.Millisecond)
		c.CompletedAt = &v
	}
	return &c
}

func truncQueue(q *domain.Queue) *domain.Queue {
	c := *q
	c.CreatedAt = c.CreatedAt.Truncate(time.Millisecond)
	c.UpdatedAt = c.UpdatedAt.Truncate(time.Millisecond)
	return &c
}

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dlm.db")
	for i := range 3 {
		s, err := sqlite.Open(t.Context(), path, sqlite.Options{Clock: fixedClock})
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		qs, err := s.Queues().List(t.Context())
		if err != nil {
			t.Fatalf("List #%d: %v", i, err)
		}
		if len(qs) != 1 || qs[0].ID != domain.MainQueueID {
			t.Fatalf("open #%d: queues = %+v, want only Main", i, qs)
		}
		want := truncQueue(domain.NewMainQueue(baseTime))
		if diff := cmp.Diff(want, qs[0], allowUnexported); diff != "" {
			t.Errorf("main queue mismatch (-want +got):\n%s", diff)
		}
		if err := s.EnsureDefaults(t.Context()); err != nil {
			t.Fatalf("EnsureDefaults: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

func TestOpenInvalidPath(t *testing.T) {
	if _, err := sqlite.Open(t.Context(), "", sqlite.Options{}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}

func TestGlobalSettings(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	got, err := s.Settings().GetGlobal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(settings.Settings{}, got, allowUnexported); diff != "" {
		t.Fatalf("unsaved settings not empty:\n%s", diff)
	}
	for _, want := range []settings.Settings{
		settings.Defaults("/downloads"),
		{SpeedLimit: settings.Some[int64](0), Proxy: settings.Some(settings.Proxy{Mode: settings.ProxyNone})},
	} {
		if err := s.Settings().SaveGlobal(ctx, want); err != nil {
			t.Fatal(err)
		}
		got, err := s.Settings().GetGlobal(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, got, allowUnexported); diff != "" {
			t.Errorf("settings round trip (-want +got):\n%s", diff)
		}
	}
}

func TestQueueCRUD(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	repo := s.Queues()

	pos, err := repo.NextPosition(ctx)
	if err != nil || pos != 1 {
		t.Fatalf("NextPosition = %d, %v; want 1", pos, err)
	}
	q := newQueue(t, "q1", "Fast", pos)
	q.Settings.Schedule = settings.Some(settings.Schedule{
		Timezone: "UTC",
		Windows:  []settings.TimeWindow{{Days: []settings.Weekday{1, 2}, Start: "01:00", End: "05:00"}},
	})
	if err := repo.Create(ctx, q); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.Get(ctx, "q1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(truncQueue(q), got, allowUnexported); diff != "" {
		t.Errorf("Get (-want +got):\n%s", diff)
	}

	t.Run("duplicate id", func(t *testing.T) {
		err := repo.Create(ctx, newQueue(t, "q1", "Other", 5))
		if !errors.Is(err, domain.ErrAlreadyExists) {
			t.Fatalf("err = %v, want ErrAlreadyExists", err)
		}
	})
	t.Run("duplicate name case-insensitive", func(t *testing.T) {
		err := repo.Create(ctx, newQueue(t, "q2", "fAST", 5))
		if !errors.Is(err, domain.ErrAlreadyExists) {
			t.Fatalf("err = %v, want ErrAlreadyExists", err)
		}
	})

	q.Stop(baseTime.Add(time.Minute))
	if err := q.Rename("Faster", baseTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	q.Position = 7
	if err := repo.Update(ctx, q); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = repo.Get(ctx, "q1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(truncQueue(q), got, allowUnexported); diff != "" {
		t.Errorf("after Update (-want +got):\n%s", diff)
	}

	t.Run("update rename to existing name", func(t *testing.T) {
		main, err := repo.Get(ctx, domain.MainQueueID)
		if err != nil {
			t.Fatal(err)
		}
		main.Name = "faster"
		if err := repo.Update(ctx, main); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Fatalf("err = %v, want ErrAlreadyExists", err)
		}
	})

	q2 := newQueue(t, "q2", "Slow", 3)
	if err := repo.Create(ctx, q2); err != nil {
		t.Fatal(err)
	}
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, q := range list {
		ids = append(ids, q.ID)
	}
	if diff := cmp.Diff([]string{domain.MainQueueID, "q2", "q1"}, ids); diff != "" {
		t.Errorf("List order (-want +got):\n%s", diff)
	}

	if err := repo.Delete(ctx, "q2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for name, err := range map[string]error{
		"get":    func() error { _, err := repo.Get(ctx, "q2"); return err }(),
		"delete": repo.Delete(ctx, "q2"),
		"update": repo.Update(ctx, q2),
	} {
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s missing queue: err = %v, want ErrNotFound", name, err)
		}
	}
}

func TestDeleteQueueWithTasksFails(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	q := newQueue(t, "q1", "Fast", 1)
	if err := s.Queues().Create(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err := s.Tasks().Create(ctx, newTask(t, "t1", "q1", "a", "https://e.com/a")); err != nil {
		t.Fatal(err)
	}
	err := s.Queues().Delete(ctx, "q1")
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, err := s.Queues().Get(ctx, "q1"); err != nil {
		t.Fatalf("queue gone after failed delete: %v", err)
	}
}

func TestTaskCRUD(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	repo := s.Tasks()

	minimal := newTask(t, "t-min", "", "min", "https://e.com/min")
	full := fullTask(t, "t-full")
	for _, task := range []*domain.Task{minimal, full} {
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("Create %s: %v", task.ID, err)
		}
		got, err := repo.Get(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(truncTask(task), got, allowUnexported); diff != "" {
			t.Errorf("Get %s (-want +got):\n%s", task.ID, diff)
		}
	}

	t.Run("duplicate", func(t *testing.T) {
		if err := repo.Create(ctx, minimal); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Fatalf("err = %v, want ErrAlreadyExists", err)
		}
	})
	t.Run("missing queue", func(t *testing.T) {
		if err := repo.Create(ctx, newTask(t, "t-x", "nope", "x", "https://e.com/x")); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	// Full update: clear optional fields, change status and queue.
	q := newQueue(t, "q1", "Other", 1)
	if err := s.Queues().Create(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err := full.TransitionTo(domain.StatusQueued, baseTime.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	full.QueueID = "q1"
	full.Resource = nil
	full.Labels = nil
	full.Settings = settings.Settings{}
	full.Position = 42
	if err := repo.Update(ctx, full); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := repo.Get(ctx, full.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(truncTask(full), got, allowUnexported); diff != "" {
		t.Errorf("after Update (-want +got):\n%s", diff)
	}

	t.Run("progress", func(t *testing.T) {
		at := baseTime.Add(time.Hour)
		if err := repo.UpdateProgress(ctx, full.ID, 500, 2000, at); err != nil {
			t.Fatal(err)
		}
		got, err := repo.Get(ctx, full.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Downloaded != 500 || got.Size != 2000 || !got.UpdatedAt.Equal(at.Truncate(time.Millisecond)) {
			t.Fatalf("progress not saved: downloaded=%d size=%d updatedAt=%v", got.Downloaded, got.Size, got.UpdatedAt)
		}
		if got.Name != full.Name || got.Status != full.Status {
			t.Fatalf("UpdateProgress changed other columns: %+v", got)
		}
	})

	t.Run("counts and positions", func(t *testing.T) {
		all, err := repo.CountByStatus(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(map[domain.TaskStatus]int{domain.StatusQueued: 2}, all); diff != "" {
			t.Errorf("CountByStatus all (-want +got):\n%s", diff)
		}
		inQ, err := repo.CountByStatus(ctx, "q1")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(map[domain.TaskStatus]int{domain.StatusQueued: 1}, inQ); diff != "" {
			t.Errorf("CountByStatus q1 (-want +got):\n%s", diff)
		}
		for queueID, want := range map[string]int64{"q1": 43, domain.MainQueueID: 1, "empty": 0} {
			got, err := repo.NextPosition(ctx, queueID)
			if err != nil || got != want {
				t.Errorf("NextPosition(%s) = %d, %v; want %d", queueID, got, err, want)
			}
		}
	})

	if err := repo.Delete(ctx, full.ID); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"get":      func() error { _, err := repo.Get(ctx, full.ID); return err }(),
		"delete":   repo.Delete(ctx, full.ID),
		"update":   repo.Update(ctx, full),
		"progress": repo.UpdateProgress(ctx, full.ID, 1, 1, baseTime),
	} {
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s missing task: err = %v, want ErrNotFound", name, err)
		}
	}
}

func TestSegmentsAndEngineState(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	task := newTask(t, "t1", "", "a", "https://e.com/a")
	if err := s.Tasks().Create(ctx, task); err != nil {
		t.Fatal(err)
	}

	segs, err := s.Segments().List(ctx, "t1")
	if err != nil || len(segs) != 0 {
		t.Fatalf("List empty = %v, %v", segs, err)
	}
	first := []domain.Segment{{Index: 0, Start: 0, End: 100, Done: 10}, {Index: 1, Start: 100, End: 200, Done: 0}}
	if err := s.Segments().Replace(ctx, "t1", first); err != nil {
		t.Fatal(err)
	}
	second := []domain.Segment{{Index: 0, Start: 0, End: 50, Done: 50}, {Index: 1, Start: 50, End: 100, Done: 5}, {Index: 2, Start: 100, End: 200, Done: 7}}
	if err := s.Segments().Replace(ctx, "t1", second); err != nil {
		t.Fatal(err)
	}
	segs, err = s.Segments().List(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(second, segs); diff != "" {
		t.Errorf("segments (-want +got):\n%s", diff)
	}

	t.Run("replace is atomic", func(t *testing.T) {
		bad := []domain.Segment{{Index: 0, Start: 0, End: 10}, {Index: 0, Start: 10, End: 20}}
		if err := s.Segments().Replace(ctx, "t1", bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Fatalf("err = %v, want ErrInvalidArgument", err)
		}
		segs, err := s.Segments().List(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(second, segs); diff != "" {
			t.Errorf("segments changed by failed replace (-want +got):\n%s", diff)
		}
	})
	t.Run("missing task", func(t *testing.T) {
		if err := s.Segments().Replace(ctx, "nope", first); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if err := s.EngineState().Put(ctx, "nope", []byte("x")); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	es := s.EngineState()
	if _, err := es.Get(ctx, "t1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get absent: err = %v, want ErrNotFound", err)
	}
	for _, state := range [][]byte{[]byte("v1"), {0, 1, 2, 255}} {
		if err := es.Put(ctx, "t1", state); err != nil {
			t.Fatal(err)
		}
		got, err := es.Get(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(state, got); diff != "" {
			t.Errorf("engine state (-want +got):\n%s", diff)
		}
	}
	if err := es.Delete(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := es.Delete(ctx, "t1"); err != nil {
		t.Fatalf("Delete is not idempotent: %v", err)
	}
	if _, err := es.Get(ctx, "t1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete: err = %v, want ErrNotFound", err)
	}

	t.Run("cascade on task delete", func(t *testing.T) {
		if err := es.Put(ctx, "t1", []byte("s")); err != nil {
			t.Fatal(err)
		}
		if err := s.Tasks().Delete(ctx, "t1"); err != nil {
			t.Fatal(err)
		}
		segs, err := s.Segments().List(ctx, "t1")
		if err != nil || len(segs) != 0 {
			t.Fatalf("segments after task delete = %v, %v", segs, err)
		}
		if _, err := es.Get(ctx, "t1"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("engine state after task delete: err = %v", err)
		}
	})
}

func TestWithTx(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()

	t.Run("commit", func(t *testing.T) {
		err := s.WithTx(ctx, func(tx storage.Store) error {
			if err := tx.Queues().Create(ctx, newQueue(t, "q1", "One", 1)); err != nil {
				return err
			}
			// Nested WithTx reuses the transaction and sees its writes.
			return tx.WithTx(ctx, func(inner storage.Store) error {
				if _, err := inner.Queues().Get(ctx, "q1"); err != nil {
					return err
				}
				return inner.Tasks().Create(ctx, newTask(t, "t1", "q1", "a", "https://e.com/a"))
			})
		})
		if err != nil {
			t.Fatalf("WithTx: %v", err)
		}
		if _, err := s.Tasks().Get(ctx, "t1"); err != nil {
			t.Fatalf("committed task missing: %v", err)
		}
	})

	t.Run("rollback on error", func(t *testing.T) {
		sentinel := errors.New("stop")
		err := s.WithTx(ctx, func(tx storage.Store) error {
			if err := tx.Queues().Create(ctx, newQueue(t, "q2", "Two", 2)); err != nil {
				return err
			}
			if err := tx.Tasks().Create(ctx, newTask(t, "t2", "q2", "b", "https://e.com/b")); err != nil {
				return err
			}
			if err := tx.Settings().SaveGlobal(ctx, settings.Defaults("/x")); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want sentinel", err)
		}
		if _, err := s.Queues().Get(ctx, "q2"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("queue persisted after rollback: %v", err)
		}
		if _, err := s.Tasks().Get(ctx, "t2"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("task persisted after rollback: %v", err)
		}
		got, err := s.Settings().GetGlobal(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(settings.Settings{}, got, allowUnexported); diff != "" {
			t.Errorf("settings persisted after rollback:\n%s", diff)
		}
	})

	t.Run("rollback on panic", func(t *testing.T) {
		func() {
			defer func() { _ = recover() }()
			_ = s.WithTx(ctx, func(tx storage.Store) error {
				if err := tx.Queues().Create(ctx, newQueue(t, "q3", "Three", 3)); err != nil {
					return err
				}
				panic("boom")
			})
		}()
		if _, err := s.Queues().Get(ctx, "q3"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("queue persisted after panic: %v", err)
		}
		// The writer connection must be usable again.
		if err := s.Queues().Create(ctx, newQueue(t, "q4", "Four", 4)); err != nil {
			t.Fatalf("write after panic: %v", err)
		}
	})
}

func TestConcurrentWrites(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	const workers, perWorker = 8, 25

	tasks := make([][]*domain.Task, workers)
	for w := range workers {
		for i := range perWorker {
			id := fmt.Sprintf("t-%d-%d", w, i)
			tasks[w] = append(tasks[w], newTask(t, id, "", id, "https://e.com/"+id))
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, 4*workers*perWorker)
	for w := range workers {
		wg.Go(func() {
			for _, task := range tasks[w] {
				id := task.ID
				if err := s.Tasks().Create(ctx, task); err != nil {
					errs <- err
					continue
				}
				if err := s.Tasks().UpdateProgress(ctx, id, 1, 10, baseTime); err != nil {
					errs <- err
				}
				if err := s.WithTx(ctx, func(tx storage.Store) error {
					return tx.Segments().Replace(ctx, id, []domain.Segment{{Index: 0, Start: 0, End: 10, Done: 1}})
				}); err != nil {
					errs <- err
				}
				if _, err := s.Tasks().List(ctx, storage.TaskFilter{Limit: 5}); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write: %v", err)
	}
	counts, err := s.Tasks().CountByStatus(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if counts[domain.StatusQueued] != workers*perWorker {
		t.Fatalf("queued = %d, want %d", counts[domain.StatusQueued], workers*perWorker)
	}
}

func TestContextCancelled(t *testing.T) {
	s := openStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Queues().List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
