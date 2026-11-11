package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

//go:embed overlay.html
var overlayHTML []byte

//go:embed overlay.js
var overlayJS []byte

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
var formatRe = regexp.MustCompile(`^[a-z0-9]{2,8}$`)

const maxBody = 16 * 1024

// ---- rooms (one per OBS ID) --------------------------------------------------------

type client struct{ ch chan string }

type hub struct {
	mu    sync.Mutex
	rooms map[string]map[*client]struct{}
}

func newHub() *hub { return &hub{rooms: map[string]map[*client]struct{}{}} }

func (h *hub) add(id string, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms[id] == nil {
		h.rooms[id] = map[*client]struct{}{}
	}
	h.rooms[id][c] = struct{}{}
}

func (h *hub) remove(id string, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.rooms[id], c)
	if len(h.rooms[id]) == 0 {
		delete(h.rooms, id)
	}
}

func (h *hub) count(id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms[id])
}

// broadcast sends a line to every listener of an ID and returns how many there were.
func (h *hub) broadcast(id, line string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for c := range h.rooms[id] {
		n++
		select {
		case c.ch <- line:
		default: // a stuck listener must not block the others
		}
	}
	return n
}

func (h *hub) ping() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, set := range h.rooms {
		for c := range set {
			select {
			case c.ch <- ": ping\n\n":
			default:
			}
		}
	}
}

// ---- helpers -----------------------------------------------------------------------

func cors(w http.ResponseWriter) {
	hd := w.Header()
	hd.Set("Access-Control-Allow-Origin", "*")
	hd.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	hd.Set("Access-Control-Allow-Headers", "Content-Type")
	hd.Set("Access-Control-Allow-Private-Network", "true")
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	cors(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]interface{}{"ok": false, "error": msg})
}

// jsLen counts UTF-16 units, so limits match what the browser-side code measures.
func jsLen(s string) int { return len(utf16.Encode([]rune(s))) }

func str(v interface{}, max int) (string, bool) {
	s, ok := v.(string)
	if !ok || s == "" || jsLen(s) > max {
		return "", false
	}
	return s, true
}

func cut(s string, max int) string {
	u := utf16.Encode([]rune(s))
	if len(u) > max {
		return string(utf16.Decode(u[:max]))
	}
	return s
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- payload -----------------------------------------------------------------------

type payload struct {
	V        int      `json:"v"`
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Voice    string   `json:"voice"`
	Endpoint string   `json:"endpoint"`
	Model    string   `json:"model"`
	Format   string   `json:"format"`
	User     string   `json:"user"`
	Command  string   `json:"command"`
	Platform string   `json:"platform"`
	TS       int64    `json:"ts"`
	Speed    *float64 `json:"speed,omitempty"`
	// Optional extras (protocol 2). Intro is a short first clip in its own voice ("Paul says"),
	// played before the message. Kind "chat" marks a line read from the chat (not a command), and
	// MaxQueue is how many such lines may wait; the oldest waiting chat line is dropped first.
	Intro    *intro `json:"intro,omitempty"`
	Kind     string `json:"kind,omitempty"`
	MaxQueue int    `json:"maxQueue,omitempty"`
}

type intro struct {
	Text  string   `json:"text"`
	Voice string   `json:"voice"`
	Model string   `json:"model,omitempty"`
	Speed *float64 `json:"speed,omitempty"`
}

// protocolVersion tells the extension which features this relay understands. The extension
// needs 2 or higher (intro clip, chat lines).
const protocolVersion = 2

func validate(p map[string]interface{}) *payload {
	if p == nil {
		return nil
	}
	text, ok1 := str(p["text"], 2000)
	voice, ok2 := str(p["voice"], 200)
	endpoint, ok3 := str(p["endpoint"], 500)
	if !ok1 || !ok2 || !ok3 || !isHTTPURL(endpoint) {
		return nil
	}
	out := &payload{V: 1, Text: text, Voice: voice, Endpoint: endpoint, Model: "tts-1-hd", Format: "wav", TS: time.Now().UnixMilli()}
	if id, ok := str(p["id"], 64); ok {
		out.ID = id
	} else {
		out.ID = strconv.FormatInt(out.TS, 10) + randomHex(3)
	}
	if m, ok := str(p["model"], 100); ok {
		out.Model = m
	}
	if f, ok := p["format"].(string); ok && formatRe.MatchString(f) {
		out.Format = f
	}
	if s, ok := p["user"].(string); ok {
		out.User = cut(s, 100)
	}
	if s, ok := p["command"].(string); ok {
		out.Command = cut(s, 50)
	}
	if s, ok := p["platform"].(string); ok {
		out.Platform = cut(s, 30)
	}
	if sp, ok := p["speed"].(float64); ok && sp >= 0.1 && sp <= 4 {
		out.Speed = &sp
	}
	if raw, present := p["intro"]; present && raw != nil {
		m, isMap := raw.(map[string]interface{})
		if !isMap {
			return nil
		}
		it, ok1 := str(m["text"], 300)
		iv, ok2 := str(m["voice"], 200)
		if !ok1 || !ok2 {
			return nil
		}
		in := &intro{Text: it, Voice: iv}
		if im, ok := str(m["model"], 100); ok {
			in.Model = im
		}
		if sp, ok := m["speed"].(float64); ok && sp >= 0.1 && sp <= 4 {
			in.Speed = &sp
		}
		out.Intro = in
	}
	if k, ok := p["kind"].(string); ok && k == "chat" {
		out.Kind = "chat"
		if mq, ok := p["maxQueue"].(float64); ok && mq >= 1 && mq <= 100 {
			out.MaxQueue = int(mq)
		}
	}
	return out
}

func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("too large")
	}
	return b, nil
}

