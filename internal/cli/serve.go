package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/daemon"
	"github.com/sabadia/dlm-server/internal/platform"
)

// reexec starts the process again; tests replace it.
var reexec = platform.Reexec

func (a *app) newServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the dlm daemon",
		Long: "Run the dlm daemon in the foreground until interrupted.\n\n" +
			"On first start a random API token is written to the token file in the config directory " +
			"(see `dlm token show`).\n\n" +
			"With --address 127.0.0.1:0 the system picks a free port; the daemon publishes it in runtime.json in the config " +
			"directory, which the other commands read to find it. Only one daemon runs per config directory.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			err = daemon.Run(cmd.Context(), cfg, a.version, cmd.ErrOrStderr())
			if errors.Is(err, daemon.ErrRestart) {
				// The daemon stopped cleanly because restart-daemon asked for
				// it; start the same command line again.
				return reexec()
			}
			return err //nolint:wrapcheck // daemon errors are already descriptive
		},
	}
}
