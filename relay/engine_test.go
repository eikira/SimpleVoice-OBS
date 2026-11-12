//go:build !windows

// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// These tests run the real worker.py with its fake backend (OMNIVOICE_FAKE=1), so the program
// start-up, the supervisor, the HTTP calls and the WAV handling are all real. Only the model is not.

func makeWAV(secs float64) []byte {
	n := int(16000 * secs)
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+n*2))
	b.WriteString("WAVEfmt ")
	for _, v := range []interface{}{uint32(16), uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)} {
		binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(n*2))
	b.Write(make([]byte, n*2))
	return b.Bytes()
}

type env struct {
	srv    *httptest.Server
	r      *relay
	voices string
}

func newEnv(t *testing.T) *env { return newEnvMode(t, "auto") }

func newEnvMode(t *testing.T, mode string) *env {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	t.Setenv("OMNIVOICE_FAKE", "1")
	dir := t.TempDir()
	model := filepath.Join(dir, "model")
	os.MkdirAll(model, 0o755)
	vdir := filepath.Join(dir, "voices")
	os.MkdirAll(vdir, 0o755)
	script := filepath.Join(dir, "worker.py")
	os.WriteFile(script, workerPy, 0o644)
	kok := filepath.Join(dir, "kokoro")
	os.MkdirAll(kok, 0o755)
	eng := newEngine(engineCfg{python: py, model: model, kokoro: kok, script: script, steps: 16, startWait: 20 * time.Second})
	eng.setMode(mode)
	r := newRelay(eng, voiceStore{dir: vdir})
	r.ffmpeg = findFFmpeg(dir, "")
	r.drafts = filepath.Join(dir, "drafts")
	eng.start()
	srv := httptest.NewServer(r)
	t.Cleanup(func() { srv.Close(); eng.stop() })
	return &env{srv, r, vdir}
}

func (e *env) addVoice(t *testing.T, name string) {
	t.Helper()
	if err := e.r.voices.add(name, "Some words that were spoken.", makeWAV(4)); err != nil {
		t.Fatal(err)
	}
}

func (e *env) waitReady(t *testing.T) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if e.r.eng.status().State == "ready" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("engine not ready: %+v", e.r.eng.status())
}

func (e *env) speak(t *testing.T, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/audio/speech", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestSpeechEndToEndAndSpeed(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "Anna")
	e.waitReady(t)
	secs := func(speed string) float64 {
		resp, b := e.speak(t, `{"model":"tts-1-hd","input":"This is a sentence of some length.","voice":"anna","response_format":"wav"`+speed+`}`, nil)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" {
			t.Fatalf("status %d %s", resp.StatusCode, b)
		}
		s, err := wavSeconds(b)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	normal, fast, slow := secs(""), secs(`,"speed":1.5`), secs(`,"speed":0.5`)
	if !(fast < normal && normal < slow) {
		t.Fatalf("speed has no effect: slow %.2f normal %.2f fast %.2f", slow, normal, fast)
	}
	if got := secs(`,"speed":9`); got != fast {
		t.Fatalf("speed above 1.5 must be limited to 1.5: %.2f vs %.2f", got, fast)
	}
	if got := secs(`,"speed":1.04`); got != normal {
		t.Fatalf("speed is rounded to 0.1 steps: %.2f vs %.2f", got, normal)
	}
}

func TestVoicesModelsInfo(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "Anna")
	os.WriteFile(filepath.Join(e.voices, "NoText.wav"), makeWAV(4), 0o644) // dropped in by hand, no transcript
	get := func(p string) map[string]interface{} {
		resp, err := http.Get(e.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&m)
		return m
	}
	vs := get("/v1/audio/voices")["voices"].([]interface{})
	if len(vs) != 1 || vs[0].(map[string]interface{})["id"] != "Anna" || vs[0].(map[string]interface{})["name"] != "Anna" {
		t.Fatalf("voices: %v", vs)
	}
	if d := get("/v1/models")["data"].([]interface{}); len(d) != 1 || d[0].(map[string]interface{})["id"] == "" {
		t.Fatalf("models: %v", d)
	}
	info := get("/info")
	if info["protocol"].(float64) != 2 || info["ok"] != true {
		t.Fatalf("info: %v", info)
	}
	resp, b := e.speak(t, `{"input":"hi","voice":"NoText"}`, nil)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "NoText.txt") {
		t.Fatalf("voice without transcript: %d %s", resp.StatusCode, b)
	}
}

func TestSpeechRefusals(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "Anna")
	e.waitReady(t)
	cases := []struct {
		name string
		body string
		hdr  map[string]string
		want int
	}{
		{"unknown voice", `{"input":"hi","voice":"Nobody"}`, nil, 404},
		{"path as voice", `{"input":"hi","voice":"../Anna"}`, nil, 404},
		{"empty text", `{"input":"  ","voice":"Anna"}`, nil, 400},
		{"too long", `{"input":"` + strings.Repeat("a", 4001) + `","voice":"Anna"}`, nil, 400},
		{"bad json", `{`, nil, 400},
		{"not json content type", `{"input":"hi","voice":"Anna"}`, map[string]string{"Content-Type": "text/plain"}, 415},
		{"web page origin", `{"input":"hi","voice":"Anna"}`, map[string]string{"Origin": "https://evil.example"}, 403},
		{"dns rebinding host", `{"input":"hi","voice":"Anna"}`, map[string]string{"Host": "evil.example:8787"}, 403},
	}
	for _, c := range cases {
		if resp, b := e.speak(t, c.body, c.hdr); resp.StatusCode != c.want {
			t.Errorf("%s: got %d (%s), want %d", c.name, resp.StatusCode, b, c.want)
		}
	}
	// the extension may call it, and the browser's preflight is answered for it only
	resp, _ := e.speak(t, `{"input":"hi","voice":"Anna"}`, map[string]string{"Origin": "chrome-extension://abc"})
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "chrome-extension://abc" {
		t.Errorf("extension origin: %d %v", resp.StatusCode, resp.Header)
	}
	for origin, want := range map[string]int{"chrome-extension://abc": 204, "https://evil.example": 403} {
		req, _ := http.NewRequest("OPTIONS", e.srv.URL+"/v1/audio/speech", nil)
		req.Header.Set("Origin", origin)
		r2, _ := http.DefaultClient.Do(req)
		if r2.StatusCode != want {
			t.Errorf("preflight from %s: %d", origin, r2.StatusCode)
		}
	}
}

