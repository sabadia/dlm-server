package nativehost

import (
	"encoding/json"
	"fmt"
	"strings"
)

// HostName is the name extensions pass to runtime.connectNative. Browsers
// allow lower-case letters, digits, underscores and dots.
const HostName = "dev.sabadia.dlm"

const hostDescription = "dlm download manager: gives the browser extension the daemon address and token"

// Manifest is a native messaging host manifest. Chromium-family browsers
// read AllowedOrigins, Firefox reads AllowedExtensions.
type Manifest struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Path              string   `json:"path"`
	Type              string   `json:"type"`
	AllowedOrigins    []string `json:"allowed_origins,omitempty"`
	AllowedExtensions []string `json:"allowed_extensions,omitempty"`
}

// ChromiumManifest returns the manifest for Chromium-family browsers; path
// is the absolute path of the launcher and origins are "chrome-extension://ID/".
func ChromiumManifest(path string, origins []string) Manifest {
	return Manifest{Name: HostName, Description: hostDescription, Path: path, Type: "stdio", AllowedOrigins: origins}
}

// FirefoxManifest returns the manifest for Firefox; ids are add-on IDs such
// as "dlm@sabadia.dev".
func FirefoxManifest(path string, ids []string) Manifest {
	return Manifest{Name: HostName, Description: hostDescription, Path: path, Type: "stdio", AllowedExtensions: ids}
}

// Marshal returns the manifest as indented JSON.
func (m Manifest) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("nativehost: encode manifest: %w", err)
	}
	return append(data, '\n'), nil
}

// ChromiumOrigin turns an extension ID (or an origin) into the
// "chrome-extension://ID/" form of allowed_origins. A Chrome extension ID
// is 32 characters a-p; anything else is rejected so a typo cannot silently
// match nothing.
func ChromiumOrigin(idOrOrigin string) (string, error) {
	id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(idOrOrigin), "chrome-extension://"), "/")
	if len(id) != 32 || strings.Trim(id, "abcdefghijklmnop") != "" {
		return "", fmt.Errorf("nativehost: %q is not a Chromium extension ID (32 letters a-p)", idOrOrigin)
	}
	return "chrome-extension://" + id + "/", nil
}
