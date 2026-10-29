package platform

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// AutostartName names the login item on every system: the launchd label, the
// systemd unit and the Windows Run value.
const (
	AutostartLabel = "io.github.sabadia.dlm"
	autostartUnit  = "dlm.service"
	autostartValue = "dlm"
)

// AutostartSpec is the command line that starts the daemon at login.
type AutostartSpec struct {
	// Executable is the absolute path of the dlm binary.
	Executable string
	// Args follow it, normally ["serve"].
	Args []string
}

// AutostartStatus describes the login item.
type AutostartStatus struct {
	// Kind is launchd, systemd or registry.
	Kind string `json:"kind"`
	// Installed is true when the login item exists.
	Installed bool `json:"installed"`
	// Active is true when the system also has it loaded or enabled.
	Active bool `json:"active"`
	// Path is the plist or unit file, or the registry key on Windows.
	Path string `json:"path,omitempty"`
}

// Autostart installs the daemon as a login item of the current user: a
// launchd agent on macOS, a systemd user unit on Linux and a Run registry
// value on Windows. The fields below default to the running system; tests set
// them.
type Autostart struct {
	GOOS          string
	Home          string
	UserConfigDir string
	// UID is the user id launchd needs (default os.Getuid).
	UID int
	// Run runs a system command (launchctl, systemctl); default exec.
	Run func(ctx context.Context, c Command) error
	// Registry stores the Windows Run value; default the real registry.
	Registry AutostartRegistry
}

// AutostartRegistry is the Windows Run key.
type AutostartRegistry interface {
	Set(name, command string) error
	Delete(name string) error
	Get(name string) (command string, ok bool, err error)
}

func (a *Autostart) goos() string {
	if a.GOOS != "" {
		return a.GOOS
	}
	return runtime.GOOS
}

func (a *Autostart) run(ctx context.Context, c Command) error {
	if a.Run != nil {
		return a.Run(ctx, c)
	}
	return run(ctx, c.Name, c)
}

func (a *Autostart) home() (string, error) {
	if a.Home != "" {
		return a.Home, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("platform: home directory: %w", err)
	}
	return h, nil
}

func (a *Autostart) uid() int {
	if a.UID != 0 {
		return a.UID
	}
	return os.Getuid()
}

func (a *Autostart) registry() AutostartRegistry {
	if a.Registry != nil {
		return a.Registry
	}
	return systemAutostartRegistry{}
}

// plistPath is the launchd agent file.
func (a *Autostart) plistPath() (string, error) {
	h, err := a.home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "Library", "LaunchAgents", AutostartLabel+".plist"), nil
}

// unitPath is the systemd user unit file.
func (a *Autostart) unitPath() (string, error) {
	dir := a.UserConfigDir
	if dir == "" {
		h, err := a.home()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(h, ".config")
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && a.Home == "" {
			dir = x
		}
	}
	return filepath.Join(dir, "systemd", "user", autostartUnit), nil
}

// ErrAutostartUnsupported is returned on a system without a known login
// mechanism.
var ErrAutostartUnsupported = errors.New("platform: starting at login is not supported on this operating system")

