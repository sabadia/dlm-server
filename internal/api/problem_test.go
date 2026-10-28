package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/events"
)

type codedProblem struct {
	Type   string            `json:"type"`
	Title  string            `json:"title"`
	Status int               `json:"status"`
	Detail string            `json:"detail"`
	Code   string            `json:"code"`
	Params map[string]string `json:"params"`
	Errors []struct {
		Message  string `json:"message"`
		Location string `json:"location"`
	} `json:"errors"`
}

func problemOf(t *testing.T, f *opsFixture, method, path, body string, status int) codedProblem {
	t.Helper()
	w := do(t, f.s, request{method: method, path: path, body: body, headers: bearer(testToken)})
	assertProblem(t, w, status)
	var p codedProblem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProblemsCarryAStableCode(t *testing.T) {
	f := newOpsFixture(t)
	task := f.createTask(t, `{"url":"`+f.fileURL()+`"}`)

	for _, tc := range []struct {
		name           string
		method, path   string
		body           string
		status         int
		code           string
		params         map[string]string
		detailContains string
	}{
		{"unknown task", http.MethodGet, "/v1/tasks/nope", "", 404, "task.not_found", map[string]string{"entity": "task", "id": "nope"}, "nope"},
		{"unknown queue", http.MethodGet, "/v1/queues/nope", "", 404, "queue.not_found", map[string]string{"entity": "queue", "id": "nope"}, ""},
		{"unknown webhook", http.MethodGet, "/v1/webhooks/nope", "", 404, "webhook.not_found", nil, ""},
		{"duplicate task", http.MethodPost, "/v1/tasks", `{"url":"` + f.fileURL() + `"}`, 409, "task.duplicate", map[string]string{"taskId": task.ID, "status": "queued"}, "duplicate"},
		{"queue name taken", http.MethodPost, "/v1/queues", `{"name":"Main"}`, 409, "queue.name_taken", nil, ""},
		{"default queue is not deletable", http.MethodDelete, "/v1/queues/main", "", 409, "", nil, ""},
		{"invalid input", http.MethodPost, "/v1/categories", `{"name":"x","dir":"../up","extensions":["a"]}`, 422, "invalid_argument", nil, ""},
		{"schema validation", http.MethodPost, "/v1/tasks", `{"priority":"urgent","url":"http://x/"}`, 422, "validation_failed", nil, ""},
		{"settings validation", http.MethodPatch, "/v1/settings", `{"maxConnections":-5}`, 422, "settings.invalid", nil, ""},
		{"state conflict", http.MethodPost, "/v1/tasks/" + task.ID + "/restart", "", 409, "task.invalid_state", map[string]string{"taskId": task.ID, "operation": "restart", "status": "queued"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := problemOf(t, f, tc.method, tc.path, tc.body, tc.status)
			if tc.code != "" && p.Code != tc.code {
				t.Errorf("code = %q, want %q (detail %q)", p.Code, tc.code, p.Detail)
			}
			if p.Code == "" {
				t.Errorf("no code on %+v", p)
			}
			if p.Type != "urn:dlm:problem:"+p.Code {
				t.Errorf("type = %q, want the urn of the code", p.Type)
			}
			for k, v := range tc.params {
				if p.Params[k] != v {
					t.Errorf("params[%s] = %q, want %q (params %v)", k, p.Params[k], v, p.Params)
				}
			}
			if tc.detailContains != "" && !strings.Contains(p.Detail, tc.detailContains) {
				t.Errorf("detail = %q", p.Detail)
			}
			if p.Title == "" || p.Status != tc.status {
				t.Errorf("title %q status %d", p.Title, p.Status)
			}
		})
	}
}

func TestMiddlewareAndFrameworkProblemsHaveCodes(t *testing.T) {
	f := newOpsFixture(t)
	for _, tc := range []struct {
		name    string
		req     request
		status  int
		code    string
		matches string
	}{
		{"no token", request{method: http.MethodGet, path: "/v1/tasks"}, 401, "unauthorized", ""},
		{"bad token", request{method: http.MethodGet, path: "/v1/tasks", headers: bearer("wrong")}, 401, "unauthorized", ""},
		{"wrong content type", request{method: http.MethodPost, path: "/v1/tasks", body: "x", headers: bearerWith(testToken, "Content-Type", "text/plain")}, 415, "unsupported_media_type", ""},
		{"malformed body", request{method: http.MethodPost, path: "/v1/tasks", body: "{", headers: bearer(testToken)}, 400, "bad_request", ""},
		{"too large", request{method: http.MethodPost, path: "/v1/tasks", body: `{"url":"` + strings.Repeat("a", int(MaxBodyBytes)) + `"}`, headers: bearer(testToken)}, 413, "payload_too_large", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, f.s, tc.req)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d; %s", w.Code, tc.status, w.Body)
			}
			var p codedProblem
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if p.Code != tc.code {
				t.Errorf("code = %q, want %q (%s)", p.Code, tc.code, w.Body)
			}
		})
	}
}

