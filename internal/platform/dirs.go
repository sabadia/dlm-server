package platform

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// UserDataDir returns the base directory for user-specific application data.
//
//   - Linux/BSD: $XDG_DATA_HOME or ~/.local/share
//   - macOS: ~/Library/Application Support
//   - Windows: %LocalAppData%
func UserDataDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LocalAppData"); dir != "" {
			return dir, nil
		}
		return "", errors.New("platform: %LocalAppData% is not defined")
	case "darwin", "ios":
		return os.UserConfigDir() //nolint:wrapcheck // stdlib error is descriptive
	default:
		if dir := os.Getenv("XDG_DATA_HOME"); dir != "" && filepath.IsAbs(dir) {
			return dir, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err //nolint:wrapcheck // stdlib error is descriptive
		}
		return filepath.Join(home, ".local", "share"), nil
	}
}

// UserDownloadsDir returns the user's downloads directory. It honours
// $XDG_DOWNLOAD_DIR on Linux and falls back to ~/Downloads everywhere.
func UserDownloadsDir() (string, error) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		if dir := os.Getenv("XDG_DOWNLOAD_DIR"); dir != "" && filepath.IsAbs(dir) {
			return dir, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err //nolint:wrapcheck // stdlib error is descriptive
	}
	return filepath.Join(home, "Downloads"), nil
}
