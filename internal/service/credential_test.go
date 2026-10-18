package service

import (
	"bytes"
	"errors"
	"testing"

	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/netclient"
)

func newCredentialService(t *testing.T) (*CredentialService, *svcFixture) {
	t.Helper()
	f := newSvcFixture(t)
	cipher, err := cookies.NewCipher(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return NewCredentialService(Deps{Store: f.store}, cipher), f
}

func TestCredentialServiceLifecycleAndSealing(t *testing.T) {
	ctx := t.Context()
	svc, f := newCredentialService(t)
	c, err := svc.Create(ctx, CreateCredentialInput{Host: " Files.Example.COM ", Username: "ann", Password: "pw1"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "files.example.com" || c.Scheme != domain.CredentialAuto {
		t.Fatalf("created = %+v (host folded to lower case, scheme defaults to auto)", c)
	}
	rec, err := f.store.Credentials().Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rec.SealedPassword, []byte("pw1")) {
		t.Error("the password is stored in the clear")
	}

	if l, ok := svc.Lookup("files.example.com"); !ok || l.Password != "pw1" || l.Username != "ann" || l.Scheme != domain.CredentialAuto {
		t.Fatalf("Lookup = %+v, %v", l, ok)
	}
	pw, user, scheme := "pw2", "ann2", "digest"
	if _, err := svc.Update(ctx, c.ID, UpdateCredentialInput{Password: &pw, Username: &user, Scheme: &scheme}); err != nil {
		t.Fatal(err)
	}
	if l, _ := svc.Lookup("files.example.com"); l.Password != "pw2" || l.Username != "ann2" || l.Scheme != "digest" {
		t.Errorf("Lookup after update = %+v (the cache must follow changes)", l)
	}
	// An update without a password keeps the old one.
	host := "other.example.com"
	if _, err := svc.Update(ctx, c.ID, UpdateCredentialInput{Host: &host}); err != nil {
		t.Fatal(err)
	}
	if l, ok := svc.Lookup("other.example.com"); !ok || l.Password != "pw2" {
		t.Errorf("Lookup after moving the host = %+v, %v", l, ok)
	}
	if _, ok := svc.Lookup("files.example.com"); ok {
		t.Error("the old host still has a login")
	}
	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if err := svc.Delete(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.Lookup("other.example.com"); ok {
		t.Error("a deleted login is still used")
	}
	wantErr(t, svc.Delete(ctx, c.ID), domain.ErrNotFound)
	_, err = svc.Get(ctx, c.ID)
	wantErr(t, err, domain.ErrNotFound)
}

func TestCredentialServiceValidation(t *testing.T) {
	ctx := t.Context()
	svc, _ := newCredentialService(t)
	for name, in := range map[string]CreateCredentialInput{
		"no password": {Host: "a.example", Username: "u"},
		"no username": {Host: "a.example", Password: "p"},
		"bad host":    {Host: "https://a.example", Username: "u", Password: "p"},
		"bad scheme":  {Host: "a.example", Scheme: "ntlm", Username: "u", Password: "p"},
	} {
		if _, err := svc.Create(ctx, in); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	a, err := svc.Create(ctx, CreateCredentialInput{Host: "a.example", Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, CreateCredentialInput{Host: "A.example", Username: "v", Password: "q"}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("duplicate host: %v", err)
	}
	empty := ""
	if _, err := svc.Update(ctx, a.ID, UpdateCredentialInput{Password: &empty}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("empty password: %v", err)
	}
	_, err = svc.Update(ctx, "missing", UpdateCredentialInput{})
	wantErr(t, err, domain.ErrNotFound)
}

func TestCredentialLookupPrefersExactThenMostSpecificWildcard(t *testing.T) {
	ctx := t.Context()
	svc, _ := newCredentialService(t)
	for host, user := range map[string]string{"example.com": "exact", "*.example.com": "wild", "*.files.example.com": "deep"} {
		if _, err := svc.Create(ctx, CreateCredentialInput{Host: host, Username: user, Password: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	for host, want := range map[string]string{
		"example.com":           "exact",
		"www.example.com":       "wild",
		"a.b.example.com":       "wild",
		"a.files.example.com":   "deep",
		"x.y.files.example.com": "deep",
		"files.example.com":     "wild", // the wildcard of files.example.com needs a label before it
		"EXAMPLE.COM":           "exact",
	} {
		l, ok := svc.Lookup(host)
		if !ok || l.Username != want {
			t.Errorf("Lookup(%q) = %+v, %v; want %s", host, l, ok, want)
		}
	}
	for _, host := range []string{"example.org", "com", "notexample.com", ""} {
		if _, ok := svc.Lookup(host); ok {
			t.Errorf("Lookup(%q) found a login", host)
		}
	}
}

// The HTTP clients look logins up through the service.
var _ netclient.CredentialSource = (*CredentialService)(nil)
