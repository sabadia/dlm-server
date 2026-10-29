package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func (a *app) newTaskCleanupCommand() *cobra.Command {
	var (
		req client.CleanupRequest
		all bool
	)
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Remove old completed downloads",
		Long: "Remove the completed tasks that finished at least --older-than days ago from the list, or every completed task with --all. " +
			"--files also deletes their downloaded files. --dry-run only shows what would go. The retentionDays setting does this " +
			"automatically (without touching files).",
		Example:     "  dlm task cleanup --older-than 30 --dry-run\n  dlm task cleanup --all --files",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "cleanup-tasks"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if all == cmd.Flags().Changed("older-than") {
				return &usageError{err: fmt.Errorf("give --older-than DAYS, or --all")}
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			res, err := c.CleanupTasks(cmd.Context(), req)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(res, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
				verb := "removed"
				if req.DryRun {
					verb = "would remove"
				}
				return output.Table{
					Columns: []string{"matched", "size", verb, "failed"},
					Rows:    [][]string{{fmt.Sprint(res.Matched), formatSize(res.Bytes), fmt.Sprint(res.Deleted), fmt.Sprint(res.Failed)}},
				}
			})
		},
	}
	fl := cmd.Flags()
	fl.IntVar(&req.OlderThanDays, "older-than", 0, "remove tasks that completed at least this many days ago")
	fl.BoolVar(&all, "all", false, "remove every completed task")
	fl.StringVar(&req.QueueID, "queue", "", "only tasks of this queue ID")
	fl.BoolVar(&req.Files, "files", false, "also delete the downloaded files")
	fl.BoolVar(&req.DryRun, "dry-run", false, "only show what would be removed")
	return cmd
}
