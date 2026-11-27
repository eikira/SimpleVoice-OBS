//go:build !windows

// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

// constWAV is a clip with one constant sample value (easy to find again in the joined audio).
func constWAV(secs float64, value int16) []byte {
	n := int(16000 * secs)
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(value))
	}
	return buildWAV(16000, pcm)
}

func (e *env) sbAdd(t *testing.T, name string, wav []byte, volume string, hdr bool) (int, map[string]interface{}) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("name", name)
	mw.WriteField("volume", volume)
	fw, _ := mw.CreateFormFile("audio", "clip.wav")
	fw.Write(wav)
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/soundbites/add", &buf)
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

func TestSplitEmotes(t *testing.T) {
	s := newSoundbites(t.TempDir())
	if err := s.add("choppa", buildWAV(sbRate, make([]byte, 4800)), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.add("Boom", buildWAV(sbRate, make([]byte, 4800)), 1); err != nil {
		t.Fatal(err)
	}
	parts, clips := s.splitEmotes("hello :choppa: friend :BOOM: :nope: bye")
	var got []string
	for _, p := range parts {
		if p.clip != "" {
			got = append(got, "["+p.clip+"]")
		} else {
			got = append(got, p.text)
		}
	}
	if clips != 2 || strings.Join(got, "|") != "hello|[choppa]|friend|[BOOM]|:nope: bye" {
		t.Fatalf("clips=%d parts=%q", clips, got)
	}
	if _, c := s.splitEmotes("no emotes: here, 12:30:45 or :nope:"); c != 0 {
		t.Fatal("unknown names are plain text")
	}
	many := strings.Repeat(":choppa: ", 10)
	if _, c := s.splitEmotes(many); c != sbMaxClips {
		t.Fatalf("at most %d clips from one message, got %d", sbMaxClips, c)
	}
}

func TestSoundbitesEndToEndOrderAndWaiting(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "Anna")
	e.waitReady(t)
	if code, m := e.sbAdd(t, "choppa", constWAV(2, 20000), "1", true); code != 200 {
		t.Fatalf("add: %d %v", code, m)
	}
	resp, _ := http.Get(e.srv.URL + "/v1/soundbites")
	var l struct{ Soundbites []sbEntry }
	json.NewDecoder(resp.Body).Decode(&l)
	resp.Body.Close()
	if len(l.Soundbites) != 1 || l.Soundbites[0].Name != "choppa" || l.Soundbites[0].Seconds < 1.9 || l.Soundbites[0].Seconds > 2.1 {
		t.Fatalf("list: %+v", l)
	}

	r, b := e.speak(t, `{"input":"hello :choppa: friend","voice":"anna"}`, nil)
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	rate, pcm, err := parseWAV(b)
	if err != nil || rate != 24000 {
		t.Fatal(rate, err)
	}
	// where is the clip (constant, loud)? There must be speech before it and after it, in that order.
	first, last, n := -1, -1, len(pcm)/2
	loud := 0
	for i := 0; i < n; i++ {
		v := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		if v > 15000 && v < 25000 {
			loud++
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	clipSecs := float64(last-first) / 24000
	if first < 24000/10 || clipSecs < 1.8 || clipSecs > 2.2 || n-last < 24000/10 {
		t.Fatalf("clip is not between the two spoken pieces: first=%d last=%d n=%d loud=%d", first, last, n, loud)
	}
	// hello (5+1 chars) 0.3 s, friend (6+1) 0.35 s, clip 2 s, two gaps 0.18 s
	if total := float64(n) / 24000; total < 2.7 || total > 3.2 {
		t.Fatalf("total %.2f s", total)
	}

	// only a clip: no voice needed
	r, b = e.speak(t, `{"input":":choppa:","voice":"nobody"}`, nil)
	if r.StatusCode != 200 {
		t.Fatalf("clip only: %d %s", r.StatusCode, b)
	}
	// the clip was added at 16 kHz and is played at 16 kHz (its own rate)
	if rt, p, _ := parseWAV(b); rt != 16000 || len(p)/2 < 16000*19/10 || len(p)/2 > 16000*21/10 {
		t.Fatalf("clip only: %d Hz, %d samples", rt, len(p)/2)
	}
	// spoken text with a wrong voice is still refused
	if r, _ = e.speak(t, `{"input":"hello :choppa:","voice":"nobody"}`, nil); r.StatusCode != 404 {
		t.Fatalf("wrong voice: %d", r.StatusCode)
	}
	// an unknown :name: is read as before (one spoken piece, no clip)
	r, b = e.speak(t, `{"input":"look :unknown: here","voice":"anna"}`, nil)
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	if _, p, _ := parseWAV(b); len(p)/2 > 24000*13/10 {
		t.Fatalf("an unknown emote must not add a clip: %d samples", len(p)/2)
	}
}

func TestSoundbitesVolumeRulesAndGuard(t *testing.T) {
	e := newEnv(t)
	e.waitReady(t)
	if code, _ := e.sbAdd(t, "quiet", constWAV(1, 20000), "0.5", false); code != 403 {
		t.Fatalf("without the panel header: %d", code)
	}
	for _, bad := range []string{"", "a b", "x:y", "../evil", strings.Repeat("a", 41)} {
		if code, _ := e.sbAdd(t, bad, constWAV(1, 20000), "1", true); code != 400 {
			t.Errorf("name %q: %d", bad, code)
		}
	}
	if code, m := e.sbAdd(t, "long", constWAV(16, 1000), "1", true); code != 400 || !strings.Contains(m["error"].(string), "15 seconds") {
		t.Fatalf("a 16 second clip: %d %v", code, m)
	}
	if code, m := e.sbAdd(t, ":quiet:", constWAV(1, 20000), "0.5", true); code != 200 {
		t.Fatalf("colons around the name are accepted: %d %v", code, m)
	}
	qc, ok := e.r.sounds.audio("QUIET")
	if !ok {
		t.Fatal("name is not case sensitive")
	}
	pcm := qc.pcm
	if v := int16(binary.LittleEndian.Uint16(pcm[200:])); v < 9000 || v > 11000 {
		t.Fatalf("volume 0.5 of 20000 should be about 10000, got %d", v)
	}
	// it survives a restart
	if again := newSoundbites(e.r.sounds.dir); !again.has("quiet") {
		t.Fatal("not kept")
	}
	if code, _ := e.panelPost(t, "/api/soundbites/delete", `{"name":"quiet"}`, true); code != 200 || e.r.sounds.has("quiet") {
		t.Fatal("delete")
	}
	resp, _ := http.Get(e.srv.URL + "/api/soundbites/play?name=quiet")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("play after delete: %d", resp.StatusCode)
	}
}

// stereoWAV: left channel constant l, right channel constant r.
func stereoWAV(rate int, secs float64, l, r int16) []byte {
	n := int(float64(rate) * secs)
	pcm := make([]byte, n*4)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(pcm[i*4:], uint16(l))
		binary.LittleEndian.PutUint16(pcm[i*4+2:], uint16(r))
	}
	return buildWAVn(rate, 2, pcm)
}

