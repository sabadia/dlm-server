package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
)

const tagTasks = "tasks"

// SourceInput is the user-settable part of a download source.
type SourceInput struct {
	CurrentURL    string              `json:"currentUrl" minLength:"1" doc:"Absolute URL to download."`
	OriginalURL   string              `json:"originalUrl,omitempty" doc:"URL the user started from (defaults to currentUrl); used for link refresh."`
	PageURL       string              `json:"pageUrl,omitempty" doc:"Web page the link was found on."`
	Referrer      string              `json:"referrer,omitempty" doc:"Referer header to send."`
	Method        string              `json:"method,omitempty" doc:"HTTP method (default GET)."`
	Body          []byte              `json:"body,omitempty" doc:"Request body (base64)."`
	Headers       map[string]string   `json:"headers,omitempty" doc:"Request headers captured with the link."`
	UserAgent     string              `json:"userAgent,omitempty" doc:"User agent captured with the link."`
	CookieDomains []string            `json:"cookieDomains,omitempty" doc:"Domains whose stored cookies apply."`
	MediaHint     string              `json:"mediaHint,omitempty" doc:"Manifest pattern or variant hint for streams."`
	Mirrors       []string            `json:"mirrors,omitempty" maxItems:"16" doc:"Other http(s) links to the same file. A ranged download spreads its connections over the mirrors that serve a file of the same size and supports ranges; the others are ignored."`
	ResolverKind  domain.ResolverKind `json:"resolverKind,omitempty" enum:"direct,browser,ytdlp,plugin" doc:"How the link was obtained (default direct)."`
}

func (in *SourceInput) toDomain() domain.Source {
	return domain.Source{
		CurrentURL: in.CurrentURL, OriginalURL: in.OriginalURL, PageURL: in.PageURL, Referrer: in.Referrer,
		Method: in.Method, Body: in.Body, Headers: in.Headers, UserAgent: in.UserAgent,
		CookieDomains: in.CookieDomains, MediaHint: in.MediaHint, Mirrors: in.Mirrors, ResolverKind: in.ResolverKind,
	}
}

// SourceFields are the alternative ways to name what to download.
type SourceFields struct {
	URL    string       `json:"url,omitempty" doc:"URL to download; shorthand for source.currentUrl. Mutually exclusive with source."`
	Source *SourceInput `json:"source,omitempty" doc:"Full source description. Mutually exclusive with url."`
}

// source returns the domain source; required reports whether one of url
// and source must be present.
func (f *SourceFields) source(required bool) (domain.Source, error) {
	switch {
	case f.URL != "" && f.Source != nil:
		return domain.Source{}, newProblem(http.StatusUnprocessableEntity, "url and source are mutually exclusive",
			&huma.ErrorDetail{Message: "set either url or source", Location: "body.url"})
	case f.Source != nil:
		return f.Source.toDomain(), nil
	case f.URL != "":
		return domain.Source{CurrentURL: f.URL}, nil
	case required:
		return domain.Source{}, newProblem(http.StatusUnprocessableEntity, "one of url or source is required",
			&huma.ErrorDetail{Message: "url or source is required", Location: "body.url"})
	default:
		return domain.Source{}, nil
	}
}

// ResolveTaskBody is the request of resolve-task.
type ResolveTaskBody struct {
	SourceFields
	QueueID  string            `json:"queueId,omitempty" doc:"Queue whose settings apply (default: the Main queue)."`
	Settings settings.Settings `json:"settings,omitzero" doc:"Task-level settings overrides used for the probe."`
}

// CreateTaskBody is the request of create-task (and one item of create-tasks).
type CreateTaskBody struct {
	SourceFields
	ResolveID string            `json:"resolveId,omitempty" doc:"ID of a resolve-task result; its probe is reused and url/source are ignored."`
	QueueID   string            `json:"queueId,omitempty" doc:"Queue to add the task to (default: the Main queue)."`
	Name      string            `json:"name,omitempty" doc:"File name without directories (default: the suggested name)."`
	Dir       string            `json:"dir,omitempty" doc:"Absolute download directory (default: the effective downloadDir)."`
	Settings  settings.Settings `json:"settings,omitzero" doc:"Task-level settings overrides."`
	Priority  domain.Priority   `json:"priority,omitempty" enum:"low,normal,high" doc:"Priority within the queue (default normal)."`
	Labels    map[string]string `json:"labels,omitempty" doc:"Free-form labels."`
	Paused    bool              `json:"paused,omitempty" doc:"Create the task paused instead of queued."`
	// AllowDuplicate lets a task repeat another one.
	AllowDuplicate bool `json:"allowDuplicate,omitempty" doc:"Add the task even when another task has the same link, expected checksum, or (once resolved) file name and size; otherwise it is refused with 409."`
}

