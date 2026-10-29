package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

// shortIDLen is the number of trailing ID characters shown in task tables.
// Task IDs are UUIDv7, whose leading characters are a timestamp shared by
// tasks created close together, so the random tail is shown instead.
const shortIDLen = 8

func shortID(id string) string {
	if len(id) <= shortIDLen {
		return id
	}
	return id[len(id)-shortIDLen:]
}

func formatProgress(t *client.Task) string {
	p := t.Progress()
	if p < 0 {
		return "-"
	}
	return strconv.FormatFloat(p*100, 'f', 1, 64) + "%"
}

// displayURL shortens what is not meant to be read: a data: URL (an embedded
// .torrent file can be megabytes) becomes its type and size.
func displayURL(u string) string {
	if !strings.HasPrefix(strings.ToLower(u), "data:") {
		return u
	}
	head, _, _ := strings.Cut(u, ",")
	return fmt.Sprintf("%s,… (%s)", head, formatSize(int64(len(u))))
}

// taskTable renders tasks; fullID shows complete IDs instead of short ones.
func taskTable(tasks []*client.Task, fullID bool) output.Table {
	t := output.Table{Columns: []string{"id", "name", "status", "progress", "size", "queue"}}
	for _, task := range tasks {
		id := task.ID
		if !fullID {
			id = shortID(id)
		}
		name := task.Name
		if name == "" {
			name = displayURL(task.Source.CurrentURL)
		}
		t.Rows = append(t.Rows, []string{id, name, task.Status, formatProgress(task), formatSize(task.Size), task.QueueID})
	}
	return t
}

func (a *app) printTask(cmd *cobra.Command, t *client.Task) error {
	return a.printer(cmd).Print(t, func() output.Table { return taskTable([]*client.Task{t}, true) })
}

// errItemsFailed reports that some items of a bulk operation failed.
type errItemsFailed struct{ failed, total int }

func (e *errItemsFailed) Error() string {
	return fmt.Sprintf("%d of %d items failed", e.failed, e.total)
}

// printBatch prints the per-item outcome of a bulk operation and returns an
// error (exit code 1) when any item failed.
func (a *app) printBatch(cmd *cobra.Command, items []client.BatchItem) error {
	if items == nil {
		items = []client.BatchItem{}
	}
	err := a.printer(cmd).Print(items, func() output.Table {
		t := output.Table{Columns: []string{"id", "result", "status", "error"}}
		for _, it := range items {
			result, status, msg := "ok", "", ""
			if it.Task != nil {
				status = it.Task.Status
			}
			if !it.OK {
				result = "failed"
				if it.Error != nil {
					msg = it.Error.Error()
				}
			}
			t.Rows = append(t.Rows, []string{it.ID, result, status, msg})
		}
		return t
	})
	if err != nil {
		return err
	}
	if len(items) == 0 {
		writeErr(cmd.ErrOrStderr(), "note: no tasks matched")
	}
	failed := 0
	for _, it := range items {
		if !it.OK {
			failed++
		}
	}
	if failed > 0 {
		return &errItemsFailed{failed: failed, total: len(items)}
	}
	return nil
}

// selectFlags select the tasks of a bulk command by filter.
type selectFlags struct {
	all    bool
	queue  string
	status []string
}

func (f *selectFlags) bind(cmd *cobra.Command, verb string) {
	fl := cmd.Flags()
	fl.BoolVar(&f.all, "all", false, verb+" every task")
	fl.StringVar(&f.queue, "queue", "", verb+" the tasks of this queue ID")
	fl.StringSliceVar(&f.status, "status", nil, verb+" the tasks in this status (repeatable or comma-separated)")
}

func (f *selectFlags) filtered() bool { return f.all || f.queue != "" || len(f.status) > 0 }

// selection returns explicit IDs or the filter; exactly one must be given.
func (f *selectFlags) selection(ids []string) (client.TaskSelection, error) {
	switch {
	case len(ids) > 0 && f.filtered():
		return client.TaskSelection{}, &usageError{err: errors.New("give task IDs or --all/--queue/--status, not both")}
	case len(ids) > 0:
		return client.TaskSelection{IDs: ids}, nil
	case f.filtered():
		return client.TaskSelection{Filter: &client.TaskFilter{QueueID: f.queue, Status: f.status}}, nil
	default:
		return client.TaskSelection{}, &usageError{err: errors.New("give task IDs or one of --all, --queue, --status")}
	}
}

