package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

type tokenRepo struct{ s *Store }

// Create implements storage.TokenRepository.
func (r tokenRepo) Create(ctx context.Context, t *storage.TokenRecord) error {
	_, err := r.s.writeConn().ExecContext(ctx,
		`INSERT INTO api_tokens (id, name, scope, hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Scope, t.Hash, toMillis(t.CreatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("storage: token %q: %w", t.Name, domain.AlreadyExists("token", t.Name))
		}
		return fmt.Errorf("storage: create token: %w", err)
	}
	return nil
}

// List implements storage.TokenRepository.
func (r tokenRepo) List(ctx context.Context) ([]*storage.TokenRecord, error) {
	rows, err := r.s.readConn().QueryContext(ctx, `SELECT id, name, scope, hash, created_at, last_used_at FROM api_tokens ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("storage: list tokens: %w", err)
	}
	defer rows.Close() //nolint:errcheck // the error is reported by rows.Err below
	var out []*storage.TokenRecord
	for rows.Next() {
		var (
			t        storage.TokenRecord
			created  int64
			lastUsed sql.NullInt64
		)
		if err := rows.Scan(&t.ID, &t.Name, &t.Scope, &t.Hash, &created, &lastUsed); err != nil {
			return nil, fmt.Errorf("storage: list tokens: %w", err)
		}
		t.CreatedAt = fromMillis(created)
		if lastUsed.Valid {
			at := fromMillis(lastUsed.Int64)
			t.LastUsedAt = &at
		}
		out = append(out, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list tokens: %w", err)
	}
	return out, nil
}

// Delete implements storage.TokenRepository.
func (r tokenRepo) Delete(ctx context.Context, id string) error {
	res, err := r.s.writeConn().ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("storage: delete token: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("storage: delete token: %w", domain.NotFound("token", id))
	}
	return nil
}

// Touch implements storage.TokenRepository.
func (r tokenRepo) Touch(ctx context.Context, id string, at time.Time) error {
	if _, err := r.s.writeConn().ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, toMillis(at), id); err != nil {
		return fmt.Errorf("storage: touch token: %w", err)
	}
	return nil
}
