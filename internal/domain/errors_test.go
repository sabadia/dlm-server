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

func TestDescribeGivesStableCodes(t *testing.T) {
	for _, c := range []struct {
		name   string
		err    error
		code   string
		params map[string]string
	}{
		{"not found", NotFound("task", "t1"), "task.not_found", map[string]string{"entity": "task", "id": "t1"}},
		{"entity with spaces", NotFound("browser profile", "g"), "browser_profile.not_found", nil},
		{"entity of something", NotFound("refresh attempt of task", "t"), "refresh_attempt.not_found", nil},
		{"exists", AlreadyExists("plugin", "p"), "plugin.already_exists", map[string]string{"id": "p"}},
		{"unique name", AlreadyExists("queue name", "Main"), "queue.name_taken", nil},
		{"conflict code", ConflictCode("task.duplicate", map[string]string{"taskId": "x"}, "dup %s", "x"), "task.duplicate", map[string]string{"taskId": "x"}},
		{"invalid code", InvalidArgumentCode("category.bad_dir", nil, "bad"), "category.bad_dir", nil},
		{"generic conflict", Conflict("nope"), "conflict", nil},
		{"generic invalid", InvalidArgument("bad %d", 1), "invalid_argument", nil},
		{"wrapped", fmt.Errorf("service: %w", NotFound("queue", "q")), "queue.not_found", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, params, ok := Describe(c.err)
			if !ok || code != c.code {
				t.Fatalf("Describe = %q, %v; want %q", code, ok, c.code)
			}
			for k, v := range c.params {
				if params[k] != v {
					t.Errorf("params[%s] = %q, want %q", k, params[k], v)
				}
			}
		})
	}
	if _, _, ok := Describe(errors.New("plain")); ok {
		t.Error("a plain error was described")
	}
	// The sentinels still match.
	if !errors.Is(ConflictCode("x", nil, "c"), ErrConflict) || !errors.Is(NotFound("a", "b"), ErrNotFound) {
		t.Error("codes must not change which sentinel an error matches")
	}
	if !errors.Is(invalidTransition("t", StatusQueued, StatusCompleted), ErrConflict) {
		t.Error("an invalid transition is a conflict")
	}
	if code, p, _ := Describe(invalidTransition("t", StatusQueued, StatusCompleted)); code != "task.invalid_transition" || p["from"] != "queued" || p["to"] != "completed" {
		t.Errorf("transition = %q %v", code, p)
	}
}
