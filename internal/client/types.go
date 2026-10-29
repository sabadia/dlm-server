package client

import (
	"encoding/json"
	"time"
)

// Task mirrors the daemon's task JSON.
type Task struct {
	ID         string `json:"id"`
	QueueID    string `json:"queueId"`
	Status     string `json:"status"`
	Priority   string `json:"priority"`
	Position   int64  `json:"position"`
	Engine     string `json:"engine,omitempty"`
	Name       string `json:"name,omitempty"`
	Dir        string `json:"dir,omitempty"`
	Size       int64  `json:"size"` // -1 while unknown
	Downloaded int64  `json:"downloaded"`
	Source     Source `json:"source"`
	// Resource is the resolved resource (absent until probed).
	Resource *Resource `json:"resource,omitempty"`
	// Settings are the task-level overrides (a settings layer object).
	Settings    json.RawMessage   `json:"settings,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Error       *TaskError        `json:"error,omitempty"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
	StartedAt   *time.Time        `json:"startedAt,omitempty"`
	CompletedAt *time.Time        `json:"completedAt,omitempty"`
}

// Progress returns the downloaded fraction in [0, 1], or -1 when the size
// is unknown.
func (t *Task) Progress() float64 {
	switch {
	case t.Size < 0:
		return -1
	case t.Size == 0:
		if t.Status == "completed" {
			return 1
		}
		return 0
	default:
		return min(1, max(0, float64(t.Downloaded)/float64(t.Size)))
	}
}

// Source describes where a task downloads from. In requests only the
// user-settable fields are sent (Fingerprint is computed by the daemon).
type Source struct {
	CurrentURL    string            `json:"currentUrl"`
	OriginalURL   string            `json:"originalUrl,omitempty"`
	RedirectURL   string            `json:"redirectUrl,omitempty"`
	PageURL       string            `json:"pageUrl,omitempty"`
	Referrer      string            `json:"referrer,omitempty"`
	Method        string            `json:"method,omitempty"`
	Body          []byte            `json:"body,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	UserAgent     string            `json:"userAgent,omitempty"`
	CookieDomains []string          `json:"cookieDomains,omitempty"`
	MediaHint     string            `json:"mediaHint,omitempty"`
	Mirrors       []string          `json:"mirrors,omitempty"`
	StreamKind    string            `json:"streamKind,omitempty"` // hls or dash, for a manifest whose URL does not say so
	ResolverKind  string            `json:"resolverKind,omitempty"`
	Fingerprint   *Fingerprint      `json:"fingerprint,omitempty"`
}

// Fingerprint identifies the remote file.
type Fingerprint struct {
	Size         int64      `json:"size"`
	ETag         string     `json:"etag,omitempty"`
	LastModified *time.Time `json:"lastModified,omitempty"`
	Filename     string     `json:"filename,omitempty"`
	ContentType  string     `json:"contentType,omitempty"`
}

// Resource is what a source resolves to.
type Resource struct {
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	RangeSupported bool   `json:"rangeSupported"`
	Files          []File `json:"files,omitempty"`
}

// File is one file of a multi-file resource.
type File struct {
	Index    int    `json:"index"`
	Path     string `json:"path,omitempty"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Selected bool   `json:"selected"`
}

// TaskError is the last failure of a task.
type TaskError struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
	At        time.Time `json:"at"`
}

// Queue mirrors the daemon's queue JSON.
type Queue struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	IsDefault bool            `json:"isDefault"`
	Position  int64           `json:"position"`
	State     string          `json:"state"`
	Settings  json.RawMessage `json:"settings,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// ResolveResult is the response of resolve-task.
type ResolveResult struct {
	ResolveID string    `json:"resolveId"`
	Engine    string    `json:"engine"`
	Source    Source    `json:"source"`
	Resource  Resource  `json:"resource"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expiresAt"`
	// DuplicateOf is set when another task already has this download.
	DuplicateOf *DuplicateHint `json:"duplicateOf,omitempty"`
	// Formats are the qualities a stream offers, best first; pass an id as the
	// source's mediaHint to download that one.
	Formats []Format `json:"formats,omitempty"`
}

// Format is one quality a stream can be downloaded in.
type Format struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Kind      string `json:"kind"` // video or audio
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Bandwidth int64  `json:"bandwidth,omitempty"`
	Codecs    string `json:"codecs,omitempty"`
	Container string `json:"container,omitempty"`
	Size      int64  `json:"size"`
}

