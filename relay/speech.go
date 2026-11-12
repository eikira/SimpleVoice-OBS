// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

//go:embed panel.html
var panelHTML []byte

const (
	maxSpeechChars = 4000
	maxUpload      = 100 << 20
)

// hostOK refuses requests whose Host header is a name (other than localhost). That stops a web
// page from reaching this program through a DNS name that points to 127.0.0.1.
func hostOK(h string) bool {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	return h == "localhost" || net.ParseIP(h) != nil
}

// originOK lets through requests without an Origin (OBS, curl, the relay itself), from the
// same address (the control panel) and from browser extensions. Other web pages are refused.
func originOK(req *http.Request) bool {
	o := req.Header.Get("Origin")
	if o == "" || strings.HasPrefix(o, "chrome-extension://") {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == req.Host
}

// guard checks host and origin and adds the CORS headers for an allowed browser origin.
func guard(w http.ResponseWriter, req *http.Request) bool {
	if !hostOK(req.Host) || !originOK(req) {
		fail(w, 403, "forbidden")
		return false
	}
	if o := req.Header.Get("Origin"); o != "" {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", o)
		h.Set("Vary", "Origin")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
	}
	return true
}

// guardPanel is for actions that change things: this computer only, and a header that a web
// page cannot send without a preflight (which is refused).
func guardPanel(w http.ResponseWriter, req *http.Request) bool {
	if !hostOK(req.Host) || !isLoopback(req.RemoteAddr) || req.Header.Get("X-Relay-Panel") != "1" || req.Header.Get("Origin") != "" && !originOK(req) {
		fail(w, 403, "forbidden")
		return false
	}
	return true
}

// languages the graphics card engine can be told about ("" = let the model find out).
var languages = []struct{ Code, Name string }{
	{"", "Automatic"}, {"en", "English"}, {"de", "German"}, {"fr", "French"}, {"es", "Spanish"}, {"it", "Italian"},
	{"pt", "Portuguese"}, {"nl", "Dutch"}, {"pl", "Polish"}, {"ru", "Russian"}, {"tr", "Turkish"},
	{"ja", "Japanese"}, {"ko", "Korean"}, {"zh", "Chinese"},
}

// validLanguage returns the code if it is one of the offered languages, otherwise "" (automatic).
func validLanguage(code string) string {
	for _, l := range languages {
		if l.Code != "" && l.Code == code {
			return code
		}
	}
	return ""
}

// minCloneSpeed is the slowest speed for imported voices (made from a recording). Below it the model
// can repeat parts of the recording inside the speech (seen at 0.7, not at 0.8). The 13 sample voices
// that Setup makes are not limited.
const minCloneSpeed = 0.8

var sampleVoiceNames = []string{"Heart", "Bella", "Nicole", "Aoede", "Kore", "Sarah", "Michael", "Fenrir", "Puck", "Emma", "Isabella", "George", "Fable"}

// isSampleVoice tells by name if a voice file is one of the sample voices. A sample voice that was
// renamed counts as an imported one.
func isSampleVoice(wavPath string) bool {
	n := strings.TrimSuffix(filepath.Base(wavPath), filepath.Ext(wavPath))
	for _, s := range sampleVoiceNames {
		if strings.EqualFold(s, n) {
			return true
		}
	}
	return false
}

// minBuiltinSpeed is the slowest speed of the built-in and sample voices (the range of the extension and the panel).
const minBuiltinSpeed = 0.5

// minSpeedOf is the slowest speed a voice can take, reported to the extension in the voice list. It is
// the same rule speech() applies: imported voices (not sample voices, only on the graphics card engine)
// cannot go below minCloneSpeed; built-in voices and sample voices go down to minBuiltinSpeed.
func (r *relay) minSpeedOf(name string) float64 {
	if r.eng.kindNow() == "cpu" || isSampleVoice(name) {
		return minBuiltinSpeed
	}
	return minCloneSpeed
}

// endingMark is added to the end of a text that ends in a letter or a digit, on the graphics card engine only.
// Without any ending the model tends to swallow the last word ("ever" came out as "e..."). A comma was chosen
// over a full stop because it does not end the sentence. One place to change it.
const endingMark = ","

func withEnding(text string) string {
	rs := []rune(strings.TrimRight(text, " \t\r\n"))
	if len(rs) == 0 {
		return text
	}
	if last := rs[len(rs)-1]; unicode.IsLetter(last) || unicode.IsDigit(last) {
		return string(rs) + endingMark
	}
	return text
}

func clampSpeed(s float64) float64 {
	if math.IsNaN(s) || s == 0 {
		return 1
	}
	return math.Round(math.Min(1.5, math.Max(0.5, s))*10) / 10
}

func (r *relay) v1(w http.ResponseWriter, req *http.Request) {
	if !guard(w, req) {
		return
	}
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/v1/models":
		writeJSONNoCORS(w, 200, map[string]interface{}{"object": "list", "data": []map[string]string{{"id": "omnivoice", "object": "model"}}})
	case req.Method == http.MethodGet && req.URL.Path == "/v1/audio/voices":
		vs := []map[string]interface{}{}
		for _, n := range r.speakable() {
			vs = append(vs, map[string]interface{}{"id": n, "name": n, "min_speed": r.minSpeedOf(n)})
		}
		writeJSONNoCORS(w, 200, map[string]interface{}{"voices": vs})
	case req.Method == http.MethodPost && req.URL.Path == "/v1/audio/speech":
		r.speech(w, req)
	default:
		fail(w, 404, "not found")
	}
}

