// Package urlpattern expands URL patterns such as
// "https://example.com/img[001-100].jpg" into the URLs they stand for.
//
// A range is "[first-last]" or "[first-last:step]". The ends are either both
// numbers (leading zeros set the width: [001-100] gives 001, 002, ... 100) or
// both single letters ([a-e]). Several ranges give every combination, the last
// range changing fastest. A bracket that is not a valid range is literal, so
// IPv6 hosts such as http://[::1]/ pass through unchanged.
package urlpattern

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// DefaultMax bounds how many URLs one pattern may produce.
const DefaultMax = 10000

// ErrTooMany means the pattern expands to more than the allowed number of URLs.
var ErrTooMany = errors.New("urlpattern: pattern expands to too many URLs")

// part is a literal piece or a range of the pattern.
type part struct {
	lit    string
	values []string // non-nil for a range
}

// Expand returns the URLs pattern stands for, at most max (DefaultMax when
// max <= 0). A pattern without ranges is returned as it is.
func Expand(pattern string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = DefaultMax
	}
	parts, err := parse(pattern, limit)
	if err != nil {
		return nil, err
	}
	total := 1
	for _, p := range parts {
		if p.values != nil {
			if total > limit/len(p.values) {
				return nil, fmt.Errorf("%w (limit %d)", ErrTooMany, limit)
			}
			total *= len(p.values)
		}
	}
	out := make([]string, 0, total)
	var build func(i int, prefix string)
	build = func(i int, prefix string) {
		if i == len(parts) {
			out = append(out, prefix)
			return
		}
		if parts[i].values == nil {
			build(i+1, prefix+parts[i].lit)
			return
		}
		for _, v := range parts[i].values {
			build(i+1, prefix+v)
		}
	}
	build(0, "")
	return out, nil
}

// HasPattern reports whether s contains at least one valid range.
func HasPattern(s string) bool {
	parts, err := parse(s, DefaultMax)
	if err != nil {
		return true // an invalid range is still meant as a pattern
	}
	for _, p := range parts {
		if p.values != nil {
			return true
		}
	}
	return false
}

func parse(s string, limit int) ([]part, error) {
	var parts []part
	lit := strings.Builder{}
	for i := 0; i < len(s); {
		if s[i] != '[' {
			lit.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i:], ']')
		if end < 0 {
			lit.WriteString(s[i:])
			break
		}
		values, ok, err := parseRange(s[i+1:i+end], limit)
		if err != nil {
			return nil, err
		}
		if !ok {
			lit.WriteByte('[') // not a range: literal bracket
			i++
			continue
		}
		if lit.Len() > 0 {
			parts = append(parts, part{lit: lit.String()})
			lit.Reset()
		}
		parts = append(parts, part{values: values})
		i += end + 1
	}
	if lit.Len() > 0 {
		parts = append(parts, part{lit: lit.String()})
	}
	return parts, nil
}

// parseRange reads the inside of a bracket. ok is false when it is not range
// syntax at all; err is set when it is one but invalid or too large.
func parseRange(body string, limit int) (values []string, ok bool, err error) {
	rng, stepText, hasStep := strings.Cut(body, ":")
	from, to, found := strings.Cut(rng, "-")
	if !found || from == "" || to == "" {
		return nil, false, nil
	}
	step := 1
	if hasStep {
		if step, err = strconv.Atoi(stepText); err != nil || step < 1 {
			return nil, false, nil //nolint:nilerr // not range syntax: the bracket is literal
		}
	}
	if len(from) == 1 && len(to) == 1 && isLetter(from[0]) && isLetter(to[0]) {
		a, b := from[0], to[0]
		if a > b {
			return nil, true, fmt.Errorf("urlpattern: range [%s] counts backwards", body)
		}
		for c := int(a); c <= int(b); c += step {
			values = append(values, string(rune(c))) //nolint:gosec // c stays between two ASCII letters
		}
		return values, true, nil
	}
	lo, errLo := strconv.ParseUint(from, 10, 63)
	hi, errHi := strconv.ParseUint(to, 10, 63)
	if errLo != nil || errHi != nil {
		return nil, false, nil //nolint:nilerr // not range syntax: the bracket is literal
	}
	if lo > hi {
		return nil, true, fmt.Errorf("urlpattern: range [%s] counts backwards", body)
	}
	if (hi-lo)/uint64(step)+1 > uint64(limit) { //nolint:gosec // step >= 1 and limit > 0
		return nil, true, fmt.Errorf("%w (limit %d)", ErrTooMany, limit)
	}
	width := 0
	if from[0] == '0' || to[0] == '0' {
		width = max(len(from), len(to))
	}
	for n := lo; n <= hi; n += uint64(step) {
		values = append(values, fmt.Sprintf("%0*d", width, n))
	}
	return values, true, nil
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
