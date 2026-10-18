package client

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Credential is a stored site login. The password is never returned.
type Credential struct {
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	Scheme    string    `json:"scheme"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// CreateCredentialRequest is the request of create-credential.
type CreateCredentialRequest struct {
	Host     string `json:"host"`
	Scheme   string `json:"scheme,omitempty"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// UpdateCredentialRequest is the request of update-credential; nil fields are
// left unchanged.
type UpdateCredentialRequest struct {
	Host     *string `json:"host,omitempty"`
	Scheme   *string `json:"scheme,omitempty"`
	Username *string `json:"username,omitempty"`
	Password *string `json:"password,omitempty"`
}

func credentialPath(id string) string { return "/v1/credentials/" + url.PathEscape(id) }

// ListCredentials calls list-credentials (GET /v1/credentials).
func (c *Client) ListCredentials(ctx context.Context) ([]*Credential, error) {
	var out struct {
		Credentials []*Credential `json:"credentials"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/credentials", nil, &out); err != nil {
		return nil, err
	}
	return out.Credentials, nil
}

// CreateCredential calls create-credential (POST /v1/credentials).
func (c *Client) CreateCredential(ctx context.Context, req CreateCredentialRequest) (*Credential, error) {
	var out Credential
	if err := c.do(ctx, http.MethodPost, "/v1/credentials", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCredential calls get-credential (GET /v1/credentials/{id}).
func (c *Client) GetCredential(ctx context.Context, id string) (*Credential, error) {
	var out Credential
	if err := c.do(ctx, http.MethodGet, credentialPath(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateCredential calls update-credential (PATCH /v1/credentials/{id}).
func (c *Client) UpdateCredential(ctx context.Context, id string, req UpdateCredentialRequest) (*Credential, error) {
	var out Credential
	if err := c.do(ctx, http.MethodPatch, credentialPath(id), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteCredential calls delete-credential (DELETE /v1/credentials/{id}).
func (c *Client) DeleteCredential(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, credentialPath(id), nil, nil)
}
