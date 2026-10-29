package cli

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func speedText(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	return formatSize(n) + "/s"
}

func limitsTable(l *client.Limits) output.Table {
	t := output.Table{Columns: []string{"scope", "speed limit", "configured", "profile", "quota"}}
	row := func(name string, s client.ScopeLimits) []string {
		profile, quota := "", ""
		if s.ActiveProfile != nil {
			profile = "#" + strconv.Itoa(s.ActiveProfile.Index) + " " + speedText(s.ActiveProfile.SpeedLimit)
		}
		if q := s.Quota; q != nil {
			quota = fmt.Sprintf("%s of %s per %s", formatSize(q.Used), formatSize(q.Bytes), q.Period)
			if q.Exhausted {
				quota += " (used up)"
			}
		}
		return []string{name, speedText(s.SpeedLimit), speedText(s.ConfiguredSpeedLimit), profile, quota}
	}
	t.Rows = append(t.Rows, row("global", l.Global))
	for _, q := range l.Queues {
		t.Rows = append(t.Rows, row("queue "+q.Name, q))
	}
	if l.Override != nil {
		t.Rows = append(t.Rows, []string{"override", speedText(l.Override.SpeedLimit), "", "until " + l.Override.Until.Local().Format(time.TimeOnly), ""})
	}
	return t
}

func (a *app) newLimitsCommand() *cobra.Command {
	var (
		minutes int
		clearIt bool
	)
	cmd := &cobra.Command{
		Use:   "limits",
		Short: "Show the speed limits and quotas in force",
		Long: "Show the global and per-queue speed limits now in force, the active speed profile, the quota usage and a temporary override. " +
			"'dlm limits override' changes the global limit for a while.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "get-limits"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			l, err := c.GetLimits(cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(l, func() output.Table { return limitsTable(l) }) //nolint:wrapcheck // stdout write errors need no context
		},
	}
	override := &cobra.Command{
		Use:   "override [SPEED]",
		Short: "Replace the global speed limit for a while",
		Long: "Replace the global speed limit by SPEED per second (500K, 2MB, ...; none or 0: unlimited, \"turbo\") for --minutes (1-1440, " +
			"default 60). The configured limit and profiles apply again afterwards. --clear ends an override now.",
		Example:     "  dlm limits override           # turbo for an hour\n  dlm limits override 1MB -m 30\n  dlm limits override --clear",
		Args:        cobra.MaximumNArgs(1),
		Annotations: map[string]string{annotationOperationID: "set-limits-override,clear-limits-override"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			var l *client.Limits
			if clearIt {
				if len(args) > 0 {
					return &usageError{err: errors.New("--clear takes no speed")}
				}
				l, err = c.ClearLimitsOverride(cmd.Context())
			} else {
				var speed int64
				if len(args) == 1 && args[0] != "none" {
					if speed, err = parseSize(args[0]); err != nil {
						return &usageError{err: err}
					}
				}
				l, err = c.SetLimitsOverride(cmd.Context(), speed, minutes)
			}
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(l, func() output.Table { return limitsTable(l) }) //nolint:wrapcheck // stdout write errors need no context
		},
	}
	override.Flags().IntVarP(&minutes, "minutes", "m", 60, "how long the override lasts, 1-1440")
	override.Flags().BoolVar(&clearIt, "clear", false, "end the override now")
	cmd.AddCommand(override)
	return cmd
}

func (a *app) newStatsTimelineCommand() *cobra.Command {
	var o client.TimelineOptions
	cmd := &cobra.Command{
		Use:   "timeline",
		Short: "Show the traffic per hour or day",
		Long: "Show the bytes downloaded and uploaded per hour or day (UTC), optionally split by engine or queue. " +
			"Defaults to the last 24 hours (--bucket hour) or 30 days (--bucket day).",
		Example:     "  dlm stats timeline\n  dlm stats timeline --bucket day --group-by engine",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "get-stats-timeline"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			tl, err := c.StatsTimeline(cmd.Context(), o)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(tl, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
				t := output.Table{Columns: []string{"time", "group", "down", "up"}}
				format := time.DateTime
				if tl.Bucket == "day" {
					format = time.DateOnly
				}
				for _, p := range tl.Points {
					t.Rows = append(t.Rows, []string{p.Time.Format(format), p.Group, formatSize(p.Down), formatSize(p.Up)})
				}
				return t
			})
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&o.Bucket, "bucket", "", "hour (default) or day")
	fl.StringVar(&o.GroupBy, "group-by", "", "none (default), engine or queue")
	fl.StringVar(&o.From, "from", "", "start of the range, RFC 3339 (inclusive)")
	fl.StringVar(&o.To, "to", "", "end of the range, RFC 3339 (exclusive)")
	return cmd
}
