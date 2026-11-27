// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

// Soundbites: short sound clips that are played when a chat message contains :NAME: (the way Rumble
// writes its emotes). Nothing is synthesized for a clip. A message that holds clips is answered with
// ONE finished WAV: every spoken piece is made first, then the pieces and clips are joined in the order
// they were written.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

const (
	sbMaxSecs    = 15    // longest clip that can be added
	sbMaxClips   = 6     // clips played from one message; further ones are dropped
	sbMaxTotal   = 30.0  // seconds of clips in one message
	sbMaxEntries = 300   // clips in the library
	sbRate       = 24000 // the rate the voices speak at (clips keep their own rate, up to sbMaxRate)
	sbMaxRate    = 48000 // clips are kept at their own rate up to this one
	sbGapMs      = 90    // silence between pieces
)

var (
	sbNameRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
	sbTokenRe = regexp.MustCompile(`:([A-Za-z0-9_-]{1,40}):`)
)

type sbEntry struct {
	Name    string  `json:"name"`
	Volume  float64 `json:"volume"` // 1 = as recorded
	Seconds float64 `json:"seconds"`
}

type soundbites struct {
	dir  string
	mu   sync.Mutex
	list map[string]sbEntry // key: lower-case name
}

func newSoundbites(dir string) *soundbites {
	s := &soundbites{dir: dir, list: map[string]sbEntry{}}
	if b, err := os.ReadFile(filepath.Join(dir, "index.json")); err == nil {
		var es []sbEntry
		if json.Unmarshal(b, &es) == nil {
			for _, e := range es {
				if sbNameRe.MatchString(e.Name) {
					if _, err := os.Stat(s.file(e.Name)); err == nil {
						s.list[strings.ToLower(e.Name)] = e
					}
				}
			}
		}
	}
	return s
}

func (s *soundbites) file(name string) string {
	return filepath.Join(s.dir, strings.ToLower(name)+".wav")
}

func (s *soundbites) entries() []sbEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sbEntry, 0, len(s.list))
	for _, e := range s.list {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (s *soundbites) has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.list[strings.ToLower(name)]
	return ok
}

func (s *soundbites) saveLocked() error {
	es := make([]sbEntry, 0, len(s.list))
	for _, e := range s.list {
		es = append(es, e)
	}
	sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
	b, _ := json.MarshalIndent(es, "", "  ")
	return os.WriteFile(filepath.Join(s.dir, "index.json"), b, 0o644)
}

// add stores a clip (a 16 bit WAV, mono or stereo, up to 48 kHz) under name. An existing clip of that name is replaced.
func (s *soundbites) add(name string, wav []byte, volume float64) error {
	if !sbNameRe.MatchString(name) {
		return errors.New("The name may only have letters, digits, - and _ (1 to 40 characters). Write it without the colons.")
	}
	rate, ch, pcm, err := parseWAVn(wav)
	if err != nil || rate < 8000 || rate > sbMaxRate {
		return errors.New("The clip could not be read.")
	}
	secs := float64(len(pcm)/(2*ch)) / float64(rate)
	if secs < 0.05 {
		return errors.New("The clip is empty.")
	}
	if secs > sbMaxSecs+0.5 {
		return errors.New("The clip is longer than 15 seconds. Use a shorter one.")
	}
	if !(volume > 0) || volume > 3 {
		volume = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.list[strings.ToLower(name)]; !exists && len(s.list) >= sbMaxEntries {
		return errors.New("The library is full (300 clips). Remove one first.")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.file(name), wav, 0o644); err != nil {
		return err
	}
	s.list[strings.ToLower(name)] = sbEntry{Name: name, Volume: math.Round(volume*100) / 100, Seconds: math.Round(secs*10) / 10}
	return s.saveLocked()
}

func (s *soundbites) remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.list[strings.ToLower(name)]; !ok {
		return
	}
	os.Remove(s.file(name))
	delete(s.list, strings.ToLower(name))
	s.saveLocked()
}

// clipAudio is a clip as it is played: its own rate and channel count, volume applied.
type clipAudio struct {
	rate, ch int
	pcm      []byte
}

func (c clipAudio) secs() float64 { return float64(len(c.pcm)/(2*c.ch)) / float64(c.rate) }

// audio returns the clip with its volume applied.
func (s *soundbites) audio(name string) (clipAudio, bool) {
	s.mu.Lock()
	e, ok := s.list[strings.ToLower(name)]
	s.mu.Unlock()
	if !ok {
		return clipAudio{}, false
	}
	b, err := os.ReadFile(s.file(name))
	if err != nil {
		return clipAudio{}, false
	}
	rate, ch, pcm, err := parseWAVn(b)
	if err != nil {
		return clipAudio{}, false
	}
	return clipAudio{rate, ch, gain(pcm, e.Volume)}, true
}

