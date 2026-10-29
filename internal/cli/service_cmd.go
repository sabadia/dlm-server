package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/platform"
)

// autostart is the login item of the running system; tests replace it.
var autostart = &platform.Autostart{}

func (a *app) newServiceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Start the daemon when you log in",
		Long: "Install or remove a login item for the current user that runs `dlm serve`: a launchd agent on macOS, a systemd user unit on " +
			"Linux and a Run registry value on Windows. It does not need administrator rights.",
	}
	printStatus := func(cmd *cobra.Command, st platform.AutostartStatus) error {
		return a.printer(cmd).Print(st, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
			return output.Table{
				Columns: []string{"kind", "installed", "active", "path"},
				Rows:    [][]string{{st.Kind, fmt.Sprint(st.Installed), fmt.Sprint(st.Active), st.Path}},
			}
		})
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "install",
			Short: "Start the daemon at login, and now",
			Long: "Install the login item and start the daemon through it now. It runs this `dlm` binary with `serve`, and with --home when " +
				"this command was given one, so a portable setup stays portable.",
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := a.loadConfig(cmd)
				if err != nil {
					return err
				}
				exe, err := os.Executable()
				if err != nil {
					return fmt.Errorf("cli: locate the dlm executable: %w", err)
				}
				if resolved, err := filepath.EvalSymlinks(exe); err == nil {
					exe = resolved
				}
				spec := platform.AutostartSpec{Executable: exe, Args: []string{"serve"}}
				if cfg.Home != "" {
					spec.Args = append(spec.Args, "--home", cfg.Home)
				}
				st, err := autostart.Install(cmd.Context(), spec)
				if perr := printStatus(cmd, st); perr != nil {
					return perr
				}
				if err != nil {
					return fmt.Errorf("cli: %w", err)
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "uninstall",
			Short: "Stop starting the daemon at login",
			Long:  "Remove the login item and stop the daemon it started. A daemon you started yourself is not touched.",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if err := autostart.Uninstall(cmd.Context()); err != nil {
					return fmt.Errorf("cli: %w", err)
				}
				st, err := autostart.Status(cmd.Context())
				if err != nil {
					return fmt.Errorf("cli: %w", err)
				}
				return printStatus(cmd, st)
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Show whether the daemon starts at login",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				st, err := autostart.Status(cmd.Context())
				if err != nil {
					return fmt.Errorf("cli: %w", err)
				}
				return printStatus(cmd, st)
			},
		},
	)
	return cmd
}
