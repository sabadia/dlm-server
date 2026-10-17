package browser

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/sabadia/dlm-server/internal/platform"
)

// ErrNoBrowser is returned when no usable browser was found and none can be
// downloaded.
var ErrNoBrowser = errors.New("browser: no Chromium-based browser found")

// Installation is a browser executable that can be driven over the Chrome
// DevTools Protocol.
type Installation struct {
	// Name is the browser family ("chrome", "chromium", "edge", "brave"), or
	// "headless-shell" for a downloaded chrome-headless-shell.
	Name string `json:"name"`
	Path string `json:"path"`
	// Version is known for downloaded headless shells only.
	Version string `json:"version,omitempty"`
}

// NameHeadlessShell is the Installation.Name of chrome-headless-shell.
const NameHeadlessShell = "headless-shell"

// Discover returns the Chromium-family browsers installed on this machine, in
// preference order (Chrome, Chromium, Edge, Brave), without duplicates.
func Discover() []Installation {
	return discover(platform.BrowserCandidates(), exec.LookPath, statFile)
}

// discover resolves candidates with the given PATH lookup and file check.
func discover(candidates []platform.BrowserCandidate, lookPath func(string) (string, error), isFile func(string) bool) []Installation {
	var out []Installation
	var seen []string
	for _, c := range candidates {
		path := c.Path
		if c.Lookup {
			p, err := lookPath(c.Path)
			if err != nil {
				continue
			}
			path = p
		} else if !isFile(path) {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		if slices.Contains(seen, path) {
			continue
		}
		seen = append(seen, path)
		out = append(out, Installation{Name: string(c.Browser), Path: path})
	}
	return out
}

func statFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Validate checks that path is an executable file.
func Validate(path string) (Installation, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Installation{}, fmt.Errorf("browser: %q does not exist", path)
	case err != nil:
		return Installation{}, fmt.Errorf("browser: %w", err)
	case !info.Mode().IsRegular():
		return Installation{}, fmt.Errorf("browser: %q is not a file", path)
	}
	return Installation{Name: "custom", Path: path}, nil
}
