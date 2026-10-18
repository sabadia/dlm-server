package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

func TestCategoryServiceCRUDAndMatch(t *testing.T) {
	f := newSvcFixture(t)
	s := NewCategoryService(Deps{Store: f.store})
	ctx := t.Context()

	video, err := s.Create(ctx, CreateCategoryInput{Name: " Video ", Dir: "Videos", Extensions: []string{".MP4", " mkv ", ""}, MIMETypes: []string{"Video/*"}})
	if err != nil {
		t.Fatal(err)
	}
	if video.Name != "Video" || video.Position != 0 || strings.Join(video.Extensions, ",") != "mp4,mkv" || strings.Join(video.MIMETypes, ",") != "video/*" {
		t.Fatalf("created = %+v (rules are trimmed, lower-cased and without dots)", video)
	}
	docs, err := s.Create(ctx, CreateCategoryInput{Name: "Docs", Dir: "/data/docs", Extensions: []string{"pdf", "doc"}})
	if err != nil || docs.Position != 1 {
		t.Fatalf("second category = %+v, %v", docs, err)
	}
	for name, in := range map[string]CreateCategoryInput{
		"no rules": {Name: "X", Dir: "x"}, "no dir": {Name: "X", Extensions: []string{"a"}}, "escaping dir": {Name: "X", Dir: "../x", Extensions: []string{"a"}},
		"bad ext": {Name: "X", Dir: "x", Extensions: []string{"a/b"}}, "bad mime": {Name: "X", Dir: "x", MIMETypes: []string{"video"}},
	} {
		if _, err := s.Create(ctx, in); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	if _, err := s.Create(ctx, CreateCategoryInput{Name: "VIDEO", Dir: "x", Extensions: []string{"avi"}}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("duplicate name: err = %v", err)
	}

	for _, c := range []struct {
		name, mime, want string
	}{
		{"movie.MP4", "", "Video"},
		{"clip", "video/webm; codecs=vp9", "Video"},
		{"paper.pdf", "application/pdf", "Docs"},
		{"data.bin", "application/octet-stream", ""},
		{"", "", ""},
	} {
		got, err := s.Match(ctx, c.name, c.mime)
		if err != nil {
			t.Fatal(err)
		}
		if (got == nil && c.want != "") || (got != nil && got.Name != c.want) {
			t.Errorf("Match(%q, %q) = %v, want %q", c.name, c.mime, got, c.want)
		}
	}

	// The first match in position order wins: move Docs in front and give it mp4.
	pos := int64(0)
	exts := []string{"pdf", "MP4"}
	updated, err := s.Update(ctx, docs.ID, UpdateCategoryInput{Extensions: &exts, Position: &pos})
	if err != nil || strings.Join(updated.Extensions, ",") != "pdf,mp4" {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	// Equal positions are ordered by ID, which is time ordered: Video (older) first again.
	video2 := int64(5)
	if _, err := s.Update(ctx, video.ID, UpdateCategoryInput{Position: &video2}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Match(ctx, "a.mp4", ""); got == nil || got.Name != "Docs" {
		t.Errorf("after moving Docs first, a.mp4 matched %v", got)
	}
	neg := int64(-1)
	if _, err := s.Update(ctx, docs.ID, UpdateCategoryInput{Position: &neg}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("negative position: err = %v", err)
	}
	empty := []string{}
	if _, err := s.Update(ctx, docs.ID, UpdateCategoryInput{Extensions: &empty}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a category left without rules: err = %v", err)
	}
	if _, err := s.Update(ctx, "nope", UpdateCategoryInput{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("update of a missing category: err = %v", err)
	}
	if err := s.Delete(ctx, docs.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, docs.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second delete: err = %v", err)
	}
	if got, _ := s.Get(ctx, video.ID); got == nil || got.Position != 5 {
		t.Errorf("Get = %+v", got)
	}
}

func TestRunnerSortsDownloadsIntoCategories(t *testing.T) {
	srv := newFileServer(t, 64<<10, 0)
	f := newFixture(t)
	cats := NewCategoryService(Deps{Store: f.store})
	f.runner.d.Categories = cats
	if _, err := cats.Create(t.Context(), CreateCategoryInput{Name: "Binaries", Dir: "Sorted/Bin", Extensions: []string{"bin"}}); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(t.TempDir(), "elsewhere")
	if _, err := cats.Create(t.Context(), CreateCategoryInput{Name: "Archives", Dir: abs, Extensions: []string{"zip"}}); err != nil {
		t.Fatal(err)
	}
	on := settings.Settings{AutoCategorize: settings.Some(true)}
	create := func(path string, s settings.Settings, dir string) *domain.Task {
		task, err := domain.NewTask(domain.NewID(), time.Now(), domain.NewTaskParams{Source: domain.Source{CurrentURL: srv.URL + path}, Settings: s})
		if err != nil {
			t.Fatal(err)
		}
		task.Dir = dir
		if err := f.store.Tasks().Create(t.Context(), task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	run := func(task *domain.Task) *domain.Task {
		t.Helper()
		if err := f.runner.Run(t.Context(), task.ID); err != nil {
			t.Fatal(err)
		}
		got := f.task(t, task.ID)
		if got.Status != domain.StatusCompleted {
			t.Fatalf("status = %s %+v", got.Status, got.Error)
		}
		return got
	}

	// A relative category directory is below the download directory.
	got := run(create("/files/data.bin", on, ""))
	if got.Dir != filepath.Join(f.dir, "Sorted", "Bin") {
		t.Errorf("dir = %q, want it in the category directory", got.Dir)
	}
	sameContent(t, filepath.Join(f.dir, "Sorted", "Bin", "data.bin"), srv.data)
	// An absolute one is used as it is.
	// (The file server answers every path with the same data; the name comes from the URL.)
	got = run(create("/files/pack.zip", on, ""))
	if got.Dir != abs {
		t.Errorf("dir = %q, want %q", got.Dir, abs)
	}
	// No match: the download directory.
	got = run(create("/files/readme.txt", on, ""))
	if got.Dir != f.dir {
		t.Errorf("unmatched file went to %q, want %q", got.Dir, f.dir)
	}
	// Sorting is off unless autoCategorize is on.
	got = run(create("/files/other.bin", settings.Settings{}, ""))
	if got.Dir != f.dir {
		t.Errorf("without autoCategorize the file went to %q", got.Dir)
	}
	// A task with a directory of its own keeps it.
	own := filepath.Join(t.TempDir(), "mine")
	got = run(create("/files/mine.bin", on, own))
	if got.Dir != own {
		t.Errorf("explicit dir = %q, want %q", got.Dir, own)
	}
}

func TestRunnerFileExistsPolicies(t *testing.T) {
	srv := newFileServer(t, 64<<10, 0)
	old := []byte("an earlier file")
	same := make([]byte, len(srv.data)) // as big as the download
	type want struct {
		status   domain.TaskStatus
		name     string // the task's name afterwards
		existing []byte // what is at data.bin afterwards (nil: unchanged, compared to old)
		fetched  bool   // whether the download ran (a skipped one still probes the server)
		code     string
	}
	cases := map[string]struct {
		policy settings.FileExistsPolicy
		prior  []byte
		want   want
	}{
		"rename":                 {settings.FileExistsRename, old, want{domain.StatusCompleted, "data (1).bin", old, true, ""}},
		"overwrite":              {settings.FileExistsOverwrite, old, want{domain.StatusCompleted, "data.bin", srv.data, true, ""}},
		"skip":                   {settings.FileExistsSkip, old, want{domain.StatusCompleted, "data.bin", old, false, ""}},
		"resume, other size":     {settings.FileExistsResume, old, want{domain.StatusCompleted, "data (1).bin", old, true, ""}},
		"resume, the same size":  {settings.FileExistsResume, same, want{domain.StatusCompleted, "data.bin", same, false, ""}},
		"ask":                    {settings.FileExistsAsk, old, want{domain.StatusError, "data.bin", old, false, domain.ErrorCodeFileExists}},
		"default is rename":      {"", old, want{domain.StatusCompleted, "data (1).bin", old, true, ""}},
		"no file, nothing to do": {settings.FileExistsAsk, nil, want{domain.StatusCompleted, "data.bin", srv.data, true, ""}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			path := filepath.Join(f.dir, "data.bin")
			if c.prior != nil {
				if err := os.WriteFile(path, c.prior, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var s settings.Settings
			if c.policy != "" {
				s.FileExistsPolicy = settings.Some(c.policy)
			}
			task, err := domain.NewTask(domain.NewID(), time.Now(), domain.NewTaskParams{Source: domain.Source{CurrentURL: srv.URL + "/files/data.bin"}, Settings: s})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.Tasks().Create(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			if err := f.runner.Run(t.Context(), task.ID); err != nil {
				t.Fatal(err)
			}
			got := f.task(t, task.ID)
			if got.Status != c.want.status || got.Name != c.want.name {
				t.Fatalf("status %s name %q (%+v), want %s %q", got.Status, got.Name, got.Error, c.want.status, c.want.name)
			}
			if c.want.code != "" && (got.Error == nil || got.Error.Code != c.want.code) {
				t.Errorf("error = %+v, want code %s", got.Error, c.want.code)
			}
			if b, err := os.ReadFile(path); err != nil || string(b) != string(c.want.existing) {
				t.Errorf("data.bin holds %d bytes (%v), want %d", len(b), err, len(c.want.existing))
			}
			if c.want.fetched && c.want.name != "data.bin" {
				sameContent(t, filepath.Join(f.dir, c.want.name), srv.data)
			}
			if got.Status == domain.StatusCompleted && !c.want.fetched {
				// The existing file is the result: the counters say so.
				if got.Downloaded != int64(len(c.want.existing)) || got.Size != got.Downloaded {
					t.Errorf("counters = %d/%d for a %d byte file", got.Downloaded, got.Size, len(c.want.existing))
				}
			}
		})
	}
}

func TestFileExistsOverwriteNeverRemovesADirectory(t *testing.T) {
	srv := newFileServer(t, 4<<10, 0)
	f := newFixture(t)
	dirAtPath := filepath.Join(f.dir, "data.bin")
	if err := os.Mkdir(dirAtPath, 0o755); err != nil {
		t.Fatal(err)
	}
	task, err := domain.NewTask(domain.NewID(), time.Now(), domain.NewTaskParams{
		Source:   domain.Source{CurrentURL: srv.URL + "/files/data.bin"},
		Settings: settings.Settings{FileExistsPolicy: settings.Some(settings.FileExistsOverwrite)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Tasks().Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.task(t, task.ID); got.Status != domain.StatusError || got.Error.Code != domain.ErrorCodeFileExists {
		t.Errorf("task = %s %+v", got.Status, got.Error)
	}
	if fi, err := os.Stat(dirAtPath); err != nil || !fi.IsDir() {
		t.Errorf("the directory was touched: %v", err)
	}
}

func TestFreeName(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.tar.gz", "a.tar (1).gz", "a.tar (2).gz.part", "noext", "noext (1)"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for in, want := range map[string]string{"a.tar.gz": "a.tar (3).gz", "noext": "noext (2)", "fresh.bin": "fresh (1).bin"} {
		if got, err := freeName(dir, in); err != nil || got != want {
			t.Errorf("freeName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestDuplicateDetection(t *testing.T) {
	f := newSvcFixture(t)
	srv := newFileServer(t, 8<<10, 0)
	ctx := t.Context()
	u := srv.URL + "/files/a.bin"

	first, err := f.tasks.Create(ctx, CreateTaskInput{Source: domain.Source{CurrentURL: u}})
	if err != nil {
		t.Fatal(err)
	}
	// The same link is refused, with the existing task named; the flag allows it.
	_, err = f.tasks.Create(ctx, CreateTaskInput{Source: domain.Source{CurrentURL: u}})
	if !errors.Is(err, domain.ErrConflict) || !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("duplicate: err = %v, want a conflict naming %s", err, first.ID)
	}
	if _, err := f.tasks.Create(ctx, CreateTaskInput{Source: domain.Source{CurrentURL: u}, AllowDuplicate: true}); err != nil {
		t.Fatalf("allowed duplicate: %v", err)
	}
	// The original URL counts too.
	_, err = f.tasks.Create(ctx, CreateTaskInput{Source: domain.Source{CurrentURL: srv.URL + "/files/new-link", OriginalURL: u}})
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("same original URL: err = %v", err)
	}
	// An expected checksum identifies the file whatever the link.
	cs := settings.Settings{Checksum: settings.Some(settings.Checksum{Algorithm: settings.ChecksumSHA256, Expected: strings.Repeat("ab", 32)})}
	if _, err := f.tasks.Create(ctx, CreateTaskInput{Source: domain.Source{CurrentURL: srv.URL + "/files/x1"}, Settings: cs}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.Create(ctx, CreateTaskInput{Source: domain.Source{CurrentURL: srv.URL + "/files/x2"}, Settings: cs}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("same checksum: err = %v", err)
	}
	// A resolved task with the name and size of another is a duplicate too.
	res, err := f.tasks.Resolve(ctx, ResolveInput{Source: domain.Source{CurrentURL: srv.URL + "/files/b.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.Create(ctx, CreateTaskInput{ResolveID: res.ResolveID}); err != nil {
		t.Fatal(err)
	}
	res2, err := f.tasks.Resolve(ctx, ResolveInput{Source: domain.Source{CurrentURL: srv.URL + "/other/b.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.Create(ctx, CreateTaskInput{ResolveID: res2.ResolveID}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("same name and size: err = %v", err)
	}
	// The refused request did not consume its resolve result.
	if _, err := f.tasks.Create(ctx, CreateTaskInput{ResolveID: res2.ResolveID, AllowDuplicate: true}); err != nil {
		t.Errorf("allowed after a refusal: %v", err)
	}
	// Batches judge every item on its own.
	out := f.tasks.CreateBatch(ctx, []CreateTaskInput{
		{Source: domain.Source{CurrentURL: u}},
		{Source: domain.Source{CurrentURL: srv.URL + "/files/fresh"}},
	})
	if !errors.Is(out[0].Err, domain.ErrConflict) || out[1].Err != nil {
		t.Errorf("batch = %v / %v", out[0].Err, out[1].Err)
	}
}
