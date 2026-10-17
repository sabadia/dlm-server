// Package domain defines the core entities of the download manager (Task,
// Queue, Source, Resource, Segment, Category), the task status state machine
// and the typed domain errors.
//
// The package is pure: no I/O, no logging and no mutable package state.
// Constructors take the entity ID and the current time as parameters so they
// stay deterministic and testable; services generate IDs with NewID and pass
// a clock-derived time.
//
// Invariants are enforced by methods: a task's status changes only through
// Task.TransitionTo (driven by an immutable transition table), the built-in
// Main queue cannot be deleted, and per-level settings overrides are checked
// with settings.Settings.Validate for the entity's level.
//
// Every returned error wraps one of the sentinels ErrNotFound,
// ErrAlreadyExists, ErrInvalidArgument, ErrConflict or ErrInvalidTransition.
package domain
