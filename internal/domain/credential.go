package domain

import (
	"strings"
	"time"
)

// Credential schemes.
const (
	CredentialAuto   = "auto"
	CredentialBasic  = "basic"
	CredentialDigest = "digest"
)

// Credential is a stored login for the sites of a host. The password is
// write-only: it is never part of the entity.
type Credential struct {
	ID string `json:"id"`
	// Host is a host name ("example.com", matching only that host) or a
	// wildcard ("*.example.com", matching its subdomains). Lower case, no port.
	Host string `json:"host"`
	// Scheme is auto (answer whatever the server asks), basic (send with the
	// first request) or digest.
	Scheme    string    `json:"scheme"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ValidCredentialScheme reports whether s is a credential scheme.
func ValidCredentialScheme(s string) bool {
	return s == CredentialAuto || s == CredentialBasic || s == CredentialDigest
}

// ValidateCredentialHost checks a credential host pattern. It returns an error
// matching ErrInvalidArgument.
func ValidateCredentialHost(h string) error {
	name := strings.TrimPrefix(h, "*.")
	if name == "" || strings.ContainsAny(name, "/:@*?# \\\t") || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return InvalidArgument("credential host %q must be a host name such as example.com or *.example.com, without scheme, port or path", h)
	}
	if h != strings.ToLower(h) {
		return InvalidArgument("credential host %q must be lower case", h)
	}
	return nil
}

// Validate checks the credential's own fields.
func (c *Credential) Validate() error {
	if c.ID == "" {
		return InvalidArgument("credential: id is required")
	}
	if err := ValidateCredentialHost(c.Host); err != nil {
		return err
	}
	if !ValidCredentialScheme(c.Scheme) {
		return InvalidArgument("credential %q: scheme must be auto, basic or digest", c.ID)
	}
	if c.Username == "" {
		return InvalidArgument("credential %q: username is required", c.ID)
	}
	if strings.ContainsRune(c.Username, ':') && c.Scheme != CredentialDigest {
		return InvalidArgument("credential %q: a Basic username must not contain a colon", c.ID)
	}
	return nil
}
