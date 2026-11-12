// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

// Command setup is the one installer for SimpleVoice OBS: it installs the relay program, Python,
// PyTorch, OmniVoice and the voice model, makes the shortcuts, and uninstalls again. Its window
// is a plain app window (no tabs, no address bar) that shows a local page.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var uiHTML []byte

type server struct {
	ctx       context.Context
	host      string
	dir       string
	run       func(context.Context) error
	uninstall func(deleteVoices bool) error
	present   func() bool
	setOpts   func(options)
	launch    func() error
	focus     string // "uninstall" when started from Windows' app list
	access    func() []need
	parts     func() ([]compInfo, compInfo)
	setSel    func(selection)
	hw        func() hardware
	plan      func(selection) (int, int)
	voices    func() []voiceInfo
	wg        sync.WaitGroup
	quit      chan struct{}
	pres      *presence // is the page still open

	mu    sync.Mutex
	mode  string // install or uninstall
	state string // idle, running, done, failed
	err   string
	lines []string
}

func (s *server) logf(l string) {
	fmt.Println(l)
	s.mu.Lock()
	s.lines = append(s.lines, l)
	s.mu.Unlock()
}

// guard blocks other web pages: wrong Host (DNS rebinding) and POSTs without the header.
func (s *server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.host || (r.Method == http.MethodPost && r.Header.Get("X-Setup") != "1") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.pres != nil {
			s.pres.seen() // every request of the page counts as a sign of life
		}
		h(w, r)
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Language", "en")
		w.Write(bytes.ReplaceAll(uiHTML, []byte("{{BUILD}}"), []byte(setupBuild)))
	}))
	mux.HandleFunc("GET /state", s.guard(s.handleState))
	mux.HandleFunc("GET /plan", s.guard(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sel := selection{CPU: q.Get("cpu") == "1", GPU: q.Get("gpu") == "1", ASR: q.Get("asr") == "1"}
		disk, load := s.plan(sel)
		json.NewEncoder(w).Encode(map[string]int{"diskMB": disk, "downloadMB": load})
	}))
	mux.HandleFunc("POST /start", s.guard(s.handleStart))
	mux.HandleFunc("POST /uninstall", s.guard(s.handleUninstall))
	mux.HandleFunc("POST /launch", s.guard(func(w http.ResponseWriter, r *http.Request) {
		if err := s.launch(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	// The page says goodbye when its window closes. A beacon cannot add the X-Setup header, so this one only
	// checks the host. It does nothing by itself: if the page asks again within a few seconds (a reload),
	// the goodbye is forgotten.
	mux.HandleFunc("POST /bye", func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.pres != nil {
			s.pres.goodbye()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /quit", s.guard(func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.quit <- struct{}{}:
		default:
		}
	}))
	return mux
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	var from int
	fmt.Sscan(r.URL.Query().Get("from"), &from)
	s.mu.Lock()
	if from < 0 || from > len(s.lines) {
		from = 0
	}
	resp := map[string]any{"state": s.state, "mode": s.mode, "error": s.err, "dir": s.dir, "focus": s.focus,
		"lines": append([]string{}, s.lines[from:]...), "next": len(s.lines)}
	s.mu.Unlock()
	resp["installed"] = s.present()
	resp["access"] = s.access()
	list, libs := s.parts()
	resp["components"], resp["libs"], resp["hardware"] = list, libs, s.hw()
	if s.voices != nil {
		resp["voices"] = s.voices()
	}
	json.NewEncoder(w).Encode(resp)
}

// begin marks a job as running. Only one job runs at a time.
func (s *server) begin(mode string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == "running" {
		return false
	}
	s.mode, s.state, s.err = mode, "running", ""
	return true
}

func (s *server) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.state, s.err = "failed", err.Error()
		s.lines = append(s.lines, "Stopped: "+err.Error())
		return
	}
	s.state = "done"
}

func (s *server) handleStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Accepted, Desktop, Autostart bool
		Components                   []string
		Voices                       []string
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || !body.Accepted {
		http.Error(w, "Please tick the box first.", http.StatusBadRequest)
		return
	}
	sel := selection{CPU: true, GPU: true, ASR: true} // no list sent: everything
	if body.Components != nil {
		sel = selection{}
	}
	sel.Voices = body.Voices
	for _, c := range body.Components {
		switch c {
		case "cpu":
			sel.CPU = true
		case "gpu":
			sel.GPU = true
		case "asr":
			sel.ASR = true
		}
	}
	if s.hw != nil && !s.hw().Usable { // no card this build can use: nothing that only the graphics card engine needs
		sel.GPU, sel.ASR = false, false
	}
	list, _ := s.parts()
	haveEngine := sel.CPU || sel.GPU
	for _, c := range list {
		if c.Installed && (c.ID == "cpu" || c.ID == "gpu") {
			haveEngine = true
		}
	}
	if !haveEngine {
		http.Error(w, "Tick at least one engine: the processor engine or the graphics card engine.", http.StatusBadRequest)
		return
	}
	s.setSel(sel)
	s.setOpts(options{Desktop: body.Desktop, Autostart: body.Autostart})
	if !s.begin("install") {
		http.Error(w, "already running", http.StatusConflict)
		return
	}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.finish(s.run(s.ctx)) }()
}

