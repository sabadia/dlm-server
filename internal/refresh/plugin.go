package refresh

import (
	"context"
	"errors"
	"fmt"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// PluginRefresher asks the plugins for a new link (plugin.Manager).
type PluginRefresher interface {
	// Refresh returns the task's source with the link a plugin gave back, or
	// handled false when no plugin handles the task; a plugin's failure is
	// returned when no other plugin succeeded.
	Refresh(ctx context.Context, t *domain.Task) (next domain.Source, handled bool, err error)
}

// Plugin is the strategy that lets the plugins' refresh hooks find a new link,
// for sites whose links only a script knows how to renew.
type Plugin struct {
	Plugins PluginRefresher
}

// Name implements Strategy.
func (Plugin) Name() settings.RefreshStrategy { return settings.RefreshPlugin }

// Refresh implements Strategy.
func (p Plugin) Refresh(ctx context.Context, req Request) (Candidate, error) {
	next, handled, err := p.Plugins.Refresh(ctx, req.Task)
	switch {
	case err != nil && !handled:
		return Candidate{}, fmt.Errorf("plugin: %w", err)
	case !handled:
		return Candidate{}, fmt.Errorf("%w: no plugin handles this link", ErrNotApplicable)
	case next.CurrentURL == req.Task.Source.CurrentURL:
		return Candidate{}, errors.New("plugin: the plugin returned the link that expired")
	}
	return Candidate{Source: next}, nil
}