// ---- server ------------------------------------------------------------------------

type relay struct {
	hub     *hub
	quit    chan struct{}
	once    sync.Once
	http    *http.Client
	eng     *engine
	voices  voiceStore
	ffmpeg  string // path of ffmpeg, "" if not installed
	drafts  string // recordings waiting for the user's review
	fmtOnce sync.Once
	fmts    []string
}

func newRelay(eng *engine, vs voiceStore) *relay {
	return &relay{
		eng:    eng,
		voices: vs,
		hub:    newHub(),
		quit:   make(chan struct{}),
		// No system proxy: VoiceStudio is on this PC or in the same network.
		http: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: nil}},
	}
}

func (r *relay) requestQuit() { r.once.Do(func() { close(r.quit) }) }

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (r *relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	id := req.URL.Query().Get("id")
	path := req.URL.Path

	if req.Method == http.MethodOptions && strings.HasPrefix(path, "/v1/") {
		if guard(w, req) {
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	if req.Method == http.MethodOptions {
		cors(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch {
	case req.Method == http.MethodGet && (path == "/overlay.html" || path == "/overlay.js"):
		cors(w)
		if path == "/overlay.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(overlayHTML)
		} else {
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(overlayJS)
		}

	case strings.HasPrefix(path, "/v1/"):
		r.v1(w, req)

	case path == "/" || strings.HasPrefix(path, "/api/"):
		r.panel(w, req)

	case req.Method == http.MethodGet && path == "/info":
		st := r.eng.status()
		writeJSON(w, 200, map[string]interface{}{"ok": true, "version": version, "protocol": protocolVersion, "updateCheck": false,
			"latest": "", "updateAvailable": false, "downloadUrl": "", "engine": st.State})

	case req.Method == http.MethodPost && path == "/open-folder":
		// Opens the folder this program runs from. Same protection as /shutdown: this computer only,
		// and a custom header that a web page cannot send.
		if !isLoopback(req.RemoteAddr) || req.Header.Get("X-Relay-Open") != "1" {
			fail(w, 403, "forbidden")
			return
		}
		if err := openPath(appDir()); err != nil {
			fail(w, 500, "could not open the folder")
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "path": appDir()})

	case req.Method == http.MethodGet && path == "/status":
		if !idRe.MatchString(id) {
			fail(w, 400, "bad id")
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true, "listeners": r.hub.count(id)})

	case req.Method == http.MethodGet && path == "/events":
		r.events(w, req, id)

	case req.Method == http.MethodPost && path == "/tts":
		r.proxy(w, req, id)

	case req.Method == http.MethodPost && path == "/send":
		r.send(w, req, id)

	case req.Method == http.MethodPost && path == "/shutdown":
		// Only from this computer, and only with a custom header (a web page cannot add one
		// without a CORS preflight, which this server refuses for that header).
		if !isLoopback(req.RemoteAddr) || req.Header.Get("X-Relay-Stop") != "1" {
			fail(w, 403, "forbidden")
			return
		}
		writeJSON(w, 200, map[string]interface{}{"ok": true})
		go func() { time.Sleep(150 * time.Millisecond); r.requestQuit() }()

	default:
		fail(w, 404, "not found")
	}
}

func (r *relay) events(w http.ResponseWriter, req *http.Request, id string) {
	if !idRe.MatchString(id) {
		fail(w, 400, "bad id")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "streaming unsupported")
		return
	}
	cors(w)
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream; charset=utf-8")
	hd.Set("Cache-Control", "no-cache, no-transform")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	io.WriteString(w, "retry: 2000\n\n")
	fl.Flush()
	c := &client{ch: make(chan string, 64)}
	r.hub.add(id, c)
	defer r.hub.remove(id, c)
	for {
		select {
		case line := <-c.ch:
			if _, err := io.WriteString(w, line); err != nil {
				return
			}
			fl.Flush()
		case <-req.Context().Done():
			return
		case <-r.quit:
			return
		}
	}
}