func TestExtensionProxyPath(t *testing.T) {
	// The extension's test buttons and the OBS page can go through /tts to the relay itself.
	e := newEnv(t)
	e.addVoice(t, "Anna")
	e.waitReady(t)
	body, _ := json.Marshal(map[string]interface{}{"endpoint": e.srv.URL + "/v1/audio/speech",
		"body": map[string]interface{}{"model": "tts-1-hd", "input": "hello there", "voice": "Anna", "response_format": "wav"}})
	resp, err := http.Post(e.srv.URL+"/tts?id=testid1234567890", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if _, err := wavSeconds(b); resp.StatusCode != 200 || err != nil {
		t.Fatalf("%d %v %s", resp.StatusCode, err, b[:min(80, len(b))])
	}
}

func TestParallelRequestsAreServed(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "Anna")
	e.waitReady(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, b := e.speak(t, `{"input":"two clips at once","voice":"Anna"}`, nil); resp.StatusCode != 200 {
				t.Errorf("%d %s", resp.StatusCode, b)
			}
		}()
	}
	wg.Wait()
}

func TestEngineMissingAndSpeechWaitsForLoading(t *testing.T) {
	dir := t.TempDir()
	eng := newEngine(engineCfg{python: filepath.Join(dir, "nope.exe"), model: dir, script: dir, steps: 16, startWait: time.Second})
	eng.start()
	time.Sleep(200 * time.Millisecond)
	st := eng.status()
	if st.State != "missing" || !strings.Contains(st.Msg, "nope.exe") {
		t.Fatalf("%+v", st)
	}
	r := newRelay(eng, voiceStore{dir: dir})
	os.WriteFile(filepath.Join(dir, "A.wav"), makeWAV(3), 0o644)
	os.WriteFile(filepath.Join(dir, "A.txt"), []byte("words"), 0o644)
	srv := httptest.NewServer(r)
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/v1/audio/speech", "application/json", strings.NewReader(`{"input":"hi","voice":"A"}`))
	if resp.StatusCode != 503 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestEngineRestartsAfterCrashAndStopKillsWorker(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "Anna")
	e.waitReady(t)
	pid := func() int { e.r.eng.mu.Lock(); defer e.r.eng.mu.Unlock(); return e.r.eng.cmd.Process.Pid }
	first := pid()
	syscall.Kill(first, syscall.SIGKILL)
	for i := 0; i < 100 && (pid() == first || e.r.eng.status().State != "ready"); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if pid() == first || e.r.eng.status().State != "ready" {
		t.Fatalf("no restart: %+v", e.r.eng.status())
	}
	if resp, b := e.speak(t, `{"input":"after restart","voice":"Anna"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	second := pid()
	e.r.eng.stop()
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(second, 0); err == nil {
		t.Fatal("worker still running after stop")
	}
}

func TestPanelDeleteVoice(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "Bob")
	del := func(hdr bool) int {
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/voices/delete", strings.NewReader(`{"name":"Bob"}`))
		if hdr {
			req.Header.Set("X-Relay-Panel", "1")
		}
		resp, _ := http.DefaultClient.Do(req)
		return resp.StatusCode
	}
	if del(false) != 403 || del(true) != 200 || del(true) != 404 {
		t.Fatal("delete rules")
	}
	if entries, _ := os.ReadDir(e.voices); len(entries) != 0 {
		t.Fatalf("delete left files: %v", entries)
	}
}

func TestVoiceValidationRefusesBadInput(t *testing.T) {
	e := newEnv(t)
	bad := []struct {
		name, text string
		wav        []byte
	}{{"../evil", "w", makeWAV(4)}, {"CON", "w", makeWAV(4)}, {"Bob", "", makeWAV(4)}, {"Bob", "w", []byte("not a wav at all")}, {"Bob", "w", makeWAV(0.2)}}
	for i, c := range bad {
		if e.r.voices.add(c.name, c.text, c.wav) == nil {
			t.Errorf("bad voice %d accepted", i)
		}
	}
	if entries, _ := os.ReadDir(e.voices); len(entries) != 0 {
		t.Fatalf("refused voices left files: %v", entries)
	}
	e.addVoice(t, "Bob")
	if e.r.voices.add("Bob", "again", makeWAV(4)) == nil {
		t.Fatal("duplicate name accepted")
	}
}

// ---- importing recordings in any format ----

func need(t *testing.T, e *env) {
	t.Helper()
	if e.r.ffmpeg == "" {
		t.Skip("ffmpeg not installed")
	}
}

// makeAudio writes a sine tone in the format of the file name, using ffmpeg.
func makeAudio(t *testing.T, e *env, name string, secs float64, extra ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	args := append([]string{"-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=330:duration=%g", secs)}, extra...)
	if out, err := exec.Command(e.r.ffmpeg, append(args, "-y", p)...).CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg cannot make %s: %s", name, out)
	}
	return p
}

func (e *env) panelPost(t *testing.T, path, body string, hdr bool) (int, map[string]interface{}) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(body))
	if hdr {
		req.Header.Set("X-Relay-Panel", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func (e *env) importFile(t *testing.T, name string, data []byte, hdr bool) (int, map[string]interface{}) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("audio", name)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/voices/draft", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if hdr {
		req.Header.Set("X-Relay-Panel", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func TestImportReviewSaveForEveryFormat(t *testing.T) {
	e := newEnv(t)
	need(t, e)
	e.waitReady(t)
	for _, f := range []struct {
		file string
		args []string
	}{{"a.wav", nil}, {"a.flac", nil}, {"a.mp3", nil}, {"a.aac", []string{"-c:a", "aac"}}, {"a.m4a", []string{"-c:a", "aac"}}, {"a.ogg", nil}} {
		path := makeAudio(t, e, f.file, 4, f.args...)
		data, _ := os.ReadFile(path)
		code, m := e.importFile(t, f.file, data, true)
		if code != 200 || m["ok"] != true {
			t.Fatalf("%s: %d %v", f.file, code, m)
		}
		if m["transcript"] != "This is a fake transcript." || m["seconds"].(float64) < 3.5 || m["seconds"].(float64) > 4.5 {
			t.Fatalf("%s: %v", f.file, m)
		}
		id := m["draft"].(string)
		// the preview is a WAV the browser can play
		resp, _ := http.Get(e.srv.URL + "/api/voices/draft/" + id + ".wav")
		b, _ := io.ReadAll(resp.Body)
		if s, err := wavSeconds(b); err != nil || s < 3.5 || binary.LittleEndian.Uint32(b[24:28]) != 24000 {
			t.Fatalf("%s: preview is not 24 kHz WAV: %v", f.file, err)
		}
		name := "Voice" + strings.TrimSuffix(f.file, filepath.Ext(f.file)) + strings.TrimPrefix(filepath.Ext(f.file), ".")
		body, _ := json.Marshal(map[string]string{"draft": id, "name": name, "transcript": "My corrected words."})
		if code, m := e.panelPost(t, "/api/voices", string(body), true); code != 200 {
			t.Fatalf("%s: save: %d %v", f.file, code, m)
		}
		if txt, _ := os.ReadFile(filepath.Join(e.voices, name+".txt")); strings.TrimSpace(string(txt)) != "My corrected words." {
			t.Fatalf("%s: the corrected text was not saved: %q", f.file, txt)
		}
		if _, err := os.Stat(filepath.Join(e.r.drafts, id+".wav")); err == nil {
			t.Errorf("%s: draft left behind", f.file)
		}
		if resp, b := e.speak(t, `{"input":"hello","voice":"`+name+`"}`, nil); resp.StatusCode != 200 {
			t.Fatalf("%s: cannot speak with the new voice: %d %s", f.file, resp.StatusCode, b)
		}
	}
}

func TestRecordingsOver20SecondsAreRefused(t *testing.T) {
	e := newEnv(t)
	need(t, e)
	e.waitReady(t)
	for _, secs := range []int{30, 100} {
		name := fmt.Sprintf("long%d.flac", secs)
		data, _ := os.ReadFile(makeAudio(t, e, name, float64(secs)))
		code, m := e.importFile(t, name, data, true)
		if code != 400 || !strings.Contains(fmt.Sprint(m["error"]), "longer than 20 seconds") {
			t.Fatalf("a %d second recording must be refused: %d %v", secs, code, m)
		}
	}
	if entries, _ := os.ReadDir(e.r.drafts); len(entries) != 0 {
		t.Fatalf("a refused recording left files behind: %v", entries)
	}
	data, _ := os.ReadFile(makeAudio(t, e, "ok.flac", 19))
	code, m := e.importFile(t, "ok.flac", data, true)
	if s, _ := m["seconds"].(float64); code != 200 || s < 18.5 || s > 19.5 {
		t.Fatalf("a 19 second recording must be accepted whole: %d %v", code, m)
	}
}

// A voice that is already in the folder and is over the limit can not be spoken with any more.
func TestExistingLongVoiceIsMarkedUnusable(t *testing.T) {
	e := newEnv(t)
	need(t, e)
	data, _ := os.ReadFile(makeAudio(t, e, "x.wav", 30))
	os.WriteFile(filepath.Join(e.voices, "Longone.wav"), data, 0o644)
	os.WriteFile(filepath.Join(e.voices, "Longone.txt"), []byte("some words\n"), 0o644)
	for _, v := range e.r.voices.list() {
		if v.Name == "Longone" {
			if v.Usable || !strings.Contains(v.Note, "too long") {
				t.Fatalf("%+v", v)
			}
			return
		}
	}
	t.Fatal("voice not listed")
}

func TestImportRefusals(t *testing.T) {
	e := newEnv(t)
	need(t, e)
	e.waitReady(t)
	good, _ := os.ReadFile(makeAudio(t, e, "g.mp3", 4))
	if code, _ := e.importFile(t, "g.mp3", good, false); code != 403 {
		t.Errorf("import without the panel header: %d", code)
	}
	for name, data := range map[string][]byte{
		"text file":     []byte("just some text, not audio at all"),
		"playlist":      []byte("#EXTM3U\n#EXTINF:1,\n/etc/passwd\n"),
		"concat script": []byte("ffconcat version 1.0\nfile '/etc/passwd'\n"),
		"broken audio":  append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte{7}, 500)...),
		"tiny":          []byte("RIFF"),
	} {
		if code, m := e.importFile(t, "x.mp3", data, true); code != 400 {
			t.Errorf("%s accepted: %d %v", name, code, m)
		}
	}
	short, _ := os.ReadFile(makeAudio(t, e, "s.wav", 0.3))
	if code, _ := e.importFile(t, "s.wav", short, true); code != 400 {
		t.Error("recording shorter than a second accepted")
	}
	e.r.ffmpeg = ""
	if code, m := e.importFile(t, "g.mp3", good, true); code != 400 || !strings.Contains(m["error"].(string), "ffmpeg") {
		t.Errorf("without ffmpeg: %d %v", code, m)
	}
	if entries, _ := os.ReadDir(e.r.drafts); len(entries) != 0 {
		t.Errorf("refused imports left files: %v", entries)
	}
}

func TestSaveDraftRules(t *testing.T) {
	e := newEnv(t)
	need(t, e)
	e.waitReady(t)
	data, _ := os.ReadFile(makeAudio(t, e, "g.wav", 4))
	_, m := e.importFile(t, "g.wav", data, true)
	id := m["draft"].(string)
	save := func(draft, name, text string, hdr bool) int {
		b, _ := json.Marshal(map[string]string{"draft": draft, "name": name, "transcript": text})
		code, _ := e.panelPost(t, "/api/voices", string(b), hdr)
		return code
	}
	if save(id, "Ann", "words", false) != 403 || save("../../etc/passwd", "Ann", "words", true) != 400 || save("0123456789abcdef", "Ann", "words", true) != 404 {
		t.Fatal("draft id or header rules")
	}
	if save(id, "bad/name", "words", true) != 400 || save(id, "Ann", "  ", true) != 400 {
		t.Fatal("bad name or empty text accepted")
	}
	if save(id, "Ann", "words", true) != 200 {
		t.Fatal("the draft must survive a refused save, so the user can fix the name")
	}
	_, m = e.importFile(t, "g.wav", data, true)
	id2 := m["draft"].(string)
	e.panelPost(t, "/api/voices/draft/discard", `{"draft":"`+id2+`"}`, true)
	if _, err := os.Stat(filepath.Join(e.r.drafts, id2+".wav")); err == nil {
		t.Error("discard did not delete the draft")
	}
}

func TestTranscribeAndEditExistingVoice(t *testing.T) {
	e := newEnv(t)
	e.waitReady(t)
	os.WriteFile(filepath.Join(e.voices, "Hand.wav"), makeWAV(4), 0o644) // copied in by hand, no text
	list := func() voiceInfo { return e.r.voices.list()[0] }
	if v := list(); v.Usable || !strings.Contains(v.Note, "Transcribe") {
		t.Fatalf("%+v", v)
	}
	if code, _ := e.panelPost(t, "/api/voices/transcribe", `{"name":"Hand"}`, false); code != 403 {
		t.Fatal("transcribe without the panel header")
	}
	if code, _ := e.panelPost(t, "/api/voices/transcribe", `{"name":"../x"}`, true); code != 404 {
		t.Fatal("unknown voice")
	}
	code, m := e.panelPost(t, "/api/voices/transcribe", `{"name":"Hand"}`, true)
	if code != 200 || m["transcript"] != "This is a fake transcript." {
		t.Fatalf("%d %v", code, m)
	}
	if v := list(); v.Usable {
		t.Fatal("transcribing alone must not save anything: the user reviews first")
	}
	if code, _ := e.panelPost(t, "/api/voices/transcript", `{"name":"Hand","transcript":"Fixed by me."}`, true); code != 200 {
		t.Fatal("save transcript")
	}
	if v := list(); !v.Usable || v.Text != "Fixed by me." {
		t.Fatalf("%+v", v)
	}
	for _, bad := range []string{`{"name":"Hand","transcript":""}`, `{"name":"Nope","transcript":"x"}`, `{"name":"../Hand","transcript":"x"}`} {
		if code, _ := e.panelPost(t, "/api/voices/transcript", bad, true); code != 400 {
			t.Errorf("accepted %s", bad)
		}
	}
	if code, _ := e.panelPost(t, "/api/voices/transcript", `{"name":"Hand","transcript":"x"}`, false); code != 403 {
		t.Fatal("edit without the panel header")
	}
}

func TestSniffAudio(t *testing.T) {
	for name, b := range map[string][]byte{"wav": makeWAV(1), "flac": []byte("fLaC\x00\x00\x00\x22xxxxxxxx"), "ogg": []byte("OggS\x00\x02xxxxxxxxxxxx"),
		"mp3 tag": []byte("ID3\x03\x00\x00\x00\x00\x00\x00xx"), "mp3 frame": {0xFF, 0xFB, 0x90, 0, 0, 0, 0, 0, 0, 0, 0, 0}, "adts": {0xFF, 0xF1, 0x50, 0x80, 0, 0, 0, 0, 0, 0, 0, 0},
		"m4a": {0, 0, 0, 0x20, 'f', 't', 'y', 'p', 'M', '4', 'A', ' '}} {
		if !sniffAudio(b) {
			t.Errorf("%s refused", name)
		}
	}
	for name, b := range map[string][]byte{"text": []byte("hello world, this is text"), "m3u": []byte("#EXTM3U\n#EXTINF:1,xx"), "short": []byte("RIFF")} {
		if sniffAudio(b) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestPanelPageAndStatus(t *testing.T) {
	e := newEnv(t)
	resp, _ := http.Get(e.srv.URL + "/")
	b, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(b, []byte("SimpleVoice OBS")) {
		t.Fatal("no panel")
	}
	resp, _ = http.Get(e.srv.URL + "/api/status")
	var m map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&m)
	if m["engine"] == nil || m["voices"] == nil {
		t.Fatalf("%v", m)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/status", nil)
	req.Host = "evil.example"
	if r2, _ := http.DefaultClient.Do(req); r2.StatusCode != 403 {
		t.Fatal("panel reachable through a DNS name")
	}
}

func TestWavSecondsAndNames(t *testing.T) {
	if s, err := wavSeconds(makeWAV(2.5)); err != nil || s < 2.49 || s > 2.51 {
		t.Fatalf("%v %v", s, err)
	}
	for _, n := range []string{"Anna", "Max Mustermann", "Jörg", "a.b-c_d", "声"} {
		if !validName(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	for _, n := range []string{"", ".x", "a/b", `a\b`, "a:b", "nul", "Com1.x", "a.", "a ", strings.Repeat("a", 61)} {
		if validName(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
}

func TestWorkerQuitsWhenParentClosesStdin(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "worker.py"), workerPy, 0o644)
	port, _ := freePort()
	cmd := exec.Command(py, filepath.Join(dir, "worker.py"), "--port", fmt.Sprint(port), "--model", dir)
	cmd.Env = append(os.Environ(), "OMNIVOICE_FAKE=1", "OMNIVOICE_TOKEN=x")
	in, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	time.Sleep(500 * time.Millisecond)
	in.Close()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		t.Fatal("worker kept running after its parent closed stdin")
	}
}

func (e *env) status(t *testing.T) map[string]interface{} {
	t.Helper()
	resp, err := http.Get(e.srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&m)
	return m
}

func (e *env) voiceIDs(t *testing.T) []string {
	t.Helper()
	resp, err := http.Get(e.srv.URL + "/v1/audio/voices")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m struct{ Voices []struct{ ID string } }
	json.NewDecoder(resp.Body).Decode(&m)
	out := []string{}
	for _, v := range m.Voices {
		out = append(out, v.ID)
	}
	return out
}

func TestProcessorEngineSpeaksBuiltInVoices(t *testing.T) {
	e := newEnvMode(t, "cpu")
	e.addVoice(t, "Anna")
	e.waitReady(t)
	if st := e.r.eng.status(); st.Kind != "cpu" {
		t.Fatalf("%+v", st)
	}
	ids := strings.Join(e.voiceIDs(t), ",")
	if !strings.Contains(ids, "Heart") || strings.Contains(ids, "Anna") {
		t.Fatalf("processor engine lists built-in voices only: %s", ids)
	}
	if resp, b := e.speak(t, `{"input":"hello there","voice":"heart"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	resp, b := e.speak(t, `{"input":"hello","voice":"Anna"}`, nil)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "graphics card") {
		t.Fatalf("a cloned voice on the processor engine must explain itself: %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.speak(t, `{"input":"hello","voice":"nobody"}`, nil); resp.StatusCode != 404 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if st := e.status(t); st["speakable"] == nil {
		t.Fatalf("%v", st)
	}
}

func TestAutoFallsBackToProcessorWithoutGPU(t *testing.T) {
	t.Setenv("OMNIVOICE_FAKE_NOGPU", "1")
	e := newEnvMode(t, "auto")
	e.waitReady(t)
	st := e.r.eng.status()
	if st.Kind != "cpu" || st.Note == "" || !strings.Contains(strings.Join(st.Log, "\n"), "Switching to the processor engine") {
		t.Fatalf("%+v", st)
	}
	if resp, b := e.speak(t, `{"input":"hello","voice":"Heart"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestForcedGPUWithoutGPUReportsError(t *testing.T) {
	t.Setenv("OMNIVOICE_FAKE_NOGPU", "1")
	e := newEnvMode(t, "gpu")
	for i := 0; i < 100 && e.r.eng.status().State != "error"; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	st := e.r.eng.status()
	if st.State != "error" || st.Kind != "gpu" || !strings.Contains(st.Msg, "NVIDIA") {
		t.Fatalf("%+v", st)
	}
	if resp, _ := e.speak(t, `{"input":"hello","voice":"x"}`, nil); resp.StatusCode != 503 && resp.StatusCode != 404 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestSwitchEngineFromPanel(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "Anna")
	e.waitReady(t)
	if code, _ := e.panelPost(t, "/api/engine/mode", `{"mode":"cpu"}`, false); code != 403 {
		t.Fatalf("needs the panel header: %d", code)
	}
	if code, _ := e.panelPost(t, "/api/engine/mode", `{"mode":"banana"}`, true); code != 400 {
		t.Fatalf("%d", code)
	}
	if code, _ := e.panelPost(t, "/api/engine/mode", `{"mode":"cpu"}`, true); code != 200 {
		t.Fatalf("%d", code)
	}
	e.waitReady(t)
	if st := e.r.eng.status(); st.Kind != "cpu" || st.Mode != "cpu" {
		t.Fatalf("%+v", st)
	}
	if code, _ := e.panelPost(t, "/api/engine/mode", `{"mode":"gpu"}`, true); code != 200 {
		t.Fatalf("%d", code)
	}
	e.waitReady(t)
	if st := e.r.eng.status(); st.Kind != "gpu" {
		t.Fatalf("%+v", st)
	}
	if resp, b := e.speak(t, `{"input":"hello","voice":"Anna"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestTranscriptionWorksOnProcessorEngine(t *testing.T) {
	e := newEnvMode(t, "cpu")
	need(t, e)
	e.waitReady(t)
	for i := 0; i < 50 && e.r.eng.status().ASR != "ready"; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if e.r.eng.status().ASR != "ready" {
		t.Fatalf("%+v", e.r.eng.status())
	}
	data, _ := os.ReadFile(makeAudio(t, e, "a.mp3", 4))
	_, m := e.importFile(t, "a.mp3", data, true)
	if m["transcript"] != "This is a fake transcript." {
		t.Fatalf("%v", m)
	}
}

func TestSplitAudioNeverExceedsLimit(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	if exec.Command(py, "-c", "import numpy").Run() != nil {
		t.Skip("numpy not installed")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "worker.py"), workerPy, 0o644)
	code := `
import sys, numpy as np
sys.path.insert(0, sys.argv[1])
import worker
sr = 16000
a = np.random.RandomState(1).randn(sr * 75).astype("float32")
a[int(sr*27):int(sr*27.5)] = 0   # a quiet spot just before the limit
segs = worker.split_audio(a, sr)
assert sum(len(s) for s in segs) == len(a)
assert all(len(s) <= 28 * sr for s in segs), [len(s) / sr for s in segs]
assert len(segs) == 3, len(segs)
assert abs(len(segs[0]) - int(sr * 27.25)) < sr * 0.3, len(segs[0]) / sr
short = worker.split_audio(a[:sr * 10], sr)
assert len(short) == 1
print("ok")
`
	out, err := exec.Command(py, "-I", "-c", code, dir).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("%v %s", err, out)
	}
}

func TestAutoFallsBackWhenTheGPUEngineFailsForAnyReason(t *testing.T) {
	t.Setenv("OMNIVOICE_FAKE_FAIL", "1") // the failure seen on a real PC: an ImportError while loading
	e := newEnvMode(t, "auto")
	e.waitReady(t)
	st := e.r.eng.status()
	if st.Kind != "cpu" || !strings.Contains(st.Note, "AutoFeatureExtractor") || !strings.Contains(st.Note, "processor engine") {
		t.Fatalf("%+v", st)
	}
	if resp, b := e.speak(t, `{"input":"hello","voice":"Heart"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestForcedGPUShowsTheRealError(t *testing.T) {
	t.Setenv("OMNIVOICE_FAKE_FAIL", "1")
	e := newEnvMode(t, "gpu")
	for i := 0; i < 100 && e.r.eng.status().State != "error"; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	st := e.r.eng.status()
	if st.State != "error" || st.Kind != "gpu" || !strings.Contains(st.Msg, "AutoFeatureExtractor") {
		t.Fatalf("a forced engine must not hide its error: %+v", st)
	}
}

// The transcription model must not start loading while the voice engine is still loading: two
// threads importing PyTorch and transformers together broke the engine on a real PC.
func TestTranscriptionLoadsOnlyAfterTheVoiceEngine(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "worker.py")
	os.WriteFile(script, workerPy, 0o644)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cmd := exec.Command(py, "-I", script, "--port", fmt.Sprint(port), "--backend", "omnivoice", "--model", dir, "--kokoro", dir, "--asr", dir)
	cmd.Env = append(os.Environ(), "OMNIVOICE_FAKE=1", "OMNIVOICE_FAKE_SLOW=1", "OMNIVOICE_TOKEN=t")
	in, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); cmd.Wait() }()
	health := func() (state, asr string) {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
		req.Header.Set("X-Worker-Token", "t")
		resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
		if err != nil {
			return "", ""
		}
		defer resp.Body.Close()
		var h struct{ State, ASR string }
		json.NewDecoder(resp.Body).Decode(&h)
		return h.State, h.ASR
	}
	sawLoading := false
	for i := 0; i < 100; i++ {
		st, asr := health()
		if st == "loading" && asr == "loading" {
			sawLoading = true
		}
		if st == "loading" && asr == "ready" {
			t.Fatal("transcription started loading while the voice engine was still loading")
		}
		if st == "ready" && asr == "ready" {
			if !sawLoading {
				t.Fatal("never observed the loading phase")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("did not become ready")
}

func TestSlowLoadPrintsWhereItIsStuck(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "worker.py")
	os.WriteFile(script, workerPy, 0o644)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cmd := exec.Command(py, "-I", script, "--port", fmt.Sprint(port), "--backend", "omnivoice", "--model", dir, "--kokoro", dir, "--asr", dir)
	cmd.Env = append(os.Environ(), "OMNIVOICE_FAKE=1", "OMNIVOICE_FAKE_SLOW=1", "OMNIVOICE_DUMP_AFTER=0.4", "OMNIVOICE_TOKEN=t")
	var buf bytes.Buffer
	var mu sync.Mutex
	cmd.Stderr = writerFunc(func(p []byte) { mu.Lock(); buf.Write(p); mu.Unlock() })
	in, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	in.Close()
	cmd.Wait()
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "worker started") || !strings.Contains(out, "most recent call first") {
		t.Fatalf("no start line or stack dump in:\n%s", out)
	}
}

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }

func TestEngineLogKeepsALongStallReport(t *testing.T) {
	e := &engine{}
	for i := 0; i < 150; i++ {
		e.addLog(fmt.Sprintf("line %d", i))
	}
	if got := len(e.status().Log); got != 150 {
		t.Fatalf("kept %d lines of 150", got)
	}
}

func TestPanelIsMarkedNotToBeTranslated(t *testing.T) {
	if !bytes.Contains(panelHTML, []byte(`translate="no"`)) || !bytes.Contains(panelHTML, []byte(`content="notranslate"`)) {
		t.Fatal("panel can trigger the browser translate popup")
	}
}

// On Windows a new thread can deadlock against a library that is being loaded. So while the
// engine loads, the worker must answer requests without starting any thread.
func TestWorkerStartsNoThreadsWhileLoading(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "worker.py"), workerPy, 0o644)
	script := `
import sys, threading, urllib.request
sys.path.insert(0, sys.argv[1])
import worker
worker.token = "t"
srv = worker.Server(("127.0.0.1", 0), worker.Handler)
port = srv.server_address[1]
def ask():
    req = urllib.request.Request("http://127.0.0.1:%d/health" % port, headers={"X-Worker-Token": "t"})
    urllib.request.urlopen(req, timeout=10).read()
starts = []
real = threading.Thread.start
def counting(self):
    starts.append(self.name)
    real(self)
def run(state, asr):
    worker.status["state"], worker.status["asr"] = state, asr
    c = threading.Thread(target=ask)
    real(c)
    n = len(starts)
    srv.handle_request()
    c.join()
    return len(starts) - n
threading.Thread.start = counting
print("loading", run("loading", "loading"))
print("engine ready, transcription loading", run("ready", "loading"))
print("all ready", run("ready", "ready"))
`
	out, err := exec.Command(py, "-I", "-c", script, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "loading 0\nengine ready, transcription loading 0\nall ready 1\n"
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

// On Windows the worker must never block a thread reading its stdin pipe (that froze NumPy's loading
// on a real PC). It waits for the relay process instead. The real Windows calls cannot run here, so
// this checks the logic with stand-ins.
func TestWindowsWorkerWatchesTheRelayProcessNotStdin(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "worker.py"), workerPy, 0o644)
	script := `
import sys, ctypes
sys.path.insert(0, sys.argv[1])
import worker
class NoStdin:
    @property
    def buffer(self):
        raise AssertionError("read stdin")
sys.stdin = NoStdin()
exits = []
worker.os._exit = lambda code: exits.append(code)
calls = []
def make(opened):
    def OpenProcess(a, b, pid):
        calls.append(("open", a, pid)); return opened
    def WaitForSingleObject(h, ms):
        calls.append(("wait", h, ms)); return 0
    def GetLastError(): return 5
    class K: pass
    K.OpenProcess, K.WaitForSingleObject, K.GetLastError = staticmethod(OpenProcess), staticmethod(WaitForSingleObject), staticmethod(GetLastError)
    class W: kernel32 = K
    return W
def run(opened):
    del calls[:]; del exits[:]
    ctypes.windll = make(opened)
    worker.os.name = "nt"
    try:
        worker.watch_parent(1234)
    finally:
        worker.os.name = "posix"
    return list(calls), list(exits)
print(run(77))
print(run(0))
`
	out, err := exec.Command(py, "-I", "-c", script, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "([('open', 1048576, 1234), ('wait', 77, 4294967295)], [0])\n([('open', 1048576, 1234)], [])\n"
	if !strings.HasSuffix(string(out), want) {
		t.Fatalf("got:\n%s\nwant (at the end):\n%s", out, want)
	}
}

func TestWorkerWithoutPipeIsStoppedByKilling(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep command")
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	killer{cmd}.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker not stopped")
	}
}

func TestQualityChoiceReachesTheEngineAndIsSaved(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "Anna")
	e.waitReady(t)
	if got := e.status(t)["steps"]; got != float64(16) {
		t.Fatalf("test engine starts with 16 steps, status says %v", got)
	}
	post := func(body string) int {
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/engine/quality", strings.NewReader(body))
		for k, v := range map[string]string{"X-Relay-Panel": "1", "Content-Type": "application/json"} {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if post(`{"steps":7}`) != 400 {
		t.Fatal("an odd number of steps must be refused")
	}
	if post(`{"steps":48}`) != 200 {
		t.Fatal("high quality must be accepted")
	}
	if got := e.status(t)["steps"]; got != float64(48) {
		t.Fatalf("status says %v", got)
	}
	e.speak(t, `{"input":"hello there friend","voice":"anna","response_format":"wav"}`, nil)
	if !strings.Contains(strings.Join(e.r.eng.status().Log, "\n"), "(speed 1.00, 48 steps, language automatic)") {
		t.Fatalf("the speech was not made with 48 steps:\n%s", strings.Join(e.r.eng.status().Log, "\n"))
	}
}

func TestEveryProcessorVoiceBecomesASampleVoice(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "worker.py"), workerPy, 0o644)
	out, err := exec.Command(py, "-I", "-c", "import sys\nsys.path.insert(0, sys.argv[1])\nimport worker\nprint(len(worker.STARTERS), sorted(worker.STARTERS) == sorted(worker.KOKORO_VOICES))", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "13 True" {
		t.Fatalf("got %q", out)
	}
}

func TestRenameAVoice(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "Anna")
	e.addVoice(t, "Ben")
	post := func(body string) (int, string) {
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/voices/rename", strings.NewReader(body))
		req.Header.Set("X-Relay-Panel", "1")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	dir := e.r.voices.dir
	before := readTranscript(filepath.Join(dir, "Anna.txt"))
	if code, b := post(`{"name":"Anna","newName":"Luna Rose"}`); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "Anna.wav")); err == nil {
		t.Fatal("the old recording is still there")
	}
	if _, err := os.Stat(filepath.Join(dir, "Luna Rose.wav")); err != nil {
		t.Fatal("the recording did not move")
	}
	if got := readTranscript(filepath.Join(dir, "Luna Rose.txt")); got == "" || got != before {
		t.Fatalf("the words did not move: %q vs %q", got, before)
	}
	if _, err := os.Stat(filepath.Join(dir, "Anna.txt")); err == nil {
		t.Fatal("the old words file is still there")
	}
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"name":"Luna Rose","newName":"ben"}`, 400},       // taken (any capitals)
		{`{"name":"Luna Rose","newName":"a/b"}`, 400},       // not a valid name
		{`{"name":"Luna Rose","newName":"  "}`, 400},        // empty
		{`{"name":"Luna Rose","newName":"CON"}`, 400},       // reserved by Windows
		{`{"name":"Nobody","newName":"Someone"}`, 404},      // no such voice
		{`{"name":"../x","newName":"Someone"}`, 404},        // no escape from the folder
		{`{"name":"Luna Rose","newName":"luna rose"}`, 200}, // only the capitals change
	} {
		if code, b := post(c.body); code != c.code {
			t.Errorf("%s: %d %s", c.body, code, b)
		}
	}
	if ids := strings.Join(e.voiceIDs(t), ","); !strings.Contains(ids, "luna rose") && !strings.Contains(strings.ToLower(ids), "luna rose") {
		t.Fatalf("the new name is not offered: %s", ids)
	}
	// without the panel header it is refused
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/voices/rename", strings.NewReader(`{"name":"Ben","newName":"Bob"}`))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("rename without the panel header: %d", resp.StatusCode)
		}
	}
}

