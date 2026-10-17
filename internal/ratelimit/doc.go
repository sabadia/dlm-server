// Package ratelimit provides hierarchical (global, queue, task) bandwidth
// limiting.
//
// Every scope owns a [Bucket], a token bucket measured in bytes per second
// whose limit can be changed while transfers are running (0 = unlimited).
// A task's bytes must pass its task, queue and global buckets in that order:
// [Chain] combines buckets into an engine.Limiter and [Manager] owns the
// buckets of a running daemon and hands out the chain for a task.
//
// Engines apply the limiter per chunk of data actually transferred, usually
// by wrapping a response body with [Reader].
package ratelimit
