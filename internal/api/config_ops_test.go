package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/config"
)

func newConfigServer(t *testing.T) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	cfg, err := config.Load(config.LoadOptions{Environ: func() []string { return []string{"DLM_HOME=" + home} }})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, Deps{Token: testToken, Version: testVersion})
	if err != nil {
		t.Fatal(err)
	}
	return s, cfg.File()
}

func configEntry(v *config.View, key string) *config.Entry {
	for i := range v.Entries {
		if v.Entries[i].Key == key {
			return &v.Entries[i]
		}
	}
	return nil
}

func TestConfigOperations(t *testing.T) {
	s, file := newConfigServer(t)

	var v config.View
	callServer(t, s, http.MethodGet, "/v1/config", "", "", http.StatusOK, &v)
	if e := configEntry(&v, "log.level"); e == nil || e.Value != "info" || e.Source != config.SourceDefault || !e.RestartRequired {
		t.Fatalf("log.level = %+v", e)
	}
	if v.RestartPending || v.File != file {
		t.Errorf("view = file %q pending %v", v.File, v.RestartPending)
	}

	callServer(t, s, http.MethodPatch, "/v1/config",
		`{"values":{"log.level":"debug","server.fs_roots":["`+os.TempDir()+`"],"torrent.dht":false,"server.shutdown_timeout":"20s"}}`, "", http.StatusOK, &v)
	if e := configEntry(&v, "log.level"); e.Value != "info" || e.Pending != "debug" {
		t.Errorf("log.level after the patch = %+v: the running value stays, the new one is pending", e)
	}
	if !v.RestartPending {
		t.Error("restartPending is false after a change")
	}
	if data, err := os.ReadFile(file); err != nil || !strings.Contains(string(data), "level: debug") {
		t.Errorf("config file = %q, %v", data, err)
	}

	// Removing a key puts the default back (as pending).
	callServer(t, s, http.MethodPatch, "/v1/config", `{"values":{"log.level":null}}`, "", http.StatusOK, &v)
	if e := configEntry(&v, "log.level"); e.Pending != nil {
		t.Errorf("log.level = %+v: the file no longer differs from the running value", e)
	}

	for name, tc := range map[string]struct{ body, code, loc string }{
		"unknown key": {`{"values":{"no.such":1}}`, "config.unknown_key", "body.values.no.such"},
		"read only":   {`{"values":{"paths.config_dir":"/x"}}`, "config.read_only", "body.values.paths.config_dir"},
		"bad type":    {`{"values":{"torrent.dht":"yes"}}`, "config.bad_value", "body.values.torrent.dht"},
		"invalid":     {`{"values":{"log.level":"loud"}}`, "config.invalid", "body.values"},
	} {
		w := do(t, s, request{method: http.MethodPatch, path: "/v1/config", body: tc.body, headers: bearer(testToken)})
		p := decodeProblem(t, w, http.StatusUnprocessableEntity)
		var coded struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &coded); err != nil || coded.Code != tc.code {
			t.Errorf("%s: code = %q (%v), body %s", name, coded.Code, err, w.Body)
		}
		if len(p.Errors) == 0 || p.Errors[0].Location != tc.loc {
			t.Errorf("%s: errors = %+v, want location %q", name, p.Errors, tc.loc)
		}
	}
	callServer(t, s, http.MethodPatch, "/v1/config", `{"values":{}}`, "", http.StatusUnprocessableEntity, nil)
	// The file still only holds what was valid.
	if data, _ := os.ReadFile(file); strings.Contains(string(data), "loud") || strings.Contains(string(data), "no.such") {
		t.Errorf("a refused change reached the file:\n%s", data)
	}
}
