package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
)

func pluginDir(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := `{"name":"` + name + `","version":"1.0.0","title":"T","hooks":{"resolve":{"match":["https://*.example.com/*"]}},"net":["api.example.com"]}`
	for file, content := range map[string]string{"manifest.json": manifest, "index.js": script} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const goodScript = `dlm.on("resolve", function (ctx) { return {url: ctx.url + "/direct"}; });`

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func TestPluginOperations(t *testing.T) {
	f := newOpsFixture(t)
	var list PluginList
	f.call(t, http.MethodGet, "/v1/plugins", "", http.StatusOK, &list)
	if list.Plugins == nil || len(list.Plugins) != 0 {
		t.Fatalf("empty list = %+v (want [] not null)", list)
	}

	src := pluginDir(t, "demo", goodScript)
	var p domain.Plugin
	f.call(t, http.MethodPost, "/v1/plugins", `{"source":`+jsonString(src)+`}`, http.StatusCreated, &p)
	if p.Name != "demo" || !p.Enabled || p.Error != "" || len(p.Hooks) != 1 || p.Hooks[0] != "resolve" || len(p.Net) != 1 || p.Source != src {
		t.Fatalf("installed = %+v", p)
	}
	f.call(t, http.MethodGet, "/v1/plugins", "", http.StatusOK, &list)
	if len(list.Plugins) != 1 {
		t.Fatalf("list = %+v", list)
	}
	var got domain.Plugin
	f.call(t, http.MethodGet, "/v1/plugins/demo", "", http.StatusOK, &got)
	f.call(t, http.MethodPatch, "/v1/plugins/demo", `{"enabled":false}`, http.StatusOK, &got)
	if got.Enabled {
		t.Errorf("after disabling: %+v", got)
	}
	f.call(t, http.MethodPatch, "/v1/plugins/demo", `{}`, http.StatusOK, &got)
	if got.Enabled {
		t.Errorf("an empty patch changed the plugin: %+v", got)
	}
	f.call(t, http.MethodPatch, "/v1/plugins/demo", `{"enabled":true}`, http.StatusOK, &got)
	if !got.Enabled || got.Error != "" {
		t.Errorf("after enabling: %+v", got)
	}

	// The plugin now resolves links when tasks are created through the API.
	var task domain.Task
	f.call(t, http.MethodPost, "/v1/tasks", `{"url":"https://www.example.com/page","paused":true}`, http.StatusCreated, &task)
	if task.Source.CurrentURL != "https://www.example.com/page/direct" || task.Source.ResolverKind != domain.ResolverPlugin || task.Source.OriginalURL != "https://www.example.com/page" {
		t.Errorf("task source = %+v", task.Source)
	}

	f.call(t, http.MethodPost, "/v1/plugins", `{"source":`+jsonString(src)+`}`, http.StatusConflict, nil)
	v2 := pluginDir(t, "demo", `dlm.on("resolve", function () { return null; });`)
	f.call(t, http.MethodPost, "/v1/plugins", `{"source":`+jsonString(v2)+`,"replace":true,"disabled":true}`, http.StatusCreated, &p)
	if p.Enabled {
		t.Errorf("replaced disabled = %+v", p)
	}
	f.call(t, http.MethodPost, "/v1/plugins", `{"source":"relative/dir"}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPost, "/v1/plugins", `{"source":`+jsonString(pluginDir(t, "broken", `throw new Error("x")`))+`}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodPost, "/v1/plugins", `{"source":""}`, http.StatusUnprocessableEntity, nil)
	f.call(t, http.MethodGet, "/v1/plugins/nope", "", http.StatusNotFound, nil)
	f.call(t, http.MethodPatch, "/v1/plugins/nope", `{"enabled":true}`, http.StatusNotFound, nil)
	f.call(t, http.MethodDelete, "/v1/plugins/nope", "", http.StatusNotFound, nil)

	f.call(t, http.MethodDelete, "/v1/plugins/demo", "", http.StatusNoContent, nil)
	f.call(t, http.MethodGet, "/v1/plugins", "", http.StatusOK, &list)
	if len(list.Plugins) != 0 {
		t.Errorf("after delete: %+v", list)
	}
}

func TestPluginOperationsUnavailableWithoutServices(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/plugins", ""},
		{http.MethodPost, "/v1/plugins", `{"source":"/x"}`},
		{http.MethodGet, "/v1/plugins/x", ""},
		{http.MethodPatch, "/v1/plugins/x", `{"enabled":true}`},
		{http.MethodDelete, "/v1/plugins/x", ""},
	} {
		w := do(t, s, request{method: tc.method, path: tc.path, body: tc.body, headers: bearer(testToken)})
		assertProblem(t, w, http.StatusServiceUnavailable)
	}
}
