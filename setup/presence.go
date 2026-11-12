// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"sync"
	"time"
)

// presence tells whether the setup page is still open. The page asks /state every second, which is the
// heartbeat, and sends a "bye" when its window closes (the browser's pagehide event).
//
//   - closed: a bye arrived and no page request followed within the grace time (a reload sends a bye
//     too, but its new page asks at once). This is the user closing the window: a running install is
//     reversed, as before.
//   - lost: no request at all for a long time (a sleeping computer, a browser that throttles hidden
//     pages, a page that never loaded). This alone never cancels an install: setup finishes the job
//     and then leaves.
type presence struct {
	mu    sync.Mutex
	last  time.Time
	bye   time.Time
	clock func() time.Time
}

const (
	byeGrace  = 4 * time.Second
	lostAfter = 150 * time.Second // browsers may run a hidden page's timer only once a minute
)

func newPresence(clock func() time.Time) *presence {
	if clock == nil {
		clock = time.Now
	}
	return &presence{last: clock(), clock: clock}
}

// seen is called for every request the page makes.
func (p *presence) seen() {
	p.mu.Lock()
	p.last, p.bye = p.clock(), time.Time{}
	p.mu.Unlock()
}

// goodbye is called when the page says it is closing.
func (p *presence) goodbye() {
	p.mu.Lock()
	if p.bye.IsZero() {
		p.bye = p.clock()
	}
	p.mu.Unlock()
}

func (p *presence) status() (closed, lost bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	return !p.bye.IsZero() && now.Sub(p.bye) >= byeGrace, now.Sub(p.last) >= lostAfter
}
