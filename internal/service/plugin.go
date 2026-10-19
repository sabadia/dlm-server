package service

import (
	"context"
	"fmt"

	"github.com/sabadia/dlm-server/internal/domain"
)

// PluginManager installs and runs plugins (plugin.Manager).
type PluginManager interface {
	List(ctx context.Context) []*domain.Plugin
	Get(ctx context.Context, name string) (*domain.Plugin, error)
	Install(ctx context.Context, in domain.PluginInstall) (*domain.Plugin, error)
	SetEnabled(ctx context.Context, name string, enabled bool) (*domain.Plugin, error)
	Uninstall(ctx context.Context, name string) error
}

// PluginService manages the installed plugins.
type PluginService struct {
	m PluginManager
}

// NewPluginService creates a PluginService.
func NewPluginService(m PluginManager) *PluginService { return &PluginService{m: m} }

// List returns the installed plugins ordered by name.
func (s *PluginService) List(ctx context.Context) []*domain.Plugin { return s.m.List(ctx) }

// Get returns one plugin.
func (s *PluginService) Get(ctx context.Context, name string) (*domain.Plugin, error) {
	p, err := s.m.Get(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("service: get plugin: %w", err)
	}
	return p, nil
}

// Install installs (or with in.Replace upgrades) a plugin from a directory or
// a git URL.
func (s *PluginService) Install(ctx context.Context, in domain.PluginInstall) (*domain.Plugin, error) {
	p, err := s.m.Install(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("service: install plugin: %w", err)
	}
	return p, nil
}

// SetEnabled switches a plugin on or off.
func (s *PluginService) SetEnabled(ctx context.Context, name string, enabled bool) (*domain.Plugin, error) {
	p, err := s.m.SetEnabled(ctx, name, enabled)
	if err != nil {
		return nil, fmt.Errorf("service: update plugin: %w", err)
	}
	return p, nil
}

// Delete uninstalls a plugin and removes its files and stored data.
func (s *PluginService) Delete(ctx context.Context, name string) error {
	if err := s.m.Uninstall(ctx, name); err != nil {
		return fmt.Errorf("service: delete plugin: %w", err)
	}
	return nil
}
