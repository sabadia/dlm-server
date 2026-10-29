package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabadia/dlm-server/internal/auth"
	"github.com/sabadia/dlm-server/internal/cli/output"
	"github.com/sabadia/dlm-server/internal/client"
	"github.com/sabadia/dlm-server/internal/config"
	"github.com/sabadia/dlm-server/internal/instance"
	"github.com/sabadia/dlm-server/internal/nativehost"
	"github.com/sabadia/dlm-server/internal/platform"
)

func (a *app) newNativeHostCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "native-host [ORIGIN]",
		Short: "Native Messaging host for the browser extension",
		Long: "Run the Native Messaging host. Browsers start it (through the launcher that `native-host install` writes) " +
			"when the extension calls runtime.connectNative; it answers `connect` with where the daemon is, and `pair` / " +
			"`pair.status` so the extension can be given a token of its own, over stdin/stdout. It is not meant to be run by hand.\n\n" +
			"Without a subcommand it serves the protocol until stdin closes. Browser-supplied arguments (the caller's " +
			"origin, --parent-window) are accepted.",
		Args:               cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			baseURL, err := a.baseURL(cfg)
			if err != nil {
				return err
			}
			origin, addon := "", ""
			for _, arg := range args {
				switch {
				case strings.HasPrefix(arg, "chrome-extension://"):
					origin = arg
				case strings.Contains(arg, "@") && !strings.ContainsAny(arg, `/\`):
					addon = arg // Firefox passes the manifest path and then the add-on id
				}
			}
			h := &nativehost.Host{
				Daemon:         func(ctx context.Context) nativehost.DaemonInfo { return a.hostDaemonInfo(ctx, cfg, baseURL) },
				Pairing:        hostPairing{newClient: func() (*client.Client, error) { return a.hostClient(cfg, baseURL) }},
				Origin:         origin,
				AddonID:        addon,
				AllowedOrigins: cfg.Server.AllowedOrigins,
			}
			return h.Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout()) //nolint:wrapcheck // protocol errors are already prefixed
		},
	}
	cmd.AddCommand(a.newNativeHostInstallCommand(), a.newNativeHostUninstallCommand(), a.newNativeHostStatusCommand())
	return cmd
}

// baseURL is --server, or derived from the configuration.
func (a *app) baseURL(cfg *config.Config) (string, error) {
	if a.serverURL != "" {
		return a.serverURL, nil
	}
	u, err := client.BaseURL(cfg)
	if err != nil {
		return "", fmt.Errorf("cli: %w", err)
	}
	return u, nil
}

// hostDaemonInfo says where the daemon is right now. A running daemon of this
// configuration directory publishes its real address (it may have started on
// a random port, so what was baked into the launcher can be stale); otherwise
// the configured address is tried, which also covers a daemon on another
// machine.
func (a *app) hostDaemonInfo(ctx context.Context, cfg *config.Config, baseURL string) nativehost.DaemonInfo {
	if info, err := instance.Discover(cfg.Paths.ConfigDir); err == nil && info.URL != "" {
		return nativehost.DaemonInfo{BaseURL: info.URL, Running: true, Version: info.Version, APIVersion: info.APIVersion, TLSFingerprint: info.TLSFingerprint}
	}
	out := nativehost.DaemonInfo{BaseURL: baseURL}
	if v, err := a.daemonVersion(ctx, baseURL); err == nil {
		out.Running, out.Version = true, v
	}
	return out
}

// hostClient is a client for the daemon of this host with the primary token,
// read from the token file on every call so a rotated token is picked up.
func (a *app) hostClient(cfg *config.Config, baseURL string) (*client.Client, error) {
	if info, err := instance.Discover(cfg.Paths.ConfigDir); err == nil && info.URL != "" {
		baseURL = info.URL
	}
	token, err := auth.ReadToken(cfg.TokenFile())
	if err != nil || token == "" {
		return nil, errors.New("the daemon has no token yet: run `dlm serve` once")
	}
	c, err := client.New(baseURL, token, client.WithUserAgent("dlm-native-host/"+a.version), client.WithTimeout(10*time.Second))
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return c, nil
}

// hostPairing adapts the API client to what the host needs.
type hostPairing struct {
	newClient func() (*client.Client, error)
}

func pairingInfo(p client.Pairing, token string) nativehost.PairingInfo {
	return nativehost.PairingInfo{ID: p.ID, Status: p.Status, Code: p.Code, ExpiresAt: p.ExpiresAt, Token: token}
}

func (h hostPairing) Start(ctx context.Context, req nativehost.PairingRequest) (nativehost.PairingInfo, error) {
	c, err := h.newClient()
	if err != nil {
		return nativehost.PairingInfo{}, err
	}
	p, err := c.StartPairing(ctx, client.PairingRequest{Name: req.Name, Browser: req.Browser, ClientID: req.ClientID, Version: req.Version})
	if err != nil {
		return nativehost.PairingInfo{}, err //nolint:wrapcheck // client errors are already user-facing
	}
	return pairingInfo(*p, ""), nil
}

func (h hostPairing) Status(ctx context.Context, id string) (nativehost.PairingInfo, error) {
	c, err := h.newClient()
	if err != nil {
		return nativehost.PairingInfo{}, err
	}
	st, err := c.GetPairing(ctx, id)
	if err != nil {
		return nativehost.PairingInfo{}, err //nolint:wrapcheck // client errors are already user-facing
	}
	return pairingInfo(st.Pairing, st.Token), nil
}

// daemonVersion asks the daemon's public health endpoint for its version.
func (a *app) daemonVersion(ctx context.Context, baseURL string) (string, error) {
	c, err := client.New(baseURL, "", client.WithUserAgent("dlm-native-host/"+a.version), client.WithTimeout(2*time.Second))
	if err != nil {
		return "", fmt.Errorf("cli: %w", err)
	}
	h, err := c.Health(ctx)
	if err != nil {
		return "", err //nolint:wrapcheck // only used to tell whether the daemon runs
	}
	return h.Version, nil
}

type nativeHostFlags struct {
	extensionIDs []string
	firefoxIDs   []string
	browsers     []string
}

func (f *nativeHostFlags) bind(cmd *cobra.Command, withIDs bool) {
	fl := cmd.Flags()
	if withIDs {
		fl.StringArrayVar(&f.extensionIDs, "extension-id", nil,
			"Chromium extension ID or chrome-extension://ID/ origin to allow (repeatable; default: the chrome-extension:// entries of server.allowed_origins)")
		fl.StringArrayVar(&f.firefoxIDs, "firefox-id", nil, "Firefox add-on ID to allow, e.g. dlm@example.org (repeatable)")
	}
	fl.StringArrayVar(&f.browsers, "browser", nil, "limit to these browsers: "+browserNames()+" (repeatable; default: all)")
	_ = cmd.RegisterFlagCompletionFunc("browser", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return strings.Split(browserNames(), ", "), cobra.ShellCompDirectiveNoFileComp
	}) // only fails if the flag does not exist
}

func browserNames() string {
	var names []string
	for _, b := range platform.Browsers() {
		names = append(names, string(b))
	}
	return strings.Join(names, ", ")
}

func (f *nativeHostFlags) parsedBrowsers() ([]platform.Browser, error) {
	var out []platform.Browser
	for _, name := range f.browsers {
		b := platform.Browser(strings.ToLower(name))
		if !b.Valid() {
			return nil, &usageError{err: fmt.Errorf("unknown browser %q (want %s)", name, browserNames())}
		}
		if !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (a *app) nativeHostInstaller(cmd *cobra.Command) (*nativehost.Installer, *config.Config, error) {
	cfg, err := a.loadConfig(cmd)
	if err != nil {
		return nil, nil, err
	}
	baseURL, err := a.baseURL(cfg)
	if err != nil {
		return nil, nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("cli: locate the dlm executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return &nativehost.Installer{
		Executable: exe,
		ConfigDir:  cfg.Paths.ConfigDir,
		ServerURL:  baseURL,
		DataDir:    cfg.Paths.ConfigDir,
	}, cfg, nil
}

func (a *app) newNativeHostInstallCommand() *cobra.Command {
	var f nativeHostFlags
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Register the Native Messaging host with the browsers",
		Long: "Write the host launcher and register it, for the current user, with Chrome, Chromium, Edge, Brave and " +
			"Firefox (manifest files on Linux and macOS, registry keys on Windows).\n\n" +
			"The published DLM extension (Chrome and every Chromium browser, and Firefox) is always registered; " +
			"--extension-id and --firefox-id (or the chrome-extension:// entries of server.allowed_origins) add others, " +
			"for example a build you made yourself. Another extension also has to be in server.allowed_origins to be answered.",
		Example: "  dlm native-host install\n" +
			"  dlm native-host install --extension-id abcdefghijklmnopabcdefghijklmnop --browser chrome",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "update-native-host"}, // the same installer as the API, run here
		RunE: func(cmd *cobra.Command, _ []string) error {
			browsers, err := f.parsedBrowsers()
			if err != nil {
				return err
			}
			in, cfg, err := a.nativeHostInstaller(cmd)
			if err != nil {
				return err
			}
			t := nativehost.Target{Browsers: browsers, FirefoxIDs: f.firefoxIDs}
			ids := f.extensionIDs
			if len(ids) == 0 {
				ids = chromiumOrigins(cfg.Server.AllowedOrigins)
			}
			for _, id := range ids {
				origin, err := nativehost.ChromiumOrigin(id)
				if err != nil {
					return &usageError{err: err}
				}
				if !slices.Contains(t.ChromiumOrigins, origin) {
					t.ChromiumOrigins = append(t.ChromiumOrigins, origin)
				}
			}
			results, err := in.Install(t.WithBuiltinIDs())
			if perr := a.printNativeHost(cmd, results); perr != nil {
				return perr
			}
			if err != nil {
				return fmt.Errorf("cli: %w", err)
			}
			if a.format == output.FormatTable {
				for _, o := range t.ChromiumOrigins {
					if !slices.Contains(cfg.Server.AllowedOrigins, o) && !slices.Contains(cfg.Server.AllowedOrigins, strings.TrimSuffix(o, "/")) {
						writeErr(cmd.ErrOrStderr(), "note: "+o+" is not in server.allowed_origins; the daemon will refuse that extension until you add it")
					}
				}
			}
			return nil
		},
	}
	f.bind(cmd, true)
	return cmd
}

func (a *app) newNativeHostUninstallCommand() *cobra.Command {
	var f nativeHostFlags
	cmd := &cobra.Command{
		Use:         "uninstall",
		Short:       "Remove the Native Messaging host registrations",
		Long:        "Remove the host registration from the selected browsers (default: all) and, when all were selected, the launcher.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "update-native-host"}, // the same installer as the API, run here
		RunE: func(cmd *cobra.Command, _ []string) error {
			browsers, err := f.parsedBrowsers()
			if err != nil {
				return err
			}
			in, _, err := a.nativeHostInstaller(cmd)
			if err != nil {
				return err
			}
			results, err := in.Uninstall(browsers)
			if perr := a.printNativeHost(cmd, results); perr != nil {
				return perr
			}
			if err != nil {
				return fmt.Errorf("cli: %w", err)
			}
			return nil
		},
	}
	f.bind(cmd, false)
	return cmd
}

func (a *app) printNativeHost(cmd *cobra.Command, results []nativehost.Result) error {
	if results == nil {
		results = []nativehost.Result{}
	}
	return a.printer(cmd).Print(results, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
		t := output.Table{Columns: []string{"browser", "status", "manifest", "registry", "reason"}}
		for _, r := range results {
			t.Rows = append(t.Rows, []string{string(r.Browser), r.Status, r.Manifest, r.Registry, r.Reason})
		}
		return t
	})
}

// chromiumOrigins picks the chrome-extension:// entries of a list of origins.
func chromiumOrigins(origins []string) []string {
	var out []string
	for _, o := range origins {
		if strings.HasPrefix(o, "chrome-extension://") {
			out = append(out, o)
		}
	}
	return out
}

func (a *app) newNativeHostStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "status",
		Short:       "Show where the Native Messaging host is registered",
		Long:        "Ask the daemon (it must run on this machine) whether the host is registered with each browser, for the user the daemon runs as.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annotationOperationID: "get-native-host"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.newClient(cmd, client.DefaultTimeout)
			if err != nil {
				return err
			}
			st, err := c.NativeHostStatus(cmd.Context())
			if err != nil {
				return err //nolint:wrapcheck // client errors are already user-facing
			}
			return a.printer(cmd).Print(st, func() output.Table { //nolint:wrapcheck // stdout write errors need no context
				t := output.Table{Columns: []string{"browser", "status", "manifest", "registry"}}
				for _, r := range st.Browsers {
					t.Rows = append(t.Rows, []string{r.Browser, r.Status, r.Manifest, r.Registry})
				}
				return t
			})
		},
	}
}
