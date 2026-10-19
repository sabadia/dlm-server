package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
)

// Install limits.
const (
	maxPluginBytes = 5 << 20
	maxPluginFiles = 200
	maxManifest    = 64 << 10
	gitTimeout     = 2 * time.Minute
)

var refRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

// IsGitSource reports whether source names a git repository to clone rather
// than a directory: an https:// URL.
func IsGitSource(source string) bool {
	return strings.HasPrefix(strings.ToLower(source), "https://")
}

// GitClone fetches the repository at url (a branch or tag ref when not empty)
// into dst, which must not exist yet.
type GitClone func(ctx context.Context, url, ref, dst string) error

// gitArgs is the command line of the default clone. Only https is allowed, so
// a repository cannot redirect the clone to a local path or another protocol.
func gitArgs(url, ref, dst string) []string {
	args := []string{"-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "clone", "--depth=1", "--no-tags", "--single-branch"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	return append(args, "--", url, dst)
}

// RunGit clones with the git program on PATH.
func RunGit(ctx context.Context, url, ref, dst string) error {
	if ref != "" && !refRE.MatchString(ref) {
		return domain.InvalidArgument("git ref %q must be a branch or tag name", ref)
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", gitArgs(url, ref, dst)...) //nolint:gosec // fixed program; the URL follows "--" and must be https
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=echo")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New("git is not installed: install it, or install the plugin from a directory")
		}
		return fmt.Errorf("git clone failed: %w: %s", err, clip(string(out)))
	}
	return nil
}

// copyTree copies the regular files of src into dst, leaving out .git. It
// refuses links (they could point outside the plugin) and trees larger than
// the limits.
func copyTree(src, dst string) error {
	var files int
	var total int64
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err //nolint:wrapcheck // the walk's error is the cause
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return fmt.Errorf("copy plugin: %w", err)
		}
		if d.IsDir() {
			if d.Name() == ".git" && rel != "." {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755) //nolint:gosec // plugin files are not secret
		}
		if !d.Type().IsRegular() {
			return domain.InvalidArgument("plugin file %s is not a regular file (links and devices are not allowed)", rel)
		}
		files++
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("copy plugin: %w", err)
		}
		total += info.Size()
		if files > maxPluginFiles || total > maxPluginBytes {
			return domain.InvalidArgument("the plugin has more than %d files or %d bytes", maxPluginFiles, maxPluginBytes)
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
	if err != nil {
		return fmt.Errorf("copy plugin: %w", err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src comes from walking the plugin directory
	if err != nil {
		return fmt.Errorf("copy plugin: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // plugin files are not secret
	if err != nil {
		return fmt.Errorf("copy plugin: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy plugin: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("copy plugin: %w", err)
	}
	return nil
}

// readPlugin reads and validates the manifest and script of the plugin in dir.
func readPlugin(dir string) (*Manifest, []byte, error) {
	data, err := readLimited(filepath.Join(dir, ManifestFile), maxManifest)
	if err != nil {
		return nil, nil, domain.InvalidArgument("plugin %s: %v", ManifestFile, err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		return nil, nil, err
	}
	script, err := readLimited(filepath.Join(dir, filepath.FromSlash(m.Main)), maxScriptBytes)
	if err != nil {
		return nil, nil, domain.InvalidArgument("plugin %s: %v", m.Main, err)
	}
	return m, script, nil
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path is built from the plugin directory and a validated relative name
	if err != nil {
		return nil, errors.New("cannot be read: " + strings.TrimPrefix(err.Error(), "open "+path+": "))
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("cannot be read: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("is larger than %d bytes", limit)
	}
	return data, nil
}
