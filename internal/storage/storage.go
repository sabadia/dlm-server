package storage

import (
	"context"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// Error contract shared by every repository:
//   - a missing entity returns an error matching domain.ErrNotFound;
//   - a uniqueness violation (duplicate ID, duplicate queue name) returns an
//     error matching domain.ErrAlreadyExists;
//   - an operation refused by a referential constraint (for example deleting
//     a queue that still has tasks) returns an error matching
//     domain.ErrConflict;
//   - invalid caller input (for example a malformed pagination cursor)
//     returns an error matching domain.ErrInvalidArgument.
//
// Timestamps are persisted with millisecond precision in UTC; values read
// back are truncated to the millisecond and are in UTC.

// SettingsRepository persists the global settings layer.
type SettingsRepository interface {
	// GetGlobal returns the stored global settings, or empty Settings (every
	// field unset) when they were never saved.
	GetGlobal(ctx context.Context) (settings.Settings, error)
	// SaveGlobal replaces the stored global settings.
	SaveGlobal(ctx context.Context, s settings.Settings) error
}

// QueueRepository persists queues.
type QueueRepository interface {
	Create(ctx context.Context, q *domain.Queue) error
	Get(ctx context.Context, id string) (*domain.Queue, error)
	// List returns every queue ordered by position, then creation time.
	List(ctx context.Context) ([]*domain.Queue, error)
	// Update replaces every mutable column of the queue (all but ID and
	// CreatedAt).
	Update(ctx context.Context, q *domain.Queue) error
	// Delete removes the queue. It fails with domain.ErrConflict while tasks
	// still reference it. It does not enforce domain rules such as
	// Queue.CanDelete; that is the service layer's job.
	Delete(ctx context.Context, id string) error
	// NextPosition returns one past the highest queue position (0 when there
	// are no queues).
	NextPosition(ctx context.Context) (int64, error)
}

// TaskSort selects the order of a task listing.
type TaskSort string

// Task sort orders.
const (
	// TaskSortPosition is queue order: priority descending, then position
	// and creation time ascending. It is the default and has one direction.
	TaskSortPosition TaskSort = "position"
	// TaskSortCreated orders by creation time, newest first by default.
	TaskSortCreated TaskSort = "created"
	// TaskSortName orders by name, case-insensitively, A to Z by default.
	TaskSortName TaskSort = "name"
	// TaskSortSize orders by size (unknown sizes count as -1), smallest
	// first by default.
	TaskSortSize TaskSort = "size"
	// TaskSortCompleted orders by completion time, oldest first by default;
	// tasks that are not completed count as never and come first (last when
	// descending).
	TaskSortCompleted TaskSort = "completed"
)

// SortOrder is the direction of a listing.
type SortOrder string

// Sort directions. The empty order means the default of the sort.
const (
	OrderAsc  SortOrder = "asc"
	OrderDesc SortOrder = "desc"
)

// Task listing page-size limits.
const (
	DefaultTaskLimit = 50
	MaxTaskLimit     = 500
)

// TaskFilter selects and orders tasks for TaskRepository.List. Zero values
// mean "no filter" / defaults.
type TaskFilter struct {
	// QueueID restricts the listing to one queue.
	QueueID string
	// Statuses restricts the listing to tasks in any of these states.
	Statuses []domain.TaskStatus
	// Search matches, case-insensitively, a substring of the task name or of
	// its current URL. Case folding covers ASCII letters only.
	Search string
	// Engine restricts the listing to tasks of one engine.
	Engine string
	// Label restricts the listing to tasks with this label: "key" matches
	// any value, "key=value" an exact one.
	Label string
	// NameSuffixes and ContentTypes together select a category: a task
	// matches when its name ends with one of the suffixes (".iso", compared
	// case-insensitively) or its content type is one of ContentTypes ("type/*"
	// matches every subtype). Both empty means no restriction.
	NameSuffixes []string
	ContentTypes []string
	// MinSize and MaxSize bound the size in bytes (inclusive); tasks of
	// unknown size match neither bound.
	MinSize, MaxSize *int64
	// CreatedAfter/Before and CompletedAfter/Before bound the creation and
	// completion times (inclusive); zero means unbounded. A bound on the
	// completion time leaves out tasks that are not completed.
	CreatedAfter, CreatedBefore     time.Time
	CompletedAfter, CompletedBefore time.Time
	// Sort defaults to TaskSortPosition.
	Sort TaskSort
	// Order is the direction; empty means the default of Sort.
	Order SortOrder
	// Limit is the page size: <= 0 means DefaultTaskLimit; values above
	// MaxTaskLimit are clamped.
	Limit int
	// Cursor is the opaque TaskPage.NextCursor of the previous page; empty
	// starts at the beginning. A cursor is only valid with the same Sort and
	// Order.
	Cursor string
	// WithTotal also counts every task matching the filters (ignoring the
	// cursor and the limit).
	WithTotal bool
}

// TaskPage is one page of a task listing.
type TaskPage struct {
	Tasks []*domain.Task
	// NextCursor is empty when there are no more tasks.
	NextCursor string
	// Total is the number of tasks matching the filters, set when
	// TaskFilter.WithTotal was requested.
	Total int64
}

// TaskRepository persists tasks.
type TaskRepository interface {
	// Create inserts the task. A missing queue returns domain.ErrNotFound.
	Create(ctx context.Context, t *domain.Task) error
	Get(ctx context.Context, id string) (*domain.Task, error)
	// Update replaces every mutable column of the task (all but ID and
	// CreatedAt).
	Update(ctx context.Context, t *domain.Task) error
	// UpdateProgress is the cheap hot-path write of the transfer counters.
	UpdateProgress(ctx context.Context, id string, downloaded, size int64, updatedAt time.Time) error
	// Delete removes the task together with its segments and engine state.
	Delete(ctx context.Context, id string) error
	// List returns one page of tasks using keyset pagination.
	List(ctx context.Context, f TaskFilter) (TaskPage, error)
	// NextPosition returns one past the highest task position in the queue
	// (0 for an empty queue).
	NextPosition(ctx context.Context, queueID string) (int64, error)
	// FindDuplicate returns an existing task that the query describes the same
	// download as: first one with the same link (current or original URL),
	// then one with the same expected checksum, then one that is not failed
	// with the same name and size. It returns nil when there is none.
	FindDuplicate(ctx context.Context, q DuplicateQuery) (*domain.Task, error)
	// CountByStatus counts the tasks of a queue per status; an empty queueID
	// counts across all queues. Statuses without tasks are absent.
	CountByStatus(ctx context.Context, queueID string) (map[domain.TaskStatus]int, error)
	// Stats summarizes every task: counts per status, tasks and bytes per
	// engine, the bytes of all tasks, and for each time in since the tasks
	// completed at or after it.
	Stats(ctx context.Context, since []time.Time) (TaskStats, error)
}

// TaskStats is the result of TaskRepository.Stats.
type TaskStats struct {
	ByStatus map[domain.TaskStatus]int
	// ByEngine is ordered by engine name.
	ByEngine []EngineStats
	// DownloadedBytes is the sum of the bytes downloaded by all tasks.
	DownloadedBytes int64
	// Completed has one entry per requested time, in the same order.
	Completed []CompletedStats
}

// EngineStats are the tasks of one engine (the protocol).
type EngineStats struct {
	Engine          string
	Tasks           int
	DownloadedBytes int64
}

// CompletedStats are the tasks completed since a time.
type CompletedStats struct {
	Since time.Time
	Tasks int
	Bytes int64
}

// DuplicateQuery describes a download for TaskRepository.FindDuplicate. Empty
// or non-positive fields are not compared.
type DuplicateQuery struct {
	// URLs are the links of the download.
	URLs []string
	// Checksum is the expected checksum, hex.
	Checksum string
	// Name and Size identify the file once it is resolved.
	Name string
	Size int64
}

// SegmentRepository persists a task's download segments.
type SegmentRepository interface {
	// Replace atomically replaces all segments of the task. A missing task
	// returns domain.ErrNotFound.
	Replace(ctx context.Context, taskID string, segs []domain.Segment) error
	// List returns the task's segments ordered by index (empty when none).
	List(ctx context.Context, taskID string) ([]domain.Segment, error)
}

// EngineStateRepository persists opaque per-engine resume data.
type EngineStateRepository interface {
	// Get returns the stored state or an error matching domain.ErrNotFound.
	Get(ctx context.Context, taskID string) ([]byte, error)
	// Put stores (inserts or replaces) the state. A missing task returns
	// domain.ErrNotFound.
	Put(ctx context.Context, taskID string, state []byte) error
	// Delete removes the state. It is idempotent: deleting absent state is
	// not an error.
	Delete(ctx context.Context, taskID string) error
}

// RefreshHistoryRepository persists the per-attempt record of link
// refreshes. Entries are append-only and are removed with their task.
type RefreshHistoryRepository interface {
	// Add inserts an entry. A missing task returns domain.ErrNotFound.
	Add(ctx context.Context, a *domain.RefreshAttempt) error
	// List returns the task's entries, oldest first.
	List(ctx context.Context, taskID string) ([]*domain.RefreshAttempt, error)
	// Latest returns the task's newest entry, or an error matching
	// domain.ErrNotFound when it has none.
	Latest(ctx context.Context, taskID string) (*domain.RefreshAttempt, error)
	// CountAt returns how many strategy attempts (success, failure,
	// mismatch, restart; not waits or skips) were made while the task had
	// downloaded exactly this many bytes, that is, without progress since.
	CountAt(ctx context.Context, taskID string, downloaded int64) (int, error)
	// PendingAwaits returns, for every awaiting_link task whose newest entry
	// is a wait with a deadline, that entry, ordered by deadline.
	PendingAwaits(ctx context.Context) ([]*domain.RefreshAttempt, error)
}

// CategoryRepository persists the download categories.
type CategoryRepository interface {
	// Create inserts the category. A duplicate ID or name returns
	// domain.ErrAlreadyExists.
	Create(ctx context.Context, c *domain.Category) error
	// Get returns the category or an error matching domain.ErrNotFound.
	Get(ctx context.Context, id string) (*domain.Category, error)
	// List returns every category ordered by position.
	List(ctx context.Context) ([]*domain.Category, error)
	// Update replaces the name, position and rules of the category.
	Update(ctx context.Context, c *domain.Category) error
	// Delete removes the category; a missing one returns domain.ErrNotFound.
	Delete(ctx context.Context, id string) error
	// NextPosition returns one past the highest category position.
	NextPosition(ctx context.Context) (int64, error)
}

// WebhookRecord is the persisted webhook; the secret is sealed.
type WebhookRecord struct {
	ID string
	// URL receives the events.
	URL string
	// Events are the event types the webhook wants.
	Events  []string
	Enabled bool
	// SealedSecret is the signing secret, encrypted by the service; empty
	// when the webhook is not signed.
	SealedSecret []byte
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// WebhookRepository persists webhooks.
type WebhookRepository interface {
	// Create inserts the webhook; a duplicate ID returns
	// domain.ErrAlreadyExists.
	Create(ctx context.Context, w *WebhookRecord) error
	// Get returns the webhook or an error matching domain.ErrNotFound.
	Get(ctx context.Context, id string) (*WebhookRecord, error)
	// List returns every webhook, oldest first.
	List(ctx context.Context) ([]*WebhookRecord, error)
	// Update replaces the URL, events, enabled flag and secret.
	Update(ctx context.Context, w *WebhookRecord) error
	// Delete removes the webhook; a missing one returns domain.ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// CredentialRecord is the persisted login; the password is sealed.
type CredentialRecord struct {
	ID string
	// Host is the host name the login is for: "example.com" or
	// "*.example.com" (lower case, no port).
	Host string
	// Scheme is auto, basic or digest.
	Scheme   string
	Username string
	// SealedPassword is the password, encrypted by the service.
	SealedPassword []byte
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CredentialRepository persists logins.
type CredentialRepository interface {
	// Create inserts the login; a duplicate ID or host returns
	// domain.ErrAlreadyExists.
	Create(ctx context.Context, c *CredentialRecord) error
	// Get returns the login or an error matching domain.ErrNotFound.
	Get(ctx context.Context, id string) (*CredentialRecord, error)
	// List returns every login, ordered by host.
	List(ctx context.Context) ([]*CredentialRecord, error)
	// Update replaces host, scheme, username and password; a host used by
	// another login returns domain.ErrAlreadyExists.
	Update(ctx context.Context, c *CredentialRecord) error
	// Delete removes the login; a missing one returns domain.ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// PluginRecord is the persisted part of an installed plugin; its manifest is
// read from the plugin's files.
type PluginRecord struct {
	Name        string
	Version     string
	Title       string
	Description string
	Source      string
	Enabled     bool
	InstalledAt time.Time
	UpdatedAt   time.Time
}

// PluginRepository persists installed plugins.
type PluginRepository interface {
	// Create inserts the plugin; a duplicate name returns domain.ErrAlreadyExists.
	Create(ctx context.Context, p *PluginRecord) error
	// Get returns the plugin or an error matching domain.ErrNotFound.
	Get(ctx context.Context, name string) (*PluginRecord, error)
	// List returns every plugin ordered by name.
	List(ctx context.Context) ([]*PluginRecord, error)
	// Update replaces everything but the name and installation time.
	Update(ctx context.Context, p *PluginRecord) error
	// Delete removes the plugin; a missing one returns domain.ErrNotFound.
	Delete(ctx context.Context, name string) error
}

// CookieRecord represents the persisted cookie row with encrypted value.
type CookieRecord struct {
	ID           string
	Domain       string
	Name         string
	Path         string
	PartitionKey string
	EncryptedVal []byte
	Secure       bool
	HTTPOnly     bool
	SameSite     string
	HostOnly     bool
	ExpiresAt    *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CookieFilter selects cookies for CookieRepository.List.
type CookieFilter struct {
	Domain       string
	Name         string
	PartitionKey string
}

// CookieRepository persists encrypted cookies.
type CookieRepository interface {
	Upsert(ctx context.Context, c *CookieRecord) error
	Get(ctx context.Context, id string) (*CookieRecord, error)
	Delete(ctx context.Context, id string) error
	DeleteExpired(ctx context.Context, now time.Time) (int64, error)
	List(ctx context.Context, filter CookieFilter) ([]*CookieRecord, error)
}

// Store aggregates the repositories and provides transactions.
type Store interface {
	Settings() SettingsRepository
	Queues() QueueRepository
	Tasks() TaskRepository
	Segments() SegmentRepository
	EngineState() EngineStateRepository
	Cookies() CookieRepository
	RefreshHistory() RefreshHistoryRepository
	Categories() CategoryRepository
	Webhooks() WebhookRepository
	Credentials() CredentialRepository
	Plugins() PluginRepository
	// WithTx runs fn in a transaction. The Store passed to fn is bound to the
	// transaction; it is committed when fn returns nil and rolled back when
	// fn returns an error or panics. Calling WithTx on the transaction-bound
	// Store reuses the same transaction. fn must use only the Store it is
	// given: using the outer Store from inside fn may block until the
	// transaction ends.
	WithTx(ctx context.Context, fn func(tx Store) error) error
	// Close releases the underlying resources.
	Close() error
}
