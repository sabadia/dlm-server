package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors that callers branch on with errors.Is. Every error returned
// by this package wraps exactly one of them.
//
// The API layer maps them to HTTP status codes (RFC 9457 problem details):
//
//	ErrNotFound          → 404 Not Found
//	ErrAlreadyExists     → 409 Conflict
//	ErrConflict          → 409 Conflict (operation not allowed in the current state)
//	ErrInvalidTransition → 409 Conflict (it also matches ErrConflict)
//	ErrInvalidArgument   → 422 Unprocessable Content
var (
	ErrNotFound        = errors.New("domain: not found")
	ErrAlreadyExists   = errors.New("domain: already exists")
	ErrInvalidArgument = errors.New("domain: invalid argument")
	ErrConflict        = errors.New("domain: conflict")
	// ErrInvalidTransition reports a disallowed task status change. It wraps
	// ErrConflict, so errors.Is(err, ErrConflict) is also true.
	ErrInvalidTransition = fmt.Errorf("domain: invalid status transition: %w", ErrConflict)
)

// domainError carries a human-readable message, the sentinel it belongs to
// and an optional cause (for example a *settings.ValidationError).
type domainError struct {
	kind  error
	msg   string
	cause error
}

func (e *domainError) Error() string { return e.msg }

// Unwrap exposes both the sentinel and the cause to errors.Is / errors.As.
func (e *domainError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.kind}
	}
	return []error{e.kind, e.cause}
}

func newError(kind, cause error, format string, args ...any) error {
	return &domainError{kind: kind, msg: "domain: " + fmt.Sprintf(format, args...), cause: cause}
}

// NotFound returns an error matching ErrNotFound, e.g. `domain: task "x" not found`.
func NotFound(entity, id string) error {
	return newError(ErrNotFound, nil, "%s %q not found", entity, id)
}

// AlreadyExists returns an error matching ErrAlreadyExists.
func AlreadyExists(entity, id string) error {
	return newError(ErrAlreadyExists, nil, "%s %q already exists", entity, id)
}

// InvalidArgument returns an error matching ErrInvalidArgument. The format
// may use %w; wrapped errors stay reachable through errors.Is / errors.As.
func InvalidArgument(format string, args ...any) error {
	cause := fmt.Errorf(format, args...)
	return &domainError{kind: ErrInvalidArgument, msg: "domain: " + cause.Error(), cause: cause}
}

// Conflict returns an error matching ErrConflict: the operation is not
// allowed in the entity's current state. The format may use %w.
func Conflict(format string, args ...any) error {
	cause := fmt.Errorf(format, args...)
	return &domainError{kind: ErrConflict, msg: "domain: " + cause.Error(), cause: cause}
}

// invalidTransition returns an error matching ErrInvalidTransition and ErrConflict.
func invalidTransition(taskID string, from, to TaskStatus) error {
	return newError(ErrInvalidTransition, nil, "task %q: cannot change status from %q to %q", taskID, from, to)
}

// invalidSettings wraps a settings validation error so that it matches
// ErrInvalidArgument and still unwraps to *settings.ValidationError.
func invalidSettings(entity, id string, err error) error {
	return newError(ErrInvalidArgument, err, "%s %q: %v", entity, id, err)
}
