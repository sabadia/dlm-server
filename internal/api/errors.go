package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/settings"
)

// Locations of settings field errors in problem details, relative to the
// request (see huma.ErrorDetail.Location).
const (
	locBody         = "body"
	locBodySettings = "body.settings"
)

// errServicesUnavailable is returned by service-backed operations when the
// server was built without services (e.g. for the OpenAPI document only).
func errServicesUnavailable() error {
	return newProblem(http.StatusServiceUnavailable, "the daemon services are not available")
}

// newProblem returns an RFC 9457 problem with the given status and detail.
func newProblem(status int, detail string, details ...*huma.ErrorDetail) *huma.ErrorModel {
	return &huma.ErrorModel{
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
		Errors: details,
	}
}

// problemFor maps a service error to an RFC 9457 problem (the central error
// mapping of the API):
//
//   - a failed origin probe (*engine.Error) → 502 Bad Gateway, with the
//     engine error code in the detail;
//   - a cancelled or timed-out request → 503 Service Unavailable;
//   - service.ErrBrowserUnavailable (no headless browser) → 503 Service Unavailable;
//   - domain.ErrNotFound → 404 Not Found;
//   - domain.ErrInvalidArgument → 422 Unprocessable Content; settings
//     validation failures list one error per invalid field, located at
//     settingsLoc + "." + the field's JSON path;
//   - domain.ErrAlreadyExists and domain.ErrConflict (including invalid
//     status transitions) → 409 Conflict;
//   - anything else → 500 Internal Server Error without details (logged).
func (s *Server) problemFor(ctx context.Context, op string, err error, settingsLoc string) *huma.ErrorModel {
	var engErr *engine.Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &engErr) && engErr.Kind == engine.KindCanceled:
		return newProblem(http.StatusServiceUnavailable, "the request was cancelled before it completed")
	case errors.As(err, &engErr):
		code := engErr.Code
		if code == "" {
			code = engErr.Kind.String()
		}
		return newProblem(http.StatusBadGateway, fmt.Sprintf("origin request failed [%s]: %v", code, err))
	case errors.Is(err, service.ErrBrowserUnavailable):
		return newProblem(http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, domain.ErrNotFound):
		return newProblem(http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrInvalidArgument):
		return newProblem(http.StatusUnprocessableEntity, err.Error(), settingsDetails(err, settingsLoc)...)
	case errors.Is(err, domain.ErrAlreadyExists), errors.Is(err, domain.ErrConflict):
		return newProblem(http.StatusConflict, err.Error())
	default:
		s.log.ErrorContext(ctx, "api: operation failed", "operation", op, "error", err)
		return newProblem(http.StatusInternalServerError, "internal server error")
	}
}

// fail is problemFor returned as an error, for handlers.
func (s *Server) fail(ctx context.Context, op string, err error, settingsLoc string) error {
	return s.problemFor(ctx, op, err, settingsLoc)
}

// settingsDetails lists the field errors of a settings validation failure.
func settingsDetails(err error, loc string) []*huma.ErrorDetail {
	var verr *settings.ValidationError
	if !errors.As(err, &verr) {
		return nil
	}
	out := make([]*huma.ErrorDetail, len(verr.Fields))
	for i, f := range verr.Fields {
		out[i] = &huma.ErrorDetail{Message: f.Message, Location: loc + "." + f.Path}
	}
	return out
}
