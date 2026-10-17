package client

import (
	"errors"
	"net/http"
)

// APIError is an RFC 9457 problem-details error returned by the daemon.
type APIError struct {
	Status int           `json:"status"`
	Title  string        `json:"title"`
	Detail string        `json:"detail"`
	Errors []ErrorDetail `json:"errors,omitempty"`
}

// ErrorDetail describes one validation problem inside an APIError.
type ErrorDetail struct {
	Message  string `json:"message,omitempty"`
	Location string `json:"location,omitempty"`
	Value    any    `json:"value,omitempty"`
}

// Error returns "<title>: <detail>" (or just the title), suitable for showing
// to users as-is.
func (e *APIError) Error() string {
	if e.Detail == "" || e.Detail == e.Title {
		return e.Title
	}
	return e.Title + ": " + e.Detail
}

// IsNotFound reports whether err is an APIError with status 404.
func IsNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

// IsUnauthorized reports whether err is an APIError with status 401.
func IsUnauthorized(err error) bool { return hasStatus(err, http.StatusUnauthorized) }

func hasStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}
