package domain

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

var (
	t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Minute)
	t2 = t0.Add(2 * time.Minute)
)

// expected is written independently of the production table so the test
// reviews it rather than mirroring it.
var expected = map[TaskStatus]map[TaskStatus]bool{
	StatusQueued:         {StatusRunning: true, StatusPaused: true, StatusError: true},
	StatusRunning:        {StatusQueued: true, StatusPaused: true, StatusAwaitingLink: true, StatusRefreshing: true, StatusPostprocessing: true, StatusError: true},
	StatusRefreshing:     {StatusQueued: true, StatusRunning: true, StatusAwaitingLink: true, StatusPaused: true, StatusError: true},
	StatusAwaitingLink:   {StatusQueued: true, StatusRefreshing: true, StatusPaused: true, StatusError: true},
	StatusPostprocessing: {StatusCompleted: true, StatusError: true},
	StatusPaused:         {StatusQueued: true},
	StatusError:          {StatusQueued: true, StatusPaused: true},
	StatusCompleted:      {StatusQueued: true},
}

func newTestTask(t *testing.T, status TaskStatus) *Task {
	t.Helper()
	task, err := NewTask("task-1", t0, NewTaskParams{Source: Source{CurrentURL: "https://example.com/f.zip"}})
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	task.Status = status // test setup only; production code uses TransitionTo
	return task
}

func TestTransitionTableComplete(t *testing.T) {
	all := append(AllStatuses(), TaskStatus("bogus"), TaskStatus(""))
	for _, from := range all {
		for _, to := range all {
			want := expected[from][to]
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				if got := CanTransition(from, to); got != want {
					t.Fatalf("CanTransition(%q, %q) = %v, want %v", from, to, got, want)
				}
				task := newTestTask(t, from)
				err := task.TransitionTo(to, t1)
				if want {
					if err != nil {
						t.Fatalf("TransitionTo: unexpected error %v", err)
					}
					if task.Status != to || !task.UpdatedAt.Equal(t1) {
						t.Fatalf("status=%q updatedAt=%v, want %q %v", task.Status, task.UpdatedAt, to, t1)
					}
					return
				}
				if !errors.Is(err, ErrInvalidTransition) || !errors.Is(err, ErrConflict) {
					t.Fatalf("err = %v, want ErrInvalidTransition and ErrConflict", err)
				}
				if task.Status != from || !task.UpdatedAt.Equal(t0) {
					t.Fatalf("task changed on rejected transition: status=%q updatedAt=%v", task.Status, task.UpdatedAt)
				}
			})
		}
	}
	if len(transitions) != len(expected) {
		t.Fatalf("table has %d states, want %d", len(transitions), len(expected))
	}
}

func TestStatusPredicates(t *testing.T) {
	tests := []struct {
		s                       TaskStatus
		valid, terminal, active bool
	}{
		{StatusQueued, true, false, false},
		{StatusRunning, true, false, true},
		{StatusPaused, true, false, false},
		{StatusAwaitingLink, true, false, false},
		{StatusRefreshing, true, false, true},
		{StatusPostprocessing, true, false, true},
		{StatusCompleted, true, true, false},
		{StatusError, true, false, false},
		{"bogus", false, false, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.s), func(t *testing.T) {
			if tt.s.Valid() != tt.valid || tt.s.Terminal() != tt.terminal || tt.s.Active() != tt.active {
				t.Fatalf("Valid/Terminal/Active = %v/%v/%v, want %v/%v/%v",
					tt.s.Valid(), tt.s.Terminal(), tt.s.Active(), tt.valid, tt.terminal, tt.active)
			}
		})
	}
}

