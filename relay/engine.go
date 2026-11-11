// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The engine is a Python program (worker.py) that keeps the OmniVoice model loaded. This file
// starts it, restarts it if it dies, and sends it text.

type engineCfg struct {
	python, model, script, asr, kokoro string
	steps                              int
	startWait                          time.Duration // how long the model may take to load
}

type engine struct {
	cfg    engineCfg
	token  string
	client *http.Client
	busy   chan struct{} // one speech at a time

	mu          sync.Mutex
	state       string // missing | starting | ready | error | stopped
	msg         string
	dev         string
	port        int
	mode        string // auto | gpu | cpu: what the user chose
	kind        string // gpu | cpu: what is running (or starting) now
	fellBack    bool   // auto mode found no usable graphics card and uses the processor engine
	retry       bool   // the supervisor should start again at once (engine switch, not a failure)
	note        string
	asr, asrErr string
	builtin     []string // voices of the processor engine
	log         []string
	cmd         *exec.Cmd
	stdin       io.Closer
	steps       int    // decoding steps of the graphics card engine (quality)
	language    string // "" = automatic, otherwise a language code for the graphics card engine
	numbers     bool   // write numbers as words before speaking (graphics card engine, English)
	done        chan struct{}
	wg          sync.WaitGroup
}

func newEngine(c engineCfg) *engine {
	return &engine{cfg: c, token: randomHex(16), client: &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{Proxy: nil}},
		busy: make(chan struct{}, 1), state: "stopped", mode: "auto", done: make(chan struct{}), steps: c.steps}
}

// setMode chooses the engine: auto (graphics card if there is one, else processor), gpu or cpu.
func (e *engine) setMode(m string) bool {
	if m != "auto" && m != "gpu" && m != "cpu" {
		return false
	}
	e.mu.Lock()
	e.mode, e.fellBack, e.note = m, false, ""
	e.mu.Unlock()
	return true
}

// backend decides what to run now.
func (e *engine) backend() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mode == "cpu" || e.mode == "auto" && e.fellBack {
		return "kokoro"
	}
	return "omnivoice"
}

func (e *engine) kindNow() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.kind == "" {
		return "gpu"
	}
	return e.kind
}

func (e *engine) builtinVoices() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.builtin...)
}

type engineStatus struct {
	State  string   `json:"state"`
	Msg    string   `json:"message"`
	Device string   `json:"device,omitempty"`
	Mode   string   `json:"mode"`
	Kind   string   `json:"kind"`
	Note   string   `json:"note,omitempty"`
	ASR    string   `json:"transcription"`
	ASRErr string   `json:"transcriptionError,omitempty"`
	Log    []string `json:"log"`
}

func (e *engine) status() engineStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	kind := e.kind
	if kind == "" {
		kind = "gpu"
	}
	return engineStatus{e.state, e.msg, e.dev, e.mode, kind, e.note, e.asr, e.asrErr, append([]string{}, e.log...)}
}

func (e *engine) set(state, msg string) {
	e.mu.Lock()
	e.state, e.msg = state, msg
	e.mu.Unlock()
}

func (e *engine) setSteps(n int) {
	e.mu.Lock()
	e.steps = n
	e.mu.Unlock()
}

func (e *engine) setOptions(language string, numbers bool) {
	e.mu.Lock()
	e.language, e.numbers = language, numbers
	e.mu.Unlock()
}

func (e *engine) getOptions() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.language, e.numbers
}

func (e *engine) getSteps() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.steps
}

func (e *engine) addLog(line string) {
	e.mu.Lock()
	e.log = append(e.log, time.Now().Format("15:04:05 ")+line)
	if len(e.log) > 400 { // a stall report alone is about 60 lines
		e.log = e.log[len(e.log)-400:]
	}
	e.mu.Unlock()
}

func (e *engine) lastLog() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n := len(e.log); n > 0 {
		return e.log[n-1]
	}
	return ""
}

type lineLog struct {
	e   *engine
	buf []byte
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(string(l.buf[:i])); line != "" {
			l.e.addLog(line)
			logf("engine: %s", line) // also in relay.log, which keeps everything
		}
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// start launches the supervisor. Calling it again after "missing" or "error" tries again.
func (e *engine) start() {
	e.mu.Lock()
	if e.state == "starting" || e.state == "ready" {
		e.mu.Unlock()
		return
	}
	e.state, e.msg, e.retry = "starting", "Loading the voice model...", false
	e.done = make(chan struct{})
	done := e.done
	e.mu.Unlock()
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.supervise(done) }()
}

