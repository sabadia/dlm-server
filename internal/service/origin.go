package service

import (
	"context"
	"maps"

	"github.com/sabadia/dlm-server/internal/callctx"
	"github.com/sabadia/dlm-server/internal/domain"
)

// OriginLabel is the label that records who started a task. A task that a
// browser-extension token created carries OriginExtension for good: whatever
// the daemon does for it later (a refresh, a retry after a restart) is held to
// the same limits as the request that created it.
const (
	OriginLabel     = "dlm.origin"
	OriginExtension = "extension"
)

// taskContext returns ctx marked as work for task t: its id (so its own
// cookies are used) and, for a task an extension started, the restriction.
func taskContext(ctx context.Context, t *domain.Task) context.Context {
	return callctx.With(ctx, callctx.Caller{TaskID: t.ID, Restricted: t.Labels[OriginLabel] == OriginExtension})
}

// stampOrigin marks the labels of a task about to be created. An extension
// token cannot choose its own origin: the label is set, whatever it sent.
func stampOrigin(ctx context.Context, labels map[string]string) map[string]string {
	if !callctx.From(ctx).Restricted {
		return labels
	}
	out := maps.Clone(labels)
	if out == nil {
		out = map[string]string{}
	}
	out[OriginLabel] = OriginExtension
	return out
}
