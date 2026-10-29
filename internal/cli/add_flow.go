package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func (a *app) newTaskExpandCommand() *cobra.Command {
	var preview int
	cmd := &cobra.Command{
		Use:   "expand PATTERN",
		Short: "Show the URLs a pattern with ranges stands for",
		Long: "Show how many URLs a pattern such as 'https://example.com/img[001-100].jpg' stands for and list the first ones, " +
			"before adding them with 'dlm add --expand'.",
		Example:     "  dlm task expand 'https://example.com/img[001-100].jpg' --preview 5",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "expand-tasks"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			res, err := c.ExpandTasks(cmd.Context(), args[0], preview)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(res, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
				t := output.Table{Columns: []string{"#", "url"}}
				for i, u := range res.URLs {
					t.Rows = append(t.Rows, []string{strconv.Itoa(i + 1), u})
				}
				if res.Count > len(res.URLs) {
					t.Rows = append(t.Rows, []string{"…", fmt.Sprintf("%d URLs in all", res.Count)})
				}
				return t
			})
		},
	}
	cmd.Flags().IntVar(&preview, "preview", 0, "how many URLs to list, 0-1000 (default 20)")
	return cmd
}

func (a *app) newResolveManyCommand() *cobra.Command {
	var queue string
	return &cobra.Command{
		Use:   "resolve-many URL...",
		Short: "Probe several URLs without downloading them",
		Long: "Probe up to 100 URLs, a few at a time, and show for each what would be downloaded, or why it failed, and whether " +
			"another task already has it. The resolve IDs can be passed to the API's create-task until they expire.",
		Args:        cobra.MinimumNArgs(1),
		Annotations: map[string]string{annotationOperationID: "resolve-tasks"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			items := make([]client.ResolveRequest, len(args))
			for i, u := range args {
				items[i] = client.ResolveRequest{URL: u, QueueID: queue}
			}
			res, err := c.ResolveTasks(cmd.Context(), items)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(res, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
				t := output.Table{Columns: []string{"url", "name", "size", "note"}}
				for i, r := range res {
					if r.Result == nil {
						msg := "failed"
						if r.Error != nil {
							msg = r.Error.Detail
						}
						t.Rows = append(t.Rows, []string{args[i], "", "", msg})
						continue
					}
					note := ""
					if d := r.Result.DuplicateOf; d != nil {
						note = fmt.Sprintf("duplicate of %s (%s)", d.Name, d.Status)
					}
					t.Rows = append(t.Rows, []string{args[i], r.Result.Name, formatSize(r.Result.Resource.Size), note})
				}
				return t
			})
		},
	}
}

func (a *app) newFSCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fs",
		Short: "Browse the daemon's machine (limited to server.fs_roots)",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "browse [PATH]",
			Short: "List the directories below a path",
			Long: "List the sub-directories of a directory below server.fs_roots on the machine the daemon runs on, " +
				"for choosing a download directory there. Without a path it lists the roots.",
			Args:        cobra.MaximumNArgs(1),
			Annotations: map[string]string{annotationOperationID: "get-fs-browse"},
			RunE: func(cmd *cobra.Command, args []string) error {
				path := ""
				if len(args) == 1 {
					path = args[0]
				}
				c, err := a.newClient(cmd, client.DefaultTimeout)
				if err != nil {
					return err
				}
				l, err := c.FSBrowse(cmd.Context(), path)
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				return a.printer(cmd).Print(l, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
					t := output.Table{Columns: []string{"directory"}}
					for _, e := range l.Entries {
						t.Rows = append(t.Rows, []string{e.Path})
					}
					return t
				})
			},
		},
		&cobra.Command{
			Use:         "free PATH",
			Short:       "Show the free space of a directory",
			Args:        cobra.ExactArgs(1),
			Annotations: map[string]string{annotationOperationID: "get-fs-free"},
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := a.newClient(cmd, client.DefaultTimeout)
				if err != nil {
					return err
				}
				f, err := c.FSFreeSpace(cmd.Context(), args[0])
				if err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				return a.printer(cmd).Print(f, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
					return output.Table{Columns: []string{"path", "free"}, Rows: [][]string{{f.Path, formatSize(int64(min(f.FreeBytes, 1<<62)))}}} //nolint:gosec // clamped
				})
			},
		},
	)
	return cmd
}