func (e *engine) supervise(done chan struct{}) {
	for fails := 0; fails < 5; fails++ {
		select {
		case <-done:
			return
		default:
		}
		backend := e.backend()
		need := []string{e.cfg.python, e.cfg.script, e.cfg.model}
		if backend == "kokoro" {
			need[2] = e.cfg.kokoro
		}
		missing := ""
		for _, p := range need {
			if _, err := os.Stat(p); err != nil {
				missing = p
				break
			}
		}
		if missing != "" {
			e.mu.Lock()
			auto := e.mode == "auto" && backend == "omnivoice" && !e.fellBack
			if auto {
				e.fellBack, e.note = true, "The graphics card engine is not installed, so the processor engine is used."
			}
			e.mu.Unlock()
			if auto {
				e.addLog("The graphics card engine files are missing. Using the processor engine.")
				fails--
				continue
			}
			e.set("missing", "Engine files not found: "+missing+". Run Setup again.")
			e.addLog("Engine files not found: " + missing)
			return
		}
		began := time.Now()
		e.runOnce(done, backend)
		select {
		case <-done:
			return
		default:
		}
		e.mu.Lock()
		again := e.retry
		e.retry = false
		e.mu.Unlock()
		if again {
			fails--
			continue
		}
		if st := e.status(); st.State == "error" {
			return // the worker is alive and says why it cannot work; restarting would not help
		}
		e.set("starting", "The engine stopped ("+e.lastLog()+"). Restarting...")
		if time.Since(began) > time.Minute {
			fails = 0 // it ran for a while, so this is not a start-up loop
		}
		select {
		case <-done:
			return
		case <-time.After(2 * time.Second):
		}
	}
	e.set("error", "The engine keeps stopping: "+e.lastLog())
}

// runOnce starts the worker and returns when it has exited.
func (e *engine) runOnce(done chan struct{}, backend string) {
	port, err := freePort()
	if err != nil {
		e.set("error", err.Error())
		return
	}
	kind := "gpu"
	if backend == "kokoro" {
		kind = "cpu"
	}
	e.mu.Lock()
	e.kind, e.asr, e.asrErr = kind, "loading", ""
	e.mu.Unlock()
	e.addLog("Starting the " + map[string]string{"gpu": "graphics card", "cpu": "processor"}[kind] + " engine...")
	cmd := exec.Command(e.cfg.python, e.cfg.script, "--parent", strconv.Itoa(os.Getpid()), "--port", strconv.Itoa(port), "--backend", backend, "--model", e.cfg.model, "--kokoro", e.cfg.kokoro, "--asr", e.cfg.asr)
	cmd.Env = append(os.Environ(), "OMNIVOICE_TOKEN="+e.token, "PYTHONNOUSERSITE=1", "PYTHONUTF8=1", "HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1", "HF_HUB_DISABLE_TELEMETRY=1")
	hideWindow(cmd)
	out := &lineLog{e: e}
	cmd.Stdout, cmd.Stderr = out, out
	var in io.Closer = killer{cmd}
	if runtime.GOOS != "windows" {
		// Closing the pipe tells the worker to quit. Not on Windows: a worker thread blocked in a read
		// on a pipe froze the loading of NumPy there, so Windows gives it no pipe and stops it by force.
		w, err := cmd.StdinPipe()
		if err != nil {
			e.set("error", err.Error())
			return
		}
		in = w
	}
	if err := cmd.Start(); err != nil {
		e.set("error", "Could not start the engine: "+err.Error())
		e.addLog("Could not start the engine: " + err.Error())
		return
	}
	e.mu.Lock()
	e.cmd, e.stdin, e.port = cmd, in, port
	e.mu.Unlock()
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	go e.watchHealth(port, exited, done)
	select {
	case <-exited:
	case <-done:
		in.Close()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-exited
		}
	}
}

// killer is how a worker without a stdin pipe is told to quit.
type killer struct{ cmd *exec.Cmd }

func (k killer) Close() error {
	if k.cmd.Process != nil {
		return k.cmd.Process.Kill()
	}
	return nil
}

