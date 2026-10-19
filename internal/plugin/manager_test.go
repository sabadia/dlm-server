package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/storage"
	"github.com/sabadia/dlm-server/internal/storage/sqlite"
)

type fixture struct {
	store storage.Store
	dir   string
	bus   *events.Bus
	m     *Manager
}

func newFixture(t *testing.T, git GitClone) *fixture {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "dlm.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bus := events.New(events.Options{})
	t.Cleanup(bus.Close)
	f := &fixture{store: store, dir: filepath.Join(t.TempDir(), "plugins"), bus: bus}
	f.m = NewManager(Options{Dir: f.dir, Store: store, Bus: bus, Git: git})
	return f
}

// source writes a plugin directory and returns its path.
func source(t *testing.T, manifest, script string, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "manifest.json"), manifest)
	write(t, filepath.Join(dir, "index.js"), script)
	for name, content := range extra {
		write(t, filepath.Join(dir, name), content)
	}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func manifest(name, hooks string) string {
	return `{"name":"` + name + `","version":"1.0.0","title":"T ` + name + `","hooks":{` + hooks + `}}`
}

const host = `
	dlm.on("resolve", function (ctx) {
		if (ctx.url.indexOf("/page/") < 0) return null;
		return {url: ctx.url.replace("/page/", "/file/"), name: "found.bin", referrer: ctx.url, headers: {"X-Added": "1"}, mirrors: ["https://m.example/f"]};
	});`

const refresher = `dlm.on("refresh", function (ctx) { return {url: ctx.url + "?fresh=" + ctx.task.id}; });`

