package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Browser is a web browser that supports Native Messaging.
type Browser string

// Supported browsers.
const (
	BrowserChrome   Browser = "chrome"
	BrowserChromium Browser = "chromium"
	BrowserEdge     Browser = "edge"
	BrowserBrave    Browser = "brave"
	BrowserFirefox  Browser = "firefox"
)

// Browsers lists every supported browser.
func Browsers() []Browser {
	return []Browser{BrowserChrome, BrowserChromium, BrowserEdge, BrowserBrave, BrowserFirefox}
}

// Valid reports whether b is a supported browser.
func (b Browser) Valid() bool {
	for _, k := range Browsers() {
		if b == k {
			return true
		}
	}
	return false
}

// Firefox reports whether b is Firefox, which uses a different manifest
// format (allowed_extensions instead of allowed_origins) and other
// locations than the Chromium family.
func (b Browser) Firefox() bool { return b == BrowserFirefox }

// NativeHostRegistration says where a browser looks for a native messaging
// host: on Linux and macOS the manifest file is placed in a fixed per-user
// directory; on Windows the manifest can live anywhere and a registry key
// points to it.
type NativeHostRegistration struct {
	// ManifestPath is where the host manifest file must be written.
	ManifestPath string
	// RegistryKey is, on Windows only, the HKEY_CURRENT_USER subkey whose
	// default value must be set to ManifestPath.
	RegistryKey string
}

// NativeHostRegistrationFor returns the per-user registration of the host
// called name for browser b on the running operating system. dataDir is
// where Windows manifests are stored (it is not used elsewhere).
func NativeHostRegistrationFor(b Browser, name, dataDir string) (NativeHostRegistration, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return NativeHostRegistration{}, fmt.Errorf("platform: %w", err)
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return NativeHostRegistration{}, fmt.Errorf("platform: %w", err)
	}
	return nativeHostRegistration(runtime.GOOS, b, name, home, cfg, dataDir)
}

// NativeHostRegistrationForDirs is NativeHostRegistrationFor for an explicit
// operating system and directories (see os.UserHomeDir, os.UserConfigDir),
// for tests and cross-platform tooling.
func NativeHostRegistrationForDirs(goos string, b Browser, name, home, userConfigDir, dataDir string) (NativeHostRegistration, error) {
	return nativeHostRegistration(goos, b, name, home, userConfigDir, dataDir)
}

// nativeHostRegistration is NativeHostRegistrationFor for an explicit
// operating system and directories, so the location tables are testable on
// every OS. userConfigDir is os.UserConfigDir: ~/Library/Application Support
// on macOS, $XDG_CONFIG_HOME or ~/.config on Linux.
func nativeHostRegistration(goos string, b Browser, name, home, userConfigDir, dataDir string) (NativeHostRegistration, error) {
	if !b.Valid() {
		return NativeHostRegistration{}, fmt.Errorf("platform: unknown browser %q", b)
	}
	file := name + ".json"
	switch goos {
	case "windows":
		key := map[Browser]string{
			BrowserChrome:   `Software\Google\Chrome\NativeMessagingHosts`,
			BrowserChromium: `Software\Chromium\NativeMessagingHosts`,
			BrowserEdge:     `Software\Microsoft\Edge\NativeMessagingHosts`,
			BrowserBrave:    `Software\BraveSoftware\Brave-Browser\NativeMessagingHosts`,
			BrowserFirefox:  `Software\Mozilla\NativeMessagingHosts`,
		}[b]
		return NativeHostRegistration{
			ManifestPath: filepath.Join(dataDir, "native-messaging", string(b), file),
			RegistryKey:  key + `\` + name,
		}, nil
	case "darwin":
		dir := map[Browser]string{
			BrowserChrome:   filepath.Join(userConfigDir, "Google", "Chrome", "NativeMessagingHosts"),
			BrowserChromium: filepath.Join(userConfigDir, "Chromium", "NativeMessagingHosts"),
			BrowserEdge:     filepath.Join(userConfigDir, "Microsoft Edge", "NativeMessagingHosts"),
			BrowserBrave:    filepath.Join(userConfigDir, "BraveSoftware", "Brave-Browser", "NativeMessagingHosts"),
			BrowserFirefox:  filepath.Join(userConfigDir, "Mozilla", "NativeMessagingHosts"),
		}[b]
		return NativeHostRegistration{ManifestPath: filepath.Join(dir, file)}, nil
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		dir := map[Browser]string{
			BrowserChrome:   filepath.Join(userConfigDir, "google-chrome", "NativeMessagingHosts"),
			BrowserChromium: filepath.Join(userConfigDir, "chromium", "NativeMessagingHosts"),
			BrowserEdge:     filepath.Join(userConfigDir, "microsoft-edge", "NativeMessagingHosts"),
			BrowserBrave:    filepath.Join(userConfigDir, "BraveSoftware", "Brave-Browser", "NativeMessagingHosts"),
			BrowserFirefox:  filepath.Join(home, ".mozilla", "native-messaging-hosts"),
		}[b]
		return NativeHostRegistration{ManifestPath: filepath.Join(dir, file)}, nil
	default:
		return NativeHostRegistration{}, fmt.Errorf("%w: native messaging on %s", ErrUnsupported, goos)
	}
}
