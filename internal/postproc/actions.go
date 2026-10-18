package postproc

import (
	"context"
	"errors"
	"log/slog"
)

// Action is one kind of post action (see settings.PostAction).
type Action interface {
	// Run does the work with the parameters of the configured action. It
	// reports what it did, or why it could not, with Job.Warn; the error is
	// the reason the action failed.
	Run(ctx context.Context, j *Job, params map[string]string) error
}

// Actions is the step that runs the task's configured post actions in order. An
// action that fails does not fail the task and does not stop the next action:
// the download is complete and in place, so the failure is only recorded as a
// warning.
type Actions struct {
	kinds map[string]Action
	log   *slog.Logger
}

// NewActions returns the step for the given kinds of action.
func NewActions(log *slog.Logger, kinds map[string]Action) Actions {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return Actions{kinds: kinds, log: log}
}

// Name implements Step.
func (Actions) Name() string { return "actions" }

// Run implements Step.
func (a Actions) Run(ctx context.Context, j *Job) error {
	actions := j.Settings.PostActions.OrElse(nil)
	for _, pa := range actions {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // cancellation is reported as is
		}
		impl, ok := a.kinds[pa.Kind]
		if !ok {
			j.Note("%s: unknown post action", pa.Kind)
			continue
		}
		if err := impl.Run(ctx, j, pa.Params); err != nil {
			if ctx.Err() != nil {
				return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
			}
			var inf *InfectedError
			if errors.As(err, &inf) {
				return err // a flagged file must not complete
			}
			a.log.Warn("post action failed", "task_id", j.Task.ID, "action", pa.Kind, "error", err)
			j.Note("%s: %v", pa.Kind, err)
		}
	}
	return nil
}
