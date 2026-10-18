package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/service"
)

const (
	// Title is the OpenAPI document title.
	Title = "dlm-server API"
	// SecurityScheme is the name of the bearer-token security scheme in the
	// OpenAPI document.
	SecurityScheme = "bearer"
	// MaxBodyBytes is the default request body limit.
	MaxBodyBytes int64 = 1 << 20

	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

// Deps are the runtime dependencies of the API server.
type Deps struct {
	// Logger receives access and error logs. Nil discards them.
	Logger *slog.Logger
	// Version is reported by /v1/health and the OpenAPI document.
	Version string
	// Token is the bearer token clients must present. It must not be empty.
	Token string
	// Bus is the event bus streamed by stream-events. When nil the operation
	// is still registered (OpenAPI document, CLI parity) but answers 503.
	Bus *events.Bus
	// BootID identifies this daemon process in SSE event IDs
	// ("<bootID>-<seq>"). It must be alphanumeric; empty generates a random
	// one, which is what the daemon wants (one per process).
	BootID string
	// EventsHeartbeat is the interval between SSE keep-alive comments. Zero
	// selects DefaultEventsHeartbeat.
	EventsHeartbeat time.Duration

	// Settings, Queues and Tasks are the services behind the settings, queue
	// and task operations. The operations are always registered (OpenAPI
	// document, CLI parity); while a service is nil they answer 503.
	Settings *service.SettingsService
	Queues   *service.QueueService
	Tasks    *service.TaskService
	// Cookies is the service behind the cookie operations; like the others
	// it answers 503 while nil.
	Cookies *service.CookieService
	// Categories is the service behind the category operations; 503 while nil.
	Categories *service.CategoryService
	// Webhooks is the service behind the webhook operations; 503 while nil.
	Webhooks *service.WebhookService
	// Credentials is the service behind the credential operations; 503 while nil.
	Credentials *service.CredentialService
	// Capture is the service behind capture-download; 503 while nil.
	Capture *service.CaptureService
	// Refresh is the service behind the refresh operations; 503 while nil.
	Refresh *service.RefreshService
	// Browser is the service behind the /v1/browser operations; 503 while nil.
	Browser *service.BrowserService
}

// Server is the REST API server.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	version string
	token   []byte
	mux     *http.ServeMux
	api     huma.API
	handler http.Handler

	bus       *events.Bus
	bootID    string
	heartbeat time.Duration

	settings    *service.SettingsService
	queues      *service.QueueService
	tasks       *service.TaskService
	cookies     *service.CookieService
	categories  *service.CategoryService
	webhooks    *service.WebhookService
	credentials *service.CredentialService
	capture     *service.CaptureService
	refresh     *service.RefreshService
	browser     *service.BrowserService

	mu     sync.RWMutex
	public map[string]struct{} // mux patterns ("GET /v1/health") that need no token
}

// New builds the server and registers the built-in operations.
func New(cfg *config.Config, deps Deps) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("api: nil config")
	}
	if deps.Token == "" {
		return nil, errors.New("api: empty token")
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	version := deps.Version
	if version == "" {
		version = "dev"
	}
	bootID := deps.BootID
	if bootID == "" {
		var err error
		if bootID, err = newBootID(); err != nil {
			return nil, err
		}
	} else if !validBootID(bootID) {
		return nil, fmt.Errorf("api: invalid boot id %q: must be 1-64 ASCII letters or digits", bootID)
	}
	heartbeat := deps.EventsHeartbeat
	if heartbeat <= 0 {
		heartbeat = DefaultEventsHeartbeat
	}

	s := &Server{
		cfg:     cfg,
		log:     log,
		version: version,
		token:   []byte(deps.Token),
		mux:     http.NewServeMux(),
		public:  make(map[string]struct{}),

		bus:       deps.Bus,
		bootID:    bootID,
		heartbeat: heartbeat,

		settings:    deps.Settings,
		queues:      deps.Queues,
		tasks:       deps.Tasks,
		cookies:     deps.Cookies,
		categories:  deps.Categories,
		webhooks:    deps.Webhooks,
		credentials: deps.Credentials,
		capture:     deps.Capture,
		refresh:     deps.Refresh,
		browser:     deps.Browser,
	}
	s.api = humago.New(s.mux, humaConfig(version))

	handler, err := s.buildHandler()
	if err != nil {
		return nil, err
	}
	s.handler = handler

	registerSettingsSchemas(s.api.OpenAPI().Components.Schemas)
	s.registerHealth()
	s.registerEvents()
	s.registerTasks()
	s.registerQueues()
	s.registerSettings()
	s.registerCookies()
	s.registerCategories()
	s.registerWebhooks()
	s.registerCredentials()
	s.registerPortable()
	s.registerCapture()
	s.registerRefresh()
	s.registerBrowser()
	return s, nil
}

