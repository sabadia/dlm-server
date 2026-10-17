package domain

import (
	"strings"
)

// Category is an auto-sort rule: files matching an extension or MIME type
// are saved under Dir.
type Category struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Extensions are lower-case, without the leading dot; multi-part
	// extensions such as "tar.gz" are allowed.
	Extensions []string `json:"extensions,omitempty"`
	// MIMETypes are lower-case "type/subtype" or "type/*" patterns.
	MIMETypes []string `json:"mimeTypes,omitempty"`
	// Dir is the target directory (absolute, or relative to the effective
	// download directory).
	Dir      string `json:"dir"`
	Position int64  `json:"position"`
}

// Validate checks the category. It returns an error matching
// ErrInvalidArgument.
func (c *Category) Validate() error {
	if c.ID == "" {
		return InvalidArgument("category: id is required")
	}
	if strings.TrimSpace(c.Name) == "" {
		return InvalidArgument("category %q: name is required", c.ID)
	}
	if strings.TrimSpace(c.Dir) == "" {
		return InvalidArgument("category %q: dir is required", c.ID)
	}
	if len(c.Extensions) == 0 && len(c.MIMETypes) == 0 {
		return InvalidArgument("category %q: at least one extension or MIME type is required", c.ID)
	}
	for _, e := range c.Extensions {
		if e == "" || e != strings.ToLower(e) || strings.HasPrefix(e, ".") || strings.ContainsAny(e, `/\ `) {
			return InvalidArgument("category %q: invalid extension %q (lower-case, no leading dot)", c.ID, e)
		}
	}
	for _, m := range c.MIMETypes {
		typ, sub, ok := strings.Cut(m, "/")
		if !ok || typ == "" || sub == "" || typ == "*" || m != strings.ToLower(m) || strings.ContainsAny(m, " ;") {
			return InvalidArgument("category %q: invalid MIME type %q", c.ID, m)
		}
	}
	return nil
}

// Matches reports whether a file with the given name or MIME type belongs to
// the category. Extensions match case-insensitively at the end of filename;
// MIME types match exactly or by "type/*" wildcard, ignoring parameters.
func (c *Category) Matches(filename, mimeType string) bool {
	name := strings.ToLower(filename)
	for _, e := range c.Extensions {
		if strings.HasSuffix(name, "."+e) {
			return true
		}
	}
	mt, _, _ := strings.Cut(mimeType, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	if mt == "" {
		return false
	}
	for _, p := range c.MIMETypes {
		if p == mt {
			return true
		}
		if prefix, ok := strings.CutSuffix(p, "/*"); ok && strings.HasPrefix(mt, prefix+"/") {
			return true
		}
	}
	return false
}
