//go:build linux

package platform

// Linux uses systemd; logind decides whether the user may suspend or power
// off without extra privileges.
func sleepCommand() (Command, bool) {
	return Command{Name: "systemctl", Args: []string{"suspend"}}, true
}

func shutdownCommand() (Command, bool) {
	return Command{Name: "systemctl", Args: []string{"poweroff"}}, true
}
