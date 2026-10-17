//go:build !darwin && !windows

package platform

import "context"

// SystemProxy returns an empty configuration on Linux and the BSDs: there is
// no desktop-independent OS proxy setting (GNOME/KDE settings are not read),
// so only the HTTP_PROXY/HTTPS_PROXY/NO_PROXY environment variables apply.
func SystemProxy(_ context.Context) (*ProxySettings, error) {
	return &ProxySettings{}, nil
}
