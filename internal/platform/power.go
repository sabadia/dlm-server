package platform

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrUnsupported is returned by Sleep and Shutdown on operating systems
// without a known command for them.
var ErrUnsupported = errors.New("platform: not supported on this operating system")

// Command is an executable with its arguments, run without a shell.
type Command struct {
	Name string
	Args []string
}

// String formats the command for logs.
func (c Command) String() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

// SleepCommand returns the command that suspends the machine, or false
// when the operating system has none.
func SleepCommand() (Command, bool) { return sleepCommand() }

// ShutdownCommand returns the command that powers the machine off, or
// false when the operating system has none.
func ShutdownCommand() (Command, bool) { return shutdownCommand() }

// Sleep suspends the machine (see SleepCommand). It returns once the
// command has exited, which on most systems is after the machine wakes up
// again, or ErrUnsupported.
func Sleep(ctx context.Context) error {
	c, ok := SleepCommand()
	if !ok {
		return ErrUnsupported
	}
	return run(ctx, "sleep", c)
}

// Shutdown powers the machine off (see ShutdownCommand), or returns
// ErrUnsupported. The daemon is usually terminated by the OS shortly after.
func Shutdown(ctx context.Context) error {
	c, ok := ShutdownCommand()
	if !ok {
		return ErrUnsupported
	}
	return run(ctx, "shutdown", c)
}

// maxCommandOutput bounds the command output quoted in errors.
const maxCommandOutput = 512

func run(ctx context.Context, what string, c Command) error {
	out, err := exec.CommandContext(ctx, c.Name, c.Args...).CombinedOutput() //nolint:gosec // fixed per-OS command, no user input
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > maxCommandOutput {
			msg = msg[:maxCommandOutput]
		}
		return fmt.Errorf("platform: %s (%s): %w: %s", what, c, err, msg)
	}
	return nil
}