func gain(pcm []byte, g float64) []byte {
	if g == 1 {
		return pcm
	}
	out := make([]byte, len(pcm))
	for i := 0; i+1 < len(pcm); i += 2 {
		v := math.Round(float64(int16(binary.LittleEndian.Uint16(pcm[i:]))) * g)
		binary.LittleEndian.PutUint16(out[i:], uint16(int16(math.Max(-32768, math.Min(32767, v)))))
	}
	return out
}

// ---- splitting a message ------------------------------------------------------------

type piece struct {
	text string // spoken text, or
	clip string // the name of a clip
}

// splitEmotes cuts the text at every :NAME: that is in the library. Unknown :names: stay in the text.
// Only the first sbMaxClips clips are kept; further ones are dropped from the message.
func (s *soundbites) splitEmotes(text string) (parts []piece, clips int) {
	last := 0
	for _, m := range sbTokenRe.FindAllStringSubmatchIndex(text, -1) {
		name := text[m[2]:m[3]]
		if !s.has(name) {
			continue
		}
		if before := strings.TrimSpace(text[last:m[0]]); before != "" {
			parts = append(parts, piece{text: before})
		}
		if clips < sbMaxClips {
			parts = append(parts, piece{clip: name})
			clips++
		}
		last = m[1]
	}
	if rest := strings.TrimSpace(text[last:]); rest != "" {
		parts = append(parts, piece{text: rest})
	}
	return parts, clips
}

// hasWords tells whether a text piece has anything to say (letters or digits, not only punctuation).
func hasWords(t string) bool {
	for _, r := range t {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// ---- WAV helpers ---------------------------------------------------------------------

// parseWAV reads a 16 bit mono PCM WAV (what the engines make).
func parseWAV(b []byte) (rate int, pcm []byte, err error) {
	rate, ch, pcm, err := parseWAVn(b)
	if err == nil && ch != 1 {
		return 0, nil, errors.New("need 16 bit mono")
	}
	return rate, pcm, err
}

// parseWAVn reads a 16 bit PCM WAV with one or two channels (samples of the channels are interleaved).
func parseWAVn(b []byte) (rate, ch int, pcm []byte, err error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, 0, nil, errors.New("not a WAV")
	}
	var bits int
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:p+8]))
		body := p + 8
		switch id {
		case "fmt ":
			if size < 16 || body+16 > len(b) || binary.LittleEndian.Uint16(b[body:]) != 1 {
				return 0, 0, nil, errors.New("not PCM")
			}
			ch = int(binary.LittleEndian.Uint16(b[body+2:]))
			rate = int(binary.LittleEndian.Uint32(b[body+4:]))
			bits = int(binary.LittleEndian.Uint16(b[body+14:]))
		case "data":
			if (ch != 1 && ch != 2) || bits != 16 || rate <= 0 {
				return 0, 0, nil, errors.New("need 16 bit mono or stereo")
			}
			if size > len(b)-body {
				size = len(b) - body
			}
			return rate, ch, b[body : body+size&^(2*ch-1)], nil
		}
		p = body + size + size&1
	}
	return 0, 0, nil, errors.New("no audio data")
}

func buildWAV(rate int, pcm []byte) []byte { return buildWAVn(rate, 1, pcm) }

func buildWAVn(rate, ch int, pcm []byte) []byte {
	var buf bytes.Buffer
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(ch))
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*2*ch))
	binary.LittleEndian.PutUint16(h[32:], uint16(2*ch))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	buf.Write(h)
	buf.Write(pcm)
	return buf.Bytes()
}

// resamplePCM changes the rate with ffmpeg and keeps the channels. Used when clips and speech have different rates.
func resamplePCM(ctx context.Context, ffmpeg string, pcm []byte, ch, from, to int) ([]byte, error) {
	if ffmpeg == "" {
		return nil, errNoFFmpeg
	}
	dir, err := os.MkdirTemp("", "sb-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "in.wav"), filepath.Join(dir, "out.wav")
	if err := os.WriteFile(in, buildWAVn(from, ch, pcm), 0o600); err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-y", "-v", "error", "-i", in, "-ac", strconv.Itoa(ch), "-ar", strconv.Itoa(to), "-c:a", "pcm_s16le", "-f", "wav", out)
	hideWindow(c)
	if b, err := c.CombinedOutput(); err != nil {
		return nil, errors.New("could not change the clip's sample rate: " + lastLine(string(b)))
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	_, _, p, err := parseWAVn(b)
	return p, err
}

// toStereo repeats every mono sample in both channels.
func toStereo(pcm []byte) []byte {
	out := make([]byte, len(pcm)*2)
	for i := 0; i+1 < len(pcm); i += 2 {
		copy(out[i*2:], pcm[i:i+2])
		copy(out[i*2+2:], pcm[i:i+2])
	}
	return out
}

func silence(rate, ch, ms int) []byte { return make([]byte, rate*ms/1000*2*ch) }
