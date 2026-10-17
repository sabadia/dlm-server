//go:build darwin

package platform

import (
	"context"
	"fmt"
	"os/exec"
)

// SystemProxy returns the macOS manual proxy settings by running
// `scutil --proxy` (no cgo). It does not cache; callers should.
func SystemProxy(ctx context.Context) (*ProxySettings, error) {
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return nil, fmt.Errorf("platform: scutil --proxy: %w", err)
	}
	return ParseScutilProxy(string(out)), nil
}
