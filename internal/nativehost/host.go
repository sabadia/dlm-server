package nativehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// Error codes of failure responses.
const (
	CodeInvalidRequest   = "invalid_request"
	CodeUnknownType      = "unknown_type"
	CodeOriginNotAllowed = "origin_not_allowed"
	// CodePairingFailed: the daemon refused or could not take the pairing
	// request; the message says why.
	CodePairingFailed = "pairing_failed"
	// CodeDaemonUnreachable: the daemon does not answer.
	CodeDaemonUnreachable = "daemon_unreachable"
)

// operationTimeout bounds one call to the daemon on behalf of the extension.
const operationTimeout = 10 * time.Second

// DaemonInfo says where the daemon listens and what it is.
type DaemonInfo struct {
	// BaseURL is the daemon's API base URL.
	BaseURL string
	// Running is false when no daemon answers; BaseURL is then the address it
	// would have.
	Running    bool
	Version    string
	APIVersion int
	// TLSFingerprint is "sha256:<hex>" of the certificate when the daemon
	// serves HTTPS.
	TLSFingerprint string
}

// PairingRequest is what the extension says about itself.
type PairingRequest struct {
	Name    string
	Browser string
	// ClientID is the extension's id as the browser reports it (never as the
	// extension claims it).
	ClientID string
	Version  string
}

// PairingInfo is a pairing as the daemon reports it.
type PairingInfo struct {
	ID        string
	Status    string // pending, approved, denied or expired
	Code      string
	ExpiresAt time.Time
	// Token is the issued token of an approved pairing, present once.
	Token string
}

// PairingAPI is the daemon's pairing interface, called with the primary token
// so the extension never needs one.
type PairingAPI interface {
	Start(ctx context.Context, req PairingRequest) (PairingInfo, error)
	Status(ctx context.Context, id string) (PairingInfo, error)
}

// Host answers the messages of a browser extension: where the daemon is
// (connect) and a way to get a token of its own (pair, pair.status). It never
// hands out the primary token.
type Host struct {
	// Daemon reports where the daemon listens now. It is called for every
	// connect, so a daemon that restarted on another port is found.
	Daemon func(ctx context.Context) DaemonInfo
	// Pairing is the daemon's pairing interface; nil refuses pair requests.
	Pairing PairingAPI
	// Origin is the caller origin the browser passed as the first argument
	// ("chrome-extension://<id>/"); empty when none was passed (Firefox).
	Origin string
	// AddonID is the add-on id Firefox passes as an argument; empty for Chromium browsers.
	AddonID string
	// AllowedOrigins are extra extension origins the daemon accepts
	// (server.allowed_origins). The published extension's origin is always
	// accepted. Another chrome-extension:// origin outside the list is
	// refused.
	AllowedOrigins []string
	Logger         *slog.Logger
}

type request struct {
	Type string          `json:"type"`
	ID   json.RawMessage `json:"id,omitempty"`
	// Pairing fields.
	Name      string `json:"name,omitempty"`
	Browser   string `json:"browser,omitempty"`
	Version   string `json:"version,omitempty"`
	PairingID string `json:"pairingId,omitempty"`
}

// response is every reply. Fields that do not apply are omitted.
type response struct {
	Type           string          `json:"type,omitempty"`
	ID             json.RawMessage `json:"id,omitempty"`
	OK             bool            `json:"ok"`
	BaseURL        string          `json:"baseUrl,omitempty"`
	Running        *bool           `json:"running,omitempty"`
	Version        string          `json:"version,omitempty"`
	APIVersion     int             `json:"apiVersion,omitempty"`
	TLSFingerprint string          `json:"tlsFingerprint,omitempty"`
	PairingID      string          `json:"pairingId,omitempty"`
	Status         string          `json:"status,omitempty"`
	Code           string          `json:"code,omitempty"`
	ExpiresAt      *time.Time      `json:"expiresAt,omitempty"`
	Token          string          `json:"token,omitempty"`
	Error          string          `json:"error,omitempty"`
	Message        string          `json:"message,omitempty"`
}

// Serve reads requests from r and writes replies to w until r ends (the
// browser closed the port) or ctx is cancelled between messages. Nothing but
// protocol frames may be written to w. A message the host cannot frame (too
// large, truncated) ends the session after an error reply, because the byte
// stream cannot be resynchronised.
func (h *Host) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	log := h.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	for ctx.Err() == nil {
		msg, err := ReadMessage(r, MaxRequestSize)
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			_ = h.write(w, response{Error: CodeInvalidRequest, Message: err.Error()}) // best effort: the session ends anyway
			return err
		}
		resp := h.handle(ctx, msg)
		if err := h.write(w, resp); err != nil {
			return err
		}
		if !resp.OK {
			log.Debug("nativehost: request refused", "error", resp.Error)
		}
	}
	return nil
}

