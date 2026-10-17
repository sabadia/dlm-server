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
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/logging"
	"github.com/sabadia/dlm-server/internal/netclient"
	"github.com/sabadia/dlm-server/internal/platform"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/scheduler"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

// Run starts the daemon and blocks until ctx is cancelled. It returns nil
// on a clean shutdown. stderr receives logs when file logging is disabled
// or log.stderr is set.
func Run(ctx context.Context, cfg *config.Config, version string, stderr io.Writer) (err error) {
	if err := cfg.EnsureDirs(); err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
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
	clients := netclient.New(netclient.Options{Logger: log})
	defer func() { _ = clients.Close() }() // only releases idle connections
	engines, err := engine.NewRegistry(httpx.New(engine.Env{Logger: log, Clock: time.Now, Clients: clients}))
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
	locks := service.NewTaskLocks()
	runner := service.NewRunner(service.RunnerDeps{
		Store:     store,
		Engines:   engines,
		Limits:    limits,
		Bus:       bus,
		Defaults:  defaults,
		Logger:    log,
		TaskLocks: locks,
	})
	sched := scheduler.New(scheduler.Options{
		Store:    store,
		Runner:   runner,
		Defaults: defaults,
		Logger:   log,
	})
	services := NewServices(service.Deps{
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
	})
	if err := services.Settings.SyncLimits(ctx); err != nil {
		bus.Close()
		return fmt.Errorf("daemon: %w", err)
	}
	_ = services // the API registers its task, queue and settings operations on them in P3.4
	srv, err := BuildServer(cfg, api.Deps{Logger: log, Version: version, Token: token, Bus: bus})
	if err != nil {
		bus.Close()
		return err
	}

	log.InfoContext(ctx, "dlm daemon starting",
		"version", version,
		"address", cfg.Server.Address,
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
	// The scheduler starts queued tasks, including the ones recovered
	// above; on shutdown it requeues its running tasks and waits for them
	// (the store stays open until g.Wait returns).
	g.Go(func() error { return sched.Run(gctx) })
	g.Go(func() error {
		// The bus outlives the server so in-flight requests can still
		// publish; closing it also stops bus.Run.
		defer bus.Close()
		defer stopSweep()
		return srv.Run(gctx)
	})
	if err := g.Wait(); err != nil {
		log.ErrorContext(ctx, "dlm daemon stopped with error", "error", err)
		return fmt.Errorf("daemon: serve: %w", err)
	}
	log.InfoContext(ctx, "dlm daemon stopped")
	return nil
}

// Services are the application use-cases the adapters (REST API, MCP) call.
type Services struct {
	Settings *service.SettingsService
	Queues   *service.QueueService
	Tasks    *service.TaskService
}

// NewServices builds every service on the same dependencies.
func NewServices(d service.Deps) *Services {
	return &Services{
		Settings: service.NewSettingsService(d),
		Queues:   service.NewQueueService(d),
		Tasks:    service.NewTaskService(d),
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
