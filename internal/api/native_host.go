package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/nativehost"
	"github.com/sabadia/dlm-server/internal/platform"
)

const tagNativeHost = "native-host"

// NativeHostStatus is the response of get-native-host.
type NativeHostStatus struct {
	Launcher       string              `json:"launcher" doc:"The script browsers start; the manifests point to it."`
	LauncherExists bool                `json:"launcherExists"`
	Browsers       []nativehost.Result `json:"browsers" doc:"Per browser: whether the host is registered (installed or absent) and where."`
	AllowedOrigins []string            `json:"allowedOrigins" doc:"The chrome-extension:// origins the daemon accepts (server.allowed_origins); an extension outside the list is refused."`
}

// NativeHostRequest is the request of update-native-host.
type NativeHostRequest struct {
	Action       string   `json:"action" enum:"install,uninstall" doc:"install registers the host with the browsers, uninstall removes it."`
	Browsers     []string `json:"browsers,omitempty" doc:"Limit to these browsers (see get-native-host for the names); default: all."`
	ExtensionIDs []string `json:"extensionIds,omitempty" doc:"More Chromium extension IDs or chrome-extension://ID/ origins to allow (install), besides the published extension; default: the chrome-extension:// entries of server.allowed_origins."`
	FirefoxIDs   []string `json:"firefoxIds,omitempty" doc:"More Firefox add-on IDs to allow (install), besides the published extension."`
}

func (s *Server) registerNativeHost() {
	Register(s, huma.Operation{
		OperationID: "get-native-host",
		Method:      http.MethodGet,
		Path:        "/v1/native-host",
		Summary:     "Browser extension host status",
		Description: "Whether the Native Messaging host the browser extension talks to is registered with each browser, for the user the daemon runs as. " +
			"Only from the machine the daemon runs on (403 native_host.local_only otherwise).",
		Tags:   []string{tagNativeHost},
		Errors: []int{http.StatusForbidden},
	}, s.getNativeHost)

	Register(s, huma.Operation{
		OperationID: "update-native-host",
		Method:      http.MethodPost,
		Path:        "/v1/native-host",
		Summary:     "Register or remove the browser extension host",
		Description: "Writes (install) or removes (uninstall) the Native Messaging host registration of the selected browsers, as `dlm native-host install|uninstall` does. " +
			"The published DLM extension is always registered; extensionIds and firefoxIds add others (another Chromium extension also needs its origin in server.allowed_origins). Only from the machine the daemon runs on (403 native_host.local_only otherwise).",
		Tags:   []string{tagNativeHost},
		Errors: []int{http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}, s.updateNativeHost)
}

func localOnly(ctx context.Context) error {
	if !Local(ctx) {
		return newProblem(http.StatusForbidden, "this only works from the machine the daemon runs on").withCode("native_host.local_only", nil)
	}
	return nil
}

func (s *Server) nativeHostInstaller(ctx context.Context) (*nativehost.Installer, error) {
	if s.nativeHost != nil {
		return s.nativeHost()
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err //nolint:wrapcheck // reported as a 500 with the OS message
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	addr := s.cfg.Server.Address
	if s.system != nil {
		if a := s.system.SystemInfo(ctx).Address; a != "" {
			addr = a
		}
	}
	scheme := "http"
	if s.cfg.TLSEnabled() {
		scheme = "https"
	}
	return &nativehost.Installer{
		Executable: exe, ConfigDir: s.cfg.Paths.ConfigDir, DataDir: s.cfg.Paths.ConfigDir, ServerURL: scheme + "://" + addr,
	}, nil
}

func parseBrowsers(names []string) ([]platform.Browser, error) {
	var out []platform.Browser
	for _, n := range names {
		b := platform.Browser(strings.ToLower(n))
		if !b.Valid() {
			return nil, newProblem(http.StatusUnprocessableEntity, "unknown browser "+n,
				&huma.ErrorDetail{Message: "unknown browser", Location: "body.browsers", Value: n})
		}
		if !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (s *Server) getNativeHost(ctx context.Context, _ *struct{}) (*struct{ Body NativeHostStatus }, error) {
	if err := localOnly(ctx); err != nil {
		return nil, err
	}
	in, err := s.nativeHostInstaller(ctx)
	if err != nil {
		return nil, s.fail(ctx, "get-native-host", err, locBody)
	}
	results, err := in.Status(nil)
	if err != nil {
		return nil, s.fail(ctx, "get-native-host", err, locBody)
	}
	_, statErr := os.Stat(in.LauncherPath())
	origins := slices.Clone(s.cfg.Server.AllowedOrigins)
	if origins == nil {
		origins = []string{}
	}
	if results == nil {
		results = []nativehost.Result{}
	}
	return &struct{ Body NativeHostStatus }{Body: NativeHostStatus{
		Launcher: in.LauncherPath(), LauncherExists: statErr == nil, Browsers: results, AllowedOrigins: origins,
	}}, nil
}

func (s *Server) updateNativeHost(ctx context.Context, in *struct{ Body NativeHostRequest }) (*struct{ Body []nativehost.Result }, error) {
	if err := localOnly(ctx); err != nil {
		return nil, err
	}
	browsers, err := parseBrowsers(in.Body.Browsers)
	if err != nil {
		return nil, err
	}
	inst, err := s.nativeHostInstaller(ctx)
	if err != nil {
		return nil, s.fail(ctx, "update-native-host", err, locBody)
	}
	var results []nativehost.Result
	if in.Body.Action == "uninstall" {
		results, err = inst.Uninstall(browsers)
	} else {
		t := nativehost.Target{Browsers: browsers, FirefoxIDs: in.Body.FirefoxIDs}
		ids := in.Body.ExtensionIDs
		if len(ids) == 0 {
			for _, o := range s.cfg.Server.AllowedOrigins {
				if strings.HasPrefix(o, "chrome-extension://") {
					ids = append(ids, o)
				}
			}
		}
		for _, id := range ids {
			origin, oerr := nativehost.ChromiumOrigin(id)
			if oerr != nil {
				return nil, newProblem(http.StatusUnprocessableEntity, oerr.Error(),
					&huma.ErrorDetail{Message: oerr.Error(), Location: "body.extensionIds", Value: id})
			}
			if !slices.Contains(t.ChromiumOrigins, origin) {
				t.ChromiumOrigins = append(t.ChromiumOrigins, origin)
			}
		}
		results, err = inst.Install(t.WithBuiltinIDs())
	}
	if err != nil {
		anyInstalled := slices.ContainsFunc(results, func(r nativehost.Result) bool { return r.Status == nativehost.StatusInstalled })
		if in.Body.Action != "uninstall" && !anyInstalled {
			// "nothing installed: give an extension ID" is the caller's input.
			return nil, newProblem(http.StatusUnprocessableEntity, err.Error(),
				&huma.ErrorDetail{Message: err.Error(), Location: "body.extensionIds"}).withCode("native_host.nothing_to_install", nil)
		}
		return nil, s.fail(ctx, "update-native-host", err, locBody)
	}
	if results == nil {
		results = []nativehost.Result{}
	}
	return &struct{ Body []nativehost.Result }{Body: results}, nil
}
