package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/settings"
)

// Bounds of a speed override.
const (
	MinOverride = time.Minute
	MaxOverride = 24 * time.Hour
)

// QuotaUsage is how much of a quota scope is used in its current window.
type QuotaUsage struct {
	Quota       settings.Quota
	Used        int64
	WindowStart time.Time // zero when no window is open
	Exhausted   bool
}

// QuotaSource reports quota usage: queueID "" is the global scope. ok is
// false when the scope has no quota. The scheduler's QuotaMeter is one.
type QuotaSource interface {
	Usage(queueID string) (usage QuotaUsage, ok bool)
}

// LimitsService describes the speed limits and quotas in force and sets the
// temporary global speed override ("turbo").
type LimitsService struct {
	d      Deps
	quotas QuotaSource
}

// NewLimitsService creates the service. Deps.Store and Deps.Limits are
// required; quotas may be nil (no quota is reported).
func NewLimitsService(d Deps, quotas QuotaSource) *LimitsService {
	return &LimitsService{d: d.withDefaults(), quotas: quotas}
}

// QuotaView is the state of one quota.
type QuotaView struct {
	Bytes  int64             `json:"bytes"`
	Period settings.Duration `json:"period"`
	// Used is what was downloaded in the current window.
	Used        int64      `json:"used"`
	WindowStart *time.Time `json:"windowStart,omitempty"`
	// ResetsAt is when the window ends and the usage drops to zero.
	ResetsAt  *time.Time `json:"resetsAt,omitempty"`
	Exhausted bool       `json:"exhausted"`
}

// ProfileView names the speed profile that is active.
type ProfileView struct {
	// Index is the position in the list of speed profiles.
	Index      int   `json:"index"`
	SpeedLimit int64 `json:"speedLimit"`
}

// ScopeLimits are the limits of the global scope or of one queue.
type ScopeLimits struct {
	// QueueID is empty for the global scope.
	QueueID string `json:"queueId,omitempty"`
	Name    string `json:"name,omitempty"`
	// SpeedLimit is what is in force now, in bytes per second (0 = unlimited).
	SpeedLimit int64 `json:"speedLimit"`
	// ConfiguredSpeedLimit is what the settings and speed profiles give, which
	// differs from SpeedLimit only for the global scope during an override.
	ConfiguredSpeedLimit int64        `json:"configuredSpeedLimit"`
	ActiveProfile        *ProfileView `json:"activeProfile,omitempty"`
	Quota                *QuotaView   `json:"quota,omitempty"`
}

// LimitsView is the answer of Limits.
type LimitsView struct {
	Global ScopeLimits `json:"global"`
	// Override is the temporary global limit, absent when there is none.
	Override *events.SpeedOverride `json:"override,omitempty"`
	Queues   []ScopeLimits         `json:"queues"`
}

// Limits returns the limits and quotas in force now.
func (s *LimitsService) Limits(ctx context.Context) (*LimitsView, error) {
	now := s.d.now()
	g, err := s.d.Store.Settings().GetGlobal(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: limits: %w", err)
	}
	queues, err := s.d.Store.Queues().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: limits: %w", err)
	}
	eff := settings.Merge(s.d.Defaults(), g)
	v := &LimitsView{Queues: []ScopeLimits{}}
	v.Global = s.scope("", "", eff.SpeedLimit.Value(), eff.SpeedProfiles.Value(), now)
	if s.d.Limits != nil {
		v.Global.SpeedLimit = s.d.Limits.Global().Limit()
		if o, ok := s.d.Limits.CurrentOverride(now); ok {
			v.Override = &events.SpeedOverride{SpeedLimit: o.BytesPerSec, Until: o.Until}
		}
	}
	for _, q := range queues {
		sc := s.scope(q.ID, q.Name, q.Settings.SpeedLimit.Value(), q.Settings.SpeedProfiles.Value(), now)
		v.Queues = append(v.Queues, sc)
	}
	return v, nil
}

func (s *LimitsService) scope(queueID, name string, base int64, profiles []settings.SpeedProfile, now time.Time) ScopeLimits {
	sc := ScopeLimits{QueueID: queueID, Name: name}
	sc.ConfiguredSpeedLimit = settings.EffectiveSpeedLimit(base, profiles, now)
	sc.SpeedLimit = sc.ConfiguredSpeedLimit
	for i, p := range profiles {
		if p.ActiveAt(now) {
			sc.ActiveProfile = &ProfileView{Index: i, SpeedLimit: p.SpeedLimit}
			break
		}
	}
	if s.quotas != nil {
		if u, ok := s.quotas.Usage(queueID); ok {
			q := &QuotaView{Bytes: u.Quota.Bytes, Period: u.Quota.Period, Used: u.Used, Exhausted: u.Exhausted}
			if !u.WindowStart.IsZero() {
				ws := u.WindowStart
				end := ws.Add(u.Quota.Period.Std())
				q.WindowStart, q.ResetsAt = &ws, &end
			}
			sc.Quota = q
		}
	}
	return sc
}

// SetOverride replaces the global speed limit by speedLimit bytes per second
// (0 = unlimited, "turbo") for d, between MinOverride and MaxOverride; the
// configured limit applies again afterwards. Settings changes made meanwhile
// are kept and take effect when the override ends.
func (s *LimitsService) SetOverride(ctx context.Context, speedLimit int64, d time.Duration) (*LimitsView, error) {
	if s.d.Limits == nil {
		return nil, errors.New("service: speed limits are not available")
	}
	if speedLimit < 0 {
		return nil, domain.InvalidArgumentCode("limits.bad_limit", nil, "service: the speed limit must not be negative")
	}
	if d < MinOverride || d > MaxOverride {
		return nil, domain.InvalidArgumentCode("limits.bad_duration", map[string]string{"min": MinOverride.String(), "max": MaxOverride.String()},
			"service: an override lasts between %v and %v", MinOverride, MaxOverride)
	}
	now := s.d.now()
	s.d.Limits.SetOverride(speedLimit, now.Add(d))
	publishLimits(s.d.Bus, s.d.Limits, now)
	return s.Limits(ctx)
}

// ClearOverride ends the temporary global limit now; without one it does
// nothing.
func (s *LimitsService) ClearOverride(ctx context.Context) (*LimitsView, error) {
	if s.d.Limits == nil {
		return nil, errors.New("service: speed limits are not available")
	}
	if s.d.Limits.ClearOverride() {
		publishLimits(s.d.Bus, s.d.Limits, s.d.now())
	}
	return s.Limits(ctx)
}
