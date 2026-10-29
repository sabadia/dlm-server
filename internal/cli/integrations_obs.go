package cli

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func (a *app) newWebhookDeliveriesCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "deliveries ID",
		Short: "Show a webhook's recent deliveries",
		Long: "Show the last attempts to send an event to the webhook since the daemon started, newest first: the event, the attempt, " +
			"the HTTP status the endpoint answered, how long it took and what went wrong.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "list-webhook-deliveries"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			list, err := c.WebhookDeliveries(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if list == nil {
				list = []client.WebhookDelivery{}
			}
			return a.printer(cmd).Print(list, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
				t := output.Table{Columns: []string{"time", "event", "attempt", "status", "took", "result"}}
				for _, d := range list {
					result := "ok"
					switch {
					case !d.OK && d.WillRetry:
						result = "failed, will retry: " + d.Error
					case !d.OK:
						result = "failed: " + d.Error
					}
					status := "-"
					if d.Status > 0 {
						status = strconv.Itoa(d.Status)
					}
					t.Rows = append(t.Rows, []string{
						d.Time.Local().Format(time.TimeOnly), d.Event, strconv.Itoa(d.Attempt), status,
						fmt.Sprintf("%dms", d.DurationMs), result,
					})
				}
				return t
			})
		},
	}
}

func (a *app) newPluginLogsCommand() *cobra.Command {
	var o client.LogsOptions
	cmd := &cobra.Command{
		Use:   "logs NAME",
		Short: "Show a plugin's log",
		Long: "Show the latest log records of a plugin (500 are kept): what its script wrote with dlm.log and what the daemon said " +
			"about it, such as failed starts and hooks.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "get-plugin-logs"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			list, err := c.PluginLogs(cmd.Context(), args[0], o)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if list.Records == nil {
				list.Records = []client.LogRecord{}
			}
			if a.format != output.FormatTable {
				return a.printer(cmd).Print(list, func() output.Table { return output.Table{} }) //nolint:wrapcheck // stdout write errors need no context
			}
			for _, r := range list.Records {
				fmt.Fprintln(cmd.OutOrStdout(), logLine(r))
			}
			if list.HasMore {
				writeErr(cmd.ErrOrStderr(), "note: older records exist; raise -n")
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.IntVarP(&o.Limit, "lines", "n", 100, "how many of the newest records to show, 1-500")
	fl.StringVar(&o.Level, "level", "", "only this level and above: debug, info, warn or error")
	fl.StringVar(&o.Query, "q", "", "only records containing this text")
	return cmd
}
