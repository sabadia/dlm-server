package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
)

// Kind classifies a download failure. It decides what the task runner does
// next: retry with backoff, try a link refresh, or give up.
type Kind int

// Error kinds.
const (
	// KindFatal errors cannot be fixed by retrying (bad request, disk
	// error, unsupported resource). It is the zero value so unclassified
	// errors are never retried by accident.
	KindFatal Kind = iota
	// KindTransient errors may succeed on retry (network resets, timeouts,
	// 5xx, 429).
	KindTransient
	// KindLinkExpired means the URL no longer serves the file (401/403/404/
	// 410, HTML instead of the file, fingerprint mismatch); a link refresh
	// may fix it.
	KindLinkExpired
	// KindCanceled means the context was cancelled (pause or shutdown).
	KindCanceled
)

var kindNames = [...]string{"fatal", "transient", "link_expired", "canceled"}

// String returns the lower-case kind name.
func (k Kind) String() string {
	if k < 0 || int(k) >= len(kindNames) {
		return fmt.Sprintf("kind(%d)", int(k))
	}
	return kindNames[k]
}

// Error is a classified engine error.
type Error struct {
	Kind Kind
	// Code is a domain.ErrorCode* value recorded on the task.
	Code string
	// StatusCode is the HTTP (or protocol) status, when relevant.
	StatusCode int
	// RetryAfter is a server-requested delay (e.g. Retry-After), or 0.
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	msg := "engine: " + e.Kind.String()
	if e.StatusCode != 0 {
		msg += fmt.Sprintf(" (status %d)", e.StatusCode)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Transient wraps err as a retryable error.
func Transient(code string, err error) *Error {
	return &Error{Kind: KindTransient, Code: code, Err: err}
}

// Fatal wraps err as a non-retryable error.
func Fatal(code string, err error) *Error {
	return &Error{Kind: KindFatal, Code: code, Err: err}
}

// LinkExpired wraps err as an expired-link error.
func LinkExpired(err error) *Error {
	return &Error{Kind: KindLinkExpired, Code: domain.ErrorCodeLinkExpired, Err: err}
}

// Classify returns the kind of err: the Kind of the outermost *Error in the
// chain, KindCanceled for context cancellation, otherwise KindFatal.
func Classify(err error) Kind {
	if err == nil {
		return KindFatal
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	if errors.Is(err, context.Canceled) {
		return KindCanceled
	}
	return KindFatal
}

// TaskError converts err into the domain.TaskError recorded on a task.
func TaskError(err error, now time.Time) domain.TaskError {
	te := domain.TaskError{Code: domain.ErrorCodeFatal, Message: err.Error(), At: now.UTC()}
	var e *Error
	if errors.As(err, &e) {
		if e.Code != "" {
			te.Code = e.Code
		}
		te.Retryable = e.Kind == KindTransient || e.Kind == KindLinkExpired
	}
	return te
}
