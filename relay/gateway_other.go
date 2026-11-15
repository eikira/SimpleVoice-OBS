//go:build !windows

package main

import (
	"encoding/hex"
	"net"
	"os"
	"strings"
)

// gatewayFor: on Linux the default route is read from /proc/net/route; elsewhere "" (the .1 guess is used).
func gatewayFor(ip string) string {
	iface := ifaceOf(ip)
	if iface == "" {
		return ""
	}
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != iface || f[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(f[2])
		if err != nil || len(b) != 4 {
			continue
		}
		return net.IPv4(b[3], b[2], b[1], b[0]).String() // stored in the computer's byte order
	}
	return ""
}

// ifaceOf is the name of the adapter that has this address.
func ifaceOf(ip string) string {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.String() == ip {
				return i.Name
			}
		}
	}
	return ""
}
