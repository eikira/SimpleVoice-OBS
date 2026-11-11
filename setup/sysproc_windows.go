// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"os/exec"
	"syscall"
)

// hideWindow stops a console window from flashing up for each helper program setup starts.
func hideWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
