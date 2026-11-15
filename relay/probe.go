// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later
//
// A firewall (Windows Firewall, simplewall and similar) only asks about a program when it sees traffic
// of that program. Listening on a network address alone shows nothing. So right after the relay starts
// on a network address it makes one short connection attempt from that address to the gateway of that
// network. The answer does not matter (it may fail); the attempt only has to leave the program so the
// firewall can show its question. Nothing is sent to the internet.

package main

import (
	"net"
	"strings"
	"time"
)

// gatewayOrGuess returns the gateway of the network the address belongs to, or the .1 address of its /24.
func gatewayOrGuess(ip string) string {
	if g := gatewayFor(ip); g != "" && g != "0.0.0.0" && g != ip {
		return g
	}
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ""
	}
	g := strings.Join(parts[:3], ".") + ".1"
	if g == ip {
		return ""
	}
	return g
}

// probeDial makes one connection attempt from `local` to target ("host:port"). Returns the error (nil = connected).
func probeDial(local, target string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout, LocalAddr: &net.TCPAddr{IP: net.ParseIP(local)}}
	c, err := d.Dial("tcp", target)
	if err == nil {
		c.Close()
	}
	return err
}

// firewallProbe runs after the relay listens on `host`. Only for a specific private/network address.
func firewallProbe(host string) {
	if isLocalOnly(host) || net.ParseIP(host) == nil || net.ParseIP(host).IsUnspecified() {
		return
	}
	gw := gatewayOrGuess(host)
	if gw == "" {
		return
	}
	err := probeDial(host, net.JoinHostPort(gw, "80"), 1500*time.Millisecond)
	if err != nil {
		logf("Firewall check: one connection attempt from %s to %s:80 (%s). It only lets your firewall ask about this program; allow it for your private network.", host, gw, shortErr(err))
	} else {
		logf("Firewall check: one connection attempt from %s to %s:80 (connected). Your firewall allows this program on the network.", host, gw)
	}
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}
