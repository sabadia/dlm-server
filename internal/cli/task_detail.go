package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func (a *app) newTaskSegmentsCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "segments ID",
		Short:       "Show the segments of a task's download",
		Long:        "Show the byte ranges a task downloads in parallel and how much of each is done, as last saved (about once a second while it runs).",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "list-task-segments"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			segs, err := c.TaskSegments(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if segs == nil {
				segs = []client.Segment{}
			}
			return a.printer(cmd).Print(segs, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
				t := output.Table{Columns: []string{"#", "start", "end", "done", "%"}}
				for _, s := range segs {
					pct := 0
					if n := s.End - s.Start; n > 0 {
						pct = int(s.Done * 100 / n)
					}
					t.Rows = append(t.Rows, []string{
						strconv.Itoa(s.Index), formatSize(s.Start), formatSize(s.End), formatSize(s.Done), strconv.Itoa(pct),
					})
				}
				return t
			})
		},
	}
}

func (a *app) newTaskFilesCommand() *cobra.Command {
	var selected []int
	cmd := &cobra.Command{
		Use:   "files ID",
		Short: "List a task's files or choose which are downloaded",
		Long: "Without --select, list the files of a multi-file task (a torrent) and whether each is selected. With --select INDEX[,INDEX…] " +
			"download only those files from now on (every other file is skipped). The task must be paused or not started: " +
			"active and completed tasks cannot change.",
		Example:     "  dlm task files 01J…\n  dlm task files 01J… --select 0,3",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "update-task-files"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			var t *client.Task
			if cmd.Flags().Changed("select") {
				t, err = c.SetTaskFiles(cmd.Context(), args[0], selected)
			} else {
				t, err = c.GetTask(cmd.Context(), args[0])
			}
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			var files []client.File
			if t.Resource != nil {
				files = t.Resource.Files
			}
			if files == nil {
				files = []client.File{}
			}
			return a.printer(cmd).Print(files, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
				tb := output.Table{Columns: []string{"#", "selected", "size", "path"}}
				for _, f := range files {
					path := f.Name
					if f.Path != "" {
						path = f.Path + "/" + f.Name
					}
					tb.Rows = append(tb.Rows, []string{strconv.Itoa(f.Index), strconv.FormatBool(f.Selected), formatSize(f.Size), path})
				}
				return tb
			})
		},
	}
	cmd.Flags().IntSliceVar(&selected, "select", nil, "indexes of the files to download")
	return cmd
}

func (a *app) newTaskTorrentCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "torrent ID",
		Short: "Show the swarm of a torrent task",
		Long: "Show the state, trackers, peers, bytes shared and share ratio of a torrent task: live while it runs, " +
			"otherwise what its last run saved.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "get-task-torrent"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			info, err := c.TaskTorrent(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(info, func() output.Table { return torrentTable(info) }) //nolint:wrapcheck // stdout write errors need no context
		},
	}
}

func torrentTable(i *client.TorrentInfo) output.Table {
	t := output.Table{Columns: []string{"field", "value"}}
	add := func(k, v string) { t.Rows = append(t.Rows, []string{k, v}) }
	add("state", i.State)
	add("name", i.Name)
	add("info hash", i.InfoHash)
	add("private", strconv.FormatBool(i.Private))
	add("downloaded", formatSize(i.Downloaded))
	add("uploaded", formatSize(i.Uploaded))
	add("ratio", fmt.Sprintf("%.2f", i.Ratio))
	add("seeding", fmt.Sprintf("%ds", i.SeedingSeconds))
	add("peers", fmt.Sprintf("%d connected (%d seeders), %d known", i.PeerCount, i.Seeders, i.KnownPeers))
	add("trackers", strings.Join(i.Trackers, "\n"))
	for _, p := range i.Peers {
		add("peer", fmt.Sprintf("%s %s %s/s", p.Address, p.Client, formatSize(p.DownloadRate)))
	}
	return t
}
