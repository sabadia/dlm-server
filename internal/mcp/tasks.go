package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/storage"
)

// TaskInfo is a download as the tools report it.
type TaskInfo struct {
	ID         string  `json:"id"`
	Name       string  `json:"name,omitempty"`
	Status     string  `json:"status" jsonschema:"queued, running, paused, awaiting_link, refreshing, postprocessing, completed or error"`
	URL        string  `json:"url"`
	QueueID    string  `json:"queueId"`
	Dir        string  `json:"dir,omitempty"`
	Size       int64   `json:"size" jsonschema:"total bytes; -1 when unknown"`
	Downloaded int64   `json:"downloaded" jsonschema:"bytes downloaded so far"`
	Percent    float64 `json:"percent,omitempty" jsonschema:"0-100; absent when the size is unknown"`
	Error      string  `json:"error,omitempty" jsonschema:"why the download failed, when status is error"`
	Retryable  bool    `json:"retryable,omitempty" jsonschema:"whether resuming the failed download may succeed"`
	Notes      string  `json:"notes,omitempty" jsonschema:"what the post actions did"`
}

func taskInfo(t *domain.Task) TaskInfo {
	i := TaskInfo{
		ID: t.ID, Name: t.Name, Status: string(t.Status), URL: t.Source.CurrentURL, QueueID: t.QueueID,
		Dir: t.Dir, Size: t.Size, Downloaded: t.Downloaded, Notes: t.Labels[service.PostprocessLabel],
	}
	if t.Size > 0 {
		i.Percent = float64(t.Downloaded) * 100 / float64(t.Size)
	}
	if t.Error != nil {
		i.Error, i.Retryable = t.Error.Message, t.Error.Retryable
	}
	return i
}

// ItemResult is the outcome for one task of a bulk tool.
type ItemResult struct {
	ID    string    `json:"id"`
	OK    bool      `json:"ok"`
	Task  *TaskInfo `json:"task,omitempty"`
	Error string    `json:"error,omitempty"`
}

// BulkOutput is the result of a bulk tool: one entry per task, in order.
type BulkOutput struct {
	Results []ItemResult `json:"results"`
}

func bulk(rs []service.BatchResult) BulkOutput {
	out := BulkOutput{Results: make([]ItemResult, len(rs))}
	for i, r := range rs {
		it := ItemResult{ID: r.ID, OK: r.Err == nil}
		if it.ID == "" && r.Task != nil {
			it.ID = r.Task.ID
		}
		if r.Task != nil && r.Err == nil {
			ti := taskInfo(r.Task)
			it.Task = &ti
		}
		if r.Err != nil {
			it.Error = r.Err.Error()
		}
		out.Results[i] = it
	}
	return out
}

type addInput struct {
	URL     string   `json:"url" jsonschema:"what to download: an http(s), ftp(s), magnet or ed2k link, or a video page URL when ytdlp is true"`
	Mirrors []string `json:"mirrors,omitempty" jsonschema:"other http(s) links to the same file"`
	Name    string   `json:"name,omitempty" jsonschema:"file name; default: detected"`
	QueueID string   `json:"queueId,omitempty" jsonschema:"queue to add the download to; default: the Main queue"`
	Paused  bool     `json:"paused,omitempty" jsonschema:"add it paused instead of starting it"`
	Ytdlp   bool     `json:"ytdlp,omitempty" jsonschema:"download a video or audio page with yt-dlp"`
}

type listInput struct {
	Status  []string `json:"status,omitempty" jsonschema:"only downloads in these states: queued, running, paused, awaiting_link, refreshing, postprocessing, completed, error"`
	QueueID string   `json:"queueId,omitempty"`
	Search  string   `json:"search,omitempty" jsonschema:"text in the name or link"`
	Limit   int      `json:"limit,omitempty" jsonschema:"page size, 1-200; default 50"`
	Cursor  string   `json:"cursor,omitempty" jsonschema:"nextCursor of the previous page"`
}

type listOutput struct {
	Tasks      []TaskInfo `json:"tasks"`
	NextCursor string     `json:"nextCursor,omitempty" jsonschema:"pass as cursor for the next page; absent on the last page"`
}

type idInput struct {
	ID string `json:"id" jsonschema:"download ID"`
}

type idsInput struct {
	IDs []string `json:"ids" jsonschema:"download IDs"`
}

type deleteInput struct {
	IDs         []string `json:"ids" jsonschema:"download IDs"`
	DeleteFiles bool     `json:"deleteFiles,omitempty" jsonschema:"also delete the downloaded and partial files from disk"`
}

