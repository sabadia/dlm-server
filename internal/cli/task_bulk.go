package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/client"
)

// newTaskBulkSelectCommand builds a command that acts on several tasks chosen
// by ID or by --all/--queue/--status, like pause and resume.
func (a *app) newTaskBulkSelectCommand(use, short, long, op string, bind func(*cobra.Command),
	call func(*client.Client, *cobra.Command, client.TaskSelection) ([]client.BatchItem, error),
) *cobra.Command {
	var sel selectFlags
	cmd := &cobra.Command{
		Use:         use,
		Short:       short,
		Long:        long + " Chooses tasks by ID, or every task matching --all, --queue and/or --status. Prints one result per task; exits 1 if any failed.",
		Annotations: map[string]string{annotationOperationID: op},
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := sel.selection(args)
			if err != nil {
				return err
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			items, err := call(c, cmd, s)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printBatch(cmd, items)
		},
	}
	sel.bind(cmd, "act on")
	if bind != nil {
		bind(cmd)
	}
	return cmd
}

func (a *app) newTaskMoveTasksCommand() *cobra.Command {
	var queue string
	return a.newTaskBulkSelectCommand("move-tasks [ID...]", "Move several tasks to another queue",
		"Append each chosen task at the end of the queue given with --to.", "move-tasks",
		func(cmd *cobra.Command) {
			cmd.Flags().StringVar(&queue, "to", "", "target queue ID (required)")
			_ = cmd.MarkFlagRequired("to") // only fails if the flag does not exist
		},
		func(c *client.Client, cmd *cobra.Command, s client.TaskSelection) ([]client.BatchItem, error) {
			return c.MoveTasks(cmd.Context(), s, queue)
		})
}

func (a *app) newTaskUpdateTasksCommand() *cobra.Command {
	var (
		change       client.UpdateTasksRequest
		labels       []string
		removeLabels []string
		settings     string
	)
	return a.newTaskBulkSelectCommand("update-tasks [ID...]", "Change several tasks at once",
		"Set the priority, add or remove labels, patch the settings overrides and/or move each chosen task. Only the given fields change.",
		"update-tasks",
		func(cmd *cobra.Command) {
			fl := cmd.Flags()
			fl.StringVar(&change.Priority, "priority", "", "new priority: low, normal or high")
			fl.StringVar(&change.QueueID, "to", "", "also move the tasks to this queue ID")
			fl.StringArrayVar(&labels, "label", nil, "label to add or change, KEY=VALUE (repeatable)")
			fl.StringArrayVar(&removeLabels, "remove-label", nil, "label key to remove (repeatable)")
			fl.StringVar(&settings, "settings", "", "JSON merge patch of the task settings overrides (null unsets a field)")
		},
		func(c *client.Client, cmd *cobra.Command, s client.TaskSelection) ([]client.BatchItem, error) {
			if len(labels) > 0 {
				m, err := parseKeyValues("label", labels)
				if err != nil {
					return nil, err
				}
				change.Labels = m
			}
			change.RemoveLabels = removeLabels
			if strings.TrimSpace(settings) != "" {
				if !json.Valid([]byte(settings)) {
					return nil, &usageError{err: errors.New("--settings is not valid JSON")}
				}
				change.Settings = json.RawMessage(settings)
			}
			if change.Priority == "" && change.QueueID == "" && len(change.Labels) == 0 && len(change.RemoveLabels) == 0 && change.Settings == nil {
				return nil, &usageError{err: fmt.Errorf("nothing to change: give --priority, --to, --label, --remove-label or --settings")}
			}
			return c.UpdateTasks(cmd.Context(), s, change)
		})
}

func (a *app) newTaskRestartTasksCommand() *cobra.Command {
	return a.newTaskBulkSelectCommand("restart-tasks [ID...]", "Download several finished or failed tasks again",
		"Restart each chosen task from scratch.", "restart-tasks", nil,
		func(c *client.Client, cmd *cobra.Command, s client.TaskSelection) ([]client.BatchItem, error) {
			return c.RestartTasks(cmd.Context(), s)
		})
}

func (a *app) newTaskRefreshTasksCommand() *cobra.Command {
	return a.newTaskBulkSelectCommand("refresh-tasks [ID...]", "Refresh the links of several tasks",
		"Run the refresh strategies of each chosen task now (see 'dlm task refresh').", "refresh-tasks", nil,
		func(c *client.Client, cmd *cobra.Command, s client.TaskSelection) ([]client.BatchItem, error) {
			return c.RefreshTasks(cmd.Context(), s)
		})
}
