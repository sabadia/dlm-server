package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
)

// MoveTasksBody is the request of move-tasks.
type MoveTasksBody struct {
	TaskSelector
	QueueID string `json:"queueId" minLength:"1" doc:"Target queue ID; the tasks go to its end."`
}

// UpdateTasksBody is the request of update-tasks. Fields left out are not
// changed.
type UpdateTasksBody struct {
	TaskSelector
	Priority     *domain.Priority  `json:"priority,omitempty" enum:"low,normal,high" doc:"New priority."`
	QueueID      string            `json:"queueId,omitempty" doc:"Also move the tasks to this queue."`
	Labels       map[string]string `json:"labels,omitempty" doc:"Labels to add or change; the other labels of each task stay."`
	RemoveLabels []string          `json:"removeLabels,omitempty" doc:"Labels to drop (by key)."`
	Settings     SettingsPatch     `json:"settings,omitempty" doc:"Merge patch of each task's settings overrides; null values unset fields."`
}

func (s *Server) registerTaskBulk() {
	const bulkErrors = http.StatusServiceUnavailable
	Register(s, huma.Operation{
		OperationID: "move-tasks",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/move",
		Summary:     "Move tasks to another queue",
		Description: "Appends each selected task at the end of the target queue. The answer lists the outcome of every task.",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusUnprocessableEntity, bulkErrors},
	}, s.moveTasks)

	Register(s, huma.Operation{
		OperationID: "update-tasks",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/update",
		Summary:     "Change several tasks at once",
		Description: "Sets the priority, adds or removes labels, patches the settings overrides and/or moves each selected task. " +
			"A task that cannot take the change (for example a settings patch it rejects) reports its own error.",
		Tags:   []string{tagTasks},
		Errors: []int{http.StatusUnprocessableEntity, bulkErrors},
	}, s.updateTasks)

	Register(s, huma.Operation{
		OperationID: "restart-tasks",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/restart",
		Summary:     "Restart tasks",
		Description: "Downloads each selected finished or failed task again from scratch.",
		Tags:        []string{tagTasks},
		Errors:      []int{http.StatusUnprocessableEntity, bulkErrors},
	}, s.restartTasks)

	Register(s, huma.Operation{
		OperationID: "refresh-tasks",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/refresh",
		Summary:     "Refresh the links of tasks",
		Description: "Runs the refresh strategies of each selected task now (see refresh-task).",
		Tags:        []string{tagTasks, tagRefresh},
		Errors:      []int{http.StatusUnprocessableEntity, bulkErrors},
	}, s.refreshTasks)
}

func (s *Server) moveTasks(ctx context.Context, in *struct{ Body MoveTasksBody }) (*BatchOutput, error) {
	return s.bulk(ctx, "move-tasks", in.Body.TaskSelector, func(ctx context.Context, ids []string) []service.BatchResult {
		return s.tasks.MoveMany(ctx, ids, in.Body.QueueID)
	})
}

func (s *Server) updateTasks(ctx context.Context, in *struct{ Body UpdateTasksBody }) (*BatchOutput, error) {
	b := in.Body
	change := service.UpdateTaskInput{
		Priority: b.Priority, LabelsSet: b.Labels, LabelsRemove: b.RemoveLabels, SettingsPatch: []byte(b.Settings),
	}
	return s.bulk(ctx, "update-tasks", b.TaskSelector, func(ctx context.Context, ids []string) []service.BatchResult {
		return s.tasks.UpdateMany(ctx, ids, change, b.QueueID)
	})
}

func (s *Server) restartTasks(ctx context.Context, in *struct{ Body TaskSelector }) (*BatchOutput, error) {
	return s.bulk(ctx, "restart-tasks", in.Body, s.tasks.RestartMany)
}

func (s *Server) refreshTasks(ctx context.Context, in *struct{ Body TaskSelector }) (*BatchOutput, error) {
	if s.refresh == nil {
		return nil, errServicesUnavailable()
	}
	return s.bulk(ctx, "refresh-tasks", in.Body, func(ctx context.Context, ids []string) []service.BatchResult {
		out := make([]service.BatchResult, len(ids))
		for i, id := range ids {
			t, err := s.refresh.Refresh(ctx, id)
			out[i] = service.BatchResult{ID: id, Task: t, Err: err}
		}
		return out
	})
}
