package ratelimit

import (
	"sync"

	"github.com/sabadia/dlm-server/internal/engine"
)

// Manager owns the global bucket and the per-queue and per-task buckets of
// a daemon. Queue and task buckets are created on demand (unlimited) and
// must be removed with RemoveQueue / RemoveTask once their owner is gone,
// otherwise they are kept forever. A Manager is safe for concurrent use.
type Manager struct {
	clk    clock
	global *Bucket

	mu     sync.Mutex
	queues map[string]*Bucket
	tasks  map[string]*Bucket
}

// NewManager returns a manager whose global bucket is limited to
// globalBytesPerSec (0 = unlimited).
func NewManager(globalBytesPerSec int64) *Manager {
	return newManager(globalBytesPerSec, realClock{})
}

func newManager(globalBytesPerSec int64, clk clock) *Manager {
	return &Manager{
		clk:    clk,
		global: newBucket(globalBytesPerSec, clk),
		queues: make(map[string]*Bucket),
		tasks:  make(map[string]*Bucket),
	}
}

// Global returns the global bucket.
func (m *Manager) Global() *Bucket { return m.global }

// Queue returns the bucket of queue id, creating an unlimited one if needed.
func (m *Manager) Queue(id string) *Bucket { return m.bucket(m.queues, id) }

// Task returns the bucket of task id, creating an unlimited one if needed.
func (m *Manager) Task(id string) *Bucket { return m.bucket(m.tasks, id) }

// SetGlobal changes the global limit (bytes per second, 0 = unlimited).
func (m *Manager) SetGlobal(bytesPerSec int64) { m.global.SetLimit(bytesPerSec) }

// SetQueue changes the limit of queue id, creating its bucket if needed.
func (m *Manager) SetQueue(id string, bytesPerSec int64) { m.Queue(id).SetLimit(bytesPerSec) }

// SetTask changes the limit of task id, creating its bucket if needed.
func (m *Manager) SetTask(id string, bytesPerSec int64) { m.Task(id).SetLimit(bytesPerSec) }

// RemoveQueue forgets the bucket of queue id. Limiters already handed out
// keep using the old bucket; a later Queue(id) creates a fresh one.
func (m *Manager) RemoveQueue(id string) { m.remove(m.queues, id) }

// RemoveTask forgets the bucket of task id. Limiters already handed out
// keep using the old bucket; a later Task(id) creates a fresh one.
func (m *Manager) RemoveTask(id string) { m.remove(m.tasks, id) }

// ForTask returns the limiter for a task: its bytes pass the task bucket,
// then the queue bucket, then the global bucket. An empty taskID or queueID
// skips that level.
func (m *Manager) ForTask(taskID, queueID string) engine.Limiter {
	var task, queue *Bucket
	if taskID != "" {
		task = m.Task(taskID)
	}
	if queueID != "" {
		queue = m.Queue(queueID)
	}
	return Chain(task, queue, m.global)
}

func (m *Manager) bucket(set map[string]*Bucket, id string) *Bucket {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := set[id]
	if !ok {
		b = newBucket(0, m.clk)
		set[id] = b
	}
	return b
}

func (m *Manager) remove(set map[string]*Bucket, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(set, id)
}
