package nativehost

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestManifests(t *testing.T) {
	data, err := ChromiumManifest("/x/launcher", []string{"chrome-extension://abc/"}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != HostName || got["type"] != "stdio" || got["path"] != "/x/launcher" {
		t.Errorf("chromium manifest = %v", got)
	}
	if o, _ := got["allowed_origins"].([]any); len(o) != 1 || o[0] != "chrome-extension://abc/" {
		t.Errorf("allowed_origins = %v", got["allowed_origins"])
	}
	if _, has := got["allowed_extensions"]; has {
		t.Error("chromium manifest has allowed_extensions")
	}

	data, err = FirefoxManifest("/x/launcher", []string{"dlm@example.org"}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if e, _ := got["allowed_extensions"].([]any); len(e) != 1 || e[0] != "dlm@example.org" {
		t.Errorf("allowed_extensions = %v", got["allowed_extensions"])
	}
	if _, has := got["allowed_origins"]; has {
		t.Error("firefox manifest has allowed_origins")
	}
}

func TestHostNameIsValid(t *testing.T) {
	// Chromium: lower-case letters, digits, underscores and dots; no leading,
	// trailing or doubled dots.
	if HostName != strings.ToLower(HostName) || strings.HasPrefix(HostName, ".") || strings.HasSuffix(HostName, ".") ||
		strings.Contains(HostName, "..") || strings.Trim(HostName, "abcdefghijklmnopqrstuvwxyz0123456789_.") != "" {
		t.Errorf("invalid host name %q", HostName)
	}
}

func TestChromiumOrigin(t *testing.T) {
	const id = "abcdefghijklmnopabcdefghijklmnop"
	for _, in := range []string{id, "chrome-extension://" + id, "chrome-extension://" + id + "/", "  " + id + " "} {
		got, err := ChromiumOrigin(in)
		if err != nil || got != "chrome-extension://"+id+"/" {
			t.Errorf("ChromiumOrigin(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "short", id + "q", strings.ToUpper(id), "https://" + id} {
		if _, err := ChromiumOrigin(in); err == nil {
			t.Errorf("ChromiumOrigin(%q) accepted", in)
		}
	}
}