func (a *app) newResolveCommand() *cobra.Command {
	var (
		queue string
		sets  []string
	)
	cmd := &cobra.Command{
		Use:   "resolve URL",
		Short: "Probe a URL without downloading it",
		Long: "Probe a URL with the effective settings and show what would be downloaded. The resolve ID can be " +
			"passed to the API's create-task until it expires.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "resolve-task"},
		RunE: func(cmd *cobra.Command, args []string) error {
			doc := settingsDoc{}
			if err := doc.apply(sets, nil); err != nil {
				return &usageError{err: err}
			}
			layer, err := doc.raw()
			if err != nil {
				return err
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			res, err := c.ResolveTask(cmd.Context(), client.ResolveRequest{URL: args[0], QueueID: queue, Settings: layer})
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(res, func() output.Table {
				return output.Table{
					Columns: []string{"resolve id", "name", "size", "ranges", "engine", "expires", "formats"},
					Rows: [][]string{{
						res.ResolveID, res.Name, formatSize(res.Resource.Size),
						strconv.FormatBool(res.Resource.RangeSupported), res.Engine, res.ExpiresAt.Local().Format(time.TimeOnly),
						formatLabels(res.Formats),
					}},
				}
			})
		},
	}
	cmd.Flags().StringVar(&queue, "queue", "", "queue ID whose settings apply (default: Main)")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "task setting override path=value (repeatable), e.g. retry.maxAttempts=3")
	return cmd
}

func (a *app) newTaskCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Manage download tasks",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		a.newTaskAddCommand(),
		a.newTaskExportCommand(),
		a.newTaskImportCommand(),
		a.newTaskListCommand(),
		a.newTaskGetCommand(),
		a.newTaskSetCommand(),
		a.newTaskMoveCommand(),
		a.newTaskReorderCommand(),
		a.newTaskBulkCommand("pause", "Pause tasks", "pause-tasks", (*client.Client).PauseTasks),
		a.newTaskBulkCommand("resume", "Resume paused or failed tasks", "resume-tasks", (*client.Client).ResumeTasks),
		a.newTaskRestartCommand(),
		a.newTaskMoveTasksCommand(),
		a.newTaskUpdateTasksCommand(),
		a.newTaskRestartTasksCommand(),
		a.newTaskRefreshTasksCommand(),
		a.newTaskRefreshCommand(),
		a.newTaskAwaitLinkCommand(),
		a.newTaskSetURLCommand(),
		a.newTaskRefreshHistoryCommand(),
		a.newTaskSegmentsCommand(),
		a.newTaskExpandCommand(),
		a.newTaskCleanupCommand(),
		a.newTaskFilesCommand(),
		a.newTaskTorrentCommand(),
		a.newTaskRemoveCommand(),
		a.newTaskSettingsCommand(),
	)
	return cmd
}

type taskAddFlags struct {
	file        string
	queue       string
	name        string
	dir         string
	paused      bool
	priority    string
	connections int
	speedLimit  string
	headers     []string
	sets        []string
	ytdlp       bool
	format      string
	checksum    string
	allowDup    bool
	expand      bool
	mirrors     []string
}

