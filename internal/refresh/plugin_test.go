package refresh

import (
	"context"
	"errors"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

type fakeRefresher struct {
	next    domain.Source
	handled bool
	err     error
}

func (f fakeRefresher) Refresh(context.Context, *domain.Task) (domain.Source, bool, error) {
	return f.next, f.handled, f.err
}

func TestPluginStrategy(t *testing.T) {
	task := &domain.Task{ID: "t", Source: domain.Source{CurrentURL: "https://h/old"}}
	req := Request{Task: task}
	if got := (Plugin{}).Name(); got != settings.RefreshPlugin {
		t.Errorf("Name = %q", got)
	}
	fresh := domain.Source{CurrentURL: "https://h/new", ResolverKind: domain.ResolverPlugin}
	cand, err := Plugin{Plugins: fakeRefresher{next: fresh, handled: true}}.Refresh(t.Context(), req)
	if err != nil || cand.Source.CurrentURL != "https://h/new" {
		t.Errorf("handled = %+v, %v", cand, err)
	}
	if _, err := (Plugin{Plugins: fakeRefresher{}}).Refresh(t.Context(), req); !errors.Is(err, ErrNotApplicable) {
		t.Errorf("not handled: %v, want ErrNotApplicable", err)
	}
	boom := errors.New("script failed")
	if _, err := (Plugin{Plugins: fakeRefresher{err: boom}}).Refresh(t.Context(), req); !errors.Is(err, boom) || errors.Is(err, ErrNotApplicable) {
		t.Errorf("failure: %v", err)
	}
	// A failure that another plugin made up for does not matter.
	if _, err := (Plugin{Plugins: fakeRefresher{next: fresh, handled: true, err: boom}}).Refresh(t.Context(), req); err != nil {
		t.Errorf("handled with an error: %v", err)
	}
	same := domain.Source{CurrentURL: "https://h/old"}
	if _, err := (Plugin{Plugins: fakeRefresher{next: same, handled: true}}).Refresh(t.Context(), req); err == nil {
		t.Error("a plugin that returns the expired link was accepted")
	}
}