func (s *server) handleUninstall(w http.ResponseWriter, r *http.Request) {
	var body struct{ DeleteVoices bool }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.begin("uninstall") {
		http.Error(w, "already running", http.StatusConflict)
		return
	}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.finish(s.uninstall(body.DeleteVoices)) }()
}

func defaultDir() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "SimpleVoice-OBS")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".simplevoice-obs")
}

// appWindow opens the page in its own window (Edge or Chrome in app mode: no tabs, no address bar).
// The returned channel closes when the user closes that window. It is nil when only the normal
// browser could be used, because then the window cannot be watched.
func appWindow(url, profile string) <-chan struct{} {
	if runtime.GOOS == "windows" {
		var roots []string
		for _, e := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
			if v := os.Getenv(e); v != "" {
				roots = append(roots, v)
			}
		}
		for _, rel := range []string{`Microsoft\Edge\Application\msedge.exe`, `Google\Chrome\Application\chrome.exe`} {
			for _, root := range roots {
				exe := filepath.Join(root, rel)
				if _, err := os.Stat(exe); err != nil {
					continue
				}
				c := exec.Command(exe, "--app="+url, "--user-data-dir="+profile, "--no-first-run", "--no-default-browser-check", "--window-size=820,900")
				started := time.Now()
				if c.Start() != nil {
					continue
				}
				closed := make(chan struct{})
				go func() { c.Wait(); close(closed) }()
				time.Sleep(2 * time.Second)
				select {
				case <-closed:
					if time.Since(started) < 5*time.Second { // it handed over to another process: cannot be watched
						return nil
					}
				default:
				}
				return closed
			}
		}
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
		return nil
	}
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	exec.Command(cmd, url).Start()
	return nil
}

const (
	profileName    = "simplevoice-obs-setup-" // start of the temporary browser profile folder of one run
	productionName = "SimpleVoice-OBS-Setup.exe"
)

func profilePrefix() string { return filepath.Join(os.TempDir(), profileName) }

func isProductionExe(path string) bool { return strings.EqualFold(filepath.Base(path), productionName) }

// removeOldProfiles deletes the temporary browser profile folders that earlier runs could not remove.
func removeOldProfiles() {
	old, _ := filepath.Glob(profilePrefix() + "*")
	for _, d := range old {
		os.RemoveAll(d)
	}
}

func main() {
	dir := flag.String("dir", defaultDir(), "install folder")
	uninstall := flag.Bool("uninstall", false, "open in uninstall mode")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	s := &server{ctx: ctx, host: ln.Addr().String(), dir: *dir, state: "idle", quit: make(chan struct{}, 1)}
	if *uninstall {
		s.focus = "uninstall"
	}
	inst := newInstaller(*dir, s.logf)
	inst.proxy = newProxy(inst.client, s.logf)
	s.run, s.uninstall, s.access = inst.run, inst.uninstall, inst.needs
	s.present = func() bool { return inst.installed(true) }
	s.setOpts = func(o options) { inst.opts = o }
	s.parts = inst.components
	s.setSel = func(v selection) { inst.sel = &v }
	s.plan = inst.planned
	s.voices = inst.voiceList
	var hwOnce sync.Once
	var gpu hardware
	s.hw = func() hardware {
		hwOnce.Do(func() { gpu = detectGPU() })
		h := gpu
		h.FreeMB = freeMB(*dir)
		return h
	}
	s.launch = inst.launch
	go http.Serve(ln, s.handler())

	// An earlier setup run that is still around (closed window, no exit) is ended first. Only for the real
	// program name, so a test build or a copy under another name never ends anything.
	if exe, err := os.Executable(); err == nil && isProductionExe(exe) {
		endOtherSetups()
		removeOldProfiles()
	}
	profile, _ := os.MkdirTemp("", profileName)
	s.pres = newPresence(nil)
	window := appWindow("http://"+s.host+"/", profile)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var waiting bool
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case <-s.quit:
			break wait
		case <-window: // the window process ended: the user closed it; a running install is reversed
			cancel()
			break wait
		case <-tick.C:
			closed, lost := s.pres.status()
			if closed { // the page said goodbye and did not come back: same as the window closing
				cancel()
				break wait
			}
			if lost {
				s.mu.Lock()
				running := s.state == "running"
				s.mu.Unlock()
				if !running {
					break wait
				}
				if !waiting { // no sign of the page, but nobody asked to stop: finish, then leave
					waiting = true
					s.logf("The setup window is not answering. The installation continues and setup closes when it is done.")
				}
			}
		}
	}
	s.wg.Wait()           // after closing, let a running install finish reversing itself
	closeBrowser(profile) // the app window and its helper processes must not keep the profile folder
	os.RemoveAll(profile)
	s.mu.Lock()
	removed := s.mode == "uninstall" && s.state == "done"
	s.mu.Unlock()
	if removed {
		selfDelete(*dir)
	}
}
