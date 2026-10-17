//go:build windows

package platform

// sleepCommand calls SetSuspendState(hibernate=0, force=1,
// disableWakeEvent=0). When hibernation is enabled Windows hibernates
// instead of sleeping.
func sleepCommand() (Command, bool) {
	return Command{Name: "rundll32.exe", Args: []string{"powrprof.dll,SetSuspendState", "0,1,0"}}, true
}

func shutdownCommand() (Command, bool) {
	return Command{Name: "shutdown.exe", Args: []string{"/s", "/t", "0"}}, true
}
