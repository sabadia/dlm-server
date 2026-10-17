package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// settingsDoc builds a settings layer object or an RFC 7396 merge patch from
// dotted paths ("retry.maxAttempts=3"). It knows nothing about the settings
// schema; the daemon validates the result.
type settingsDoc map[string]any

// set stores v at the dotted path, creating intermediate objects. v == nil
// stores JSON null (unset, in a merge patch).
func (d settingsDoc) set(path string, v any) error {
	parts := strings.Split(path, ".")
	if slices.Contains(parts, "") {
		return fmt.Errorf("cli: invalid settings path %q", path)
	}
	m := map[string]any(d)
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			if _, exists := m[p]; exists {
				return fmt.Errorf("cli: settings path %q conflicts with a value set at %q", path, p)
			}
			next = make(map[string]any)
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
	return nil
}

// assign parses "path=value" and stores the value: JSON when it parses as
// JSON (numbers, booleans, null, quoted strings, objects, arrays), else the
// literal string.
func (d settingsDoc) assign(kv string) error {
	path, raw, ok := strings.Cut(kv, "=")
	if !ok {
		return fmt.Errorf("cli: invalid assignment %q (want path=value)", kv)
	}
	return d.set(strings.TrimSpace(path), parseValue(raw))
}

// parseValue returns raw decoded as JSON when valid, else raw itself.
func parseValue(raw string) any {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return raw
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return raw
	}
	return v
}

// apply stores every "path=value" assignment and unsets every path.
func (d settingsDoc) apply(assignments, unsets []string) error {
	for _, kv := range assignments {
		if err := d.assign(kv); err != nil {
			return err
		}
	}
	for _, p := range unsets {
		if err := d.set(strings.TrimSpace(p), nil); err != nil {
			return err
		}
	}
	return nil
}

// raw returns the document as JSON, or nil when it is empty.
func (d settingsDoc) raw() (json.RawMessage, error) {
	if len(d) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(map[string]any(d))
	if err != nil {
		return nil, fmt.Errorf("cli: encode settings: %w", err)
	}
	return b, nil
}

// parseKeyValues parses repeated "key=value" flags into a map.
func parseKeyValues(flag string, pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, &usageError{err: fmt.Errorf("invalid --%s %q (want key=value)", flag, kv)}
		}
		out[k] = v
	}
	return out, nil
}

// readInput reads a file, or stdin when name is "-".
func readInput(name string, stdin io.Reader) ([]byte, error) {
	if name == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("cli: read stdin: %w", err)
		}
		return b, nil
	}
	b, err := os.ReadFile(name) //nolint:gosec // the user names the file to read on the command line
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return b, nil
}

// readSettingsFile reads a settings layer object from a JSON or YAML file
// (or stdin for "-") and returns it as JSON.
func readSettingsFile(name string, stdin io.Reader) (json.RawMessage, error) {
	data, err := readInput(name, stdin)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(data)
	if json.Valid(trimmed) {
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, fmt.Errorf("cli: %s: settings must be a JSON object", name)
		}
		return trimmed, nil
	}
	var v map[string]any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("cli: %s: not valid JSON or YAML: %w", name, err)
	}
	if v == nil {
		return nil, fmt.Errorf("cli: %s: settings must be an object", name)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("cli: %s: %w", name, err)
	}
	return b, nil
}

// readURLList reads one URL per line; blank lines and lines starting with
// '#' are skipped.
func readURLList(name string, stdin io.Reader) ([]string, error) {
	data, err := readInput(name, stdin)
	if err != nil {
		return nil, err
	}
	var urls []string
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		urls = append(urls, line)
	}
	return urls, nil
}

// settingsRows renders a settings layer object as PATH VALUE rows, one per
// top-level field (values as compact JSON).
func settingsRows(raw json.RawMessage) [][]string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return [][]string{{"(invalid)", string(raw)}}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "$schema" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, []string{k, compactJSON(m[k])})
	}
	return rows
}

// lookupPath returns the JSON value at a dotted path of a settings object.
func lookupPath(raw json.RawMessage, path string) (json.RawMessage, bool) {
	cur := raw
	for p := range strings.SplitSeq(path, ".") {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(cur, &m); err != nil {
			return nil, false
		}
		next, ok := m[p]
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// compactJSON renders JSON compactly; strings are shown unquoted.
func compactJSON(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
