package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/settings"
)

// errExistingFile is returned by Runner.download when the file-exists policy
// decided that the file already at the destination is the download: the task
// completes without downloading.
var errExistingFile = errors.New("service: the destination already has the file")

// maxRenameTries bounds the search for a free file name.
const maxRenameTries = 10000

// applyFileExists applies the effective fileExistsPolicy to a download that is
// about to start from nothing and whose destination exists:
//
//   - rename (default): the task takes the next free name, "name (1).ext";
//   - overwrite: the existing file is deleted (a directory is never deleted);
//   - skip: the download is not made; the existing file counts as the result;
//   - resume: the existing file counts as the result when it has the size the
//     download has, otherwise the task takes a new name like with rename;
//   - ask: the task fails with file_exists, so the policy or the name can be
//     changed before it is resumed.
//
// It returns errExistingFile for a result that is already there, and nil when
// the download goes on (possibly under a new name).
func (r *Runner) applyFileExists(ctx context.Context, task *domain.Task, eff settings.Settings) error {
	path := filepath.Join(task.Dir, task.Name)
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("service: stat %q: %w", path, err))
	}
	policy := eff.FileExistsPolicy.OrElse(settings.FileExistsRename)
	r.d.Logger.InfoContext(ctx, "destination exists", "task_id", task.ID, "path", path, "policy", policy)
	switch policy {
	case settings.FileExistsSkip:
		return errExistingFile
	case settings.FileExistsResume:
		if fi.Mode().IsRegular() && task.Size >= 0 && fi.Size() == task.Size {
			return errExistingFile
		}
		return r.renameTask(ctx, task)
	case settings.FileExistsOverwrite:
		if !fi.Mode().IsRegular() {
			return engine.Fatal(domain.ErrorCodeFileExists, fmt.Errorf("service: %q exists and is not a regular file; it is not overwritten", path))
		}
		if err := os.Remove(path); err != nil {
			return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("service: remove %q: %w", path, err))
		}
		return nil
	case settings.FileExistsAsk:
		te := fmt.Errorf("service: %q already exists; set fileExistsPolicy to rename, overwrite or skip, then retry", path)
		return engine.Fatal(domain.ErrorCodeFileExists, te)
	case settings.FileExistsRename:
		return r.renameTask(ctx, task)
	default:
		return r.renameTask(ctx, task)
	}
}

// renameTask gives the task the next free name and persists it.
func (r *Runner) renameTask(ctx context.Context, task *domain.Task) error {
	name, err := freeName(task.Dir, task.Name)
	if err != nil {
		return engine.Fatal(domain.ErrorCodeFatal, err)
	}
	r.d.Logger.InfoContext(ctx, "renaming the download", "task_id", task.ID, "from", task.Name, "to", name)
	return r.mutate(ctx, task, func(fresh *domain.Task) error {
		fresh.Name = name
		fresh.UpdatedAt = r.d.Clock().UTC()
		return nil
	})
}

// freeName returns "name (n).ext" for the smallest n >= 1 such that neither
// the file nor its .part file exists in dir.
func freeName(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for n := 1; n <= maxRenameTries; n++ {
		candidate := base + " (" + strconv.Itoa(n) + ")" + ext
		p := filepath.Join(dir, candidate)
		_, e1 := os.Lstat(p)
		_, e2 := os.Lstat(p + ".part")
		if errors.Is(e1, fs.ErrNotExist) && errors.Is(e2, fs.ErrNotExist) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("service: no free name for %q in %q", name, dir)
}

// markPresent records that the file at the destination is the result: the
// task's counters become the file's size.
func (r *Runner) markPresent(ctx context.Context, task *domain.Task) error {
	path := filepath.Join(task.Dir, task.Name)
	size := task.Size
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
		size = fi.Size()
	}
	return r.mutate(ctx, task, func(fresh *domain.Task) error {
		if size >= 0 {
			fresh.Size, fresh.Downloaded = size, size
		}
		fresh.UpdatedAt = r.d.Clock().UTC()
		return nil
	})
}