// settings builds the task-level settings layer from the flags.
func (f *taskAddFlags) settings(cmd *cobra.Command) (settingsDoc, error) {
	doc := settingsDoc{}
	if cmd.Flags().Changed("connections") {
		if err := doc.set("maxConnections", f.connections); err != nil {
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
	if f.checksum != "" {
		cs, err := parseChecksum(f.checksum)
		if err != nil {
			return nil, &usageError{err: err}
		}
		if err := doc.set("checksum", cs); err != nil {
			return nil, err
		}
	}
	if len(f.headers) > 0 {
		h, err := parseKeyValues("header", f.headers)
		if err != nil {
			return nil, err
		}
		if err := doc.set("headers", h); err != nil {
			return nil, err
		}
	}
	if err := doc.apply(f.sets, nil); err != nil {
		return nil, &usageError{err: err}
	}
	return doc, nil
}

func (a *app) newTaskAddCommand() *cobra.Command {
	var f taskAddFlags
	cmd := &cobra.Command{
		Use:   "add URL...",
		Short: "Add download tasks",
		Long: "Add one task per URL (and per line of --file; '-' reads stdin, blank lines and # comments are skipped).\n\n" +
			"One URL creates a single task (create-task); several URLs or a file create them in one batch " +
			"(create-tasks) and print one result per URL. Settings flags apply to every task.\n\n" +
			"A URL can be a magnet link, an ed2k:// link, or the path of a local .torrent file, which is sent to the daemon (files above 512 KiB are uploaded as they are).\n\n" +
			"--ytdlp downloads a video or audio page with yt-dlp instead of fetching the URL itself; --format picks the " +
			"yt-dlp format (for example 'bv*[height<=720]+ba/b') or the quality of an HLS/DASH stream (for example 720).",
		Example: "  dlm task add https://example.com/file.iso --queue <id> --speed-limit 1MB\n" +
			"  dlm task add -f urls.txt --paused\n" +
			"  dlm task add 'https://example.com/img[001-100].jpg' --expand\n" +
			"  dlm task add https://a.example/f.iso --mirror https://b.example/f.iso --mirror https://c.example/f.iso\n" +
			"  dlm task add ./ubuntu.iso.torrent 'magnet:?xt=urn:btih:...'\n" +
			"  dlm task add https://example.com/file.iso --checksum sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08\n" +
			"  dlm task add --ytdlp 'https://video.example/watch?v=abc' --format 'bv*[height<=720]+ba/b'\n" +
			"  dlm task add https://example.com/a.bin --header 'Cookie=x=1' --set retry.maxAttempts=10",
		Annotations: map[string]string{annotationOperationID: "create-task,create-tasks,add-torrent-file"},
		RunE: func(cmd *cobra.Command, args []string) error {
			urls := slices.Clone(args)
			if f.file != "" {
				more, err := readURLList(f.file, cmd.InOrStdin())
				if err != nil {
					return err
				}
				urls = append(urls, more...)
			}
			if len(urls) == 0 {
				return &usageError{err: errors.New("give at least one URL or --file")}
			}
			batch := len(urls) > 1 || f.file != "" || f.expand
			if batch && f.name != "" {
				return &usageError{err: errors.New("--name can only be used with a single URL")}
			}
			if batch && len(f.mirrors) > 0 {
				return &usageError{err: errors.New("--mirror can only be used with a single URL")}
			}
			doc, err := f.settings(cmd)
			if err != nil {
				return err
			}
			layer, err := doc.raw()
			if err != nil {
				return err
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			// A big .torrent file does not fit in the JSON of create-task: it is uploaded as it is.
			var rest []string
			for _, u := range urls {
				data, ok, err := largeTorrentFile(u)
				if err != nil {
					return err
				}
				if !ok {
					rest = append(rest, u)
					continue
				}
				t, err := c.AddTorrentFile(cmd.Context(), data, client.TorrentUpload{
					QueueID: f.queue, Name: f.name, Dir: f.dir, Priority: f.priority, Paused: f.paused, AllowDuplicate: f.allowDup,
				})
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				if err := a.printTask(cmd, t); err != nil {
					return err
				}
			}
			if urls = rest; len(urls) == 0 {
				return nil
			}
			batch = len(urls) > 1 || f.file != "" || f.expand
			reqs := make([]client.CreateTaskRequest, len(urls))
			for i, u := range urls {
				u, err := resolveTorrentFile(u)
				if err != nil {
					return err
				}
				reqs[i] = client.CreateTaskRequest{
					URL: u, QueueID: f.queue, Name: f.name, Dir: f.dir, Settings: layer,
					Priority: f.priority, Paused: f.paused, AllowDuplicate: f.allowDup,
				}
				if f.ytdlp || f.format != "" || len(f.mirrors) > 0 {
					src := &client.Source{CurrentURL: u, MediaHint: f.format, Mirrors: f.mirrors}
					if f.ytdlp {
						src.ResolverKind = "ytdlp"
					}
					reqs[i].URL, reqs[i].Source = "", src
				}
			}
			if !batch {
				t, err := c.CreateTask(cmd.Context(), reqs[0])
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				return a.printTask(cmd, t)
			}
			items, err := c.CreateTasksExpanded(cmd.Context(), reqs, f.expand)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printBatch(cmd, items)
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&f.file, "file", "f", "", "read URLs from this file, one per line ('-' for stdin)")
	fl.StringVar(&f.queue, "queue", "", "queue ID (default: Main)")
	fl.StringVar(&f.name, "name", "", "file name (single URL only; default: detected)")
	fl.StringVar(&f.dir, "dir", "", "absolute download directory (default: the effective downloadDir)")
	fl.BoolVar(&f.paused, "paused", false, "add the tasks paused")
	fl.StringArrayVar(&f.mirrors, "mirror", nil, "another http(s) link to the same file (repeatable, single URL only)")
	fl.BoolVar(&f.expand, "expand", false, "expand ranges in the URLs: file[001-100].jpg is 100 tasks ([a-e] and [1-10:2] work too)")
	fl.BoolVar(&f.allowDup, "allow-duplicate", false, "add the tasks even when another task has the same link, checksum, or file name and size")
	fl.StringVar(&f.checksum, "checksum", "", "expected checksum ALGORITHM:HEX (md5, sha1, sha256 or blake3); a file that does not match is downloaded again")
	fl.BoolVar(&f.ytdlp, "ytdlp", false, "download the URLs with yt-dlp (video and audio pages)")
	fl.StringVar(&f.format, "format", "", "media format: a yt-dlp format selector with --ytdlp, or the quality of an HLS/DASH stream")
	fl.StringVar(&f.priority, "priority", "", "priority: low, normal or high")
	fl.IntVar(&f.connections, "connections", 0, "maximum connections per task (setting maxConnections)")
	fl.StringVar(&f.speedLimit, "speed-limit", "", "speed limit per task in bytes/s: "+sizeHelp)
	fl.StringArrayVar(&f.headers, "header", nil, "extra request header Name=Value (repeatable)")
	fl.StringArrayVar(&f.sets, "set", nil, "task setting path=value (repeatable; value parsed as JSON when valid), e.g. retry.maxAttempts=3")
	return cmd
}

func (a *app) newTaskListCommand() *cobra.Command {
	var (
		opts client.ListTasksOptions
		all  bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tasks",
		Long: "List tasks, one page at a time (--limit, --cursor) or all of them (--all follows the cursors).\n\n" +
			"The table shows the last " + strconv.Itoa(shortIDLen) + " characters of each task ID; use -o json for full IDs.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "list-tasks"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			page, err := c.ListTasks(cmd.Context(), opts)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			for all && page.NextCursor != "" {
				o := opts
				o.Cursor = page.NextCursor
				next, err := c.ListTasks(cmd.Context(), o)
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				page.Tasks, page.NextCursor = append(page.Tasks, next.Tasks...), next.NextCursor
			}
			if page.Tasks == nil {
				page.Tasks = []*client.Task{}
			}
			if err := a.printer(cmd).Print(page, func() output.Table { return taskTable(page.Tasks, false) }); err != nil {
				return err
			}
			if page.Total != nil && a.format == output.FormatTable {
				writeErr(cmd.ErrOrStderr(), fmt.Sprintf("note: %d tasks match", *page.Total))
			}
			if page.NextCursor != "" && a.format == output.FormatTable {
				writeErr(cmd.ErrOrStderr(), "note: more tasks available; use --all or --cursor "+page.NextCursor)
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&opts.QueueID, "queue", "", "only tasks of this queue ID")
	fl.StringSliceVar(&opts.Status, "status", nil, "only tasks in this status (repeatable or comma-separated)")
	fl.StringVar(&opts.Search, "search", "", "only tasks whose name or URL contains this text")
	fl.StringVar(&opts.Sort, "sort", "", "sort: position (queue order, default), created (newest first), name, size or completed")
	fl.StringVar(&opts.Order, "order", "", "direction: asc or desc (default: the one of --sort)")
	fl.StringVar(&opts.Engine, "engine", "", "only tasks of this engine (http, bt, ...)")
	fl.StringVar(&opts.Label, "label", "", "only tasks with this label: KEY or KEY=VALUE")
	fl.StringVar(&opts.Category, "category", "", "only tasks of this category (ID or name)")
	fl.Func("min-size", "only tasks of at least this size (500K, 2MB, ...)", func(v string) error {
		n, err := parseSize(v)
		opts.MinSize = &n
		return err
	})
	fl.Func("max-size", "only tasks of at most this size (500K, 2MB, ...)", func(v string) error {
		n, err := parseSize(v)
		opts.MaxSize = &n
		return err
	})
	fl.StringVar(&opts.CreatedAfter, "created-after", "", "only tasks created at or after this time (RFC 3339)")
	fl.StringVar(&opts.CreatedBefore, "created-before", "", "only tasks created at or before this time (RFC 3339)")
	fl.StringVar(&opts.CompletedAfter, "completed-after", "", "only tasks completed at or after this time (RFC 3339)")
	fl.StringVar(&opts.CompletedBefore, "completed-before", "", "only tasks completed at or before this time (RFC 3339)")
	fl.BoolVar(&opts.Total, "total", false, "also count every matching task")
	fl.IntVar(&opts.Limit, "limit", 0, "page size, 1-500 (default 50)")
	fl.StringVar(&opts.Cursor, "cursor", "", "continue after this cursor (from a previous page)")
	fl.BoolVar(&all, "all", false, "follow cursors and list every matching task")
	return cmd
}

func (a *app) newTaskGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "get ID",
		Short:       "Show a task",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "get-task"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			t, err := c.GetTask(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printTask(cmd, t)
		},
	}
}

func (a *app) newTaskSetCommand() *cobra.Command {
	var (
		name, dir, priority string
		labels, sets, unset []string
	)
	cmd := &cobra.Command{
		Use:   "set ID",
		Short: "Change a task",
		Long: "Change a task's name, directory, priority, labels or settings overrides.\n\n" +
			"--label replaces all labels. --set path=value and --unset path build a JSON merge patch of the task's " +
			"settings: dotted paths, values parsed as JSON when valid (else a string); --unset makes the setting " +
			"inherit again.",
		Example:     "  dlm task set <id> --priority high --set speedLimit=1048576 --unset retry.maxAttempts",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "update-task"},
		RunE: func(cmd *cobra.Command, args []string) error {
			var req client.UpdateTaskRequest
			fl := cmd.Flags()
			if fl.Changed("name") {
				req.Name = &name
			}
			if fl.Changed("dir") {
				req.Dir = &dir
			}
			if fl.Changed("priority") {
				req.Priority = &priority
			}
			if fl.Changed("label") {
				m, err := parseKeyValues("label", labels)
				if err != nil {
					return err
				}
				req.Labels = &m
			}
			doc := settingsDoc{}
			if err := doc.apply(sets, unset); err != nil {
				return &usageError{err: err}
			}
			patch, err := doc.raw()
			if err != nil {
				return err
			}
			req.SettingsPatch = patch
			if req.Name == nil && req.Dir == nil && req.Priority == nil && req.Labels == nil && patch == nil {
				return &usageError{err: errors.New("nothing to change; give at least one flag")}
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			t, err := c.UpdateTask(cmd.Context(), args[0], req)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printTask(cmd, t)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&name, "name", "", "new file name")
	fl.StringVar(&dir, "dir", "", "new absolute download directory")
	fl.StringVar(&priority, "priority", "", "new priority: low, normal or high")
	fl.StringArrayVar(&labels, "label", nil, "label key=value (repeatable; replaces all labels)")
	fl.StringArrayVar(&sets, "set", nil, "set a setting override path=value (repeatable)")
	fl.StringArrayVar(&unset, "unset", nil, "remove a setting override (repeatable)")
	return cmd
}

func (a *app) newTaskMoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "move ID QUEUE",
		Short:       "Move a task to the end of another queue",
		Args:        cobra.ExactArgs(2),
		Annotations: map[string]string{annotationOperationID: "move-task"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			t, err := c.MoveTask(cmd.Context(), args[0], args[1])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printTask(cmd, t)
		},
	}
}

// parsePosition parses a 0-based position argument.
func parsePosition(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, &usageError{err: fmt.Errorf("invalid position %q: want a number >= 0", s)}
	}
	return n, nil
}

