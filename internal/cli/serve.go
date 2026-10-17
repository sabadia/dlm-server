package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/sabadia/dlm-server/internal/api"
	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/logging"
)

// This file is the daemon's composition root. Client commands must not use
// the server-side packages imported here; they go through internal/client.

func (a *app) newServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the dlm daemon",
		Long: "Run the dlm daemon in the foreground until interrupted.\n\n" +
			"On first start a random API token is written to the token file in the config directory " +
			"(see `dlm token show`).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			return serve(cmd.Context(), cfg, a.version, cmd.ErrOrStderr())
		},
	}
}

// serve runs the daemon until ctx is cancelled and returns nil on a clean
// shutdown.
func serve(ctx context.Context, cfg *config.Config, version string, stderr io.Writer) (err error) {
	if err := cfg.EnsureDirs(); err != nil {
		return fmt.Errorf("cli: %w", err)
	}
	logger, err := logging.New(cfg.Log, cfg.Paths.LogDir, stderr)
	if err != nil {
		return fmt.Errorf("cli: %w", err)
	}
	defer func() {
		if cerr := logger.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	log := logger.Logger

	token, err := auth.LoadOrCreateToken(cfg.TokenFile())
	if err != nil {
		return fmt.Errorf("cli: %w", err)
	}

	bus := events.New(events.Options{Logger: log})
	srv, err := buildServer(cfg, api.Deps{Logger: log, Version: version, Token: token})
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
		return fmt.Errorf("cli: serve: %w", err)
	}
	log.InfoContext(ctx, "dlm daemon stopped")
	return nil
}

// buildServer constructs the API server and registers every operation. It
// is shared by `dlm serve` and the CLI parity test, so any operation
// registered here is automatically checked for a matching CLI command.
func buildServer(cfg *config.Config, deps api.Deps) (*api.Server, error) {
	srv, err := api.New(cfg, deps)
	if err != nil {
		return nil, fmt.Errorf("cli: build api server: %w", err)
	}
	return srv, nil
}
