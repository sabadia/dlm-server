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
	return newProblem(http.StatusServiceUnavailable, "the daemon services are not available").withCode("service.unavailable", nil)
}

// newProblem returns an RFC 9457 problem with the given status and detail and
// the generic code of the status; use withCode for a more specific one.
func newProblem(status int, detail string, details ...*huma.ErrorDetail) *Problem {
	p := &Problem{ErrorModel: huma.ErrorModel{
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
		Errors: details,
	}}
	return p.withCode(codeForStatus(status), nil)
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
//   - domain.ErrPreconditionFailed (a failed If-Match) → 412 Precondition Failed;
//   - anything else → 500 Internal Server Error without details (logged).
func (s *Server) problemFor(ctx context.Context, op string, err error, settingsLoc string) *Problem {
	var engErr *engine.Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &engErr) && engErr.Kind == engine.KindCanceled:
		return newProblem(http.StatusServiceUnavailable, "the request was cancelled before it completed").withCode("request.cancelled", nil)
	case errors.As(err, &engErr):
		code := engErr.Code
		if code == "" {
			code = engErr.Kind.String()
		}
		return newProblem(http.StatusBadGateway, fmt.Sprintf("origin request failed [%s]: %v", code, err)).
			withCode("origin.failed", map[string]string{"kind": engErr.Kind.String(), "code": code})
	case errors.Is(err, service.ErrBrowserUnavailable):
		return newProblem(http.StatusServiceUnavailable, err.Error()).withCode("browser.unavailable", nil)
	case errors.Is(err, domain.ErrNotFound):
		return domainProblem(http.StatusNotFound, err, nil)
	case errors.Is(err, domain.ErrInvalidArgument):
		details := settingsDetails(err, settingsLoc)
		p := domainProblem(http.StatusUnprocessableEntity, err, details)
		if len(details) > 0 && p.Code == "invalid_argument" {
			_ = p.withCode("settings.invalid", nil)
		}
		return p
	case errors.Is(err, domain.ErrPreconditionFailed):
		return domainProblem(http.StatusPreconditionFailed, err, nil)
	case errors.Is(err, domain.ErrAlreadyExists), errors.Is(err, domain.ErrConflict):
		return domainProblem(http.StatusConflict, err, nil)
	default:
		s.log.ErrorContext(ctx, "api: operation failed", "operation", op, "error", err)
		return newProblem(http.StatusInternalServerError, "internal server error")
	}
}

// domainProblem builds the problem of a domain error with its own code and
// parameters (see domain.Describe).
func domainProblem(status int, err error, details []*huma.ErrorDetail) *Problem {
	p := newProblem(status, err.Error(), details...)
	if code, params, ok := domain.Describe(err); ok {
		_ = p.withCode(code, params)
	}
	return p
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
