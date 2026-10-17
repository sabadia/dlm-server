// Package client is the typed Go client for the REST API, shared by the CLI
// and tests.
//
// The client deliberately does not import the server packages: request and
// response types are defined here and mirror the API's JSON shapes. Errors
// returned by the daemon (RFC 9457 problem details) are decoded into
// [*APIError]; a daemon that cannot be reached yields an error matching
// [ErrDaemonUnreachable].
package client
