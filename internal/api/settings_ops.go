package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/settings"
)

const (
	tagSettings = "settings"

	// mergePatchContentType is the RFC 7396 media type accepted (besides
	// application/json) by every PATCH operation.
	mergePatchContentType = "application/merge-patch+json"
)

type settingsOutput struct {
	ETag string `header:"ETag" doc:"Version of the settings layer; send it as If-Match to update only if unchanged"`
	Body settings.Settings
}

func newSettingsOutput(g settings.Settings) *settingsOutput {
	return &settingsOutput{ETag: settings.ETag(g), Body: g}
}

func (s *Server) registerSettings() {
	Register(s, huma.Operation{
		OperationID: "get-settings",
		Method:      http.MethodGet,
		Path:        "/v1/settings",
		Summary:     "Get the global settings",
		Description: "Returns the stored global settings layer; unset fields inherit the defaults (see get-default-settings).",
		Tags:        []string{tagSettings},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.getSettings)

	Register(s, huma.Operation{
		OperationID: "replace-settings",
		Method:      http.MethodPut,
		Path:        "/v1/settings",
		Summary:     "Replace the global settings",
		Description: "Replaces the global settings layer as a whole.",
		Tags:        []string{tagSettings},
		Errors:      []int{http.StatusPreconditionFailed, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.replaceSettings)

	Register(s, huma.Operation{
		OperationID: "patch-settings",
		Method:      http.MethodPatch,
		Path:        "/v1/settings",
		Summary:     "Patch the global settings",
		Description: "Applies an RFC 7396 JSON merge patch to the global settings layer (null unsets a field). " +
			"Accepts application/json and application/merge-patch+json.",
		Tags:   []string{tagSettings},
		Errors: []int{http.StatusPreconditionFailed, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.patchSettings)
	s.allowMergePatch(http.MethodPatch, "/v1/settings")

	Register(s, huma.Operation{
		OperationID: "get-default-settings",
		Method:      http.MethodGet,
		Path:        "/v1/settings/defaults",
		Summary:     "Get the default settings",
		Description: "Returns the built-in lowest settings layer.",
		Tags:        []string{tagSettings},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.getDefaultSettings)

	Register(s, huma.Operation{
		OperationID: "get-settings-schema",
		Method:      http.MethodGet,
		Path:        "/v1/settings/schema",
		Summary:     "Describe the settings fields",
		Description: "Returns one entry per settings field with its type, group, unit, numeric bounds, accepted values and " +
			"the levels (default, global, queue, task) that may set it, for building generic settings forms.",
		Tags: []string{tagSettings},
	}, s.getSettingsSchema)
}

type settingsSchemaOutput struct {
	Body settings.Schema
}

func (s *Server) getSettingsSchema(_ context.Context, _ *struct{}) (*settingsSchemaOutput, error) {
	return &settingsSchemaOutput{Body: settings.DescribeSchema()}, nil
}

// allowMergePatch documents application/merge-patch+json as an accepted
// request media type of a registered operation (huma decodes any +json
// media type as JSON).
func (s *Server) allowMergePatch(method, path string) { //nolint:unparam // PATCH is the only caller today; the switch below also serves PUT and POST
	item := s.api.OpenAPI().Paths[path]
	if item == nil {
		panic("api: allowMergePatch: unknown path " + path) // programming error caught at startup
	}
	var op *huma.Operation
	switch method {
	case http.MethodPatch:
		op = item.Patch
	case http.MethodPut:
		op = item.Put
	case http.MethodPost:
		op = item.Post
	default:
	}
	if op == nil || op.RequestBody == nil || op.RequestBody.Content["application/json"] == nil {
		panic("api: allowMergePatch: no JSON request body for " + method + " " + path) // programming error caught at startup
	}
	op.RequestBody.Content[mergePatchContentType] = op.RequestBody.Content["application/json"]
}

func (s *Server) getSettings(ctx context.Context, _ *struct{}) (*settingsOutput, error) {
	if s.settings == nil {
		return nil, errServicesUnavailable()
	}
	g, err := s.settings.Global(ctx)
	if err != nil {
		return nil, s.fail(ctx, "get-settings", err, locBody)
	}
	return newSettingsOutput(g), nil
}

func (s *Server) replaceSettings(ctx context.Context, in *struct {
	IfMatch string `header:"If-Match" doc:"ETag of the settings last read; the update fails with 412 (settings.changed) if they changed since"`
	Body    settings.Settings
},
) (*settingsOutput, error) {
	if s.settings == nil {
		return nil, errServicesUnavailable()
	}
	g, err := s.settings.ReplaceGlobalIfMatch(ctx, in.IfMatch, in.Body)
	if err != nil {
		return nil, s.fail(ctx, "replace-settings", err, locBody)
	}
	return newSettingsOutput(g), nil
}

func (s *Server) patchSettings(ctx context.Context, in *struct {
	IfMatch string `header:"If-Match" doc:"ETag of the settings last read; the update fails with 412 (settings.changed) if they changed since"`
	Body    SettingsPatch
},
) (*settingsOutput, error) {
	if s.settings == nil {
		return nil, errServicesUnavailable()
	}
	g, err := s.settings.PatchGlobalIfMatch(ctx, in.IfMatch, in.Body)
	if err != nil {
		return nil, s.fail(ctx, "patch-settings", err, locBody)
	}
	return newSettingsOutput(g), nil
}

func (s *Server) getDefaultSettings(_ context.Context, _ *struct{}) (*settingsOutput, error) {
	if s.settings == nil {
		return nil, errServicesUnavailable()
	}
	return newSettingsOutput(s.settings.Defaults()), nil
}
