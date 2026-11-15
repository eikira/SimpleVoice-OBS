package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyIP(t *testing.T) {
	cases := map[string]string{
		"192.168.1.20": "lan", "10.10.10.5": "lan", "172.16.0.1": "lan", "172.31.255.1": "lan",
		"172.15.0.1": "public", "172.32.0.1": "public", "20.1.2.3": "public", "8.8.8.8": "public",
		"100.64.0.1": "shared", "100.127.255.1": "shared", "100.128.0.1": "public",
		"169.254.1.1": "", "127.0.0.1": "", "224.0.0.1": "", "0.0.0.0": "",
	}
	for ip, want := range cases {
		if got := classifyIP(net.ParseIP(ip)); got != want {
			t.Errorf("%s: got %q want %q", ip, got, want)
		}
	}
	if classifyIP(net.ParseIP("fe80::1")) != "" {
		t.Error("IPv6 must not be offered")
	}
}

func TestLooksVirtual(t *testing.T) {
	yes := []string{"Tailscale", "vEthernet (WSL)", "NordLynx", "tun0", "utun3", "wg0", "docker0", "br-1a2b3c", "virbr0", "veth12ab", "VMware Network Adapter VMnet8", "VirtualBox Host-Only Network", "OpenVPN TAP-Windows", "ppp0", "bridge0", "Hyper-V Virtual Ethernet Adapter"}
	no := []string{"Ethernet", "Wi-Fi", "en0", "eth0", "enp4s0", "wlp3s0", "Ethernet 2", "Local Area Connection", "WLAN", "Laptop LAN"}
	for _, n := range yes {
		if !looksVirtual(n) {
			t.Errorf("%q should look virtual", n)
		}
	}
	for _, n := range no {
		if looksVirtual(n) {
			t.Errorf("%q should not look virtual", n)
		}
	}
}

func fakeAdapters(list []adapterAddr) func() {
	old := listAdapters
	listAdapters = func() []adapterAddr { return list }
	return func() { listAdapters = old }
}

