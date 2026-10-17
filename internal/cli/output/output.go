// Package output renders command results as a table, JSON or YAML, selected
// by the CLI's --output flag.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"go.yaml.in/yaml/v3"
)

// Format is an output format.
type Format string

// Supported formats.
const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
)

// Formats lists the supported format names, for flag help and completion.
func Formats() []string {
	return []string{string(FormatTable), string(FormatJSON), string(FormatYAML)}
}

// ParseFormat validates a format name (case-insensitive).
func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(s)); f {
	case FormatTable, FormatJSON, FormatYAML:
		return f, nil
	default:
		return "", fmt.Errorf("output: unknown format %q (want %s)", s, strings.Join(Formats(), ", "))
	}
}

// Table is the tabular rendering of a value.
type Table struct {
	Columns []string
	Rows    [][]string
}

// Printer writes values in one format.
type Printer struct {
	w      io.Writer
	format Format
}

// New returns a Printer writing to w in format f.
func New(w io.Writer, f Format) *Printer { return &Printer{w: w, format: f} }

// Format returns the printer's format.
func (p *Printer) Format() Format { return p.format }

// Print renders v. JSON and YAML render v itself (YAML uses the same field
// names as JSON); the table format renders the result of table, which is
// only called for that format.
func (p *Printer) Print(v any, table func() Table) error {
	switch p.format {
	case FormatJSON:
		return p.printJSON(v)
	case FormatYAML:
		return p.printYAML(v)
	case FormatTable:
		if table == nil {
			return p.printJSON(v)
		}
		return p.printTable(table())
	default:
		return fmt.Errorf("output: unknown format %q", p.format)
	}
}

func (p *Printer) printJSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("output: encode json: %w", err)
	}
	data = append(data, '\n')
	if _, err := p.w.Write(data); err != nil {
		return fmt.Errorf("output: write: %w", err)
	}
	return nil
}

func (p *Printer) printYAML(v any) error {
	// Round-trip through JSON so YAML keys match the API's JSON field names
	// (camelCase) and honour json tags such as omitempty.
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("output: encode yaml: %w", err)
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return fmt.Errorf("output: encode yaml: %w", err)
	}
	enc := yaml.NewEncoder(p.w)
	enc.SetIndent(2)
	if err := enc.Encode(numbersToYAML(generic)); err != nil {
		return fmt.Errorf("output: write yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("output: write yaml: %w", err)
	}
	return nil
}

// numbersToYAML converts json.Number values to int64 or float64 so the YAML
// encoder emits them as numbers rather than strings.
func numbersToYAML(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = numbersToYAML(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = numbersToYAML(e)
		}
		return t
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	default:
		return v
	}
}

func (p *Printer) printTable(t Table) error {
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	if len(t.Columns) > 0 {
		header := make([]string, len(t.Columns))
		for i, c := range t.Columns {
			header[i] = strings.ToUpper(c)
		}
		if _, err := fmt.Fprintln(tw, strings.Join(header, "\t")); err != nil {
			return fmt.Errorf("output: write table: %w", err)
		}
	}
	for _, row := range t.Rows {
		if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
			return fmt.Errorf("output: write table: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("output: write table: %w", err)
	}
	return nil
}
