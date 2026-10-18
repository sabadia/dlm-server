package sqlite_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
)

func TestCategoryRepository(t *testing.T) {
	ctx := t.Context()
	repo := openStore(t).Categories()

	if n, err := repo.NextPosition(ctx); err != nil || n != 0 {
		t.Fatalf("NextPosition of an empty table = %d, %v", n, err)
	}
	video := &domain.Category{ID: "c1", Name: "Video", Dir: "Videos", Extensions: []string{"mp4", "mkv"}, MIMETypes: []string{"video/*"}, Position: 0}
	docs := &domain.Category{ID: "c2", Name: "Documents", Dir: "/data/docs", Extensions: []string{"pdf"}, Position: 1}
	for _, c := range []*domain.Category{docs, video} { // inserted out of order
		if err := repo.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := repo.NextPosition(ctx); err != nil || n != 2 {
		t.Errorf("NextPosition = %d, %v, want 2", n, err)
	}
	got, err := repo.Get(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(video, got); diff != "" {
		t.Errorf("Get differs (-want +got):\n%s", diff)
	}
	list, err := repo.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "c1" || list[1].ID != "c2" {
		t.Fatalf("List = %v, %v; want c1, c2 by position", list, err)
	}

	// Names are unique without regard to case; IDs are unique.
	if err := repo.Create(ctx, &domain.Category{ID: "c3", Name: "VIDEO", Dir: "x", Extensions: []string{"avi"}}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("duplicate name: err = %v, want ErrAlreadyExists", err)
	}
	if err := repo.Create(ctx, &domain.Category{ID: "c1", Name: "Other", Dir: "x", Extensions: []string{"avi"}}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("duplicate id: err = %v, want ErrAlreadyExists", err)
	}

	video.Name, video.Dir, video.Position, video.Extensions = "Movies", "Films", 5, []string{"mp4"}
	if err := repo.Update(ctx, video); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Get(ctx, "c1"); got.Name != "Movies" || got.Dir != "Films" || got.Position != 5 || len(got.Extensions) != 1 {
		t.Errorf("after Update: %+v", got)
	}
	if err := repo.Update(ctx, &domain.Category{ID: "c2", Name: "movies", Dir: "x", Extensions: []string{"pdf"}}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("rename onto an existing name: err = %v", err)
	}
	if err := repo.Update(ctx, &domain.Category{ID: "nope", Name: "N", Dir: "x", Extensions: []string{"a"}}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("update of a missing category: err = %v", err)
	}
	if err := repo.Delete(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, "c1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second delete: err = %v", err)
	}
	if _, err := repo.Get(ctx, "c1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get after delete: err = %v", err)
	}
}

func TestFindDuplicate(t *testing.T) {
	ctx := t.Context()
	s := openStore(t)
	mk := func(id, name, url string, size int64, status domain.TaskStatus, mutate func(*domain.Task)) {
		task := newTask(t, id, domain.MainQueueID, name, url)
		task.Size, task.Status = size, status
		if mutate != nil {
			mutate(task)
		}
		if err := s.Tasks().Create(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	sum := strings.Repeat("ab", 32)
	mk("t1", "a.iso", "https://x.example/a.iso", 1000, domain.StatusCompleted, func(task *domain.Task) {
		task.Source.OriginalURL = "https://x.example/original"
		task.Settings.Checksum = settings.Some(settings.Checksum{Algorithm: settings.ChecksumSHA256, Expected: sum})
	})
	mk("t2", "broken.bin", "https://x.example/broken", 500, domain.StatusError, nil)
	repo := s.Tasks()

	find := func(q storage.DuplicateQuery) string {
		t.Helper()
		got, err := repo.FindDuplicate(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			return ""
		}
		return got.ID
	}
	for name, c := range map[string]struct {
		q    storage.DuplicateQuery
		want string
	}{
		"current url":              {storage.DuplicateQuery{URLs: []string{"https://x.example/a.iso"}}, "t1"},
		"original url":             {storage.DuplicateQuery{URLs: []string{"nope", "https://x.example/original"}}, "t1"},
		"checksum, any case":       {storage.DuplicateQuery{Checksum: strings.ToUpper(sum)}, "t1"},
		"name and size":            {storage.DuplicateQuery{Name: "a.iso", Size: 1000}, "t1"},
		"name with another size":   {storage.DuplicateQuery{Name: "a.iso", Size: 999}, ""},
		"unknown size is not used": {storage.DuplicateQuery{Name: "a.iso", Size: -1}, ""},
		"failed tasks do not count for name and size": {storage.DuplicateQuery{Name: "broken.bin", Size: 500}, ""},
		"failed tasks count for the same url":         {storage.DuplicateQuery{URLs: []string{"https://x.example/broken"}}, "t2"},
		"nothing":                                     {storage.DuplicateQuery{}, ""},
		"url wins over the rest":                      {storage.DuplicateQuery{URLs: []string{"https://x.example/broken"}, Name: "a.iso", Size: 1000}, "t2"},
	} {
		if got := find(c.q); got != c.want {
			t.Errorf("%s: found %q, want %q", name, got, c.want)
		}
	}
}