func TestInstallFromAFolderAndRestart(t *testing.T) {
	f := newFixture(t, nil)
	src := source(t, `{"name":"host","version":"1.0.0","title":"Host","description":"d","hooks":{"resolve":{"match":["https://*.example.com/*"]},"refresh":{}},"net":["api.example.com"]}`, host+refresher, map[string]string{"lib/util.txt": "x"})
	p, err := f.m.Install(t.Context(), InstallInput{Source: src})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "host" || !p.Enabled || p.Error != "" || strings.Join(p.Hooks, ",") != "resolve,refresh" || len(p.Net) != 1 || p.Source != src {
		t.Fatalf("installed = %+v", p)
	}
	for _, rel := range []string{"manifest.json", "index.js", "lib/util.txt"} {
		if _, err := os.Stat(filepath.Join(f.dir, "host", filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s was not copied: %v", rel, err)
		}
	}
	if entries, _ := os.ReadDir(f.dir); len(entries) != 1 {
		t.Errorf("the plugins directory holds %d entries (staging left behind?)", len(entries))
	}

	next, name, handled := f.m.Resolve(t.Context(), domain.Source{CurrentURL: "https://www.example.com/page/1"})
	if !handled || name != "found.bin" || next.CurrentURL != "https://www.example.com/file/1" || next.ResolverKind != domain.ResolverPlugin {
		t.Fatalf("resolved = %+v %q %v", next, name, handled)
	}
	if next.OriginalURL != "https://www.example.com/page/1" || next.PageURL != "https://www.example.com/page/1" || next.Referrer != "https://www.example.com/page/1" ||
		next.Headers["X-Added"] != "1" || len(next.Mirrors) != 1 {
		t.Errorf("resolved source = %+v", next)
	}

	// A new manager over the same store and directory (a daemon restart) runs it again.
	m2 := NewManager(Options{Dir: f.dir, Store: f.store})
	if err := m2.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, handled := m2.Resolve(t.Context(), domain.Source{CurrentURL: "https://www.example.com/page/2"}); !handled {
		t.Error("the plugin did not come back after a restart")
	}
	if got := m2.List(t.Context()); len(got) != 1 || got[0].Name != "host" {
		t.Errorf("List = %+v", got)
	}
}

func TestInstallRefusals(t *testing.T) {
	f := newFixture(t, nil)
	ctx := t.Context()
	good := source(t, manifest("good", `"resolve":{}`), host, nil)
	for name, in := range map[string]InstallInput{
		"relative path":   {Source: "plugins/x"},
		"missing dir":     {Source: filepath.Join(t.TempDir(), "nope")},
		"ref on a folder": {Source: good, Ref: "main"},
		"a file":          {Source: filepath.Join(good, "index.js")},
	} {
		if _, err := f.m.Install(ctx, in); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	for name, src := range map[string]string{
		"bad manifest": source(t, `{"name":"x"}`, host, nil),
		"no manifest":  t.TempDir(),
		"missing script": func() string {
			d := source(t, manifest("nos", `"resolve":{}`), host, nil)
			_ = os.Remove(filepath.Join(d, "index.js"))
			return d
		}(),
		"script throws":   source(t, manifest("boom", `"resolve":{}`), `throw new Error("x")`, nil),
		"no handlers":     source(t, manifest("idle", `"resolve":{}`), `var x = 1;`, nil),
		"undeclared hook": source(t, manifest("undecl", `"resolve":{}`), `dlm.on("done", function () {})`, nil),
	} {
		if _, err := f.m.Install(ctx, InstallInput{Source: src}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if entries, _ := os.ReadDir(f.dir); len(entries) != 0 {
		t.Errorf("refused installs left %d entries behind", len(entries))
	}
	if got := f.m.List(ctx); len(got) != 0 {
		t.Errorf("refused installs registered %+v", got)
	}

	if runtime.GOOS != "windows" {
		linked := source(t, manifest("linked", `"resolve":{}`), host, nil)
		if err := os.Symlink("/etc/hosts", filepath.Join(linked, "leak.txt")); err != nil {
			t.Fatal(err)
		}
		if _, err := f.m.Install(ctx, InstallInput{Source: linked}); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a plugin with a link: %v", err)
		}
	}
	big := source(t, manifest("big", `"resolve":{}`), host, map[string]string{"data.bin": strings.Repeat("x", maxPluginBytes+1)})
	if _, err := f.m.Install(ctx, InstallInput{Source: big}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("an oversized plugin: %v", err)
	}

	if _, err := f.m.Install(ctx, InstallInput{Source: good}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Install(ctx, InstallInput{Source: good}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("a second install: %v", err)
	}
}

func TestReplaceUpgradesAndKeepsTheInstallTime(t *testing.T) {
	f := newFixture(t, nil)
	ctx := t.Context()
	v1 := source(t, `{"name":"up","version":"1","hooks":{"resolve":{}}}`, `dlm.on("resolve", function () { return {url: "https://v1/x"}; })`, nil)
	v2 := source(t, `{"name":"up","version":"2","hooks":{"resolve":{}}}`, `dlm.on("resolve", function () { return {url: "https://v2/x"}; })`, nil)
	first, err := f.m.Install(ctx, InstallInput{Source: v1})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	second, err := f.m.Install(ctx, InstallInput{Source: v2, Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != "2" || !second.InstalledAt.Equal(first.InstalledAt) || !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("first %+v, second %+v", first, second)
	}
	if next, _, _ := f.m.Resolve(ctx, domain.Source{CurrentURL: "https://a/b"}); next.CurrentURL != "https://v2/x" {
		t.Errorf("the old version still runs: %s", next.CurrentURL)
	}
	// A failed upgrade leaves the working version in place.
	broken := source(t, `{"name":"up","version":"3","hooks":{"resolve":{}}}`, `throw 1`, nil)
	if _, err := f.m.Install(ctx, InstallInput{Source: broken, Replace: true}); err == nil {
		t.Fatal("a broken upgrade was accepted")
	}
	if p, _ := f.m.Get(ctx, "up"); p.Version != "2" || p.Error != "" {
		t.Errorf("after the failed upgrade: %+v", p)
	}
	if next, _, _ := f.m.Resolve(ctx, domain.Source{CurrentURL: "https://a/b"}); next.CurrentURL != "https://v2/x" {
		t.Errorf("the working version was lost: %s", next.CurrentURL)
	}
}

func TestEnableDisableUninstall(t *testing.T) {
	f := newFixture(t, nil)
	ctx := t.Context()
	src := source(t, manifest("sw", `"resolve":{}`), host, nil)
	p, err := f.m.Install(ctx, InstallInput{Source: src, Disabled: true})
	if err != nil || p.Enabled {
		t.Fatalf("disabled install = %+v, %v", p, err)
	}
	link := domain.Source{CurrentURL: "https://h/page/1"}
	if _, _, handled := f.m.Resolve(ctx, link); handled {
		t.Error("a disabled plugin resolved a link")
	}
	if p, err = f.m.SetEnabled(ctx, "sw", true); err != nil || !p.Enabled {
		t.Fatalf("enable = %+v, %v", p, err)
	}
	if _, _, handled := f.m.Resolve(ctx, link); !handled {
		t.Error("an enabled plugin did not resolve")
	}
	if _, err = f.m.SetEnabled(ctx, "sw", false); err != nil {
		t.Fatal(err)
	}
	if _, _, handled := f.m.Resolve(ctx, link); handled {
		t.Error("a plugin that was switched off still resolves")
	}
	if _, err := f.m.SetEnabled(ctx, "nope", true); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("enable unknown: %v", err)
	}

	write(t, f.m.storageFile("sw"), `{"k":1}`)
	if err := f.m.Uninstall(ctx, "sw"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "sw")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("files remain: %v", err)
	}
	if _, err := os.Stat(f.m.storageFile("sw")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the plugin's data remains: %v", err)
	}
	if _, err := f.m.Get(ctx, "sw"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get after uninstall: %v", err)
	}
	if err := f.m.Uninstall(ctx, "sw"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("uninstall twice: %v", err)
	}
}

func TestBrokenPluginsReportTheirErrorAndSparetheOthers(t *testing.T) {
	f := newFixture(t, nil)
	ctx := t.Context()
	for _, n := range []string{"aaa-gone", "bbb-ok"} {
		if _, err := f.m.Install(ctx, InstallInput{Source: source(t, manifest(n, `"resolve":{}`), host, nil)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(filepath.Join(f.dir, "aaa-gone")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.dir, "bbb-ok", "index.js"), host)
	m2 := NewManager(Options{Dir: f.dir, Store: f.store})
	if err := m2.Load(ctx); err != nil {
		t.Fatalf("a missing plugin must not fail Load: %v", err)
	}
	gone, _ := m2.Get(ctx, "aaa-gone")
	if gone.Error == "" || !gone.Enabled {
		t.Errorf("gone = %+v", gone)
	}
	if _, _, handled := m2.Resolve(ctx, domain.Source{CurrentURL: "https://h/page/1"}); !handled {
		t.Error("the healthy plugin was held back by the broken one")
	}
}

func TestResolveOrderMatchingAndFailures(t *testing.T) {
	f := newFixture(t, nil)
	ctx := t.Context()
	install := func(name, hooks, script string) {
		t.Helper()
		if _, err := f.m.Install(ctx, InstallInput{Source: source(t, manifest(name, hooks), script, nil)}); err != nil {
			t.Fatal(err)
		}
	}
	install("a-throws", `"resolve":{}`, `dlm.on("resolve", function () { throw new Error("nope") })`)
	install("b-junk", `"resolve":{}`, `dlm.on("resolve", function () { return {name: "no url"} })`)
	install("c-declines", `"resolve":{}`, `dlm.on("resolve", function () { return null })`)
	install("d-other-site", `"resolve":{"match":["https://other.test/*"]}`, `dlm.on("resolve", function () { return {url: "https://wrong/"} })`)
	install("e-takes", `"resolve":{"match":["https://site.test/*"]}`, `dlm.on("resolve", function (c) { return {url: "https://cdn.test/e"} })`)
	install("f-late", `"resolve":{}`, `dlm.on("resolve", function () { return {url: "https://cdn.test/f"} })`)

	next, _, handled := f.m.Resolve(ctx, domain.Source{CurrentURL: "https://site.test/v"})
	if !handled || next.CurrentURL != "https://cdn.test/e" {
		t.Fatalf("resolved = %+v %v: broken, declining and non-matching plugins must be skipped, the first taker wins", next, handled)
	}
	if next, _, handled = f.m.Resolve(ctx, domain.Source{CurrentURL: "https://nobody.test/v"}); !handled || next.CurrentURL != "https://cdn.test/f" {
		t.Errorf("fallback = %+v %v", next, handled)
	}
	if _, _, handled = f.m.Resolve(ctx, domain.Source{CurrentURL: "https://site.test/v"}); !handled {
		t.Error("resolve is not repeatable")
	}
}

func TestRefreshHook(t *testing.T) {
	f := newFixture(t, nil)
	ctx := t.Context()
	task, err := domain.NewTask("task-1", time.Now(), domain.NewTaskParams{Source: domain.Source{CurrentURL: "https://h/old", PageURL: "https://h/page"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.Refresh(ctx, task); err != nil {
		t.Fatalf("no plugins: %v", err)
	}
	if _, err := f.m.Install(ctx, InstallInput{Source: source(t, manifest("rf", `"refresh":{}`), refresher, nil)}); err != nil {
		t.Fatal(err)
	}
	next, handled, err := f.m.Refresh(ctx, task)
	if err != nil || !handled || next.CurrentURL != "https://h/old?fresh=task-1" || next.ResolverKind != domain.ResolverPlugin {
		t.Errorf("refresh = %+v %v %v", next, handled, err)
	}

	f2 := newFixture(t, nil)
	if _, err := f2.m.Install(ctx, InstallInput{Source: source(t, manifest("bad", `"refresh":{}`), `dlm.on("refresh", function () { throw new Error("down") })`, nil)}); err != nil {
		t.Fatal(err)
	}
	if _, handled, err := f2.m.Refresh(ctx, task); handled || err == nil || !strings.Contains(err.Error(), "down") {
		t.Errorf("failing plugin: handled %v, err %v", handled, err)
	}
	if _, err := f2.m.Install(ctx, InstallInput{Source: source(t, manifest("good", `"refresh":{}`), refresher, nil)}); err != nil {
		t.Fatal(err)
	}
	if _, handled, err := f2.m.Refresh(ctx, task); !handled || err != nil {
		t.Errorf("a later plugin must make up for a failing one: handled %v, err %v", handled, err)
	}
}

func TestStartErrorAndDoneHooksFollowTaskEvents(t *testing.T) {
	f := newFixture(t, nil)
	ctx := t.Context()
	script := `
		function note(c) { var l = dlm.storage.get("log") || []; l.push(c.event + ":" + c.task.id + ":" + c.task.status + (c.task.error ? ":" + c.task.error.code : "")); dlm.storage.set("log", l); }
		dlm.on("start", note); dlm.on("error", note); dlm.on("done", note);`
	if _, err := f.m.Install(ctx, InstallInput{Source: source(t, manifest("obs", `"start":{},"error":{},"done":{}`), script, nil)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Install(ctx, InstallInput{Source: source(t, manifest("picky", `"done":{"match":["https://nowhere.test/*"]}`), `dlm.on("done", function () { dlm.storage.set("hit", true) })`, nil)}); err != nil {
		t.Fatal(err)
	}

	task, err := domain.NewTask("t1", time.Now(), domain.NewTaskParams{Source: domain.Source{CurrentURL: "https://site.test/f.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Tasks().Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.m.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	<-f.m.Ready()

	set := func(to domain.TaskStatus, e *domain.TaskError) {
		fresh, _ := f.store.Tasks().Get(ctx, "t1")
		fresh.Status, fresh.Error = to, e
		if err := f.store.Tasks().Update(ctx, fresh); err != nil {
			t.Fatal(err)
		}
		f.bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: "t1", Data: service.StatusData{To: to, Error: e}})
	}
	set(domain.StatusRunning, nil)
	f.bus.Publish(events.Event{Type: events.TypeTaskStatus, TaskID: "t1", Data: service.StatusData{To: domain.StatusPaused}}) // no hook
	set(domain.StatusError, &domain.TaskError{Code: "network", Message: "reset"})
	set(domain.StatusCompleted, nil)

	want := []string{"start:t1:running", "error:t1:error:network", "done:t1:completed"}
	var got []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got = readLog(f.m.storageFile("obs"))
		if len(got) == len(want) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("hook log = %v, want %v", got, want)
	}
	if _, err := os.Stat(f.m.storageFile("picky")); err == nil {
		t.Error("a hook ran for a link its patterns do not match")
	}
}

func readLog(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string][]string
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return m["log"]
}

func TestGitSources(t *testing.T) {
	var gotURL, gotRef string
	fake := func(_ context.Context, url, ref, dst string) error {
		gotURL, gotRef = url, ref
		write(t, filepath.Join(dst, "manifest.json"), manifest("fromgit", `"resolve":{}`))
		write(t, filepath.Join(dst, "index.js"), host)
		write(t, filepath.Join(dst, ".git", "config"), "secret")
		return nil
	}
	f := newFixture(t, fake)
	p, err := f.m.Install(t.Context(), InstallInput{Source: "https://example.com/org/plugin.git", Ref: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != "https://example.com/org/plugin.git" || gotRef != "v1" || p.Name != "fromgit" || p.Source != gotURL {
		t.Errorf("clone url %q ref %q, plugin %+v", gotURL, gotRef, p)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "fromgit", ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the .git directory was kept: %v", err)
	}

	failing := newFixture(t, func(context.Context, string, string, string) error { return errors.New("repository not found") })
	if _, err := failing.m.Install(t.Context(), InstallInput{Source: "https://example.com/missing.git"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("failed clone: %v", err)
	}
	if entries, _ := os.ReadDir(failing.dir); len(entries) != 0 {
		t.Errorf("a failed clone left %d entries", len(entries))
	}
}

func TestGitCommandLine(t *testing.T) {
	args := strings.Join(gitArgs("https://example.com/x.git", "v1.2", "/dst"), " ")
	for _, want := range []string{"protocol.allow=never", "protocol.https.allow=always", "--depth=1", "--branch v1.2", "-- https://example.com/x.git /dst"} {
		if !strings.Contains(args, want) {
			t.Errorf("git args %q lack %q", args, want)
		}
	}
	if strings.Contains(strings.Join(gitArgs("https://example.com/x.git", "", "/dst"), " "), "--branch") {
		t.Error("an empty ref must not add --branch")
	}
	for _, ref := range []string{"--upload-pack=x", "-x", "a b", "a;b", ""} {
		if ref == "" {
			continue
		}
		if err := RunGit(t.Context(), "https://example.com/x.git", ref, filepath.Join(t.TempDir(), "d")); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ref %q: %v", ref, err)
		}
	}
	for src, want := range map[string]bool{"https://x/y": true, "HTTPS://x/y": true, "http://x/y": false, "/dir": false, "git@github.com:a/b.git": false, "file:///x": false} {
		if IsGitSource(src) != want {
			t.Errorf("IsGitSource(%q) = %v", src, !want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pw@host/p?token=1#f": "https://host/p", "https://host/p": "https://host/p", "ed2k://|file|x": "ed2k://|file|x",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}