func (a *app) newTaskReorderCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "reorder ID POSITION",
		Short:       "Move a task to a 0-based position within its queue",
		Args:        cobra.ExactArgs(2),
		Annotations: map[string]string{annotationOperationID: "reorder-task"},
		RunE: func(cmd *cobra.Command, args []string) error {
			pos, err := parsePosition(args[1])
			if err != nil {
				return err
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			t, err := c.ReorderTask(cmd.Context(), args[0], pos)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printTask(cmd, t)
		},
	}
}

// bulkFunc is a client bulk operation (pause-tasks, resume-tasks).
type bulkFunc func(*client.Client, context.Context, client.TaskSelection) ([]client.BatchItem, error)

func (a *app) newTaskBulkCommand(verb, short, op string, call bulkFunc) *cobra.Command {
	var sel selectFlags
	cmd := &cobra.Command{
		Use:         verb + " [ID...]",
		Short:       short,
		Long:        short + " by ID, or every task matching --all, --queue and/or --status. Prints one result per task; exits 1 if any failed.",
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
			items, err := call(c, cmd.Context(), s)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printBatch(cmd, items)
		},
	}
	sel.bind(cmd, verb)
	return cmd
}

func (a *app) newTaskRestartCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "restart ID",
		Short:       "Download a completed or failed task again from scratch",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "restart-task"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			t, err := c.RestartTask(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printTask(cmd, t)
		},
	}
}

