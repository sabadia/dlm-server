package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func queueTable(qs []*client.Queue) output.Table {
	t := output.Table{Columns: []string{"id", "name", "state", "position", "max concurrent"}}
	for _, q := range qs {
		maxConc := "-" // inherited
		if raw, ok := lookupPath(q.Settings, "maxConcurrentDownloads"); ok {
			maxConc = compactJSON(raw)
		}
		t.Rows = append(t.Rows, []string{q.ID, q.Name, q.State, strconv.FormatInt(q.Position, 10), maxConc})
	}
	return t
}

func (a *app) printQueue(cmd *cobra.Command, q *client.Queue) error {
	return a.printer(cmd).Print(q, func() output.Table { return queueTable([]*client.Queue{q}) })
}

// queueSettingsFlags are the settings shortcuts of queue add/set.
type queueSettingsFlags struct {
	maxConcurrent int
	speedLimit    string
	sets          []string
	unset         []string
}

func (f *queueSettingsFlags) bind(cmd *cobra.Command, withUnset bool) {
	fl := cmd.Flags()
	fl.IntVar(&f.maxConcurrent, "max-concurrent", 0, "tasks the queue runs at once (setting maxConcurrentDownloads)")
	fl.StringVar(&f.speedLimit, "speed-limit", "", "queue speed limit in bytes/s: "+sizeHelp)
	fl.StringArrayVar(&f.sets, "set", nil, "queue setting path=value (repeatable; value parsed as JSON when valid)")
	if withUnset {
		fl.StringArrayVar(&f.unset, "unset", nil, "remove a queue setting override (repeatable)")
	}
}

// doc builds the settings layer (add) or merge patch (set) from the flags.
func (f *queueSettingsFlags) doc(cmd *cobra.Command) (settingsDoc, error) {
	doc := settingsDoc{}
	if cmd.Flags().Changed("max-concurrent") {
		if err := doc.set("maxConcurrentDownloads", f.maxConcurrent); err != nil {
			return nil, err
		}
	}
	if cmd.Flags().Changed("speed-limit") {
		n, err := parseSize(f.speedLimit)
		if err != nil {
			return nil, &usageError{err: err}
		}
		if err := doc.set("speedLimit", n); err != nil {
			return nil, err
		}
	}
	if err := doc.apply(f.sets, f.unset); err != nil {
		return nil, &usageError{err: err}
	}
	return doc, nil
}

func (a *app) newQueueCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "queue",
		Short: "Manage download queues",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		a.newQueueListCommand(),
		a.newQueueAddCommand(),
		a.newQueueGetCommand(),
		a.newQueueSetCommand(),
		a.newQueueReplaceSettingsCommand(),
		a.newQueueRemoveCommand(),
		a.newQueueStateCommand("start", "Start a queue (its tasks may run)", "start-queue", (*client.Client).StartQueue),
		a.newQueueStateCommand("stop", "Stop a queue (running tasks return to queued)", "stop-queue", (*client.Client).StopQueue),
		a.newQueueReorderCommand(),
		a.newQueueSettingsCommand(),
	)
	return cmd
}

func (a *app) newQueueListCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List queues",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "list-queues"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			qs, err := c.ListQueues(cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if qs == nil {
				qs = []*client.Queue{}
			}
			return a.printer(cmd).Print(qs, func() output.Table { return queueTable(qs) })
		},
	}
}

func (a *app) newQueueAddCommand() *cobra.Command {
	var (
		f       queueSettingsFlags
		stopped bool
	)
	cmd := &cobra.Command{
		Use:         "add NAME",
		Short:       "Create a queue",
		Example:     "  dlm queue add fast --max-concurrent 2 --speed-limit 1MB",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "create-queue"},
		RunE: func(cmd *cobra.Command, args []string) error {
			doc, err := f.doc(cmd)
			if err != nil {
				return err
			}
			layer, err := doc.raw()
			if err != nil {
				return err
			}
			req := client.CreateQueueRequest{Name: args[0], Settings: layer}
			if stopped {
				req.State = "stopped"
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			q, err := c.CreateQueue(cmd.Context(), req)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printQueue(cmd, q)
		},
	}
	f.bind(cmd, false)
	cmd.Flags().BoolVar(&stopped, "stopped", false, "create the queue stopped")
	return cmd
}

