//go:build windows

package platform

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// SetNativeHostRegistry sets the default value of HKEY_CURRENT_USER\key to
// manifestPath, creating the key.
func SetNativeHostRegistry(key, manifestPath string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("platform: create registry key %s: %w", key, err)
	}
	defer func() { _ = k.Close() }() // a read-only handle; nothing to flush
	if err := k.SetStringValue("", manifestPath); err != nil {
		return fmt.Errorf("platform: set registry key %s: %w", key, err)
	}
	return nil
}

// DeleteNativeHostRegistry deletes HKEY_CURRENT_USER\key; a missing key is
// not an error.
func DeleteNativeHostRegistry(key string) error {
	err := registry.DeleteKey(registry.CURRENT_USER, key)
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("platform: delete registry key %s: %w", key, err)
	}
	return nil
}
