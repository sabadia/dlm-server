package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/storage"
)

// ExportTasksInput are the parameters of export-tasks.
type ExportTasksInput struct {
	Format  string   `query:"format" enum:"json,urls,aria2" default:"json" doc:"json (dlm's own), urls (one link per line) or aria2 (aria2's input file)."`
	QueueID string   `query:"queueId" doc:"Only tasks of this queue."`
	Status  []string `query:"status" enum:"queued,running,paused,awaiting_link,refreshing,postprocessing,completed,error" doc:"Only tasks in any of these states (comma-separated)."`
	Q       string   `query:"q" doc:"Case-insensitive substring of the task name or URL."`
}

// ExportedTasks is the response of export-tasks.
type ExportedTasks struct {
	Format  string `json:"format" doc:"The format of content."`
	Count   int    `json:"count" doc:"How many tasks the list holds."`
	Content string `json:"content" doc:"The list. Captured headers and cookies are not exported."`
}

// ImportTasksBody is the request of import-tasks.
type ImportTasksBody struct {
	Format         string `json:"format" enum:"json,urls,aria2" doc:"Format of content."`
	Content        string `json:"content" minLength:"1" maxLength:"8388608" doc:"The list to import (at most 8 MiB and 10000 entries)."`
	QueueID        string `json:"queueId,omitempty" doc:"Queue to add the tasks to (default: the Main queue)."`
	Paused         bool   `json:"paused,omitempty" doc:"Add the tasks paused."`
	AllowDuplicate bool   `json:"allowDuplicate,omitempty" doc:"Add tasks even when another task has the same link, checksum, or file name and size."`
	ExpandPatterns bool   `json:"expandPatterns,omitempty" doc:"Expand ranges in the URLs: file[001-100].jpg becomes 100 tasks."`
	DryRun         bool   `json:"dryRun,omitempty" doc:"Create nothing: answer, per entry, whether it would be added (ok) or why not (an invalid link, a duplicate of an existing task's link)."`
}

// StatsBody is the response of get-stats.
type StatsBody struct {
	Tasks           int                 `json:"tasks" doc:"Number of tasks."`
	ByStatus        map[string]int      `json:"byStatus" doc:"Tasks per status; statuses without tasks are absent."`
	ByEngine        []EngineStatsBody   `json:"byEngine" doc:"Tasks and bytes per protocol engine."`
	DownloadedBytes int64               `json:"downloadedBytes" doc:"Bytes downloaded by all tasks so far."`
	Completed       []CompletedStatsRow `json:"completed" doc:"Downloads completed within the last 24 hours, 7 days and 30 days."`
}

// EngineStatsBody are the tasks of one engine.
type EngineStatsBody struct {
	Engine          string `json:"engine" doc:"Engine name: http, ftp, hls, dash, bt, ed2k, ytdlp, ..."`
	Tasks           int    `json:"tasks" doc:"Number of tasks."`
	DownloadedBytes int64  `json:"downloadedBytes" doc:"Bytes downloaded by them."`
}

// CompletedStatsRow are the downloads completed within a period.
type CompletedStatsRow struct {
	Period string `json:"period" enum:"24h,7d,30d" doc:"The period ending now."`
	Tasks  int    `json:"tasks" doc:"Downloads completed in the period."`
	Bytes  int64  `json:"bytes" doc:"Their size."`
}

const (
	importPath = "/v1/tasks/import"
	// MaxImportBodyBytes is the request body limit of import-tasks: a list of
	// up to 8 MiB, which JSON escaping can roughly double.
	MaxImportBodyBytes int64 = 16 << 20
)

func (s *Server) registerPortable() {
	Register(s, huma.Operation{
		OperationID: "export-tasks",
		Method:      http.MethodGet,
		Path:        "/v1/tasks/export",
		Summary:     "Export tasks as a list",
		Description: "Writes the matching tasks as dlm JSON, a plain list of URLs, or an aria2 input file. Captured headers and cookies are never exported.",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.exportTasks)

	Register(s, huma.Operation{
		OperationID:  "import-tasks",
		Method:       http.MethodPost,
		Path:         importPath,
		Summary:      "Import a list of downloads",
		Description:  "Creates one task per entry of a dlm JSON, URL list, or aria2 input file, independently: one failure does not stop the others. Returns one result per created task. An unreadable list is refused with 422 and creates nothing.",
		Tags:         []string{tagTasks},
		MaxBodyBytes: MaxImportBodyBytes,
		Errors:       []int{http.StatusUnprocessableEntity, http.StatusRequestEntityTooLarge, http.StatusServiceUnavailable},
	}, s.importTasks)

	Register(s, huma.Operation{
		OperationID: "get-stats",
		Method:      http.MethodGet,
		Path:        "/v1/stats",
		Summary:     "Download statistics",
		Description: "Task counts per status and engine, bytes downloaded, and what completed in the last day, week and month.",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.getStats)
}

func (s *Server) exportTasks(ctx context.Context, in *ExportTasksInput) (*struct{ Body ExportedTasks }, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	f := storage.TaskFilter{QueueID: in.QueueID, Search: in.Q}
	for _, st := range in.Status {
		f.Statuses = append(f.Statuses, domain.TaskStatus(st))
	}
	res, err := s.tasks.Export(ctx, in.Format, f)
	if err != nil {
		return nil, s.fail(ctx, "export-tasks", err, locBody)
	}
	return &struct{ Body ExportedTasks }{Body: ExportedTasks{Format: res.Format, Count: res.Count, Content: res.Content}}, nil
}

func (s *Server) importTasks(ctx context.Context, in *struct{ Body ImportTasksBody }) (*BatchOutput, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	results, err := s.tasks.Import(ctx, service.ImportInput{
		Format: in.Body.Format, Content: in.Body.Content, QueueID: in.Body.QueueID, Paused: in.Body.Paused,
		AllowDuplicate: in.Body.AllowDuplicate, ExpandPatterns: in.Body.ExpandPatterns, DryRun: in.Body.DryRun,
	})
	if err != nil {
		return nil, s.fail(ctx, "import-tasks", err, locBody)
	}
	out := &BatchOutput{}
	out.Body.Results = make([]BatchItem, len(results))
	for i, r := range results {
		out.Body.Results[i] = s.batchItem(ctx, "import-tasks", r)
	}
	return out, nil
}

func (s *Server) getStats(ctx context.Context, _ *struct{}) (*struct{ Body StatsBody }, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	st, err := s.tasks.Stats(ctx)
	if err != nil {
		return nil, s.fail(ctx, "get-stats", err, locBody)
	}
	body := StatsBody{
		Tasks: st.Tasks, ByStatus: make(map[string]int, len(st.ByStatus)), ByEngine: []EngineStatsBody{},
		DownloadedBytes: st.DownloadedBytes, Completed: []CompletedStatsRow{},
	}
	for status, n := range st.ByStatus {
		body.ByStatus[string(status)] = n
	}
	for _, e := range st.ByEngine {
		body.ByEngine = append(body.ByEngine, EngineStatsBody{Engine: e.Engine, Tasks: e.Tasks, DownloadedBytes: e.DownloadedBytes})
	}
	for _, c := range st.Completed {
		body.Completed = append(body.Completed, CompletedStatsRow{Period: c.Period, Tasks: c.Tasks, Bytes: c.Bytes})
	}
	return &struct{ Body StatsBody }{Body: body}, nil
}
