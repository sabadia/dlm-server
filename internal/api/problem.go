package api

import (
	"net/http"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
)

// Problem is the body of every error response: an RFC 9457 problem with two
// extension members clients use to show their own, localised message:
//
//   - code is a stable machine-readable identifier such as "task.not_found",
//     "task.duplicate" or "validation_failed"; codes are never reused for
//     another meaning;
//   - params are the values to put in that message (ids, names, statuses).
//
// title and detail stay English fallbacks for clients that do not know a code.
type Problem struct {
	huma.ErrorModel

	// Code identifies the kind of problem; see the codes in docs/API.md.
	Code string `json:"code" example:"task.not_found" doc:"Stable machine-readable error code, for example task.not_found, task.duplicate or validation_failed. Clients map it to a localised message; unknown codes fall back to title and detail."`
	// Params carries the values a localised message needs.
	Params map[string]string `json:"params,omitempty" doc:"Values for the localised message of the code (ids, names, statuses)."`
}

// problemTypePrefix prefixes the problem code to form the RFC 9457 type URI.
const problemTypePrefix = "urn:dlm:problem:"

// withCode sets the code, the matching type URI and the parameters.
func (p *Problem) withCode(code string, params map[string]string) *Problem {
	p.Code, p.Params = code, params
	p.Type = problemTypePrefix + code
	return p
}

// codeForStatus is the generic code of a problem that has no more specific
// one (for example a framework error or a middleware rejection).
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusNotAcceptable:
		return "not_acceptable"
	case http.StatusConflict:
		return "conflict"
	case http.StatusPreconditionFailed:
		return "precondition_failed"
	case http.StatusRequestEntityTooLarge:
		return "payload_too_large"
	case http.StatusUnsupportedMediaType:
		return "unsupported_media_type"
	case http.StatusUnprocessableEntity:
		return "validation_failed"
	case http.StatusTooManyRequests:
		return "too_many_requests"
	case http.StatusInternalServerError:
		return "internal"
	case http.StatusBadGateway:
		return "bad_gateway"
	case http.StatusServiceUnavailable:
		return "unavailable"
	default:
		return strings.ReplaceAll(strings.ToLower(http.StatusText(status)), " ", "_")
	}
}

var installProblem sync.Once

// useProblemModel makes the framework's own errors (request validation,
// malformed bodies, unsupported media types, ...) and the middleware's
// rejections carry a code too. huma builds its error responses through the
// package-level hook huma.NewError, so it is replaced once, before any
// operation is registered; replacing it with the same function again is not
// possible, which keeps concurrent servers (tests) race-free.
func useProblemModel() {
	installProblem.Do(func() {
		base := huma.NewError
		huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
			m, ok := base(status, msg, errs...).(*huma.ErrorModel)
			if !ok {
				return base(status, msg, errs...)
			}
			p := &Problem{ErrorModel: *m}
			return p.withCode(codeForStatus(status), nil)
		}
	})
}
