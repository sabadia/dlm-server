//go:build !windows

package platform

// systemAutostartRegistry exists only on Windows, where login items live in
// the registry.
type systemAutostartRegistry struct{}

func (systemAutostartRegistry) Set(string, string) error { return ErrAutostartUnsupported }
func (systemAutostartRegistry) Delete(string) error      { return ErrAutostartUnsupported }
func (systemAutostartRegistry) Get(string) (string, bool, error) {
	return "", false, ErrAutostartUnsupported
}
