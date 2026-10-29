package cli

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
	"github.com/sabadia/dlm-server/internal/portable"
)

func (a *app) newTaskExportCommand() *cobra.Command {
	var opts client.ExportOptions
	var file string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write tasks as a list (JSON, URL list or aria2 input file)",
		Long: "Writes the matching tasks to stdout, or to --file, as dlm JSON (default), one URL per line, or an aria2 input " +
			"file. Only the link, mirrors, file name, directory, referrer and user agent are exported: captured headers and " +
			"cookies, which may hold logins, never are. dlm task import reads the list back.",
		Example:     "  dlm task export --format urls --status completed\n  dlm task export -f backup.json",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "export-tasks"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.Format == "" && file != "" {
				opts.Format = portable.Detect(file)
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			res, err := c.ExportTasks(cmd.Context(), opts)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if file != "" {
				if err := os.WriteFile(file, []byte(res.Content), 0o600); err != nil {
					return fmt.Errorf("write %s: %w", file, err)
				}
				writeErr(cmd.ErrOrStderr(), fmt.Sprintf("exported %d tasks to %s", res.Count, file))
				return nil
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), res.Content)
			return err //nolint:wrapcheck // stdout write errors need no context
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&opts.Format, "format", "", "json (default), urls or aria2")
	fl.StringVarP(&file, "file", "f", "", "write to this file instead of stdout (the format follows .json/.aria2/.txt)")
	fl.StringVar(&opts.QueueID, "queue", "", "only tasks of this queue ID")
	fl.StringSliceVar(&opts.Status, "status", nil, "only tasks in this status (repeatable or comma-separated)")
	fl.StringVar(&opts.Search, "search", "", "only tasks whose name or URL contains this text")
	return cmd
}

func (a *app) newTaskImportCommand() *cobra.Command {
	var req client.ImportRequest
	var format string
	cmd := &cobra.Command{
		Use:   "import FILE",
		Short: "Add the downloads of a list (JSON, URL list or aria2 input file)",
		Long: "Reads a list written by dlm task export, a file of URLs, or an aria2 input file ('-' reads stdin) and adds one " +
			"task per entry; entries that fail do not stop the others. The format follows the file extension " +
			"(.json, .aria2, .txt) unless --format says otherwise.",
		Example:     "  dlm task import backup.json\n  dlm task import downloads.aria2 --paused\n  dlm task import - --format urls < urls.txt",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "import-tasks"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if format == "" {
				format = portable.Detect(args[0])
			}
			if format == "" {
				return &usageError{err: errors.New("cannot tell the format from the file name: give --format json, urls or aria2")}
			}
			data, err := readInput(args[0], cmd.InOrStdin())
			if err != nil {
				return err
			}
			req.Format, req.Content = format, string(data)
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			items, err := c.ImportTasks(cmd.Context(), req)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printBatch(cmd, items)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&format, "format", "", "json, urls or aria2 (default: from the file extension)")
	fl.StringVar(&req.QueueID, "queue", "", "queue ID (default: Main)")
	fl.BoolVar(&req.Paused, "paused", false, "add the tasks paused")
	fl.BoolVar(&req.AllowDuplicate, "allow-duplicate", false, "add tasks even when another task has the same link, checksum, or file name and size")
	fl.BoolVar(&req.ExpandPatterns, "expand", false, "expand ranges in the URLs: file[001-100].jpg is 100 tasks")
	fl.BoolVar(&req.DryRun, "dry-run", false, "create nothing; show per entry whether it would be added or why not")
	return cmd
}

func (a *app) newStatsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "stats",
		Short:       "Show download statistics",
		Long:        "Shows the number of tasks per status and per engine (protocol), the bytes downloaded, and what completed in the last 24 hours, 7 days and 30 days.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "get-stats"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			st, err := c.GetStats(cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(st, func() output.Table { return statsTable(st) })
		},
	}
	cmd.AddCommand(a.newStatsTimelineCommand())
	return cmd
}

func statsTable(st *client.Stats) output.Table {
	t := output.Table{Columns: []string{"what", "tasks", "bytes"}}
	t.Rows = append(t.Rows, []string{"all tasks", strconv.Itoa(st.Tasks), formatBytes(st.DownloadedBytes)})
	statuses := make([]string, 0, len(st.ByStatus))
	for s := range st.ByStatus {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)
	for _, s := range statuses {
		t.Rows = append(t.Rows, []string{"status " + s, strconv.Itoa(st.ByStatus[s]), ""})
	}
	for _, e := range st.ByEngine {
		t.Rows = append(t.Rows, []string{"engine " + e.Engine, strconv.Itoa(e.Tasks), formatBytes(e.DownloadedBytes)})
	}
	for _, c := range st.Completed {
		t.Rows = append(t.Rows, []string{"completed in " + c.Period, strconv.Itoa(c.Tasks), formatBytes(c.Bytes)})
	}
	return t
}
