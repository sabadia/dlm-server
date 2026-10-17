package service

import (
	"context"
	"fmt"
	"sync"
)

// TaskLocks is a keyed mutex: one lock per task ID. The Runner and the
// services share one instance (through RunnerDeps.TaskLocks and
// Deps.TaskLocks) so that every read-modify-write of a task — a status
// transition or any other mutation — happens under that task's lock: the
// holder re-reads the task from the store, applies the domain change and
// persists it before releasing the lock. Without it a service mutation and
// a runner transition could both read the same version and one update
// would be lost (for example Pause racing with the scheduler starting the
// task).
//
// Lock ordering rules:
//   - a task lock is taken before a store transaction (WithTx), never inside
//     one;
//   - a goroutine holds one task lock at a time, except the multi-task
//     paths (task reorder, moving a deleted queue's tasks), which take all
//     their locks in ascending ID order before doing anything else;
//   - a task lock is never held while calling Scheduler.Stop or waiting for
//     a run to end: the runner needs the lock to persist the task's final
//     state, so waiting while holding it would deadlock.
//
// Entries exist only while a lock is held or waited for, so the map does not
// grow with the number of tasks ever seen. The zero value is not usable;
// create instances with NewTaskLocks.
type TaskLocks struct {
	mu    sync.Mutex
	locks map[string]*taskLock
}

type taskLock struct {
	sem  chan struct{} // capacity 1: holding the token is holding the lock
	refs int           // holders + waiters; guarded by TaskLocks.mu
}

// NewTaskLocks creates an empty TaskLocks.
func NewTaskLocks() *TaskLocks {
	return &TaskLocks{locks: make(map[string]*taskLock)}
}

// Lock acquires the lock of taskID, waiting until it is free or ctx is
// done (then it returns an error wrapping ctx.Err()). On success the
// returned function releases the lock; it must be called exactly once.
func (l *TaskLocks) Lock(ctx context.Context, taskID string) (unlock func(), err error) {
	l.mu.Lock()
	tl := l.locks[taskID]
	if tl == nil {
		tl = &taskLock{sem: make(chan struct{}, 1)}
		l.locks[taskID] = tl
	}
	tl.refs++
	l.mu.Unlock()

	select {
	case tl.sem <- struct{}{}:
		return func() {
			<-tl.sem
			l.release(taskID, tl)
		}, nil
	case <-ctx.Done():
		l.release(taskID, tl)
		return nil, fmt.Errorf("service: lock task %q: %w", taskID, ctx.Err())
	}
}

// release drops one reference and forgets the entry when it was the last.
func (l *TaskLocks) release(taskID string, tl *taskLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tl.refs--
	if tl.refs == 0 {
		delete(l.locks, taskID)
	}
}

// size returns the number of live entries (for tests).
func (l *TaskLocks) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.locks)
}
