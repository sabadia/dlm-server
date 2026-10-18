package service

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/webhook"
)

type fakeTester struct {
	err    error
	target webhook.Target
	event  events.Event
}

func (f *fakeTester) Deliver(_ context.Context, t webhook.Target, e events.Event) error {
	f.target, f.event = t, e
	return f.err
}

func newWebhookService(t *testing.T, tester WebhookTester) (*WebhookService, *svcFixture) {
	t.Helper()
	f := newSvcFixture(t)
	cipher, err := cookies.NewCipher(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return NewWebhookService(Deps{Store: f.store}, cipher, tester), f
}

func TestWebhookServiceLifecycleAndSecretHandling(t *testing.T) {
	ctx := t.Context()
	svc, f := newWebhookService(t, nil)

	w, err := svc.Create(ctx, CreateWebhookInput{URL: " https://example.com/hook ", Events: []string{"Task.Status", "task.status", "queue.completed"}, Secret: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if w.URL != "https://example.com/hook" || !w.Enabled || !w.HasSecret || !slices.Equal(w.Events, []string{"task.status", "queue.completed"}) {
		t.Fatalf("created = %+v", w)
	}

	// The secret is stored sealed, and tied to the webhook (a copy moved to
	// another row does not open).
	rec, err := f.store.Webhooks().Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rec.SealedSecret, []byte("s3cret")) {
		t.Error("the secret is stored in the clear")
	}
	ts, err := svc.Targets(ctx)
	if err != nil || len(ts) != 1 || string(ts[0].Secret) != "s3cret" || ts[0].URL != w.URL {
		t.Fatalf("Targets = %+v, %v", ts, err)
	}
	moved := *rec
	moved.ID = "other"
	if _, err := svc.target(&moved); err == nil {
		t.Error("a sealed secret opened under another webhook ID")
	}

	v0 := svc.Version()
	off := false
	got, err := svc.Update(ctx, w.ID, UpdateWebhookInput{Enabled: &off})
	if err != nil || got.Enabled || !got.HasSecret {
		t.Fatalf("disable: %+v, %v (the secret stays)", got, err)
	}
	if svc.Version() == v0 {
		t.Error("Update did not change the version")
	}
	if ts, _ = svc.Targets(ctx); len(ts) != 0 {
		t.Errorf("a disabled webhook is a target: %+v", ts)
	}
	on, empty := true, ""
	if got, err = svc.Update(ctx, w.ID, UpdateWebhookInput{Enabled: &on, Secret: &empty, Events: &[]string{"task.deleted"}}); err != nil || got.HasSecret || len(got.Events) != 1 {
		t.Fatalf("update: %+v, %v", got, err)
	}
	if ts, _ = svc.Targets(ctx); len(ts) != 1 || len(ts[0].Secret) != 0 {
		t.Errorf("Targets after removing the secret = %+v", ts)
	}

	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	v1 := svc.Version()
	if err := svc.Delete(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if svc.Version() == v1 {
		t.Error("Delete did not change the version")
	}
	wantErr(t, svc.Delete(ctx, w.ID), domain.ErrNotFound)
	_, err = svc.Get(ctx, w.ID)
	wantErr(t, err, domain.ErrNotFound)
}

func TestWebhookServiceValidation(t *testing.T) {
	ctx := t.Context()
	svc, _ := newWebhookService(t, nil)
	for name, in := range map[string]CreateWebhookInput{
		"no events":       {URL: "https://x.example/"},
		"unknown event":   {URL: "https://x.example/", Events: []string{"task.exploded"}},
		"progress":        {URL: "https://x.example/", Events: []string{"task.progress"}},
		"not a url":       {URL: "::", Events: []string{"task.status"}},
		"ftp":             {URL: "ftp://x.example/", Events: []string{"task.status"}},
		"no host":         {URL: "https:///hook", Events: []string{"task.status"}},
		"embedded logins": {URL: "https://user:pw@x.example/", Events: []string{"task.status"}},
	} {
		if _, err := svc.Create(ctx, in); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	w, err := svc.Create(ctx, CreateWebhookInput{URL: "http://localhost:8080/h", Events: []string{"task.status"}, Disabled: true})
	if err != nil || w.Enabled {
		t.Fatalf("create disabled: %+v, %v", w, err)
	}
	bad := "ftp://x"
	if _, err := svc.Update(ctx, w.ID, UpdateWebhookInput{URL: &bad}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("update to a bad URL: %v", err)
	}
	if _, err := svc.Update(ctx, w.ID, UpdateWebhookInput{Events: &[]string{}}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("update to no events: %v", err)
	}
	if got, _ := svc.Get(ctx, w.ID); got.URL != "http://localhost:8080/h" {
		t.Errorf("a refused update changed the webhook: %+v", got)
	}
	_, err = svc.Update(ctx, "missing", UpdateWebhookInput{})
	wantErr(t, err, domain.ErrNotFound)
}

func TestWebhookServiceTest(t *testing.T) {
	ctx := t.Context()
	ft := &fakeTester{}
	svc, _ := newWebhookService(t, ft)
	w, err := svc.Create(ctx, CreateWebhookInput{URL: "https://x.example/", Events: []string{"task.status"}, Secret: "k", Disabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Test(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if ft.event.Type != "webhook.test" || string(ft.target.Secret) != "k" || ft.target.URL != w.URL {
		t.Errorf("delivered %+v to %+v (a disabled webhook can still be tested)", ft.event, ft.target)
	}
	ft.err = errors.New("endpoint answered 500")
	if err := svc.Test(ctx, w.ID); err == nil {
		t.Error("a failed delivery was not reported")
	}
	wantErr(t, svc.Test(ctx, "missing"), domain.ErrNotFound)

	noTester, _ := newWebhookService(t, nil)
	wantErr(t, noTester.Test(ctx, "x"), ErrWebhookTestUnavailable)
}
