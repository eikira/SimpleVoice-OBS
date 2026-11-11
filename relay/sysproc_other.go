//go:build !windows

// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later
package main

import "os/exec"

func hideWindow(*exec.Cmd) {}