func (h *Host) write(w io.Writer, resp response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("nativehost: encode response: %w", err)
	}
	return WriteMessage(w, data)
}

func (h *Host) handle(ctx context.Context, msg []byte) response {
	var req request
	if err := json.Unmarshal(msg, &req); err != nil || req.Type == "" {
		return response{Error: CodeInvalidRequest, Message: `want a JSON object with a "type"`}
	}
	var resp response
	switch req.Type {
	case "ping":
		resp = response{OK: true}
	case "connect":
		resp = h.connect(ctx)
	case "pair":
		resp = h.pair(ctx, req)
	case "pair.status":
		resp = h.pairStatus(ctx, req)
	default:
		resp = response{Error: CodeUnknownType, Message: fmt.Sprintf("unknown message type %q", req.Type)}
	}
	resp.Type, resp.ID = req.Type, req.ID
	return resp
}

func (h *Host) refused() (response, bool) {
	if h.originAllowed() {
		return response{}, false
	}
	return response{Error: CodeOriginNotAllowed, Message: fmt.Sprintf(
		"%s is not DLM's browser extension and is not in server.allowed_origins", h.Origin)}, true
}

func (h *Host) connect(ctx context.Context) response {
	if r, refused := h.refused(); refused {
		return r
	}
	var info DaemonInfo
	if h.Daemon != nil {
		dctx, cancel := context.WithTimeout(ctx, operationTimeout)
		defer cancel()
		info = h.Daemon(dctx)
	}
	running := info.Running
	return response{OK: true, BaseURL: info.BaseURL, Running: &running, Version: info.Version, APIVersion: info.APIVersion, TLSFingerprint: info.TLSFingerprint}
}

// callerID is who the browser says is calling: the extension id of a Chromium
// origin or the add-on id Firefox passed. The extension cannot choose it.
func (h *Host) callerID() string {
	if id, ok := strings.CutPrefix(h.Origin, "chrome-extension://"); ok {
		return strings.TrimRight(id, "/")
	}
	return h.AddonID
}

func (h *Host) pair(ctx context.Context, req request) response {
	if r, refused := h.refused(); refused {
		return r
	}
	if h.Pairing == nil {
		return response{Error: CodePairingFailed, Message: "pairing is not available"}
	}
	pctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	p, err := h.Pairing.Start(pctx, PairingRequest{Name: req.Name, Browser: req.Browser, ClientID: h.callerID(), Version: req.Version})
	if err != nil {
		return pairingFailure(err)
	}
	return pairingResponse(p)
}

func (h *Host) pairStatus(ctx context.Context, req request) response {
	if r, refused := h.refused(); refused {
		return r
	}
	if h.Pairing == nil {
		return response{Error: CodePairingFailed, Message: "pairing is not available"}
	}
	if req.PairingID == "" {
		return response{Error: CodeInvalidRequest, Message: `pair.status needs a "pairingId"`}
	}
	pctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	p, err := h.Pairing.Status(pctx, req.PairingID)
	if err != nil {
		return pairingFailure(err)
	}
	return pairingResponse(p)
}

func pairingResponse(p PairingInfo) response {
	exp := p.ExpiresAt
	return response{OK: true, PairingID: p.ID, Status: p.Status, Code: p.Code, ExpiresAt: &exp, Token: p.Token}
}

func pairingFailure(err error) response {
	return response{Error: CodePairingFailed, Message: err.Error()}
}

// originAllowed applies the origin check to chrome-extension:// callers: the
// published extension always passes, any other must be in AllowedOrigins.
// Callers that pass no such origin (Firefox) are already restricted by the
// manifest's allowed_extensions.
func (h *Host) originAllowed() bool {
	if !strings.HasPrefix(h.Origin, "chrome-extension://") {
		return true
	}
	want := strings.TrimRight(h.Origin, "/")
	if want == strings.TrimRight(BuiltinChromiumOrigin, "/") {
		return true
	}
	for _, o := range h.AllowedOrigins {
		if strings.TrimRight(o, "/") == want {
			return true
		}
	}
	return false
}
