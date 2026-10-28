package domain

import (
	"errors"
	"fmt"
	"strings"
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
//	ErrPreconditionFailed → 412 Precondition Failed (an If-Match check failed)
var (
	ErrNotFound        = errors.New("domain: not found")
	ErrAlreadyExists   = errors.New("domain: already exists")
	ErrInvalidArgument = errors.New("domain: invalid argument")
	ErrConflict        = errors.New("domain: conflict")
	// ErrPreconditionFailed reports that the stored entity no longer matches
	// the version the caller read (a compare-and-set failed).
	ErrPreconditionFailed = errors.New("domain: precondition failed")
	// ErrInvalidTransition reports a disallowed task status change. It wraps
	// ErrConflict, so errors.Is(err, ErrConflict) is also true.
	ErrInvalidTransition = fmt.Errorf("domain: invalid status transition: %w", ErrConflict)
)

// domainError carries a human-readable message, the sentinel it belongs to
// and an optional cause (for example a *settings.ValidationError). code and
// params are the machine-readable description clients localise (see Describe).
type domainError struct {
	kind   error
	msg    string
	cause  error
	code   string
	params map[string]string
}

func (e *domainError) Error() string { return e.msg }

// Unwrap exposes both the sentinel and the cause to errors.Is / errors.As.
func (e *domainError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.kind}
	}
	return []error{e.kind, e.cause}
}

func newError(kind, cause error, format string, args ...any) *domainError {
	return &domainError{kind: kind, msg: "domain: " + fmt.Sprintf(format, args...), cause: cause}
}

// Describe returns the stable machine-readable code and the parameters of a
// domain error, for clients that localise messages. Codes look like
// "task.not_found" (an entity and a condition) or are generic
// ("invalid_argument", "conflict"). ok is false when err carries no domain
// error.
func Describe(err error) (code string, params map[string]string, ok bool) {
	var de *domainError
	if !errors.As(err, &de) {
		return "", nil, false
	}
	code = de.code
	if code == "" {
		switch {
		case errors.Is(de.kind, ErrNotFound):
			code = "not_found"
		case errors.Is(de.kind, ErrAlreadyExists):
			code = "already_exists"
		case errors.Is(de.kind, ErrInvalidArgument):
			code = "invalid_argument"
		default:
			code = "conflict"
		}
	}
	return code, de.params, true
}

// entityCode turns an entity description such as "browser profile" or
// "refresh attempt of task" into the identifier used in codes
// ("browser_profile", "refresh_attempt").
func entityCode(entity string) string {
	if i := strings.Index(entity, " of "); i > 0 {
		entity = entity[:i]
	}
	return strings.ReplaceAll(strings.TrimSpace(entity), " ", "_")
}

// NotFound returns an error matching ErrNotFound, e.g. `domain: task "x" not found`.
func NotFound(entity, id string) error {
	e := newError(ErrNotFound, nil, "%s %q not found", entity, id)
	e.code, e.params = entityCode(entity)+".not_found", map[string]string{"entity": entity, "id": id}
	return e
}

// AlreadyExists returns an error matching ErrAlreadyExists.
func AlreadyExists(entity, id string) error {
	e := newError(ErrAlreadyExists, nil, "%s %q already exists", entity, id)
	e.code, e.params = entityCode(entity)+".already_exists", map[string]string{"entity": entity, "id": id}
	if base, ok := strings.CutSuffix(entity, " name"); ok { // "queue name": a unique name is taken
		e.code = entityCode(base) + ".name_taken"
	}
	return e
}

// InvalidArgument returns an error matching ErrInvalidArgument. The format
// may use %w; wrapped errors stay reachable through errors.Is / errors.As.
func InvalidArgument(format string, args ...any) error { return invalidArgument(format, args...) }

func invalidArgument(format string, args ...any) *domainError {
	cause := fmt.Errorf(format, args...)
	return &domainError{kind: ErrInvalidArgument, msg: "domain: " + cause.Error(), cause: cause}
}

// Conflict returns an error matching ErrConflict: the operation is not
// allowed in the entity's current state. The format may use %w.
func Conflict(format string, args ...any) error { return conflict(format, args...) }

func conflict(format string, args ...any) *domainError {
	cause := fmt.Errorf(format, args...)
	return &domainError{kind: ErrConflict, msg: "domain: " + cause.Error(), cause: cause}
}

// ConflictCode is Conflict with a machine-readable code and parameters (see
// Describe): clients show a localised message for the code and use the
// parameters in it. The formatted message stays the English fallback.
func ConflictCode(code string, params map[string]string, format string, args ...any) error {
	e := conflict(format, args...)
	e.code, e.params = code, params
	return e
}

// InvalidArgumentCode is InvalidArgument with a machine-readable code and
// parameters (see Describe).
func InvalidArgumentCode(code string, params map[string]string, format string, args ...any) error {
	e := invalidArgument(format, args...)
	e.code, e.params = code, params
	return e
}

// PreconditionFailed returns an error matching ErrPreconditionFailed with a
// machine-readable code (see Describe).
func PreconditionFailed(code, format string, args ...any) error {
	return &domainError{kind: ErrPreconditionFailed, msg: "domain: " + fmt.Sprintf(format, args...), code: code}
}

// invalidTransition returns an error matching ErrInvalidTransition and ErrConflict.
func invalidTransition(taskID string, from, to TaskStatus) error {
	e := newError(ErrInvalidTransition, nil, "task %q: cannot change status from %q to %q", taskID, from, to)
	e.code = "task.invalid_transition"
	e.params = map[string]string{"taskId": taskID, "from": string(from), "to": string(to)}
	return e
}

// invalidSettings wraps a settings validation error so that it matches
// ErrInvalidArgument and still unwraps to *settings.ValidationError.
func invalidSettings(entity, id string, err error) error {
	return newError(ErrInvalidArgument, err, "%s %q: %v", entity, id, err)
}
