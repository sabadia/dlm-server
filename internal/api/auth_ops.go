package api

import (
	"context"
	"net/http"
	"sync"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/auth"
)

const tagAuth = "auth"

// tokenMu serialises rotations of the primary token.
var tokenMu sync.Mutex

// WhoAmI is the response of whoami.
type WhoAmI struct {
	Name  string     `json:"name" doc:"The token's name; primary for the token in the daemon's token file."`
	Scope auth.Scope `json:"scope" enum:"admin,read,extension" doc:"admin may do everything; read may only read (GET), and not cookie exports, the token list or MCP; extension (a paired browser extension) may hand downloads to the daemon and follow them, and nothing else."`
	// Primary is true for the token in the token file.
	Primary bool `json:"primary"`
	Local   bool `json:"local" doc:"Whether the request came from the machine the daemon runs on."`
	TLS     bool `json:"tls" doc:"Whether the connection is encrypted."`
}

// TokenList is the response of list-tokens.
type TokenList struct {
	Tokens []auth.Token `json:"tokens" doc:"The named tokens, oldest first. The primary token is not listed."`
}

// CreateTokenBody is the request of create-token.
type CreateTokenBody struct {
	Name  string     `json:"name" minLength:"1" maxLength:"64" doc:"A name to recognise the token by (unique, ignoring case)."`
	Scope auth.Scope `json:"scope" enum:"admin,read" doc:"admin may do everything; read may only read."`
}

// CreatedToken is the response of create-token and rotate-token.
type CreatedToken struct {
	Token string `json:"token" doc:"The secret. It is shown only now: the daemon keeps a hash of a named token, and the primary token's file is the only other copy."`
	// Info describes a named token; absent for a rotated primary token.
	Info *auth.Token `json:"info,omitempty"`
}

func (s *Server) registerAuth() {
	Register(s, huma.Operation{
		OperationID: "whoami",
		Method:      http.MethodGet,
		Path:        "/v1/auth/whoami",
		Summary:     "Who am I",
		Description: "The name and scope of the token the request carries, and whether the request is local and encrypted. A client uses it to tell what it may do.",
		Tags:        []string{tagAuth},
	}, s.whoami)

	Register(s, huma.Operation{
		OperationID: "rotate-token",
		Method:      http.MethodPost,
		Path:        "/v1/auth/token/rotate",
		Summary:     "Replace the primary token",
		Description: "Generates a new primary token, writes it to the daemon's token file and stops accepting the old one at once. The new token is in the answer " +
			"and nowhere else in the API; a client keeps using it from now on. Named tokens are not affected. Needs an admin token.",
		Tags:   []string{tagAuth},
		Errors: []int{http.StatusForbidden, http.StatusServiceUnavailable},
	}, s.rotateToken)

	Register(s, huma.Operation{
		OperationID: "list-tokens",
		Method:      http.MethodGet,
		Path:        "/v1/auth/tokens",
		Summary:     "List named tokens",
		Description: "The named tokens with their scope, creation time and when they were last used (never the secrets). Needs an admin token.",
		Tags:        []string{tagAuth},
		Errors:      []int{http.StatusForbidden, http.StatusServiceUnavailable},
	}, s.listTokens)

	Register(s, huma.Operation{
		OperationID:   "create-token",
		Method:        http.MethodPost,
		Path:          "/v1/auth/tokens",
		Summary:       "Create a named token",
		Description:   "Creates a token with the scope admin or read (read-only) for a dashboard, a script or another device, and returns its secret once. Needs an admin token.",
		Tags:          []string{tagAuth},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.createToken)

	Register(s, huma.Operation{
		OperationID:   "delete-token",
		Method:        http.MethodDelete,
		Path:          "/v1/auth/tokens/{id}",
		Summary:       "Revoke a named token",
		Description:   "Revokes the token at once. Needs an admin token.",
		Tags:          []string{tagAuth},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.deleteToken)
}

// requireAdmin refuses a request whose token is not an admin one.
func requireAdmin(ctx context.Context) error {
	if p, ok := PrincipalOf(ctx); !ok || p.Scope != auth.ScopeAdmin {
		return newProblem(http.StatusForbidden, "this needs an admin token").withCode("auth.admin_required", nil)
	}
	return nil
}

func (s *Server) whoami(ctx context.Context, _ *struct{}) (*struct{ Body WhoAmI }, error) {
	p, _ := PrincipalOf(ctx)
	return &struct{ Body WhoAmI }{Body: WhoAmI{Name: p.Name, Scope: p.Scope, Primary: p.Primary, Local: Local(ctx), TLS: s.cfg.TLSEnabled()}}, nil
}

func (s *Server) rotateToken(ctx context.Context, _ *struct{}) (*struct{ Body CreatedToken }, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.tokenFile == "" {
		return nil, errServicesUnavailable()
	}
	tokenMu.Lock()
	defer tokenMu.Unlock()
	tok, err := auth.RotateToken(s.tokenFile)
	if err != nil {
		return nil, s.fail(ctx, "rotate-token", err, locBody)
	}
	b := []byte(tok)
	s.token.Store(&b)
	s.log.InfoContext(ctx, "the primary API token was rotated")
	return &struct{ Body CreatedToken }{Body: CreatedToken{Token: tok}}, nil
}

func (s *Server) listTokens(ctx context.Context, _ *struct{}) (*struct{ Body TokenList }, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.tokens == nil {
		return nil, errServicesUnavailable()
	}
	list, err := s.tokens.List(ctx)
	if err != nil {
		return nil, s.fail(ctx, "list-tokens", err, locBody)
	}
	if list == nil {
		list = []auth.Token{}
	}
	return &struct{ Body TokenList }{Body: TokenList{Tokens: list}}, nil
}

func (s *Server) createToken(ctx context.Context, in *struct{ Body CreateTokenBody }) (*struct{ Body CreatedToken }, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.tokens == nil {
		return nil, errServicesUnavailable()
	}
	secret, info, err := s.tokens.Create(ctx, in.Body.Name, in.Body.Scope)
	if err != nil {
		return nil, s.fail(ctx, "create-token", err, locBody)
	}
	s.log.InfoContext(ctx, "a named API token was created", "name", info.Name, "scope", string(info.Scope))
	return &struct{ Body CreatedToken }{Body: CreatedToken{Token: secret, Info: &info}}, nil
}

func (s *Server) deleteToken(ctx context.Context, in *struct {
	ID string `path:"id" doc:"Token ID."`
},
) (*struct{}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.tokens == nil {
		return nil, errServicesUnavailable()
	}
	if err := s.tokens.Delete(ctx, in.ID); err != nil {
		return nil, s.fail(ctx, "delete-token", err, locBody)
	}
	s.limiter.forget(in.ID)
	s.log.InfoContext(ctx, "a named API token was revoked", "id", in.ID)
	return &struct{}{}, nil
}