// writeJSONNoCORS is writeJSON without the open CORS headers that the rest of the relay uses.
func writeJSONNoCORS(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func speechError(w http.ResponseWriter, status int, msg string) {
	writeJSONNoCORS(w, status, map[string]interface{}{"ok": false, "error": msg})
}

// saveFormats is asked once: which formats this ffmpeg can make.
func (r *relay) saveFormats() []string {
	r.fmtOnce.Do(func() { r.fmts = savable(context.Background(), r.ffmpeg) })
	return r.fmts
}

// speakable lists the voices that work with the engine that runs now: your own voices on the
// graphics card engine, the built-in voices on the processor engine.
func (r *relay) speakable() []string {
	out := []string{}
	if r.eng.kindNow() == "cpu" {
		return append(out, r.eng.builtinVoices()...)
	}
	for _, v := range r.voices.list() {
		if v.Usable {
			out = append(out, v.Name)
		}
	}
	return out
}

// pickVoice finds what the engine needs for a voice name.
func (r *relay) pickVoice(name string) (wav, ref, builtin string, err error) {
	if r.eng.kindNow() == "cpu" {
		for _, n := range r.eng.builtinVoices() {
			if strings.EqualFold(n, name) {
				return "", "", n, nil
			}
		}
		if _, _, e := r.voices.find(name); e == nil {
			return "", "", "", errors.New("The voice is one of your cloned voices. Cloned voices need the graphics card engine; the processor engine is running. Switch the engine in the control panel or pick a built-in voice: " + strings.Join(r.eng.builtinVoices(), ", "))
		}
		return "", "", "", errNoVoice
	}
	wav, ref, err = r.voices.find(name)
	return wav, ref, "", err
}

func (r *relay) speech(w http.ResponseWriter, req *http.Request) {
	if !strings.HasPrefix(strings.ToLower(req.Header.Get("Content-Type")), "application/json") {
		speechError(w, 415, "Content-Type must be application/json")
		return
	}
	var p struct {
		Input string   `json:"input"`
		Voice string   `json:"voice"`
		Speed *float64 `json:"speed"`
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 64<<10))
	if err != nil || json.Unmarshal(body, &p) != nil {
		speechError(w, 400, "invalid json")
		return
	}
	text := strings.TrimSpace(p.Input)
	if text == "" || len([]rune(text)) > maxSpeechChars {
		speechError(w, 400, "input must be 1 to 4000 characters")
		return
	}
	wav, ref, builtin, err := r.pickVoice(p.Voice)
	if err != nil {
		if errors.Is(err, errNoVoice) {
			speechError(w, 404, "Voice \""+p.Voice+"\" does not exist. Add it in the control panel.")
		} else {
			speechError(w, 400, "Voice \""+p.Voice+"\": "+err.Error())
		}
		return
	}
	speed := 1.0
	if p.Speed != nil {
		speed = clampSpeed(*p.Speed)
	}
	if wav != "" && !isSampleVoice(wav) && speed < minCloneSpeed { // imported recordings: slower than this lets the sample leak into the speech
		speed = minCloneSpeed
	}
	if wav != "" { // graphics card engine
		text = withEnding(text)
	}
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Minute)
	defer cancel()
	began := time.Now()
	audio, err := r.eng.speak(ctx, text, builtin, wav, ref, speed)
	if err != nil {
		var ee errEngine
		code := 500
		if errors.As(err, &ee) {
			code = 503
		}
		logf("speech failed: %v", err)
		speechError(w, code, err.Error())
		return
	}
	logf("voice \"%s\": %d characters in %.2f s", p.Voice, len([]rune(text)), time.Since(began).Seconds())
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(audio)
}

