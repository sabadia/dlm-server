// Package client is the typed Go client for the REST API, shared by the CLI
// and tests.
//
// The client deliberately does not import the server packages (api,
// service, domain, settings): request and response types are defined here
// and mirror the API's JSON shapes, so the client depends only on the wire
// contract. Settings layers are kept as raw JSON objects
// ([encoding/json.RawMessage]) because their shape is owned by the daemon;
// the CLI builds them and RFC 7396 merge patches from dotted paths. Errors
// returned by the daemon (RFC 9457 problem details) are decoded into
// [*APIError]; a daemon that cannot be reached yields an error matching
// [ErrDaemonUnreachable].
package client
