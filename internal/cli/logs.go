package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

// logLine formats a record for the terminal: time, level, message and the
// attributes as key=value.
func logLine(r client.LogRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s %s", r.Time.Local().Format("15:04:05.000"), strings.ToUpper(r.Level), r.Message)
	keys := make([]string, 0, len(r.Attrs))
	for k := range r.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, r.Attrs[k])
	}
	return b.String()
}

// sinceArg turns a duration such as 15m into an RFC 3339 time that long ago;
// anything else is passed on as it is (an RFC 3339 time).
func sinceArg(v string, now time.Time) string {
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(-d).UTC().Format(time.RFC3339)
	}
	return v
}

func (a *app) newLogsCommand() *cobra.Command {
	var (
		o      client.LogsOptions
		follow bool
		since  string
	)
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show the daemon's log",
		Long: "Show the most recent records of the daemon's log (it keeps the last 5000 since it started; older ones are in its log " +
			"directory). -f keeps printing new records. Table output prints one line per record; json prints one JSON object per line.",
		Example:     "  dlm logs -n 50 --level warn\n  dlm logs -f --q scheduler\n  dlm logs --since 15m",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "get-logs,stream-logs"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			if since != "" {
				o.Since = sinceArg(since, time.Now())
			}
			list, err := c.GetLogs(cmd.Context(), o)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			out := cmd.OutOrStdout()
			emit := func(r client.LogRecord) error {
				if a.format == output.FormatTable {
					_, err := fmt.Fprintln(out, logLine(r))
					return err //nolint:wrapcheck // stdout write errors need no context
				}
				return a.printer(cmd).Print(r, func() output.Table { return output.Table{} }) //nolint:wrapcheck // stdout write errors need no context
			}
			for _, r := range list.Records {
				if err := emit(r); err != nil {
					return err
				}
			}
			if !follow {
				if list.HasMore {
					writeErr(cmd.ErrOrStderr(), "note: older records exist; raise -n or use --since")
				}
				return nil
			}
			streamOpts := client.LogsOptions{Level: o.Level, Query: o.Query}
			last := fmt.Sprintf("%s-%d", list.BootID, list.LastID)
			err = c.StreamLogs(cmd.Context(), streamOpts, last, emit, func(reason string) {
				writeErr(cmd.ErrOrStderr(), "note: some records may have been missed ("+reason+")")
			})
			if err != nil && cmd.Context().Err() == nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.IntVarP(&o.Limit, "lines", "n", 100, "how many of the newest records to show, 1-1000")
	fl.StringVar(&o.Level, "level", "", "only this level and above: debug, info, warn or error")
	fl.StringVar(&o.Query, "q", "", "only records containing this text")
	fl.StringVar(&since, "since", "", "only records since this long ago (15m, 2h) or this time (RFC 3339)")
	fl.StringVar(&o.Until, "until", "", "only records until this time (RFC 3339)")
	fl.BoolVarP(&follow, "follow", "f", false, "keep printing new records")
	return cmd
}
