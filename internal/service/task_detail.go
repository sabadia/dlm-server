package service

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/storage"
)

// Segments returns the download segments of a task as last checkpointed
// (about once a second while it runs). Tasks without segments (engines that
// do not split the file, or not started yet) have none. An unknown task
// matches domain.ErrNotFound.
func (s *TaskService) Segments(ctx context.Context, id string) ([]domain.Segment, error) {
	if _, err := s.d.Store.Tasks().Get(ctx, id); err != nil {
		return nil, fmt.Errorf("service: segments of task %q: %w", id, err)
	}
	segs, err := s.d.Store.Segments().List(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("service: segments of task %q: %w", id, err)
	}
	return segs, nil
}

// SetFiles chooses which files of a multi-file task are downloaded: selected
// lists the indexes of the files to keep, every other file is skipped. The
// task must not be active or completed (409 task.files_locked: pause it
// first); it must have a resolved file list (422) and at least one file must
// stay selected (422). The next run downloads exactly the selected files;
// files that were downloaded and are no longer selected stay on disk.
func (s *TaskService) SetFiles(ctx context.Context, id string, selected []int) (*domain.Task, error) {
	var t *domain.Task
	err := s.d.withTaskLock(ctx, id, func() error {
		return s.d.Store.WithTx(ctx, func(tx storage.Store) error { //nolint:wrapcheck // wrapped inside the transaction
			var err error
			if t, err = tx.Tasks().Get(ctx, id); err != nil {
				return fmt.Errorf("service: set files of task %q: %w", id, err)
			}
			if t.Status.Active() || t.Status == domain.StatusCompleted {
				return domain.ConflictCode("task.files_locked", map[string]string{"taskId": id, "status": string(t.Status)},
					"service: task %q is %s: pause it before changing its files (completed tasks must be restarted)", id, t.Status)
			}
			if t.Resource == nil || len(t.Resource.Files) == 0 {
				return domain.InvalidArgumentCode("task.no_files", map[string]string{"taskId": id},
					"service: task %q has no file list to choose from", id)
			}
			want := make(map[int]bool, len(selected))
			for _, i := range selected {
				if !slices.ContainsFunc(t.Resource.Files, func(f domain.File) bool { return f.Index == i }) {
					return domain.InvalidArgumentCode("task.unknown_file", map[string]string{"taskId": id, "index": fmt.Sprint(i)},
						"service: task %q has no file with index %d", id, i)
				}
				want[i] = true
			}
			if len(want) == 0 {
				return domain.InvalidArgumentCode("task.no_file_selected", map[string]string{"taskId": id},
					"service: select at least one file of task %q", id)
			}
			files := slices.Clone(t.Resource.Files)
			for i := range files {
				files[i].Selected = want[files[i].Index]
			}
			res := *t.Resource
			res.Files = files
			t.Resource = &res
			if size := res.SelectedSize(); size >= 0 {
				t.Size = size
				t.Downloaded = min(t.Downloaded, size)
			}
			t.UpdatedAt = s.d.now()
			if err := t.Validate(); err != nil {
				return fmt.Errorf("service: set files of task %q: %w", id, err)
			}
			if err := tx.Tasks().Update(ctx, t); err != nil {
				return fmt.Errorf("service: set files of task %q: %w", id, err)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	s.d.publishTask(events.TypeTaskUpdated, t)
	return t, nil
}

// Torrent describes the swarm of a torrent task: live while it runs,
// otherwise what its last run saved. A task of another engine, or a torrent
// that never ran, matches domain.ErrNotFound with the code task.no_torrent.
func (s *TaskService) Torrent(ctx context.Context, id string) (*domain.TorrentInfo, error) {
	t, err := s.d.Store.Tasks().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("service: torrent of task %q: %w", id, err)
	}
	if s.d.Runner != nil {
		if dl, ok := s.d.Runner.Running(id); ok {
			if in, ok := dl.(engine.Inspectable); ok {
				if info, ok := in.Torrent(); ok {
					return info, nil
				}
			}
		}
	}
	if t.Engine != "" && s.d.Engines != nil {
		if eng, err := s.d.Engines.Get(t.Engine); err == nil {
			if d, ok := eng.(engine.Describer); ok {
				state, err := s.d.Store.EngineState().Get(ctx, id)
				if err != nil && !errors.Is(err, domain.ErrNotFound) {
					return nil, fmt.Errorf("service: torrent of task %q: %w", id, err)
				}
				eff, err := effective(ctx, s.d, t.QueueID, t.Settings)
				if err != nil {
					return nil, err
				}
				if info, ok := d.Describe(state, t.Resource, eff.Settings); ok {
					return info, nil
				}
			}
		}
	}
	return nil, domain.NotFoundCode("task.no_torrent", map[string]string{"taskId": id},
		"service: task %q has no torrent information", id)
}
