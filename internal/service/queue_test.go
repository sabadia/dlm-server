package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/settings"
)

func TestQueueServiceCRUD(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	sub := f.bus.Subscribe(ctx, events.Filter{})

	q, err := f.queues.Create(ctx, CreateQueueInput{Name: " Fast ", Settings: settings.Settings{SpeedLimit: settings.Some[int64](1000)}})
	if err != nil {
		t.Fatal(err)
	}
	if q.Name != "Fast" || q.Position != 1 || q.State != domain.QueueRunning || q.IsDefault {
		t.Errorf("created = %+v", q)
	}
	if f.limits.Queue(q.ID).Limit() != 1000 {
		t.Errorf("queue limit = %d", f.limits.Queue(q.ID).Limit())
	}
	stopped, err := f.queues.Create(ctx, CreateQueueInput{Name: "later", State: domain.QueueStopped})
	if err != nil || stopped.State != domain.QueueStopped || stopped.Position != 2 {
		t.Fatalf("stopped queue = %+v, %v", stopped, err)
	}

	for name, in := range map[string]CreateQueueInput{
		"empty name":     {Name: "  "},
		"unknown state":  {Name: "x", State: "paused"},
		"queue settings": {Name: "y", Settings: settings.Settings{Checksum: settings.Some(settings.Checksum{Algorithm: settings.ChecksumMD5, Expected: "00"})}},
	} {
		if _, err := f.queues.Create(ctx, in); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	_, err = f.queues.Create(ctx, CreateQueueInput{Name: "FAST"})
	wantErr(t, err, domain.ErrAlreadyExists)

	got, err := f.queues.Get(ctx, q.ID)
	if err != nil || got.Name != "Fast" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	_, err = f.queues.Get(ctx, "nope")
	wantErr(t, err, domain.ErrNotFound)
	list, err := f.queues.List(ctx)
	if err != nil || len(list) != 3 || list[0].ID != domain.MainQueueID {
		t.Fatalf("List = %v, %v", list, err)
	}

	name := "Faster"
	up, err := f.queues.Update(ctx, q.ID, UpdateQueueInput{Name: &name, SettingsPatch: []byte(`{"speedLimit":5000,"maxConcurrentDownloads":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	if up.Name != "Faster" || up.Settings.SpeedLimit.Value() != 5000 || up.Settings.MaxConcurrentDownloads.Value() != 2 {
		t.Errorf("updated = %+v", up)
	}
	if f.limits.Queue(q.ID).Limit() != 5000 {
		t.Errorf("queue limit = %d", f.limits.Queue(q.ID).Limit())
	}
	repl := settings.Settings{UserAgent: settings.Some("ua")}
	up, err = f.queues.Update(ctx, q.ID, UpdateQueueInput{Settings: &repl})
	if err != nil || up.Settings.SpeedLimit.IsSet() || up.Settings.UserAgent.Value() != "ua" {
		t.Fatalf("replace settings = %+v, %v", up, err)
	}
	if f.limits.Queue(q.ID).Limit() != 0 {
		t.Errorf("queue limit after replace = %d", f.limits.Queue(q.ID).Limit())
	}
	_, err = f.queues.Update(ctx, q.ID, UpdateQueueInput{Settings: &repl, SettingsPatch: []byte(`{}`)})
	wantErr(t, err, domain.ErrInvalidArgument)
	_, err = f.queues.Update(ctx, q.ID, UpdateQueueInput{SettingsPatch: []byte(`{"checksum":{"algorithm":"md5","expected":"00"}}`)})
	wantErr(t, err, domain.ErrInvalidArgument)
	dup := "later"
	_, err = f.queues.Update(ctx, q.ID, UpdateQueueInput{Name: &dup})
	wantErr(t, err, domain.ErrAlreadyExists)
	_, err = f.queues.Update(ctx, "nope", UpdateQueueInput{Name: &name})
	wantErr(t, err, domain.ErrNotFound)

	want := []events.Type{events.TypeQueueCreated, events.TypeQueueCreated, events.TypeQueueUpdated, events.TypeQueueUpdated}
	if got := eventTypes(collect(sub)); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func TestQueueServiceDelete(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()

	err := f.queues.Delete(ctx, domain.MainQueueID, DeleteQueueOptions{})
	wantErr(t, err, domain.ErrConflict)
	err = f.queues.Delete(ctx, "nope", DeleteQueueOptions{})
	wantErr(t, err, domain.ErrNotFound)

	empty, err := f.queues.Create(ctx, CreateQueueInput{Name: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	sub := f.bus.Subscribe(ctx, events.Filter{Types: []events.Type{events.TypeQueueDeleted}})
	if err := f.queues.Delete(ctx, empty.ID, DeleteQueueOptions{}); err != nil {
		t.Fatal(err)
	}
	if evs := collect(sub); len(evs) != 1 || evs[0].QueueID != empty.ID {
		t.Errorf("delete events = %v", evs)
	}

	full, err := f.queues.Create(ctx, CreateQueueInput{Name: "full"})
	if err != nil {
		t.Fatal(err)
	}
	mainTask := f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: "http://example.com/m"}})
	var ids []string
	for i := range 3 {
		ids = append(ids, f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: fmt.Sprintf("http://example.com/%d", i)}, QueueID: full.ID}).ID)
	}
	// Put the last task first so "order" means positions, not creation.
	if _, err := f.tasks.Reorder(ctx, ids[2], 0); err != nil {
		t.Fatal(err)
	}
	order := []string{ids[2], ids[0], ids[1]}

	err = f.queues.Delete(ctx, full.ID, DeleteQueueOptions{})
	wantErr(t, err, domain.ErrConflict)
	err = f.queues.Delete(ctx, full.ID, DeleteQueueOptions{MoveTasksTo: full.ID})
	wantErr(t, err, domain.ErrInvalidArgument)
	err = f.queues.Delete(ctx, full.ID, DeleteQueueOptions{MoveTasksTo: "nope"})
	wantErr(t, err, domain.ErrNotFound)

	wakes := f.sched.wakes.Load()
	if err := f.queues.Delete(ctx, full.ID, DeleteQueueOptions{MoveTasksTo: domain.MainQueueID}); err != nil {
		t.Fatal(err)
	}
	if f.sched.wakes.Load() == wakes {
		t.Error("moving tasks did not wake the scheduler")
	}
	_, err = f.queues.Get(ctx, full.ID)
	wantErr(t, err, domain.ErrNotFound)
	if got := f.get(t, mainTask.ID); got.Position != 0 {
		t.Errorf("main task position = %d", got.Position)
	}
	for i, id := range order {
		got := f.get(t, id)
		if got.QueueID != domain.MainQueueID || got.Position != int64(i+1) {
			t.Errorf("moved task %d: queue %s position %d, want main/%d", i, got.QueueID, got.Position, i+1)
		}
	}
}

func TestQueueServiceStartStop(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	srv := newFileServer(t, 2<<20, 5*time.Millisecond)
	q, err := f.queues.Create(ctx, CreateQueueInput{Name: "q"})
	if err != nil {
		t.Fatal(err)
	}
	running := f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: srv.URL + "/files/a.bin"}, QueueID: q.ID})
	queued := f.create(t, CreateTaskInput{Source: domain.Source{CurrentURL: srv.URL + "/files/b.bin"}, QueueID: q.ID})
	f.sched.run(t, f.store, running.ID)

	stopped, err := f.queues.Stop(ctx, q.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != domain.QueueStopped {
		t.Errorf("state = %s", stopped.State)
	}
	calls := f.sched.stopCalls()
	if len(calls) != 1 || calls[0].TaskID != running.ID || !errors.Is(calls[0].Cause, ErrRequeued) {
		t.Fatalf("stop calls = %+v", calls)
	}
	if got := f.get(t, running.ID); got.Status != domain.StatusQueued {
		t.Errorf("running task status = %s, want queued", got.Status)
	}
	if got := f.get(t, queued.ID); got.Status != domain.StatusQueued {
		t.Errorf("queued task status = %s", got.Status)
	}
	// Stopping again is idempotent.
	if again, err := f.queues.Stop(ctx, q.ID); err != nil || again.State != domain.QueueStopped {
		t.Fatalf("second stop = %+v, %v", again, err)
	}

	wakes := f.sched.wakes.Load()
	started, err := f.queues.Start(ctx, q.ID)
	if err != nil || started.State != domain.QueueRunning {
		t.Fatalf("Start = %+v, %v", started, err)
	}
	if f.sched.wakes.Load() != wakes+1 {
		t.Error("Start did not wake the scheduler")
	}
	_, err = f.queues.Start(ctx, "nope")
	wantErr(t, err, domain.ErrNotFound)
}

func TestQueueServiceReorder(t *testing.T) {
	f := newSvcFixture(t)
	ctx := t.Context()
	ids := []string{domain.MainQueueID}
	for _, n := range []string{"a", "b", "c"} {
		q, err := f.queues.Create(ctx, CreateQueueInput{Name: n})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, q.ID)
	}
	order := func() string {
		qs, err := f.queues.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var s string
		for i, q := range qs {
			if q.Position != int64(i) {
				t.Errorf("queue %s position %d at index %d", q.Name, q.Position, i)
			}
			s += q.Name + " "
		}
		return s
	}
	steps := []struct {
		id   string
		pos  int
		want string
	}{
		{ids[3], 0, "c Main a b "},
		{domain.MainQueueID, 99, "c a b Main "},
		{ids[1], 2, "c b a Main "},
		{ids[1], 2, "c b a Main "},
	}
	for _, st := range steps {
		if _, err := f.queues.Reorder(ctx, st.id, st.pos); err != nil {
			t.Fatal(err)
		}
		if got := order(); got != st.want {
			t.Errorf("after moving %s to %d: %q, want %q", st.id, st.pos, got, st.want)
		}
	}
	_, err := f.queues.Reorder(ctx, ids[1], -1)
	wantErr(t, err, domain.ErrInvalidArgument)
	_, err = f.queues.Reorder(ctx, "nope", 0)
	wantErr(t, err, domain.ErrNotFound)
}
