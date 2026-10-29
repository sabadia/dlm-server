package events

import (
	"slices"
	"time"
)

// Type identifies the kind of an Event. Values are lower-case dotted strings
// and are part of the public API (SSE event names, filter values).
type Type string

// Event types emitted by the system. Add new types here; the bus treats every
// type uniformly except TypeTaskProgress, which is coalesced per task.
const (
	TypeTaskCreated  Type = "task.created"
	TypeTaskUpdated  Type = "task.updated"
	TypeTaskStatus   Type = "task.status"
	TypeTaskProgress Type = "task.progress"
	TypeTaskDeleted  Type = "task.deleted"
	// TypeTaskRefresh: a link-refresh step happened (a strategy ran or was
	// skipped, a wait started or ended); Data is the domain.RefreshAttempt.
	TypeTaskRefresh     Type = "task.refresh"
	TypeQueueCreated    Type = "queue.created"
	TypeQueueUpdated    Type = "queue.updated"
	TypeQueueDeleted    Type = "queue.deleted"
	TypeSettingsUpdated Type = "settings.updated"
	// TypeQueueCompleted: a queue with an onQueueComplete action ran out
	// of work; the action runs after its delay unless cancelled.
	TypeQueueCompleted Type = "queue.completed"
	// TypeQueueActionCancelled: a pending post-queue action was cancelled
	// (new work, queue stopped or deleted, settings changed).
	TypeQueueActionCancelled Type = "queue.action_cancelled"
	// TypeQuotaExhausted: a global or queue byte quota was used up.
	TypeQuotaExhausted Type = "quota.exhausted"
	// TypeQuotaReset: an exhausted quota is available again.
	TypeQuotaReset Type = "quota.reset"
	// TypeSystemSpeed: the transfer speed of the whole daemon, once a second
	// while anything moves (and once more when it stops).
	TypeSystemSpeed Type = "system.speed"
	// TypeLimitsChanged: the temporary global speed override was set,
	// cleared or ran out.
	TypeLimitsChanged Type = "limits.changed"
)

// Event is a single notification published on the Bus.
//
// ID and Time are assigned by the bus when the event is delivered; values set
// by the publisher are overwritten. Data is shared between all subscribers and
// the replay buffer, so publishers must not mutate it after publishing.
type Event struct {
	ID      uint64    `json:"id"`
	Type    Type      `json:"type"`
	Time    time.Time `json:"time"`
	TaskID  string    `json:"taskId,omitempty"`
	QueueID string    `json:"queueId,omitempty"`
	Data    any       `json:"data,omitempty"`
}

// Filter selects events. Zero-valued fields match everything, so the zero
// Filter matches all events. Non-empty fields are combined with AND; Types is
// a set combined with OR.
type Filter struct {
	Types   []Type
	TaskID  string
	QueueID string
}

// Match reports whether e satisfies the filter.
func (f Filter) Match(e Event) bool {
	if len(f.Types) > 0 && !slices.Contains(f.Types, e.Type) {
		return false
	}
	if f.TaskID != "" && f.TaskID != e.TaskID {
		return false
	}
	if f.QueueID != "" && f.QueueID != e.QueueID {
		return false
	}
	return true
}
