package settings

import (
	"slices"
	"testing"
)

func TestDescribeSchemaCoversEveryField(t *testing.T) {
	t.Parallel()
	s := DescribeSchema()
	got := map[string]FieldSchema{}
	for _, f := range s.Fields {
		got[f.Path] = f
		if f.Group == "" || f.Description == "" {
			t.Errorf("%s: group and description are required", f.Path)
		}
	}
	for _, f := range fields() {
		if _, ok := got[f.path]; !ok {
			t.Errorf("%s is missing from the schema", f.path)
		}
	}
	for path := range fieldMetas {
		if _, ok := got[path]; !ok {
			t.Errorf("fieldMetas has an entry for unknown field %s", path)
		}
	}
}

func TestDescribeSchemaDetails(t *testing.T) {
	t.Parallel()
	byPath := map[string]FieldSchema{}
	for _, f := range DescribeSchema().Fields {
		byPath[f.Path] = f
	}
	cases := map[string]string{
		"maxConnections":       "integer",
		"speedLimit":           "integer",
		"tlsVerify":            "boolean",
		"retry.initialBackoff": "duration",
		"torrent.seedRatio":    "number",
		"torrent.trackers":     "array",
		"quota":                "object",
		"downloadDir":          "string",
	}
	for path, want := range cases {
		if got := byPath[path].Type; got != want {
			t.Errorf("%s type = %q, want %q", path, got, want)
		}
	}
	mc := byPath["maxConnections"]
	if mc.Min == nil || *mc.Min != 1 || mc.Max == nil || *mc.Max != MaxConnectionsLimit {
		t.Errorf("maxConnections bounds = %v..%v", mc.Min, mc.Max)
	}
	if !slices.Equal(byPath["checksum"].Levels, []Level{LevelDefault, LevelTask}) {
		t.Errorf("checksum levels = %v", byPath["checksum"].Levels)
	}
	if !slices.Equal(byPath["downloadDir"].Levels, allLevels) {
		t.Errorf("downloadDir levels = %v", byPath["downloadDir"].Levels)
	}
	if !byPath["headers"].Secret {
		t.Error("headers must be flagged secret")
	}
	if len(byPath["fileExistsPolicy"].Enum) != 5 {
		t.Errorf("fileExistsPolicy enum = %v", byPath["fileExistsPolicy"].Enum)
	}
}
