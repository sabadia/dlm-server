//go:build windows

package platform

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// SystemProxy returns the current user's WinINET manual proxy settings from
// the registry. It does not cache; callers should.
func SystemProxy(_ context.Context) (*ProxySettings, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return &ProxySettings{}, nil
		}
		return nil, fmt.Errorf("platform: open internet settings: %w", err)
	}
	defer k.Close() //nolint:errcheck // read-only key; close error is irrelevant

	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return &ProxySettings{}, nil
		}
		return nil, fmt.Errorf("platform: read ProxyEnable: %w", err)
	}
	server, err := stringValue(k, "ProxyServer")
	if err != nil {
		return nil, err
	}
	override, err := stringValue(k, "ProxyOverride")
	if err != nil {
		return nil, err
	}
	return ParseWindowsProxy(enable != 0, server, override), nil
}

func stringValue(k registry.Key, name string) (string, error) {
	v, _, err := k.GetStringValue(name)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("platform: read %s: %w", name, err)
	}
	return v, nil
}
