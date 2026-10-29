//go:build windows

package platform

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

type systemAutostartRegistry struct{}

func (systemAutostartRegistry) Set(name, command string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, windowsRunKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("platform: open the Run key: %w", err)
	}
	defer func() { _ = k.Close() }() // nothing to flush
	if err := k.SetStringValue(name, command); err != nil {
		return fmt.Errorf("platform: set the Run value: %w", err)
	}
	return nil
}

func (systemAutostartRegistry) Delete(name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("platform: open the Run key: %w", err)
	}
	defer func() { _ = k.Close() }() // nothing to flush
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("platform: delete the Run value: %w", err)
	}
	return nil
}

func (systemAutostartRegistry) Get(name string) (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("platform: open the Run key: %w", err)
	}
	defer func() { _ = k.Close() }() // nothing to flush
	v, _, err := k.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("platform: read the Run value: %w", err)
	}
	return v, true, nil
}
