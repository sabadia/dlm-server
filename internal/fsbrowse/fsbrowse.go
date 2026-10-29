// Package fsbrowse lists directories and reports free space for clients that
// cannot use the daemon's file system directly (the desktop app talking to a
// remote daemon). Everything is limited to the directories named in
// server.fs_roots: a path outside them, whether given directly or reached
// through a symbolic link, is refused.
package fsbrowse

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/platform"
)

// MaxEntries bounds one listing.
const MaxEntries = 5000

// Entry is a directory.
type Entry struct {
	Name string `json:"name"`
	// Path is the absolute path, usable as the path of the next listing.
	Path string `json:"path"`
}

// Listing is the sub-directories of a directory.
type Listing struct {
	// Path is the listed directory ("" for the list of roots).
	Path string `json:"path"`
	// Parent is the directory above Path, empty at a root.
	Parent  string  `json:"parent,omitempty"`
	Entries []Entry `json:"entries"`
	// Truncated is set when the directory has more than MaxEntries
	// sub-directories.
	Truncated bool `json:"truncated,omitempty"`
}

// Browser lists directories below its roots.
type Browser struct {
	roots []string // absolute, symlinks resolved
	names []string // the roots as configured, for display
}

// New returns a Browser for the given absolute directories. A root that
// cannot be resolved (it does not exist yet) is an error, so a typo is
// noticed at start-up. No roots means the feature is off.
func New(roots []string) (*Browser, error) {
	b := &Browser{}
	for _, r := range roots {
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("fsbrowse: root %q is not absolute", r)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Clean(r))
		if err != nil {
			return nil, fmt.Errorf("fsbrowse: root %q: %w", r, err)
		}
		if !slices.Contains(b.roots, resolved) {
			b.roots = append(b.roots, resolved)
			b.names = append(b.names, filepath.Clean(r))
		}
	}
	return b, nil
}

// Enabled reports whether any root is configured.
func (b *Browser) Enabled() bool { return b != nil && len(b.roots) > 0 }

// Roots returns the configured roots.
func (b *Browser) Roots() []string {
	if b == nil {
		return nil
	}
	return slices.Clone(b.names)
}

func disabled() error {
	return domain.NotFoundCode("fs.disabled", nil, "fsbrowse: browsing is off: set server.fs_roots to allow it")
}

// inside reports whether the resolved path is a root or below one.
func (b *Browser) inside(resolved string) bool {
	for _, r := range b.roots {
		rel, err := filepath.Rel(r, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// lexicallyInside reports whether the cleaned path is below a root as
// written, without looking at the file system.
func (b *Browser) lexicallyInside(clean string) bool {
	for _, r := range append(slices.Clone(b.roots), b.names...) {
		rel, err := filepath.Rel(r, clean)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func outside(path string) error {
	return domain.InvalidArgumentCode("fs.outside_roots", map[string]string{"path": path}, "fsbrowse: %q is outside server.fs_roots", path)
}

// isRoot reports whether the resolved path is exactly a root.
func (b *Browser) isRoot(resolved string) bool { return slices.Contains(b.roots, resolved) }

// resolve cleans path, follows its symbolic links and checks that it is a
// directory inside the roots. It returns the resolved path.
func (b *Browser) resolve(path string) (string, error) {
	if !b.Enabled() {
		return "", disabled()
	}
	if !filepath.IsAbs(path) {
		return "", domain.InvalidArgumentCode("fs.not_absolute", map[string]string{"path": path}, "fsbrowse: %q is not an absolute path", path)
	}
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil && !b.lexicallyInside(clean) {
		// What happens outside the roots is nobody's business, not even
		// whether a path exists there.
		return "", outside(path)
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", domain.NotFoundCode("fs.path_not_found", map[string]string{"path": path}, "fsbrowse: %q does not exist", path)
	case err != nil:
		return "", domain.InvalidArgumentCode("fs.unreadable", map[string]string{"path": path}, "fsbrowse: %q: %v", path, err)
	}
	if !b.inside(resolved) {
		return "", outside(path)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", domain.InvalidArgumentCode("fs.unreadable", map[string]string{"path": path}, "fsbrowse: %q: %v", path, err)
	}
	if !fi.IsDir() {
		return "", domain.InvalidArgumentCode("fs.not_directory", map[string]string{"path": path}, "fsbrowse: %q is not a directory", path)
	}
	return resolved, nil
}

// List returns the sub-directories of path, sorted by name. An empty path
// lists the roots. Entries that are symbolic links to a place outside the
// roots are left out.
func (b *Browser) List(path string) (*Listing, error) {
	if !b.Enabled() {
		return nil, disabled()
	}
	if path == "" {
		out := &Listing{Entries: []Entry{}}
		for _, n := range b.names {
			out.Entries = append(out.Entries, Entry{Name: n, Path: n})
		}
		return out, nil
	}
	resolved, err := b.resolve(path)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(resolved)
	if err != nil {
		return nil, domain.InvalidArgumentCode("fs.unreadable", map[string]string{"path": path}, "fsbrowse: %q: %v", path, err)
	}
	out := &Listing{Path: filepath.Clean(path), Entries: []Entry{}}
	if !b.isRoot(resolved) {
		out.Parent = filepath.Dir(out.Path)
	}
	for _, de := range des {
		full := filepath.Join(out.Path, de.Name())
		isDir := de.IsDir()
		if de.Type()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(full)
			if err != nil || !b.inside(target) {
				continue
			}
			fi, err := os.Stat(target)
			if err != nil {
				continue
			}
			isDir = fi.IsDir()
		}
		if !isDir {
			continue
		}
		if len(out.Entries) >= MaxEntries {
			out.Truncated = true
			break
		}
		out.Entries = append(out.Entries, Entry{Name: de.Name(), Path: full})
	}
	slices.SortFunc(out.Entries, func(a, c Entry) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(c.Name)) })
	return out, nil
}

// Free returns the bytes available to the daemon in the directory path.
func (b *Browser) Free(path string) (uint64, error) {
	resolved, err := b.resolve(path)
	if err != nil {
		return 0, err
	}
	n, err := platform.FreeSpace(resolved)
	if err != nil {
		return 0, domain.InvalidArgumentCode("fs.unreadable", map[string]string{"path": path}, "fsbrowse: free space of %q: %v", path, err)
	}
	return n, nil
}
