package domain

import (
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/settings"
)

// Priority orders tasks inside a queue (higher first, then by position).
// In JSON it is the lower-case string "low", "normal" or "high".
type Priority int

// Task priorities.
const (
	PriorityLow    Priority = -1
	PriorityNormal Priority = 0
	PriorityHigh   Priority = 1
)

// Valid reports whether p is a known priority.
func (p Priority) Valid() bool { return p >= PriorityLow && p <= PriorityHigh }

// String returns the lower-case priority name.
func (p Priority) String() string {
	switch p {
	case PriorityLow:
		return "low"
	case PriorityNormal:
		return "normal"
	case PriorityHigh:
		return "high"
	default:
		return fmt.Sprintf("priority(%d)", int(p))
	}
}

// MarshalText implements encoding.TextMarshaler.
func (p Priority) MarshalText() ([]byte, error) {
	if !p.Valid() {
		return nil, InvalidArgument("unknown priority %d", int(p))
	}
	return []byte(p.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (p *Priority) UnmarshalText(text []byte) error {
	switch string(text) {
	case "low":
		*p = PriorityLow
	case "normal":
		*p = PriorityNormal
	case "high":
		*p = PriorityHigh
	default:
		return InvalidArgument("unknown priority %q", text)
	}
	return nil
}

// Error codes for TaskError. Engines may add their own codes later; these
// are the classes the scheduler and refresh logic understand.
const (
	// ErrorCodeNetwork: connection reset, timeout, DNS failure (transient).
	ErrorCodeNetwork = "network"
	// ErrorCodeHTTPStatus: the server answered with an unexpected status.
	ErrorCodeHTTPStatus = "http_status"
	// ErrorCodeLinkExpired: the link no longer works; refresh may fix it.
	ErrorCodeLinkExpired = "link_expired"
	// ErrorCodeBrowser: the headless browser could not open a page.
	ErrorCodeBrowser = "browser"
	// ErrorCodeDiskFull: not enough free space at the destination.
	ErrorCodeDiskFull = "disk_full"
	// ErrorCodeChecksumMismatch: the downloaded file failed verification.
	ErrorCodeChecksumMismatch = "checksum_mismatch"
	// ErrorCodePostprocess: a step after the download (extraction, a
	// post-action) failed; the downloaded file is in place.
	ErrorCodePostprocess = "postprocess"
	// ErrorCodeFatal: any other non-recoverable failure.
	ErrorCodeFatal = "fatal"
)

// TaskError describes why a task is in the error state.
type TaskError struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
	At        time.Time `json:"at"`
}

// Task is one download.
type Task struct {
	ID      string     `json:"id"`
	QueueID string     `json:"queueId"`
	Status  TaskStatus `json:"status"`
	// Priority and Position order the task within its queue.
	Priority Priority `json:"priority"`
	Position int64    `json:"position"`
	// Engine is the protocol engine ("http", "hls", "bt", …); empty until resolved.
	Engine string `json:"engine,omitempty"`
	Name   string `json:"name,omitempty"`
	Dir    string `json:"dir,omitempty"`
	// Size is the total size in bytes; -1 when unknown.
	Size       int64     `json:"size"`
	Downloaded int64     `json:"downloaded"`
	Source     Source    `json:"source"`
	Resource   *Resource `json:"resource,omitempty"`
	// Settings are the task-level overrides.
	Settings    settings.Settings `json:"settings"`
	Labels      map[string]string `json:"labels,omitempty"`
	Error       *TaskError        `json:"error,omitempty"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
	StartedAt   *time.Time        `json:"startedAt,omitempty"`
	CompletedAt *time.Time        `json:"completedAt,omitempty"`
}

// NewTaskParams are the caller-supplied fields of a new task.
type NewTaskParams struct {
	// QueueID defaults to MainQueueID when empty.
	QueueID  string
	Name     string
	Engine   string
	Source   Source
	Settings settings.Settings
	Priority Priority
	Labels   map[string]string
}

// NewTask builds a validated task in the queued state with an unknown size.
// Source.OriginalURL defaults to CurrentURL, Method to GET and ResolverKind
// to direct. Labels are copied. Errors match ErrInvalidArgument.
func NewTask(id string, now time.Time, p NewTaskParams) (*Task, error) {
	now = now.UTC()
	queueID := p.QueueID
	if queueID == "" {
		queueID = MainQueueID
	}
	src := p.Source
	src.applyDefaults()
	t := &Task{
		ID:        id,
		QueueID:   queueID,
		Status:    StatusQueued,
		Priority:  p.Priority,
		Engine:    p.Engine,
		Name:      p.Name,
		Size:      -1,
		Source:    src,
		Settings:  p.Settings,
		Labels:    maps.Clone(p.Labels),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return t, nil
}

// Validate checks the task's invariants. Settings errors match
// ErrInvalidArgument and unwrap to *settings.ValidationError; all other
// errors match ErrInvalidArgument.
func (t *Task) Validate() error {
	if t.ID == "" {
		return InvalidArgument("task: id is required")
	}
	if t.QueueID == "" {
		return InvalidArgument("task %q: queueId is required", t.ID)
	}
	if !t.Status.Valid() {
		return InvalidArgument("task %q: unknown status %q", t.ID, t.Status)
	}
	if !t.Priority.Valid() {
		return InvalidArgument("task %q: unknown priority %d", t.ID, int(t.Priority))
	}
	if err := t.Source.validate(); err != nil {
		return InvalidArgument("task %q: %w", t.ID, err)
	}
	if t.Size < -1 {
		return InvalidArgument("task %q: size must be >= -1", t.ID)
	}
	if t.Downloaded < 0 {
		return InvalidArgument("task %q: downloaded must be >= 0", t.ID)
	}
	if t.Size >= 0 && t.Downloaded > t.Size {
		return InvalidArgument("task %q: downloaded %d exceeds size %d", t.ID, t.Downloaded, t.Size)
	}
	if err := t.Settings.Validate(settings.LevelTask); err != nil {
		return invalidSettings("task", t.ID, err)
	}
	for k := range t.Labels {
		if strings.TrimSpace(k) == "" {
			return InvalidArgument("task %q: label keys must not be empty", t.ID)
		}
	}
	return nil
}

// Progress returns the completed fraction in [0, 1], or -1 when the size is
// unknown. A zero-byte task reports 1 once completed, else 0.
func (t *Task) Progress() float64 {
	switch {
	case t.Size < 0:
		return -1
	case t.Size == 0:
		if t.Status == StatusCompleted {
			return 1
		}
		return 0
	default:
		return min(1, max(0, float64(t.Downloaded)/float64(t.Size)))
	}
}
