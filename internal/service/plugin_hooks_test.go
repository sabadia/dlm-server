package service

import (
	"context"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
)

type fakePlugins struct {
	next    domain.Source
	name    string
	handled bool
	seen    []string
}

func (f *fakePlugins) Resolve(_ context.Context, src domain.Source) (domain.Source, string, bool) {
	f.seen = append(f.seen, src.CurrentURL)
	return f.next, f.name, f.handled
}

func TestCreateLetsPluginsResolveTheLink(t *testing.T) {
	f := newSvcFixture(t)
	fp := &fakePlugins{
		handled: true, name: "found.bin",
		next: domain.Source{CurrentURL: "http://127.0.0.1:1/real.bin", OriginalURL: "http://127.0.0.1:1/page", PageURL: "http://127.0.0.1:1/page", ResolverKind: domain.ResolverPlugin},
	}
	f.tasks.d.Plugins = fp

	task, err := f.tasks.Create(t.Context(), CreateTaskInput{Source: domain.Source{CurrentURL: "http://127.0.0.1:1/page"}, Paused: true})
	if err != nil {
		t.Fatal(err)
	}
	if task.Source.CurrentURL != "http://127.0.0.1:1/real.bin" || task.Source.OriginalURL != "http://127.0.0.1:1/page" || task.Source.ResolverKind != domain.ResolverPlugin || task.Name != "found.bin" {
		t.Errorf("task = %+v name %q", task.Source, task.Name)
	}

	// A name the user gave wins over the plugin's.
	task, err = f.tasks.Create(t.Context(), CreateTaskInput{Source: domain.Source{CurrentURL: "http://127.0.0.1:1/page2"}, Name: "mine.bin", Paused: true, AllowDuplicate: true})
	if err != nil || task.Name != "mine.bin" {
		t.Errorf("name = %q, %v", task.Name, err)
	}

	// Sources that are already resolved are not offered to the plugins again.
	fp.seen = nil
	if _, err := f.tasks.Create(t.Context(), CreateTaskInput{
		Source: domain.Source{CurrentURL: "http://127.0.0.1:1/y", ResolverKind: domain.ResolverYtdlp}, Paused: true, AllowDuplicate: true,
	}); err != nil {
		t.Fatal(err)
	}
	if len(fp.seen) != 0 {
		t.Errorf("plugins saw %v", fp.seen)
	}
}

func TestPluginResultsCannotBreakACreate(t *testing.T) {
	f := newSvcFixture(t)
	fp := &fakePlugins{handled: true, name: "../evil", next: domain.Source{CurrentURL: "not a url"}}
	f.tasks.d.Plugins = fp
	task, err := f.tasks.Create(t.Context(), CreateTaskInput{Source: domain.Source{CurrentURL: "http://127.0.0.1:1/ok.bin"}, Paused: true})
	if err != nil {
		t.Fatalf("a bad plugin result must be ignored: %v", err)
	}
	if task.Source.CurrentURL != "http://127.0.0.1:1/ok.bin" || task.Source.ResolverKind == domain.ResolverPlugin {
		t.Errorf("task = %+v", task.Source)
	}

	// A valid source with an invalid name keeps the source and drops the name.
	fp.next, fp.name = domain.Source{CurrentURL: "http://127.0.0.1:1/other.bin", ResolverKind: domain.ResolverPlugin}, "a/b.bin"
	task, err = f.tasks.Create(t.Context(), CreateTaskInput{Source: domain.Source{CurrentURL: "http://127.0.0.1:1/p2"}, Paused: true})
	if err != nil || task.Source.CurrentURL != "http://127.0.0.1:1/other.bin" || task.Name == "a/b.bin" {
		t.Errorf("task = %+v name %q, %v", task.Source, task.Name, err)
	}

	// Declined links are left alone.
	fp.handled = false
	task, err = f.tasks.Create(t.Context(), CreateTaskInput{Source: domain.Source{CurrentURL: "http://127.0.0.1:1/p3"}, Paused: true})
	if err != nil || task.Source.CurrentURL != "http://127.0.0.1:1/p3" {
		t.Errorf("declined: %+v, %v", task.Source, err)
	}
}