func (a *app) newQueueGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "get ID",
		Short:       "Show a queue",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "get-queue"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			q, err := c.GetQueue(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printQueue(cmd, q)
		},
	}
}

func (a *app) newQueueSetCommand() *cobra.Command {
	var (
		f    queueSettingsFlags
		name string
	)
	cmd := &cobra.Command{
		Use:   "set ID",
		Short: "Rename a queue or change its settings overrides",
		Long: "Rename a queue and/or patch its settings overrides. --set path=value and --unset path build a JSON " +
			"merge patch (dotted paths, values parsed as JSON when valid); --unset makes the setting inherit again.",
		Example:     "  dlm queue set <id> --name fast --max-concurrent 3 --unset speedLimit",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "update-queue"},
		RunE: func(cmd *cobra.Command, args []string) error {
			doc, err := f.doc(cmd)
			if err != nil {
				return err
			}
			patch, err := doc.raw()
			if err != nil {
				return err
			}
			req := client.UpdateQueueRequest{SettingsPatch: patch}
			if cmd.Flags().Changed("name") {
				req.Name = &name
			}
			if req.Name == nil && patch == nil {
				return &usageError{err: errors.New("nothing to change; give at least one flag")}
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			q, err := c.UpdateQueue(cmd.Context(), args[0], req)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printQueue(cmd, q)
		},
	}
	f.bind(cmd, true)
	cmd.Flags().StringVar(&name, "name", "", "new queue name")
	return cmd
}

func (a *app) newQueueReplaceSettingsCommand() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:         "replace-settings ID -f FILE",
		Short:       "Replace a queue's settings overrides with a JSON or YAML file",
		Long:        "Replace all settings overrides of a queue with the settings object in FILE (JSON or YAML; '-' reads stdin).",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "replace-queue-settings"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return &usageError{err: errors.New("--file is required")}
			}
			layer, err := readSettingsFile(file, cmd.InOrStdin())
			if err != nil {
				return err
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			q, err := c.ReplaceQueueSettings(cmd.Context(), args[0], layer)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printQueue(cmd, q)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "settings file (JSON or YAML; '-' for stdin)")
	return cmd
}

func (a *app) newQueueRemoveCommand() *cobra.Command {
	var moveTo string
	cmd := &cobra.Command{
		Use:         "rm ID",
		Aliases:     []string{"delete"},
		Short:       "Delete a queue",
		Long:        "Delete a queue. A queue that still has tasks needs --move-tasks-to; the Main queue cannot be deleted.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "delete-queue"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			if err := c.DeleteQueue(cmd.Context(), args[0], moveTo); err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if a.format == output.FormatTable {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "deleted "+args[0])
				return err //nolint:wrapcheck // stdout write errors need no context
			}
			return a.printer(cmd).Print(map[string]any{"id": args[0], "deleted": true}, nil)
		},
	}
	cmd.Flags().StringVar(&moveTo, "move-tasks-to", "", "queue ID that receives the deleted queue's tasks")
	return cmd
}

// queueStateFunc is start-queue or stop-queue.
type queueStateFunc func(*client.Client, context.Context, string) (*client.Queue, error)

func (a *app) newQueueStateCommand(verb, short, op string, call queueStateFunc) *cobra.Command {
	return &cobra.Command{
		Use:         verb + " ID",
		Short:       short,
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: op},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			q, err := call(c, cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printQueue(cmd, q)
		},
	}
}

func (a *app) newQueueReorderCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "reorder ID POSITION",
		Short:       "Move a queue to a 0-based position in the queue order",
		Args:        cobra.ExactArgs(2),
		Annotations: map[string]string{annotationOperationID: "reorder-queue"},
		RunE: func(cmd *cobra.Command, args []string) error {
			pos, err := parsePosition(args[1])
			if err != nil {
				return err
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			q, err := c.ReorderQueue(cmd.Context(), args[0], pos)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printQueue(cmd, q)
		},
	}
}

func (a *app) newQueueSettingsCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "settings ID",
		Short:       "Show a queue's effective settings and where each value comes from",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "get-queue-effective-settings"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			eff, err := c.QueueEffectiveSettings(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printEffective(cmd, eff)
		},
	}
}
