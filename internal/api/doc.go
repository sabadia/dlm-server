// Package api exposes the REST API (OpenAPI 3.1) and the Server-Sent Events stream.
//
// The API is built with huma v2 on top of the standard library http.ServeMux.
// The OpenAPI document is served at /openapi.json (and /openapi.yaml), the
// interactive documentation at /docs. Every error, including those produced
// by middleware (401, 403, 413, 500), is an RFC 9457 problem-details document
// with Content-Type application/problem+json.
//
// # Registering operations
//
// Operations MUST be added with [Register] (token required) or
// [RegisterPublic] (no token), never with huma.Register directly. The helpers
// enforce the following conventions and panic at startup if one is violated:
//
//   - Every operation has an OperationID in kebab-case, e.g. "list-tasks",
//     "get-health". The CLI parity test maps OpenAPI operationIDs to CLI
//     commands (cobra Annotations["operationId"]), so an operation without an
//     ID, or with a duplicated one, breaks parity.
//   - Every operation has at least one tag (e.g. "tasks", "queues", "system").
//   - Every path lives under /v1/.
//
// # Authentication
//
// Requests carry "Authorization: Bearer <token>". Every route is protected by
// default (deny by default, including unknown /v1 paths); only routes added
// with [RegisterPublic], plus /openapi.*, /schemas/* and /docs, are reachable
// without the token. Public operations can call [Authenticated] to learn
// whether a valid token was presented anyway.
package api
