package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite/db"
)

// ---- settings ----

type settingsRepo struct{ s *Store }

func (r settingsRepo) GetGlobal(ctx context.Context) (settings.Settings, error) {
	data, err := r.s.read().GetGlobalSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return settings.Settings{}, nil
	}
	if err != nil {
		return settings.Settings{}, fmt.Errorf("storage: get global settings: %w", err)
	}
	return decodeSettings(data)
}

func (r settingsRepo) SaveGlobal(ctx context.Context, st settings.Settings) error {
	data, err := encodeSettings(st)
	if err != nil {
		return err
	}
	err = r.s.write().SaveGlobalSettings(ctx, db.SaveGlobalSettingsParams{
		Data:      data,
		UpdatedAt: toMillis(r.s.now()),
	})
	if err != nil {
		return fmt.Errorf("storage: save global settings: %w", err)
	}
	return nil
}

// ---- queues ----

type queueRepo struct{ s *Store }

func (r queueRepo) Create(ctx context.Context, q *domain.Queue) error {
	st, err := encodeSettings(q.Settings)
	if err != nil {
		return err
	}
	err = r.s.write().CreateQueue(ctx, db.CreateQueueParams{
		ID:        q.ID,
		Name:      q.Name,
		IsDefault: boolToInt(q.IsDefault),
		Position:  q.Position,
		State:     string(q.State),
		Settings:  st,
		CreatedAt: toMillis(q.CreatedAt),
		UpdatedAt: toMillis(q.UpdatedAt),
	})
	if err != nil {
		return queueWriteError("create", q, err)
	}
	return nil
}

func queueWriteError(op string, q *domain.Queue, err error) error {
	switch {
	case isPrimaryKeyViolation(err):
		return domain.AlreadyExists("queue", q.ID)
	case isUniqueViolation(err):
		return domain.AlreadyExists("queue name", q.Name)
	default:
		return fmt.Errorf("storage: %s queue %q: %w", op, q.ID, err)
	}
}

func (r queueRepo) Get(ctx context.Context, id string) (*domain.Queue, error) {
	row, err := r.s.read().GetQueue(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFound("queue", id)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get queue %q: %w", id, err)
	}
	return decodeQueue(row)
}

