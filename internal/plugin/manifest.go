package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
)

// Hook names.
const (
	HookResolve = "resolve"
	HookRefresh = "refresh"
	HookStart   = "start"
	HookError   = "error"
	HookDone    = "done"
)

// Hooks lists the hooks a plugin may implement.
var Hooks = []string{HookResolve, HookRefresh, HookStart, HookError, HookDone}

// Manifest limits.
const (
	// DefaultTimeout bounds one call of a plugin when its manifest says nothing.
	DefaultTimeout = 10 * time.Second
	// MaxTimeout is the longest timeout a manifest may ask for.
	MaxTimeout = 60 * time.Second

	maxMatch = 32
	maxNet   = 32
	// ManifestFile is the manifest's file name.
	ManifestFile = "manifest.json"
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Manifest describes a plugin (manifest.json).
type Manifest struct {
	// Name identifies the plugin: lower-case letters, digits and dashes.
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
	// Description is one line about what the plugin does.
	Description string `json:"description,omitempty"`
	// Main is the script, relative to the plugin directory; default index.js.
	Main string `json:"main,omitempty"`
	// Hooks maps each implemented hook to its options.
	Hooks map[string]HookSpec `json:"hooks"`
	// Net lists the hosts dlm.http may reach: "api.example.com" or
	// "*.example.com". Empty means no network access.
	Net []string `json:"net,omitempty"`
	// Timeout limits one call of the plugin, such as "15s" (at most 60s).
	Timeout string `json:"timeout,omitempty"`
}

// HookSpec is the options of one hook.
type HookSpec struct {
	// Match restricts the hook to links matching one of these patterns, where
	// * stands for any run of characters ("https://*.example.com/*"). Empty
	// means every link.
	Match []string `json:"match,omitempty"`
}

// ParseManifest decodes and validates a manifest. Errors match
// domain.ErrInvalidArgument.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, domain.InvalidArgument("plugin manifest is not valid JSON: %v", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if m.Main == "" {
		m.Main = "index.js"
	}
	return &m, nil
}

// Validate checks the manifest.
func (m *Manifest) Validate() error {
	if !nameRE.MatchString(m.Name) {
		return domain.InvalidArgument("plugin name %q must be 1-64 lower-case letters, digits or dashes, starting with a letter or digit", m.Name)
	}
	if v := strings.TrimSpace(m.Version); v == "" || len(v) > 64 {
		return domain.InvalidArgument("plugin %q: version is required (at most 64 characters)", m.Name)
	}
	if len(m.Title) > 200 || len(m.Description) > 1000 {
		return domain.InvalidArgument("plugin %q: title or description is too long", m.Name)
	}
	if m.Main != "" {
		if err := checkRelPath(m.Main); err != nil || !strings.HasSuffix(m.Main, ".js") {
			return domain.InvalidArgument("plugin %q: main %q must be a relative .js file inside the plugin", m.Name, m.Main)
		}
	}
	if len(m.Hooks) == 0 {
		return domain.InvalidArgument("plugin %q: hooks must name at least one of %s", m.Name, strings.Join(Hooks, ", "))
	}
	for name, spec := range m.Hooks {
		if !slices.Contains(Hooks, name) {
			return domain.InvalidArgument("plugin %q: unknown hook %q (use %s)", m.Name, name, strings.Join(Hooks, ", "))
		}
		if len(spec.Match) > maxMatch {
			return domain.InvalidArgument("plugin %q: hook %q has more than %d match patterns", m.Name, name, maxMatch)
		}
		for _, p := range spec.Match {
			if strings.TrimSpace(p) == "" || len(p) > 500 {
				return domain.InvalidArgument("plugin %q: hook %q has an empty or too long match pattern", m.Name, name)
			}
		}
	}
	if len(m.Net) > maxNet {
		return domain.InvalidArgument("plugin %q: net lists more than %d hosts", m.Name, maxNet)
	}
	for _, h := range m.Net {
		if err := domain.ValidateCredentialHost(h); err != nil {
			return domain.InvalidArgument("plugin %q: net host %q must be a host name such as api.example.com or *.example.com", m.Name, h)
		}
	}
	if _, err := m.CallTimeout(); err != nil {
		return domain.InvalidArgument("plugin %q: %v", m.Name, err)
	}
	return nil
}

// CallTimeout is how long one call of the plugin may run.
func (m *Manifest) CallTimeout() (time.Duration, error) {
	if m.Timeout == "" {
		return DefaultTimeout, nil
	}
	d, err := time.ParseDuration(m.Timeout)
	if err != nil || d <= 0 || d > MaxTimeout {
		return 0, fmt.Errorf("timeout %q must be a duration between 1ms and %s", m.Timeout, MaxTimeout)
	}
	return d, nil
}

// HookNames returns the hooks the manifest declares, in the order of Hooks.
func (m *Manifest) HookNames() []string {
	var out []string
	for _, h := range Hooks {
		if _, ok := m.Hooks[h]; ok {
			out = append(out, h)
		}
	}
	return out
}

// Matches reports whether the hook applies to url. A hook without patterns
// applies to every link; start, error and done hooks are matched against the
// task's link too.
func (m *Manifest) Matches(hook, url string) bool {
	spec, ok := m.Hooks[hook]
	if !ok {
		return false
	}
	if len(spec.Match) == 0 {
		return true
	}
	url = strings.ToLower(url)
	for _, p := range spec.Match {
		if globMatch(strings.ToLower(p), url) {
			return true
		}
	}
	return false
}

// AllowsHost reports whether dlm.http may contact host.
func (m *Manifest) AllowsHost(host string) bool {
	host = strings.ToLower(host)
	for _, h := range m.Net {
		if name, ok := strings.CutPrefix(h, "*."); ok {
			if strings.HasSuffix(host, "."+name) {
				return true
			}
		} else if host == h {
			return true
		}
	}
	return false
}

// globMatch matches s against a pattern where * stands for any run of
// characters (including none) and everything else is literal.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// checkRelPath requires a clean relative path that stays inside its directory.
func checkRelPath(p string) error {
	if p == "" || strings.ContainsAny(p, "\\\x00") || path.IsAbs(p) {
		return errors.New("not a relative path")
	}
	if c := path.Clean(p); c != p || c == ".." || strings.HasPrefix(c, "../") {
		return errors.New("path escapes the plugin directory")
	}
	return nil
}