func TestTransitionTimestamps(t *testing.T) {
	task := newTestTask(t, StatusQueued)
	if task.StartedAt != nil || task.CompletedAt != nil {
		t.Fatal("new task must have no StartedAt/CompletedAt")
	}
	steps := []struct {
		to          TaskStatus
		at          time.Time
		wantStarted *time.Time
		wantDone    *time.Time
	}{
		{StatusRunning, t1, &t1, nil},
		{StatusPaused, t1.Add(time.Second), &t1, nil},
		{StatusQueued, t1.Add(2 * time.Second), &t1, nil},
		{StatusRunning, t2, &t1, nil}, // StartedAt keeps the first start
		{StatusPostprocessing, t2.Add(time.Second), &t1, nil},
		{StatusCompleted, t2.Add(2 * time.Second), &t1, ptr(t2.Add(2 * time.Second))},
		{StatusQueued, t2.Add(3 * time.Second), &t1, nil}, // restart clears CompletedAt
	}
	for i, s := range steps {
		if err := task.TransitionTo(s.to, s.at); err != nil {
			t.Fatalf("step %d -> %s: %v", i, s.to, err)
		}
		if !eqTimePtr(task.StartedAt, s.wantStarted) || !eqTimePtr(task.CompletedAt, s.wantDone) || !task.UpdatedAt.Equal(s.at) {
			t.Fatalf("step %d -> %s: startedAt=%v completedAt=%v updatedAt=%v", i, s.to, task.StartedAt, task.CompletedAt, task.UpdatedAt)
		}
	}
}

func TestTransitionStoresUTC(t *testing.T) {
	task := newTestTask(t, StatusQueued)
	loc := time.FixedZone("X", 3600)
	if err := task.TransitionTo(StatusRunning, t1.In(loc)); err != nil {
		t.Fatal(err)
	}
	if task.UpdatedAt.Location() != time.UTC || task.StartedAt.Location() != time.UTC {
		t.Fatal("timestamps must be stored in UTC")
	}
}

func TestSetError(t *testing.T) {
	t.Run("records error", func(t *testing.T) {
		task := newTestTask(t, StatusRunning)
		e := TaskError{Code: ErrorCodeNetwork, Message: "reset", Retryable: true}
		if err := task.SetError(e, t1); err != nil {
			t.Fatal(err)
		}
		if task.Status != StatusError || task.Error == nil || task.Error.Code != ErrorCodeNetwork || !task.Error.At.Equal(t1) {
			t.Fatalf("got status=%q error=%+v", task.Status, task.Error)
		}
	})
	t.Run("keeps explicit At", func(t *testing.T) {
		task := newTestTask(t, StatusRunning)
		if err := task.SetError(TaskError{Code: ErrorCodeFatal, At: t0}, t1); err != nil {
			t.Fatal(err)
		}
		if !task.Error.At.Equal(t0) {
			t.Fatalf("At = %v, want %v", task.Error.At, t0)
		}
	})
	t.Run("empty code rejected", func(t *testing.T) {
		task := newTestTask(t, StatusRunning)
		if err := task.SetError(TaskError{}, t1); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("err = %v, want ErrInvalidArgument", err)
		}
		if task.Status != StatusRunning {
			t.Fatal("status changed")
		}
	})
	t.Run("not allowed from paused", func(t *testing.T) {
		task := newTestTask(t, StatusPaused)
		err := task.SetError(TaskError{Code: ErrorCodeFatal}, t1)
		if !errors.Is(err, ErrInvalidTransition) || task.Error != nil {
			t.Fatalf("err = %v, error = %+v", err, task.Error)
		}
	})
}

func TestLeavingErrorState(t *testing.T) {
	tests := []struct {
		to        TaskStatus
		wantClear bool
	}{
		{StatusQueued, true},
		{StatusPaused, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.to), func(t *testing.T) {
			task := newTestTask(t, StatusRunning)
			if err := task.SetError(TaskError{Code: ErrorCodeDiskFull}, t1); err != nil {
				t.Fatal(err)
			}
			if err := task.TransitionTo(tt.to, t2); err != nil {
				t.Fatal(err)
			}
			if (task.Error == nil) != tt.wantClear {
				t.Fatalf("Error = %+v, wantClear %v", task.Error, tt.wantClear)
			}
		})
	}
}

func TestStatusJSONRoundTrip(t *testing.T) {
	for _, s := range AllStatuses() {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var got TaskStatus
		if err := json.Unmarshal(b, &got); err != nil || got != s {
			t.Fatalf("round trip %q: got %q, %v (json %s)", s, got, err, b)
		}
	}
	if b, _ := json.Marshal(StatusAwaitingLink); string(b) != `"awaiting_link"` {
		t.Fatalf("json = %s", b)
	}
}

func ptr[T any](v T) *T { return &v }

func eqTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
