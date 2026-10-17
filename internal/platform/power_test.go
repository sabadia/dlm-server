package platform

import (
	"runtime"
	"testing"
)

// The commands are only inspected, never run: running them would suspend
// or power off the test machine.
func TestPowerCommands(t *testing.T) {
	supported := runtime.GOOS == "darwin" || runtime.GOOS == "linux" || runtime.GOOS == "windows"
	for name, get := range map[string]func() (Command, bool){"sleep": SleepCommand, "shutdown": ShutdownCommand} {
		c, ok := get()
		if ok != supported {
			t.Fatalf("%s: supported = %v on %s", name, ok, runtime.GOOS)
		}
		if ok && (c.Name == "" || len(c.Args) == 0) {
			t.Errorf("%s: empty command %+v", name, c)
		}
	}
}

func TestCommandString(t *testing.T) {
	if got := (Command{Name: "a", Args: []string{"b", "c"}}).String(); got != "a b c" {
		t.Errorf("String() = %q", got)
	}
}
