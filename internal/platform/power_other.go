//go:build !darwin && !linux && !windows

package platform

func sleepCommand() (Command, bool) { return Command{}, false }

func shutdownCommand() (Command, bool) { return Command{}, false }
