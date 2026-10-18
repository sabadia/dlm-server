package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
)

const tagWebhooks = "webhooks"

// WebhookList is the response of list-webhooks.
type WebhookList struct {
	Webhooks []*domain.Webhook `json:"webhooks" doc:"Every webhook, oldest first."`
}

// CreateWebhookBody is the request of create-webhook.
type CreateWebhookBody struct {
	URL      string   `json:"url" minLength:"1" doc:"The http or https endpoint that receives a POST per event."`
	Events   []string `json:"events" minItems:"1" doc:"Event types to deliver, such as task.status or queue.completed."`
	Secret   string   `json:"secret,omitempty" doc:"Signs each body: the X-DLM-Signature header is sha256= and the hex HMAC-SHA256. Never returned."`
	Disabled bool     `json:"disabled,omitempty" doc:"Create the webhook switched off."`
}

// UpdateWebhookBody is the request of update-webhook. Absent fields are
// unchanged.
type UpdateWebhookBody struct {
	URL     *string   `json:"url,omitempty" minLength:"1" doc:"New endpoint."`
	Events  *[]string `json:"events,omitempty" doc:"New event types (replaces the old list)."`
	Secret  *string   `json:"secret,omitempty" doc:"New signing secret; an empty string removes it."`
	Enabled *bool     `json:"enabled,omitempty" doc:"Switch the webhook on or off."`
}

type webhookOutput struct {
	Body *domain.Webhook
}

type webhookPathInput struct {
	ID string `path:"id" doc:"Webhook ID."`
}

func (s *Server) registerWebhooks() {
	Register(s, huma.Operation{
		OperationID: "list-webhooks",
		Method:      http.MethodGet,
		Path:        "/v1/webhooks",
		Summary:     "List webhooks",
		Description: "Webhooks receive a signed JSON POST for the events they subscribe to. Secrets are never returned.",
		Tags:        []string{tagWebhooks},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.listWebhooks)

	Register(s, huma.Operation{
		OperationID:   "create-webhook",
		Method:        http.MethodPost,
		Path:          "/v1/webhooks",
		Summary:       "Create a webhook",
		Tags:          []string{tagWebhooks},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.createWebhook)

	Register(s, huma.Operation{
		OperationID: "get-webhook",
		Method:      http.MethodGet,
		Path:        "/v1/webhooks/{id}",
		Summary:     "Get a webhook",
		Tags:        []string{tagWebhooks},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.getWebhook)

	Register(s, huma.Operation{
		OperationID: "update-webhook",
		Method:      http.MethodPatch,
		Path:        "/v1/webhooks/{id}",
		Summary:     "Update a webhook",
		Tags:        []string{tagWebhooks},
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.updateWebhook)

	Register(s, huma.Operation{
		OperationID:   "delete-webhook",
		Method:        http.MethodDelete,
		Path:          "/v1/webhooks/{id}",
		Summary:       "Delete a webhook",
		Tags:          []string{tagWebhooks},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.deleteWebhook)

	Register(s, huma.Operation{
		OperationID:   "test-webhook",
		Method:        http.MethodPost,
		Path:          "/v1/webhooks/{id}/test",
		Summary:       "Send a test event",
		Description:   "Delivers a webhook.test event now (one attempt, even when the webhook is disabled). Answers 502 when the endpoint does not accept it.",
		Tags:          []string{tagWebhooks},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound, http.StatusBadGateway, http.StatusServiceUnavailable},
	}, s.testWebhook)
}

func (s *Server) listWebhooks(ctx context.Context, _ *struct{}) (*struct{ Body WebhookList }, error) {
	if s.webhooks == nil {
		return nil, errServicesUnavailable()
	}
	ws, err := s.webhooks.List(ctx)
	if err != nil {
		return nil, s.fail(ctx, "list-webhooks", err, locBody)
	}
	return &struct{ Body WebhookList }{Body: WebhookList{Webhooks: ws}}, nil
}

func (s *Server) createWebhook(ctx context.Context, in *struct{ Body CreateWebhookBody }) (*webhookOutput, error) {
	if s.webhooks == nil {
		return nil, errServicesUnavailable()
	}
	w, err := s.webhooks.Create(ctx, service.CreateWebhookInput{
		URL: in.Body.URL, Events: in.Body.Events, Secret: in.Body.Secret, Disabled: in.Body.Disabled,
	})
	if err != nil {
		return nil, s.fail(ctx, "create-webhook", err, locBody)
	}
	return &webhookOutput{Body: w}, nil
}

func (s *Server) getWebhook(ctx context.Context, in *webhookPathInput) (*webhookOutput, error) {
	if s.webhooks == nil {
		return nil, errServicesUnavailable()
	}
	w, err := s.webhooks.Get(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "get-webhook", err, locBody)
	}
	return &webhookOutput{Body: w}, nil
}

func (s *Server) updateWebhook(ctx context.Context, in *struct {
	ID   string `path:"id" doc:"Webhook ID."`
	Body UpdateWebhookBody
},
) (*webhookOutput, error) {
	if s.webhooks == nil {
		return nil, errServicesUnavailable()
	}
	w, err := s.webhooks.Update(ctx, in.ID, service.UpdateWebhookInput{
		URL: in.Body.URL, Events: in.Body.Events, Secret: in.Body.Secret, Enabled: in.Body.Enabled,
	})
	if err != nil {
		return nil, s.fail(ctx, "update-webhook", err, locBody)
	}
	return &webhookOutput{Body: w}, nil
}

func (s *Server) deleteWebhook(ctx context.Context, in *webhookPathInput) (*struct{}, error) {
	if s.webhooks == nil {
		return nil, errServicesUnavailable()
	}
	if err := s.webhooks.Delete(ctx, in.ID); err != nil {
		return nil, s.fail(ctx, "delete-webhook", err, locBody)
	}
	return &struct{}{}, nil
}

func (s *Server) testWebhook(ctx context.Context, in *webhookPathInput) (*struct{}, error) {
	if s.webhooks == nil {
		return nil, errServicesUnavailable()
	}
	err := s.webhooks.Test(ctx, in.ID)
	switch {
	case err == nil:
		return &struct{}{}, nil
	case errors.Is(err, service.ErrWebhookTestUnavailable):
		return nil, errServicesUnavailable()
	case errors.Is(err, domain.ErrNotFound):
		return nil, s.fail(ctx, "test-webhook", err, locBody)
	default:
		return nil, huma.Error502BadGateway("the webhook endpoint did not accept the test event: " + err.Error()) //nolint:wrapcheck // builds the problem response
	}
}
