package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/postproc"
	"github.com/sabadia/dlm-server/internal/settings"
)

// withPost returns a fixture whose runner post-processes with p.
func withPost(t *testing.T, p PostProcessor, mutate ...func(*settings.Settings)) *fixture {
	t.Helper()
	f := newFixture(t)
	f.runner.d.Post = p
	if len(mutate) > 0 {
		def := f.runner.d.Defaults()
		for _, m := range mutate {
			m(&def)
		}
		f.runner.d.Defaults = func() settings.Settings { return def }
	}
	return f
}

// createChecked creates a task for url that must match the given SHA-256.
func (f *fixture) createChecked(t *testing.T, url, sum string) *domain.Task {
	t.Helper()
	task, err := domain.NewTask(domain.NewID(), time.Now(), domain.NewTaskParams{
		Source:   domain.Source{CurrentURL: url},
		Settings: settings.Settings{Checksum: settings.Some(settings.Checksum{Algorithm: settings.ChecksumSHA256, Expected: sum})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Tasks().Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestRunnerVerifiesTheChecksum(t *testing.T) {
	srv := newFileServer(t, 1<<20, 0)
	sum := sha256.Sum256(srv.data)
	f := withPost(t, postproc.Default(postproc.Options{}))
	task := f.createChecked(t, srv.URL+"/files/data.bin", hex.EncodeToString(sum[:]))
	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.task(t, task.ID); got.Status != domain.StatusCompleted {
		t.Fatalf("status = %s (%+v)", got.Status, got.Error)
	}
	sameContent(t, filepath.Join(f.dir, "data.bin"), srv.data)
}

func TestRunnerDownloadsAgainOnAChecksumMismatchThenGivesUp(t *testing.T) {
	srv := newFileServer(t, 1<<20, 0)
	f := withPost(t, postproc.Default(postproc.Options{}), func(s *settings.Settings) { s.Retry.MaxAttempts = settings.Some(2) })
	task := f.createChecked(t, srv.URL+"/files/data.bin", hex.EncodeToString(make([]byte, 32)))
	path := filepath.Join(f.dir, "data.bin")

	for attempt := 1; attempt <= 2; attempt++ {
		if err := f.runner.Run(t.Context(), task.ID); err != nil {
			t.Fatalf("run %d: %v", attempt, err)
		}
		got := f.task(t, task.ID)
		if got.Status != domain.StatusQueued || got.Downloaded != 0 {
			t.Fatalf("run %d: status %s downloaded %d, want queued with the progress cleared", attempt, got.Status, got.Downloaded)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("run %d: the file with the wrong checksum is still there: %v", attempt, err)
		}
		if segs, err := f.store.Segments().List(t.Context(), task.ID); err != nil || len(segs) != 0 {
			t.Fatalf("run %d: segments %v, %v", attempt, segs, err)
		}
	}
	// The third failed check exceeds retry.maxAttempts (2).
	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	got := f.task(t, task.ID)
	if got.Status != domain.StatusError || got.Error == nil || got.Error.Code != domain.ErrorCodeChecksumMismatch || !got.Error.Retryable {
		t.Fatalf("task = %s %+v", got.Status, got.Error)
	}
	// The counter starts over: a retry downloads again instead of failing at once.
	if err := f.store.Tasks().Update(t.Context(), mustQueued(t, f, task.ID)); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.task(t, task.ID); got.Status != domain.StatusQueued {
		t.Errorf("after a retry: status %s, want queued for another download", got.Status)
	}
}

func mustQueued(t *testing.T, f *fixture, id string) *domain.Task {
	t.Helper()
	task := f.task(t, id)
	if err := task.TransitionTo(domain.StatusQueued, time.Now()); err != nil {
		t.Fatal(err)
	}
	return task
}

type fakePost struct {
	job *postproc.Job
	err error
	// block makes Process wait for the context.
	block bool
}

func (p fakePost) Process(ctx context.Context, t *domain.Task, eff settings.Settings) (*postproc.Job, error) {
	if p.block {
		<-ctx.Done()
		return nil, ctx.Err() //nolint:wrapcheck // a test double
	}
	if p.job == nil && p.err == nil {
		return postproc.NewJob(t, eff), nil
	}
	return p.job, p.err
}

func TestRunnerPostprocessOutcomes(t *testing.T) {
	srv := newFileServer(t, 64<<10, 0)

	// A step that moves the file: the task follows it.
	f := withPost(t, nil)
	task := f.createTask(t.Context(), t, srv.URL+"/files/data.bin")
	moved := filepath.Join(f.dir, "sorted", "renamed.bin")
	f.runner.d.Post = postProc(func(tk *domain.Task, eff settings.Settings) (*postproc.Job, error) {
		j := postproc.NewJob(tk, eff)
		j.Path = moved
		return j, nil
	})
	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	got := f.task(t, task.ID)
	if got.Status != domain.StatusCompleted || got.Dir != filepath.Dir(moved) || got.Name != "renamed.bin" {
		t.Errorf("moved: task = %s dir %q name %q", got.Status, got.Dir, got.Name)
	}

	// A failing step: error with the postprocess code, retryable.
	f = withPost(t, fakePost{err: errors.New("postproc: extract: boom")})
	task = f.createTask(t.Context(), t, srv.URL+"/files/data.bin")
	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	got = f.task(t, task.ID)
	if got.Status != domain.StatusError || got.Error.Code != domain.ErrorCodePostprocess || !got.Error.Retryable {
		t.Errorf("failing step: task = %s %+v", got.Status, got.Error)
	}
	sameContent(t, filepath.Join(f.dir, "data.bin"), srv.data) // the download stays

	// A stop during post-processing leaves a retryable error, like a crash does.
	f = withPost(t, fakePost{block: true})
	task = f.createTask(t.Context(), t, srv.URL+"/files/data.bin")
	ctx, cancel := context.WithCancelCause(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.runner.Run(ctx, task.ID) }()
	deadline := time.Now().Add(15 * time.Second)
	for f.task(t, task.ID).Status != domain.StatusPostprocessing {
		if time.Now().After(deadline) {
			t.Fatal("the task never reached postprocessing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel(ErrPaused)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got = f.task(t, task.ID)
	if got.Status != domain.StatusError || got.Error == nil || !got.Error.Retryable {
		t.Errorf("stopped: task = %s %+v", got.Status, got.Error)
	}
}

// postProc adapts a function to PostProcessor.
type postProc func(*domain.Task, settings.Settings) (*postproc.Job, error)

func (p postProc) Process(_ context.Context, t *domain.Task, eff settings.Settings) (*postproc.Job, error) {
	return p(t, eff)
}

func TestRunnerKeepsPostActionNotesInALabel(t *testing.T) {
	srv := newFileServer(t, 8<<10, 0)
	f := withPost(t, postProc(func(tk *domain.Task, eff settings.Settings) (*postproc.Job, error) {
		j := postproc.NewJob(tk, eff)
		j.Note("extract: 3 files unpacked to /x")
		j.Note("add-torrent: no engine")
		return j, nil
	}))
	task := f.createTask(t.Context(), t, srv.URL+"/files/data.bin")
	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	got := f.task(t, task.ID)
	if got.Status != domain.StatusCompleted || got.Labels[PostprocessLabel] != "extract: 3 files unpacked to /x; add-torrent: no engine" {
		t.Errorf("task = %s labels %v", got.Status, got.Labels)
	}
	// Notes never grow the label without bound.
	f = withPost(t, postProc(func(tk *domain.Task, eff settings.Settings) (*postproc.Job, error) {
		j := postproc.NewJob(tk, eff)
		j.Note("%s", string(make([]byte, 5000)))
		return j, nil
	}))
	task = f.createTask(t.Context(), t, srv.URL+"/files/data.bin")
	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	if l := f.task(t, task.ID).Labels[PostprocessLabel]; len(l) > maxNotesLength+len("…") {
		t.Errorf("label has %d bytes", len(l))
	}
	// Without notes there is no label.
	f = withPost(t, fakePost{})
	task = f.createTask(t.Context(), t, srv.URL+"/files/data.bin")
	if err := f.runner.Run(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.task(t, task.ID).Labels[PostprocessLabel]; ok {
		t.Error("a label was added although the actions had nothing to say")
	}
}
