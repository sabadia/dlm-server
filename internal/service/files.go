package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sabadia/dlm-server/internal/domain"
)

// partSuffix is the suffix engines append to the final path while
// downloading (it matches httpx.PartSuffix; a test keeps them in sync).
const partSuffix = ".part"

// errIsDir reports that removeFile found a directory and left it alone.
var errIsDir = errors.New("service: path is a directory")

// isBaseName reports whether name is a single, local path element on every
// OS: no separators, not "." or "..", no volume name.
func isBaseName(name string) bool {
	// filepath.IsLocal accepts ".", which would make the download path the
	// directory itself and its ".part" a sibling outside it.
	return name != "" && name != "." && name != ".." && filepath.IsLocal(name) &&
		filepath.Base(name) == name && !strings.ContainsAny(name, `/\`)
}

// taskPaths returns a task's final and partial file paths. ok is false
// when the task has no directory or name yet (it never ran) or when its name
// is not a safe base name, so callers never touch files outside the task's
// directory.
func taskPaths(t *domain.Task) (final, part string, ok bool) {
	if t.Dir == "" || !filepath.IsAbs(t.Dir) || !isBaseName(t.Name) {
		return "", "", false
	}
	final = filepath.Join(t.Dir, t.Name)
	return final, final + partSuffix, true
}

// fileExists reports whether anything exists at path (without following a
// final symlink).
func fileExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("service: stat %q: %w", path, err)
	}
}

// removeFile removes a regular file or symlink (never its target). A
// missing file is not an error; a directory is left in place and reported
// with errIsDir.
func removeFile(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("service: stat %q: %w", path, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("%w: %q", errIsDir, path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("service: remove %q: %w", path, err)
	}
	return nil
}

// workSuffixes are the suffixes, appended to the final path, of the work
// files and directories an engine leaves while a download is unfinished: the
// single .part file (HTTP, FTP) or directory (yt-dlp) and the HLS/DASH
// segment directory.
var workSuffixes = []string{partSuffix, ".hls", ".ytdl"}

// removeWork removes the work artifacts of an unfinished download next to
// final: files are unlinked and directories removed with everything in them.
// A symlink is unlinked, never followed, and only paths built from the task's
// own file name are touched. A missing path is not an error. Every artifact
// is tried; the errors are joined.
func removeWork(final string) error {
	var errs []error
	for _, suffix := range workSuffixes {
		p := final + suffix
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("service: stat %q: %w", p, err))
			continue
		}
		if fi.IsDir() {
			err = os.RemoveAll(p)
		} else {
			err = os.Remove(p)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("service: remove %q: %w", p, err))
		}
	}
	return errors.Join(errs...)
}
