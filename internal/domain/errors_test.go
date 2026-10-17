package domain

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
)

func TestErrorsIs(t *testing.T) {
	sentinels := []error{ErrNotFound, ErrAlreadyExists, ErrInvalidArgument, ErrConflict, ErrInvalidTransition}
	tests := []struct {
		name string
		err  error
		want []error
		msg  string
	}{
		{"not found", NotFound("task", "x"), []error{ErrNotFound}, `domain: task "x" not found`},
		{"already exists", AlreadyExists("queue", "q"), []error{ErrAlreadyExists}, `domain: queue "q" already exists`},
		{"invalid argument", InvalidArgument("bad %s", "thing"), []error{ErrInvalidArgument}, "domain: bad thing"},
		{"conflict", Conflict("busy"), []error{ErrConflict}, "domain: busy"},
		{"invalid transition", invalidTransition("t", StatusPaused, StatusRunning), []error{ErrInvalidTransition, ErrConflict}, `domain: task "t": cannot change status from "paused" to "running"`},
		{"sentinel transition", ErrInvalidTransition, []error{ErrInvalidTransition, ErrConflict}, ""},
		{"wrapped further", fmt.Errorf("service: %w", NotFound("task", "y")), []error{ErrNotFound}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, s := range sentinels {
				want := slices.Contains(tt.want, s)
				if got := errors.Is(tt.err, s); got != want {
					t.Errorf("errors.Is(%v, %v) = %v, want %v", tt.err, s, got, want)
				}
			}
			if tt.msg != "" && tt.err.Error() != tt.msg {
				t.Errorf("message = %q, want %q", tt.err.Error(), tt.msg)
			}
		})
	}
}

func TestInvalidArgumentKeepsWrappedCause(t *testing.T) {
	err := InvalidArgument("read: %w", io.ErrUnexpectedEOF)
	if !errors.Is(err, ErrInvalidArgument) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v", err)
	}
	if err.Error() != "domain: read: unexpected EOF" {
		t.Fatalf("message = %q", err.Error())
	}
}