func TestCheckHost(t *testing.T) {
	defer fakeAdapters([]adapterAddr{
		{Address: "192.168.1.20", Adapter: "Ethernet", Kind: "lan"},
		{Address: "20.5.5.5", Adapter: "Odd", Kind: "public"},
	})()
	if checkHost("127.0.0.1", false) != "" || checkHost("192.168.1.20", false) != "" {
		t.Error("loopback and a listed private address must be allowed")
	}
	if checkHost("0.0.0.0", false) == "" {
		t.Error("0.0.0.0 must not be saved from the page")
	}
	if checkHost("192.168.9.9", false) == "" {
		t.Error("an address no adapter has must be refused")
	}
	if checkHost("20.5.5.5", false) == "" || checkHost("20.5.5.5", true) != "" {
		t.Error("a public address needs the confirmation")
	}
	for _, bad := range []string{"", "abc", "::1", "192.168.1.20:80", "192.168.1.20 "} {
		if checkHost(bad, true) == "" {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestResolveHost(t *testing.T) {
	c := config{host: "127.0.0.1", port: "1"}
	if resolveHost(&c) != "" || c.host != "127.0.0.1" {
		t.Error("loopback stays")
	}
	c = config{host: "0.0.0.0", port: "1"}
	if resolveHost(&c) != "" || c.host != "0.0.0.0" {
		t.Error("an old 0.0.0.0 setting stays as it was")
	}
	c = config{host: "198.51.100.77", port: "1"} // documentation range: no adapter has it
	note := resolveHost(&c)
	if note == "" || c.host != "127.0.0.1" || !strings.Contains(note, "198.51.100.77") {
		t.Errorf("a vanished address must fall back to 127.0.0.1 with a note: %q %q", note, c.host)
	}
}

func TestDialHost(t *testing.T) {
	for host, want := range map[string]string{"": "127.0.0.1", "0.0.0.0": "127.0.0.1", "localhost": "127.0.0.1", "127.0.0.1": "127.0.0.1", "192.168.1.20": "192.168.1.20"} {
		if got := dialHost(config{host: host}); got != want {
			t.Errorf("%q: got %q want %q", host, got, want)
		}
	}
}

func TestNetworkEndpoints(t *testing.T) {
	defer fakeAdapters([]adapterAddr{{Address: "192.168.1.20", Adapter: "Ethernet", Kind: "lan"}, {Address: "10.8.0.2", Adapter: "tun0", Kind: "lan", Virtual: true}})()
	dir := t.TempDir()
	os.Setenv("RELAY_NO_RESTART", "1")
	defer os.Unsetenv("RELAY_NO_RESTART")
	// keep settings.txt out of the source folder: the config folder is the fallback, but writeSetting
	// tries the program folder first, so point both to the temp dir through HOME / APPDATA.
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Setenv("HOME", dir)
	os.Setenv("APPDATA", dir)
	exe, _ := os.Executable()
	primary := filepath.Join(filepath.Dir(exe), "settings.txt")
	if _, err := os.Stat(primary); err == nil {
		t.Skip("settings.txt exists next to the test binary")
	}
	defer os.Remove(primary)
	activeCfg = config{host: "127.0.0.1", port: "8787"}
	r := newRelay(nil, voiceStore{dir: dir})
	srv := httptest.NewServer(r)
	defer srv.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	post := func(body string, header bool) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/network/host", strings.NewReader(body))
		if header {
			req.Header.Set("X-Relay-Open", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}

	if code, body := get("/network"); code != 200 || !strings.Contains(body, "Do not choose a VPN") {
		t.Errorf("page: %d", code)
	}
	code, body := get("/api/network")
	var info networkInfo
	json.Unmarshal([]byte(body), &info)
	if code != 200 || info.Listening != "127.0.0.1" || len(info.Adapters) != 2 || info.Port != "8787" {
		t.Errorf("api: %d %s", code, body)
	}
	if code, _ := post(`{"host":"192.168.1.20"}`, false); code != 403 {
		t.Errorf("without the header must be refused, got %d", code)
	}
	if code, _ := post(`{"host":"192.168.7.7"}`, true); code != 400 {
		t.Errorf("an address no adapter has must be refused, got %d", code)
	}
	if code, _ := post(`not json`, true); code != 400 {
		t.Errorf("bad json must be refused, got %d", code)
	}
	if code, body := post(`{"host":"192.168.1.20"}`, true); code != 200 {
		t.Errorf("a listed address must be saved, got %d %s", code, body)
	}
	if got := readSettings()["HOST"]; got != "192.168.1.20" {
		t.Errorf("HOST was not saved: %q", got)
	}
}

func TestOriginFirefox(t *testing.T) {
	r, _ := http.NewRequest("GET", "http://127.0.0.1:8787/v1/audio/voices", nil)
	r.Header.Set("Origin", "moz-extension://abc-123")
	if !originOK(r) {
		t.Error("the Firefox extension must be accepted")
	}
	r.Header.Set("Origin", "https://evil.example")
	if originOK(r) {
		t.Error("a web page must be refused")
	}
}

func TestPanelIsLocalOnly(t *testing.T) {
	r := newRelay(nil, voiceStore{dir: t.TempDir()})
	for _, p := range []string{"/", "/api/status"} {
		req := httptest.NewRequest("GET", "http://192.168.1.20:8787"+p, nil)
		req.RemoteAddr = "192.168.1.55:50000" // another computer
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Errorf("%s from another computer must be refused, got %d", p, w.Code)
		}
	}
}

func TestListeningInfo(t *testing.T) {
	old := listAdapters
	defer func() { listAdapters = old }()
	listAdapters = func() []adapterAddr {
		return []adapterAddr{{Address: "192.168.1.20", Adapter: "Ethernet", Kind: "lan"}}
	}
	activeCfg = config{host: "192.168.1.20", port: "8787"}
	if h, a := listeningInfo(); h != "192.168.1.20" || a != "Ethernet" {
		t.Errorf("got %q %q", h, a)
	}
	activeCfg = config{host: "127.0.0.1", port: "8787"}
	if h, a := listeningInfo(); h != "127.0.0.1" || a != "" {
		t.Errorf("got %q %q", h, a)
	}
}
