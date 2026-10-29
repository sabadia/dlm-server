package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/auth"
)

const tagPairing = "pairing"

// PairingBody is the request of start-pairing.
type PairingBody struct {
	Name     string `json:"name,omitempty" maxLength:"64" doc:"What the client calls itself, shown to the user and used to name the token. Default: the browser's name and \"extension\"."`
	Browser  string `json:"browser,omitempty" enum:"chrome,chromium,edge,brave,opera,vivaldi,firefox,other" doc:"The browser the extension runs in."`
	ClientID string `json:"clientId,omitempty" maxLength:"128" doc:"The extension's id (Chrome) or add-on id (Firefox)."`
	Version  string `json:"version,omitempty" maxLength:"32" doc:"The extension's version."`
}

// PairingList is the response of list-pairings.
type PairingList struct {
	Pairings []auth.Pairing `json:"pairings" doc:"The pairings waiting for an answer, oldest first."`
}

// PairingState is the response of get-pairing.
type PairingState struct {
	auth.Pairing
	// Token is the issued token. It is in the answer once, the first time the
	// pairing is read after the user allowed it.
	Token string `json:"token,omitempty" doc:"The token of an approved pairing. Present once: the first time the pairing is read after it was allowed."`
}

func pairingLocalOnly(ctx context.Context) error {
	if !Local(ctx) {
		return newProblem(http.StatusForbidden, "pairing only works from the machine the daemon runs on").withCode("pairing.local_only", nil)
	}
	return nil
}

// pairingGate checks what every pairing operation needs: an admin token, the
// local machine and the service.
func (s *Server) pairingGate(ctx context.Context) error {
	if err := requireAdmin(ctx); err != nil {
		return err
	}
	if err := pairingLocalOnly(ctx); err != nil {
		return err
	}
	if s.pairings == nil {
		return errServicesUnavailable()
	}
	return nil
}

func (s *Server) registerPairing() {
	errs := []int{http.StatusForbidden, http.StatusServiceUnavailable}
	Register(s, huma.Operation{
		OperationID:   "start-pairing",
		Method:        http.MethodPost,
		Path:          "/v1/pairing",
		Summary:       "Ask to pair a browser extension",
		Description:   "A browser extension asks to be given a token of its own. The request waits for the user to allow or deny it in the DLM app (event pairing.requested); it carries a six-digit code the extension shows and the user compares. Called by the native host with the primary token, so the extension never sees an admin token. Only from the machine the daemon runs on. At most 5 pairings wait at once (409 pairing.too_many).",
		Tags:          []string{tagPairing},
		DefaultStatus: http.StatusCreated,
		Errors:        append([]int{http.StatusConflict, http.StatusUnprocessableEntity}, errs...),
	}, s.startPairing)

	Register(s, huma.Operation{
		OperationID: "list-pairings",
		Method:      http.MethodGet,
		Path:        "/v1/pairing",
		Summary:     "List pairings waiting for an answer",
		Description: "The pairings the user has not answered yet, for an app that started after the request was made. Needs an admin token, from the machine the daemon runs on.",
		Tags:        []string{tagPairing},
		Errors:      errs,
	}, s.listPairings)

	Register(s, huma.Operation{
		OperationID: "get-pairing",
		Method:      http.MethodGet,
		Path:        "/v1/pairing/{id}",
		Summary:     "Read a pairing",
		Description: "Where a pairing is: pending, approved, denied or expired. After the user allowed it, the first read also returns the token, once. A pairing is kept for a few minutes after it ends.",
		Tags:        []string{tagPairing},
		Errors:      append([]int{http.StatusNotFound}, errs...),
	}, s.getPairing)

	Register(s, huma.Operation{
		OperationID: "approve-pairing",
		Method:      http.MethodPost,
		Path:        "/v1/pairing/{id}/approve",
		Summary:     "Allow a pairing",
		Description: "The user allows the pairing: a token with the extension scope is issued and handed to the extension the next time it reads the pairing. If it is not read within five minutes the token is revoked. Needs an admin token, from the machine the daemon runs on.",
		Tags:        []string{tagPairing},
		Errors:      append([]int{http.StatusNotFound, http.StatusConflict}, errs...),
	}, s.approvePairing)

	Register(s, huma.Operation{
		OperationID: "deny-pairing",
		Method:      http.MethodPost,
		Path:        "/v1/pairing/{id}/deny",
		Summary:     "Refuse a pairing",
		Description: "The user refuses the pairing. Needs an admin token, from the machine the daemon runs on.",
		Tags:        []string{tagPairing},
		Errors:      append([]int{http.StatusNotFound, http.StatusConflict}, errs...),
	}, s.denyPairing)
}

type pairingID struct {
	ID string `path:"id" doc:"Pairing ID."`
}

func (s *Server) startPairing(ctx context.Context, in *struct{ Body PairingBody }) (*struct{ Body auth.Pairing }, error) {
	if err := s.pairingGate(ctx); err != nil {
		return nil, err
	}
	p, err := s.pairings.Start(ctx, auth.PairingStart{Name: in.Body.Name, Browser: in.Body.Browser, ClientID: in.Body.ClientID, Version: in.Body.Version})
	if err != nil {
		return nil, s.fail(ctx, "start-pairing", err, locBody)
	}
	s.log.InfoContext(ctx, "a pairing was requested", "id", p.ID, "browser", p.Browser)
	return &struct{ Body auth.Pairing }{Body: p}, nil
}

func (s *Server) listPairings(ctx context.Context, _ *struct{}) (*struct{ Body PairingList }, error) {
	if err := s.pairingGate(ctx); err != nil {
		return nil, err
	}
	list := s.pairings.Pending(ctx)
	if list == nil {
		list = []auth.Pairing{}
	}
	return &struct{ Body PairingList }{Body: PairingList{Pairings: list}}, nil
}

func (s *Server) getPairing(ctx context.Context, in *pairingID) (*struct{ Body PairingState }, error) {
	if err := s.pairingGate(ctx); err != nil {
		return nil, err
	}
	p, token, err := s.pairings.Get(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "get-pairing", err, locBody)
	}
	return &struct{ Body PairingState }{Body: PairingState{Pairing: p, Token: token}}, nil
}

func (s *Server) approvePairing(ctx context.Context, in *pairingID) (*struct{ Body auth.Pairing }, error) {
	if err := s.pairingGate(ctx); err != nil {
		return nil, err
	}
	p, err := s.pairings.Approve(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "approve-pairing", err, locBody)
	}
	s.log.InfoContext(ctx, "a pairing was allowed", "id", p.ID, "token_id", p.TokenID)
	return &struct{ Body auth.Pairing }{Body: p}, nil
}

func (s *Server) denyPairing(ctx context.Context, in *pairingID) (*struct{ Body auth.Pairing }, error) {
	if err := s.pairingGate(ctx); err != nil {
		return nil, err
	}
	p, err := s.pairings.Deny(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "deny-pairing", err, locBody)
	}
	s.log.InfoContext(ctx, "a pairing was denied", "id", p.ID)
	return &struct{ Body auth.Pairing }{Body: p}, nil
}
