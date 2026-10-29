package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
)

const tagQueues = "queues"

// QueueList is the response of list-queues.
type QueueList struct {
	Queues []*domain.Queue `json:"queues" doc:"Every queue, ordered by position."`
	// Counts maps a queue ID to the number of its tasks per status.
	Counts map[string]map[string]int `json:"counts" doc:"Per queue ID, the number of tasks in each status (statuses without tasks are left out)."`
}

// CreateQueueBody is the request of create-queue.
type CreateQueueBody struct {
	Name     string            `json:"name" minLength:"1" maxLength:"100" doc:"Unique (case-insensitive) queue name."`
	Settings settings.Settings `json:"settings,omitzero" doc:"Queue-level settings overrides."`
	State    domain.QueueState `json:"state,omitempty" enum:"running,stopped" doc:"Initial state (default running)."`
}

// UpdateQueueBody is the request of update-queue. Absent fields are unchanged.
type UpdateQueueBody struct {
	Name     *string       `json:"name,omitempty" minLength:"1" maxLength:"100" doc:"New queue name."`
	Settings SettingsPatch `json:"settings,omitempty" doc:"Merge patch of the queue's settings overrides; null values unset fields."`
}

type queueOutput struct {
	Body *domain.Queue
}

type queuePathInput struct {
	ID string `path:"id" doc:"Queue ID."`
}

