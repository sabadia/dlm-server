package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func loadPlugin(t *testing.T, manifest, script string, env Env) *Instance {
	t.Helper()
	m, err := ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	i, err := Load(context.Background(), m, []byte(script), env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return i
}

const basic = `{"name":"t","version":"1","hooks":{"resolve":{},"done":{},"start":{}}}`

func TestHandlersReceiveArgumentsAndReturnObjects(t *testing.T) {
	i := loadPlugin(t, basic, `
		dlm.on("resolve", function (ctx) {
			if (ctx.url.indexOf("skip") >= 0) return null;
			return {url: ctx.url + "/direct", headers: {"X-A": "1"}, mirrors: ["https://m/x"], n: ctx.task.size};
		});
		dlm.on("done", function () {});
	`, Env{})
	if !i.Handles(HookResolve) || i.Handles(HookStart) {
		t.Errorf("Handles: resolve %v, start %v", i.Handles(HookResolve), i.Handles(HookStart))
	}
	got, err := i.Call(t.Context(), HookResolve, map[string]any{"url": "https://h/a", "task": map[string]any{"size": 42}})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := got.(map[string]any)
	if m["url"] != "https://h/a/direct" || m["n"] != int64(42) {
		t.Fatalf("result = %#v", got)
	}
	if h, _ := m["headers"].(map[string]any); h["X-A"] != "1" {
		t.Errorf("headers = %#v", m["headers"])
	}
	if got, err := i.Call(t.Context(), HookResolve, map[string]any{"url": "https://h/skip"}); err != nil || got != nil {
		t.Errorf("a null result = %v, %v", got, err)
	}
	if got, err := i.Call(t.Context(), HookDone, map[string]any{}); err != nil || got != nil {
		t.Errorf("no result = %v, %v", got, err)
	}
	if _, err := i.Call(t.Context(), HookStart, nil); !errors.Is(err, ErrNoHandler) {
		t.Errorf("a hook without a handler: %v", err)
	}
}

func TestLoadErrors(t *testing.T) {
	m, _ := ParseManifest([]byte(basic))
	for name, script := range map[string]string{
		"syntax error":    `function (`,
		"throws":          `throw new Error("boom")`,
		"undeclared hook": `dlm.on("refresh", function () {})`,
		"not a function":  `dlm.on("resolve", 5)`,
		"reference error": `nope.nothing()`,
		"endless loop":    `while (true) {}`,
	} {
		mm := *m
		mm.Timeout = "100ms"
		if _, err := Load(context.Background(), &mm, []byte(script), Env{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := Load(context.Background(), m, make([]byte, maxScriptBytes+1), Env{}); err == nil {
		t.Error("an oversized script was accepted")
	}
}

func TestExceptionsAndTimeouts(t *testing.T) {
	i := loadPlugin(t, `{"name":"t","version":"1","hooks":{"resolve":{},"done":{}},"timeout":"150ms"}`, `
		dlm.on("resolve", function () { throw new Error("it broke") });
		dlm.on("done", function (ctx) { if (ctx.spin) { while (true) {} } return "fine"; });
	`, Env{})
	if _, err := i.Call(t.Context(), HookResolve, nil); err == nil || !strings.Contains(err.Error(), "it broke") {
		t.Errorf("exception: %v", err)
	}
	start := time.Now()
	_, err := i.Call(t.Context(), HookDone, map[string]any{"spin": true})
	if err == nil || !strings.Contains(err.Error(), "ran longer") || time.Since(start) > 3*time.Second {
		t.Fatalf("endless loop: %v after %s", err, time.Since(start))
	}
	// The runtime still works after an interrupt.
	if got, err := i.Call(t.Context(), HookDone, map[string]any{}); err != nil || got != "fine" {
		t.Errorf("after the timeout: %v, %v", got, err)
	}
	// A cancelled context stops the script too.
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	i2 := loadPlugin(t, `{"name":"t","version":"1","hooks":{"done":{}},"timeout":"30s"}`, `dlm.on("done", function () { while (true) {} })`, Env{})
	if _, err := i2.Call(ctx, HookDone, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled call: %v", err)
	}
	if got, err := i.Call(t.Context(), HookDone, map[string]any{}); err != nil || got != "fine" {
		t.Errorf("a late interrupt leaked into the next call: %v, %v", got, err)
	}
}

func TestAsyncHandlers(t *testing.T) {
	i := loadPlugin(t, basic, `
		dlm.on("resolve", async function (ctx) { return {url: "https://a/b"}; });
		dlm.on("done", async function () { throw new Error("late failure"); });
		dlm.on("start", function () { return new Promise(function () {}); });
	`, Env{})
	if got, err := i.Call(t.Context(), HookResolve, nil); err != nil || asMap(got)["url"] != "https://a/b" {
		t.Errorf("async result = %v, %v", got, err)
	}
	if _, err := i.Call(t.Context(), HookDone, nil); err == nil || !strings.Contains(err.Error(), "late failure") {
		t.Errorf("rejected promise: %v", err)
	}
	if _, err := i.Call(t.Context(), HookStart, nil); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Errorf("pending promise: %v", err)
	}
}

func TestSandboxHasNoAmbientAuthority(t *testing.T) {
	i := loadPlugin(t, basic, `
		dlm.on("resolve", function () {
			return [typeof require, typeof process, typeof fetch, typeof XMLHttpRequest, typeof os, typeof fs, typeof module].join(",");
		});
	`, Env{})
	got, err := i.Call(t.Context(), HookResolve, nil)
	if err != nil || got != "undefined,undefined,undefined,undefined,undefined,undefined,undefined" {
		t.Errorf("globals = %v, %v", got, err)
	}
}

func TestHTTPIsLimitedToTheDeclaredHosts(t *testing.T) {
	var mu sync.Mutex
	var gotBody, gotType, gotHeader string
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		if r.URL.Path == "/echo" {
			gotBody, gotType = string(b), r.Header.Get("Content-Type")
		}
		if r.URL.Path == "/json" {
			gotHeader = r.Header.Get("X-Plugin")
		}
		mu.Unlock()
		switch r.URL.Path {
		case "/json":
			w.Header().Set("X-Reply", "yes")
			_, _ = io.WriteString(w, `{"link":"https://cdn/x","n":3}`)
		case "/bounce":
			http.Redirect(w, r, "http://localhost:1/never", http.StatusFound)
		case "/huge":
			_, _ = w.Write(make([]byte, maxHTTPBody+10))
		case "/slow":
			time.Sleep(2 * time.Second)
		default:
			_, _ = io.WriteString(w, "plain")
		}
	}))
	t.Cleanup(allowed.Close)
	host := strings.Split(strings.TrimPrefix(allowed.URL, "http://"), ":")[0] // 127.0.0.1

	script := `
		dlm.on("resolve", function (ctx) {
			var o = {};
			try { dlm.http.get(ctx.other); o.other = "reached"; } catch (e) { o.other = String(e); }
			try { dlm.http.get(ctx.base + "/bounce"); o.bounce = "followed"; } catch (e) { o.bounce = String(e); }
			try { dlm.http.get(ctx.base + "/huge"); o.huge = "read"; } catch (e) { o.huge = String(e); }
			try { dlm.http.get(ctx.base + "/slow", {timeout: 100}); o.slow = "waited"; } catch (e) { o.slow = String(e); }
			try { dlm.http.get("ftp://" + ctx.host + "/x"); o.ftp = "sent"; } catch (e) { o.ftp = String(e); }
			var r = dlm.http.get(ctx.base + "/json", {headers: {"X-Plugin": "p"}});
			o.status = r.status; o.reply = r.headers["x-reply"]; o.link = r.json().link; o.body = r.body.length > 0;
			var p = dlm.http.post(ctx.base + "/echo", {a: 1});
			o.postStatus = p.status;
			return o;
		});`
	manifest := `{"name":"t","version":"1","hooks":{"resolve":{}},"net":["` + host + `"],"timeout":"20s"}`
	i := loadPlugin(t, manifest, script, Env{HTTP: allowed.Client()})
	res, err := i.Call(t.Context(), HookResolve, map[string]any{"base": allowed.URL, "other": "http://localhost:1/x", "host": host})
	if err != nil {
		t.Fatal(err)
	}
	o := asMap(res)
	for key, want := range map[string]string{
		"other": "may not contact", "bounce": "may not contact", "huge": "larger than", "slow": "deadline", "ftp": "not an http(s) URL",
	} {
		if s, _ := o[key].(string); !strings.Contains(s, want) {
			t.Errorf("%s = %q, want it to mention %q", key, o[key], want)
		}
	}
	if o["status"] != int64(200) || o["reply"] != "yes" || o["link"] != "https://cdn/x" || o["body"] != true || o["postStatus"] != int64(200) {
		t.Errorf("results = %#v", o)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotHeader != "p" {
		t.Errorf("the X-Plugin header = %q", gotHeader)
	}
	if gotBody != `{"a":1}` || gotType != "application/json" {
		t.Errorf("post body %q type %q", gotBody, gotType)
	}
}

func TestHTTPWithoutNetAccess(t *testing.T) {
	i := loadPlugin(t, basic, `dlm.on("resolve", function () { dlm.http.get("https://example.com/"); });`, Env{HTTP: http.DefaultClient})
	if _, err := i.Call(t.Context(), HookResolve, nil); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("got %v", err)
	}
}

