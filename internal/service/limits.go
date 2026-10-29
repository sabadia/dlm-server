package service

import (
	"context"
	"fmt"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/settings"
)

// LimitsController applies the effective global and per-queue speed limits
// to the rate-limit manager (Deps.Limits). A level's effective limit is the
// SpeedLimit of its first speed profile active at the current time, or its
// SpeedLimit when no profile is active (see settings.EffectiveSpeedLimit):
//
//   - global: profiles and limit of Defaults ⊕ Global;
//   - queue: the queue's own profiles and limit only (neither is inherited
//     from the global layer, which has its own bucket).
//
// Profiles switch on the wall clock, so the limits must be re-applied over
// time: the scheduler calls Apply on every admission pass. Applying an
// unchanged limit is a no-op for the buckets.
type LimitsController struct {
	d Deps
}

// NewLimitsController creates a LimitsController. Only Store, Limits,
// Defaults and Clock of d are used; a nil Limits makes it a no-op.
func NewLimitsController(d Deps) *LimitsController {
	return &LimitsController{d: d.withDefaults()}
}

// Sync loads the global layer and the queues and applies their limits at
// the current time.
func (c *LimitsController) Sync(ctx context.Context) error {
	if c.d.Limits == nil {
		return nil
	}
	g, err := c.d.Store.Settings().GetGlobal(ctx)
	if err != nil {
		return fmt.Errorf("service: sync limits: %w", err)
	}
	queues, err := c.d.Store.Queues().List(ctx)
	if err != nil {
		return fmt.Errorf("service: sync limits: %w", err)
	}
	c.Apply(g, queues, c.d.now())
	return nil
}

// Apply sets the global bucket and every queue's bucket to their effective
// limits at now. A temporary speed override whose time is up ends here, and
// clients are told.
func (c *LimitsController) Apply(global settings.Settings, queues []*domain.Queue, now time.Time) {
	if c.d.Limits == nil {
		return
	}
	if c.d.Limits.ExpireOverride(now) {
		publishLimits(c.d.Bus, c.d.Limits, now)
	}
	c.applyGlobal(global, now)
	for _, q := range queues {
		c.applyQueue(q, now)
	}
}

// applyGlobal sets the global bucket from the global layer (falling back to
// the default layer).
func (c *LimitsController) applyGlobal(global settings.Settings, now time.Time) {
	if c.d.Limits == nil {
		return
	}
	eff := settings.Merge(c.d.Defaults(), global)
	c.d.Limits.SetGlobal(settings.EffectiveSpeedLimit(eff.SpeedLimit.Value(), eff.SpeedProfiles.Value(), now))
}

// applyQueue sets a queue's bucket from its own overrides.
func (c *LimitsController) applyQueue(q *domain.Queue, now time.Time) {
	if c.d.Limits == nil {
		return
	}
	c.d.Limits.SetQueue(q.ID, settings.EffectiveSpeedLimit(q.Settings.SpeedLimit.Value(), q.Settings.SpeedProfiles.Value(), now))
}

// publishLimits tells clients which global limit is now in force.
func publishLimits(bus *events.Bus, limits *ratelimit.Manager, now time.Time) {
	if bus == nil || limits == nil {
		return
	}
	data := events.LimitsChangedData{SpeedLimit: limits.Global().Limit()}
	if o, ok := limits.CurrentOverride(now); ok {
		data.Override = &events.SpeedOverride{SpeedLimit: o.BytesPerSec, Until: o.Until}
	}
	bus.Publish(events.Event{Type: events.TypeLimitsChanged, Data: data})
}
