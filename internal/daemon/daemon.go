// Package daemon is the composition root of the dlm daemon: it wires
// configuration, logging, storage, the event bus, services and the API
// server together and runs them until shutdown. Only cmd/dlm (via the cli
// "serve" command) uses it.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/browser"
	"github.com/sabadia/dlm-server/internal/client"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/browserx"
	"github.com/sabadia/dlm-server/internal/engine/bt"
	"github.com/sabadia/dlm-server/internal/engine/dash"
	"github.com/sabadia/dlm-server/internal/engine/ed2k"
	"github.com/sabadia/dlm-server/internal/engine/ftp"
	"github.com/sabadia/dlm-server/internal/engine/hls"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	ytdlpengine "github.com/sabadia/dlm-server/internal/engine/ytdlp"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/ffmpeg"
	"github.com/sabadia/dlm-server/internal/instance"
	"github.com/sabadia/dlm-server/internal/logging"
	"github.com/sabadia/dlm-server/internal/mcp"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/platform"
	"github.com/sabadia/dlm-server/internal/plugin"
	"github.com/sabadia/dlm-server/internal/postproc"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/refresh"
	"github.com/sabadia/dlm-server/internal/scheduler"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
	"github.com/sabadia/dlm-server/internal/webhook"
	"github.com/sabadia/dlm-server/internal/ytdlp"
)

