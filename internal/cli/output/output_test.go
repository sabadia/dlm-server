package output

import (
	"bytes"
	"testing"
)

type sample struct {
	Name    string  `json:"name"`
	Count   int     `json:"count"`
	Ratio   float64 `json:"ratio"`
	Enabled bool    `json:"isEnabled"`
	Note    string  `json:"note,omitempty"`
}

func sampleTable() Table {
	return Table{Columns: []string{"name", "count"}, Rows: [][]string{{"alpha", "1"}, {"b", "22"}}}
}

func TestPrint(t *testing.T) {
	v := sample{Name: "alpha", Count: 3, Ratio: 0.5, Enabled: true}
	tests := []struct {
		format Format
		table  func() Table
		want   string
	}{
		{FormatJSON, sampleTable, "{\n  \"name\": \"alpha\",\n  \"count\": 3,\n  \"ratio\": 0.5,\n  \"isEnabled\": true\n}\n"},
		{FormatYAML, sampleTable, "count: 3\nisEnabled: true\nname: alpha\nratio: 0.5\n"},
		{FormatTable, sampleTable, "NAME   COUNT\nalpha  1\nb      22\n"},
		{FormatTable, nil, "{\n  \"name\": \"alpha\",\n  \"count\": 3,\n  \"ratio\": 0.5,\n  \"isEnabled\": true\n}\n"},
	}
	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			var buf bytes.Buffer
			if err := New(&buf, tt.format).Print(v, tt.table); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("output:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestTableNotCalledForJSON(t *testing.T) {
	var buf bytes.Buffer
	err := New(&buf, FormatJSON).Print(map[string]int{"a": 1}, func() Table {
		t.Error("table func called for JSON output")
		return Table{}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseFormat(t *testing.T) {
	for _, s := range []string{"table", "JSON", "yaml"} {
		if _, err := ParseFormat(s); err != nil {
			t.Errorf("ParseFormat(%q): %v", s, err)
		}
	}
	if _, err := ParseFormat("xml"); err == nil {
		t.Error("ParseFormat(xml) succeeded")
	}
}
