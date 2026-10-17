// Package scheduler admits queued tasks for execution, enforcing global and
// per-queue concurrency and priorities. It implements service.Scheduler and
// drives a task runner (service.Runner in the daemon).
//
// # Admission
//
// An admission pass runs when the scheduler is woken (Wake), when a run
// ends, and periodically (Options.Tick). It loads the queues and skips the
// stopped ones. For every running queue the free slots are the queue's
// effective maxConcurrentDownloads (Defaults ⊕ Global ⊕ Queue) minus the
// tasks the scheduler itself is running from that queue; the scheduler's
// in-memory running set is authoritative, not the stored statuses. The
// queue's queued tasks are fetched in queue order (priority descending,
// then position and creation time ascending), at most as many as it has
// free slots. When the effective maxActiveDownloadsTotal is positive it
// caps the running tasks across all queues, and the free global slots are
// handed out round-robin over the queues in queue order, one task per
// queue per round, so the first queue cannot take them all.
//
// Task states are read when a pass runs, and the runner re-checks the
// task under its lock before starting it, so a task paused, deleted or
// moved to a stopped queue in between is never started.
//
// # Limits changes and preemption
//
// Lowering a concurrency limit never preempts running tasks: it only
// affects later admissions, so the number of running tasks drains down to
// the new limit as runs end. Preempting a running task for a higher
// priority one is not done either; priority only orders admission.
//
// # Shutdown
//
// When the context given to Run is cancelled, every running task is stopped
// with cause service.ErrRequeued (the runner leaves it queued, so it resumes
// on the next start) and Run returns once all runs have ended.
package scheduler
