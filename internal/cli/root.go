package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
	"github.com/sabadia/dlm-server/internal/config"
)

// Exit codes returned by Execute.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// annotationOperationID is the cobra annotation naming the API operation(s)
// a command calls (comma-separated). See the package documentation.
const annotationOperationID = "operationId"

// Option customises the root command; used by tests.
type Option func(*app)

// WithIO sets the standard input/output streams (default os.Stdout/os.Stderr).
func WithIO(stdout, stderr io.Writer) Option {
	return func(a *app) { a.stdout, a.stderr = stdout, stderr }
}

// WithEnviron sets the environment used for configuration (default
// os.Environ).
func WithEnviron(environ func() []string) Option {
	return func(a *app) { a.environ = environ }
}

// app holds the state shared by the commands of one invocation.
type app struct {
	version string
	stdout  io.Writer
	stderr  io.Writer
	environ func() []string

	// Global flag values.
	outputFlag string
	serverURL  string
	token      string

	format output.Format
	// started is set once argument and flag validation passed and a command
	// begins to run; errors before that are usage errors.
	started bool

	root *cobra.Command
}

// usageError marks errors caused by invalid invocation (exit code 2).
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// Execute runs the CLI with os.Args and returns the process exit code.
func Execute(ctx context.Context, version string) int {
	return run(ctx, version, os.Args[1:])
}

// NewRootCommand builds the dlm command tree.
func NewRootCommand(version string, opts ...Option) *cobra.Command {
	return newApp(version, opts...).root
}

func run(ctx context.Context, version string, args []string, opts ...Option) int {
	a := newApp(version, opts...) //nolint:contextcheck // commands receive ctx via cobra (cmd.Context()), set by ExecuteContextC
	a.root.SetArgs(args)
	return a.execute(ctx)
}

func newApp(version string, opts ...Option) *app {
	a := &app{
		version: version,
		stdout:  os.Stdout,
		stderr:  os.Stderr,
		environ: os.Environ,
	}
	for _, opt := range opts {
		opt(a)
	}

	root := &cobra.Command{
		Use:   config.AppName,
		Short: "dlm download manager: daemon and command-line client",
		Long: "dlm is a cross-platform download manager.\n\n" +
			"Run `dlm serve` to start the daemon; every other command talks to it over its REST API.",
		Version:           version,
		SilenceErrors:     true,
		SilenceUsage:      true,
		PersistentPreRunE: a.preRun,
	}
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err: err} })

	pf := root.PersistentFlags()
	config.BindFlags(pf)
	pf.StringVarP(&a.outputFlag, "output", "o", string(output.FormatTable), "output format: "+strings.Join(output.Formats(), ", "))
	pf.StringVar(&a.serverURL, "server", "", "API base URL (default: derived from --address)")
	pf.StringVar(&a.token, "token", "", "API bearer token (default: read from the daemon's token file)")
	_ = root.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return output.Formats(), cobra.ShellCompDirectiveNoFileComp
	}) // only fails if the flag does not exist

	root.AddCommand(
		a.newServeCommand(),
		a.newHealthCommand(),
		a.newVersionCommand(),
		a.newTokenCommand(),
	)
	a.root = root
	return a
}

func (a *app) preRun(*cobra.Command, []string) error {
	f, err := output.ParseFormat(a.outputFlag)
	if err != nil {
		return &usageError{err: err}
	}
	a.format = f
	a.started = true
	return nil
}

func (a *app) execute(ctx context.Context) int {
	cmd, err := a.root.ExecuteContextC(ctx)
	if err == nil {
		return ExitOK
	}
	var uerr *usageError
	usage := !a.started || errors.As(err, &uerr)
	writeErr(a.stderr, "error: "+err.Error())
	if usage {
		writeErr(a.stderr, fmt.Sprintf("Run '%s --help' for usage.", cmd.CommandPath()))
		return ExitUsage
	}
	return ExitError
}

func writeErr(w io.Writer, line string) {
	// Nothing sensible can be done if stderr is unwritable.
	_, _ = fmt.Fprintln(w, line)
}

// printer returns the printer for the selected --output format.
func (a *app) printer(cmd *cobra.Command) *output.Printer {
	return output.New(cmd.OutOrStdout(), a.format)
}

// loadConfig loads the boot configuration using the parsed flags of cmd.
func (a *app) loadConfig(cmd *cobra.Command) (*config.Config, error) {
	cfg, err := config.Load(config.LoadOptions{Flags: cmd.Flags(), Environ: a.environ})
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return cfg, nil
}

// newClient builds an API client. The base URL comes from --server or the
// configured address; the token from --token or the daemon's token file. A
// missing token file is not an error (public operations still work): the
// client never creates the token, only `dlm serve` does.
func (a *app) newClient(cmd *cobra.Command, timeout time.Duration) (*client.Client, error) {
	baseURL, token := a.serverURL, a.token
	if baseURL == "" || !cmd.Flags().Changed("token") {
		cfg, err := a.loadConfig(cmd)
		if err != nil {
			return nil, err
		}
		if baseURL == "" {
			if baseURL, err = client.BaseURL(cfg); err != nil {
				return nil, fmt.Errorf("cli: %w", err)
			}
		}
		if !cmd.Flags().Changed("token") {
			token, err = auth.ReadToken(cfg.TokenFile())
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("cli: %w", err)
			}
		}
	}
	c, err := client.New(baseURL, token,
		client.WithUserAgent("dlm-cli/"+a.version),
		client.WithTimeout(timeout),
	)
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return c, nil
}
