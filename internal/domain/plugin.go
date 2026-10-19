package domain

import "time"

// Plugin is an installed JavaScript plugin (see package plugin).
type Plugin struct {
	// Name identifies the plugin.
	Name        string `json:"name"`
	Version     string `json:"version"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// Source is where it was installed from: a directory or a git URL.
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
	// Hooks are the hooks the plugin implements.
	Hooks []string `json:"hooks"`
	// Net are the hosts the plugin may contact.
	Net []string `json:"net,omitempty"`
	// Error is why the plugin is not running although it is enabled (its files
	// are missing or its script fails); empty when it is fine or switched off.
	Error       string    `json:"error,omitempty"`
	InstalledAt time.Time `json:"installedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// PluginInstall says what to install.
type PluginInstall struct {
	// Source is an absolute directory on the daemon's host, or an https:// git
	// URL.
	Source string
	// Ref is a branch or tag of a git source.
	Ref string
	// Replace upgrades an installed plugin of the same name instead of
	// refusing.
	Replace bool
	// Disabled installs the plugin switched off.
	Disabled bool
}
