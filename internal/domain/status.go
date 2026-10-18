package domain

import (
	"slices"
	"time"
)

// TaskStatus is the lifecycle state of a task. Values are lower-case strings
// in JSON and storage.
type TaskStatus string

// Task statuses.
const (
	// StatusQueued: waiting in its queue for a scheduler slot.
	StatusQueued TaskStatus = "queued"
	// StatusRunning: an engine is transferring data.
	StatusRunning TaskStatus = "running"
	// StatusPaused: stopped by the user; never started automatically.
	StatusPaused TaskStatus = "paused"
	// StatusAwaitingLink: the link expired and the task waits for a new one
	// (extension capture or a manual URL update). A failed task can also be
	// put here to wait for a new link.
	StatusAwaitingLink TaskStatus = "awaiting_link"
	// StatusRefreshing: a refresh strategy is obtaining a new link.
	StatusRefreshing TaskStatus = "refreshing"
	// StatusPostprocessing: the download finished and post-actions run.
	StatusPostprocessing TaskStatus = "postprocessing"
	// StatusCompleted: done; files are in place.
	StatusCompleted TaskStatus = "completed"
	// StatusError: failed; Task.Error describes why.
	StatusError TaskStatus = "error"
)

// transitions is the task status state machine: from → allowed targets.
// It is the single source of truth for status changes and is never mutated.
var transitions = map[TaskStatus][]TaskStatus{
	StatusQueued:         {StatusRunning, StatusPaused, StatusError},
	StatusRunning:        {StatusQueued, StatusPaused, StatusAwaitingLink, StatusRefreshing, StatusPostprocessing, StatusError},
	StatusRefreshing:     {StatusQueued, StatusRunning, StatusAwaitingLink, StatusPaused, StatusError},
	StatusAwaitingLink:   {StatusQueued, StatusRefreshing, StatusPaused, StatusError},
	StatusPostprocessing: {StatusCompleted, StatusQueued, StatusError},
	StatusPaused:         {StatusQueued},
	StatusError:          {StatusQueued, StatusPaused, StatusAwaitingLink},
	StatusCompleted:      {StatusQueued},
}

// AllStatuses lists every valid task status.
func AllStatuses() []TaskStatus {
	return []TaskStatus{
		StatusQueued, StatusRunning, StatusPaused, StatusAwaitingLink,
		StatusRefreshing, StatusPostprocessing, StatusCompleted, StatusError,
	}
}

// Valid reports whether s is a known status.
func (s TaskStatus) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// Terminal reports whether s is a final state (only completed).
func (s TaskStatus) Terminal() bool { return s == StatusCompleted }

// Active reports whether a task in state s holds a scheduler slot.
func (s TaskStatus) Active() bool {
	switch s {
	case StatusRunning, StatusRefreshing, StatusPostprocessing:
		return true
	default:
		return false
	}
}

// CanTransition reports whether the state machine allows from → to.
func CanTransition(from, to TaskStatus) bool {
	return slices.Contains(transitions[from], to)
}

// TransitionTo moves the task to status to. It is the only way to change
// Task.Status. Side effects on success:
//   - UpdatedAt is set to now;
//   - StartedAt is set on the first transition to running;
//   - CompletedAt is set on transition to completed and cleared when a
//     completed task is queued again;
//   - Error is cleared when leaving error for queued or running.
//
// A disallowed transition returns an error matching ErrInvalidTransition
// (and ErrConflict) and leaves the task unchanged.
func (t *Task) TransitionTo(to TaskStatus, now time.Time) error {
	from := t.Status
	if !CanTransition(from, to) {
		return invalidTransition(t.ID, from, to)
	}
	now = now.UTC()
	t.Status = to
	t.UpdatedAt = now
	switch to {
	case StatusRunning:
		if t.StartedAt == nil {
			t.StartedAt = &now
		}
	case StatusCompleted:
		t.CompletedAt = &now
	case StatusQueued:
		if from == StatusCompleted {
			t.CompletedAt = nil
		}
	default:
	}
	if from == StatusError && (to == StatusQueued || to == StatusRunning) {
		t.Error = nil
	}
	return nil
}

// SetError transitions the task to the error state and records e. When e.At
// is zero it is set to now. It returns an error matching
// ErrInvalidArgument when e.Code is empty, or ErrInvalidTransition when the
// current status cannot move to error.
func (t *Task) SetError(e TaskError, now time.Time) error {
	if e.Code == "" {
		return InvalidArgument("task %q: error code is required", t.ID)
	}
	if err := t.TransitionTo(StatusError, now); err != nil {
		return err
	}
	if e.At.IsZero() {
		e.At = now
	}
	e.At = e.At.UTC()
	t.Error = &e
	return nil
}
