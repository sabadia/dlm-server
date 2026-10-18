package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func credentialTable(cs []*client.Credential) output.Table {
	t := output.Table{Columns: []string{"id", "host", "scheme", "username"}}
	for _, c := range cs {
		t.Rows = append(t.Rows, []string{c.ID, c.Host, c.Scheme, c.Username})
	}
	return t
}

func (a *app) printCredential(cmd *cobra.Command, c *client.Credential) error {
	return a.printer(cmd).Print(c, func() output.Table { return credentialTable([]*client.Credential{c}) })
}

// readPassword takes the password from --password, or with --password-stdin
// from the first line of stdin, so it need not appear in the process list.
func readPassword(cmd *cobra.Command, flagValue string, fromStdin bool) (string, error) {
	switch {
	case fromStdin && flagValue != "":
		return "", &usageError{err: errors.New("give --password or --password-stdin, not both")}
	case fromStdin:
		line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read password: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	default:
		return flagValue, nil
	}
}

func (a *app) newCredentialCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credential",
		Short: "Manage stored site logins (HTTP Basic and Digest)",
		Long: "A stored login answers the Basic or Digest authentication challenge of downloads from its host. " +
			"Passwords are stored encrypted and never shown. Basic credentials are sent in clear text over http:// links.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.newCredentialListCommand(), a.newCredentialAddCommand(), a.newCredentialGetCommand(),
		a.newCredentialSetCommand(), a.newCredentialRemoveCommand())
	return cmd
}

func (a *app) newCredentialListCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List stored logins",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "list-credentials"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			cs, err := c.ListCredentials(cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if cs == nil {
				cs = []*client.Credential{}
			}
			return a.printer(cmd).Print(cs, func() output.Table { return credentialTable(cs) })
		},
	}
}

func (a *app) newCredentialAddCommand() *cobra.Command {
	var user, password, scheme string
	var stdin bool
	cmd := &cobra.Command{
		Use:         "add HOST",
		Short:       "Store a login for a host",
		Example:     "  dlm credential add files.example.com --user ann --password-stdin\n  dlm credential add '*.example.org' --user bob --password s3cret --scheme digest",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "create-credential"},
		RunE: func(cmd *cobra.Command, args []string) error {
			pw, err := readPassword(cmd, password, stdin)
			if err != nil {
				return err
			}
			if pw == "" {
				return &usageError{err: errors.New("give --password or --password-stdin")}
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			cred, err := c.CreateCredential(cmd.Context(), client.CreateCredentialRequest{Host: args[0], Scheme: scheme, Username: user, Password: pw})
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printCredential(cmd, cred)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&user, "user", "", "user name (required)")
	fl.StringVar(&password, "password", "", "password (visible in the process list: prefer --password-stdin)")
	fl.BoolVar(&stdin, "password-stdin", false, "read the password from the first line of stdin")
	fl.StringVar(&scheme, "scheme", "", "auto (default), basic or digest")
	_ = cmd.MarkFlagRequired("user") // a flag that was just defined
	return cmd
}

func (a *app) newCredentialGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "get ID",
		Short:       "Show a stored login (without its password)",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "get-credential"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			cred, err := c.GetCredential(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printCredential(cmd, cred)
		},
	}
}

func (a *app) newCredentialSetCommand() *cobra.Command {
	var host, user, password, scheme string
	var stdin bool
	cmd := &cobra.Command{
		Use:         "set ID",
		Short:       "Change a stored login",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "update-credential"},
		RunE: func(cmd *cobra.Command, args []string) error {
			fl := cmd.Flags()
			var req client.UpdateCredentialRequest
			if fl.Changed("host") {
				req.Host = &host
			}
			if fl.Changed("user") {
				req.Username = &user
			}
			if fl.Changed("scheme") {
				req.Scheme = &scheme
			}
			if fl.Changed("password") || stdin {
				pw, err := readPassword(cmd, password, stdin)
				if err != nil {
					return err
				}
				req.Password = &pw
			}
			if req == (client.UpdateCredentialRequest{}) {
				return &usageError{err: errors.New("nothing to change: give --host, --user, --scheme, --password or --password-stdin")}
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			cred, err := c.UpdateCredential(cmd.Context(), args[0], req)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printCredential(cmd, cred)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&host, "host", "", "new host")
	fl.StringVar(&user, "user", "", "new user name")
	fl.StringVar(&password, "password", "", "new password (prefer --password-stdin)")
	fl.BoolVar(&stdin, "password-stdin", false, "read the new password from the first line of stdin")
	fl.StringVar(&scheme, "scheme", "", "new scheme: auto, basic or digest")
	return cmd
}

func (a *app) newCredentialRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "rm ID",
		Aliases:     []string{"delete"},
		Short:       "Delete a stored login",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "delete-credential"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			if err := c.DeleteCredential(cmd.Context(), args[0]); err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if a.format == output.FormatTable {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "deleted "+args[0])
				return err //nolint:wrapcheck // stdout write errors need no context
			}
			return a.printer(cmd).Print(map[string]any{"id": args[0], "deleted": true}, nil)
		},
	}
}
