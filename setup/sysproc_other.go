//go:build !windows

// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import "os/exec"

func hideWindow(*exec.Cmd) {}
