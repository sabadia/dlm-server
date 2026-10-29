package nativehost

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sabadia/dlm-server/internal/platform"
)

// Registry sets the Windows registry keys through which browsers find the
// host manifest. The default implementation is the real registry.
type Registry interface {
	Set(key, manifestPath string) error
	Delete(key string) error
}

type systemRegistry struct{}

func (systemRegistry) Set(key, path string) error { return platform.SetNativeHostRegistry(key, path) }
func (systemRegistry) Delete(key string) error    { return platform.DeleteNativeHostRegistry(key) }

// Installer writes and removes the per-user native messaging registrations.
type Installer struct {
	// Executable is the absolute path of the dlm binary the launcher runs.
	Executable string
	// ConfigDir and ServerURL are baked into the launcher as --config-dir and
	// --server: browsers start hosts with a bare environment, so the host
	// cannot rely on the user's flags or variables to find the token.
	ConfigDir string
	ServerURL string
	// DataDir is where the launcher is stored (and, on Windows, the
	// manifests).
	DataDir string

	// The fields below default to the running system; tests set them.
	GOOS          string
	Home          string
	UserConfigDir string
	Registry      Registry
}

// Target selects what Install registers.
type Target struct {
	// Browsers defaults to every supported browser.
	Browsers []platform.Browser
	// ChromiumOrigins are "chrome-extension://ID/" values (see
	// ChromiumOrigin) for Chromium-family browsers; FirefoxIDs are add-on
	// IDs for Firefox. A browser without any is skipped.
	ChromiumOrigins []string
	FirefoxIDs      []string
}

// Result is the outcome for one browser.
type Result struct {
	Browser platform.Browser `json:"browser"`
	// Manifest is the manifest file path.
	Manifest string `json:"manifest,omitempty"`
	// Registry is the Windows registry key (HKEY_CURRENT_USER).
	Registry string `json:"registry,omitempty"`
	// Status is "installed", "removed", "absent" (uninstall: nothing to
	// remove) or "skipped".
	Status string `json:"status"`
	// Reason explains a skip.
	Reason string `json:"reason,omitempty"`
}

// Result statuses.
const (
	StatusInstalled = "installed"
	StatusRemoved   = "removed"
	StatusAbsent    = "absent"
	StatusSkipped   = "skipped"
)

func (in *Installer) goos() string {
	if in.GOOS != "" {
		return in.GOOS
	}
	return runtime.GOOS
}

func (in *Installer) registry() Registry {
	if in.Registry != nil {
		return in.Registry
	}
	return systemRegistry{}
}

// LauncherPath is the file the manifests point to.
func (in *Installer) LauncherPath() string {
	name := "dlm-native-host"
	if in.goos() == "windows" {
		name += ".bat"
	}
	return filepath.Join(in.DataDir, "native-messaging", name)
}

func (in *Installer) registration(b platform.Browser) (platform.NativeHostRegistration, error) {
	if in.Home == "" && in.UserConfigDir == "" {
		return platform.NativeHostRegistrationFor(b, HostName, in.DataDir) //nolint:wrapcheck // already prefixed
	}
	return platform.NativeHostRegistrationForDirs(in.goos(), b, HostName, in.Home, in.UserConfigDir, in.DataDir) //nolint:wrapcheck // already prefixed
}

// Install writes the launcher and registers the host with each selected
// browser. A browser without extension IDs is skipped, not an error; it is an
// error when nothing could be installed.
func (in *Installer) Install(t Target) ([]Result, error) {
	if in.Executable == "" || !filepath.IsAbs(in.Executable) {
		return nil, fmt.Errorf("nativehost: executable %q must be an absolute path", in.Executable)
	}
	browsers := t.Browsers
	if len(browsers) == 0 {
		browsers = platform.Browsers()
	}
	launcher := in.LauncherPath()
	wrote := false
	var results []Result
	for _, b := range browsers {
		if !b.Valid() {
			return nil, fmt.Errorf("nativehost: unknown browser %q", b)
		}
		var m Manifest
		switch {
		case b.Firefox() && len(t.FirefoxIDs) == 0:
			results = append(results, Result{Browser: b, Status: StatusSkipped, Reason: "no Firefox add-on ID given"})
			continue
		case !b.Firefox() && len(t.ChromiumOrigins) == 0:
			results = append(results, Result{Browser: b, Status: StatusSkipped, Reason: "no extension ID given"})
			continue
		case b.Firefox():
			m = FirefoxManifest(launcher, t.FirefoxIDs)
		default:
			m = ChromiumManifest(launcher, t.ChromiumOrigins)
		}
		if !wrote {
			if err := in.writeLauncher(launcher); err != nil {
				return results, err
			}
			wrote = true
		}
		reg, err := in.registration(b)
		if err != nil {
			return results, err
		}
		data, err := m.Marshal()
		if err != nil {
			return results, err
		}
		if err := os.MkdirAll(filepath.Dir(reg.ManifestPath), 0o755); err != nil { //nolint:gosec // browser profile directories are user-readable
			return results, fmt.Errorf("nativehost: create manifest directory: %w", err)
		}
		if err := os.WriteFile(reg.ManifestPath, data, 0o644); err != nil { //nolint:gosec // the manifest holds no secret; browsers must read it
			return results, fmt.Errorf("nativehost: write manifest: %w", err)
		}
		if reg.RegistryKey != "" {
			if err := in.registry().Set(reg.RegistryKey, reg.ManifestPath); err != nil {
				return results, fmt.Errorf("nativehost: %w", err)
			}
		}
		results = append(results, Result{Browser: b, Manifest: reg.ManifestPath, Registry: reg.RegistryKey, Status: StatusInstalled})
	}
	if !wrote {
		return results, errors.New("nativehost: nothing installed: give --extension-id and/or --firefox-id")
	}
	return results, nil
}

