package events

import (
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// ProgressData is the payload of task.progress events.
type ProgressData struct {
	Downloaded  int64 `json:"downloaded"`
	Size        int64 `json:"size"`
	Speed       int64 `json:"speed"` // bytes per second, smoothed
	Connections int   `json:"connections"`
	// UploadSpeed is bytes per second sent to peers, smoothed (torrents).
	UploadSpeed int64 `json:"uploadSpeed,omitempty"`
	// Peers is the number of connected peers (torrents).
	Peers int `json:"peers,omitempty"`
}

// StatusData is the payload of task.status events.
type StatusData struct {
	From  domain.TaskStatus `json:"from"`
	To    domain.TaskStatus `json:"to"`
	Error *domain.TaskError `json:"error,omitempty"`
}

// QueueCompletedData is the payload of queue.completed events.
type QueueCompletedData struct {
	// CompletedTasks is the number of tasks the queue completed since it
	// last ran out of work.
	CompletedTasks int                  `json:"completedTasks"`
	Action         settings.QueueAction `json:"action"`
	// ExecuteAt is when the action runs unless it is cancelled first.
	ExecuteAt time.Time `json:"executeAt"`
}

// ActionCancelledData is the payload of queue.action_cancelled events.
type ActionCancelledData struct {
	Action settings.QueueAction `json:"action"`
	Reason string               `json:"reason"` // one of the Cancel* constants
}

// QuotaData is the payload of quota.exhausted and quota.reset events.
// Events of a queue's quota also carry the queue ID in Event.QueueID.
type QuotaData struct {
	Scope   string            `json:"scope"` // QuotaScopeGlobal or QuotaScopeQueue
	QueueID string            `json:"queueId,omitempty"`
	Bytes   int64             `json:"bytes"`
	Period  settings.Duration `json:"period"`
	// Used is the bytes counted in the current window (0 after a reset).
	Used int64 `json:"used"`
	// WindowStart and ResetsAt bound the current window; both are omitted
	// after a reset (the next window starts with the next byte).
	WindowStart time.Time `json:"windowStart,omitzero"`
	ResetsAt    time.Time `json:"resetsAt,omitzero"`
}

// SpeedData is the payload of system.speed events.
type SpeedData struct {
	// Down and Up are bytes per second over the last second, summed over all
	// tasks; Up is what torrents send to peers.
	Down int64 `json:"down"`
	Up   int64 `json:"up"`
	// Active is the number of tasks that are transferring (running or
	// refreshing), Queued the number waiting for a slot.
	Active int `json:"active"`
	Queued int `json:"queued"`
}

// LimitsChangedData is the payload of limits.changed events.
type LimitsChangedData struct {
	// SpeedLimit is the global limit now in force, in bytes per second;
	// 0 means unlimited.
	SpeedLimit int64 `json:"speedLimit"`
	// Override is the temporary override, absent when there is none.
	Override *SpeedOverride `json:"override,omitempty"`
}

// SpeedOverride is a temporary replacement of the global speed limit.
type SpeedOverride struct {
	// SpeedLimit is bytes per second; 0 means unlimited (turbo).
	SpeedLimit int64 `json:"speedLimit"`
	// Until is when the override ends and the configured limit applies again.
	Until time.Time `json:"until"`
}

// PairingRequestedData is the payload of pairing.requested events.
type PairingRequestedData struct {
	ID string `json:"id"`
	// Name is what the client calls itself, Browser the browser it runs in.
	Name    string `json:"name"`
	Browser string `json:"browser"`
	// ClientID and ClientVersion are the extension's id and version.
	ClientID      string `json:"clientId,omitempty"`
	ClientVersion string `json:"clientVersion,omitempty"`
	// Code is six digits the client shows; the user compares them before allowing.
	Code string `json:"code"`
	// ExpiresAt is when the request is dropped if nobody answers.
	ExpiresAt time.Time `json:"expiresAt"`
}

// PairingResolvedData is the payload of pairing.resolved events.
type PairingResolvedData struct {
	ID     string `json:"id"`
	Status string `json:"status" enum:"approved,denied,expired"`
	// TokenID is the issued token's id when the pairing was approved.
	TokenID string `json:"tokenId,omitempty"`
}

// Kinds of resources named by resource.changed events.
const (
	ResourceCategory   = "category"
	ResourceWebhook    = "webhook"
	ResourceCredential = "credential"
	ResourceCookie     = "cookie"
	ResourcePlugin     = "plugin"
	ResourceConfig     = "config"
)

// Operations of resource.changed events.
const (
	OpCreated = "created"
	OpUpdated = "updated"
	OpDeleted = "deleted"
)

// ResourceChangedData is the payload of resource.changed events.
type ResourceChangedData struct {
	// Kind is one of the Resource* constants.
	Kind string `json:"kind" enum:"category,webhook,credential,cookie,plugin,config"`
	// ID names the resource (the plugin's name, the config's file); empty when
	// the change touched several (a cookie import) or there is only one
	// (the config): refetch the whole list.
	ID string `json:"id,omitempty"`
	// Op is created, updated or deleted.
	Op string `json:"op" enum:"created,updated,deleted"`
}

// Kind describes one event type: what it means and the type of its data.
type Kind struct {
	// Type is the event type, as in Event.Type and the SSE event name.
	Type Type
	// Payload is a zero value of the type of Event.Data, for documentation
	// and tests (the data of a task event is a domain.Task, ...).
	Payload any
	// Doc says when the event is emitted.
	Doc string
}

// Catalogue lists every event type with the type of its data. It is the single
// description the OpenAPI document and the tests are built from; a new Type
// constant must be added here (a test enforces it).
func Catalogue() []Kind {
	return []Kind{
		{TypeTaskCreated, domain.Task{}, "A task was created."},
		{TypeTaskUpdated, domain.Task{}, "A task's fields changed (name, directory, settings, source, labels, ...)."},
		{TypeTaskStatus, StatusData{}, "A task moved to another status."},
		{TypeTaskProgress, ProgressData{}, "Transfer progress of a running task; coalesced per task, at most a few per second."},
		{TypeTaskDeleted, domain.Task{}, "A task was deleted; data is its last state."},
		{TypeTaskRefresh, domain.RefreshAttempt{}, "A link-refresh step happened: a strategy ran or was skipped, a wait started or ended."},
		{TypeQueueCreated, domain.Queue{}, "A queue was created."},
		{TypeQueueUpdated, domain.Queue{}, "A queue changed (name, state, settings, position)."},
		{TypeQueueDeleted, domain.Queue{}, "A queue was deleted; data is its last state."},
		{TypeQueueCompleted, QueueCompletedData{}, "A queue with an onQueueComplete action ran out of work; the action runs at executeAt unless cancelled."},
		{TypeQueueActionCancelled, ActionCancelledData{}, "A pending post-queue action was cancelled (new work, queue stopped or deleted, settings changed)."},
		{TypeSettingsUpdated, settings.Settings{}, "The global settings changed; data is the new global layer."},
		{TypeQuotaExhausted, QuotaData{}, "A global or queue byte quota was used up."},
		{TypeQuotaReset, QuotaData{}, "An exhausted quota is available again."},
		{TypeSystemSpeed, SpeedData{}, "The transfer speed of the whole daemon: once a second while anything moves, and once more (zeros) when it stops."},
		{TypeResourceChanged, ResourceChangedData{}, "A category, webhook, credential, cookie, plugin or the boot configuration was created, changed or deleted, by this or another client; refetch it."},
		{TypeLimitsChanged, LimitsChangedData{}, "The temporary global speed override was set, cleared or ran out; data is the limit now in force."},
		{TypePairingRequested, PairingRequestedData{}, "A browser extension asked to be paired and waits for the user to allow or deny it; compare the code with the one the extension shows."},
		{TypePairingResolved, PairingResolvedData{}, "A pairing was allowed, denied or ran out of time; close the prompt."},
	}
}