// Run starts the daemon and blocks until ctx is cancelled. It returns nil
// on a clean shutdown. stderr receives logs when file logging is disabled
// or log.stderr is set.
func Run(ctx context.Context, cfg *config.Config, version string, stderr io.Writer) (err error) {
	if err := cfg.EnsureDirs(); err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	// One daemon per configuration directory: the lock also keeps two
	// daemons started with port 0 from sharing a database.
	lock, err := instance.Acquire(cfg.Paths.ConfigDir)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	defer func() {
		if rerr := lock.Release(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("daemon: %w", rerr))
		}
	}()
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	life := &lifecycle{cancel: cancelRun}
	startedAt := time.Now()
	logger, err := logging.New(cfg.Log, cfg.Paths.LogDir, stderr)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	defer func() {
		if cerr := logger.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	log := logger.Logger

	token, err := auth.LoadOrCreateToken(cfg.TokenFile())
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	store, err := sqlite.Open(ctx, cfg.DatabaseFile(), sqlite.Options{Logger: log})
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("daemon: close store: %w", cerr))
		}
	}()

	recovered, err := service.RecoverInterrupted(ctx, store, time.Now, log)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	bus := events.New(events.Options{Logger: log})
	// Portable mode (--home) keeps the key beside the data instead of in the
	// OS keyring.
	cookieKey, err := cookies.LoadOrCreateKey(cookies.KeyOptions{Dir: cfg.Paths.ConfigDir, FileOnly: cfg.Home != "", Logger: log})
	if err != nil {
		bus.Close()
		return fmt.Errorf("daemon: %w", err)
	}
	cookieCipher, err := cookies.NewCipher(cookieKey)
	if err != nil {
		bus.Close()
		return fmt.Errorf("daemon: %w", err)
	}
	jar := cookies.NewStore(store.Cookies(), cookieCipher, cookies.Options{Logger: log})
	// Stored logins answer the Basic/Digest challenges of every client.
	credentials := service.NewCredentialService(service.Deps{Store: store, Logger: log}, cookieCipher)
	netOpts := netclient.Options{Logger: log, CookieJar: jar, Credentials: credentials}
	if cfg.HTTP.TLSImpersonation {
		netOpts.Fallback = netclient.NewImpersonation(netclient.ImpersonationOptions{Logger: log, TTL: cfg.HTTP.ImpersonationTTL})
	}
	clients := netclient.New(netOpts)
	defer func() { _ = clients.Close() }() // only releases idle connections
	// The headless browser starts on demand; creating the manager costs nothing.
	browsers := browser.NewManager(browser.Options{
		Path: cfg.Browser.Path, CacheDir: cfg.Paths.CacheDir, ProfilesDir: cfg.BrowserProfilesDir(),
		AutoDownload: cfg.Browser.AutoDownload, MaxBrowsers: cfg.Browser.MaxBrowsers, IdleTimeout: cfg.Browser.IdleTimeout,
		NoSandbox: cfg.Browser.NoSandbox, Logger: log,
	})
	defer func() { _ = browsers.Close() }() // stops the running browsers
	resolver := browser.NewResolver(browser.ResolverOptions{
		Manager: browsers, Cookies: jar, Dir: cfg.BrowserStagingDir(), Logger: log,
	})
	// ffmpeg is found (or downloaded) the first time a stream needs merging.
	ffmpegLocator := ffmpeg.NewLocator(ffmpeg.Options{
		Path: cfg.FFmpeg.Path, CacheDir: cfg.Paths.CacheDir, AutoDownload: cfg.FFmpeg.AutoDownload, Logger: log,
	})
	env := engine.Env{Logger: log, Clock: time.Now, Clients: clients}
	// Specific engines first: the first one that matches a source wins, and
	// the HTTP engine matches every http(s) URL.
	list := []engine.Engine{
		browserx.New(resolver), // dlm-browser:// files
		hls.New(env, ffmpegLocator),
		dash.New(env, ffmpegLocator),
		ftp.New(env),
	}
	if cfg.Torrent.Enabled {
		// The BitTorrent client is shared by all torrent tasks and starts when
		// the first one needs it; closing it ends the downloads using it (the
		// scheduler has stopped them by then).
		torrents := bt.New(env, bt.Config{
			ListenPort: cfg.Torrent.ListenPort, ListenHost: cfg.Torrent.ListenHost,
			DisableDHT: !cfg.Torrent.DHT, DisablePEX: !cfg.Torrent.PEX, DisablePortForwarding: !cfg.Torrent.PortForwarding,
			UploadLimit: cfg.Torrent.UploadLimit, MetadataTimeout: cfg.Torrent.MetadataTimeout,
		})
		defer func() { _ = torrents.Close() }()
		list = append(list, torrents)
	}
	if cfg.ED2K.Enabled {
		// Like the BitTorrent client, the ed2k client starts with the first task.
		ed2kEngine := ed2k.New(env, ed2k.Config{
			ListenPort: cfg.ED2K.ListenPort, Servers: cfg.ED2K.Servers, ServerMet: cfg.ED2K.ServerMet,
			EnableKad: cfg.ED2K.Kad, EnableUPnP: cfg.ED2K.UPnP, UploadLimitKB: cfg.ED2K.UploadLimitKB,
		})
		defer func() { _ = ed2kEngine.Close() }()
		list = append(list, ed2kEngine)
	}
	// yt-dlp is found (or downloaded) the first time a task of the ytdlp
	// resolver runs; the engine only takes sources that ask for it.
	ytdlpLocator := ytdlp.NewLocator(ytdlp.Options{
		Path: cfg.Ytdlp.Path, CacheDir: cfg.Paths.CacheDir, AutoDownload: cfg.Ytdlp.AutoDownload, Logger: log,
	})
	list = append(list, ytdlpengine.New(env, ytdlpLocator, ffmpegLocator, ytdlpengine.WithCookies(jar.ForURL)))
	list = append(list, httpx.New(env))
	engines, err := engine.NewRegistry(list...)
	if err != nil {
		bus.Close()
		return fmt.Errorf("daemon: %w", err)
	}
	downloadDir, err := platform.UserDownloadsDir()
	if err != nil {
		bus.Close()
		return fmt.Errorf("daemon: %w", err)
	}
	defaults := func() settings.Settings { return settings.Defaults(downloadDir) }
	limits := ratelimit.NewManager(0)
	resolves := engine.NewResolveCache(engine.CacheOptions{})
	defer resolves.Close()
	// Construction order: the runner needs the store; the scheduler needs
	// the runner and the store; the services need the scheduler. One
	// TaskLocks instance is shared by the runner and every service.
	// The runner reports downloaded bytes to the scheduler's quota meter.
	locks := service.NewTaskLocks()
	quotas := scheduler.NewQuotaMeter(time.Now)
	var services *Services // assigned below, before the scheduler runs
	runner := service.NewRunner(service.RunnerDeps{
		Store:     store,
		Engines:   engines,
		Limits:    limits,
		Bus:       bus,
		Defaults:  defaults,
		Logger:    log,
		TaskLocks: locks,
		Bytes:     quotas,
		Post: postproc.Default(postproc.Options{Logger: log, Torrents: postproc.TorrentAdderFunc(func(ctx context.Context, in postproc.NewTorrent) error {
			return services.Tasks.AddTorrentFile(ctx, in)
		})}),
		// The runner sorts downloads into category directories; the service is
		// stateless, so it can be a second instance.
		Categories: service.NewCategoryService(service.Deps{Store: store}),
		// The refresh service needs the scheduler, which needs the runner:
		// the hook reaches it through services, assigned below.
		LinkExpired: func(ctx context.Context, taskID string, cause error) error {
			return services.Refresh.HandleExpired(ctx, taskID, cause)
		},
	})
	sched := scheduler.New(scheduler.Options{
		Store:    store,
		Runner:   runner,
		Defaults: defaults,
		Logger:   log,
		Bus:      bus,
		Limits:   service.NewLimitsController(service.Deps{Store: store, Limits: limits, Defaults: defaults}),
		Quotas:   quotas,
		Actions:  scheduler.SystemActions{},
		StopQueue: func(ctx context.Context, queueID string) error {
			_, err := services.Queues.Stop(ctx, queueID)
			return err
		},
	})
	// Plugins make their own requests through a client that has no cookie jar
	// and no stored logins.
	pluginClients := netclient.New(netclient.Options{Logger: log})
	defer func() { _ = pluginClients.Close() }()
	pluginHTTP, err := pluginClients.Client(defaults())
	if err != nil {
		bus.Close()
		return fmt.Errorf("daemon: %w", err)
	}
	plugins := plugin.NewManager(plugin.Options{Dir: cfg.PluginsDir(), Store: store, Bus: bus, Logger: log, HTTP: pluginHTTP})
	if err := plugins.Load(ctx); err != nil {
		bus.Close()
		return fmt.Errorf("daemon: %w", err)
	}
	services = NewServices(service.Deps{
		Store:     store,
		Engines:   engines,
		Resolves:  resolves,
		Scheduler: sched,
		Bus:       bus,
		Limits:    limits,
		Runner:    runner,
		Defaults:  defaults,
		Logger:    log,
		TaskLocks: locks,
		Plugins:   plugins,
	}, refresh.PageReload{Resolver: resolver}, refresh.Plugin{Plugins: plugins})
	services.Plugins = service.NewPluginService(plugins)
	services.Browser = service.NewBrowserService(resolver, services.Tasks)
	// The dispatcher reads its endpoints through the webhook service, which in
	// turn tests endpoints through the dispatcher.
	webhooks := webhook.New(bus, webhook.FuncSource{
		Load:        func(ctx context.Context) ([]webhook.Target, error) { return services.Webhooks.Targets(ctx) },
		VersionFunc: func() uint64 { return services.Webhooks.Version() },
	}, webhook.Options{Logger: log})
	services.Credentials = credentials
	services.Webhooks = service.NewWebhookService(service.Deps{Store: store, Logger: log}, cookieCipher, webhooks)
	if err := services.Settings.SyncLimits(ctx); err != nil {
		bus.Close()
		return fmt.Errorf("daemon: %w", err)
	}
	system := &systemSource{
		cfg: cfg, version: version, startedAt: startedAt, downloadDir: downloadDir, engines: engines.Names(),
		ffmpeg: ffmpegLocator, ytdlp: ytdlpLocator, browsers: browsers,
	}
	srv, err := BuildServer(cfg, api.Deps{
		Logger: log, Version: version, Token: token, Bus: bus,
		Settings: services.Settings, Queues: services.Queues, Tasks: services.Tasks,
		Cookies:     service.NewCookieService(jar),
		Capture:     service.NewCaptureService(services.Tasks, jar, services.Refresh),
		Refresh:     services.Refresh,
		Categories:  services.Categories,
		Webhooks:    services.Webhooks,
		Credentials: services.Credentials,
		Plugins:     services.Plugins,
		MCP: mcp.Handler(mcp.New(mcp.Deps{
			Tasks: services.Tasks, Queues: services.Queues, Settings: services.Settings, Bus: bus, Version: version, Logger: log,
		})),
		Browser:   services.Browser,
		System:    system,
		Lifecycle: life,
	})
	if err != nil {
		bus.Close()
		return err
	}
	ln, err := srv.Listen(ctx)
	if err != nil {
		bus.Close()
		return fmt.Errorf("daemon: %w", err)
	}
	if err := publishRuntime(cfg, ln, version, startedAt, system); err != nil {
		_ = ln.Close()
		bus.Close()
		return err
	}
	defer func() {
		if rerr := instance.Remove(cfg.Paths.ConfigDir, os.Getpid()); rerr != nil {
			log.WarnContext(ctx, "daemon: remove the runtime file", "error", rerr)
		}
	}()

	log.InfoContext(ctx, "dlm daemon starting",
		"version", version,
		"address", ln.Addr().String(),
		"tls", cfg.TLSEnabled(),
		"config_dir", cfg.Paths.ConfigDir,
		"data_dir", cfg.Paths.DataDir,
		"cache_dir", cfg.Paths.CacheDir,
		"log_dir", cfg.Paths.LogDir,
		"database", cfg.DatabaseFile(),
		"token_file", cfg.TokenFile(),
		"recovered_tasks", recovered,
		"engines", engines.Names(),
	)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return bus.Run(gctx) })
	sweepCtx, stopSweep := context.WithCancel(gctx)
	defer stopSweep()
	g.Go(func() error { return resolves.Run(sweepCtx, time.Minute) })
	g.Go(func() error { return jar.Run(sweepCtx, time.Hour) })
	g.Go(func() error { return services.Refresh.Run(sweepCtx) })
	g.Go(func() error { return resolver.Run(sweepCtx) })
	g.Go(func() error { return webhooks.Run(sweepCtx) })
	g.Go(func() error { return plugins.Run(sweepCtx) })
	// The scheduler starts queued tasks, including the ones recovered
	// above; on shutdown it requeues its running tasks and waits for them
	// (the store stays open until g.Wait returns).
	g.Go(func() error { return sched.Run(gctx) })
	g.Go(func() error {
		// The bus outlives the server so in-flight requests can still
		// publish; closing it also stops bus.Run.
		defer bus.Close()
		defer stopSweep()
		return srv.Serve(gctx, ln)
	})
	if err := g.Wait(); err != nil {
		log.ErrorContext(ctx, "dlm daemon stopped with error", "error", err)
		return fmt.Errorf("daemon: serve: %w", err)
	}
	log.InfoContext(ctx, "dlm daemon stopped")
	if life.restart.Load() {
		return ErrRestart
	}
	return nil
}

