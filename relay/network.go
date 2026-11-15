// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later
//
// The listening address: which network adapter the relay answers on.
// The relay lists the addresses of this computer, shows which adapter each one belongs to, and lets the
// user pick one on a page that only this computer can open (/network). Standard library only.

package main

import (
	_ "embed"
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

//go:embed network.html
var networkHTML []byte

// adapterAddr is one IPv4 address of this computer.
type adapterAddr struct {
	Address string `json:"address"`
	Adapter string `json:"adapter"`
	Kind    string `json:"kind"` // "lan" (private range), "shared" (100.64.x.x: often a VPN), "public" (anything else)
	Virtual bool   `json:"virtual"`
	Note    string `json:"note"`
}

// Names that mean a VPN, a virtual machine, a container or a tunnel. Long words are searched in the
// whole name; short ones only at the start of a word, so "laptop" does not count as "tap".
var virtualLong = []string{"vpn", "tailscale", "zerotier", "wireguard", "hyper-v", "vethernet", "wsl", "vmware", "vmnet",
	"virtualbox", "vbox", "docker", "hamachi", "openvpn", "anyconnect", "forticlient", "nordlynx", "protonvpn",
	"podman", "virtual", "teredo", "isatap"}
var virtualPrefix = []string{"tun", "tap", "utun", "ppp", "veth", "virbr", "docker", "vnet"}
var virtualExact = []string{"wg", "br", "bridge"}

func looksVirtual(name string) bool {
	low := strings.ToLower(name)
	for _, w := range virtualLong {
		if strings.Contains(low, w) {
			return true
		}
	}
	words := strings.FieldsFunc(low, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for _, word := range words {
		for _, w := range virtualPrefix {
			if strings.HasPrefix(word, w) {
				return true
			}
		}
		stem := strings.TrimRight(word, "0123456789")
		for _, w := range virtualExact {
			if stem == w {
				return true
			}
		}
	}
	return false
}

func classifyIP(ip net.IP) string {
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	switch {
	case ip4[0] == 10,
		ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31,
		ip4[0] == 192 && ip4[1] == 168:
		return "lan"
	case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127:
		return "shared"
	case ip4[0] == 169 && ip4[1] == 254, ip4[0] == 127, ip4[0] == 0, ip4[0] >= 224:
		return "" // link-local, loopback, multicast: never offered
	}
	return "public"
}

// listAdapters can be replaced by the tests.
var listAdapters = realAdapters

// realAdapters returns the IPv4 addresses of the adapters that are up, private ones first.
func realAdapters() []adapterAddr {
	var out []adapterAddr
	ifs, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			kind := classifyIP(ipn.IP)
			if kind == "" {
				continue
			}
			aa := adapterAddr{Address: ipn.IP.To4().String(), Adapter: ifc.Name, Kind: kind, Virtual: looksVirtual(ifc.Name)}
			switch {
			case aa.Kind == "shared":
				aa.Virtual = true
				aa.Note = "Probably a VPN (the 100.64.x.x range)."
			case aa.Virtual:
				aa.Note = "Looks like a VPN or a virtual adapter."
			case aa.Kind == "public":
				aa.Note = "Not a private address. It may be reachable from the internet."
			}
			out = append(out, aa)
		}
	}
	rank := func(a adapterAddr) int {
		switch {
		case a.Kind == "lan" && !a.Virtual:
			return 0
		case a.Kind == "lan":
			return 1
		case a.Kind == "shared":
			return 2
		}
		return 3
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

// ownIPs are all addresses of this computer (also the ones of adapters that are down).
func ownIPs() map[string]bool {
	m := map[string]bool{}
	ifs, err := net.Interfaces()
	if err != nil {
		return m
	}
	for _, ifc := range ifs {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				m[ipn.IP.String()] = true
			}
		}
	}
	return m
}

// isLocalClient: the request comes from this computer. When the relay listens on a LAN address, this
// computer reaches it from that same address, not from 127.0.0.1, so own addresses count too.
func isLocalClient(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ownIPs()[ip.String()]
}

// dialHost is the address this computer uses to reach its own relay.
func dialHost(c config) string {
	if c.host == "" || c.host == "0.0.0.0" || c.host == "localhost" || c.host == "::" {
		return "127.0.0.1"
	}
	return c.host
}

func isLocalOnly(host string) bool { return host == "127.0.0.1" || host == "localhost" }