func TestNumbersAreWrittenAsWords(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "worker.py"), workerPy, 0o644)
	cases := [][2]string{
		{"I have 3 cats", "I have three cats"},
		{"In 2024 we met", "In two thousand twenty-four we met"},
		{"1,234 people", "one thousand two hundred thirty-four people"},
		{"3.5 and 50%", "three point five and fifty percent"},
		{"$20 or $5.50 or $1", "twenty dollars or five dollars and fifty cents or one dollar"},
		{"the 1st, 2nd, 3rd, 21st, 12th, 40th", "the first, second, third, twenty-first, twelfth, fortieth"},
		{"at 10:30, 9:05 and 7:00", "at ten thirty, nine oh five and seven o'clock"},
		{"say [B EY1 S] 42", "say [B EY1 S] forty-two"},
		{"1000000 and 123456789012345", "one million and 123456789012345"},
		{"no numbers here", "no numbers here"},
		{"I am 100% sure", "I am one hundred percent sure"},
		{strings.Repeat("9", 5000) + " " + strings.Repeat("7", 5000) + "th $" + strings.Repeat("1", 5000), strings.Repeat("9", 5000) + " " + strings.Repeat("7", 5000) + "th $" + strings.Repeat("1", 5000)},
	}
	cj, _ := json.Marshal(cases)
	script := "import sys, json\nsys.path.insert(0, sys.argv[1])\nimport worker\nfor a, b in json.loads(sys.argv[2]):\n    got = worker.spoken_numbers(a)\n    print('OK' if got == b else 'BAD %r -> %r (want %r)' % (a, got, b))\n"
	out, err := exec.Command(py, "-I", "-c", script, dir, string(cj)).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(string(out), "BAD") || strings.Count(string(out), "OK") != len(cases) {
		t.Fatalf("%s", out)
	}
}

