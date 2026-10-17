package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage/sqlite/db"
)

// toMillis converts t to Unix milliseconds (UTC).
func toMillis(t time.Time) int64 { return t.UnixMilli() }

// fromMillis converts Unix milliseconds to a UTC time.
func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func toNullMillis(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: toMillis(*t), Valid: true}
}

func fromNullMillis(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMillis(v.Int64)
	return &t
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func marshalJSON(what string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("storage: encode %s: %w", what, err)
	}
	return string(b), nil
}

func unmarshalJSON(what, data string, v any) error {
	if err := json.Unmarshal([]byte(data), v); err != nil {
		return fmt.Errorf("storage: decode %s: %w", what, err)
	}
	return nil
}

// marshalNullJSON encodes v, or NULL when isNil.
func marshalNullJSON(what string, v any, isNil bool) (sql.NullString, error) {
	if isNil {
		return sql.NullString{}, nil
	}
	s, err := marshalJSON(what, v)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: s, Valid: true}, nil
}

func encodeSettings(s settings.Settings) (string, error) { return marshalJSON("settings", s) }

func decodeSettings(data string) (settings.Settings, error) {
	var s settings.Settings
	err := unmarshalJSON("settings", data, &s)
	return s, err
}

func decodeQueue(r db.Queue) (*domain.Queue, error) {
	s, err := decodeSettings(r.Settings)
	if err != nil {
		return nil, fmt.Errorf("storage: queue %q: %w", r.ID, err)
	}
	return &domain.Queue{
		ID:        r.ID,
		Name:      r.Name,
		IsDefault: r.IsDefault != 0,
		Position:  r.Position,
		State:     domain.QueueState(r.State),
		Settings:  s,
		CreatedAt: fromMillis(r.CreatedAt),
		UpdatedAt: fromMillis(r.UpdatedAt),
	}, nil
}

// taskRow holds the encoded JSON columns of a task.
type taskRow struct {
	source   string
	resource sql.NullString
	settings string
	labels   string
	errJSON  sql.NullString
}

func encodeTask(t *domain.Task) (taskRow, error) {
	var (
		r   taskRow
		err error
	)
	if r.source, err = marshalJSON("source", t.Source); err != nil {
		return r, err
	}
	if r.resource, err = marshalNullJSON("resource", t.Resource, t.Resource == nil); err != nil {
		return r, err
	}
	if r.settings, err = encodeSettings(t.Settings); err != nil {
		return r, err
	}
	labels := t.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	if r.labels, err = marshalJSON("labels", labels); err != nil {
		return r, err
	}
	if r.errJSON, err = marshalNullJSON("error", t.Error, t.Error == nil); err != nil {
		return r, err
	}
	return r, nil
}

func decodeTask(r *db.Task) (*domain.Task, error) {
	t := &domain.Task{
		ID:          r.ID,
		QueueID:     r.QueueID,
		Status:      domain.TaskStatus(r.Status),
		Priority:    domain.Priority(r.Priority),
		Position:    r.Position,
		Engine:      r.Engine,
		Name:        r.Name,
		Dir:         r.Dir,
		Size:        r.Size,
		Downloaded:  r.Downloaded,
		CreatedAt:   fromMillis(r.CreatedAt),
		UpdatedAt:   fromMillis(r.UpdatedAt),
		StartedAt:   fromNullMillis(r.StartedAt),
		CompletedAt: fromNullMillis(r.CompletedAt),
	}
	if err := unmarshalJSON("source", r.Source, &t.Source); err != nil {
		return nil, fmt.Errorf("storage: task %q: %w", r.ID, err)
	}
	if r.Resource.Valid {
		t.Resource = new(domain.Resource)
		if err := unmarshalJSON("resource", r.Resource.String, t.Resource); err != nil {
			return nil, fmt.Errorf("storage: task %q: %w", r.ID, err)
		}
	}
	s, err := decodeSettings(r.Settings)
	if err != nil {
		return nil, fmt.Errorf("storage: task %q: %w", r.ID, err)
	}
	t.Settings = s
	if err := unmarshalJSON("labels", r.Labels, &t.Labels); err != nil {
		return nil, fmt.Errorf("storage: task %q: %w", r.ID, err)
	}
	if len(t.Labels) == 0 {
		// Empty and nil label maps are equivalent; normalise to nil.
		t.Labels = nil
	}
	if r.Error.Valid {
		t.Error = new(domain.TaskError)
		if err := unmarshalJSON("error", r.Error.String, t.Error); err != nil {
			return nil, fmt.Errorf("storage: task %q: %w", r.ID, err)
		}
	}
	return t, nil
}
