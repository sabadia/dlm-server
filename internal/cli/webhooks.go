package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func webhookTable(ws []*client.Webhook) output.Table {
	t := output.Table{Columns: []string{"id", "url", "events", "enabled", "signed"}}
	for _, w := range ws {
		t.Rows = append(t.Rows, []string{w.ID, w.URL, strings.Join(w.Events, ","), strconv.FormatBool(w.Enabled), strconv.FormatBool(w.HasSecret)})
	}
	return t
}

func (a *app) printWebhook(cmd *cobra.Command, w *client.Webhook) error {
	return a.printer(cmd).Print(w, func() output.Table { return webhookTable([]*client.Webhook{w}) })
}

func (a *app) newWebhookCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "Manage the webhooks that receive events",
		Long: "A webhook receives a JSON POST for each event it subscribes to (task.status, queue.completed, ...). " +
			"With a secret every body is signed: the X-DLM-Signature header is sha256= and the hex HMAC-SHA256 of the body.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.newWebhookDeliveriesCommand(), a.newWebhookListCommand(), a.newWebhookAddCommand(), a.newWebhookGetCommand(),
		a.newWebhookSetCommand(), a.newWebhookRemoveCommand(), a.newWebhookTestCommand())
	return cmd
}

func (a *app) newWebhookListCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List webhooks",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "list-webhooks"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			ws, err := c.ListWebhooks(cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if ws == nil {
				ws = []*client.Webhook{}
			}
			return a.printer(cmd).Print(ws, func() output.Table { return webhookTable(ws) })
		},
	}
}

func (a *app) newWebhookAddCommand() *cobra.Command {
	var (
		evs      []string
		secret   string
		disabled bool
	)
	cmd := &cobra.Command{
		Use:         "add URL",
		Short:       "Create a webhook",
		Example:     "  dlm webhook add https://example.com/hook --event task.status --event queue.completed --secret s3cret",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "create-webhook"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			w, err := c.CreateWebhook(cmd.Context(), client.CreateWebhookRequest{URL: args[0], Events: evs, Secret: secret, Disabled: disabled})
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printWebhook(cmd, w)
		},
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&evs, "event", nil, "event type to deliver (repeatable, required)")
	fl.StringVar(&secret, "secret", "", "secret that signs the deliveries")
	fl.BoolVar(&disabled, "disabled", false, "create the webhook switched off")
	_ = cmd.MarkFlagRequired("event") // a flag that was just defined
	return cmd
}

func (a *app) newWebhookGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "get ID",
		Short:       "Show a webhook",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "get-webhook"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			w, err := c.GetWebhook(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printWebhook(cmd, w)
		},
	}
}

func (a *app) newWebhookSetCommand() *cobra.Command {
	var (
		url, secret string
		evs         []string
		enabled     bool
	)
	cmd := &cobra.Command{
		Use:   "set ID",
		Short: "Change a webhook",
		Long: "Change the given fields of a webhook. --event replaces the whole list; --secret '' removes the secret; " +
			"--enabled=false switches the webhook off.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "update-webhook"},
		RunE: func(cmd *cobra.Command, args []string) error {
			fl := cmd.Flags()
			var req client.UpdateWebhookRequest
			if fl.Changed("url") {
				req.URL = &url
			}
			if fl.Changed("event") {
				req.Events = &evs
			}
			if fl.Changed("secret") {
				req.Secret = &secret
			}
			if fl.Changed("enabled") {
				req.Enabled = &enabled
			}
			if req == (client.UpdateWebhookRequest{}) {
				return &usageError{err: fmt.Errorf("nothing to change: give --url, --event, --secret or --enabled")}
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			w, err := c.UpdateWebhook(cmd.Context(), args[0], req)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printWebhook(cmd, w)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&url, "url", "", "new endpoint")
	fl.StringArrayVar(&evs, "event", nil, "new event list (repeatable; replaces the old list)")
	fl.StringVar(&secret, "secret", "", "new signing secret (empty removes it)")
	fl.BoolVar(&enabled, "enabled", true, "switch the webhook on or off")
	return cmd
}

func (a *app) newWebhookRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "rm ID",
		Aliases:     []string{"delete"},
		Short:       "Delete a webhook",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "delete-webhook"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			if err := c.DeleteWebhook(cmd.Context(), args[0]); err != nil {
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

func (a *app) newWebhookTestCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "test ID",
		Short:       "Send a test event to a webhook",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "test-webhook"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			if err := c.TestWebhook(cmd.Context(), args[0]); err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if a.format == output.FormatTable {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "delivered")
				return err //nolint:wrapcheck // stdout write errors need no context
			}
			return a.printer(cmd).Print(map[string]any{"id": args[0], "delivered": true}, nil)
		},
	}
}