// resolveHost keeps a saved address only if this computer still has it. Home networks hand out
// changing addresses; a relay that cannot bind would not start at all. The reason is returned so the
// page and the log can say it.
func resolveHost(c *config) string {
	h := c.host
	if h == "" || h == "0.0.0.0" || isLocalOnly(h) {
		return ""
	}
	ip := net.ParseIP(h)
	if ip != nil && ownIPs()[ip.String()] {
		return ""
	}
	c.host = "127.0.0.1"
	return "The saved address " + h + " does not belong to this computer any more (the network may have changed). The relay listens on this PC only (127.0.0.1) until you choose an address again."
}

// checkHost says whether the address may be saved. The wide address 0.0.0.0 is not offered here.
func checkHost(h string, confirmPublic bool) string {
	if h == "127.0.0.1" {
		return ""
	}
	ip := net.ParseIP(h)
	if ip == nil || ip.To4() == nil {
		return "That is not an IPv4 address."
	}
	for _, a := range listAdapters() {
		if a.Address == h {
			if a.Kind == "public" && !confirmPublic {
				return "This is not a private address. Tick the box to confirm that you know what you are doing."
			}
			return ""
		}
	}
	return "This computer has no active adapter with that address."
}

var hostFallbackNote string // set at start when a saved address could not be used

type networkInfo struct {
	Listening string        `json:"listening"` // the address the relay listens on now
	Port      string        `json:"port"`
	Saved     string        `json:"saved"` // what settings.txt says
	Note      string        `json:"note"`  // why the saved address is not used
	Adapters  []adapterAddr `json:"adapters"`
}

func currentNetwork() networkInfo {
	saved := readSettings()["HOST"]
	if saved == "" {
		saved = "127.0.0.1"
	}
	return networkInfo{Listening: activeCfg.host, Port: activeCfg.port, Saved: saved, Note: hostFallbackNote, Adapters: listAdapters()}
}

// handleNetwork serves the page and its two calls. Only this computer may use them.
func (r *relay) handleNetwork(w http.ResponseWriter, req *http.Request, path string) bool {
	switch {
	case req.Method == http.MethodGet && path == "/network":
		if !isLocalClient(req.RemoteAddr) {
			fail(w, 403, "forbidden")
			return true
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(networkHTML)
	case req.Method == http.MethodGet && path == "/api/network":
		if !isLocalClient(req.RemoteAddr) {
			fail(w, 403, "forbidden")
			return true
		}
		writeJSON(w, 200, currentNetwork())
	case req.Method == http.MethodPost && path == "/api/network/host":
		if !isLocalClient(req.RemoteAddr) || req.Header.Get("X-Relay-Open") != "1" {
			fail(w, 403, "forbidden")
			return true
		}
		body, err := readBody(req)
		if err != nil {
			fail(w, 413, "bad body")
			return true
		}
		var in struct {
			Host          string `json:"host"`
			ConfirmPublic bool   `json:"confirmPublic"`
		}
		if json.Unmarshal(body, &in) != nil {
			fail(w, 400, "invalid json")
			return true
		}
		in.Host = strings.TrimSpace(in.Host)
		if msg := checkHost(in.Host, in.ConfirmPublic); msg != "" {
			fail(w, 400, msg)
			return true
		}
		if err := writeSetting("HOST", in.Host); err != nil {
			fail(w, 500, "could not save the setting: "+err.Error())
			return true
		}
		logf("Listening address saved: %s. Restarting.", in.Host)
		writeJSON(w, 200, map[string]interface{}{"ok": true, "host": in.Host, "port": activeCfg.port, "restart": restartEnabled()})
		go func() {
			time.Sleep(300 * time.Millisecond)
			if restartEnabled() {
				restartSelf()
			}
			r.requestQuit()
		}()
	default:
		return false
	}
	return true
}

// listeningInfo says where the relay listens and, if that is one of this computer's addresses, which adapter it is
// (shown by the extension next to the relay address).
func listeningInfo() (host, adapter string) {
	host = activeCfg.host
	if host == "" {
		host = "127.0.0.1"
	}
	for _, a := range listAdapters() {
		if a.Address == host {
			return host, a.Adapter
		}
	}
	return host, ""
}

func listenHost() string    { h, _ := listeningInfo(); return h }
func listenAdapter() string { _, a := listeningInfo(); return a }