func (b *CreateTaskBody) toInput() (service.CreateTaskInput, error) {
	src, err := b.source(b.ResolveID == "")
	if err != nil {
		return service.CreateTaskInput{}, err
	}
	return service.CreateTaskInput{
		Source: src, ResolveID: b.ResolveID, QueueID: b.QueueID, Name: b.Name, Dir: b.Dir,
		Settings: b.Settings, Priority: b.Priority, Labels: b.Labels, Paused: b.Paused, AllowDuplicate: b.AllowDuplicate,
	}, nil
}

// TaskList is a page of tasks.
type TaskList struct {
	Tasks      []*domain.Task `json:"tasks" doc:"Tasks of this page."`
	NextCursor string         `json:"nextCursor,omitempty" doc:"Pass as cursor to get the next page; absent on the last page."`
	Total      *int64         `json:"total,omitempty" doc:"The number of tasks matching the filters (all pages); only when total=true was asked."`
}

// BatchItem is the outcome of one item of a bulk operation.
type BatchItem struct {
	ID    string       `json:"id,omitempty" doc:"Task ID (the requested one, or the created task's ID)."`
	OK    bool         `json:"ok" doc:"Whether the item succeeded."`
	Task  *domain.Task `json:"task,omitempty" doc:"The task's state after the operation (for delete: its last state)."`
	Error *Problem     `json:"error,omitempty" doc:"Why the item failed (RFC 9457 problem with a code)."`
}

// BatchOutput is the response of a bulk operation. It is 200 even when some
// items failed; check each item's ok.
type BatchOutput struct {
	Body struct {
		Results []BatchItem `json:"results" doc:"One result per item, in request order."`
	}
}

// TaskSelectorFilter selects tasks by queue and status.
type TaskSelectorFilter struct {
	QueueID string              `json:"queueId,omitempty" doc:"Only tasks of this queue."`
	Status  []domain.TaskStatus `json:"status,omitempty" enum:"queued,running,paused,awaiting_link,refreshing,postprocessing,completed,error" doc:"Only tasks in any of these states."`
}

// TaskSelector names the tasks of a bulk operation: explicit ids or a
// filter (an empty filter selects every task), not both.
type TaskSelector struct {
	IDs    []string            `json:"ids,omitempty" maxItems:"1000" doc:"Task IDs. Mutually exclusive with filter."`
	Filter *TaskSelectorFilter `json:"filter,omitempty" doc:"Select tasks by queue and status ({} selects all tasks). Mutually exclusive with ids."`
}

// DeleteTasksBody is the request of delete-tasks.
type DeleteTasksBody struct {
	TaskSelector
	Files bool `json:"files,omitempty" doc:"Also delete the partial and downloaded files."`
}

type taskOutput struct {
	Body *domain.Task
}

type effectiveOutput struct {
	Body settings.Effective
}

type taskPathInput struct {
	ID string `path:"id" doc:"Task ID."`
}

// PositionBody is the request of the reorder operations.
type PositionBody struct {
	Position int `json:"position" minimum:"0" doc:"0-based target index (clamped to the last index)."`
}

