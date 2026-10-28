// Package instance lets a client find the daemon of a configuration
// directory and keeps two daemons from sharing one. A running daemon holds an
// exclusive lock on dlm.lock and publishes runtime.json (its pid, the address
// it really listens on, its version and the fingerprint of its TLS
// certificate), so a supervisor that started it with --address 127.0.0.1:0
// can learn the port, and the CLI can reach it without flags.
package instance
