// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps the engine's console window from flashing up.
func hideWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
