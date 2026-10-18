package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func categoryTable(cs []*client.Category) output.Table {
	t := output.Table{Columns: []string{"id", "name", "position", "dir", "extensions", "mime types"}}
	for _, c := range cs {
		t.Rows = append(t.Rows, []string{c.ID, c.Name, strconv.FormatInt(c.Position, 10), c.Dir, strings.Join(c.Extensions, ","), strings.Join(c.MIMETypes, ",")})
	}
	return t
}

func (a *app) printCategory(cmd *cobra.Command, c *client.Category) error {
	return a.printer(cmd).Print(c, func() output.Table { return categoryTable([]*client.Category{c}) })
}

func (a *app) newCategoryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "category",
		Short: "Manage the categories that sort downloads into directories",
		Long: "A category sends downloads to a directory by file extension or MIME type; the first matching category " +
			"wins. They are used when the autoCategorize setting is on (dlm settings set autoCategorize=true) and the " +
			"task has no directory of its own.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.newCategoryListCommand(), a.newCategoryAddCommand(), a.newCategoryGetCommand(),
		a.newCategorySetCommand(), a.newCategoryRemoveCommand(), a.newCategoryMatchCommand())
	return cmd
}

func (a *app) newCategoryListCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List categories in the order they are tried",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "list-categories"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			cs, err := c.ListCategories(cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if cs == nil {
				cs = []*client.Category{}
			}
			return a.printer(cmd).Print(cs, func() output.Table { return categoryTable(cs) })
		},
	}
}

func (a *app) newCategoryAddCommand() *cobra.Command {
	var dir string
	var exts, mimes []string
	cmd := &cobra.Command{
		Use:         "add NAME",
		Short:       "Create a category",
		Example:     "  dlm category add Videos --dir Videos --ext mp4 --ext mkv --mime 'video/*'",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "create-category"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			cat, err := c.CreateCategory(cmd.Context(), client.CreateCategoryRequest{Name: args[0], Dir: dir, Extensions: exts, MIMETypes: mimes})
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printCategory(cmd, cat)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&dir, "dir", "", "directory of the downloads: absolute, or relative to the download directory (required)")
	fl.StringArrayVar(&exts, "ext", nil, "file extension without the dot (repeatable)")
	fl.StringArrayVar(&mimes, "mime", nil, "MIME type or family such as video/* (repeatable)")
	_ = cmd.MarkFlagRequired("dir") // a flag that was just defined
	return cmd
}

func (a *app) newCategoryGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "get ID",
		Short:       "Show a category",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "get-category"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			cat, err := c.GetCategory(cmd.Context(), args[0])
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printCategory(cmd, cat)
		},
	}
}

func (a *app) newCategorySetCommand() *cobra.Command {
	var (
		name, dir   string
		exts, mimes []string
		position    int64
	)
	cmd := &cobra.Command{
		Use:         "set ID",
		Short:       "Change a category",
		Long:        "Change the given fields of a category. --ext and --mime replace the whole list.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "update-category"},
		RunE: func(cmd *cobra.Command, args []string) error {
			fl := cmd.Flags()
			var req client.UpdateCategoryRequest
			if fl.Changed("name") {
				req.Name = &name
			}
			if fl.Changed("dir") {
				req.Dir = &dir
			}
			if fl.Changed("ext") {
				req.Extensions = &exts
			}
			if fl.Changed("mime") {
				req.MIMETypes = &mimes
			}
			if fl.Changed("position") {
				req.Position = &position
			}
			if req == (client.UpdateCategoryRequest{}) {
				return &usageError{err: fmt.Errorf("nothing to change: give --name, --dir, --ext, --mime or --position")}
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			cat, err := c.UpdateCategory(cmd.Context(), args[0], req)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printCategory(cmd, cat)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&name, "name", "", "new name")
	fl.StringVar(&dir, "dir", "", "new directory")
	fl.StringArrayVar(&exts, "ext", nil, "new extension list (repeatable; replaces the old list)")
	fl.StringArrayVar(&mimes, "mime", nil, "new MIME type list (repeatable; replaces the old list)")
	fl.Int64Var(&position, "position", 0, "new place in the order (0 is tried first)")
	return cmd
}

func (a *app) newCategoryRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "rm ID",
		Aliases:     []string{"delete"},
		Short:       "Delete a category (files already sorted by it stay where they are)",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "delete-category"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			if err := c.DeleteCategory(cmd.Context(), args[0]); err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if a.format == output.FormatTable {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "deleted "+args[0])
				return err //nolint:wrapcheck // stdout write errors need no context
			}
			return a.printer(cmd).Print(map[string]any{"id": args[0], "deleted": true}, nil)
		},
	}
}

func (a *app) newCategoryMatchCommand() *cobra.Command {
	var name, mime string
	cmd := &cobra.Command{
		Use:         "match",
		Short:       "Show the category a file would be sorted into",
		Example:     "  dlm category match --name movie.mkv",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "match-category"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" && mime == "" {
				return &usageError{err: fmt.Errorf("give --name and/or --mime")}
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			cat, err := c.MatchCategory(cmd.Context(), name, mime)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if cat == nil {
				if a.format == output.FormatTable {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "no category matches")
					return err //nolint:wrapcheck // stdout write errors need no context
				}
				return a.printer(cmd).Print(map[string]any{"category": nil}, nil)
			}
			return a.printCategory(cmd, cat)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "file name")
	cmd.Flags().StringVar(&mime, "mime", "", "MIME type")
	return cmd
}
