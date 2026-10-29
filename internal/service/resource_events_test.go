package service

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/logging"
)

// changes lists the resource.changed events delivered so far as
// "kind/id/op" strings.
func changes(sub *events.Subscription) []string {
	var out []string
	for _, e := range collect(sub) {
		d, ok := e.Data.(events.ResourceChangedData)
		if !ok {
			out = append(out, fmt.Sprintf("unexpected %T", e.Data))
			continue
		}
		out = append(out, d.Kind+"/"+d.ID+"/"+d.Op)
	}
	return out
}

func watchResources(f *svcFixture, t *testing.T) *events.Subscription {
	t.Helper()
	return f.bus.Subscribe(t.Context(), events.Filter{Types: []events.Type{events.TypeResourceChanged}})
}

func expect(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: events = %v, want %v", what, got, want)
	}
	for i := range want {
		// IDs are generated: compare kind and op, and the id when the test names one.
		gk, wk := got[i], want[i]
		if gk != wk {
			t.Errorf("%s: event %d = %q, want %q", what, i, gk, wk)
		}
	}
}

func TestCategoryChangesAreAnnounced(t *testing.T) {
	f := newSvcFixture(t)
	sub := watchResources(f, t)
	svc := NewCategoryService(f.tasks.d)
	c, err := svc.Create(t.Context(), CreateCategoryInput{Name: "Video", Dir: "Videos", Extensions: []string{"mp4"}})
	if err != nil {
		t.Fatal(err)
	}
	name := "Films"
	if _, err := svc.Update(t.Context(), c.ID, UpdateCategoryInput{Name: &name}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(t.Context(), c.ID); err != nil {
		t.Fatal(err)
	}
	expect(t, "category", changes(sub), "category/"+c.ID+"/created", "category/"+c.ID+"/updated", "category/"+c.ID+"/deleted")

	// A refused change announces nothing.
	if err := svc.Delete(t.Context(), "nope"); err == nil {
		t.Fatal("deleting an unknown category worked")
	}
	if got := changes(sub); len(got) != 0 {
		t.Errorf("a failed delete announced %v", got)
	}
}

func TestWebhookAndCredentialChangesAreAnnounced(t *testing.T) {
	cipher, err := cookies.NewCipher(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := newSvcFixture(t)
	sub := watchResources(f, t)

	hooks := NewWebhookService(f.tasks.d, cipher, nil)
	w, err := hooks.Create(t.Context(), CreateWebhookInput{URL: "https://example.com/h", Events: []string{"task.status"}})
	if err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := hooks.Update(t.Context(), w.ID, UpdateWebhookInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if err := hooks.Delete(t.Context(), w.ID); err != nil {
		t.Fatal(err)
	}
	expect(t, "webhook", changes(sub), "webhook/"+w.ID+"/created", "webhook/"+w.ID+"/updated", "webhook/"+w.ID+"/deleted")

	creds := NewCredentialService(f.tasks.d, cipher)
	c, err := creds.Create(t.Context(), CreateCredentialInput{Host: "files.example.com", Username: "ann", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	pw := "pw2"
	if _, err := creds.Update(t.Context(), c.ID, UpdateCredentialInput{Password: &pw}); err != nil {
		t.Fatal(err)
	}
	if err := creds.Delete(t.Context(), c.ID); err != nil {
		t.Fatal(err)
	}
	expect(t, "credential", changes(sub), "credential/"+c.ID+"/created", "credential/"+c.ID+"/updated", "credential/"+c.ID+"/deleted")
}

func TestCookieChangesAreAnnounced(t *testing.T) {
	f := newSvcFixture(t)
	sub := watchResources(f, t)
	cipher, err := cookies.NewCipher(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	jar := cookies.NewStore(f.store.Cookies(), cipher, cookies.Options{})
	svc := NewCookieService(jar).WithBus(f.bus)

	const doc = `[{"domain":"example.com","name":"k","value":"v","path":"/"}]`
	if _, err := svc.Import(t.Context(), "json", doc); err != nil {
		t.Fatal(err)
	}
	expect(t, "import", changes(sub), "cookie//created")
	if n, err := svc.DeleteDomain(t.Context(), "example.com"); err != nil || n != 1 {
		t.Fatalf("DeleteDomain = %d, %v", n, err)
	}
	expect(t, "delete domain", changes(sub), "cookie//deleted")
	if n, _ := svc.DeleteDomain(t.Context(), "example.com"); n != 0 || len(changes(sub)) != 0 {
		t.Error("deleting nothing announced a change")
	}
	if _, err := svc.Import(t.Context(), "json", "{"); err == nil {
		t.Fatal("an unreadable document was imported")
	}
	if got := changes(sub); len(got) != 0 {
		t.Errorf("a failed import announced %v", got)
	}
	if err := svc.Delete(t.Context(), "nope"); err == nil {
		t.Fatal("deleting an unknown cookie worked")
	}
	if got := changes(sub); len(got) != 0 {
		t.Errorf("a failed delete announced %v", got)
	}
}

type stubPlugins struct{}

func (stubPlugins) List(context.Context) []*domain.Plugin { return nil }
func (stubPlugins) Get(context.Context, string) (*domain.Plugin, error) {
	return &domain.Plugin{Name: "p"}, nil
}

func (stubPlugins) Install(context.Context, domain.PluginInstall) (*domain.Plugin, error) {
	return &domain.Plugin{Name: "p"}, nil
}

func (stubPlugins) SetEnabled(context.Context, string, bool) (*domain.Plugin, error) {
	return &domain.Plugin{Name: "p"}, nil
}
func (stubPlugins) Uninstall(context.Context, string) error { return nil }
func (stubPlugins) Logs(context.Context, string) (*logging.Ring, error) {
	return logging.NewRing(1), nil
}

func TestPluginChangesAreAnnounced(t *testing.T) {
	f := newSvcFixture(t)
	sub := watchResources(f, t)
	svc := NewPluginService(stubPlugins{}).WithBus(f.bus)
	if _, err := svc.Install(t.Context(), domain.PluginInstall{Source: "/x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetEnabled(t.Context(), "p", false); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(t.Context(), "p"); err != nil {
		t.Fatal(err)
	}
	expect(t, "plugin", changes(sub), "plugin/p/created", "plugin/p/updated", "plugin/p/deleted")

	quiet := NewPluginService(stubPlugins{}) // no bus: no events, no panic
	if _, err := quiet.Install(t.Context(), domain.PluginInstall{Source: "/x"}); err != nil {
		t.Fatal(err)
	}
	if got := changes(sub); len(got) != 0 {
		t.Errorf("a service without a bus announced %v", got)
	}
}
