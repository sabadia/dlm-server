package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

// resolveTimeout is the client timeout of resolve-page: the page may take up
// to its wait time plus the browser start.
const resolveTimeout = 3 * time.Minute

func (a *app) newBrowserCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "browser",
		Short: "Use the internal headless browser",
		Long: "Open pages in the daemon's internal headless browser to find what they download: media manifests, files and " +
			"JS-made downloads. The browser starts on demand, with the stored cookies of the site.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		a.newBrowserResolveCommand(),
		a.newBrowserDownloadCommand(),
		a.newBrowserStatusCommand(),
		a.newBrowserProfilesCommand(),
	)
	return cmd
}

func (a *app) newBrowserResolveCommand() *cobra.Command {
	var wait int
	cmd := &cobra.Command{
		Use:   "resolve URL",
		Short: "Open a page in the headless browser and list its downloads",
		Long: "Open URL in the headless browser and list the downloads it led to (the candidates). The result is kept for a while: " +
			"pass its ID and a candidate INDEX to `dlm browser download`.",
		Example:     "  dlm browser resolve https://example.com/watch?v=1\n  dlm browser download 4f3a… 0 --queue Main",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "resolve-page"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, resolveTimeout)
			if err != nil {
				return err
			}
			res, err := c.ResolvePage(cmd.Context(), args[0], wait)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if a.format != output.FormatTable {
				return a.printer(cmd).Print(res, nil) //nolint:wrapcheck // stdout write errors need no context
			}
			title := res.Title
			if title == "" {
				title = res.FinalURL
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "result %s: %s (%d downloads, kept until %s)\n",
				res.ID, title, len(res.Candidates), res.ExpiresAt.Local().Format(time.TimeOnly)); err != nil {
				return err //nolint:wrapcheck // stdout write errors need no context
			}
			return a.printer(cmd).Print(res, func() output.Table { return candidateTable(res.Candidates) }) //nolint:wrapcheck // stdout write errors need no context
		},
	}
	cmd.Flags().IntVar(&wait, "wait", 0, "seconds the page may keep producing downloads (default: the daemon's, 20)")
	return cmd
}

func candidateTable(cs []client.BrowserCandidate) output.Table {
	t := output.Table{Columns: []string{"index", "kind", "size", "source", "name", "url"}}
	for _, c := range cs {
		size := "-"
		if c.Size >= 0 {
			size = formatSize(c.Size)
		}
		t.Rows = append(t.Rows, []string{strconv.Itoa(c.Index), c.Kind, size, c.Source, c.Filename, c.URL})
	}
	return t
}

func (a *app) newBrowserDownloadCommand() *cobra.Command {
	var (
		queue    string
		name     string
		dir      string
		priority string
		paused   bool
	)
	cmd := &cobra.Command{
		Use:   "download RESULT_ID INDEX",
		Short: "Download a candidate found by `browser resolve`",
		Long: "Create a task for candidate INDEX of the resolve result RESULT_ID. Files the browser downloaded itself (blob and " +
			"attachment downloads) are imported; other candidates are downloaded from their URL. HLS playlists (.m3u8) use the HLS engine; DASH manifests (.mpd) use the DASH engine.",
		Args:        cobra.ExactArgs(2),
		Annotations: map[string]string{annotationOperationID: "download-candidate"},
		RunE: func(cmd *cobra.Command, args []string) error {
			index, err := strconv.Atoi(args[1])
			if err != nil || index < 0 {
				return &usageError{err: fmt.Errorf("INDEX %q must be a non-negative number", args[1])}
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			t, err := c.DownloadCandidate(cmd.Context(), args[0], index, client.DownloadCandidateRequest{
				QueueID: queue, Name: name, Dir: dir, Priority: priority, Paused: paused,
			})
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printTask(cmd, t)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&queue, "queue", "", "queue ID or name (default: Main)")
	fl.StringVar(&name, "name", "", "file name (default: the one the page announced)")
	fl.StringVar(&dir, "dir", "", "absolute download directory")
	fl.StringVar(&priority, "priority", "", "priority: low, normal or high")
	fl.BoolVar(&paused, "paused", false, "create the task paused")
	return cmd
}

func (a *app) newBrowserStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "status",
		Short:       "Show the headless browser in use and the running browsers",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "get-browser-status"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			st, err := c.BrowserStatus(cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(st, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
				t := output.Table{Columns: []string{"browser", "path", "running", "max"}}
				name, path := "(not started)", ""
				if st.Browser != nil {
					name, path = st.Browser.Name, st.Browser.Path
					if st.Browser.Version != "" {
						name += " " + st.Browser.Version
					}
				}
				var groups []string
				for _, r := range st.Running {
					groups = append(groups, fmt.Sprintf("%s(%d)", r.Group, r.Sessions))
				}
				t.Rows = append(t.Rows, []string{name, path, strings.Join(groups, " "), strconv.Itoa(st.Max)})
				return t
			})
		},
	}
}

func (a *app) newBrowserProfilesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profiles",
		Short: "Manage browser profiles",
		Long:  "Each site group has its own browser profile, which keeps the logins of the pages opened there.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.listBrowserProfiles(cmd)
		},
		Annotations: map[string]string{annotationOperationID: "list-browser-profiles"},
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:         "list",
			Short:       "List browser profiles",
			Args:        cobra.NoArgs,
			Annotations: map[string]string{annotationOperationID: "list-browser-profiles"},
			RunE:        func(cmd *cobra.Command, _ []string) error { return a.listBrowserProfiles(cmd) },
		},
		&cobra.Command{
			Use:         "rm GROUP",
			Aliases:     []string{"delete"},
			Short:       "Delete a browser profile (its cookies and storage)",
			Args:        cobra.ExactArgs(1),
			Annotations: map[string]string{annotationOperationID: "delete-browser-profile"},
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := a.newClient(cmd, client.DefaultTimeout)
				if err != nil {
					return err
				}
				if err := c.DeleteBrowserProfile(cmd.Context(), args[0]); err != nil {
					return err //nolint:wrapcheck // client errors are already user-facing
				}
				if a.format == output.FormatTable {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "deleted "+args[0])
					return err //nolint:wrapcheck // stdout write errors need no context
				}
				return a.printer(cmd).Print(map[string]any{"group": args[0], "deleted": true}, nil) //nolint:wrapcheck // stdout write errors need no context
			},
		},
	)
	return cmd
}

func (a *app) listBrowserProfiles(cmd *cobra.Command) error {
	c, err := a.newClient(cmd, client.DefaultTimeout)
	if err != nil {
		return err
	}
	p, err := c.BrowserProfiles(cmd.Context())
	if err != nil {
		return err //nolint:wrapcheck // client errors are already user-facing
	}
	if p == nil {
		p = []string{}
	}
	return a.printer(cmd).Print(p, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
		t := output.Table{Columns: []string{"group"}}
		for _, g := range p {
			t.Rows = append(t.Rows, []string{g})
		}
		return t
	})
}
