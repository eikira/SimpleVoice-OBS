//go:build !windows

// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

func freeMB(dir string) int {
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return -1
	}
	return int(uint64(st.Bavail) * uint64(st.Bsize) >> 20)
}
