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

	"golang.org/x/sync/errgroup"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/logging"
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

	bus := events.New(events.Options{Logger: log})
	srv, err := BuildServer(cfg, api.Deps{Logger: log, Version: version, Token: token})
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
	)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return bus.Run(gctx) })
	g.Go(func() error {
		// The bus outlives the server so in-flight requests can still
		// publish; closing it also stops bus.Run.
		defer bus.Close()
		return srv.Run(gctx)
	})
	if err := g.Wait(); err != nil {
		log.ErrorContext(ctx, "dlm daemon stopped with error", "error", err)
		return fmt.Errorf("daemon: serve: %w", err)
	}
	log.InfoContext(ctx, "dlm daemon stopped")
	return nil
}

// buildServer constructs the API server and registers every operation. It
// is shared by Run and the CLI parity test, so any operation
// registered here is automatically checked for a matching CLI command.
func BuildServer(cfg *config.Config, deps api.Deps) (*api.Server, error) {
	srv, err := api.New(cfg, deps)
	if err != nil {
		return nil, fmt.Errorf("daemon: build api server: %w", err)
	}
	return srv, nil
}