// ---- control panel ------------------------------------------------------------------

func (r *relay) panel(w http.ResponseWriter, req *http.Request) {
	if !hostOK(req.Host) {
		fail(w, 403, "forbidden")
		return
	}
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Language", "en")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(panelHTML)
	case req.Method == http.MethodGet && req.URL.Path == "/api/status":
		_, port, _ := net.SplitHostPort(req.Host)
		lang, numbers := r.eng.getOptions()
		writeJSONNoCORS(w, 200, map[string]interface{}{"version": version, "port": port, "engine": r.eng.status(),
			"voices": r.voices.list(), "voicesDir": r.voices.dir, "speakable": r.speakable(), "samples": sampleVoiceNames, "builtin": r.eng.builtinVoices(), "steps": r.eng.getSteps(),
			"language": lang, "numbers": numbers, "languages": languages, "formats": r.saveFormats()})
	case req.Method == http.MethodPost && req.URL.Path == "/api/voices/draft":
		r.newDraft(w, req)
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/voices/draft/"):
		id := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/api/voices/draft/"), ".wav")
		if !draftRe.MatchString(id) {
			fail(w, 404, "not found")
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, req, r.draftPath(id))
	case req.Method == http.MethodPost && req.URL.Path == "/api/voices":
		// Saves a reviewed draft as a voice: the name, and the transcript as the user corrected it.
		var p struct{ Draft, Name, Transcript string }
		if !guardPanel(w, req) {
			return
		}
		if json.NewDecoder(io.LimitReader(req.Body, 16<<10)).Decode(&p) != nil || !draftRe.MatchString(p.Draft) {
			speechError(w, 400, "bad request")
			return
		}
		wav, err := os.ReadFile(r.draftPath(p.Draft))
		if err != nil {
			speechError(w, 404, "The recording expired. Import it again.")
			return
		}
		if err := r.voices.add(strings.TrimSpace(p.Name), p.Transcript, wav); err != nil {
			speechError(w, 400, err.Error())
			return
		}
		os.Remove(r.draftPath(p.Draft))
		logf("voice added: %s", p.Name)
		writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true})
	case req.Method == http.MethodPost && req.URL.Path == "/api/voices/draft/discard":
		var p struct{ Draft string }
		if !guardPanel(w, req) {
			return
		}
		if json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&p) == nil && draftRe.MatchString(p.Draft) {
			os.Remove(r.draftPath(p.Draft))
		}
		writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true})
	case req.Method == http.MethodPost && req.URL.Path == "/api/voices/transcribe":
		// Listens to a voice that is already in the folder (for example one copied in by hand).
		var p struct{ Name string }
		if !guardPanel(w, req) {
			return
		}
		wav := ""
		if json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&p) == nil {
			wav = r.voices.wavFor(p.Name)
		}
		if wav == "" {
			speechError(w, 404, "No such voice.")
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), 3*time.Minute)
		defer cancel()
		text, err := r.eng.transcribe(ctx, wav)
		if err != nil {
			speechError(w, 500, err.Error())
			return
		}
		writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true, "transcript": text})
	case req.Method == http.MethodPost && req.URL.Path == "/api/voices/transcript":
		var p struct{ Name, Transcript string }
		if !guardPanel(w, req) {
			return
		}
		if json.NewDecoder(io.LimitReader(req.Body, 16<<10)).Decode(&p) != nil {
			speechError(w, 400, "bad request")
			return
		}
		if err := r.voices.setTranscript(p.Name, p.Transcript); err != nil {
			speechError(w, 400, err.Error())
			return
		}
		writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true})
	case req.Method == http.MethodPost && req.URL.Path == "/api/voices/delete":
		if !guardPanel(w, req) {
			return
		}
		var p struct{ Name string }
		if json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&p) != nil || r.voices.remove(p.Name) != nil {
			speechError(w, 404, "No such voice.")
			return
		}
		writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true})
	case req.Method == http.MethodPost && req.URL.Path == "/api/voices/rename":
		if !guardPanel(w, req) {
			return
		}
		var p struct{ Name, NewName string }
		if json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&p) != nil {
			speechError(w, 400, "Bad request.")
			return
		}
		if err := r.voices.rename(p.Name, strings.TrimSpace(p.NewName)); err != nil {
			code := 400
			if errors.Is(err, errNoVoice) {
				code = 404
			}
			speechError(w, code, err.Error())
			return
		}
		writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true})
	case req.Method == http.MethodPost && req.URL.Path == "/api/engine/mode":
		var p struct{ Mode string }
		if !guardPanel(w, req) {
			return
		}
		if json.NewDecoder(io.LimitReader(req.Body, 1024)).Decode(&p) != nil || !r.eng.setMode(p.Mode) {
			speechError(w, 400, "Choose automatic, graphics card or processor.")
			return
		}
		if err := writeSetting("ENGINE", p.Mode); err != nil {
			logf("could not save the engine choice: %v", err)
		}
		r.eng.addLog("Engine choice: " + p.Mode)
		r.eng.stop()
		time.Sleep(300 * time.Millisecond)
		r.eng.start()
		writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true})
	case req.Method == http.MethodPost && req.URL.Path == "/api/engine/quality":
		var p struct{ Steps int }
		if !guardPanel(w, req) {
			return
		}
		if json.NewDecoder(io.LimitReader(req.Body, 1024)).Decode(&p) != nil || (p.Steps != 16 && p.Steps != 32 && p.Steps != 48) {
			speechError(w, 400, "Choose fast, normal or high quality.")
			return
		}
		r.eng.setSteps(p.Steps)
		if err := writeSetting("STEPS", strconv.Itoa(p.Steps)); err != nil {
			logf("could not save the quality: %v", err)
		}
		r.eng.addLog("Voice quality: " + strconv.Itoa(p.Steps) + " steps.")
		writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true})
	case req.Method == http.MethodPost && req.URL.Path == "/api/engine/options":
		var p struct {
			Language string
			Numbers  bool
		}
		if !guardPanel(w, req) {
			return
		}
		if json.NewDecoder(io.LimitReader(req.Body, 1024)).Decode(&p) != nil || (p.Language != "" && validLanguage(p.Language) == "") {
			speechError(w, 400, "Choose one of the listed languages.")
			return
		}
		r.eng.setOptions(p.Language, p.Numbers)
		lang := p.Language
		if lang == "" {
			lang = "auto"
		}
		num := "off"
		if p.Numbers {
			num = "on"
		}
		for k, v := range map[string]string{"LANGUAGE": lang, "NUMBERS": num} {
			if err := writeSetting(k, v); err != nil {
				logf("could not save %s: %v", k, err)
			}
		}
		r.eng.addLog("Voice options: language " + lang + ", numbers as words " + num + ".")
		writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true})
	case req.Method == http.MethodPost && req.URL.Path == "/api/convert":
		if !guardPanel(w, req) {
			return
		}
		data, err := io.ReadAll(io.LimitReader(req.Body, maxWavBytes+1))
		if err != nil || len(data) > maxWavBytes {
			speechError(w, 400, "The audio is too large.")
			return
		}
		out, err := convertSpeech(req.Context(), r.ffmpeg, data, req.URL.Query().Get("format"))
		if err != nil {
			speechError(w, 400, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(out)
	case req.Method == http.MethodPost && req.URL.Path == "/api/engine/restart":
		if !guardPanel(w, req) {
			return
		}
		r.eng.addLog("Restart requested.")
		r.eng.stop()
		time.Sleep(300 * time.Millisecond)
		r.eng.start()
		writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true})
	case req.Method == http.MethodPost && req.URL.Path == "/api/open-voices":
		if !guardPanel(w, req) {
			return
		}
		if err := openPath(r.voices.dir); err != nil {
			speechError(w, 500, "Could not open the folder.")
			return
		}
		writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true})
	default:
		fail(w, 404, "not found")
	}
}

var draftRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (r *relay) draftPath(id string) string { return filepath.Join(r.drafts, id+".wav") }

// newDraft takes a recording in any common format, converts it to WAV and lets the engine write
// down what is said. Nothing becomes a voice until the user has read the text and saved it.
func (r *relay) newDraft(w http.ResponseWriter, req *http.Request) {
	if !guardPanel(w, req) {
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxUpload+(1<<20))
	if err := req.ParseMultipartForm(8 << 20); err != nil {
		speechError(w, 400, "The upload failed or is larger than 100 MB.")
		return
	}
	f, _, err := req.FormFile("audio")
	if err != nil {
		speechError(w, 400, "Choose an audio file.")
		return
	}
	defer f.Close()
	head := make([]byte, 12)
	if n, _ := io.ReadFull(f, head); n < 12 || !sniffAudio(head) {
		speechError(w, 400, "This is not an audio file. Use WAV, FLAC, MP3, AAC, M4A or OGG.")
		return
	}
	f.Seek(0, io.SeekStart)
	os.MkdirAll(r.drafts, 0o755)
	r.cleanDrafts()
	in, err := os.CreateTemp(r.drafts, "in-*")
	if err != nil {
		speechError(w, 500, "Could not save the upload.")
		return
	}
	defer os.Remove(in.Name())
	_, err = io.Copy(in, f)
	in.Close()
	if err != nil {
		speechError(w, 400, "The upload failed.")
		return
	}
	id := randomHex(8)
	if err := toWAV(req.Context(), r.ffmpeg, in.Name(), r.draftPath(id)); err != nil {
		os.Remove(r.draftPath(id))
		speechError(w, 400, err.Error())
		return
	}
	data, _ := os.ReadFile(r.draftPath(id))
	secs, err := wavSeconds(data)
	if err != nil || secs < 1 {
		os.Remove(r.draftPath(id))
		speechError(w, 400, "The recording is too short. Use at least 3 seconds of speech.")
		return
	}
	if secs > maxSecs+0.5 {
		os.Remove(r.draftPath(id))
		speechError(w, 400, fmt.Sprintf("This recording is longer than %d seconds. Use a shorter one: 5 to %d seconds of one clear speaker.", maxSecs, maxSecs))
		return
	}
	out := map[string]interface{}{"ok": true, "draft": id, "seconds": secs}
	ctx, cancel := context.WithTimeout(req.Context(), 3*time.Minute)
	defer cancel()
	text, err := r.eng.transcribe(ctx, r.draftPath(id))
	if err != nil {
		out["note"] = "The words could not be written down automatically (" + err.Error() + "). Type them in."
	}
	out["transcript"] = strings.TrimSpace(text)
	writeJSONNoCORS(w, 200, out)
}

// cleanDrafts removes recordings that were imported but never saved.
func (r *relay) cleanDrafts() {
	entries, _ := os.ReadDir(r.drafts)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 2*time.Hour {
			os.Remove(filepath.Join(r.drafts, e.Name()))
		}
	}
}
