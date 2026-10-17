package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

// queueRef resolves a queue reference — a queue ID or a queue name
// (case-insensitive; names are unique) — to the queue ID, so clients can
// say "--queue fast" instead of a UUID. An empty reference is returned
// unchanged (callers apply their own default). An ID always wins over a
// name. Unknown references return an error matching domain.ErrNotFound.
func queueRef(ctx context.Context, store storage.Store, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if _, err := store.Queues().Get(ctx, ref); err == nil {
		return ref, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return "", fmt.Errorf("service: resolve queue %q: %w", ref, err)
	}
	queues, err := store.Queues().List(ctx)
	if err != nil {
		return "", fmt.Errorf("service: resolve queue %q: %w", ref, err)
	}
	for _, q := range queues {
		if strings.EqualFold(q.Name, strings.TrimSpace(ref)) {
			return q.ID, nil
		}
	}
	return "", domain.NotFound("queue", ref)
}

// resolveQueueRef replaces *ref with the queue ID it refers to.
func resolveQueueRef(ctx context.Context, store storage.Store, ref *string) error {
	id, err := queueRef(ctx, store, *ref)
	if err != nil {
		return err
	}
	*ref = id
	return nil
}