func (r *relay) send(w http.ResponseWriter, req *http.Request, id string) {
	if !idRe.MatchString(id) {
		fail(w, 400, "bad id")
		return
	}
	body, err := readBody(req)
	if err != nil {
		fail(w, 413, "bad body")
		return
	}
	var parsed map[string]interface{}
	if json.Unmarshal(body, &parsed) != nil {
		fail(w, 400, "invalid json")
		return
	}
	p := validate(parsed)
	if p == nil {
		fail(w, 400, "invalid payload")
		return
	}
	data, _ := json.Marshal(p)
	n := r.hub.broadcast(id, "data: "+string(data)+"\n\n")
	cmd := "message"
	if p.Command != "" {
		cmd = "!" + p.Command
	}
	suffix := "s"
	if n == 1 {
		suffix = ""
	}
	logf("%s... %s -> voice \"%s\" (%d OBS listener%s)", id[:4], cmd, p.Voice, n, suffix)
	writeJSON(w, 200, map[string]interface{}{"ok": true, "listeners": n})
}

// proxy asks VoiceStudio for speech on behalf of the OBS page or the extension's Test buttons.
// VoiceStudio refuses requests that come from a web page; this program is not one.
func (r *relay) proxy(w http.ResponseWriter, req *http.Request, id string) {
	if !idRe.MatchString(id) {
		fail(w, 400, "bad id")
		return
	}
	raw, err := readBody(req)
	if err != nil {
		fail(w, 413, "bad body")
		return
	}
	var p struct {
		Endpoint string                 `json:"endpoint"`
		Body     map[string]interface{} `json:"body"`
	}
	if json.Unmarshal(raw, &p) != nil {
		fail(w, 400, "invalid json")
		return
	}
	target, err := url.Parse(p.Endpoint)
	if err != nil || target.Host == "" {
		fail(w, 400, "invalid endpoint")
		return
	}
	if (target.Scheme != "http" && target.Scheme != "https") || !strings.HasSuffix(target.Path, "/audio/speech") || p.Body == nil {
		fail(w, 400, "invalid request")
		return
	}
	data, _ := json.Marshal(p.Body)
	ctx, cancel := context.WithTimeout(req.Context(), 60*time.Second)
	defer cancel()
	up, _ := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), strings.NewReader(string(data)))
	up.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(up)
	if err != nil {
		why := err.Error()
		if ne, ok := err.(*url.Error); ok {
			why = ne.Err.Error()
		}
		low := strings.ToLower(why)
		switch {
		case strings.Contains(low, "refused"):
			why = "ECONNREFUSED"
		case strings.Contains(low, "no such host") || strings.Contains(low, "no address associated"):
			why = "ENOTFOUND"
		case strings.Contains(low, "deadline exceeded") || strings.Contains(low, "timeout") || strings.Contains(low, "timed out"):
			why = "ETIMEDOUT"
		}
		msg := "Could not reach VoiceStudio at " + target.Host + " (" + why + ")"
		logf("%s", msg)
		fail(w, 502, msg)
		return
	}
	defer resp.Body.Close()
	cors(w)
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