func (e *engine) watchHealth(port int, exited, done chan struct{}) {
	deadline := time.Now().Add(e.cfg.startWait)
	announced := ""
	for {
		select {
		case <-exited:
			return
		case <-done:
			return
		case <-time.After(300 * time.Millisecond):
		}
		var h struct {
			State, Error, Device, Reason string
			ASR                          string `json:"asr"`
			ASRError                     string `json:"asr_error"`
		}
		if err := e.call(context.Background(), http.MethodGet, port, "/health", nil, &h); err == nil {
			e.mu.Lock()
			if h.ASR != "" {
				e.asr, e.asrErr = h.ASR, h.ASRError
			}
			e.mu.Unlock()
			if h.ASR != announced && (h.ASR == "ready" || h.ASR == "error") {
				announced = h.ASR
				if h.ASR == "ready" {
					e.addLog("Transcription is ready.")
				} else {
					e.addLog("Transcription is not available: " + h.ASRError)
				}
			}
			switch h.State {
			case "ready":
				e.mu.Lock()
				first := e.state != "ready"
				kind := e.kind
				e.mu.Unlock()
				if first && kind == "cpu" { // the voice list must be known before the engine counts as ready
					var v struct{ Voices []string }
					if e.call(context.Background(), http.MethodGet, port, "/voices", nil, &v) == nil {
						e.mu.Lock()
						e.builtin = v.Voices
						e.mu.Unlock()
					}
				}
				e.mu.Lock()
				e.state, e.msg, e.dev = "ready", "Ready", h.Device
				e.mu.Unlock()
				if first {
					e.addLog("Engine ready (" + h.Device + ").")
				}
				if announced != "" {
					return
				}
			case "error":
				e.mu.Lock()
				switch {
				case e.mode == "auto" && !e.fellBack && e.kind == "gpu":
					// Automatic mode: whatever stops the graphics card engine, the processor engine takes over.
					e.fellBack, e.retry = true, true
					msg := "No NVIDIA graphics card was found, so the processor engine is used."
					if h.Reason != "no_gpu" {
						msg = "The graphics card engine could not start (" + h.Error + "), so the processor engine is used. Switch the engine in the panel to try the graphics card again."
					}
					e.note = msg
					e.mu.Unlock()
					if h.Reason == "no_gpu" {
						e.addLog("No NVIDIA graphics card was found. Switching to the processor engine.")
					} else {
						e.addLog("The graphics card engine failed: " + h.Error + ". Switching to the processor engine.")
					}
					e.stopWorker()
					return
				}
				e.state, e.msg = "error", h.Error
				e.mu.Unlock()
				e.addLog("The engine cannot start: " + h.Error)
				return
			}
		}
		if time.Now().After(deadline) {
			e.set("error", "The voice model did not finish loading in time.")
			e.addLog("The voice model did not finish loading in time.")
			return
		}
	}
}

// stopWorker tells the running worker to quit (closing its stdin does that).
func (e *engine) stopWorker() {
	e.mu.Lock()
	in := e.stdin
	e.mu.Unlock()
	if in != nil {
		in.Close()
	}
}

func (e *engine) call(ctx context.Context, method string, port int, path string, body []byte, into interface{}) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:"+strconv.Itoa(port)+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Worker-Token", e.token)
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != 200 {
		var er struct{ Error string }
		json.Unmarshal(data, &er)
		if er.Error == "" {
			er.Error = resp.Status
		}
		return errors.New(er.Error)
	}
	if b, ok := into.(*[]byte); ok {
		*b = data
		return nil
	}
	return json.Unmarshal(data, into)
}

// errBusy is returned while the model is still loading or the engine cannot work.
type errEngine struct{ msg string }

func (e errEngine) Error() string { return e.msg }

// waitReady lets a request that arrives while the model loads wait for it, up to a limit.
func (e *engine) waitReady(ctx context.Context) error {
	for waited := time.Duration(0); ; waited += 200 * time.Millisecond {
		st := e.status()
		if st.State == "ready" {
			return nil
		}
		if st.State != "starting" || waited > 45*time.Second {
			return errEngine{"The voice engine is not ready: " + st.Msg}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// speak returns a WAV file.
func (e *engine) speak(ctx context.Context, text, voice, wav, refText string, speed float64) ([]byte, error) {
	if err := e.waitReady(ctx); err != nil {
		return nil, err
	}
	select {
	case e.busy <- struct{}{}:
		defer func() { <-e.busy }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	e.mu.Lock()
	port := e.port
	e.mu.Unlock()
	e.mu.Lock()
	steps, language, numbers := e.steps, e.language, e.numbers
	e.mu.Unlock()
	body, _ := json.Marshal(map[string]interface{}{"text": text, "voice": voice, "wav": wav, "ref_text": refText, "speed": speed, "steps": steps, "language": language, "numbers": numbers})
	var out []byte
	if err := e.call(ctx, http.MethodPost, port, "/speak", body, &out); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("speech failed: %w", err)
	}
	return out, nil
}

// transcribe returns the words spoken in a WAV file.
func (e *engine) transcribe(ctx context.Context, wav string) (string, error) {
	if err := e.waitReady(ctx); err != nil {
		return "", err
	}
	e.mu.Lock()
	port := e.port
	e.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"wav": wav})
	var out struct{ Text string }
	if err := e.call(ctx, http.MethodPost, port, "/transcribe", body, &out); err != nil {
		return "", fmt.Errorf("transcription failed: %w", err)
	}
	return out.Text, nil
}

func (e *engine) stop() {
	e.mu.Lock()
	select {
	case <-e.done:
	default:
		close(e.done)
	}
	e.state = "stopped"
	e.mu.Unlock()
	e.wg.Wait() // the old model must be gone before a new one loads: video memory is tight
}
