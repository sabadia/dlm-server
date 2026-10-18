// Package portable reads and writes download lists in the formats other tools
// use: dlm's own JSON, a plain list of URLs, and aria2's input file.
package portable

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/textproto"
	"sort"
	"strings"
)

// Formats.
const (
	// FormatJSON is {"version":1,"tasks":[{"url":...}]}; a bare array of
	// entries is read too.
	FormatJSON = "json"
	// FormatURLs is one URL per line; blank lines and # comments are skipped.
	FormatURLs = "urls"
	// FormatAria2 is aria2's input file: a line of tab-separated URIs (the
	// first is the link, the others are mirrors), followed by indented
	// key=value option lines.
	FormatAria2 = "aria2"
)

// Limits bound what an import accepts.
const (
	MaxEntries = 10000
	MaxBytes   = 8 << 20
)

// Entry is one download in a list.
type Entry struct {
	URL       string            `json:"url"`
	Mirrors   []string          `json:"mirrors,omitempty"`
	Name      string            `json:"name,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	Referrer  string            `json:"referrer,omitempty"`
	UserAgent string            `json:"userAgent,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// Valid reports whether f is a known format.
func Valid(f string) bool { return f == FormatJSON || f == FormatURLs || f == FormatAria2 }

// Detect guesses the format from a file name: .json, .aria2 or .txt/.list.
// It returns "" when the name says nothing.
func Detect(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".json"):
		return FormatJSON
	case strings.HasSuffix(n, ".aria2"), strings.HasSuffix(n, ".aria2c"):
		return FormatAria2
	case strings.HasSuffix(n, ".txt"), strings.HasSuffix(n, ".list"), strings.HasSuffix(n, ".urls"):
		return FormatURLs
	}
	return ""
}

// Parse reads a list in the given format.
func Parse(format, content string) ([]Entry, error) {
	if len(content) > MaxBytes {
		return nil, fmt.Errorf("portable: the list is larger than %d bytes", MaxBytes)
	}
	var entries []Entry
	var err error
	switch format {
	case FormatJSON:
		entries, err = parseJSON(content)
	case FormatURLs:
		entries = parseURLs(content)
	case FormatAria2:
		entries, err = parseAria2(content)
	default:
		return nil, fmt.Errorf("portable: unknown format %q (use json, urls or aria2)", format)
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > MaxEntries {
		return nil, fmt.Errorf("portable: more than %d entries", MaxEntries)
	}
	return entries, nil
}

type jsonDoc struct {
	Version int     `json:"version"`
	Tasks   []Entry `json:"tasks"`
}

func parseJSON(content string) ([]Entry, error) {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "[") {
		var entries []Entry
		if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
			return nil, fmt.Errorf("portable: invalid JSON: %w", err)
		}
		return nonEmpty(entries)
	}
	var doc jsonDoc
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		return nil, fmt.Errorf("portable: invalid JSON: %w", err)
	}
	if doc.Version > 1 {
		return nil, fmt.Errorf("portable: JSON version %d is newer than this dlm understands", doc.Version)
	}
	return nonEmpty(doc.Tasks)
}

func nonEmpty(entries []Entry) ([]Entry, error) {
	for i, e := range entries {
		if strings.TrimSpace(e.URL) == "" {
			return nil, fmt.Errorf("portable: entry %d has no url", i+1)
		}
	}
	return entries, nil
}

func parseURLs(content string) []Entry {
	var out []Entry
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(nil, MaxBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, Entry{URL: line})
	}
	return out
}

func parseAria2(content string) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(nil, MaxBytes)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			var uris []string
			for _, u := range strings.Split(line, "\t") {
				if u = strings.TrimSpace(u); u != "" {
					uris = append(uris, u)
				}
			}
			e := Entry{URL: uris[0]}
			if len(uris) > 1 {
				e.Mirrors = uris[1:]
			}
			out = append(out, e)
			continue
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("portable: line %d: an option comes before any URI", n)
		}
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			return nil, fmt.Errorf("portable: line %d: expected key=value", n)
		}
		e := &out[len(out)-1]
		switch strings.TrimSpace(key) {
		case "out":
			e.Name = val
		case "dir":
			e.Dir = val
		case "referer":
			e.Referrer = val
		case "user-agent":
			e.UserAgent = val
		case "header":
			name, v, ok := strings.Cut(val, ":")
			if !ok || strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("portable: line %d: header must be \"Name: value\"", n)
			}
			if e.Headers == nil {
				e.Headers = map[string]string{}
			}
			e.Headers[textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))] = strings.TrimSpace(v)
		default:
			// Other aria2 options (split, max-connection-per-server, ...) have
			// no counterpart here and are ignored.
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("portable: read: %w", err)
	}
	return out, nil
}

// Format writes entries in the given format.
func Format(format string, entries []Entry) (string, error) {
	var b strings.Builder
	switch format {
	case FormatJSON:
		doc := jsonDoc{Version: 1, Tasks: entries}
		if doc.Tasks == nil {
			doc.Tasks = []Entry{}
		}
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return "", fmt.Errorf("portable: encode: %w", err)
		}
		b.Write(out)
		b.WriteByte('\n')
	case FormatURLs:
		for _, e := range entries {
			b.WriteString(e.URL)
			b.WriteByte('\n')
		}
	case FormatAria2:
		for _, e := range entries {
			b.WriteString(strings.Join(append([]string{e.URL}, e.Mirrors...), "\t"))
			b.WriteByte('\n')
			for _, kv := range [][2]string{{"out", e.Name}, {"dir", e.Dir}, {"referer", e.Referrer}, {"user-agent", e.UserAgent}} {
				if kv[1] != "" {
					fmt.Fprintf(&b, "  %s=%s\n", kv[0], kv[1])
				}
			}
			names := make([]string, 0, len(e.Headers))
			for name := range e.Headers {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Fprintf(&b, "  header=%s: %s\n", name, e.Headers[name])
			}
		}
	default:
		return "", errors.New("portable: unknown format " + format)
	}
	return b.String(), nil
}