// Install makes the daemon start at login and, where the system allows it,
// starts it now.
func (a *Autostart) Install(ctx context.Context, spec AutostartSpec) (AutostartStatus, error) {
	if !absoluteFor(a.goos(), spec.Executable) {
		return AutostartStatus{}, fmt.Errorf("platform: executable %q must be an absolute path", spec.Executable)
	}
	switch a.goos() {
	case "darwin":
		path, err := a.plistPath()
		if err != nil {
			return AutostartStatus{}, err
		}
		if err := writeLoginFile(path, LaunchdPlist(spec)); err != nil {
			return AutostartStatus{}, err
		}
		domain := "gui/" + strconv.Itoa(a.uid())
		_ = a.run(ctx, Command{Name: "launchctl", Args: []string{"bootout", domain + "/" + AutostartLabel}}) // not loaded yet is fine
		if err := a.run(ctx, Command{Name: "launchctl", Args: []string{"bootstrap", domain, path}}); err != nil {
			return AutostartStatus{Kind: "launchd", Installed: true, Path: path}, err
		}
		return AutostartStatus{Kind: "launchd", Installed: true, Active: true, Path: path}, nil
	case "linux":
		path, err := a.unitPath()
		if err != nil {
			return AutostartStatus{}, err
		}
		if err := writeLoginFile(path, SystemdUnit(spec)); err != nil {
			return AutostartStatus{}, err
		}
		for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "--now", autostartUnit}} {
			if err := a.run(ctx, Command{Name: "systemctl", Args: args}); err != nil {
				return AutostartStatus{Kind: "systemd", Installed: true, Path: path}, err
			}
		}
		return AutostartStatus{Kind: "systemd", Installed: true, Active: true, Path: path}, nil
	case "windows":
		if err := a.registry().Set(autostartValue, WindowsRunCommand(spec)); err != nil {
			return AutostartStatus{}, err
		}
		return AutostartStatus{Kind: "registry", Installed: true, Active: true, Path: windowsRunKey + `\` + autostartValue}, nil
	default:
		return AutostartStatus{}, ErrAutostartUnsupported
	}
}

// Uninstall stops the daemon from starting at login and removes the login
// item; nothing installed is not an error.
func (a *Autostart) Uninstall(ctx context.Context) error {
	switch a.goos() {
	case "darwin":
		path, err := a.plistPath()
		if err != nil {
			return err
		}
		_ = a.run(ctx, Command{Name: "launchctl", Args: []string{"bootout", "gui/" + strconv.Itoa(a.uid()) + "/" + AutostartLabel}})
		return removeIfExists(path)
	case "linux":
		path, err := a.unitPath()
		if err != nil {
			return err
		}
		if _, statErr := os.Stat(path); statErr == nil {
			_ = a.run(ctx, Command{Name: "systemctl", Args: []string{"--user", "disable", "--now", autostartUnit}})
		}
		if err := removeIfExists(path); err != nil {
			return err
		}
		_ = a.run(ctx, Command{Name: "systemctl", Args: []string{"--user", "daemon-reload"}})
		return nil
	case "windows":
		return a.registry().Delete(autostartValue)
	default:
		return ErrAutostartUnsupported
	}
}

// Status reports whether the login item exists and whether the system has it
// active.
func (a *Autostart) Status(ctx context.Context) (AutostartStatus, error) {
	switch a.goos() {
	case "darwin":
		path, err := a.plistPath()
		if err != nil {
			return AutostartStatus{}, err
		}
		st := AutostartStatus{Kind: "launchd", Path: path, Installed: fileExists(path)}
		if st.Installed {
			st.Active = a.run(ctx, Command{Name: "launchctl", Args: []string{"print", "gui/" + strconv.Itoa(a.uid()) + "/" + AutostartLabel}}) == nil
		}
		return st, nil
	case "linux":
		path, err := a.unitPath()
		if err != nil {
			return AutostartStatus{}, err
		}
		st := AutostartStatus{Kind: "systemd", Path: path, Installed: fileExists(path)}
		if st.Installed {
			st.Active = a.run(ctx, Command{Name: "systemctl", Args: []string{"--user", "is-enabled", "--quiet", autostartUnit}}) == nil
		}
		return st, nil
	case "windows":
		_, ok, err := a.registry().Get(autostartValue)
		if err != nil {
			return AutostartStatus{}, err
		}
		return AutostartStatus{Kind: "registry", Installed: ok, Active: ok, Path: windowsRunKey + `\` + autostartValue}, nil
	default:
		return AutostartStatus{}, ErrAutostartUnsupported
	}
}

// windowsRunKey is the per-user Run key (under HKEY_CURRENT_USER).
const windowsRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// LaunchdPlist is the launchd agent that starts the daemon at login.
func LaunchdPlist(spec AutostartSpec) string {
	var args strings.Builder
	for _, s := range append([]string{spec.Executable}, spec.Args...) {
		args.WriteString("\t\t<string>" + xmlEscape(s) + "</string>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + AutostartLabel + `</string>
	<key>ProgramArguments</key>
	<array>
` + args.String() + `	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
</dict>
</plist>
`
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s)) // a strings.Builder never fails
	return b.String()
}

// SystemdUnit is the systemd user unit that runs the daemon.
func SystemdUnit(spec AutostartSpec) string {
	parts := make([]string, 0, 1+len(spec.Args))
	for _, s := range append([]string{spec.Executable}, spec.Args...) {
		parts = append(parts, systemdQuote(s))
	}
	return `[Unit]
Description=dlm download manager

[Service]
ExecStart=` + strings.Join(parts, " ") + `
Restart=on-failure

[Install]
WantedBy=default.target
`
}

// systemdQuote quotes an ExecStart word when it needs it: spaces, quotes,
// backslashes and the % and $ that systemd expands.
func systemdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\%$;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}

// WindowsRunCommand is the command line stored in the Run key.
func WindowsRunCommand(spec AutostartSpec) string {
	parts := make([]string, 0, 1+len(spec.Args))
	for _, s := range append([]string{spec.Executable}, spec.Args...) {
		if s == "" || strings.ContainsAny(s, " \t\"") {
			s = `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// absoluteFor reports whether path is absolute on goos (the check cannot use
// filepath when the target system is not the running one).
func absoluteFor(goos, path string) bool {
	if goos == "windows" {
		return len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') || strings.HasPrefix(path, `\\`)
	}
	return strings.HasPrefix(path, "/")
}

func writeLoginFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // user-level login item directories are normally 0755
		return fmt.Errorf("platform: create %q: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // a login item is not secret
		return fmt.Errorf("platform: write %q: %w", path, err)
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("platform: remove %q: %w", path, err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
