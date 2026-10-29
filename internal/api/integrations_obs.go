package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/logging"
	"github.com/sabadia/dlm-server/internal/webhook"
)

// WebhookDeliveries is the response of list-webhook-deliveries.
type WebhookDeliveries struct {
	Deliveries []webhook.Delivery `json:"deliveries" doc:"The latest delivery attempts, newest first; each retry of a delivery is an attempt of its own."`
}

// PluginLogsInput is the request of get-plugin-logs.
type PluginLogsInput struct {
	Name   string `path:"name" doc:"Plugin name."`
	Level  string `query:"level" enum:"debug,info,warn,error" doc:"Only records of at least this level."`
	Q      string `query:"q" doc:"Only records whose message or attributes contain this text, ignoring case."`
	Before int64  `query:"before" minimum:"0" doc:"Only records older than this id: pass the first id of the previous answer to page backwards."`
	Limit  int    `query:"limit" minimum:"1" maximum:"500" default:"200" doc:"How many records, counted from the newest."`
}

func (s *Server) registerIntegrationsObservability() {
	Register(s, huma.Operation{
		OperationID: "list-webhook-deliveries",
		Method:      http.MethodGet,
		Path:        "/v1/webhooks/{id}/deliveries",
		Summary:     "List a webhook's recent deliveries",
		Description: "The last 100 attempts to send an event to the webhook since the daemon started, newest first: when, which event, the attempt number, " +
			"the HTTP status the endpoint answered (0 when it did not), how long it took, the error, and whether a retry follows. Test deliveries are included.",
		Tags:   []string{tagWebhooks},
		Errors: []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.listWebhookDeliveries)

	Register(s, huma.Operation{
		OperationID: "get-plugin-logs",
		Method:      http.MethodGet,
		Path:        "/v1/plugins/{name}/logs",
		Summary:     "Read a plugin's log",
		Description: "The latest log records of a plugin (500 are kept): what its script wrote with dlm.log and what the daemon said about it (failed starts, " +
			"failed hooks). Same filters and paging as get-logs.",
		Tags:   []string{tagPlugins, tagLogs},
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.getPluginLogs)
}

func (s *Server) listWebhookDeliveries(ctx context.Context, in *struct {
	ID string `path:"id" doc:"Webhook ID."`
},
) (*struct{ Body WebhookDeliveries }, error) {
	if s.webhooks == nil {
		return nil, errServicesUnavailable()
	}
	d, err := s.webhooks.Deliveries(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "list-webhook-deliveries", err, locBody)
	}
	if d == nil {
		d = []webhook.Delivery{}
	}
	return &struct{ Body WebhookDeliveries }{Body: WebhookDeliveries{Deliveries: d}}, nil
}

func (s *Server) getPluginLogs(ctx context.Context, in *PluginLogsInput) (*struct{ Body LogList }, error) {
	if s.plugins == nil {
		return nil, errServicesUnavailable()
	}
	ring, err := s.plugins.Logs(ctx, in.Name)
	if err != nil {
		return nil, s.fail(ctx, "get-plugin-logs", err, locBody)
	}
	f, err := logFilter(in.Level, in.Q, time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	recs, more := ring.Query(f, uint64(max(in.Before, 0)), in.Limit)
	if recs == nil {
		recs = []logging.Record{}
	}
	return &struct{ Body LogList }{Body: LogList{Records: recs, HasMore: more, LastID: ring.LastID(), BootID: s.bootID}}, nil
}
