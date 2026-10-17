//go:build !windows

package platform

// SetNativeHostRegistry is only implemented on Windows, where browsers find
// native messaging hosts through the registry.
func SetNativeHostRegistry(string, string) error { return ErrUnsupported }

// DeleteNativeHostRegistry is only implemented on Windows.
func DeleteNativeHostRegistry(string) error { return ErrUnsupported }
