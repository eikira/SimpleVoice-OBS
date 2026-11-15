package main

import (
	"net"
	"testing"
	"time"
)

func TestGatewayGuess(t *testing.T) {
	if g := gatewayOrGuess("10.10.10.16"); g == "" || g == "10.10.10.16" {
		// a real gateway may be found on this machine only if it has that address; otherwise the guess is .1
		t.Errorf("got %q", g)
	}
	if g := gatewayOrGuess("10.10.10.16"); g != "10.10.10.1" {
		t.Logf("gateway from the system: %s", g)
	}
	if g := gatewayOrGuess("10.10.10.1"); g == "10.10.10.1" {
		t.Error("an address must not be its own gateway")
	}
	if gatewayOrGuess("nonsense") != "" {
		t.Error("bad address")
	}
}

// The attempt really leaves from the chosen local address and reaches a listener.
func TestProbeDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			got <- c.RemoteAddr().(*net.TCPAddr).IP.String()
			c.Close()
		}
	}()
	if err := probeDial("127.0.0.1", ln.Addr().String(), time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case ip := <-got:
		if ip != "127.0.0.1" {
			t.Errorf("came from %s", ip)
		}
	case <-time.After(2 * time.Second):
		t.Error("nothing arrived")
	}
	// nothing listens: the attempt fails quietly
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln2.Addr().String()
	ln2.Close()
	if probeDial("127.0.0.1", addr, 500*time.Millisecond) == nil {
		t.Error("should fail")
	}
}

func TestProbeSkipsLocalOnly(t *testing.T) {
	firewallProbe("127.0.0.1") // must return at once, without logging or dialing
	firewallProbe("0.0.0.0")
	firewallProbe("not an ip")
}
