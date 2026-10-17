package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// HealthOutput is the response of get-health.
type HealthOutput struct {
	Body struct {
		Status        string `json:"status" enum:"ok" doc:"Always \"ok\" when the daemon is serving."`
		Version       string `json:"version" doc:"Daemon version."`
		Authenticated bool   `json:"authenticated" doc:"Whether the request presented a valid bearer token."`
	}
}

func (s *Server) registerHealth() {
	RegisterPublic(s, huma.Operation{
		OperationID: "get-health",
		Method:      http.MethodGet,
		Path:        "/v1/health",
		Summary:     "Check daemon health",
		Description: "Reports that the daemon is up, its version, and whether the presented bearer token (if any) is valid. Does not require authentication.",
		Tags:        []string{"system"},
	}, func(ctx context.Context, _ *struct{}) (*HealthOutput, error) {
		out := &HealthOutput{}
		out.Body.Status = "ok"
		out.Body.Version = s.version
		out.Body.Authenticated = Authenticated(ctx)
		return out, nil
	})
}
