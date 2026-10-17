//go:build darwin

package platform

func sleepCommand() (Command, bool) {
	return Command{Name: "/usr/bin/pmset", Args: []string{"sleepnow"}}, true
}

// shutdownCommand asks System Events to shut down, which works without
// root privileges (unlike shutdown -h now) and lets apps quit cleanly.
func shutdownCommand() (Command, bool) {
	return Command{Name: "/usr/bin/osascript", Args: []string{"-e", `tell application "System Events" to shut down`}}, true
}
