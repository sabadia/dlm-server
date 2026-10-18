package domain

import (
	"errors"
	"testing"
)

func TestCredentialValidation(t *testing.T) {
	good := Credential{ID: "1", Host: "files.example.com", Scheme: CredentialAuto, Username: "ann"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	wild := good
	wild.Host = "*.example.com"
	if err := wild.Validate(); err != nil {
		t.Errorf("wildcard: %v", err)
	}
	digestColon := good
	digestColon.Scheme, digestColon.Username = CredentialDigest, "a:b"
	if err := digestColon.Validate(); err != nil {
		t.Errorf("digest username with a colon: %v", err)
	}
	for name, mut := range map[string]func(*Credential){
		"no id":              func(c *Credential) { c.ID = "" },
		"scheme in host":     func(c *Credential) { c.Host = "https://example.com" },
		"port in host":       func(c *Credential) { c.Host = "example.com:8080" },
		"path in host":       func(c *Credential) { c.Host = "example.com/x" },
		"empty host":         func(c *Credential) { c.Host = "" },
		"bare wildcard":      func(c *Credential) { c.Host = "*." },
		"middle wildcard":    func(c *Credential) { c.Host = "a.*.com" },
		"upper case":         func(c *Credential) { c.Host = "Example.com" },
		"trailing dot":       func(c *Credential) { c.Host = "example.com." },
		"bad scheme":         func(c *Credential) { c.Scheme = "ntlm" },
		"no username":        func(c *Credential) { c.Username = "" },
		"basic with a colon": func(c *Credential) { c.Scheme, c.Username = CredentialBasic, "a:b" },
	} {
		c := good
		mut(&c)
		if err := c.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
}
