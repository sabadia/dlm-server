package cli

import (
	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/daemon"
)

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
			return daemon.Run(cmd.Context(), cfg, a.version, cmd.ErrOrStderr()) //nolint:wrapcheck // daemon errors are already descriptive
		},
	}
}