func TestOptionsReachTheEngineAndAreValidated(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "anna")
	e.waitReady(t)
	if c, _ := e.panelPost(t, "/api/engine/options", `{"language":"de","numbers":true}`, false); c == 200 {
		t.Fatal("options must need the panel header")
	}
	if c, _ := e.panelPost(t, "/api/engine/options", `{"language":"klingon"}`, true); c != 400 {
		t.Fatalf("unknown language gave %d", c)
	}
	if c, _ := e.panelPost(t, "/api/engine/options", `{"language":"de","numbers":true}`, true); c != 200 {
		t.Fatalf("valid options gave %d", c)
	}
	st := e.status(t)
	if st["language"] != "de" || st["numbers"] != true {
		t.Fatalf("status: %v %v", st["language"], st["numbers"])
	}
	if _, ok := st["languages"].([]interface{}); !ok {
		t.Fatal("status lacks the language list")
	}
	e.speak(t, `{"input":"hello there friend","voice":"anna","response_format":"wav"}`, nil)
	if !strings.Contains(strings.Join(e.r.eng.status().Log, "\n"), "language 'de'") {
		t.Fatalf("language did not reach the worker:\n%s", strings.Join(e.r.eng.status().Log, "\n"))
	}
}

func TestSaveConvertsToTheOfferedFormats(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "anna")
	e.waitReady(t)
	formats, _ := e.status(t)["formats"].([]interface{})
	if len(formats) == 0 {
		t.Skip("no ffmpeg here")
	}
	_, wav := e.speak(t, `{"input":"hello there friend","voice":"anna","response_format":"wav"}`, nil)
	post := func(f string, hdr bool) (int, []byte) {
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/convert?format="+f, bytes.NewReader(wav))
		if hdr {
			req.Header.Set("X-Relay-Panel", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	if c, _ := post("flac", false); c == 200 {
		t.Fatal("convert must need the panel header")
	}
	if c, _ := post("ogg", true); c != 400 {
		t.Fatalf("an unlisted format gave %d", c)
	}
	for _, f := range formats {
		if f == "wav" { // the panel saves WAV as it is; no conversion
			continue
		}
		c, b := post(f.(string), true)
		if c != 200 || len(b) < 100 {
			t.Fatalf("%v gave %d, %d bytes", f, c, len(b))
		}
	}
}

// The Test section has a Generate button that always makes a new result, a separate Play/Stop
// button, and Save; the voices are in a collapsible section; the page carries the new name.
func TestPanelHasGenerateAndPlayAndCollapsibleVoices(t *testing.T) {
	e := newEnv(t)
	resp, _ := http.Get(e.srv.URL + "/")
	b, _ := io.ReadAll(resp.Body)
	page := string(b)
	for _, want := range []string{`<h1>SimpleVoice OBS</h1>`, `id="gen"`, `>Generate<`, `id="go"`, `id="save"`, `<details id="vdet">`} {
		if !strings.Contains(page, want) {
			t.Errorf("panel lacks %s", want)
		}
	}
	if strings.Contains(page, ">Speak<") || strings.Contains(page, "<h1>OmniVoice Relay") {
		t.Error("an old label is still there")
	}
	if !strings.Contains(page, "classList.toggle('off',off)") {
		t.Error("unusable fields are not grayed out")
	}
}

// Imported voices cannot go slower than 0.8. The sample voices and the built-in voices can go to 0.5.
func TestSlowSpeedIsLimitedForRecordedVoices(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "anna")
	e.waitReady(t)
	e.speak(t, `{"input":"hello there friend","voice":"anna","response_format":"wav","speed":0.5}`, nil)
	if !strings.Contains(strings.Join(e.r.eng.status().Log, "\n"), "asked for speed 0.80") {
		t.Fatalf("a recorded voice was allowed below 0.8:\n%s", strings.Join(e.r.eng.status().Log, "\n"))
	}
	e.speak(t, `{"input":"hello there friend","voice":"anna","response_format":"wav","speed":0.9}`, nil)
	if !strings.Contains(strings.Join(e.r.eng.status().Log, "\n"), "asked for speed 0.90") {
		t.Fatal("0.9 must pass unchanged")
	}
	e.addVoice(t, "Heart")
	e.speak(t, `{"input":"hello there friend","voice":"Heart","response_format":"wav","speed":0.5}`, nil)
	if !strings.Contains(strings.Join(e.r.eng.status().Log, "\n"), "asked for speed 0.50") {
		t.Fatalf("a sample voice must still go to 0.5:\n%s", strings.Join(e.r.eng.status().Log, "\n"))
	}
	if s, _ := e.status(t)["samples"].([]interface{}); len(s) != 13 {
		t.Fatal("status lacks the sample voice names")
	}
}

// The list of sample voices exists in the worker, here, and in Setup. They must not drift apart.
func TestSampleVoiceListMatchesTheWorker(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "worker.py"), workerPy, 0o644)
	out, err := exec.Command(py, "-I", "-c", "import sys, json\nsys.path.insert(0, sys.argv[1])\nimport worker\nprint(json.dumps(worker.STARTERS))", dir).Output()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	if json.Unmarshal(out, &names) != nil || strings.Join(names, ",") != strings.Join(sampleVoiceNames, ",") {
		t.Fatalf("worker has %v, relay has %v", names, sampleVoiceNames)
	}
}