func humaConfig(version string) huma.Config {
	hc := huma.DefaultConfig(Title, version)
	hc.Info.Description = "REST API of the dlm download manager daemon."
	hc.OpenAPIPath = "/openapi"
	hc.DocsPath = "/docs"
	hc.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		SecurityScheme: {
			Type:        "http",
			Scheme:      "bearer",
			Description: "Token from the daemon's token file (see `dlm` docs).",
		},
	}
	// Every operation requires the token unless it overrides this with an
	// empty security requirement (see RegisterPublic).
	hc.Security = []map[string][]string{{SecurityScheme: {}}}
	return hc
}

// Handler returns the fully wrapped HTTP handler (middleware + routes).
func (s *Server) Handler() http.Handler { return s.handler }

// API returns the huma API, giving access to the OpenAPI document.
// Operations must be added with Register or RegisterPublic.
func (s *Server) API() huma.API { return s.api }

// Run listens on cfg.Server.Address (HTTPS when TLS is configured) and serves
// until ctx is cancelled, then shuts down gracefully. It returns nil on a
// clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Server.Address)
	if err != nil {
		return fmt.Errorf("api: listen %q: %w", s.cfg.Server.Address, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves on ln until ctx is cancelled, then shuts down gracefully within
// cfg.Server.ShutdownTimeout. ln is closed when Serve returns. It returns nil
// on a clean shutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// Request contexts are detached from ctx so that in-flight requests can
	// finish during graceful shutdown, but they are cancelled as soon as
	// shutdown begins so long-lived streams (SSE) end promptly.
	baseCtx, cancelBase := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBase()

	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		// No WriteTimeout: SSE responses are long-lived.
		ErrorLog:    slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		BaseContext: func(net.Listener) context.Context { return baseCtx },
	}
	srv.RegisterOnShutdown(cancelBase)

	tlsOn := s.cfg.TLSEnabled()
	if tlsOn {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	errCh := make(chan error, 1)
	go func() {
		var err error
		if tlsOn {
			err = srv.ServeTLS(ln, s.cfg.Server.TLSCert, s.cfg.Server.TLSKey)
		} else {
			err = srv.Serve(ln)
		}
		errCh <- err
	}()
	s.log.InfoContext(ctx, "api server listening", "address", ln.Addr().String(), "tls", tlsOn)

	select {
	case err := <-errCh:
		// Serve stopped without a shutdown request.
		return fmt.Errorf("api: serve: %w", err)
	case <-ctx.Done():
	}

	s.log.InfoContext(ctx, "api server shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Server.ShutdownTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx) //nolint:contextcheck // shutdown must outlive the cancelled ctx
	if shutdownErr != nil {
		// Timed out: force-close the remaining connections.
		if err := srv.Close(); err != nil {
			s.log.WarnContext(ctx, "api server force close failed", "error", err)
		}
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("api: serve: %w", err)
	}
	if shutdownErr != nil {
		return fmt.Errorf("api: graceful shutdown: %w", shutdownErr)
	}
	s.log.InfoContext(ctx, "api server stopped")
	return nil
}
