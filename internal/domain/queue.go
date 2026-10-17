package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sabadia/dlm-server/internal/settings"
)

// The built-in default queue. It always exists and cannot be deleted.
const (
	MainQueueID   = "main"
	MainQueueName = "Main"
)

// MaxQueueNameLength is the maximum queue name length in runes.
const MaxQueueNameLength = 100

// QueueState says whether a queue's scheduler admits new tasks.
type QueueState string

// Queue states.
const (
	QueueRunning QueueState = "running"
	QueueStopped QueueState = "stopped"
)

// Valid reports whether s is a known queue state.
func (s QueueState) Valid() bool { return s == QueueRunning || s == QueueStopped }

// Queue is an independently scheduled list of tasks with its own settings
// overrides (the queue level of the settings hierarchy, including its
// schedule).
type Queue struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	IsDefault bool       `json:"isDefault"`
	Position  int64      `json:"position"`
	State     QueueState `json:"state"`
	// Settings are this queue's overrides; unset fields inherit from global.
	Settings  settings.Settings `json:"settings"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

// NewMainQueue builds the built-in default queue (running, no overrides).
func NewMainQueue(now time.Time) *Queue {
	now = now.UTC()
	return &Queue{
		ID:        MainQueueID,
		Name:      MainQueueName,
		IsDefault: true,
		State:     QueueRunning,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// NewQueue builds a validated, running, non-default queue. The name is
// trimmed. Errors match ErrInvalidArgument.
func NewQueue(id, name string, now time.Time, s settings.Settings) (*Queue, error) {
	now = now.UTC()
	q := &Queue{
		ID:        id,
		Name:      strings.TrimSpace(name),
		State:     QueueRunning,
		Settings:  s,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := q.Validate(); err != nil {
		return nil, err
	}
	return q, nil
}

// Validate checks the queue's invariants. Settings errors match
// ErrInvalidArgument and unwrap to *settings.ValidationError.
func (q *Queue) Validate() error {
	if q.ID == "" {
		return InvalidArgument("queue: id is required")
	}
	if err := validateQueueName(q.Name); err != nil {
		return InvalidArgument("queue %q: %w", q.ID, err)
	}
	if !q.State.Valid() {
		return InvalidArgument("queue %q: unknown state %q", q.ID, q.State)
	}
	if err := q.Settings.Validate(settings.LevelQueue); err != nil {
		return invalidSettings("queue", q.ID, err)
	}
	return nil
}

func validateQueueName(name string) error {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return errors.New("name is required")
	case trimmed != name:
		return errors.New("name must not have leading or trailing spaces")
	case utf8.RuneCountInString(name) > MaxQueueNameLength:
		return fmt.Errorf("name must be at most %d characters", MaxQueueNameLength)
	default:
		return nil
	}
}

// Rename sets the queue's name (trimmed). Errors match ErrInvalidArgument.
func (q *Queue) Rename(name string, now time.Time) error {
	name = strings.TrimSpace(name)
	if err := validateQueueName(name); err != nil {
		return InvalidArgument("queue %q: %w", q.ID, err)
	}
	q.Name = name
	q.UpdatedAt = now.UTC()
	return nil
}

// SetSettings replaces the queue's overrides after validating them at the
// queue level. On error the queue is unchanged.
func (q *Queue) SetSettings(s settings.Settings, now time.Time) error {
	if err := s.Validate(settings.LevelQueue); err != nil {
		return invalidSettings("queue", q.ID, err)
	}
	q.Settings = s
	q.UpdatedAt = now.UTC()
	return nil
}

// Start lets the scheduler admit tasks from this queue. It is idempotent;
// UpdatedAt changes only when the state changes.
func (q *Queue) Start(now time.Time) { q.setState(QueueRunning, now) }

// Stop prevents the scheduler from admitting new tasks from this queue. It
// is idempotent; UpdatedAt changes only when the state changes.
func (q *Queue) Stop(now time.Time) { q.setState(QueueStopped, now) }

func (q *Queue) setState(s QueueState, now time.Time) {
	if q.State == s {
		return
	}
	q.State = s
	q.UpdatedAt = now.UTC()
}

// CanDelete returns an error matching ErrConflict for the default queue.
func (q *Queue) CanDelete() error {
	if q.IsDefault {
		return Conflict("queue %q is the default queue and cannot be deleted", q.ID)
	}
	return nil
}
