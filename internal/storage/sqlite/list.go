package sqlite

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	Sort storage.TaskSort `json:"s"`
	Desc bool             `json:"d,omitempty"`
	// Priority, Position and CreatedAt are the keys of the position order.
	Priority  int64 `json:"p,omitempty"`
	Position  int64 `json:"o,omitempty"`
	CreatedAt int64 `json:"c,omitempty"`
	// Num or Str is the key of the other orders (a number, or the name).
	Num int64  `json:"n,omitempty"`
	Str string `json:"k,omitempty"`
	ID  string `json:"i"`
}

func encodeCursor(c taskCursor) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("storage: encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeCursor(s string, sort storage.TaskSort, desc bool) (taskCursor, error) {
	var c taskCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, domain.InvalidArgument("storage: invalid cursor")
	}
	if err := json.Unmarshal(b, &c); err != nil || c.ID == "" {
		return c, domain.InvalidArgument("storage: invalid cursor")
	}
	if c.Sort != sort || c.Desc != desc {
		return c, domain.InvalidArgument("storage: cursor was issued for another sort or order than %q", sort)
	}
	return c, nil
}

// sortKey is the SQL expression of a sort order's key and its default
// direction.
func sortKey(sort storage.TaskSort) (expr string, defaultDesc, ok bool) {
	switch sort {
	case storage.TaskSortCreated:
		return "created_at", true, true
	case storage.TaskSortName:
		return "name COLLATE NOCASE", false, true
	case storage.TaskSortSize:
		return "size", false, true
	case storage.TaskSortCompleted:
		return "COALESCE(completed_at, 0)", false, true
	default:
		return "", false, false
	}
}

