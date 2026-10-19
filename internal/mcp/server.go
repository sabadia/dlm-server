package mcp

import (
	"log/slog"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/service"
)

// Deps are what the tools call.
type Deps struct {
	Tasks    *service.TaskService
	Queues   *service.QueueService
	Settings *service.SettingsService
	// Bus delivers the events wait_for_download turns into progress
	// notifications.
	Bus     *events.Bus
	Version string
	Logger  *slog.Logger
}

// New builds the MCP server with every tool registered.
func New(d Deps) *sdk.Server {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	version := d.Version
	if version == "" {
		version = "dev"
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: "dlm", Title: "dlm download manager", Version: version}, &sdk.ServerOptions{
		Instructions: "Manage downloads of the dlm daemon: add links with add_download, follow them with list_downloads, " +
			"get_download or wait_for_download, and organize them with queues. Sizes are in bytes; a size of -1 is unknown.",
		Logger: d.Logger,
	})
	t := &tools{d: d}
	t.registerTasks(srv)
	t.registerQueues(srv)
	t.registerSettings(srv)
	return srv
}

// Handler serves the server over streamable HTTP. The caller authenticates
// requests.
func Handler(srv *sdk.Server) http.Handler {
	return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
}

type tools struct{ d Deps }

func boolPtr(b bool) *bool { return &b }

var (
	readOnly    = &sdk.ToolAnnotations{ReadOnlyHint: true}
	idempotent  = &sdk.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false)}
	additive    = &sdk.ToolAnnotations{DestructiveHint: boolPtr(false)}
	destructive = &sdk.ToolAnnotations{DestructiveHint: boolPtr(true)}
)
