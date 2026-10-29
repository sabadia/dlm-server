package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func (a *app) newAuthCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Who you are to the daemon, and API tokens",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:         "whoami",
			Short:       "Show the token this client uses: its name and scope",
			Args:        cobra.NoArgs,
			Annotations: map[string]string{annotationOperationID: "whoami"},
			RunE: func(cmd *cobra.Command, _ []string) error {
				c, err := a.newClient(cmd, client.DefaultTimeout)
				if err != nil {
					return err
				}
				w, err := c.WhoAmI(cmd.Context())
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				return a.printer(cmd).Print(w, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
					return output.Table{
						Columns: []string{"token", "scope", "primary", "local", "tls"},
						Rows:    [][]string{{w.Name, w.Scope, fmt.Sprint(w.Primary), fmt.Sprint(w.Local), fmt.Sprint(w.TLS)}},
					}
				})
			},
		},
	)
	return cmd
}

// tokenAPICommands are the subcommands of `dlm token` that use the API:
// named tokens and rotating the primary one (show is local, in system.go).
func (a *app) tokenAPICommands() []*cobra.Command {
	var scope string
	add := &cobra.Command{
		Use:   "add NAME",
		Short: "Create a named token",
		Long: "Create a named token for a dashboard, a script or another device and print its secret once. --scope read makes it read-only " +
			"(it can read everything except cookie exports and the token list, and change nothing).",
		Example:     "  dlm auth token add dashboard --scope read",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "create-token"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			t, err := c.CreateToken(cmd.Context(), args[0], scope)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if a.format == output.FormatTable {
				writeErr(cmd.ErrOrStderr(), "note: this is the only time the token is shown")
			}
			return a.printer(cmd).Print(t, func() output.Table { return output.Table{Columns: []string{"token"}, Rows: [][]string{{t.Token}}} }) //nolint:wrapcheck // stdout write errors need no context
		},
	}
	add.Flags().StringVar(&scope, "scope", "admin", "admin (everything) or read (read-only)")
	return []*cobra.Command{
		add,
		{
			Use:         "list",
			Short:       "List the named tokens",
			Args:        cobra.NoArgs,
			Annotations: map[string]string{annotationOperationID: "list-tokens"},
			RunE: func(cmd *cobra.Command, _ []string) error {
				c, err := a.newClient(cmd, client.DefaultTimeout)
				if err != nil {
					return err
				}
				list, err := c.ListTokens(cmd.Context())
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				if list == nil {
					list = []client.APIToken{}
				}
				return a.printer(cmd).Print(list, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
					t := output.Table{Columns: []string{"id", "name", "scope", "created", "last used"}}
					for _, k := range list {
						used := "never"
						if k.LastUsedAt != nil {
							used = k.LastUsedAt.Local().Format(time.DateTime)
						}
						t.Rows = append(t.Rows, []string{k.ID, k.Name, k.Scope, k.CreatedAt.Local().Format(time.DateTime), used})
					}
					return t
				})
			},
		},
		{
			Use:         "rm ID",
			Aliases:     []string{"revoke"},
			Short:       "Revoke a named token",
			Args:        cobra.ExactArgs(1),
			Annotations: map[string]string{annotationOperationID: "delete-token"},
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := a.newClient(cmd, client.DefaultTimeout)
				if err != nil {
					return err
				}
				if err := c.DeleteToken(cmd.Context(), args[0]); err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				fmt.Fprintln(cmd.OutOrStdout(), "revoked")
				return nil
			},
		},
		{
			Use:   "rotate",
			Short: "Replace the primary token",
			Long: "Generate a new primary token, write it to the daemon's token file and stop accepting the old one at once. The new token is " +
				"printed so a remote client can switch to it; clients on this machine read the file.",
			Args:        cobra.NoArgs,
			Annotations: map[string]string{annotationOperationID: "rotate-token"},
			RunE: func(cmd *cobra.Command, _ []string) error {
				c, err := a.newClient(cmd, client.DefaultTimeout)
				if err != nil {
					return err
				}
				t, err := c.RotateToken(cmd.Context())
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				if a.format == output.FormatTable {
					writeErr(cmd.ErrOrStderr(), "note: the old token no longer works; local clients read the token file")
				}
				return a.printer(cmd).Print(t, func() output.Table { return output.Table{Columns: []string{"token"}, Rows: [][]string{{t.Token}}} }) //nolint:wrapcheck // stdout write errors need no context
			},
		},
	}
}
