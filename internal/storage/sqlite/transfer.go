package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/sabadia/dlm-server/internal/storage"
)

type transferRepo struct{ s *Store }

const hourMillis = int64(time.Hour / time.Millisecond)

// Add implements storage.TransferRepository.
func (r transferRepo) Add(ctx context.Context, rows []storage.TransferRow) error {
	if len(rows) == 0 {
		return nil
	}
	conn := r.s.writeConn()
	for _, row := range rows {
		if _, err := conn.ExecContext(ctx, `INSERT INTO transfer_buckets (bucket, queue_id, engine, down, up)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (bucket, queue_id, engine) DO UPDATE SET down = down + excluded.down, up = up + excluded.up`,
			toMillis(row.Bucket), row.QueueID, row.Engine, row.Down, row.Up); err != nil {
			return fmt.Errorf("storage: add transfer: %w", err)
		}
	}
	return nil
}

// Timeline implements storage.TransferRepository.
func (r transferRepo) Timeline(ctx context.Context, from, to time.Time, bucketSize time.Duration, group storage.TransferGroup) ([]storage.TransferPoint, error) {
	size := int64(bucketSize / time.Millisecond)
	if size < hourMillis || size%hourMillis != 0 {
		return nil, fmt.Errorf("storage: timeline bucket %v is not a whole number of hours", bucketSize)
	}
	groupCol := "''"
	switch group {
	case storage.TransferGroupNone:
	case storage.TransferGroupEngine:
		groupCol = "engine"
	case storage.TransferGroupQueue:
		groupCol = "queue_id"
	default:
		return nil, fmt.Errorf("storage: unknown timeline grouping %q", group)
	}
	// Buckets are aligned to the Unix epoch, which is midnight UTC.
	q := fmt.Sprintf(`SELECT (bucket / ?) * ? AS b, %s AS g, SUM(down), SUM(up)
		FROM transfer_buckets WHERE bucket >= ? AND bucket < ?
		GROUP BY b, g ORDER BY b, g`, groupCol)
	rows, err := r.s.readConn().QueryContext(ctx, q, size, size, toMillis(from), toMillis(to))
	if err != nil {
		return nil, fmt.Errorf("storage: timeline: %w", err)
	}
	defer rows.Close() //nolint:errcheck // the error is reported by rows.Err below
	var out []storage.TransferPoint
	for rows.Next() {
		var (
			b, down, up int64
			g           string
		)
		if err := rows.Scan(&b, &g, &down, &up); err != nil {
			return nil, fmt.Errorf("storage: timeline: %w", err)
		}
		out = append(out, storage.TransferPoint{Bucket: fromMillis(b), Group: g, Down: down, Up: up})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: timeline: %w", err)
	}
	return out, nil
}
