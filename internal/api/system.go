package api

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// ToolStatus reports whether an external program dlm can use is available.
type ToolStatus struct {
	Name      string `json:"name" enum:"ffmpeg,yt-dlp,browser" doc:"Which tool."`
	Available bool   `json:"available" doc:"Whether a usable build was found (without downloading one)."`
	Path      string `json:"path,omitempty" doc:"Executable path."`
	Version   string `json:"version,omitempty" doc:"Version the tool reports, when known."`
	Source    string `json:"source,omitempty" enum:"configured,path,cache,download" doc:"Where the build comes from."`
	// AutoDownload says whether dlm fetches the tool itself when it is missing.
	AutoDownload bool   `json:"autoDownload" doc:"Whether dlm downloads the tool when it is missing and needed."`
	Error        string `json:"error,omitempty" doc:"Why no usable build was found."`
}

// SystemPaths are the directories and files the daemon uses.
type SystemPaths struct {
	Config    string `json:"config" doc:"Configuration directory (token, runtime.json)."`
	Data      string `json:"data" doc:"Data directory."`
	Cache     string `json:"cache" doc:"Cache directory (downloaded tools)."`
	Log       string `json:"log" doc:"Log directory."`
	Database  string `json:"database" doc:"SQLite database file."`
	Downloads string `json:"downloads" doc:"Default download directory."`
}

// tagSystem tags the operations about the daemon itself.
const tagSystem = "system"

// SystemInfo describes the daemon and the machine it runs on. The daemon
// provides everything except Uptime and Local, which the operation adds.
type SystemInfo struct {
	Version       string       `json:"version" doc:"Daemon version."`
	APIVersion    int          `json:"apiVersion" doc:"Version of the REST API."`
	MinClientAPI  int          `json:"minClientApi" doc:"Oldest API version a client may have been written for."`
	Commit        string       `json:"commit,omitempty" doc:"Source revision the daemon was built from."`
	GoVersion     string       `json:"goVersion" doc:"Go toolchain the daemon was built with."`
	OS            string       `json:"os" doc:"Operating system."`
	Arch          string       `json:"arch" doc:"CPU architecture."`
	PID           int          `json:"pid" doc:"Process id of the daemon."`
	StartedAt     time.Time    `json:"startedAt" doc:"When the daemon started."`
	UptimeSeconds int64        `json:"uptimeSeconds" doc:"Seconds since the daemon started."`
	Address       string       `json:"address" doc:"host:port the daemon listens on."`
	TLS           bool         `json:"tls" doc:"Whether the API is served over TLS."`
	Local         bool         `json:"local" doc:"Whether this request came from the machine the daemon runs on."`
	Paths         SystemPaths  `json:"paths"`
	Engines       []string     `json:"engines" doc:"Enabled download engines."`
	Tools         []ToolStatus `json:"tools" doc:"External programs dlm can use."`
	DatabaseBytes int64        `json:"databaseBytes" doc:"Size of the database file."`
	FSRoots       []string     `json:"fsRoots" doc:"Directories clients may browse with get-fs-browse (server.fs_roots); empty means browsing is off."`
}

// SystemSource provides the daemon's SystemInfo.
type SystemSource interface {
	SystemInfo(ctx context.Context) SystemInfo
}

// Lifecycle lets the API stop or restart the daemon.
type Lifecycle interface {
	// RequestShutdown starts a graceful shutdown; with restart the process
	// starts again once it has stopped.
	RequestShutdown(restart bool)
}

type localKey struct{}

// Local reports whether the request carrying ctx came from a loopback
// address, that is, from the machine the daemon runs on.
func Local(ctx context.Context) bool {
	ok, _ := ctx.Value(localKey{}).(bool)
	return ok
}

// isLoopbackRemote reports whether a request's RemoteAddr is a loopback IP.
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SystemInfoOutput is the response of get-system-info.
type SystemInfoOutput struct {
	Body SystemInfo
}

// LifecycleOutput is the response of shutdown-daemon and restart-daemon.
type LifecycleOutput struct {
	Body struct {
		Status string `json:"status" enum:"shutting_down,restarting" doc:"What the daemon is about to do."`
	}
}

func (s *Server) registerSystem() {
	Register(s, huma.Operation{
		OperationID: "get-system-info",
		Method:      http.MethodGet,
		Path:        "/v1/system/info",
		Summary:     "Describe the daemon and its machine",
		Description: "Reports the daemon's version, API version, build, operating system, process, listen address, " +
			"directories, enabled engines, the status of the external tools (ffmpeg, yt-dlp, browser; none is downloaded " +
			"by this call) and the database size. `local` tells whether this request came from the daemon's own machine, " +
			"for clients that offer features only meaningful there (folder pickers, opening files).",
		Tags:   []string{"system"},
		Errors: []int{http.StatusServiceUnavailable},
	}, s.getSystemInfo)

	Register(s, huma.Operation{
		OperationID:   "shutdown-daemon",
		Method:        http.MethodPost,
		Path:          "/v1/system/shutdown",
		Summary:       "Stop the daemon",
		Description:   "Starts a graceful shutdown: running downloads are stopped and requeued, then the daemon exits. The response is sent before it stops.",
		Tags:          []string{"system"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusServiceUnavailable},
	}, s.shutdownDaemon(false))

	Register(s, huma.Operation{
		OperationID:   "restart-daemon",
		Method:        http.MethodPost,
		Path:          "/v1/system/restart",
		Summary:       "Restart the daemon",
		Description:   "Gracefully stops the daemon and starts it again with the same arguments (the process keeps its pid where the platform allows). Clients reconnect using runtime.json or the same address.",
		Tags:          []string{"system"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusServiceUnavailable},
	}, s.shutdownDaemon(true))
}

func (s *Server) getSystemInfo(ctx context.Context, _ *struct{}) (*SystemInfoOutput, error) {
	if s.system == nil {
		return nil, errServicesUnavailable()
	}
	info := s.system.SystemInfo(ctx)
	info.Local = Local(ctx)
	if !info.StartedAt.IsZero() {
		info.UptimeSeconds = int64(time.Since(info.StartedAt).Seconds())
	}
	if info.Engines == nil {
		info.Engines = []string{}
	}
	if info.Tools == nil {
		info.Tools = []ToolStatus{}
	}
	info.FSRoots = s.fs.Roots()
	if info.FSRoots == nil {
		info.FSRoots = []string{}
	}
	return &SystemInfoOutput{Body: info}, nil
}

func (s *Server) shutdownDaemon(restart bool) func(context.Context, *struct{}) (*LifecycleOutput, error) {
	return func(ctx context.Context, _ *struct{}) (*LifecycleOutput, error) {
		if s.lifecycle == nil {
			return nil, errServicesUnavailable()
		}
		s.log.InfoContext(ctx, "api: daemon shutdown requested", "restart", restart)
		s.lifecycle.RequestShutdown(restart)
		out := &LifecycleOutput{}
		out.Body.Status = "shutting_down"
		if restart {
			out.Body.Status = "restarting"
		}
		return out, nil
	}
}
