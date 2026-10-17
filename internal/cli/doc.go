// Package cli implements the dlm command-line interface with cobra.
//
// The same binary runs the daemon ("dlm serve") and acts as a typed REST
// client for it (every other command). Client commands talk to the daemon
// only through internal/client; serve.go is the composition root that wires
// the daemon (config, logging, token, event bus, API server) and is the only
// non-test file in this package that imports server-side packages.
//
// # REST + CLI parity rule
//
// Every API operation MUST have a CLI command. A command that calls an
// operation declares it in its cobra annotations:
//
//	Annotations: map[string]string{"operationId": "get-health"}
//
// Several operations served by one command are listed comma-separated
// ("list-tasks,get-task"). TestCLIParity builds the API server exactly as
// "dlm serve" does (via buildServer) and fails when an OpenAPI operationID
// has no annotated command, or when an annotation names an operation that
// does not exist. Register new operations in buildServer so they are covered
// automatically.
//
// # Global flags and exit codes
//
// The root command carries the boot-configuration flags of internal/config
// (--home, --config, --address, ...) so client commands find the daemon and
// its token exactly as the daemon does, plus --output/-o (table, json, yaml),
// --server (API base URL) and --token. Execute returns 0 on success, 1 on
// runtime errors and 2 on usage errors; errors are printed to stderr as one
// line: "error: <message>".
package cli
