package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
)

const tagRefresh = "refresh"

// UpdateTaskSourceBody is the request of update-task-source: the new link
// and any context to change. Fields left out keep the task's values; the
// original URL stays unless set.
type UpdateTaskSourceBody struct {
	SourceInput
	OnMismatch settings.MismatchPolicy `json:"onMismatch,omitempty" enum:"restart,fail" doc:"What to do when the new link serves different content than the one downloaded so far: restart discards the partial data, fail rejects the link. Default: the task's effective refresh.onMismatch (ask counts as fail)."`
}

// RefreshHistory is the response of list-task-refreshes.
type RefreshHistory struct {
	Attempts []*domain.RefreshAttempt `json:"attempts" doc:"The task's refresh history, oldest first."`
}

func (s *Server) registerRefresh() {
	Register(s, huma.Operation{
		OperationID: "refresh-task",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/{id}/refresh",
		Summary:     "Refresh a task's link",
		Description: "Runs the task's refresh strategies (refresh.strategies) now to obtain a new link for an expired one. " +
			"A refreshed task is queued again and resumes from where it stopped; when only waiting strategies remain it waits in awaiting_link. " +
			"Answers 502 when no strategy produced a usable link. Running tasks must be paused first (409).",
		Tags:   []string{tagRefresh, tagTasks},
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusBadGateway, http.StatusServiceUnavailable},
	}, s.refreshTask)

	Register(s, huma.Operation{
		OperationID: "await-task-link",
		Method:      http.MethodPost,
		Path:        "/v1/tasks/{id}/refresh/await",
		Summary:     "Wait for a new link from the browser",
		Description: "Puts a failed or awaiting_link task into the awaiting_link state: the next download the browser extension captures " +
			"that matches the task's fingerprint (POST /v1/capture) becomes its new link. The wait ends after refresh.awaitTimeout.",
		Tags:   []string{tagRefresh, tagTasks},
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, s.awaitTaskLink)

	Register(s, huma.Operation{
		OperationID: "update-task-source",
		Method:      http.MethodPatch,
		Path:        "/v1/tasks/{id}/source",
		Summary:     "Set a task's link",
		Description: "Replaces the task's link by hand. The new link is probed and checked against the task's fingerprint (size, strong ETag, file name): " +
			"on a match the download resumes from its segments, on a mismatch onMismatch decides (409 when the link is rejected). " +
			"Accepts application/json and application/merge-patch+json. Running tasks must be paused first (409).",
		Tags:   []string{tagRefresh, tagTasks},
		Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusBadGateway, http.StatusServiceUnavailable},
	}, s.updateTaskSource)
	s.allowMergePatch(http.MethodPatch, "/v1/tasks/{id}/source")

	Register(s, huma.Operation{
		OperationID: "list-task-refreshes",
		Method:      http.MethodGet,
		Path:        "/v1/tasks/{id}/refresh/history",
		Summary:     "List a task's refresh history",
		Description: "Every refresh step of the task: strategies that ran or were skipped, waits and their outcome, with the links before and after.",
		Tags:        []string{tagRefresh, tagTasks},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.listTaskRefreshes)
}

func (s *Server) refreshTask(ctx context.Context, in *taskPathInput) (*taskOutput, error) {
	if s.refresh == nil {
		return nil, errServicesUnavailable()
	}
	t, err := s.refresh.Refresh(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "refresh-task", err, locBody)
	}
	return &taskOutput{Body: redactTaskFor(ctx, t)}, nil
}

func (s *Server) awaitTaskLink(ctx context.Context, in *taskPathInput) (*taskOutput, error) {
	if s.refresh == nil {
		return nil, errServicesUnavailable()
	}
	t, err := s.refresh.Await(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "await-task-link", err, locBody)
	}
	return &taskOutput{Body: redactTaskFor(ctx, t)}, nil
}

func (s *Server) updateTaskSource(ctx context.Context, in *struct {
	ID   string `path:"id" doc:"Task ID."`
	Body UpdateTaskSourceBody
},
) (*taskOutput, error) {
	if s.refresh == nil {
		return nil, errServicesUnavailable()
	}
	t, err := s.refresh.SetSource(ctx, in.ID, service.SetSourceInput{
		Source: in.Body.toDomain(), OnMismatch: in.Body.OnMismatch,
	})
	if err != nil {
		return nil, s.fail(ctx, "update-task-source", err, locBody)
	}
	return &taskOutput{Body: redactTaskFor(ctx, t)}, nil
}

func (s *Server) listTaskRefreshes(ctx context.Context, in *taskPathInput) (*struct{ Body RefreshHistory }, error) {
	if s.refresh == nil {
		return nil, errServicesUnavailable()
	}
	h, err := s.refresh.History(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "list-task-refreshes", err, locBody)
	}
	return &struct{ Body RefreshHistory }{Body: RefreshHistory{Attempts: h}}, nil
}
