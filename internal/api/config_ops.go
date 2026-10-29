package api

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/config"
)

const tagConfig = "config"

// UpdateConfigBody is the request of update-config.
type UpdateConfigBody struct {
	Values map[string]any `json:"values" minProperties:"1" doc:"Keys to change, by dotted name (log.level, torrent.dht, server.fs_roots): a value sets the key in the config file, null removes it so the default applies again. Durations are text (\"30s\"), lists are arrays of strings."`
}

// configMu serialises writes of the config file.
var configMu sync.Mutex

func (s *Server) registerConfig() {
	Register(s, huma.Operation{
		OperationID: "get-config",
		Method:      http.MethodGet,
		Path:        "/v1/config",
		Summary:     "Boot configuration",
		Description: "Every boot configuration key with the value the running daemon uses, its default, where the value came from " +
			"(default, file, env or flag) and whether the config file now holds a different value that applies at the next start (pending). " +
			"All of it needs a restart to take effect. Secrets are never shown.",
		Tags:   []string{tagConfig, tagSystem},
		Errors: []int{http.StatusServiceUnavailable},
	}, s.getConfig)

	Register(s, huma.Operation{
		OperationID: "update-config",
		Method:      http.MethodPatch,
		Path:        "/v1/config",
		Summary:     "Change the boot configuration",
		Description: "Writes keys into the daemon's config file (other keys are kept, comments are not) after checking that the result loads and validates; " +
			"nothing is written otherwise (422). The running daemon is not changed: restart it (restart-daemon) to apply the change. " +
			"A key an environment variable or a flag overrides is written as well but keeps its running value. Answers like get-config.",
		Tags:   []string{tagConfig, tagSystem},
		Errors: []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.updateConfig)
}

func (s *Server) getConfig(ctx context.Context, _ *struct{}) (*struct{ Body *config.View }, error) {
	configMu.Lock()
	defer configMu.Unlock()
	v, err := s.cfg.Describe()
	if err != nil {
		return nil, s.fail(ctx, "get-config", err, locBody)
	}
	return &struct{ Body *config.View }{Body: v}, nil
}

func (s *Server) updateConfig(ctx context.Context, in *struct{ Body UpdateConfigBody }) (*struct{ Body *config.View }, error) {
	configMu.Lock()
	defer configMu.Unlock()
	if err := s.cfg.Patch(in.Body.Values); err != nil {
		var pe *config.PatchError
		if errors.As(err, &pe) {
			loc := "body.values"
			if pe.Key != "" {
				loc += "." + pe.Key
			}
			p := newProblem(http.StatusUnprocessableEntity, pe.Message, &huma.ErrorDetail{Message: pe.Message, Location: loc})
			params := map[string]string{}
			if pe.Key != "" {
				params["key"] = pe.Key
			}
			return nil, p.withCode(pe.Code, params)
		}
		return nil, s.fail(ctx, "update-config", err, locBody)
	}
	v, err := s.cfg.Describe()
	if err != nil {
		return nil, s.fail(ctx, "update-config", err, locBody)
	}
	return &struct{ Body *config.View }{Body: v}, nil
}