type waitInput struct {
	ID             string `json:"id" jsonschema:"download ID"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty" jsonschema:"how long to wait, 1-3600; default 60"`
}

type waitOutput struct {
	Task     TaskInfo `json:"task"`
	TimedOut bool     `json:"timedOut,omitempty" jsonschema:"true when the wait ended before the download finished"`
}

type statsOutput struct {
	Tasks           int            `json:"tasks"`
	ByStatus        map[string]int `json:"byStatus"`
	DownloadedBytes int64          `json:"downloadedBytes"`
	Completed       []completedRow `json:"completed" jsonschema:"downloads completed in the last 24h, 7d and 30d"`
}

type completedRow struct {
	Period string `json:"period"`
	Tasks  int    `json:"tasks"`
	Bytes  int64  `json:"bytes"`
}

const (
	defaultWait = 60
	maxWait     = 3600
	maxList     = 200
)

func (t *tools) registerTasks(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{
		Name: "add_download", Description: "Add a download. Returns the new download; follow it with get_download or wait_for_download.",
		Annotations: additive,
	}, t.addDownload)
	sdk.AddTool(srv, &sdk.Tool{
		Name: "list_downloads", Description: "List downloads, optionally filtered by state, queue or text.", Annotations: readOnly,
	}, t.listDownloads)
	sdk.AddTool(srv, &sdk.Tool{Name: "get_download", Description: "Show one download.", Annotations: readOnly}, t.getDownload)
	sdk.AddTool(srv, &sdk.Tool{
		Name: "wait_for_download", Annotations: readOnly,
		Description: "Wait until a download completes, fails or is paused, or the timeout passes, and return its state. " +
			"Sends progress notifications when the call carries a progress token.",
	}, t.waitForDownload)
	sdk.AddTool(srv, &sdk.Tool{Name: "pause_downloads", Description: "Pause downloads.", Annotations: idempotent}, t.pause)
	sdk.AddTool(srv, &sdk.Tool{Name: "resume_downloads", Description: "Resume paused or failed downloads.", Annotations: idempotent}, t.resume)
	sdk.AddTool(srv, &sdk.Tool{Name: "restart_download", Description: "Throw away what was downloaded and start a download over.", Annotations: destructive}, t.restart)
	sdk.AddTool(srv, &sdk.Tool{
		Name: "delete_downloads", Annotations: destructive,
		Description: "Remove downloads from the list; with deleteFiles also delete their files from disk. This cannot be undone.",
	}, t.delete)
	sdk.AddTool(srv, &sdk.Tool{
		Name: "get_stats", Description: "Download statistics: counts per state, bytes downloaded, recently completed.", Annotations: readOnly,
	}, t.stats)
}

func (t *tools) addDownload(ctx context.Context, _ *sdk.CallToolRequest, in addInput) (*sdk.CallToolResult, TaskInfo, error) {
	src := domain.Source{CurrentURL: in.URL, Mirrors: in.Mirrors}
	if in.Ytdlp {
		src.ResolverKind = domain.ResolverYtdlp
	}
	task, err := t.d.Tasks.Create(ctx, service.CreateTaskInput{Source: src, QueueID: in.QueueID, Name: in.Name, Paused: in.Paused})
	if err != nil {
		return nil, TaskInfo{}, err //nolint:wrapcheck // the message is the tool's error
	}
	return nil, taskInfo(task), nil
}

func (t *tools) listDownloads(ctx context.Context, _ *sdk.CallToolRequest, in listInput) (*sdk.CallToolResult, listOutput, error) {
	f := storage.TaskFilter{QueueID: in.QueueID, Search: in.Search, Sort: storage.TaskSortCreated, Limit: min(in.Limit, maxList), Cursor: in.Cursor}
	for _, s := range in.Status {
		st := domain.TaskStatus(s)
		if !st.Valid() {
			return nil, listOutput{}, fmt.Errorf("unknown status %q", s)
		}
		f.Statuses = append(f.Statuses, st)
	}
	page, err := t.d.Tasks.List(ctx, f)
	if err != nil {
		return nil, listOutput{}, err //nolint:wrapcheck // the message is the tool's error
	}
	out := listOutput{Tasks: make([]TaskInfo, len(page.Tasks)), NextCursor: page.NextCursor}
	for i, task := range page.Tasks {
		out.Tasks[i] = taskInfo(task)
	}
	return nil, out, nil
}

func (t *tools) getDownload(ctx context.Context, _ *sdk.CallToolRequest, in idInput) (*sdk.CallToolResult, TaskInfo, error) {
	task, err := t.d.Tasks.Get(ctx, in.ID)
	if err != nil {
		return nil, TaskInfo{}, err //nolint:wrapcheck // the message is the tool's error
	}
	return nil, taskInfo(task), nil
}

