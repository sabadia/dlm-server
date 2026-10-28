package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/platform"
	"github.com/sabadia/dlm-server/internal/settings"
)

// CommandTimeout bounds a post-queue "command" action.
const CommandTimeout = 10 * time.Minute

// maxLoggedOutput bounds the command output written to the log.
const maxLoggedOutput = 4 << 10

// Reasons of queue.action_cancelled events.
const (
	CancelNewWork         = "new_work"
	CancelQueueStopped    = "queue_stopped"
	CancelQueueDeleted    = "queue_deleted"
	CancelSettingsChanged = "settings_changed"
)

// errNoExecutor is logged when an action needs an executor that was not
// configured.
var errNoExecutor = errors.New("scheduler: no executor configured for this action")

// ActionExecutor performs the machine-level post-queue actions. The daemon
// uses SystemActions; tests inject a fake so the test machine is never
// suspended or powered off.
type ActionExecutor interface {
	// Sleep suspends the machine.
	Sleep(ctx context.Context) error
	// Shutdown powers the machine off.
	Shutdown(ctx context.Context) error
	// Command runs the executable name with args directly (never through
	// a shell) and returns its combined output.
	Command(ctx context.Context, name string, args []string) ([]byte, error)
}

// SystemActions is the ActionExecutor of the real machine: sleep and
// shutdown use internal/platform, commands run via os/exec without a
// shell.
type SystemActions struct{}

var _ ActionExecutor = SystemActions{}

// Sleep implements ActionExecutor.
func (SystemActions) Sleep(ctx context.Context) error {
	return platform.Sleep(ctx) //nolint:wrapcheck // platform errors carry context
}

// Shutdown implements ActionExecutor.
func (SystemActions) Shutdown(ctx context.Context) error {
	return platform.Shutdown(ctx) //nolint:wrapcheck // platform errors carry context
}

// Command implements ActionExecutor.
func (SystemActions) Command(ctx context.Context, name string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the user configured this executable through the authenticated API; no shell is involved
	cmd.WaitDelay = 5 * time.Second                // do not hang on pipes held by grandchildren after cancellation
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("scheduler: run command: %w", err)
	}
	return out, nil
}

// QueueCompletedData is the payload of queue.completed events.
type QueueCompletedData = events.QueueCompletedData

// ActionCancelledData is the payload of queue.action_cancelled events.
type ActionCancelledData = events.ActionCancelledData

// pendingAction is a post-queue action waiting for its delay to elapse.
type pendingAction struct {
	action settings.QueueCompleteAction
	due    time.Time
}

// queueActions detects queues that ran out of work and schedules, cancels
// or executes their post-queue actions (see the package documentation). It
// runs on the Run goroutine, which owns s.pending.
func (s *Scheduler) queueActions(ctx context.Context, st *passState, now time.Time) {
	s.mu.Lock()
	runningPer := make(map[string]int)
	for _, r := range s.running {
		runningPer[r.queueID]++
	}
	completed := make(map[string]int, len(s.completed))
	for id, n := range s.completed {
		completed[id] = n
	}
	s.mu.Unlock()

	byID := make(map[string]*domain.Queue, len(st.queues))
	for _, q := range st.queues {
		byID[q.ID] = q
	}
	hasWork := func(id string) (bool, error) {
		if runningPer[id] > 0 {
			return true, nil
		}
		return s.hasQueued(ctx, id)
	}

	for id, p := range s.pending {
		q := byID[id]
		reason := ""
		switch act, ok := queueAction(q); {
		case q == nil:
			reason = CancelQueueDeleted
		case q.State != domain.QueueRunning:
			reason = CancelQueueStopped
		case !ok || act.Action != p.action.Action:
			reason = CancelSettingsChanged
		default:
			busy, err := hasWork(id)
			if err != nil {
				s.o.Logger.ErrorContext(ctx, "scheduler: check queue work", "queue_id", id, "error", err)
				continue
			}
			if busy {
				reason = CancelNewWork
			}
		}
		if reason != "" {
			delete(s.pending, id)
			s.o.Logger.InfoContext(ctx, "post-queue action cancelled", "queue_id", id, "action", p.action.Action, "reason", reason)
			s.publish(events.TypeQueueActionCancelled, id, ActionCancelledData{Action: p.action.Action, Reason: reason})
			continue
		}
		if !now.Before(p.due) {
			delete(s.pending, id)
			s.execute(ctx, id, p.action)
		}
	}

	for id, n := range completed {
		q := byID[id]
		if q == nil {
			s.resetCompleted(id, n)
			continue
		}
		busy, err := hasWork(id)
		if err != nil {
			s.o.Logger.ErrorContext(ctx, "scheduler: check queue work", "queue_id", id, "error", err)
			continue
		}
		if busy {
			continue
		}
		// The queue ran out of work: this activation is over.
		s.resetCompleted(id, n)
		act, ok := queueAction(q)
		if !ok || q.State != domain.QueueRunning || s.pending[id] != nil {
			continue
		}
		due := now.Add(act.EffectiveDelay())
		s.pending[id] = &pendingAction{action: act, due: due}
		s.o.Logger.InfoContext(ctx, "queue completed", "queue_id", id, "completed_tasks", n, "action", act.Action, "execute_at", due)
		s.publish(events.TypeQueueCompleted, id, QueueCompletedData{CompletedTasks: n, Action: act.Action, ExecuteAt: due.UTC()})
	}
}