// publishRuntime tells clients where the daemon really listens (see package
// instance) and records the address for get-system-info.
func publishRuntime(cfg *config.Config, ln net.Listener, version string, startedAt time.Time, system *systemSource) error {
	bound := *cfg
	bound.Server.Address = ln.Addr().String()
	url, err := client.BaseURL(&bound)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	info := instance.Info{
		PID: os.Getpid(), Address: bound.Server.Address, URL: url, Version: version,
		APIVersion: api.APIVersion, StartedAt: startedAt.UTC(),
	}
	if cfg.TLSEnabled() {
		if info.TLSFingerprint, err = instance.Fingerprint(cfg.Server.TLSCert, cfg.Server.TLSKey); err != nil {
			return fmt.Errorf("daemon: %w", err)
		}
	}
	system.address.Store(bound.Server.Address)
	if err := instance.Write(cfg.Paths.ConfigDir, info); err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	return nil
}

// Services are the application use-cases the adapters (REST API, MCP) call.
type Services struct {
	Settings *service.SettingsService
	Queues   *service.QueueService
	Tasks    *service.TaskService
	Refresh  *service.RefreshService
	// Categories sorts downloads into directories.
	Categories *service.CategoryService
	// Webhooks is nil until the daemon wires it (it needs the secret cipher).
	Webhooks *service.WebhookService
	// Credentials holds the stored site logins; wired by the daemon (it needs the secret cipher).
	Credentials *service.CredentialService
	// Plugins manages the installed plugins; wired by the daemon (it needs the plugin manager).
	Plugins *service.PluginService
	// Browser is nil until the daemon wires it (it needs the headless browser).
	Browser *service.BrowserService
}

// NewServices builds every service on the same dependencies. extra are
// additional link-refresh strategies (page-reload needs the browser, which
// only the daemon has).
func NewServices(d service.Deps, extra ...refresh.Strategy) *Services {
	return &Services{
		Settings:   service.NewSettingsService(d),
		Queues:     service.NewQueueService(d),
		Tasks:      service.NewTaskService(d),
		Refresh:    service.NewRefreshService(d, append([]refresh.Strategy{refresh.OriginRedirect{Engines: d.Engines}}, extra...)...),
		Categories: service.NewCategoryService(d),
	}
}

// BuildServer constructs the API server and registers every operation. It
// is shared by Run and the CLI parity test, so any operation
// registered here is automatically checked for a matching CLI command.
func BuildServer(cfg *config.Config, deps api.Deps) (*api.Server, error) {
	srv, err := api.New(cfg, deps)
	if err != nil {
		return nil, fmt.Errorf("daemon: build api server: %w", err)
	}
	return srv, nil
}
