// Package postproc runs the steps that follow a finished download while the
// task is in the postprocessing state: verifying the checksum and, in later
// steps, sorting the file into its category, extracting archives and running
// the task's post-actions.
//
// A Pipeline runs its steps in order on a Job. A step may change where the
// file lives (Job.Path); the task service records the new location. The first
// step that fails stops the pipeline. A step that finds the download itself
// wrong (a checksum mismatch) returns a *MismatchError, which makes the task
// run download the file again instead of failing.
package postproc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// Job is what a step works on.
type Job struct {
	// Task is a snapshot of the finished task; steps must not change it.
	Task domain.Task
	// Settings are the effective settings of the task.
	Settings settings.Settings
	// Path is where the downloaded file (or directory) is now. A step that
	// moves or renames it must update Path.
	Path string
}

// NewJob prepares a job for a finished task.
func NewJob(t *domain.Task, eff settings.Settings) *Job {
	return &Job{Task: *t, Settings: eff, Path: filepath.Join(t.Dir, t.Name)}
}

// Step is one stage of the pipeline.
type Step interface {
	// Name identifies the step in logs and errors.
	Name() string
	// Run does the step's work. It must stop when ctx is cancelled.
	Run(ctx context.Context, j *Job) error
}

// MismatchError reports a downloaded file that is not the file that was asked
// for: the task should be downloaded again.
type MismatchError struct {
	Path      string
	Algorithm settings.ChecksumAlgorithm
	Expected  string
	Got       string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("postproc: %s checksum of %q is %s, expected %s", e.Algorithm, e.Path, e.Got, e.Expected)
}

// Pipeline runs steps in order. It is safe for concurrent use as long as its
// steps are.
type Pipeline struct {
	steps []Step
	log   *slog.Logger
}

// New returns a Pipeline of the given steps.
func New(log *slog.Logger, steps ...Step) *Pipeline {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Pipeline{steps: steps, log: log}
}

// Default returns the pipeline of all the steps dlm runs after a download.
func Default(log *slog.Logger) *Pipeline {
	return New(log, Checksum{})
}

// Process runs the pipeline for a finished task and returns the job as the
// steps left it. A failing step's error carries the step name; a
// *MismatchError is returned as it is (wrapped, so errors.As finds it).
func (p *Pipeline) Process(ctx context.Context, t *domain.Task, eff settings.Settings) (*Job, error) {
	j := NewJob(t, eff)
	for _, s := range p.steps {
		if err := ctx.Err(); err != nil {
			return j, fmt.Errorf("postproc: %w", err)
		}
		if err := s.Run(ctx, j); err != nil {
			var mm *MismatchError
			if errors.As(err, &mm) {
				return j, err
			}
			return j, fmt.Errorf("postproc: %s: %w", s.Name(), err)
		}
	}
	return j, nil
}
