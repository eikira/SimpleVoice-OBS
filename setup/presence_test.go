// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }
func newFake() (*fakeClock, *presence) {
	c := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	return c, newPresence(c.now)
}
func state(p *presence) (closed, lost bool) { return p.status() }

func TestPresenceWindowClosedNeedsAGoodbyeThatIsNotFollowedByAPageRequest(t *testing.T) {
	c, p := newFake()
	p.seen()
	c.add(30 * time.Second)
	if cl, lo := state(p); cl || lo {
		t.Fatalf("a page that asks regularly is there: %v %v", cl, lo)
	}
	p.goodbye()
	c.add(byeGrace - time.Second)
	if cl, _ := state(p); cl {
		t.Fatal("a goodbye is not final before the grace time")
	}
	c.add(2 * time.Second)
	if cl, _ := state(p); !cl {
		t.Fatal("goodbye and silence for the grace time: the window was closed")
	}
}

func TestPresenceReloadIsNotAClose(t *testing.T) {
	c, p := newFake()
	p.goodbye() // reload: pagehide
	c.add(time.Second)
	p.seen() // the new page asks at once
	c.add(time.Minute)
	p.seen()
	if cl, _ := state(p); cl {
		t.Fatal("a reload must not close setup")
	}
}

func TestPresenceSilenceAloneIsOnlyLostNeverClosed(t *testing.T) {
	c, p := newFake()
	p.seen()
	c.add(lostAfter - time.Second)
	if _, lo := state(p); lo {
		t.Fatal("not lost yet")
	}
	c.add(2 * time.Second)
	cl, lo := state(p)
	if !lo || cl {
		t.Fatalf("a page that fell silent is lost, not closed: closed=%v lost=%v", cl, lo)
	}
}

func TestPresenceAPageThatNeverLoadsIsLost(t *testing.T) {
	c, p := newFake()
	c.add(lostAfter + time.Second)
	if _, lo := state(p); !lo {
		t.Fatal("setup must not wait forever for a window that never opened")
	}
}

func TestByeEndpointAndHeartbeat(t *testing.T) {
	c, p := newFake()
	s := &server{host: "127.0.0.1:1", pres: p, state: "idle", quit: make(chan struct{}, 1)}
	h := s.handler()
	// a beacon has no X-Setup header: accepted for the right host only
	req := httptest.NewRequest("POST", "/bye", nil)
	req.Host = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("bye: %d", rec.Code)
	}
	bad := httptest.NewRequest("POST", "/bye", nil)
	bad.Host = "evil.example:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bad)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a foreign host must be refused: %d", rec.Code)
	}
	c.add(byeGrace + time.Second)
	if cl, _ := p.status(); !cl {
		t.Fatal("the beacon must have started the goodbye")
	}
	// the page asks again (any guarded request): the goodbye is forgotten
	get := httptest.NewRequest("GET", "/plan?cpu=1", nil)
	get.Host = "127.0.0.1:1"
	s.plan = func(selection) (int, int) { return 1, 1 }
	httptest.NewRecorder()
	h.ServeHTTP(httptest.NewRecorder(), get)
	if cl, _ := p.status(); cl {
		t.Fatal("a page request after the goodbye cancels it")
	}
	// the other POST routes still need the header
	q := httptest.NewRequest("POST", "/quit", nil)
	q.Host = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, q)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("/quit without the header: %d", rec.Code)
	}
}

func TestOnlyTheProductionProgramEndsOtherSetups(t *testing.T) {
	for p, want := range map[string]bool{
		`C:\x\SimpleVoice-OBS-Setup.exe`:     true,
		`c:\x\simplevoice-obs-setup.EXE`:     true,
		`/tmp/SimpleVoice-OBS-Setup.exe`:     true,
		`C:\x\SimpleVoice-OBS-Setup-1.exe`:   false,
		`C:\x\setup.test.exe`:                false,
		`/tmp/go-build123/setup.test`:        false,
		`C:\x\SimpleVoice-OBS-Setup.exe.bak`: false,
	} {
		if got := isProductionExe(filepath.FromSlash(strings.ReplaceAll(p, `\`, "/"))); got != want {
			t.Errorf("isProductionExe(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestOldProfileFoldersAreRemoved(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("TMP", tmp)
	mine := filepath.Join(tmp, profileName+"123")
	other := filepath.Join(tmp, "something-else")
	os.MkdirAll(filepath.Join(mine, "Default"), 0o755)
	os.MkdirAll(other, 0o755)
	removeOldProfiles()
	if _, err := os.Stat(mine); err == nil {
		t.Fatal("an old profile folder must be removed")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("other folders must stay")
	}
}
