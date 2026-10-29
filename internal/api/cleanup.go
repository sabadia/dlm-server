package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/service"
)

// CleanupTasksBody is the request of cleanup-tasks.
type CleanupTasksBody struct {
	OlderThanDays int    `json:"olderThanDays" minimum:"0" maximum:"36500" doc:"Remove completed tasks that completed at least this many days ago; 0 removes every completed task."`
	QueueID       string `json:"queueId,omitempty" doc:"Only tasks of this queue (default: all)."`
	Files         bool   `json:"files,omitempty" doc:"Also delete the downloaded files. Without it only the list entries go."`
	DryRun        bool   `json:"dryRun,omitempty" doc:"Only count what would be removed."`
}

func (s *Server) registerCleanup() {
	Register(s, huma.Operation{
		OperationID: "cleanup-tasks",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/cleanup",
		Summary:     "Remove old completed downloads",
		Description: "Removes the completed tasks that finished at least olderThanDays days ago, optionally with their files, and answers how many " +
			"matched and were removed. A dry run only counts. The retentionDays setting does the same automatically (never touching files).",
		Tags:   []string{tagTasks},
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.cleanupTasks)
}

func (s *Server) cleanupTasks(ctx context.Context, in *struct{ Body CleanupTasksBody }) (*struct{ Body *service.CleanupResult }, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	res, err := s.tasks.Cleanup(ctx, service.CleanupInput{
		OlderThanDays: in.Body.OlderThanDays, QueueID: in.Body.QueueID, Files: in.Body.Files, DryRun: in.Body.DryRun,
	})
	if err != nil {
		return nil, s.fail(ctx, "cleanup-tasks", err, locBody)
	}
	return &struct{ Body *service.CleanupResult }{Body: res}, nil
}