// Uninstall removes the registrations of the selected browsers (default all)
// and, when all browsers were selected, the launcher.
func (in *Installer) Uninstall(browsers []platform.Browser) ([]Result, error) {
	all := len(browsers) == 0
	if all {
		browsers = platform.Browsers()
	}
	var results []Result
	for _, b := range browsers {
		if !b.Valid() {
			return nil, fmt.Errorf("nativehost: unknown browser %q", b)
		}
		reg, err := in.registration(b)
		if err != nil {
			return results, err
		}
		res := Result{Browser: b, Manifest: reg.ManifestPath, Registry: reg.RegistryKey, Status: StatusAbsent}
		switch err := os.Remove(reg.ManifestPath); {
		case err == nil:
			res.Status = StatusRemoved
		case !errors.Is(err, fs.ErrNotExist):
			return results, fmt.Errorf("nativehost: remove manifest: %w", err)
		}
		if reg.RegistryKey != "" {
			if err := in.registry().Delete(reg.RegistryKey); err != nil {
				return results, fmt.Errorf("nativehost: %w", err)
			}
		}
		results = append(results, res)
	}
	if all {
		if err := os.Remove(in.LauncherPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return results, fmt.Errorf("nativehost: remove launcher: %w", err)
		}
	}
	return results, nil
}

// Status reports, for the selected browsers (default all), whether the host is
// registered: "installed" when its manifest file exists, otherwise "absent".
func (in *Installer) Status(browsers []platform.Browser) ([]Result, error) {
	if len(browsers) == 0 {
		browsers = platform.Browsers()
	}
	var results []Result
	for _, b := range browsers {
		if !b.Valid() {
			return nil, fmt.Errorf("nativehost: unknown browser %q", b)
		}
		reg, err := in.registration(b)
		if err != nil {
			return results, err
		}
		res := Result{Browser: b, Manifest: reg.ManifestPath, Registry: reg.RegistryKey, Status: StatusAbsent}
		if _, err := os.Stat(reg.ManifestPath); err == nil {
			res.Status = StatusInstalled
		}
		results = append(results, res)
	}
	return results, nil
}

func (in *Installer) writeLauncher(path string) error {
	script, err := LauncherScript(in.goos(), in.Executable, in.ConfigDir, in.ServerURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // holds no secret; the browser must be able to run it
		return fmt.Errorf("nativehost: create launcher directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // the browser must be able to execute it
		return fmt.Errorf("nativehost: write launcher: %w", err)
	}
	// WriteFile does not change the mode of an existing file.
	if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // see above
		return fmt.Errorf("nativehost: chmod launcher: %w", err)
	}
	return nil
}

// LauncherScript returns the script browsers run: a manifest cannot carry
// arguments, so a small script starts `dlm native-host` with the daemon
// location baked in. It is a batch file on Windows and a POSIX shell script
// elsewhere; both pass through the arguments the browser adds (the caller's
// origin, --parent-window).
func LauncherScript(goos, executable, configDir, serverURL string) (string, error) {
	for _, v := range []string{executable, configDir, serverURL} {
		if strings.ContainsAny(v, "\r\n\x00") {
			return "", fmt.Errorf("nativehost: %q contains a line break or NUL", v)
		}
	}
	if goos == "windows" {
		for _, v := range []string{executable, configDir, serverURL} {
			if strings.ContainsAny(v, `"`) {
				return "", fmt.Errorf("nativehost: %q contains a double quote", v)
			}
		}
		q := func(s string) string { return `"` + strings.ReplaceAll(s, "%", "%%") + `"` }
		return "@echo off\r\n" + q(executable) + " native-host --config-dir " + q(configDir) + " --server " + q(serverURL) + " %*\r\n", nil
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	return "#!/bin/sh\nexec " + q(executable) + " native-host --config-dir " + q(configDir) + " --server " + q(serverURL) + " \"$@\"\n", nil
}
