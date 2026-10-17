package httpx

import (
	"fmt"
	"net/http"
)

// StatusError reports an unexpected HTTP response status. It carries the
// response headers (e.g. Retry-After, Content-Type) so the classifier can
// decide between retry, link refresh and failure. The body is not retained.
type StatusError struct {
	StatusCode int
	Status     string
	Header     http.Header
	// URL is the request URL after redirects.
	URL string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("httpx: unexpected status %q from %s", e.Status, e.URL)
}

// NewStatusError builds a StatusError from a response. It does not read or
// close the body.
func NewStatusError(resp *http.Response) *StatusError {
	e := &StatusError{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone()}
	if resp.Request != nil && resp.Request.URL != nil {
		e.URL = resp.Request.URL.Redacted()
	}
	return e
}