func TestBatchItemErrorsCarryCodes(t *testing.T) {
	f := newOpsFixture(t)
	var res struct {
		Results []struct {
			OK    bool `json:"ok"`
			Error *struct {
				Code   string            `json:"code"`
				Params map[string]string `json:"params"`
			} `json:"error"`
		} `json:"results"`
	}
	f.call(t, http.MethodPost, "/v1/tasks/pause", `{"ids":["missing"]}`, http.StatusOK, &res)
	if len(res.Results) != 1 || res.Results[0].OK || res.Results[0].Error == nil ||
		res.Results[0].Error.Code != "task.not_found" || res.Results[0].Error.Params["id"] != "missing" {
		t.Fatalf("results = %+v", res.Results)
	}
}

func TestHealthReportsTheAPIVersion(t *testing.T) {
	f := newOpsFixture(t)
	var h struct {
		APIVersion   int `json:"apiVersion"`
		MinClientAPI int `json:"minClientApi"`
	}
	f.call(t, http.MethodGet, "/v1/health", "", http.StatusOK, &h)
	if h.APIVersion != APIVersion || h.MinClientAPI != MinClientAPI || h.MinClientAPI > h.APIVersion || h.APIVersion < 1 {
		t.Errorf("health = %+v", h)
	}
}

func TestOpenAPIDescribesTheProblemCode(t *testing.T) {
	s := newTestServer(t)
	doc, err := json.Marshal(s.API().OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(doc, &spec); err != nil {
		t.Fatal(err)
	}
	p, ok := spec.Components.Schemas["Problem"]
	if !ok {
		t.Fatalf("no Problem schema; have %d schemas", len(spec.Components.Schemas))
	}
	for _, name := range []string{"code", "params", "status", "title", "detail", "errors"} {
		if _, ok := p.Properties[name]; !ok {
			t.Errorf("Problem lacks %q", name)
		}
	}
	if _, ok := spec.Components.Schemas["ErrorModel"]; ok {
		t.Error("the plain ErrorModel is still in the document: some response does not use Problem")
	}
}

func bearerWith(token, name, value string) map[string]string {
	h := bearer(token)
	h[name] = value
	return h
}

func TestEventStreamSchemaIsATypedUnion(t *testing.T) {
	s := newTestServer(t)
	raw, err := json.Marshal(s.API().OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				OneOf []struct {
					Ref string `json:"$ref"`
				} `json:"oneOf"`
				Discriminator struct {
					PropertyName string            `json:"propertyName"`
					Mapping      map[string]string `json:"mapping"`
				} `json:"discriminator"`
				Properties map[string]struct {
					Ref  string   `json:"$ref"`
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	union, ok := spec.Components.Schemas["StreamEvent"]
	if !ok || union.Discriminator.PropertyName != "type" {
		t.Fatalf("StreamEvent = %+v", union)
	}
	if len(union.OneOf) != len(events.Catalogue()) || len(union.Discriminator.Mapping) != len(events.Catalogue()) {
		t.Errorf("%d variants, %d mappings, want %d", len(union.OneOf), len(union.Discriminator.Mapping), len(events.Catalogue()))
	}
	status := spec.Components.Schemas["TaskStatusEvent"]
	if got := status.Properties["data"].Ref; got != "#/components/schemas/StatusData" {
		t.Errorf("task.status data = %q, want a StatusData reference", got)
	}
	if got := status.Properties["type"].Enum; len(got) != 1 || got[0] != "task.status" {
		t.Errorf("task.status type enum = %v", got)
	}
	for _, k := range events.Catalogue() {
		name := eventSchemaName(k.Type)
		if _, ok := spec.Components.Schemas[name]; !ok {
			t.Errorf("no schema %s for %s", name, k.Type)
		}
		if union.Discriminator.Mapping[string(k.Type)] != "#/components/schemas/"+name {
			t.Errorf("mapping of %s = %q", k.Type, union.Discriminator.Mapping[string(k.Type)])
		}
	}
	if _, ok := spec.Components.Schemas["Event"]; ok {
		t.Error("the untyped Event schema is still published")
	}
}

func TestEventSchemaNames(t *testing.T) {
	for in, want := range map[events.Type]string{
		"task.status": "TaskStatusEvent", "queue.action_cancelled": "QueueActionCancelledEvent", "settings.updated": "SettingsUpdatedEvent",
	} {
		if got := eventSchemaName(in); got != want {
			t.Errorf("eventSchemaName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSettingsETagAndIfMatch(t *testing.T) {
	f := newOpsFixture(t)
	get := func() string {
		w := do(t, f.s, request{method: http.MethodGet, path: "/v1/settings", headers: bearer(testToken)})
		if w.Code != http.StatusOK {
			t.Fatalf("get status %d: %s", w.Code, w.Body)
		}
		return w.Header().Get("ETag")
	}
	etag := get()
	if etag == "" || etag[0] != '"' {
		t.Fatalf("ETag = %q, want a quoted tag", etag)
	}
	if again := get(); again != etag {
		t.Errorf("ETag changed without a write: %q -> %q", etag, again)
	}

	hdr := func(m string) map[string]string {
		h := bearer(testToken)
		h["If-Match"] = m
		h["Content-Type"] = "application/merge-patch+json"
		return h
	}

	// A stale tag is refused with a coded 412 and changes nothing.
	w := do(t, f.s, request{method: http.MethodPatch, path: "/v1/settings", body: `{"maxConnections":7}`, headers: hdr(`"stale"`)})
	assertProblem(t, w, http.StatusPreconditionFailed)
	var p codedProblem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Code != "settings.changed" {
		t.Errorf("code = %q, want settings.changed", p.Code)
	}
	if now := get(); now != etag {
		t.Error("a refused update changed the settings")
	}

	// The current tag lets the update through and yields the next tag.
	w = do(t, f.s, request{method: http.MethodPatch, path: "/v1/settings", body: `{"maxConnections":7}`, headers: hdr(etag)})
	if w.Code != http.StatusOK {
		t.Fatalf("matching update status %d: %s", w.Code, w.Body)
	}
	next := w.Header().Get("ETag")
	if next == "" || next == etag {
		t.Errorf("ETag after update = %q (before %q)", next, etag)
	}
	if now := get(); now != next {
		t.Errorf("GET ETag %q != update ETag %q", now, next)
	}

	// PUT honours If-Match too; without the header it is unconditional.
	w = do(t, f.s, request{method: http.MethodPut, path: "/v1/settings", body: `{"maxConnections":3}`, headers: hdr(etag)})
	assertProblem(t, w, http.StatusPreconditionFailed)
	h := bearer(testToken)
	w = do(t, f.s, request{method: http.MethodPut, path: "/v1/settings", body: `{"maxConnections":3}`, headers: h})
	if w.Code != http.StatusOK {
		t.Fatalf("unconditional PUT status %d: %s", w.Code, w.Body)
	}
}

func TestSettingsSchemaEndpoint(t *testing.T) {
	f := newOpsFixture(t)
	w := do(t, f.s, request{method: http.MethodGet, path: "/v1/settings/schema", headers: bearer(testToken)})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var doc struct {
		Fields []struct {
			Path   string   `json:"path"`
			Type   string   `json:"type"`
			Levels []string `json:"levels"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	found := map[string][]string{}
	for _, fl := range doc.Fields {
		found[fl.Path] = fl.Levels
	}
	if len(found) < 25 {
		t.Errorf("only %d fields described", len(found))
	}
	if got := strings.Join(found["checksum"], ","); got != "default,task" {
		t.Errorf("checksum levels = %q", got)
	}
}

func TestOpenAPI30IsServed(t *testing.T) {
	f := newOpsFixture(t)
	w := do(t, f.s, request{method: http.MethodGet, path: "/openapi-3.0.json"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.0.3" {
		t.Errorf("openapi = %q, want 3.0.3", doc.OpenAPI)
	}
}
