package cli

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func pluginTable(ps []*client.Plugin) output.Table {
	t := output.Table{Columns: []string{"name", "version", "enabled", "hooks", "net", "status"}}
	for _, p := range ps {
		status := "ok"
		switch {
		case p.Error != "":
			status = "error: " + p.Error
		case !p.Enabled:
			status = "off"
		}
		t.Rows = append(t.Rows, []string{p.Name, p.Version, strconv.FormatBool(p.Enabled), strings.Join(p.Hooks, ","), strings.Join(p.Net, ","), status})
	}
	return t
}

func (a *app) printPlugin(cmd *cobra.Command, p *client.Plugin) error {
	return a.printer(cmd).Print(p, func() output.Table { return pluginTable([]*client.Plugin{p}) })
}

func (a *app) newPluginCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage JavaScript plugins",
		Long: "Plugins are JavaScript extensions that resolve links the built-in engines cannot, refresh expired links, " +
			"and react to downloads starting, failing and completing. A plugin runs inside the daemon and may contact the " +
			"hosts its manifest lists (shown by dlm plugin get): install only plugins you trust.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.newPluginLogsCommand(), a.newPluginListCommand(), a.newPluginInstallCommand(), a.newPluginGetCommand(),
		a.newPluginEnableCommand(true), a.newPluginEnableCommand(false), a.newPluginRemoveCommand())
	return cmd
}

func (a *app) newPluginListCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List installed plugins",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "list-plugins"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			ps, err := c.ListPlugins(cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if ps == nil {
				ps = []*client.Plugin{}
			}
			return a.printer(cmd).Print(ps, func() output.Table { return pluginTable(ps) })
		},
	}
}

func (a *app) newPluginInstallCommand() *cobra.Command {
	var req client.InstallPluginRequest
	cmd := &cobra.Command{
		Use:   "install SOURCE",
		Short: "Install a plugin from a directory or a git repository",
		Long: "SOURCE is a directory holding manifest.json and the plugin's script, or an https:// git URL (the daemon needs " +
			"git on its PATH). The manifest and script are checked and the script is run once before the plugin is kept. " +
			"--replace upgrades an installed plugin of the same name.",
		Example:     "  dlm plugin install ./my-plugin\n  dlm plugin install https://github.com/example/dlm-plugin.git --ref v1.2.0",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "install-plugin"},
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Source = args[0]
			if !strings.HasPrefix(strings.ToLower(req.Source), "https://") {
				// The daemon resolves the path, not the CLI's working directory.
				abs, err := filepath.Abs(req.Source)
				if err != nil {
					return fmt.Errorf("resolve %s: %w", req.Source, err)
				}
				req.Source = abs
			}
			c, err := a.newClient(cmd, client.InstallTimeout)
			if err != nil {
				return err
			}
			p, err := c.InstallPlugin(cmd.Context(), req)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printPlugin(cmd, p)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&req.Ref, "ref", "", "branch or tag of a git source")
	fl.BoolVar(&req.Replace, "replace", false, "upgrade an installed plugin of the same name")
	fl.BoolVar(&req.Disabled, "disabled", false, "install the plugin switched off")
	return cmd
}

func (a *app) newPluginGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "get NAME",
		Short:       "Show a plugin",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "get-plugin"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			p, err := c.GetPlugin(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printPlugin(cmd, p)
		},
	}
}

func (a *app) newPluginEnableCommand(enable bool) *cobra.Command {
	use, short := "disable NAME", "Switch a plugin off"
	if enable {
		use, short = "enable NAME", "Switch a plugin on"
	}
	return &cobra.Command{
		Use:         use,
		Short:       short,
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "update-plugin"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			p, err := c.SetPluginEnabled(cmd.Context(), args[0], enable)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printPlugin(cmd, p)
		},
	}
}

func (a *app) newPluginRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "rm NAME",
		Aliases:     []string{"uninstall", "delete"},
		Short:       "Uninstall a plugin and delete its files and stored data",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "delete-plugin"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			if err := c.DeletePlugin(cmd.Context(), args[0]); err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if a.format == output.FormatTable {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "deleted "+args[0])
				return err //nolint:wrapcheck // stdout write errors need no context
			}
			return a.printer(cmd).Print(map[string]any{"name": args[0], "deleted": true}, nil)
		},
	}
}
