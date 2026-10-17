package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func (a *app) newTaskRefreshCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh ID",
		Short: "Get a new link for a task whose link expired",
		Long: "Run the task's refresh strategies (setting refresh.strategies) now. A refreshed task is queued again and resumes " +
			"from where it stopped; when only waiting strategies remain the task waits in awaiting_link. Running tasks must be paused first.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "refresh-task"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			t, err := c.RefreshTask(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printTask(cmd, t)
		},
	}
}

func (a *app) newTaskAwaitLinkCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "await-link ID",
		Short: "Wait for the browser extension to capture a new link",
		Long: "Put a failed or awaiting_link task into the awaiting_link state: the next download the browser extension " +
			"captures that matches the task's size or file name becomes its new link. The wait ends after refresh.awaitTimeout.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "await-task-link"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			t, err := c.AwaitTaskLink(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printTask(cmd, t)
		},
	}
}

func (a *app) newTaskSetURLCommand() *cobra.Command {
	var (
		pageURL    string
		referrer   string
		userAgent  string
		headers    []string
		onMismatch string
	)
	cmd := &cobra.Command{
		Use:   "set-url ID URL",
		Short: "Replace a task's link",
		Long: "Replace the link of a task, for example when it expired. The new link is probed and checked against what was " +
			"downloaded so far (size, strong ETag, file name): on a match the download resumes where it stopped; on a mismatch " +
			"--on-mismatch (default: the task's refresh.onMismatch) decides: restart discards the partial data, fail rejects the link. " +
			"Running tasks must be paused first.",
		Example:     "  dlm task set-url 01J… 'https://cdn.example.com/f.zip?token=fresh' --referrer https://example.com/",
		Args:        cobra.ExactArgs(2),
		Annotations: map[string]string{annotationOperationID: "update-task-source"},
		RunE: func(cmd *cobra.Command, args []string) error {
			req := client.UpdateTaskSourceRequest{CurrentURL: args[1], OnMismatch: onMismatch}
			fl := cmd.Flags()
			if fl.Changed("page-url") {
				req.PageURL = pageURL
			}
			if fl.Changed("referrer") {
				req.Referrer = referrer
			}
			if fl.Changed("user-agent") {
				req.UserAgent = userAgent
			}
			if len(headers) > 0 {
				h, err := parseKeyValues("header", headers)
				if err != nil {
					return err
				}
				req.Headers = h
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			t, err := c.UpdateTaskSource(cmd.Context(), args[0], req)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printTask(cmd, t)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&pageURL, "page-url", "", "web page the link was found on")
	fl.StringVar(&referrer, "referrer", "", "Referer to send")
	fl.StringVar(&userAgent, "user-agent", "", "user agent to send")
	fl.StringArrayVar(&headers, "header", nil, "request header Name=Value (repeatable)")
	fl.StringVar(&onMismatch, "on-mismatch", "", "when the link serves different content: restart or fail (default: the task's refresh.onMismatch)")
	_ = cmd.RegisterFlagCompletionFunc("on-mismatch", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"restart", "fail"}, cobra.ShellCompDirectiveNoFileComp
	}) // only fails if the flag does not exist
	return cmd
}

func (a *app) newTaskRefreshHistoryCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "refresh-history ID",
		Short:       "Show a task's link refresh history",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "list-task-refreshes"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			h, err := c.TaskRefreshes(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if h == nil {
				h = []client.RefreshAttempt{}
			}
			return a.printer(cmd).Print(h, func() output.Table { return refreshTable(h) }) //nolint:wrapcheck // stdout write errors need no context
		},
	}
}

func refreshTable(h []client.RefreshAttempt) output.Table {
	t := output.Table{Columns: []string{"time", "cause", "strategy", "outcome", "detail"}}
	for _, r := range h {
		detail := r.Error
		switch {
		case r.NewURL != "" && r.Outcome != "waiting":
			detail = strings.TrimSpace(detail + " → " + r.NewURL)
		case r.Deadline != nil:
			detail = strings.TrimSpace(detail + fmt.Sprintf(" (until %s)", r.Deadline.Local().Format(time.DateTime)))
		}
		t.Rows = append(t.Rows, []string{r.CreatedAt.Local().Format(time.DateTime), r.Cause, r.Strategy, r.Outcome, detail})
	}
	return t
}