func (r queueRepo) List(ctx context.Context) ([]*domain.Queue, error) {
	rows, err := r.s.read().ListQueues(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: list queues: %w", err)
	}
	out := make([]*domain.Queue, 0, len(rows))
	for _, row := range rows {
		q, err := decodeQueue(row)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, nil
}

func (r queueRepo) Update(ctx context.Context, q *domain.Queue) error {
	st, err := encodeSettings(q.Settings)
	if err != nil {
		return err
	}
	n, err := r.s.write().UpdateQueue(ctx, db.UpdateQueueParams{
		ID:        q.ID,
		Name:      q.Name,
		IsDefault: boolToInt(q.IsDefault),
		Position:  q.Position,
		State:     string(q.State),
		Settings:  st,
		UpdatedAt: toMillis(q.UpdatedAt),
	})
	if err != nil {
		return queueWriteError("update", q, err)
	}
	if n == 0 {
		return domain.NotFound("queue", q.ID)
	}
	return nil
}

func (r queueRepo) Delete(ctx context.Context, id string) error {
	n, err := r.s.write().DeleteQueue(ctx, id)
	if isForeignKeyViolation(err) {
		return domain.Conflict("queue %q still has tasks", id)
	}
	if err != nil {
		return fmt.Errorf("storage: delete queue %q: %w", id, err)
	}
	if n == 0 {
		return domain.NotFound("queue", id)
	}
	return nil
}

func (r queueRepo) NextPosition(ctx context.Context) (int64, error) {
	n, err := r.s.read().NextQueuePosition(ctx)
	if err != nil {
		return 0, fmt.Errorf("storage: next queue position: %w", err)
	}
	return n, nil
}

// ---- tasks ----

type taskRepo struct{ s *Store }

func (r taskRepo) Create(ctx context.Context, t *domain.Task) error {
	enc, err := encodeTask(t)
	if err != nil {
		return err
	}
	err = r.s.write().CreateTask(ctx, db.CreateTaskParams{
		ID:          t.ID,
		QueueID:     t.QueueID,
		Status:      string(t.Status),
		Priority:    int64(t.Priority),
		Position:    t.Position,
		Engine:      t.Engine,
		Name:        t.Name,
		Dir:         t.Dir,
		Size:        t.Size,
		Downloaded:  t.Downloaded,
		Source:      enc.source,
		Resource:    enc.resource,
		Settings:    enc.settings,
		Labels:      enc.labels,
		Error:       enc.errJSON,
		CreatedAt:   toMillis(t.CreatedAt),
		UpdatedAt:   toMillis(t.UpdatedAt),
		StartedAt:   toNullMillis(t.StartedAt),
		CompletedAt: toNullMillis(t.CompletedAt),
	})
	if err != nil {
		return taskWriteError("create", t, err)
	}
	return nil
}

func taskWriteError(op string, t *domain.Task, err error) error {
	switch {
	case isUniqueViolation(err):
		return domain.AlreadyExists("task", t.ID)
	case isForeignKeyViolation(err):
		return domain.NotFound("queue", t.QueueID)
	default:
		return fmt.Errorf("storage: %s task %q: %w", op, t.ID, err)
	}
}

func (r taskRepo) Get(ctx context.Context, id string) (*domain.Task, error) {
	row, err := r.s.read().GetTask(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFound("task", id)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get task %q: %w", id, err)
	}
	return decodeTask(&row)
}

func (r taskRepo) Update(ctx context.Context, t *domain.Task) error {
	enc, err := encodeTask(t)
	if err != nil {
		return err
	}
	n, err := r.s.write().UpdateTask(ctx, db.UpdateTaskParams{
		ID:          t.ID,
		QueueID:     t.QueueID,
		Status:      string(t.Status),
		Priority:    int64(t.Priority),
		Position:    t.Position,
		Engine:      t.Engine,
		Name:        t.Name,
		Dir:         t.Dir,
		Size:        t.Size,
		Downloaded:  t.Downloaded,
		Source:      enc.source,
		Resource:    enc.resource,
		Settings:    enc.settings,
		Labels:      enc.labels,
		Error:       enc.errJSON,
		UpdatedAt:   toMillis(t.UpdatedAt),
		StartedAt:   toNullMillis(t.StartedAt),
		CompletedAt: toNullMillis(t.CompletedAt),
	})
	if err != nil {
		return taskWriteError("update", t, err)
	}
	if n == 0 {
		return domain.NotFound("task", t.ID)
	}
	return nil
}

func (r taskRepo) UpdateProgress(ctx context.Context, id string, downloaded, size int64, updatedAt time.Time) error {
	n, err := r.s.write().UpdateTaskProgress(ctx, db.UpdateTaskProgressParams{
		ID:         id,
		Downloaded: downloaded,
		Size:       size,
		UpdatedAt:  toMillis(updatedAt),
	})
	if err != nil {
		return fmt.Errorf("storage: update task %q progress: %w", id, err)
	}
	if n == 0 {
		return domain.NotFound("task", id)
	}
	return nil
}

func (r taskRepo) Delete(ctx context.Context, id string) error {
	n, err := r.s.write().DeleteTask(ctx, id)
	if err != nil {
		return fmt.Errorf("storage: delete task %q: %w", id, err)
	}
	if n == 0 {
		return domain.NotFound("task", id)
	}
	return nil
}

func (r taskRepo) NextPosition(ctx context.Context, queueID string) (int64, error) {
	n, err := r.s.read().NextTaskPosition(ctx, queueID)
	if err != nil {
		return 0, fmt.Errorf("storage: next task position in queue %q: %w", queueID, err)
	}
	return n, nil
}

func (r taskRepo) CountByStatus(ctx context.Context, queueID string) (map[domain.TaskStatus]int, error) {
	out := make(map[domain.TaskStatus]int)
	if queueID == "" {
		rows, err := r.s.read().CountTasksByStatus(ctx)
		if err != nil {
			return nil, fmt.Errorf("storage: count tasks: %w", err)
		}
		for _, row := range rows {
			out[domain.TaskStatus(row.Status)] = int(row.N)
		}
		return out, nil
	}
	rows, err := r.s.read().CountQueueTasksByStatus(ctx, queueID)
	if err != nil {
		return nil, fmt.Errorf("storage: count tasks in queue %q: %w", queueID, err)
	}
	for _, row := range rows {
		out[domain.TaskStatus(row.Status)] = int(row.N)
	}
	return out, nil
}

// ---- segments ----

type segmentRepo struct{ s *Store }

func (r segmentRepo) Replace(ctx context.Context, taskID string, segs []domain.Segment) error {
	return r.s.inTx(ctx, func(tx *Store) error {
		q := tx.write()
		if err := q.DeleteSegments(ctx, taskID); err != nil {
			return fmt.Errorf("storage: replace segments of task %q: %w", taskID, err)
		}
		for _, seg := range segs {
			err := q.InsertSegment(ctx, db.InsertSegmentParams{
				TaskID:    taskID,
				Idx:       int64(seg.Index),
				Start:     seg.Start,
				EndOffset: seg.End,
				Done:      seg.Done,
			})
			switch {
			case isForeignKeyViolation(err):
				return domain.NotFound("task", taskID)
			case isUniqueViolation(err):
				return domain.InvalidArgument("task %q: duplicate segment index %d", taskID, seg.Index)
			case err != nil:
				return fmt.Errorf("storage: replace segments of task %q: %w", taskID, err)
			}
		}
		return nil
	})
}

func (r segmentRepo) List(ctx context.Context, taskID string) ([]domain.Segment, error) {
	rows, err := r.s.read().ListSegments(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("storage: list segments of task %q: %w", taskID, err)
	}
	out := make([]domain.Segment, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.Segment{Index: int(row.Idx), Start: row.Start, End: row.End, Done: row.Done})
	}
	return out, nil
}

