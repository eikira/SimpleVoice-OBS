//go:build windows

package main

import (
	"os"
	"os/exec"
)

func restartEnabled() bool { return os.Getenv("RELAY_NO_RESTART") == "" }

// restartSelf starts a new copy that waits for this one to let go of the port.
func restartSelf() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	exec.Command(exe, "--restarted").Start()
}
