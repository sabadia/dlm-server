package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func pairingRow(p client.Pairing) []string {
	return []string{p.ID, p.Status, p.Name, p.Browser, p.Code, p.ExpiresAt.Local().Format(time.TimeOnly)}
}

var pairingColumns = []string{"id", "status", "name", "browser", "code", "expires"}

func (a *app) newPairingCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pairing",
		Short: "Allow or refuse browser extensions that ask to connect",
		Long: "A browser extension asks to be paired and waits for you to allow it. The request shows a six-digit code that the " +
			"extension shows too; allow it only when they match. Allowing issues the extension a token of its own " +
			"(scope extension), which `dlm auth token list` shows and `dlm auth token rm` revokes.",
	}
	var req client.PairingRequest
	start := &cobra.Command{
		Use:         "start",
		Short:       "Ask to pair (what the extension's native host does)",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "start-pairing"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			p, err := c.StartPairing(cmd.Context(), req)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(p, func() output.Table { return output.Table{Columns: pairingColumns, Rows: [][]string{pairingRow(*p)}} }) //nolint:wrapcheck // stdout write errors need no context
		},
	}
	start.Flags().StringVar(&req.Name, "name", "", "what the client calls itself")
	start.Flags().StringVar(&req.Browser, "browser", "", "chrome, chromium, edge, brave, opera, vivaldi, firefox or other")
	start.Flags().StringVar(&req.ClientID, "client-id", "", "the extension's id")
	start.Flags().StringVar(&req.Version, "client-version", "", "the extension's version")

	cmd.AddCommand(
		&cobra.Command{
			Use:         "list",
			Short:       "List the pairings waiting for an answer",
			Args:        cobra.NoArgs,
			Annotations: map[string]string{annotationOperationID: "list-pairings"},
			RunE: func(cmd *cobra.Command, _ []string) error {
				c, err := a.newClient(cmd, client.DefaultTimeout)
				if err != nil {
					return err
				}
				list, err := c.ListPairings(cmd.Context())
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				if list == nil {
					list = []client.Pairing{}
				}
				return a.printer(cmd).Print(list, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
					t := output.Table{Columns: pairingColumns}
					for _, p := range list {
						t.Rows = append(t.Rows, pairingRow(p))
					}
					return t
				})
			},
		},
		start,
		&cobra.Command{
			Use:         "show ID",
			Short:       "Show a pairing (an approved one also prints its token, once)",
			Args:        cobra.ExactArgs(1),
			Annotations: map[string]string{annotationOperationID: "get-pairing"},
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := a.newClient(cmd, client.DefaultTimeout)
				if err != nil {
					return err
				}
				p, err := c.GetPairing(cmd.Context(), args[0])
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				return a.printer(cmd).Print(p, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
					row := pairingRow(p.Pairing)
					cols := pairingColumns
					if p.Token != "" {
						cols, row = append(append([]string{}, cols...), "token"), append(row, p.Token)
					}
					return output.Table{Columns: cols, Rows: [][]string{row}}
				})
			},
		},
		&cobra.Command{
			Use:         "approve ID",
			Short:       "Allow a pairing",
			Args:        cobra.ExactArgs(1),
			Annotations: map[string]string{annotationOperationID: "approve-pairing"},
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := a.newClient(cmd, client.DefaultTimeout)
				if err != nil {
					return err
				}
				p, err := c.ApprovePairing(cmd.Context(), args[0])
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				fmt.Fprintf(cmd.OutOrStdout(), "allowed %s (%s)\n", p.Name, p.Code)
				return nil
			},
		},
		&cobra.Command{
			Use:         "deny ID",
			Short:       "Refuse a pairing",
			Args:        cobra.ExactArgs(1),
			Annotations: map[string]string{annotationOperationID: "deny-pairing"},
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := a.newClient(cmd, client.DefaultTimeout)
				if err != nil {
					return err
				}
				p, err := c.DenyPairing(cmd.Context(), args[0])
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				fmt.Fprintf(cmd.OutOrStdout(), "refused %s\n", p.Name)
				return nil
			},
		},
	)
	return cmd
}
