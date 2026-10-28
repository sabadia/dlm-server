package service

import (
	"context"
	"fmt"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/settings"
	"github.com/sabadia/dlm-server/internal/storage"
)

// SettingsService manages the global settings layer and computes effective
// settings (Defaults ⊕ Global ⊕ Queue ⊕ Task) with per-field provenance.
type SettingsService struct {
	d Deps
}

// NewSettingsService creates a SettingsService.
func NewSettingsService(d Deps) *SettingsService {
	return &SettingsService{d: d.withDefaults()}
}

// Defaults returns the built-in lowest settings layer.
func (s *SettingsService) Defaults() settings.Settings { return s.d.Defaults() }

// Global returns the stored global layer (unset fields inherit defaults).
func (s *SettingsService) Global(ctx context.Context) (settings.Settings, error) {
	g, err := s.d.Store.Settings().GetGlobal(ctx)
	if err != nil {
		return settings.Settings{}, fmt.Errorf("service: get global settings: %w", err)
	}
	return g, nil
}

// ReplaceGlobal replaces the global layer. It is validated at the global
// level (errors match domain.ErrInvalidArgument and unwrap to
// *settings.ValidationError), saved, the global speed limit is applied, a
// settings.updated event carrying the new layer is published and the
// scheduler is woken (concurrency limits may have changed).
func (s *SettingsService) ReplaceGlobal(ctx context.Context, g settings.Settings) (settings.Settings, error) {
	return s.ReplaceGlobalIfMatch(ctx, "", g)
}

// ReplaceGlobalIfMatch is ReplaceGlobal that fails with an error matching
// domain.ErrPreconditionFailed (code settings.changed) unless the stored
// layer's settings.ETag equals etag; an empty etag skips the check.
func (s *SettingsService) ReplaceGlobalIfMatch(ctx context.Context, etag string, g settings.Settings) (settings.Settings, error) {
	return s.updateGlobal(ctx, etag, func(settings.Settings) (settings.Settings, error) { return g, nil })
}

// PatchGlobal applies an RFC 7396 JSON merge patch (see settings.MergePatch)
// to the global layer and then behaves like ReplaceGlobal. A malformed
// patch returns an error matching domain.ErrInvalidArgument.
func (s *SettingsService) PatchGlobal(ctx context.Context, patch []byte) (settings.Settings, error) {
	return s.PatchGlobalIfMatch(ctx, "", patch)
}

// PatchGlobalIfMatch is PatchGlobal with the precondition of
// ReplaceGlobalIfMatch.
func (s *SettingsService) PatchGlobalIfMatch(ctx context.Context, etag string, patch []byte) (settings.Settings, error) {
	return s.updateGlobal(ctx, etag, func(cur settings.Settings) (settings.Settings, error) {
		return patchSettings(cur, patch)
	})
}

// updateGlobal is the compare-and-set of the global layer: inside one
// transaction it checks etag against the stored layer, derives, validates and
// saves the next layer, then applies the side effects.
func (s *SettingsService) updateGlobal(ctx context.Context, etag string, next func(cur settings.Settings) (settings.Settings, error)) (settings.Settings, error) {
	var out settings.Settings
	err := s.d.Store.WithTx(ctx, func(tx storage.Store) error {
		cur, err := tx.Settings().GetGlobal(ctx)
		if err != nil {
			return fmt.Errorf("service: get global settings: %w", err)
		}
		if etag != "" && etag != settings.ETag(cur) {
			return domain.PreconditionFailed("settings.changed", "the global settings changed since they were read")
		}
		g, err := next(cur)
		if err != nil {
			return err
		}
		if err := validateLevel(g, settings.LevelGlobal); err != nil {
			return err
		}
		if err := tx.Settings().SaveGlobal(ctx, g); err != nil {
			return fmt.Errorf("service: save global settings: %w", err)
		}
		out = g
		return nil
	})
	if err != nil {
		return settings.Settings{}, err //nolint:wrapcheck // errors are wrapped inside the transaction
	}
	s.applyGlobalLimit(out)
	s.d.publish(events.TypeSettingsUpdated, "", "", out)
	s.d.Scheduler.Wake()
	return out, nil
}

// EffectiveForTask resolves Defaults ⊕ Global ⊕ Queue ⊕ Task for a task.
// An unknown task returns an error matching domain.ErrNotFound.
func (s *SettingsService) EffectiveForTask(ctx context.Context, taskID string) (settings.Effective, error) {
	t, err := s.d.Store.Tasks().Get(ctx, taskID)
	if err != nil {
		return settings.Effective{}, fmt.Errorf("service: effective settings: %w", err)
	}
	return effective(ctx, s.d, t.QueueID, t.Settings)
}

// EffectiveForQueue resolves Defaults ⊕ Global ⊕ Queue for a queue. An
// unknown queue returns an error matching domain.ErrNotFound.
func (s *SettingsService) EffectiveForQueue(ctx context.Context, queueID string) (settings.Effective, error) {
	if err := resolveQueueRef(ctx, s.d.Store, &queueID); err != nil {
		return settings.Effective{}, err
	}
	return effective(ctx, s.d, queueID, settings.Settings{})
}

// SyncLimits applies the stored global and per-queue speed limits,
// including the speed profiles active now, to the rate-limit manager (see
// LimitsController). The daemon calls it once at startup; afterwards the
// services re-apply limits on every change and the scheduler re-applies
// them on every admission pass so profiles switch on time.
func (s *SettingsService) SyncLimits(ctx context.Context) error {
	return NewLimitsController(s.d).Sync(ctx)
}

// applyGlobalLimit sets the global bucket to the global layer's effective
// speed limit now (falling back to the default layer).
func (s *SettingsService) applyGlobalLimit(g settings.Settings) {
	NewLimitsController(s.d).applyGlobal(g, s.d.now())
}

// effective resolves the layers for a task of queueID with the given task
// overrides (zero for a queue's effective settings).
func effective(ctx context.Context, d Deps, queueID string, task settings.Settings) (settings.Effective, error) {
	q, err := d.Store.Queues().Get(ctx, queueID)
	if err != nil {
		return settings.Effective{}, fmt.Errorf("service: effective settings: %w", err)
	}
	g, err := d.Store.Settings().GetGlobal(ctx)
	if err != nil {
		return settings.Effective{}, fmt.Errorf("service: effective settings: %w", err)
	}
	return settings.Resolve(settings.Layers{Defaults: d.Defaults(), Global: g, Queue: q.Settings, Task: task}), nil
}