func TestStoragePersistsAndIsBounded(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plugins", "t.json")
	script := `
		dlm.on("resolve", function (ctx) {
			var o = {};
			if (ctx.write) {
				dlm.storage.set("token", {id: 7, tags: ["a", "b"]});
				dlm.storage.set("n", 5);
				dlm.storage.set("gone", "x"); dlm.storage.delete("gone");
			}
			o.token = dlm.storage.get("token"); o.n = dlm.storage.get("n"); o.missing = dlm.storage.get("nope");
			o.keys = dlm.storage.keys();
			return o;
		});
		dlm.on("done", function () { dlm.storage.set("big", new Array(300000).join("x")); });
		dlm.on("start", function () { dlm.storage.set("", 1); });`
	i := loadPlugin(t, basic, script, Env{StorageFile: file})
	if _, err := i.Call(t.Context(), HookResolve, map[string]any{"write": true}); err != nil {
		t.Fatal(err)
	}
	// A new instance (a daemon restart) sees the stored values.
	i2 := loadPlugin(t, basic, script, Env{StorageFile: file})
	res, err := i2.Call(t.Context(), HookResolve, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	o := asMap(res)
	tok, _ := o["token"].(map[string]any)
	if fmt.Sprint(tok["id"]) != "7" || fmt.Sprint(o["n"]) != "5" || o["missing"] != nil {
		t.Errorf("stored = %#v", o)
	}
	if b, _ := json.Marshal(o["keys"]); string(b) != `["n","token"]` {
		t.Errorf("keys = %s", b)
	}
	if _, err := i2.Call(t.Context(), HookDone, nil); err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Errorf("oversized value: %v", err)
	}
	if _, err := i2.Call(t.Context(), HookStart, nil); err == nil || !strings.Contains(err.Error(), "key") {
		t.Errorf("empty key: %v", err)
	}
	noStore := loadPlugin(t, basic, `dlm.on("resolve", function () { dlm.storage.get("x"); });`, Env{})
	if _, err := noStore.Call(t.Context(), HookResolve, nil); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("storage without a file: %v", err)
	}
}

func TestCallsOfOnePluginDoNotOverlap(t *testing.T) {
	i := loadPlugin(t, basic, `
		var busy = false;
		dlm.on("resolve", function () {
			if (busy) throw new Error("overlap");
			busy = true;
			var end = Date.now() + 20; while (Date.now() < end) {}
			busy = false;
			return "ok";
		});`, Env{})
	errs := make(chan error, 8)
	for range 8 {
		go func() { _, err := i.Call(t.Context(), HookResolve, nil); errs <- err }()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func TestLogDoesNotPanic(t *testing.T) {
	i := loadPlugin(t, basic, `dlm.on("resolve", function () { dlm.log.info("a", 1, {x: 2}); dlm.log.error("b"); dlm.log.debug(); dlm.log.warn(null); return dlm.plugin.name; });`, Env{})
	if got, err := i.Call(t.Context(), HookResolve, nil); err != nil || got != "t" {
		t.Errorf("got %v, %v", got, err)
	}
}

// asMap returns v as a JSON object, or nil.
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
