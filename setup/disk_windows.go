// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// freeMB is the free space on the drive that holds dir (or its nearest existing parent).
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
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return -1
	}
	var free, total, totalFree uint64
	k := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")
	r, _, _ := k.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return -1
	}
	return int(free >> 20)
}
