// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Control panel side of the soundbites.

func (r *relay) soundbiteList(w http.ResponseWriter) {
	list := []sbEntry{}
	if r.sounds != nil {
		list = r.sounds.entries()
	}
	writeJSONNoCORS(w, 200, map[string]interface{}{"soundbites": list})
}

func (r *relay) soundbiteAdd(w http.ResponseWriter, req *http.Request) {
	if !guardPanel(w, req) {
		return
	}
	if r.sounds == nil {
		speechError(w, 500, "Soundbites are not available.")
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, 20<<20)
	if err := req.ParseMultipartForm(8 << 20); err != nil {
		speechError(w, 400, "The upload failed or is larger than 20 MB.")
		return
	}
	name := strings.Trim(strings.TrimSpace(req.FormValue("name")), ":")
	if !sbNameRe.MatchString(name) {
		speechError(w, 400, "The name may only have letters, digits, - and _ (1 to 40 characters). Write it without the colons, for example choppa.")
		return
	}
	volume := 1.0
	if v, err := strconv.ParseFloat(req.FormValue("volume"), 64); err == nil {
		volume = v
	}
	f, _, err := req.FormFile("audio")
	if err != nil {
		speechError(w, 400, "Choose a sound file.")
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
	out := r.draftPath(randomHex(8))
	defer os.Remove(out)
	if err := toClipWAV(req.Context(), r.ffmpeg, in.Name(), out, sbMaxSecs+1); err != nil {
		speechError(w, 400, err.Error())
		return
	}
	data, err := os.ReadFile(out)
	if err != nil {
		speechError(w, 500, "Could not read the converted clip.")
		return
	}
	if err := r.sounds.add(name, data, volume); err != nil {
		speechError(w, 400, err.Error())
		return
	}
	writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true})
}

func (r *relay) soundbiteDelete(w http.ResponseWriter, req *http.Request) {
	if !guardPanel(w, req) || r.sounds == nil {
		return
	}
	var p struct{ Name string }
	if json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&p) != nil || !sbNameRe.MatchString(p.Name) {
		speechError(w, 400, "bad request")
		return
	}
	r.sounds.remove(p.Name)
	writeJSONNoCORS(w, 200, map[string]interface{}{"ok": true})
}

// soundbitePlay lets the panel play a clip (with its volume) before it is used.
func (r *relay) soundbitePlay(w http.ResponseWriter, req *http.Request) {
	name := req.URL.Query().Get("name")
	if r.sounds == nil || !sbNameRe.MatchString(name) {
		fail(w, 404, "not found")
		return
	}
	c, ok := r.sounds.audio(name)
	if !ok {
		fail(w, 404, "not found")
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buildWAVn(c.rate, c.ch, c.pcm))
}
