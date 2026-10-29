package service

import (
	"context"
	"fmt"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

// MaxTimelineBuckets bounds how many buckets one Timeline call may span.
const MaxTimelineBuckets = 1000

// Timeline returns the bytes the daemon transferred between from (inclusive)
// and to (exclusive), summed into buckets of an hour or a day (aligned to
// UTC), oldest first, optionally split by engine or queue. Buckets nothing
// was transferred in are left out. A range of more than MaxTimelineBuckets
// buckets, an empty range or another bucket size is domain.ErrInvalidArgument.
func (s *TaskService) Timeline(ctx context.Context, from, to time.Time, bucket time.Duration, group storage.TransferGroup) ([]storage.TransferPoint, error) {
	if bucket != time.Hour && bucket != 24*time.Hour {
		return nil, domain.InvalidArgumentCode("stats.bad_bucket", nil, "service: timeline: buckets are an hour or a day")
	}
	if !to.After(from) {
		return nil, domain.InvalidArgumentCode("stats.bad_range", nil, "service: timeline: the range is empty")
	}
	if to.Sub(from)/bucket > MaxTimelineBuckets {
		return nil, domain.InvalidArgumentCode("stats.range_too_large", map[string]string{"max": fmt.Sprint(MaxTimelineBuckets)},
			"service: timeline: more than %d buckets", MaxTimelineBuckets)
	}
	pts, err := s.d.Store.Transfer().Timeline(ctx, from, to, bucket, group)
	if err != nil {
		return nil, fmt.Errorf("service: timeline: %w", err)
	}
	return pts, nil
}