func TestWithEnding(t *testing.T) {
	for in, want := range map[string]string{
		"hello":        "hello,",
		"hello  ":      "hello,",
		"2024":         "2024,",
		"für Ärger":    "für Ärger,",
		"hello.":       "hello.",
		"hello!":       "hello!",
		"really?":      "really?",
		"wait,":        "wait,",
		"(done)":       "(done)",
		`say "hi"`:     `say "hi"`,
		"fine 😀":       "fine 😀",
		"":             "",
		"   ":          "   ",
		"こんにちは":        "こんにちは,",
		"hello world…": "hello world…",
	} {
		if got := withEnding(in); got != want {
			t.Errorf("withEnding(%q) = %q, want %q", in, got, want)
		}
	}
}

// The fake worker makes a tone whose length follows the text, so one more character is audible as 0.05 s.
func TestEndingMarkOnlyOnTheGraphicsCardEngine(t *testing.T) {
	secs := func(e *env, voice, text string) float64 {
		t.Helper()
		resp, b := e.speak(t, `{"input":"`+text+`","voice":"`+voice+`"}`, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
		s, err := wavSeconds(b)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	gpu := newEnv(t)
	gpu.addVoice(t, "Anna")
	gpu.waitReady(t)
	if a, b := secs(gpu, "anna", "botting is the best way to stream ever"), secs(gpu, "anna", "botting is the best way to stream ever,"); a != b {
		t.Fatalf("graphics card engine: a text without ending gets a comma (same length as with one): %.2f vs %.2f", a, b)
	}
	cpu := newEnvMode(t, "cpu")
	cpu.waitReady(t)
	if a, b := secs(cpu, "heart", "botting is the best way to stream ever"), secs(cpu, "heart", "botting is the best way to stream ever,"); !(a < b) {
		t.Fatalf("processor engine: the text is spoken as it is: %.2f vs %.2f", a, b)
	}
}

func TestVoiceListTellsTheSlowestSpeed(t *testing.T) {
	read := func(e *env) map[string]float64 {
		t.Helper()
		resp, err := http.Get(e.srv.URL + "/v1/audio/voices")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m struct {
			Voices []struct {
				ID       string
				MinSpeed *float64 `json:"min_speed"`
			}
		}
		json.NewDecoder(resp.Body).Decode(&m)
		out := map[string]float64{}
		for _, v := range m.Voices {
			if v.MinSpeed == nil {
				t.Fatalf("voice %s has no min_speed", v.ID)
			}
			out[v.ID] = *v.MinSpeed
		}
		return out
	}
	gpu := newEnv(t)
	gpu.addVoice(t, "Anna")  // imported
	gpu.addVoice(t, "Heart") // a sample voice
	gpu.waitReady(t)
	got := read(gpu)
	if got["Anna"] != 0.8 || got["Heart"] != 0.5 || len(got) != 2 {
		t.Fatalf("graphics card engine: %v", got)
	}
	cpu := newEnvMode(t, "cpu")
	cpu.waitReady(t)
	for id, v := range read(cpu) {
		if v != 0.5 {
			t.Fatalf("built-in voice %s: %v", id, v)
		}
	}
	// what the list says is what speech() does
	resp, b := gpu.speak(t, `{"input":"This is a sentence of some length.","voice":"anna","speed":0.5}`, nil)
	resp2, b2 := gpu.speak(t, `{"input":"This is a sentence of some length.","voice":"anna","speed":0.8}`, nil)
	if resp.StatusCode != 200 || resp2.StatusCode != 200 || len(b) != len(b2) {
		t.Fatalf("an imported voice asked for 0.5 is made at 0.8: %d vs %d bytes", len(b), len(b2))
	}
}