func (a *app) newTaskRemoveCommand() *cobra.Command {
	var (
		sel   selectFlags
		files bool
	)
	cmd := &cobra.Command{
		Use:     "rm [ID...]",
		Aliases: []string{"delete"},
		Short:   "Delete tasks",
		Long: "Delete tasks by ID, or every task matching --all, --queue and/or --status (running tasks are stopped " +
			"first). --files also deletes the partial and downloaded files.\n\n" +
			"A single ID uses delete-task; several IDs or a filter use delete-tasks and print one result per task.",
		Annotations: map[string]string{annotationOperationID: "delete-task,delete-tasks"},
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := sel.selection(args)
			if err != nil {
				return err
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			if len(s.IDs) == 1 {
				if err := c.DeleteTask(cmd.Context(), s.IDs[0], files); err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				if a.format == output.FormatTable {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "deleted "+s.IDs[0])
					return err //nolint:wrapcheck // stdout write errors need no context
				}
				return a.printer(cmd).Print(client.BatchItem{ID: s.IDs[0], OK: true}, nil)
			}
			items, err := c.DeleteTasks(cmd.Context(), s, files)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printBatch(cmd, items)
		},
	}
	sel.bind(cmd, "delete")
	cmd.Flags().BoolVar(&files, "files", false, "also delete the partial and downloaded files")
	return cmd
}

