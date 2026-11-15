//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func restartEnabled() bool { return os.Getenv("RELAY_NO_RESTART") == "" }

// restartSelf starts a new copy that waits for this one to let go of the port.
func restartSelf() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	c := exec.Command(exe, "--restarted")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Start()
}
