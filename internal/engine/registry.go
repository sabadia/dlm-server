package engine

import (
	"fmt"
	"slices"

	"github.com/sabadia/dlm-server/internal/domain"
)

// Registry holds engines in selection order. It is immutable after
// construction and safe for concurrent use.
type Registry struct {
	engines []Engine
}

// NewRegistry returns a registry that tries engines in the given order.
// Engine names must be unique and non-empty.
func NewRegistry(engines ...Engine) (*Registry, error) {
	seen := make(map[string]bool, len(engines))
	for _, e := range engines {
		name := e.Name()
		if name == "" {
			return nil, fmt.Errorf("engine: registry: engine with empty name")
		}
		if seen[name] {
			return nil, fmt.Errorf("engine: registry: duplicate engine %q", name)
		}
		seen[name] = true
	}
	return &Registry{engines: slices.Clone(engines)}, nil
}

// Select returns the first engine that matches src. It returns an error
// matching domain.ErrInvalidArgument when no engine supports the source.
func (r *Registry) Select(src domain.Source) (Engine, error) {
	for _, e := range r.engines {
		if e.Match(src) {
			return e, nil
		}
	}
	return nil, domain.InvalidArgument("engine: no engine supports %q", src.CurrentURL)
}

// Get returns the engine with the given name (used to resume a task with
// the engine that started it). It returns an error matching
// domain.ErrNotFound for unknown names.
func (r *Registry) Get(name string) (Engine, error) {
	for _, e := range r.engines {
		if e.Name() == name {
			return e, nil
		}
	}
	return nil, domain.NotFound("engine", name)
}

// Names returns the registered engine names in selection order.
func (r *Registry) Names() []string {
	names := make([]string, len(r.engines))
	for i, e := range r.engines {
		names[i] = e.Name()
	}
	return names
}
