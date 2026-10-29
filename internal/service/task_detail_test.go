package service

import (
	"context"
	"errors"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
)

// multiFile gives the task a three-file resource (100, 200 and 300 bytes), all
// selected, the way a torrent would have after resolving.
func (f *svcFixture) multiFile(t *testing.T, task *domain.Task) *domain.Task {
	t.Helper()
	task.Resource = &domain.Resource{
		Name: "pack", Size: 600,
		Files: []domain.File{
			{Index: 0, Name: "a", Size: 100, Selected: true},
			{Index: 1, Name: "b", Size: 200, Selected: true},
			{Index: 2, Name: "c", Size: 300, Selected: true},
		},
	}
	task.Size = 600
	if err := f.store.Tasks().Update(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func code(err error) string {
	c, _, _ := domain.Describe(err)
	return c
}

func TestSegments(t *testing.T) {
	f := newSvcFixture(t)
	task := f.withStatus(t)
	got, err := f.tasks.Segments(t.Context(), task.ID)
	if err != nil || len(got) != 0 {
		t.Fatalf("no segments yet: %v, %v", got, err)
	}
	segs := []domain.Segment{{Index: 0, Start: 0, End: 100, Done: 40}, {Index: 1, Start: 100, End: 200}}
	if err := f.store.Segments().Replace(t.Context(), task.ID, segs); err != nil {
		t.Fatal(err)
	}
	got, err = f.tasks.Segments(t.Context(), task.ID)
	if err != nil || len(got) != 2 || got[0].Done != 40 {
		t.Fatalf("segments = %v, %v", got, err)
	}
	if _, err := f.tasks.Segments(t.Context(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
}

func TestSetFiles(t *testing.T) {
	f := newSvcFixture(t)
	task := f.multiFile(t, f.withStatus(t, domain.StatusPaused))

	got, err := f.tasks.SetFiles(t.Context(), task.ID, []int{0, 2})
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 400 {
		t.Errorf("size = %d, want 400 (the selected files)", got.Size)
	}
	sel := map[int]bool{}
	for _, file := range got.Resource.Files {
		sel[file.Index] = file.Selected
	}
	if !sel[0] || sel[1] || !sel[2] {
		t.Errorf("selection = %v", sel)
	}
	if stored := f.get(t, task.ID); stored.Size != 400 || stored.Resource.Files[1].Selected {
		t.Errorf("not saved: %+v", stored.Resource.Files)
	}

	// Selecting again brings the file back.
	got, err = f.tasks.SetFiles(t.Context(), task.ID, []int{0, 1, 2})
	if err != nil || got.Size != 600 {
		t.Fatalf("select all: size %d, %v", got.Size, err)
	}
}

func TestSetFilesClampsDownloaded(t *testing.T) {
	f := newSvcFixture(t)
	task := f.multiFile(t, f.withStatus(t, domain.StatusPaused))
	task.Downloaded = 500
	if err := f.store.Tasks().Update(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	got, err := f.tasks.SetFiles(t.Context(), task.ID, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 100 || got.Downloaded != 100 {
		t.Errorf("size %d downloaded %d, want both 100", got.Size, got.Downloaded)
	}
}

func TestSetFilesRefusals(t *testing.T) {
	f := newSvcFixture(t)
	tests := []struct {
		name string
		path []domain.TaskStatus
		sel  []int
		kind error
		code string
	}{
		{"running", []domain.TaskStatus{domain.StatusRunning}, []int{0}, domain.ErrConflict, "task.files_locked"},
		{"completed", []domain.TaskStatus{domain.StatusRunning, domain.StatusPostprocessing, domain.StatusCompleted}, []int{0}, domain.ErrConflict, "task.files_locked"},
		{"unknown index", []domain.TaskStatus{domain.StatusPaused}, []int{7}, domain.ErrInvalidArgument, "task.unknown_file"},
		{"nothing selected", []domain.TaskStatus{domain.StatusPaused}, nil, domain.ErrInvalidArgument, "task.no_file_selected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := f.multiFile(t, f.withStatus(t, tt.path...))
			_, err := f.tasks.SetFiles(t.Context(), task.ID, tt.sel)
			if !errors.Is(err, tt.kind) || code(err) != tt.code {
				t.Fatalf("err = %v (code %q), want %v %q", err, code(err), tt.kind, tt.code)
			}
			if got := f.get(t, task.ID); got.Size != 600 {
				t.Errorf("a refused change altered the task: size %d", got.Size)
			}
		})
	}

	t.Run("no file list", func(t *testing.T) {
		task := f.withStatus(t, domain.StatusPaused)
		_, err := f.tasks.SetFiles(t.Context(), task.ID, []int{0})
		if !errors.Is(err, domain.ErrInvalidArgument) || code(err) != "task.no_files" {
			t.Fatalf("err = %v (code %q)", err, code(err))
		}
	})
	t.Run("unknown task", func(t *testing.T) {
		if _, err := f.tasks.SetFiles(t.Context(), "nope", []int{0}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestTorrentOfAnHTTPTaskIsNotFound(t *testing.T) {
	f := newSvcFixture(t)
	task := f.withStatus(t)
	_, err := f.tasks.Torrent(t.Context(), task.ID)
	if !errors.Is(err, domain.ErrNotFound) || code(err) != "task.no_torrent" {
		t.Fatalf("err = %v (code %q)", err, code(err))
	}
	if _, err := f.tasks.Torrent(t.Context(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
}
