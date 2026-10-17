package service

import "context"

// Scheduler decides when tasks run. Services call it after state changes:
// Wake whenever admission may have changed (a task was queued, a queue was
// started, a limit changed) and Stop to interrupt a running task.
//
// The real implementation lives in internal/scheduler (P3.2); NopScheduler
// is a stand-in for tests and bootstrapping.
type Scheduler interface {
	// Wake asks the scheduler to re-evaluate admission soon. It never blocks.
	Wake()
	// Stop cancels the run of a running task with cause (ErrPaused or
	// ErrRequeued) and waits until the runner has persisted the final state
	// or ctx is done (then it returns an error wrapping ctx.Err()). It is a
	// no-op returning nil when the task is not running.
	Stop(ctx context.Context, taskID string, cause error) error
}

// NopScheduler is a Scheduler that never runs anything: Wake does nothing
// and Stop always reports the task as not running.
type NopScheduler struct{}

var _ Scheduler = NopScheduler{}

// Wake implements Scheduler.
func (NopScheduler) Wake() {}

// Stop implements Scheduler.
func (NopScheduler) Stop(context.Context, string, error) error { return nil }
