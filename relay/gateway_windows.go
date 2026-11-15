//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// gatewayFor returns the default gateway of the adapter that has this address ("" if not found).
func gatewayFor(ip string) string {
	size := uint32(15000)
	buf := make([]byte, size)
	err := syscall.GetAdaptersInfo((*syscall.IpAdapterInfo)(unsafe.Pointer(&buf[0])), &size)
	if err == syscall.ERROR_BUFFER_OVERFLOW {
		buf = make([]byte, size)
		err = syscall.GetAdaptersInfo((*syscall.IpAdapterInfo)(unsafe.Pointer(&buf[0])), &size)
	}
	if err != nil {
		return ""
	}
	for ai := (*syscall.IpAdapterInfo)(unsafe.Pointer(&buf[0])); ai != nil; ai = ai.Next {
		for a := &ai.IpAddressList; a != nil; a = a.Next {
			if cstr(a.IpAddress.String[:]) == ip {
				return cstr(ai.GatewayList.IpAddress.String[:])
			}
		}
	}
	return ""
}
