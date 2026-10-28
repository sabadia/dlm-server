package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

// versionProbeTimeout bounds the server-version lookup of `dlm version`.
const versionProbeTimeout = 3 * time.Second

// errTokenRejected is returned by `dlm health` when the daemon is up but did
// not accept the presented token.
var errTokenRejected = errors.New("the daemon rejected the API token (check --token or the token file, see `dlm token show`)")

func (a *app) newHealthCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Check that the daemon is running",
		Long: "Report the daemon's status and version and whether the API token is accepted.\n\n" +
			"Exits with an error if the daemon is unreachable or rejects the token.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "get-health"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			h, err := c.Health(cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if err := a.printer(cmd).Print(h, func() output.Table {
				return output.Table{
					Columns: []string{"status", "version", "api", "authenticated"},
					Rows:    [][]string{{h.Status, h.Version, strconv.Itoa(h.APIVersion), strconv.FormatBool(h.Authenticated)}},
				}
			}); err != nil {
				return err
			}
			if c.HasToken() && !h.Authenticated {
				return errTokenRejected
			}
			return nil
		},
	}
}

// versionInfo is the output of `dlm version`.
type versionInfo struct {
	Client string `json:"client"`
	Server string `json:"server,omitempty"`
}

func (a *app) newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI and daemon versions",
		Long:  "Print the CLI version and, if the daemon is reachable, its version. An unreachable daemon is not an error.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := versionInfo{Client: a.version}
			if v, err := a.serverVersion(cmd); err != nil {
				writeErr(cmd.ErrOrStderr(), "note: daemon version unavailable: "+err.Error())
			} else {
				info.Server = v
			}
			return a.printer(cmd).Print(info, func() output.Table {
				t := output.Table{Columns: []string{"component", "version"}, Rows: [][]string{{"client", info.Client}}}
				if info.Server != "" {
					t.Rows = append(t.Rows, []string{"server", info.Server})
				}
				return t
			})
		},
	}
}

func (a *app) serverVersion(cmd *cobra.Command) (string, error) {
	c, err := a.newClient(cmd, versionProbeTimeout)
	if err != nil {
		return "", err
	}
	h, err := c.Health(cmd.Context())
	if err != nil {
		return "", err //nolint:wrapcheck // client errors are already user-facing
	}
	return h.Version, nil
}

// tokenInfo is the output of `dlm token show`.
type tokenInfo struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Token  string `json:"token,omitempty"`
}

func (a *app) newTokenCommand() *cobra.Command {
	tokenCmd := &cobra.Command{
		Use:   "token",
		Short: "Manage the API token",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	var reveal bool
	show := &cobra.Command{
		Use:   "show",
		Short: "Show the API token file (and, with --reveal, the token)",
		Long: "Show where the daemon's API token is stored. Use --reveal to print the token itself, " +
			"e.g. to configure the browser extension. The token is created by `dlm serve`, never by this command.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			info := tokenInfo{Path: cfg.TokenFile()}
			token, err := auth.ReadToken(info.Path)
			switch {
			case err == nil:
				info.Exists = true
				if reveal {
					info.Token = token
				}
			case errors.Is(err, fs.ErrNotExist):
				if reveal {
					return fmt.Errorf("cli: no token at %s; start the daemon once with `dlm serve` to create it", info.Path)
				}
			default:
				return fmt.Errorf("cli: %w", err)
			}
			return a.printer(cmd).Print(info, func() output.Table {
				t := output.Table{Columns: []string{"path", "exists"}, Rows: [][]string{{info.Path, strconv.FormatBool(info.Exists)}}}
				if reveal {
					t.Columns = append(t.Columns, "token")
					t.Rows[0] = append(t.Rows[0], info.Token)
				}
				return t
			})
		},
	}
	show.Flags().BoolVar(&reveal, "reveal", false, "also print the token (keep it secret)")
	tokenCmd.AddCommand(show)
	return tokenCmd
}

// newSystemCommand builds `dlm system`: information about, and control of,
// the running daemon.
func (a *app) newSystemCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Inspect and control the running daemon",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.newSystemInfoCommand(), a.newSystemLifecycleCommand("stop", "shutdown-daemon", (*client.Client).Shutdown), a.newSystemLifecycleCommand("restart", "restart-daemon", (*client.Client).Restart))
	return cmd
}

func (a *app) newSystemInfoCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Describe the daemon: version, directories, engines, tools",
		Long: "Report the daemon's version, API version, build, process, listen address, directories, enabled engines, " +
			"the status of the external tools (ffmpeg, yt-dlp, browser) and the database size.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "get-system-info"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			info, err := c.SystemInfo(cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(info, func() output.Table { return systemInfoTable(info) })
		},
	}
}

func systemInfoTable(i *client.SystemInfo) output.Table {
	rows := [][]string{
		{"version", i.Version},
		{"api", strconv.Itoa(i.APIVersion)},
		{"commit", i.Commit},
		{"platform", i.OS + "/" + i.Arch + " (" + i.GoVersion + ")"},
		{"pid", strconv.Itoa(i.PID)},
		{"started", i.StartedAt},
		{"uptime", (time.Duration(i.UptimeSeconds) * time.Second).String()},
		{"address", i.Address},
		{"tls", strconv.FormatBool(i.TLS)},
		{"local", strconv.FormatBool(i.Local)},
		{"config dir", i.Paths.Config},
		{"data dir", i.Paths.Data},
		{"cache dir", i.Paths.Cache},
		{"log dir", i.Paths.Log},
		{"database", i.Paths.Database + " (" + strconv.FormatInt(i.DatabaseBytes, 10) + " bytes)"},
		{"downloads", i.Paths.Downloads},
		{"engines", strings.Join(i.Engines, ", ")},
	}
	for _, t := range i.Tools {
		state := "missing"
		if t.Available {
			state = strings.TrimSpace(t.Version + " " + t.Source + " " + t.Path)
		} else if t.Error != "" {
			state = "missing: " + t.Error
		}
		rows = append(rows, []string{t.Name, state})
	}
	return output.Table{Columns: []string{"key", "value"}, Rows: rows}
}

// newSystemLifecycleCommand builds `system stop` or `system restart`.
func (a *app) newSystemLifecycleCommand(use, op string, call func(*client.Client, context.Context) (string, error)) *cobra.Command {
	short, long := "Stop the daemon gracefully", "Stop the daemon gracefully: running downloads are stopped and requeued, then it exits."
	if use == "restart" {
		short = "Restart the daemon"
		long = "Stop the daemon gracefully and start it again with the same arguments."
	}
	return &cobra.Command{
		Use:         use,
		Short:       short,
		Long:        long,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: op},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			status, err := call(c, cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			res := struct {
				Status string `json:"status"`
			}{Status: status}
			return a.printer(cmd).Print(res, func() output.Table {
				return output.Table{Columns: []string{"status"}, Rows: [][]string{{status}}}
			})
		},
	}
}