func (a *app) newTaskSettingsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "settings ID",
		Short: "Show a task's effective settings and where each value comes from",
		Long: "Show the task's effective settings (Defaults ⊕ Global ⊕ Queue ⊕ Task) with the level each value " +
			"comes from (default, global, queue, task).",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "get-task-effective-settings"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			eff, err := c.TaskEffectiveSettings(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printEffective(cmd, eff)
		},
	}
}

// printEffective prints effective settings as PATH VALUE SOURCE rows.
func (a *app) printEffective(cmd *cobra.Command, eff *client.EffectiveSettings) error {
	return a.printer(cmd).Print(eff, func() output.Table {
		t := output.Table{Columns: []string{"path", "value", "source"}}
		paths := make([]string, 0, len(eff.Sources))
		for p := range eff.Sources {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		for _, p := range paths {
			v := "-"
			if raw, ok := lookupPath(eff.Settings, p); ok {
				v = compactJSON(raw)
			}
			t.Rows = append(t.Rows, []string{p, v, eff.Sources[p]})
		}
		return t
	})
}

// formatLabels lists the qualities of a stream for a table cell.
func formatLabels(fs []client.Format) string {
	labels := make([]string, len(fs))
	for i, f := range fs {
		labels[i] = f.Label
	}
	return strings.Join(labels, ", ")
}