// ---- engine state ----

type engineStateRepo struct{ s *Store }

func (r engineStateRepo) Get(ctx context.Context, taskID string) ([]byte, error) {
	state, err := r.s.read().GetEngineState(ctx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFound("engine state", taskID)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get engine state of task %q: %w", taskID, err)
	}
	return state, nil
}

func (r engineStateRepo) Put(ctx context.Context, taskID string, state []byte) error {
	if state == nil {
		// The column is NOT NULL; store an empty blob for a nil slice.
		state = []byte{}
	}
	err := r.s.write().PutEngineState(ctx, db.PutEngineStateParams{
		TaskID:    taskID,
		State:     state,
		UpdatedAt: toMillis(r.s.now()),
	})
	if isForeignKeyViolation(err) {
		return domain.NotFound("task", taskID)
	}
	if err != nil {
		return fmt.Errorf("storage: put engine state of task %q: %w", taskID, err)
	}
	return nil
}

func (r engineStateRepo) Delete(ctx context.Context, taskID string) error {
	if err := r.s.write().DeleteEngineState(ctx, taskID); err != nil {
		return fmt.Errorf("storage: delete engine state of task %q: %w", taskID, err)
	}
	return nil
}

// ---- cookies ----

type cookieRepo struct{ s *Store }

func (r cookieRepo) Upsert(ctx context.Context, c *storage.CookieRecord) error {
	err := r.s.write().UpsertCookie(ctx, db.UpsertCookieParams{
		ID:           c.ID,
		Domain:       c.Domain,
		Name:         c.Name,
		Path:         c.Path,
		PartitionKey: c.PartitionKey,
		EncryptedVal: c.EncryptedVal,
		Secure:       boolToInt(c.Secure),
		HttpOnly:     boolToInt(c.HTTPOnly),
		SameSite:     c.SameSite,
		HostOnly:     boolToInt(c.HostOnly),
		ExpiresAt:    toNullMillis(c.ExpiresAt),
		CreatedAt:    toMillis(c.CreatedAt),
		UpdatedAt:    toMillis(c.UpdatedAt),
	})
	if err != nil {
		return fmt.Errorf("storage: upsert cookie %q: %w", c.ID, err)
	}
	return nil
}

func (r cookieRepo) Get(ctx context.Context, id string) (*storage.CookieRecord, error) {
	row, err := r.s.read().GetCookie(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.NotFound("cookie", id)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get cookie %q: %w", id, err)
	}
	return decodeCookie(row), nil
}

func (r cookieRepo) Delete(ctx context.Context, id string) error {
	n, err := r.s.write().DeleteCookie(ctx, id)
	if err != nil {
		return fmt.Errorf("storage: delete cookie %q: %w", id, err)
	}
	if n == 0 {
		return domain.NotFound("cookie", id)
	}
	return nil
}

func (r cookieRepo) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	n, err := r.s.write().DeleteExpiredCookies(ctx, sql.NullInt64{
		Int64: toMillis(now),
		Valid: true,
	})
	if err != nil {
		return 0, fmt.Errorf("storage: delete expired cookies: %w", err)
	}
	return n, nil
}

func (r cookieRepo) List(ctx context.Context, f storage.CookieFilter) ([]*storage.CookieRecord, error) {
	rows, err := r.s.read().ListCookies(ctx, db.ListCookiesParams{
		Domain:       f.Domain,
		Name:         f.Name,
		PartitionKey: f.PartitionKey,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: list cookies: %w", err)
	}
	out := make([]*storage.CookieRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, decodeCookie(row))
	}
	return out, nil
}

func decodeCookie(row db.Cookie) *storage.CookieRecord {
	return &storage.CookieRecord{
		ID:           row.ID,
		Domain:       row.Domain,
		Name:         row.Name,
		Path:         row.Path,
		PartitionKey: row.PartitionKey,
		EncryptedVal: row.EncryptedVal,
		Secure:       row.Secure != 0,
		HTTPOnly:     row.HttpOnly != 0,
		SameSite:     row.SameSite,
		HostOnly:     row.HostOnly != 0,
		ExpiresAt:    fromNullMillis(row.ExpiresAt),
		CreatedAt:    fromMillis(row.CreatedAt),
		UpdatedAt:    fromMillis(row.UpdatedAt),
	}
}

var (
	_ storage.SettingsRepository    = settingsRepo{}
	_ storage.QueueRepository       = queueRepo{}
	_ storage.TaskRepository        = taskRepo{}
	_ storage.SegmentRepository     = segmentRepo{}
	_ storage.EngineStateRepository = engineStateRepo{}
	_ storage.CookieRepository      = cookieRepo{}
)
