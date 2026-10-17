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
	CodeTokenUnavailable = "token_unavailable" //nolint:gosec // an error code, not a credential
)

// healthTimeout bounds the daemon check of a connect request.
const healthTimeout = 2 * time.Second

// Host answers the messages of a browser extension. It tells the extension
// where the daemon listens and the token to authenticate with, the only
// thing the extension cannot learn on its own.
type Host struct {
	// BaseURL is the daemon's API base URL.
	BaseURL string
	// Token returns the API token (the daemon's token file).
	Token func() (string, error)
	// Health checks that the daemon answers and returns its version. A nil
	// Health reports running=false.
	Health func(ctx context.Context) (version string, err error)
	// Origin is the caller origin the browser passed as the first argument
	// ("chrome-extension://<id>/"); empty when none was passed (Firefox).
	Origin string
	// AllowedOrigins are the extension origins the daemon accepts
	// (server.allowed_origins). A chrome-extension:// Origin outside the list
	// is refused: the daemon would reject that extension anyway.
	AllowedOrigins []string
	Logger         *slog.Logger
}

type request struct {
	Type string          `json:"type"`
	ID   json.RawMessage `json:"id,omitempty"`
}

// response is every reply. Fields that do not apply are omitted.
type response struct {
	Type    string          `json:"type,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	OK      bool            `json:"ok"`
	BaseURL string          `json:"baseUrl,omitempty"`
	Token   string          `json:"token,omitempty"`
	Running *bool           `json:"running,omitempty"`
	Version string          `json:"version,omitempty"`
	Error   string          `json:"error,omitempty"`
	Message string          `json:"message,omitempty"`
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
	switch req.Type {
	case "ping":
		return response{Type: req.Type, ID: req.ID, OK: true}
	case "connect":
		resp := h.connect(ctx)
		resp.Type, resp.ID = req.Type, req.ID
		return resp
	default:
		return response{Type: req.Type, ID: req.ID, Error: CodeUnknownType, Message: fmt.Sprintf("unknown message type %q", req.Type)}
	}
}

func (h *Host) connect(ctx context.Context) response {
	if !h.originAllowed() {
		return response{Error: CodeOriginNotAllowed, Message: fmt.Sprintf(
			"%s is not in server.allowed_origins; add it to the config file (browsers start the host without your environment variables)", h.Origin)}
	}
	token, err := h.Token()
	if err != nil || token == "" {
		return response{Error: CodeTokenUnavailable, Message: "the daemon has no token yet: run `dlm serve` once"}
	}
	resp := response{OK: true, BaseURL: h.BaseURL, Token: token}
	running := false
	if h.Health != nil {
		hctx, cancel := context.WithTimeout(ctx, healthTimeout)
		defer cancel()
		if v, err := h.Health(hctx); err == nil {
			running, resp.Version = true, v
		}
	}
	resp.Running = &running
	return resp
}

// originAllowed applies the allowed-origins check to chrome-extension://
// callers. Callers that pass no such origin (Firefox) are already restricted
// by the manifest's allowed_extensions.
func (h *Host) originAllowed() bool {
	if !strings.HasPrefix(h.Origin, "chrome-extension://") {
		return true
	}
	want := strings.TrimRight(h.Origin, "/")
	for _, o := range h.AllowedOrigins {
		if strings.TrimRight(o, "/") == want {
			return true
		}
	}
	return false
}