// DuplicateHint names the task a new download would repeat.
type DuplicateHint struct {
	TaskID string `json:"taskId"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// EffectiveSettings are resolved settings with the level ("default",
// "global", "queue", "task") each value comes from, keyed by JSON path
// (e.g. "retry.maxAttempts").
type EffectiveSettings struct {
	Settings json.RawMessage   `json:"settings"`
	Sources  map[string]string `json:"sources"`
}

// TaskPage is one page of list-tasks.
type TaskPage struct {
	Tasks []*Task `json:"tasks"`
	// NextCursor is empty on the last page.
	NextCursor string `json:"nextCursor,omitempty"`
	// Total is the number of tasks matching the filters, when asked for.
	Total *int64 `json:"total,omitempty"`
}

// BatchItem is the outcome of one item of a bulk operation.
type BatchItem struct {
	ID    string    `json:"id,omitempty"`
	OK    bool      `json:"ok"`
	Task  *Task     `json:"task,omitempty"`
	Error *APIError `json:"error,omitempty"`
	// URL is the link of an imported entry.
	URL string `json:"url,omitempty"`
}

// ResolveRequest is the request of resolve-task. Set URL or Source.
type ResolveRequest struct {
	URL     string  `json:"url,omitempty"`
	Source  *Source `json:"source,omitempty"`
	QueueID string  `json:"queueId,omitempty"`
	// Settings is a task-level settings layer object (may be empty).
	Settings json.RawMessage `json:"settings,omitempty"`
}

// CreateTaskRequest is the request of create-task (and one item of
// create-tasks). Set URL, Source or ResolveID.
type CreateTaskRequest struct {
	URL       string            `json:"url,omitempty"`
	Source    *Source           `json:"source,omitempty"`
	ResolveID string            `json:"resolveId,omitempty"`
	QueueID   string            `json:"queueId,omitempty"`
	Name      string            `json:"name,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	Settings  json.RawMessage   `json:"settings,omitempty"`
	Priority  string            `json:"priority,omitempty"` // low, normal, high
	Labels    map[string]string `json:"labels,omitempty"`
	Paused    bool              `json:"paused,omitempty"`
	// AllowDuplicate adds the task even when it repeats another one.
	AllowDuplicate bool `json:"allowDuplicate,omitempty"`
}

// UpdateTaskRequest is the request of update-task. Nil fields are left
// unchanged.
type UpdateTaskRequest struct {
	Name     *string `json:"name,omitempty"`
	Dir      *string `json:"dir,omitempty"`
	Priority *string `json:"priority,omitempty"`
	// Labels replaces all labels; an empty map removes them.
	Labels *map[string]string `json:"labels,omitempty"`
	// SettingsPatch is an RFC 7396 merge patch of the task's settings
	// overrides (null values unset fields).
	SettingsPatch json.RawMessage `json:"settings,omitempty"`
}

// ListTasksOptions are the parameters of list-tasks.
type ListTasksOptions struct {
	QueueID string
	Status  []string
	Search  string
	// Sort is position (default), created, name, size or completed; Order is
	// asc or desc (empty: the default of the sort).
	Sort  string
	Order string
	// Engine, Label (key or key=value) and Category (ID or name) narrow the list.
	Engine, Label, Category string
	// MinSize and MaxSize bound the size in bytes; nil means no bound.
	MinSize, MaxSize *int64
	// CreatedAfter, CreatedBefore, CompletedAfter and CompletedBefore bound
	// the times (RFC 3339); empty means no bound.
	CreatedAfter, CreatedBefore     string
	CompletedAfter, CompletedBefore string
	// Total asks for the number of matching tasks (TaskPage.Total).
	Total  bool
	Limit  int // 1..500; 0 means the daemon default
	Cursor string
}

// TaskFilter selects tasks by queue and status for a bulk operation.
type TaskFilter struct {
	QueueID string   `json:"queueId,omitempty"`
	Status  []string `json:"status,omitempty"`
}

// TaskSelection names the tasks of a bulk operation: IDs, or Filter (an
// empty, non-nil filter selects every task).
type TaskSelection struct {
	IDs    []string    `json:"ids,omitempty"`
	Filter *TaskFilter `json:"filter,omitempty"`
}

// CreateQueueRequest is the request of create-queue.
type CreateQueueRequest struct {
	Name     string          `json:"name"`
	Settings json.RawMessage `json:"settings,omitempty"`
	State    string          `json:"state,omitempty"` // running (default) or stopped
}

// UpdateQueueRequest is the request of update-queue. Nil fields are left
// unchanged.
type UpdateQueueRequest struct {
	Name *string `json:"name,omitempty"`
	// SettingsPatch is an RFC 7396 merge patch of the queue's settings
	// overrides.
	SettingsPatch json.RawMessage `json:"settings,omitempty"`
}
