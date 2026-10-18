package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
)

const tagCredentials = "credentials"

// CredentialList is the response of list-credentials.
type CredentialList struct {
	Credentials []*domain.Credential `json:"credentials" doc:"Every stored login, ordered by host."`
}

// CreateCredentialBody is the request of create-credential.
type CreateCredentialBody struct {
	Host     string `json:"host" minLength:"1" doc:"Host name the login is for: example.com (only that host) or *.example.com (its subdomains). No scheme, port or path."`
	Scheme   string `json:"scheme,omitempty" enum:"auto,basic,digest" doc:"auto (default) answers whatever the server asks for, basic sends the login with the first request, digest answers a Digest challenge."`
	Username string `json:"username" minLength:"1" doc:"User name."`
	Password string `json:"password" minLength:"1" doc:"Password; stored encrypted and never returned."`
}

// UpdateCredentialBody is the request of update-credential. Absent fields are
// unchanged.
type UpdateCredentialBody struct {
	Host     *string `json:"host,omitempty" doc:"New host."`
	Scheme   *string `json:"scheme,omitempty" enum:"auto,basic,digest" doc:"New scheme."`
	Username *string `json:"username,omitempty" doc:"New user name."`
	Password *string `json:"password,omitempty" doc:"New password."`
}

type credentialOutput struct {
	Body *domain.Credential
}

type credentialPathInput struct {
	ID string `path:"id" doc:"Credential ID."`
}

func (s *Server) registerCredentials() {
	Register(s, huma.Operation{
		OperationID: "list-credentials",
		Method:      http.MethodGet,
		Path:        "/v1/credentials",
		Summary:     "List stored logins",
		Description: "Stored logins answer HTTP Basic and Digest challenges of downloads from their host. Passwords are never returned.",
		Tags:        []string{tagCredentials},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.listCredentials)

	Register(s, huma.Operation{
		OperationID:   "create-credential",
		Method:        http.MethodPost,
		Path:          "/v1/credentials",
		Summary:       "Store a login",
		Tags:          []string{tagCredentials},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.createCredential)

	Register(s, huma.Operation{
		OperationID: "get-credential",
		Method:      http.MethodGet,
		Path:        "/v1/credentials/{id}",
		Summary:     "Get a stored login",
		Tags:        []string{tagCredentials},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.getCredential)

	Register(s, huma.Operation{
		OperationID: "update-credential",
		Method:      http.MethodPatch,
		Path:        "/v1/credentials/{id}",
		Summary:     "Update a stored login",
		Tags:        []string{tagCredentials},
		Errors:      []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.updateCredential)

	Register(s, huma.Operation{
		OperationID:   "delete-credential",
		Method:        http.MethodDelete,
		Path:          "/v1/credentials/{id}",
		Summary:       "Delete a stored login",
		Tags:          []string{tagCredentials},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.deleteCredential)
}

func (s *Server) listCredentials(ctx context.Context, _ *struct{}) (*struct{ Body CredentialList }, error) {
	if s.credentials == nil {
		return nil, errServicesUnavailable()
	}
	cs, err := s.credentials.List(ctx)
	if err != nil {
		return nil, s.fail(ctx, "list-credentials", err, locBody)
	}
	return &struct{ Body CredentialList }{Body: CredentialList{Credentials: cs}}, nil
}

func (s *Server) createCredential(ctx context.Context, in *struct{ Body CreateCredentialBody }) (*credentialOutput, error) {
	if s.credentials == nil {
		return nil, errServicesUnavailable()
	}
	c, err := s.credentials.Create(ctx, service.CreateCredentialInput{
		Host: in.Body.Host, Scheme: in.Body.Scheme, Username: in.Body.Username, Password: in.Body.Password,
	})
	if err != nil {
		return nil, s.fail(ctx, "create-credential", err, locBody)
	}
	return &credentialOutput{Body: c}, nil
}

func (s *Server) getCredential(ctx context.Context, in *credentialPathInput) (*credentialOutput, error) {
	if s.credentials == nil {
		return nil, errServicesUnavailable()
	}
	c, err := s.credentials.Get(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "get-credential", err, locBody)
	}
	return &credentialOutput{Body: c}, nil
}

func (s *Server) updateCredential(ctx context.Context, in *struct {
	ID   string `path:"id" doc:"Credential ID."`
	Body UpdateCredentialBody
},
) (*credentialOutput, error) {
	if s.credentials == nil {
		return nil, errServicesUnavailable()
	}
	c, err := s.credentials.Update(ctx, in.ID, service.UpdateCredentialInput{
		Host: in.Body.Host, Scheme: in.Body.Scheme, Username: in.Body.Username, Password: in.Body.Password,
	})
	if err != nil {
		return nil, s.fail(ctx, "update-credential", err, locBody)
	}
	return &credentialOutput{Body: c}, nil
}

func (s *Server) deleteCredential(ctx context.Context, in *credentialPathInput) (*struct{}, error) {
	if s.credentials == nil {
		return nil, errServicesUnavailable()
	}
	if err := s.credentials.Delete(ctx, in.ID); err != nil {
		return nil, s.fail(ctx, "delete-credential", err, locBody)
	}
	return &struct{}{}, nil
}
