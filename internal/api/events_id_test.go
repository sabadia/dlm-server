package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sabadia/dlm-server/internal/config"
)

func TestParseEventID(t *testing.T) {
	for _, tc := range []struct {
		in   string
		boot string
		seq  uint64
		ok   bool
	}{
		{"abc123-42", "abc123", 42, true},
		{"abc-0", "abc", 0, true},
		{"abc", "", 0, false},
		{"-1", "", 0, false},
		{"abc-", "", 0, false},
		{"abc--1", "", 0, false},
		{"abc-1-2", "", 0, false},
		{"a_b-1", "", 0, false},
		{"abc-18446744073709551616", "", 0, false},
	} {
		boot, seq, ok := parseEventID(tc.in)
		if boot != tc.boot || seq != tc.seq || ok != tc.ok {
			t.Errorf("parseEventID(%q) = %q, %d, %v; want %q, %d, %v", tc.in, boot, seq, ok, tc.boot, tc.seq, tc.ok)
		}
	}
	if got := formatEventID("abc", 7); got != "abc-7" {
		t.Errorf("formatEventID = %q", got)
	}
}

func TestBootID(t *testing.T) {
	cfg := config.Default()
	a, err := New(&cfg, Deps{Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(&cfg, Deps{Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	if !validBootID(a.bootID) || a.bootID == b.bootID {
		t.Fatalf("generated boot ids %q, %q: want valid and distinct", a.bootID, b.bootID)
	}
	if _, err := New(&cfg, Deps{Token: testToken, BootID: "has-dash"}); err == nil {
		t.Fatal("New accepted a boot id containing '-'")
	}
}

func TestStreamEventsOpenAPI(t *testing.T) {
	s := newEventsServer(t, nil, 0)
	w := do(t, s, request{method: http.MethodGet, path: "/openapi.json"})
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Parameters  []struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
			Responses map[string]struct {
				Content map[string]json.RawMessage `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	op := doc.Paths["/v1/events"]["get"]
	if op.OperationID != "stream-events" {
		t.Fatalf("operationId = %q", op.OperationID)
	}
	if _, ok := op.Responses["200"].Content["text/event-stream"]; !ok {
		t.Errorf("200 response lacks text/event-stream: %v", op.Responses["200"].Content)
	}
	if _, ok := op.Responses["401"]; !ok {
		t.Error("401 response not documented")
	}
	params := map[string]string{}
	for _, p := range op.Parameters {
		params[p.Name] = p.In
	}
	for name, in := range map[string]string{"types": "query", "taskId": "query", "queueId": "query", "lastEventId": "query", "Last-Event-ID": "header"} {
		if params[name] != in {
			t.Errorf("parameter %s in %q, want %q (params: %v)", name, params[name], in, params)
		}
	}
}