func (t *tools) pause(ctx context.Context, _ *sdk.CallToolRequest, in idsInput) (*sdk.CallToolResult, BulkOutput, error) {
	return nil, bulk(t.d.Tasks.Pause(ctx, in.IDs)), nil
}

func (t *tools) resume(ctx context.Context, _ *sdk.CallToolRequest, in idsInput) (*sdk.CallToolResult, BulkOutput, error) {
	return nil, bulk(t.d.Tasks.Resume(ctx, in.IDs)), nil
}

func (t *tools) delete(ctx context.Context, _ *sdk.CallToolRequest, in deleteInput) (*sdk.CallToolResult, BulkOutput, error) {
	return nil, bulk(t.d.Tasks.Delete(ctx, in.IDs, service.DeleteOptions{Files: in.DeleteFiles})), nil
}

func (t *tools) restart(ctx context.Context, _ *sdk.CallToolRequest, in idInput) (*sdk.CallToolResult, TaskInfo, error) {
	task, err := t.d.Tasks.Restart(ctx, in.ID)
	if err != nil {
		return nil, TaskInfo{}, err //nolint:wrapcheck // the message is the tool's error
	}
	return nil, taskInfo(task), nil
}

func (t *tools) stats(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, statsOutput, error) {
	st, err := t.d.Tasks.Stats(ctx)
	if err != nil {
		return nil, statsOutput{}, err //nolint:wrapcheck // the message is the tool's error
	}
	out := statsOutput{Tasks: st.Tasks, ByStatus: make(map[string]int, len(st.ByStatus)), DownloadedBytes: st.DownloadedBytes}
	for s, n := range st.ByStatus {
		out.ByStatus[string(s)] = n
	}
	for _, c := range st.Completed {
		out.Completed = append(out.Completed, completedRow{Period: c.Period, Tasks: c.Tasks, Bytes: c.Bytes})
	}
	return nil, out, nil
}

// settled reports whether waiting for a task is over: it finished, failed or
// was stopped.
func settled(s domain.TaskStatus) bool {
	return s == domain.StatusCompleted || s == domain.StatusError || s == domain.StatusPaused
}

func (t *tools) waitForDownload(ctx context.Context, req *sdk.CallToolRequest, in waitInput) (*sdk.CallToolResult, waitOutput, error) {
	secs := in.TimeoutSeconds
	if secs <= 0 {
		secs = defaultWait
	}
	timeout := time.Duration(min(secs, maxWait)) * time.Second

	// Subscribe before reading the state, so a change in between is not lost.
	sub := t.d.Bus.Subscribe(ctx, events.Filter{TaskID: in.ID})
	defer sub.Unsubscribe()
	task, err := t.d.Tasks.Get(ctx, in.ID)
	if err != nil {
		return nil, waitOutput{}, err //nolint:wrapcheck // the message is the tool's error
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	token := req.Params.GetProgressToken()
	for !settled(task.Status) {
		select {
		case <-ctx.Done():
			return nil, waitOutput{}, ctx.Err() //nolint:wrapcheck // the caller went away
		case <-timer.C:
			return nil, waitOutput{Task: taskInfo(task), TimedOut: true}, nil
		case ev, ok := <-sub.C():
			if !ok {
				return nil, waitOutput{}, errors.New("the daemon is shutting down")
			}
			switch d := ev.Data.(type) {
			case service.ProgressData:
				task.Downloaded, task.Size = d.Downloaded, d.Size
				if token != nil {
					t.notify(ctx, req, token, d)
				}
			default:
				// Any other event may have changed the state: read it.
				if task, err = t.d.Tasks.Get(ctx, in.ID); err != nil {
					return nil, waitOutput{}, err //nolint:wrapcheck // the message is the tool's error
				}
			}
		}
	}
	return nil, waitOutput{Task: taskInfo(task)}, nil
}

func (t *tools) notify(ctx context.Context, req *sdk.CallToolRequest, token any, d service.ProgressData) {
	total := float64(0)
	if d.Size > 0 {
		total = float64(d.Size)
	}
	err := req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{
		ProgressToken: token, Progress: float64(d.Downloaded), Total: total,
		Message: fmt.Sprintf("%d of %d bytes, %d B/s", d.Downloaded, d.Size, d.Speed),
	})
	if err != nil {
		t.d.Logger.DebugContext(ctx, "mcp: progress notification failed", "error", err)
	}
}