func (s *Server) registerTasks() {
	Register(s, huma.Operation{
		OperationID: "resolve-task",
		Method:      http.MethodPost,
		Path:        "/v1/resolve",
		Summary:     "Resolve a download",
		Description: "Probes a URL or source with the effective settings and returns what would be downloaded " +
			"(name, size, range support). The result is cached under resolveId, which create-task accepts until " +
			"expiresAt. A failed probe returns 502 with the engine error code.",
		Tags:   []string{tagTasks},
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusBadGateway, http.StatusServiceUnavailable},
	}, s.resolveTask)

	Register(s, huma.Operation{
		OperationID:   "create-task",
		Method:        http.MethodPost,
		Path:          "/v1/tasks",
		Summary:       "Create a task",
		Description:   "Adds a task at the end of its queue, queued (or paused). Pass resolveId to reuse a resolve-task probe.",
		Tags:          []string{tagTasks},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.createTask)

	Register(s, huma.Operation{
		OperationID: "create-tasks",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/batch",
		Summary:     "Create several tasks",
		Description: "Creates each item independently; one failure does not stop the others. Returns one result per item.",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.createTasks)

	Register(s, huma.Operation{
		OperationID: "list-tasks",
		Method:      http.MethodGet,
		Path:        "/v1/tasks",
		Summary:     "List tasks",
		Description: "Returns one page of tasks matching the filters. Follow nextCursor (with the same sort) for the next page.",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.listTasks)

	Register(s, huma.Operation{
		OperationID: "get-task",
		Method:      http.MethodGet,
		Path:        "/v1/tasks/{id}",
		Summary:     "Get a task",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.getTask)

	Register(s, huma.Operation{
		OperationID: "update-task",
		Method:      http.MethodPatch,
		Path:        "/v1/tasks/{id}",
		Summary:     "Update a task",
		Description: "Changes name, dir, priority, labels (replaced as a whole; {} removes them) and/or settings " +
			"overrides. settings is an RFC 7396 merge patch: null unsets a field. Name and dir cannot change while " +
			"the task is active or has partial data (409).",
		Tags:   []string{tagTasks},
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.updateTask)
	s.allowMergePatch(http.MethodPatch, "/v1/tasks/{id}")

	Register(s, huma.Operation{
		OperationID: "move-task",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/{id}/move",
		Summary:     "Move a task to another queue",
		Description: "Appends the task at the end of the target queue.",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.moveTask)

	Register(s, huma.Operation{
		OperationID: "reorder-task",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/{id}/reorder",
		Summary:     "Reorder a task within its queue",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.reorderTask)

	Register(s, huma.Operation{
		OperationID: "pause-tasks",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/pause",
		Summary:     "Pause tasks",
		Description: "Pauses each selected task independently. Returns one result per task.",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.pauseTasks)

	Register(s, huma.Operation{
		OperationID: "resume-tasks",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/resume",
		Summary:     "Resume tasks",
		Description: "Queues each selected paused or failed task again. Returns one result per task.",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.resumeTasks)

	Register(s, huma.Operation{
		OperationID: "delete-tasks",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/delete",
		Summary:     "Delete tasks",
		Description: "Deletes each selected task independently (running tasks are stopped first), optionally with their files.",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.deleteTasks)

	Register(s, huma.Operation{
		OperationID: "restart-task",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/{id}/restart",
		Summary:     "Restart a task",
		Description: "Downloads a completed or failed task again from scratch. Fails with 409 while the downloaded file exists.",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, s.restartTask)

	Register(s, huma.Operation{
		OperationID:   "delete-task",
		Method:        http.MethodDelete,
		Path:          "/v1/tasks/{id}",
		Summary:       "Delete a task",
		Description:   "Deletes the task (stopping it first if running), optionally with its files.",
		Tags:          []string{tagTasks},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, s.deleteTask)

	Register(s, huma.Operation{
		OperationID: "get-task-effective-settings",
		Method:      http.MethodGet,
		Path:        "/v1/tasks/{id}/settings/effective",
		Summary:     "Get a task's effective settings",
		Description: "Resolves Defaults ⊕ Global ⊕ Queue ⊕ Task and reports the level each value comes from.",
		Tags:        []string{tagTasks, tagSettings},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.taskEffectiveSettings)
}

func (s *Server) resolveTask(ctx context.Context, in *struct{ Body ResolveTaskBody }) (*struct{ Body *service.ResolveResult }, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	src, err := in.Body.source(true)
	if err != nil {
		return nil, err
	}
	res, err := s.tasks.Resolve(ctx, service.ResolveInput{Source: src, QueueID: in.Body.QueueID, Settings: in.Body.Settings})
	if err != nil {
		return nil, s.fail(ctx, "resolve-task", err, locBodySettings)
	}
	return &struct{ Body *service.ResolveResult }{Body: res}, nil
}

func (s *Server) createTask(ctx context.Context, in *struct{ Body CreateTaskBody }) (*taskOutput, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	ci, err := in.Body.toInput()
	if err != nil {
		return nil, err
	}
	t, err := s.tasks.Create(ctx, ci)
	if err != nil {
		return nil, s.fail(ctx, "create-task", err, locBodySettings)
	}
	return &taskOutput{Body: t}, nil
}

// CreateTasksBody is the request of create-tasks.
type CreateTasksBody struct {
	Items []CreateTaskBody `json:"items" minItems:"1" maxItems:"1000" doc:"Tasks to create."`
	// ExpandPatterns turns a range in an item's URL into one task per value.
	ExpandPatterns bool `json:"expandPatterns,omitempty" doc:"Expand ranges in the URLs: file[001-100].jpg becomes 100 tasks, [a-e] and [1-10:2] (step) work too, several ranges give every combination (at most 10000 per URL). Results are then one per expanded task, in order; without it the URLs are used as they are."`
}

func (s *Server) createTasks(ctx context.Context, in *struct{ Body CreateTasksBody }) (*BatchOutput, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	out := &BatchOutput{}
	// slots has one entry per result; inputs are the ones that go to the
	// service, valid their indexes in slots.
	var slots []BatchItem
	inputs := make([]service.CreateTaskInput, 0, len(in.Body.Items))
	var valid []int
	for i := range in.Body.Items {
		ci, err := in.Body.Items[i].toInput()
		items := []service.CreateTaskInput{ci}
		if err == nil && in.Body.ExpandPatterns {
			items, err = service.ExpandPatternInput(ci)
		}
		if err != nil {
			slots = append(slots, BatchItem{Error: s.asProblem(ctx, "create-tasks", err, locBodySettings)})
			continue
		}
		for _, it := range items {
			valid = append(valid, len(slots))
			slots = append(slots, BatchItem{})
			inputs = append(inputs, it)
		}
	}
	for j, r := range s.tasks.CreateBatch(ctx, inputs) {
		slots[valid[j]] = s.batchItem(ctx, "create-tasks", r)
	}
	out.Body.Results = slots
	return out, nil
}

// ListTasksInput are the parameters of list-tasks.
type ListTasksInput struct {
	QueueID         string    `query:"queueId" doc:"Only tasks of this queue."`
	Status          []string  `query:"status" enum:"queued,running,paused,awaiting_link,refreshing,postprocessing,completed,error" doc:"Only tasks in any of these states (comma-separated)."`
	Q               string    `query:"q" doc:"Case-insensitive substring of the task name or URL."`
	Engine          string    `query:"engine" doc:"Only tasks of this engine (http, bt, ...)."`
	Label           string    `query:"label" doc:"Only tasks with this label: key, or key=value."`
	Category        string    `query:"category" doc:"Only tasks that belong to this category (ID or name): their name or content type matches its rules."`
	MinSize         int64     `query:"minSize" minimum:"-1" default:"-1" doc:"Only tasks of at least this many bytes (-1: no bound; tasks of unknown size are left out)."`
	MaxSize         int64     `query:"maxSize" minimum:"-1" default:"-1" doc:"Only tasks of at most this many bytes (-1: no bound; tasks of unknown size are left out)."`
	CreatedAfter    time.Time `query:"createdAfter" doc:"Only tasks created at or after this time (RFC 3339)."`
	CreatedBefore   time.Time `query:"createdBefore" doc:"Only tasks created at or before this time (RFC 3339)."`
	CompletedAfter  time.Time `query:"completedAfter" doc:"Only tasks completed at or after this time (RFC 3339); tasks that are not completed are left out."`
	CompletedBefore time.Time `query:"completedBefore" doc:"Only tasks completed at or before this time (RFC 3339); tasks that are not completed are left out."`
	Sort            string    `query:"sort" enum:"position,created,name,size,completed" default:"position" doc:"position: queue order (priority, then position); created: by creation time (newest first); name: case-insensitive (A to Z); size: smallest first (unknown sizes first); completed: oldest first (tasks that are not completed first)."`
	Order           string    `query:"order" enum:"asc,desc" doc:"Direction; the default is the one named in sort. The position order has one direction."`
	Total           bool      `query:"total" doc:"Also count every task matching the filters (total)."`
	Limit           int       `query:"limit" minimum:"1" maximum:"500" default:"50" doc:"Page size."`
	Cursor          string    `query:"cursor" doc:"nextCursor of the previous page (same sort and order)."`
}

func (s *Server) listTasks(ctx context.Context, in *ListTasksInput) (*struct{ Body TaskList }, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	f := storage.TaskFilter{
		QueueID: in.QueueID, Search: in.Q, Engine: in.Engine, Label: in.Label,
		CreatedAfter: in.CreatedAfter, CreatedBefore: in.CreatedBefore,
		CompletedAfter: in.CompletedAfter, CompletedBefore: in.CompletedBefore,
		Sort: storage.TaskSort(in.Sort), Order: storage.SortOrder(in.Order),
		Limit: in.Limit, Cursor: in.Cursor, WithTotal: in.Total,
	}
	for _, st := range in.Status {
		f.Statuses = append(f.Statuses, domain.TaskStatus(st))
	}
	if in.MinSize >= 0 {
		f.MinSize = &in.MinSize
	}
	if in.MaxSize >= 0 {
		f.MaxSize = &in.MaxSize
	}
	if in.Category != "" {
		if s.categories == nil {
			return nil, errServicesUnavailable()
		}
		c, err := s.categories.Find(ctx, in.Category)
		if err != nil {
			return nil, s.fail(ctx, "list-tasks", err, "query.category")
		}
		f.NameSuffixes, f.ContentTypes = c.Suffixes(), c.MIMETypes
	}
	page, err := s.tasks.List(ctx, f)
	if err != nil {
		return nil, s.fail(ctx, "list-tasks", err, locBody)
	}
	out := &struct{ Body TaskList }{Body: TaskList{Tasks: page.Tasks, NextCursor: page.NextCursor}}
	if in.Total {
		out.Body.Total = &page.Total
	}
	if out.Body.Tasks == nil {
		out.Body.Tasks = []*domain.Task{}
	}
	return out, nil
}

func (s *Server) getTask(ctx context.Context, in *taskPathInput) (*taskOutput, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	t, err := s.tasks.Get(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "get-task", err, locBody)
	}
	return &taskOutput{Body: t}, nil
}

// UpdateTaskBody is the request of update-task. Absent fields are unchanged.
type UpdateTaskBody struct {
	Name     *string            `json:"name,omitempty" doc:"New file name (no directories)."`
	Dir      *string            `json:"dir,omitempty" doc:"New absolute download directory; empty string resets it to the effective downloadDir."`
	Priority *domain.Priority   `json:"priority,omitempty" enum:"low,normal,high" doc:"New priority."`
	Labels   *map[string]string `json:"labels,omitempty" doc:"Replaces all labels; {} removes them."`
	Settings SettingsPatch      `json:"settings,omitempty" doc:"Merge patch of the task's settings overrides; null values unset fields."`
}

func (s *Server) updateTask(ctx context.Context, in *struct {
	ID   string `path:"id" doc:"Task ID."`
	Body UpdateTaskBody
},
) (*taskOutput, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	b := in.Body
	t, err := s.tasks.Update(ctx, in.ID, service.UpdateTaskInput{
		Name: b.Name, Dir: b.Dir, Priority: b.Priority, Labels: b.Labels, SettingsPatch: []byte(b.Settings),
	})
	if err != nil {
		return nil, s.fail(ctx, "update-task", err, locBodySettings)
	}
	return &taskOutput{Body: t}, nil
}

// MoveTaskBody is the request of move-task.
type MoveTaskBody struct {
	QueueID string `json:"queueId" minLength:"1" doc:"Target queue ID."`
}

func (s *Server) moveTask(ctx context.Context, in *struct {
	ID   string `path:"id" doc:"Task ID."`
	Body MoveTaskBody
},
) (*taskOutput, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	t, err := s.tasks.Move(ctx, in.ID, in.Body.QueueID)
	if err != nil {
		return nil, s.fail(ctx, "move-task", err, locBody)
	}
	return &taskOutput{Body: t}, nil
}

func (s *Server) reorderTask(ctx context.Context, in *struct {
	ID   string `path:"id" doc:"Task ID."`
	Body PositionBody
},
) (*taskOutput, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	t, err := s.tasks.Reorder(ctx, in.ID, in.Body.Position)
	if err != nil {
		return nil, s.fail(ctx, "reorder-task", err, locBody)
	}
	return &taskOutput{Body: t}, nil
}

func (s *Server) pauseTasks(ctx context.Context, in *struct{ Body TaskSelector }) (*BatchOutput, error) {
	return s.bulk(ctx, "pause-tasks", in.Body, s.tasks.Pause)
}

func (s *Server) resumeTasks(ctx context.Context, in *struct{ Body TaskSelector }) (*BatchOutput, error) {
	return s.bulk(ctx, "resume-tasks", in.Body, s.tasks.Resume)
}

func (s *Server) deleteTasks(ctx context.Context, in *struct{ Body DeleteTasksBody }) (*BatchOutput, error) {
	opts := service.DeleteOptions{Files: in.Body.Files}
	return s.bulk(ctx, "delete-tasks", in.Body.TaskSelector, func(ctx context.Context, ids []string) []service.BatchResult {
		return s.tasks.Delete(ctx, ids, opts)
	})
}

// bulk resolves the selector to task IDs and runs fn on them.
func (s *Server) bulk(ctx context.Context, op string, sel TaskSelector,
	fn func(context.Context, []string) []service.BatchResult,
) (*BatchOutput, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	ids, err := s.selectTasks(ctx, op, sel)
	if err != nil {
		return nil, err
	}
	out := &BatchOutput{}
	out.Body.Results = make([]BatchItem, 0, len(ids))
	for _, r := range fn(ctx, ids) {
		out.Body.Results = append(out.Body.Results, s.batchItem(ctx, op, r))
	}
	return out, nil
}

// selectTasks returns the selector's explicit IDs, or the IDs of every task
// matching its filter (all pages).
func (s *Server) selectTasks(ctx context.Context, op string, sel TaskSelector) ([]string, error) {
	switch {
	case len(sel.IDs) > 0 && sel.Filter != nil:
		return nil, newProblem(http.StatusUnprocessableEntity, "ids and filter are mutually exclusive",
			&huma.ErrorDetail{Message: "set either ids or filter", Location: "body.ids"})
	case len(sel.IDs) > 0:
		return sel.IDs, nil
	case sel.Filter == nil:
		return nil, newProblem(http.StatusUnprocessableEntity, "one of ids or filter is required",
			&huma.ErrorDetail{Message: "ids or filter is required", Location: "body.ids"})
	default:
	}
	f := storage.TaskFilter{QueueID: sel.Filter.QueueID, Statuses: sel.Filter.Status, Limit: storage.MaxTaskLimit}
	var ids []string
	for {
		page, err := s.tasks.List(ctx, f)
		if err != nil {
			return nil, s.fail(ctx, op, err, locBody)
		}
		for _, t := range page.Tasks {
			ids = append(ids, t.ID)
		}
		if page.NextCursor == "" {
			return ids, nil
		}
		f.Cursor = page.NextCursor
	}
}

// batchItem converts a service batch result.
func (s *Server) batchItem(ctx context.Context, op string, r service.BatchResult) BatchItem {
	item := BatchItem{ID: r.ID, Task: r.Task, OK: r.Err == nil}
	if item.ID == "" && r.Task != nil {
		item.ID = r.Task.ID
	}
	if r.Err != nil {
		item.Task = nil
		item.Error = s.problemFor(ctx, op, r.Err, locBodySettings)
	}
	return item
}

// asProblem returns err as a problem: problems raised by the adapter are
// kept, service errors are mapped.
func (s *Server) asProblem(ctx context.Context, op string, err error, settingsLoc string) *Problem {
	if p, ok := err.(*Problem); ok { //nolint:errorlint // adapter problems are never wrapped
		return p
	}
	return s.problemFor(ctx, op, err, settingsLoc)
}

func (s *Server) restartTask(ctx context.Context, in *taskPathInput) (*taskOutput, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	t, err := s.tasks.Restart(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "restart-task", err, locBody)
	}
	return &taskOutput{Body: t}, nil
}

func (s *Server) deleteTask(ctx context.Context, in *struct {
	ID    string `path:"id" doc:"Task ID."`
	Files bool   `query:"files" doc:"Also delete the partial and downloaded files."`
},
) (*struct{}, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	res := s.tasks.Delete(ctx, []string{in.ID}, service.DeleteOptions{Files: in.Files})
	if err := res[0].Err; err != nil {
		return nil, s.fail(ctx, "delete-task", err, locBody)
	}
	return &struct{}{}, nil
}

func (s *Server) taskEffectiveSettings(ctx context.Context, in *taskPathInput) (*effectiveOutput, error) {
	if s.settings == nil {
		return nil, errServicesUnavailable()
	}
	eff, err := s.settings.EffectiveForTask(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "get-task-effective-settings", err, locBody)
	}
	return &effectiveOutput{Body: eff}, nil
}