func TestSoundbitesStereoStaysStereo(t *testing.T) {
	e := newEnv(t)
	e.addVoice(t, "Anna")
	e.waitReady(t)
	if code, m := e.sbAdd(t, "wide", stereoWAV(44100, 1, 20000, -20000), "1", true); code != 200 {
		t.Fatalf("add stereo: %d %v", code, m)
	}
	if code, m := e.sbAdd(t, "mono", constWAV(1, 12000), "1", true); code != 200 {
		t.Fatalf("add mono: %d %v", code, m)
	}
	c, ok := e.r.sounds.audio("wide")
	if !ok || c.ch != 2 || c.rate != 44100 {
		t.Fatalf("a stereo clip keeps its channels and rate: %+v", c.rate)
	}
	if m, _ := e.r.sounds.audio("mono"); m.ch != 1 || m.rate != 16000 {
		t.Fatalf("a mono clip below 48 kHz keeps its rate: %d Hz, %d channel(s)", m.rate, m.ch)
	}

	// A message of only this clip is played in the clip's own format.
	r, b := e.speak(t, `{"input":":wide:","voice":"anna"}`, nil)
	if r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	rate, ch, pcm, err := parseWAVn(b)
	if err != nil || rate != 44100 || ch != 2 {
		t.Fatalf("clip only: %d Hz, %d channels, %v", rate, ch, err)
	}
	l := int16(binary.LittleEndian.Uint16(pcm[1000*4:]))
	rr := int16(binary.LittleEndian.Uint16(pcm[1000*4+2:]))
	if l < 19000 || rr > -19000 {
		t.Fatalf("left %d right %d: the two channels must stay different", l, rr)
	}

	// speech, clip, speech: stereo file, speech in both channels, clip left and right different, in this order.
	r, b = e.speak(t, `{"input":"hello :wide: friend","voice":"anna"}`, nil)
	if r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	rate, ch, pcm, err = parseWAVn(b)
	if err != nil || ch != 2 || rate != 44100 {
		t.Fatalf("mixed: %d Hz, %d channels, %v", rate, ch, err)
	}
	frames := len(pcm) / 4
	first, last, speechBefore, speechAfter := -1, -1, false, false
	for i := 0; i < frames; i++ {
		lv := int16(binary.LittleEndian.Uint16(pcm[i*4:]))
		rv := int16(binary.LittleEndian.Uint16(pcm[i*4+2:]))
		if lv > 18000 && rv < -18000 {
			if first < 0 {
				first = i
			}
			last = i
		} else if lv != rv && first < 0 {
			t.Fatalf("speech before the clip must be the same in both channels (frame %d: %d %d)", i, lv, rv)
		}
		if lv != 0 && lv == rv {
			if first < 0 {
				speechBefore = true
			} else if i > last {
				speechAfter = true
			}
		}
	}
	if first < 0 || !speechBefore || !speechAfter {
		t.Fatalf("order speech, clip, speech not found: first=%d last=%d before=%v after=%v", first, last, speechBefore, speechAfter)
	}
}
