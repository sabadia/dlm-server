package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
)

func cookieTable(cs []client.Cookie) output.Table {
	t := output.Table{Columns: []string{"id", "domain", "path", "name", "secure", "httponly", "expires"}}
	for _, c := range cs {
		expires := "session"
		if c.ExpiresAt != nil {
			expires = c.ExpiresAt.Local().Format(time.DateTime)
		}
		t.Rows = append(t.Rows, []string{
			c.ID, c.Domain, c.Path, c.Name, strconv.FormatBool(c.Secure), strconv.FormatBool(c.HTTPOnly), expires,
		})
	}
	return t
}

func (a *app) newCookiesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cookies",
		Aliases: []string{"cookie"},
		Short:   "Manage the cookie store",
		Long: "Manage the encrypted cookie store shared by the download engines.\n\n" +
			"Cookie values are never listed; `cookies export` writes them out on request.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		a.newCookiesListCommand(),
		a.newCookiesRemoveCommand(),
		a.newCookiesClearCommand(),
		a.newCookiesImportCommand(),
		a.newCookiesExportCommand(),
	)
	return cmd
}

func bindCookieFilter(cmd *cobra.Command, f *client.CookieFilter) {
	cmd.Flags().StringVar(&f.Domain, "domain", "", "only cookies of exactly this domain (no subdomains)")
	cmd.Flags().StringVar(&f.Name, "name", "", "only cookies with this name")
}

func (a *app) newCookiesListCommand() *cobra.Command {
	var f client.CookieFilter
	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List stored cookies (without values)",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "list-cookies"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			cs, err := c.ListCookies(cmd.Context(), f)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if cs == nil {
				cs = []client.Cookie{}
			}
			return a.printer(cmd).Print(cs, func() output.Table { return cookieTable(cs) })
		},
	}
	bindCookieFilter(cmd, &f)
	return cmd
}

func (a *app) newCookiesRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "rm ID",
		Aliases:     []string{"delete"},
		Short:       "Delete a cookie",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "delete-cookie"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			if err := c.DeleteCookie(cmd.Context(), args[0]); err != nil {
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

func (a *app) newCookiesClearCommand() *cobra.Command {
	var domainName string
	cmd := &cobra.Command{
		Use:         "clear --domain DOMAIN",
		Short:       "Delete every cookie of a domain",
		Long:        "Delete every cookie stored for exactly DOMAIN (subdomains are separate domains).",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "delete-cookies"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if domainName == "" {
				return &usageError{err: errors.New("--domain is required")}
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			n, err := c.DeleteCookies(cmd.Context(), domainName)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if a.format == output.FormatTable {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "deleted %d cookies of %s\n", n, domainName)
				return err //nolint:wrapcheck // stdout write errors need no context
			}
			return a.printer(cmd).Print(map[string]any{"domain": domainName, "deleted": n}, nil)
		},
	}
	cmd.Flags().StringVar(&domainName, "domain", "", "domain whose cookies are deleted")
	return cmd
}

func (a *app) newCookiesImportCommand() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "import FILE",
		Short: "Import cookies from a JSON or Netscape cookies.txt file",
		Long: "Import cookies from FILE ('-' reads stdin). The format (JSON array of cookies, such as a browser\n" +
			"extension's export, or Netscape cookies.txt) is detected unless --format is given. A cookie with the\n" +
			"same domain, name, path and partition key is replaced. Rejected cookies are reported on stderr.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{annotationOperationID: "import-cookies"},
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := readInput(args[0], cmd.InOrStdin())
			if err != nil {
				return err
			}
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			res, err := c.ImportCookies(cmd.Context(), format, string(data))
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if a.format != output.FormatTable {
				return a.printer(cmd).Print(res, nil)
			}
			for _, e := range res.Errors {
				writeErr(cmd.ErrOrStderr(), "rejected: "+e)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "imported %d, skipped %d expired, rejected %d\n", res.Imported, res.Skipped, len(res.Errors))
			return err //nolint:wrapcheck // stdout write errors need no context
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "file format: json, netscape (default: detect)")
	_ = cmd.RegisterFlagCompletionFunc("format", cookieFormatCompletion) // only fails if the flag does not exist
	return cmd
}

func (a *app) newCookiesExportCommand() *cobra.Command {
	var (
		f      client.CookieFilter
		format string
		file   string
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export cookies (with their values) as JSON or Netscape cookies.txt",
		Long: "Write the unexpired cookies, values included, to stdout or --file (mode 0600). The default --output\n" +
			"table writes the bare file content; --output json|yaml prints the daemon's response instead.\n" +
			"The output contains secrets: keep it private.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "export-cookies"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			exp, err := c.ExportCookies(cmd.Context(), format, f)
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			if file != "" {
				if err := os.WriteFile(file, []byte(exp.Data), 0o600); err != nil {
					return fmt.Errorf("cli: %w", err)
				}
				if a.format == output.FormatTable {
					_, err := fmt.Fprintf(cmd.OutOrStdout(), "exported %d cookies to %s\n", exp.Count, file)
					return err //nolint:wrapcheck // stdout write errors need no context
				}
				return a.printer(cmd).Print(map[string]any{"format": exp.Format, "count": exp.Count, "file": file}, nil)
			}
			if a.format != output.FormatTable {
				return a.printer(cmd).Print(exp, nil)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), exp.Data)
			return err //nolint:wrapcheck // stdout write errors need no context
		},
	}
	bindCookieFilter(cmd, &f)
	cmd.Flags().StringVar(&format, "format", "json", "file format: json, netscape")
	cmd.Flags().StringVarP(&file, "file", "f", "", "write to this file (mode 0600) instead of stdout")
	_ = cmd.RegisterFlagCompletionFunc("format", cookieFormatCompletion) // only fails if the flag does not exist
	return cmd
}

func cookieFormatCompletion(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return []string{"json", "netscape"}, cobra.ShellCompDirectiveNoFileComp
}
