package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
)

const tagPlugins = "plugins"

// PluginList is the response of list-plugins.
type PluginList struct {
	Plugins []*domain.Plugin `json:"plugins" doc:"Installed plugins, ordered by name."`
}

// InstallPluginBody is the request of install-plugin.
type InstallPluginBody struct {
	Source   string `json:"source" minLength:"1" doc:"An absolute directory on the daemon's host holding manifest.json and the script, or an https:// git URL (needs git on the daemon's PATH)."`
	Ref      string `json:"ref,omitempty" doc:"Branch or tag of a git source."`
	Replace  bool   `json:"replace,omitempty" doc:"Upgrade an installed plugin of the same name instead of refusing with 409."`
	Disabled bool   `json:"disabled,omitempty" doc:"Install the plugin switched off."`
}

// UpdatePluginBody is the request of update-plugin.
type UpdatePluginBody struct {
	Enabled *bool `json:"enabled,omitempty" doc:"Switch the plugin on or off."`
}

type pluginOutput struct {
	Body *domain.Plugin
}

type pluginPathInput struct {
	Name string `path:"name" doc:"Plugin name."`
}

func (s *Server) registerPlugins() {
	Register(s, huma.Operation{
		OperationID: "list-plugins",
		Method:      http.MethodGet,
		Path:        "/v1/plugins",
		Summary:     "List plugins",
		Description: "Plugins are JavaScript extensions that resolve links, refresh expired links and react to downloads starting, failing and completing.",
		Tags:        []string{tagPlugins},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.listPlugins)

	Register(s, huma.Operation{
		OperationID:   "install-plugin",
		Method:        http.MethodPost,
		Path:          "/v1/plugins",
		Summary:       "Install a plugin",
		Description:   "Copies a plugin from a directory, or clones it from a git repository, checks its manifest and script, and starts it. A plugin runs inside the daemon and may contact the hosts its manifest lists: install only plugins you trust.",
		Tags:          []string{tagPlugins},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.installPlugin)

	Register(s, huma.Operation{
		OperationID: "get-plugin",
		Method:      http.MethodGet,
		Path:        "/v1/plugins/{name}",
		Summary:     "Get a plugin",
		Tags:        []string{tagPlugins},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.getPlugin)

	Register(s, huma.Operation{
		OperationID: "update-plugin",
		Method:      http.MethodPatch,
		Path:        "/v1/plugins/{name}",
		Summary:     "Switch a plugin on or off",
		Tags:        []string{tagPlugins},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.updatePlugin)

	Register(s, huma.Operation{
		OperationID:   "delete-plugin",
		Method:        http.MethodDelete,
		Path:          "/v1/plugins/{name}",
		Summary:       "Uninstall a plugin",
		Description:   "Removes the plugin, its files and the data it stored.",
		Tags:          []string{tagPlugins},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.deletePlugin)
}

func (s *Server) listPlugins(ctx context.Context, _ *struct{}) (*struct{ Body PluginList }, error) {
	if s.plugins == nil {
		return nil, errServicesUnavailable()
	}
	return &struct{ Body PluginList }{Body: PluginList{Plugins: s.plugins.List(ctx)}}, nil
}

func (s *Server) installPlugin(ctx context.Context, in *struct{ Body InstallPluginBody }) (*pluginOutput, error) {
	if s.plugins == nil {
		return nil, errServicesUnavailable()
	}
	p, err := s.plugins.Install(ctx, domain.PluginInstall{
		Source: in.Body.Source, Ref: in.Body.Ref, Replace: in.Body.Replace, Disabled: in.Body.Disabled,
	})
	if err != nil {
		return nil, s.fail(ctx, "install-plugin", err, locBody)
	}
	return &pluginOutput{Body: p}, nil
}

func (s *Server) getPlugin(ctx context.Context, in *pluginPathInput) (*pluginOutput, error) {
	if s.plugins == nil {
		return nil, errServicesUnavailable()
	}
	p, err := s.plugins.Get(ctx, in.Name)
	if err != nil {
		return nil, s.fail(ctx, "get-plugin", err, locBody)
	}
	return &pluginOutput{Body: p}, nil
}

func (s *Server) updatePlugin(ctx context.Context, in *struct {
	Name string `path:"name" doc:"Plugin name."`
	Body UpdatePluginBody
},
) (*pluginOutput, error) {
	if s.plugins == nil {
		return nil, errServicesUnavailable()
	}
	var (
		p   *domain.Plugin
		err error
	)
	if in.Body.Enabled != nil {
		p, err = s.plugins.SetEnabled(ctx, in.Name, *in.Body.Enabled)
	} else {
		p, err = s.plugins.Get(ctx, in.Name)
	}
	if err != nil {
		return nil, s.fail(ctx, "update-plugin", err, locBody)
	}
	return &pluginOutput{Body: p}, nil
}

func (s *Server) deletePlugin(ctx context.Context, in *pluginPathInput) (*struct{}, error) {
	if s.plugins == nil {
		return nil, errServicesUnavailable()
	}
	if err := s.plugins.Delete(ctx, in.Name); err != nil {
		return nil, s.fail(ctx, "delete-plugin", err, locBody)
	}
	return &struct{}{}, nil
}