// queueAction returns the queue's configured post-queue action; false when
// there is none (unset, "none" or a nil queue).
func queueAction(q *domain.Queue) (settings.QueueCompleteAction, bool) {
	if q == nil {
		return settings.QueueCompleteAction{}, false
	}
	act, ok := q.Settings.OnQueueComplete.Get()
	if !ok || act.Action == settings.QueueActionNone || act.Action == "" {
		return settings.QueueCompleteAction{}, false
	}
	return act, true
}

// resetCompleted subtracts the n completions a pass has accounted for
// (completions recorded meanwhile are kept for the next pass).
func (s *Scheduler) resetCompleted(queueID string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completed[queueID] -= n; s.completed[queueID] <= 0 {
		delete(s.completed, queueID)
	}
}

// hasQueued reports whether the queue has a queued task.
func (s *Scheduler) hasQueued(ctx context.Context, queueID string) (bool, error) {
	ids, err := s.queued(ctx, queueID, 1, nil)
	if err != nil {
		return false, err
	}
	return len(ids) > 0, nil
}

// execute runs a due post-queue action in an owned goroutine; ctx is the
// Run context, so shutting the scheduler down cancels a running command.
func (s *Scheduler) execute(ctx context.Context, queueID string, act settings.QueueCompleteAction) {
	log := s.o.Logger.With("queue_id", queueID, "action", act.Action)
	s.wg.Go(func() {
		log.InfoContext(ctx, "running post-queue action")
		var err error
		switch act.Action {
		case settings.QueueActionStop:
			if s.o.StopQueue == nil {
				err = errNoExecutor
				break
			}
			err = s.o.StopQueue(ctx, queueID)
		case settings.QueueActionSleep:
			if s.o.Actions == nil {
				err = errNoExecutor
				break
			}
			err = s.o.Actions.Sleep(ctx)
		case settings.QueueActionShutdown:
			if s.o.Actions == nil {
				err = errNoExecutor
				break
			}
			err = s.o.Actions.Shutdown(ctx)
		case settings.QueueActionCommand:
			err = s.runCommand(ctx, log, act)
		default:
			err = fmt.Errorf("scheduler: unknown post-queue action %q", act.Action)
		}
		if err != nil {
			log.ErrorContext(ctx, "post-queue action failed", "error", err)
			return
		}
		log.InfoContext(ctx, "post-queue action done")
	})
}

// runCommand runs the "command" action with CommandTimeout. Only the
// executable path and the number of arguments are logged (arguments may
// hold secrets), plus the beginning of the output.
func (s *Scheduler) runCommand(ctx context.Context, log *slog.Logger, act settings.QueueCompleteAction) error {
	if s.o.Actions == nil {
		return errNoExecutor
	}
	cctx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()
	out, err := s.o.Actions.Command(cctx, act.Command, act.Args)
	output := strings.TrimSpace(string(out))
	if len(output) > maxLoggedOutput {
		output = output[:maxLoggedOutput] + "…"
	}
	log.InfoContext(ctx, "post-queue command finished", "command", act.Command, "args", len(act.Args), "output", output, "ok", err == nil)
	return err
}

// publish sends a queue-scoped event when a bus is configured.
func (s *Scheduler) publish(typ events.Type, queueID string, data any) {
	if s.o.Bus == nil {
		return
	}
	s.o.Bus.Publish(events.Event{Type: typ, QueueID: queueID, Data: data})
}
