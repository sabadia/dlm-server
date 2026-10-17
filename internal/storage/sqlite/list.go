package sqlite

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite/db"
)

// taskColumns is the column list scanned by scanTask, in db.Task field order.
const taskColumns = `id, queue_id, status, priority, position, engine, name, dir, size, downloaded,
	source, resource, settings, labels, error, created_at, updated_at, started_at, completed_at`

// taskCursor is the decoded form of an opaque pagination cursor: the sort
// key of the last row on the previous page plus its ID as a tie-breaker.
type taskCursor struct {
	Sort      storage.TaskSort `json:"s"`
	Priority  int64            `json:"p,omitempty"`
	Position  int64            `json:"o,omitempty"`
	CreatedAt int64            `json:"c"`
	ID        string           `json:"i"`
}

func encodeCursor(c taskCursor) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("storage: encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeCursor(s string, sort storage.TaskSort) (taskCursor, error) {
	var c taskCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, domain.InvalidArgument("storage: invalid cursor")
	}
	if err := json.Unmarshal(b, &c); err != nil || c.ID == "" {
		return c, domain.InvalidArgument("storage: invalid cursor")
	}
	if c.Sort != sort {
		return c, domain.InvalidArgument("storage: cursor was issued for sort %q, not %q", c.Sort, sort)
	}
	return c, nil
}

// escapeLike escapes the LIKE wildcards in s using '\' as the escape character.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// List implements storage.TaskRepository using keyset pagination. Every
// user-supplied value is passed as a bound parameter; only fixed SQL
// fragments are concatenated.
func (r taskRepo) List(ctx context.Context, f storage.TaskFilter) (storage.TaskPage, error) {
	sort := f.Sort
	if sort == "" {
		sort = storage.TaskSortPosition
	}
	if sort != storage.TaskSortPosition && sort != storage.TaskSortCreated {
		return storage.TaskPage{}, domain.InvalidArgument("storage: unknown task sort %q", f.Sort)
	}
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = storage.DefaultTaskLimit
	case limit > storage.MaxTaskLimit:
		limit = storage.MaxTaskLimit
	}

	var (
		conds []string
		args  []any
	)
	if f.QueueID != "" {
		conds = append(conds, "queue_id = ?")
		args = append(args, f.QueueID)
	}
	if len(f.Statuses) > 0 {
		ph := make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			if !st.Valid() {
				return storage.TaskPage{}, domain.InvalidArgument("storage: unknown task status %q", st)
			}
			ph[i] = "?"
			args = append(args, string(st))
		}
		conds = append(conds, "status IN ("+strings.Join(ph, ", ")+")")
	}
	if f.Search != "" {
		pattern := "%" + escapeLike(f.Search) + "%"
		conds = append(conds, `(name LIKE ? ESCAPE '\' OR json_extract(source, '$.currentUrl') LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern)
	}
	if f.Cursor != "" {
		c, err := decodeCursor(f.Cursor, sort)
		if err != nil {
			return storage.TaskPage{}, err
		}
		switch sort {
		case storage.TaskSortCreated:
			conds = append(conds, "(created_at < ? OR (created_at = ? AND id < ?))")
			args = append(args, c.CreatedAt, c.CreatedAt, c.ID)
		default:
			conds = append(conds, `(priority < ?
				OR (priority = ? AND (position > ?
				OR (position = ? AND (created_at > ?
				OR (created_at = ? AND id > ?))))))`)
			args = append(args, c.Priority, c.Priority, c.Position, c.Position, c.CreatedAt, c.CreatedAt, c.ID)
		}
	}

	var q strings.Builder
	q.WriteString("SELECT ")
	q.WriteString(taskColumns)
	q.WriteString(" FROM tasks")
	if len(conds) > 0 {
		q.WriteString(" WHERE ")
		q.WriteString(strings.Join(conds, " AND "))
	}
	if sort == storage.TaskSortCreated {
		q.WriteString(" ORDER BY created_at DESC, id DESC")
	} else {
		q.WriteString(" ORDER BY priority DESC, position ASC, created_at ASC, id ASC")
	}
	q.WriteString(" LIMIT ?")
	args = append(args, limit+1)

	rows, err := r.s.readConn().QueryContext(ctx, q.String(), args...)
	if err != nil {
		return storage.TaskPage{}, fmt.Errorf("storage: list tasks: %w", err)
	}
	defer rows.Close() //nolint:errcheck // the error is reported by rows.Err below

	raw := make([]db.Task, 0, limit+1)
	for rows.Next() {
		var t db.Task
		if err := rows.Scan(
			&t.ID, &t.QueueID, &t.Status, &t.Priority, &t.Position, &t.Engine, &t.Name, &t.Dir,
			&t.Size, &t.Downloaded, &t.Source, &t.Resource, &t.Settings, &t.Labels, &t.Error,
			&t.CreatedAt, &t.UpdatedAt, &t.StartedAt, &t.CompletedAt,
		); err != nil {
			return storage.TaskPage{}, fmt.Errorf("storage: list tasks: %w", err)
		}
		raw = append(raw, t)
	}
	if err := rows.Err(); err != nil {
		return storage.TaskPage{}, fmt.Errorf("storage: list tasks: %w", err)
	}

	var page storage.TaskPage
	if len(raw) > limit {
		raw = raw[:limit]
		last := raw[limit-1]
		c := taskCursor{Sort: sort, CreatedAt: last.CreatedAt, ID: last.ID}
		if sort == storage.TaskSortPosition {
			c.Priority, c.Position = last.Priority, last.Position
		}
		if page.NextCursor, err = encodeCursor(c); err != nil {
			return storage.TaskPage{}, err
		}
	}
	page.Tasks = make([]*domain.Task, 0, len(raw))
	for i := range raw {
		t, err := decodeTask(&raw[i])
		if err != nil {
			return storage.TaskPage{}, err
		}
		page.Tasks = append(page.Tasks, t)
	}
	return page, nil
}