// escapeLike escapes the LIKE wildcards in s using '\' as the escape character.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// filterConds turns the filters of f (all but the cursor) into WHERE
// conditions with their bound arguments.
func filterConds(f storage.TaskFilter) (conds []string, args []any, err error) {
	if f.QueueID != "" {
		conds = append(conds, "queue_id = ?")
		args = append(args, f.QueueID)
	}
	if len(f.Statuses) > 0 {
		ph := make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			if !st.Valid() {
				return nil, nil, domain.InvalidArgument("storage: unknown task status %q", st)
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
	if f.Engine != "" {
		conds = append(conds, "engine = ?")
		args = append(args, f.Engine)
	}
	if f.Label != "" {
		key, value, exact := strings.Cut(f.Label, "=")
		if exact {
			conds = append(conds, "EXISTS (SELECT 1 FROM json_each(labels) WHERE key = ? AND value = ?)")
			args = append(args, key, value)
		} else {
			conds = append(conds, "EXISTS (SELECT 1 FROM json_each(labels) WHERE key = ?)")
			args = append(args, key)
		}
	}
	if len(f.NameSuffixes) > 0 || len(f.ContentTypes) > 0 {
		var alt []string
		for _, suf := range f.NameSuffixes {
			alt = append(alt, `lower(name) LIKE ? ESCAPE '\'`)
			args = append(args, "%"+escapeLike(strings.ToLower(suf)))
		}
		for _, ct := range f.ContentTypes {
			ct = strings.ToLower(ct)
			if prefix, wild := strings.CutSuffix(ct, "/*"); wild {
				alt = append(alt, `lower(json_extract(source, '$.fingerprint.contentType')) LIKE ? ESCAPE '\'`)
				args = append(args, escapeLike(prefix)+"/%")
			} else {
				alt = append(alt, `lower(json_extract(source, '$.fingerprint.contentType')) = ?`)
				args = append(args, ct)
			}
		}
		conds = append(conds, "("+strings.Join(alt, " OR ")+")")
	}
	if f.MinSize != nil {
		conds = append(conds, "size >= 0 AND size >= ?")
		args = append(args, *f.MinSize)
	}
	if f.MaxSize != nil {
		conds = append(conds, "size >= 0 AND size <= ?")
		args = append(args, *f.MaxSize)
	}
	for _, b := range []struct {
		col  string
		op   string
		when time.Time
	}{
		{"created_at", ">=", f.CreatedAfter},
		{"created_at", "<=", f.CreatedBefore},
		{"completed_at", ">=", f.CompletedAfter},
		{"completed_at", "<=", f.CompletedBefore},
	} {
		if !b.when.IsZero() {
			conds = append(conds, b.col+" "+b.op+" ?")
			args = append(args, toMillis(b.when))
		}
	}
	return conds, args, nil
}

// List implements storage.TaskRepository using keyset pagination. Every
// user-supplied value is passed as a bound parameter; only fixed SQL
// fragments are concatenated.
func (r taskRepo) List(ctx context.Context, f storage.TaskFilter) (storage.TaskPage, error) {
	sort := f.Sort
	if sort == "" {
		sort = storage.TaskSortPosition
	}
	keyExpr, desc, known := sortKey(sort)
	if sort != storage.TaskSortPosition && !known {
		return storage.TaskPage{}, domain.InvalidArgument("storage: unknown task sort %q", f.Sort)
	}
	switch f.Order {
	case "":
	case storage.OrderAsc, storage.OrderDesc:
		if sort == storage.TaskSortPosition {
			return storage.TaskPage{}, domain.InvalidArgument("storage: the position order has one direction")
		}
		desc = f.Order == storage.OrderDesc
	default:
		return storage.TaskPage{}, domain.InvalidArgument("storage: unknown sort order %q", f.Order)
	}
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = storage.DefaultTaskLimit
	case limit > storage.MaxTaskLimit:
		limit = storage.MaxTaskLimit
	}

	conds, args, err := filterConds(f)
	if err != nil {
		return storage.TaskPage{}, err
	}
	var page storage.TaskPage
	if f.WithTotal {
		q := "SELECT COUNT(*) FROM tasks"
		if len(conds) > 0 {
			q += " WHERE " + strings.Join(conds, " AND ")
		}
		if err := r.s.readConn().QueryRowContext(ctx, q, args...).Scan(&page.Total); err != nil {
			return storage.TaskPage{}, fmt.Errorf("storage: count tasks: %w", err)
		}
	}
	if f.Cursor != "" {
		c, err := decodeCursor(f.Cursor, sort, desc)
		if err != nil {
			return storage.TaskPage{}, err
		}
		if sort == storage.TaskSortPosition {
			conds = append(conds, `(priority < ?
				OR (priority = ? AND (position > ?
				OR (position = ? AND (created_at > ?
				OR (created_at = ? AND id > ?))))))`)
			args = append(args, c.Priority, c.Priority, c.Position, c.Position, c.CreatedAt, c.CreatedAt, c.ID)
		} else {
			op := ">"
			if desc {
				op = "<"
			}
			var key any = c.Num
			if sort == storage.TaskSortName {
				key = c.Str
			}
			conds = append(conds, fmt.Sprintf("(%s %s ? OR (%s = ? AND id %s ?))", keyExpr, op, keyExpr, op))
			args = append(args, key, key, c.ID)
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
	if sort == storage.TaskSortPosition {
		q.WriteString(" ORDER BY priority DESC, position ASC, created_at ASC, id ASC")
	} else {
		dir := "ASC"
		if desc {
			dir = "DESC"
		}
		q.WriteString(" ORDER BY " + keyExpr + " " + dir + ", id " + dir)
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

	if len(raw) > limit {
		raw = raw[:limit]
		last := raw[limit-1]
		c := taskCursor{Sort: sort, Desc: desc, ID: last.ID}
		switch sort {
		case storage.TaskSortPosition:
			c.Priority, c.Position, c.CreatedAt = last.Priority, last.Position, last.CreatedAt
		case storage.TaskSortName:
			c.Str = last.Name
		case storage.TaskSortSize:
			c.Num = last.Size
		case storage.TaskSortCompleted:
			c.Num = last.CompletedAt.Int64 // NULL (not completed) counts as 0
		default:
			c.Num = last.CreatedAt
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