func (s *Server) registerQueues() {
	Register(s, huma.Operation{
		OperationID: "list-queues",
		Method:      http.MethodGet,
		Path:        "/v1/queues",
		Summary:     "List queues",
		Tags:        []string{tagQueues},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.listQueues)

	Register(s, huma.Operation{
		OperationID:   "create-queue",
		Method:        http.MethodPost,
		Path:          "/v1/queues",
		Summary:       "Create a queue",
		Description:   "Adds a queue at the end of the queue order.",
		Tags:          []string{tagQueues},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.createQueue)

	Register(s, huma.Operation{
		OperationID: "get-queue",
		Method:      http.MethodGet,
		Path:        "/v1/queues/{id}",
		Summary:     "Get a queue",
		Tags:        []string{tagQueues},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.getQueue)

	Register(s, huma.Operation{
		OperationID: "update-queue",
		Method:      http.MethodPatch,
		Path:        "/v1/queues/{id}",
		Summary:     "Update a queue",
		Description: "Renames a queue and/or patches its settings overrides (RFC 7396 merge patch: null unsets a field).",
		Tags:        []string{tagQueues},
		Errors:      []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.updateQueue)
	s.allowMergePatch(http.MethodPatch, "/v1/queues/{id}")

	Register(s, huma.Operation{
		OperationID: "replace-queue-settings",
		Method:      http.MethodPut,
		Path:        "/v1/queues/{id}/settings",
		Summary:     "Replace a queue's settings",
		Description: "Replaces the queue's settings overrides as a whole (unset fields inherit from the global layer).",
		Tags:        []string{tagQueues, tagSettings},
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.replaceQueueSettings)

	Register(s, huma.Operation{
		OperationID:   "delete-queue",
		Method:        http.MethodDelete,
		Path:          "/v1/queues/{id}",
		Summary:       "Delete a queue",
		Description:   "Deletes a queue. A queue that still has tasks needs moveTasksTo; the Main queue cannot be deleted (409).",
		Tags:          []string{tagQueues},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.deleteQueue)

	Register(s, huma.Operation{
		OperationID: "start-queue",
		Method:      http.MethodPost,
		Path:        "/v1/queues/{id}/start",
		Summary:     "Start a queue",
		Description: "Lets the scheduler run the queue's tasks.",
		Tags:        []string{tagQueues},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.startQueue)

	Register(s, huma.Operation{
		OperationID: "stop-queue",
		Method:      http.MethodPost,
		Path:        "/v1/queues/{id}/stop",
		Summary:     "Stop a queue",
		Description: "Stops admitting the queue's tasks; running tasks return to queued.",
		Tags:        []string{tagQueues},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.stopQueue)

	Register(s, huma.Operation{
		OperationID: "reorder-queue",
		Method:      http.MethodPost,
		Path:        "/v1/queues/{id}/reorder",
		Summary:     "Reorder a queue",
		Tags:        []string{tagQueues},
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.reorderQueue)

	Register(s, huma.Operation{
		OperationID: "get-queue-effective-settings",
		Method:      http.MethodGet,
		Path:        "/v1/queues/{id}/settings/effective",
		Summary:     "Get a queue's effective settings",
		Description: "Resolves Defaults ⊕ Global ⊕ Queue and reports the level each value comes from.",
		Tags:        []string{tagQueues, tagSettings},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.queueEffectiveSettings)
}

func (s *Server) listQueues(ctx context.Context, _ *struct{}) (*struct{ Body QueueList }, error) {
	if s.queues == nil {
		return nil, errServicesUnavailable()
	}
	qs, err := s.queues.List(ctx)
	if err != nil {
		return nil, s.fail(ctx, "list-queues", err, locBody)
	}
	if qs == nil {
		qs = []*domain.Queue{}
	}
	counts, err := s.queues.Counts(ctx)
	if err != nil {
		return nil, s.fail(ctx, "list-queues", err, locBody)
	}
	out := make(map[string]map[string]int, len(counts))
	for id, byStatus := range counts {
		m := make(map[string]int, len(byStatus))
		for st, n := range byStatus {
			m[string(st)] = n
		}
		out[id] = m
	}
	return &struct{ Body QueueList }{Body: QueueList{Queues: qs, Counts: out}}, nil
}

func (s *Server) createQueue(ctx context.Context, in *struct{ Body CreateQueueBody }) (*queueOutput, error) {
	if s.queues == nil {
		return nil, errServicesUnavailable()
	}
	q, err := s.queues.Create(ctx, service.CreateQueueInput{Name: in.Body.Name, Settings: in.Body.Settings, State: in.Body.State})
	if err != nil {
		return nil, s.fail(ctx, "create-queue", err, locBodySettings)
	}
	return &queueOutput{Body: q}, nil
}

func (s *Server) getQueue(ctx context.Context, in *queuePathInput) (*queueOutput, error) {
	if s.queues == nil {
		return nil, errServicesUnavailable()
	}
	q, err := s.queues.Get(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "get-queue", err, locBody)
	}
	return &queueOutput{Body: q}, nil
}

func (s *Server) updateQueue(ctx context.Context, in *struct {
	ID   string `path:"id" doc:"Queue ID."`
	Body UpdateQueueBody
},
) (*queueOutput, error) {
	if s.queues == nil {
		return nil, errServicesUnavailable()
	}
	q, err := s.queues.Update(ctx, in.ID, service.UpdateQueueInput{Name: in.Body.Name, SettingsPatch: []byte(in.Body.Settings)})
	if err != nil {
		return nil, s.fail(ctx, "update-queue", err, locBodySettings)
	}
	return &queueOutput{Body: q}, nil
}

func (s *Server) replaceQueueSettings(ctx context.Context, in *struct {
	ID   string `path:"id" doc:"Queue ID."`
	Body settings.Settings
},
) (*queueOutput, error) {
	if s.queues == nil {
		return nil, errServicesUnavailable()
	}
	q, err := s.queues.Update(ctx, in.ID, service.UpdateQueueInput{Settings: &in.Body})
	if err != nil {
		return nil, s.fail(ctx, "replace-queue-settings", err, locBody)
	}
	return &queueOutput{Body: q}, nil
}

func (s *Server) deleteQueue(ctx context.Context, in *struct {
	ID          string `path:"id" doc:"Queue ID."`
	MoveTasksTo string `query:"moveTasksTo" doc:"Queue that receives the deleted queue's tasks (appended at its end)."`
},
) (*struct{}, error) {
	if s.queues == nil {
		return nil, errServicesUnavailable()
	}
	if err := s.queues.Delete(ctx, in.ID, service.DeleteQueueOptions{MoveTasksTo: in.MoveTasksTo}); err != nil {
		return nil, s.fail(ctx, "delete-queue", err, locBody)
	}
	return &struct{}{}, nil
}

func (s *Server) startQueue(ctx context.Context, in *queuePathInput) (*queueOutput, error) {
	if s.queues == nil {
		return nil, errServicesUnavailable()
	}
	q, err := s.queues.Start(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "start-queue", err, locBody)
	}
	return &queueOutput{Body: q}, nil
}

func (s *Server) stopQueue(ctx context.Context, in *queuePathInput) (*queueOutput, error) {
	if s.queues == nil {
		return nil, errServicesUnavailable()
	}
	q, err := s.queues.Stop(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "stop-queue", err, locBody)
	}
	return &queueOutput{Body: q}, nil
}

func (s *Server) reorderQueue(ctx context.Context, in *struct {
	ID   string `path:"id" doc:"Queue ID."`
	Body PositionBody
},
) (*queueOutput, error) {
	if s.queues == nil {
		return nil, errServicesUnavailable()
	}
	q, err := s.queues.Reorder(ctx, in.ID, in.Body.Position)
	if err != nil {
		return nil, s.fail(ctx, "reorder-queue", err, locBody)
	}
	return &queueOutput{Body: q}, nil
}

func (s *Server) queueEffectiveSettings(ctx context.Context, in *queuePathInput) (*effectiveOutput, error) {
	if s.settings == nil {
		return nil, errServicesUnavailable()
	}
	eff, err := s.settings.EffectiveForQueue(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "get-queue-effective-settings", err, locBody)
	}
	return &effectiveOutput{Body: eff}, nil
}
