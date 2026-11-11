// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// A voice is a short WAV of the speaker plus a .txt with the words spoken in it, same name:
// voices\Anna.wav and voices\Anna.txt. The transcript is required because without it the
// model would have to download a second (speech recognition) model by itself.

const (
	maxWavBytes   = 10 << 20
	maxTranscript = 2000
	maxSecs       = 20 // longer samples make the model read the end of the sample aloud and slow the speech
)

var nameRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} _.-]{0,59}$`)
var reserved = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])(\..*)?$`)

type voiceInfo struct {
	Name    string  `json:"name"`
	Seconds float64 `json:"seconds"`
	Text    string  `json:"transcript"`
	Note    string  `json:"note,omitempty"` // why this voice cannot be used or is not ideal
	Usable  bool    `json:"usable"`
}

type voiceStore struct{ dir string }

func validName(n string) bool {
	return nameRe.MatchString(n) && !strings.HasSuffix(n, ".") && !strings.HasSuffix(n, " ") && !reserved.MatchString(n)
}

func (s voiceStore) wavPath(n string) string { return filepath.Join(s.dir, n+".wav") }
func (s voiceStore) txtPath(n string) string { return filepath.Join(s.dir, n+".txt") }

func readTranscript(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(b) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(string(b), "\xef\xbb\xbf"))
}

// list reads the folder every time, so voices copied in by hand show up at once.
func (s voiceStore) list() []voiceInfo {
	entries, _ := os.ReadDir(s.dir)
	out := []voiceInfo{}
	for _, e := range entries {
		n, ok := strings.CutSuffix(e.Name(), ".wav")
		if !ok {
			n, ok = strings.CutSuffix(e.Name(), ".WAV")
		}
		if !ok || e.IsDir() || !validName(n) {
			continue
		}
		v := voiceInfo{Name: n, Text: readTranscript(s.txtPath(n))}
		b, err := os.ReadFile(s.wavPath(n))
		if err == nil {
			v.Seconds, err = wavSeconds(b)
		}
		switch {
		case err != nil:
			v.Note = "This file is not a usable WAV."
		case v.Text == "":
			v.Note = "No transcript yet. Click Transcribe, or add " + n + ".txt with the words spoken."
		case v.Seconds > maxSecs+0.5:
			v.Note = fmt.Sprintf("%.0f seconds is too long. The limit is %d seconds. Import a shorter recording of this voice.", v.Seconds, maxSecs)
		case v.Seconds < 1:
			v.Note = "Too short. Use at least 3 seconds."
		default:
			v.Usable = true
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

var errNoVoice = errors.New("unknown voice")

// find matches a voice name from a request. Only names from list() are ever turned into paths.
func (s voiceStore) find(name string) (wav, text string, err error) {
	for _, v := range s.list() {
		if strings.EqualFold(v.Name, name) {
			if v.Text == "" {
				return "", "", errors.New(v.Note)
			}
			return s.wavPath(v.Name), v.Text, nil
		}
	}
	return "", "", errNoVoice
}

func (s voiceStore) add(name, transcript string, wav []byte) error {
	transcript = strings.TrimSpace(transcript)
	switch {
	case !validName(name):
		return errors.New("The name may contain letters, numbers, space, dot, dash and underscore (60 characters at most).")
	case transcript == "" || !utf8.ValidString(transcript) || utf8.RuneCountInString(transcript) > maxTranscript:
		return errors.New("Write the words spoken in the recording (up to 2000 characters).")
	case len(wav) > maxWavBytes:
		return errors.New("The WAV file is larger than 10 MB. Use a recording of up to 20 seconds.")
	}
	secs, err := wavSeconds(wav)
	if err != nil {
		return errors.New("This is not a WAV file. Convert the recording to WAV first.")
	}
	if secs < 1 {
		return errors.New("The recording is too short. Use at least 3 seconds.")
	}
	if secs > maxSecs+0.5 {
		return fmt.Errorf("The recording is %.0f seconds. The limit is %d seconds.", secs, maxSecs)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(s.wavPath(name)); err == nil {
		return errors.New("A voice with this name exists. Delete it first or pick another name.")
	}
	if err := writeAtomic(s.wavPath(name), wav); err != nil {
		return err
	}
	if err := writeAtomic(s.txtPath(name), []byte(transcript+"\n")); err != nil {
		os.Remove(s.wavPath(name))
		return err
	}
	return nil
}

func (s voiceStore) remove(name string) error {
	for _, v := range s.list() {
		if v.Name == name {
			os.Remove(s.txtPath(name))
			return os.Remove(s.wavPath(name))
		}
	}
	return errNoVoice
}

// rename gives a voice a new name. The recording and its words stay as they are.
func (s voiceStore) rename(old, name string) error {
	if !validName(name) {
		return errors.New("The name may contain letters, numbers, space, dot, dash and underscore (60 characters at most).")
	}
	found := false
	for _, v := range s.list() {
		if v.Name == old {
			found = true
		} else if strings.EqualFold(v.Name, name) {
			return errors.New("A voice with this name exists. Pick another name.")
		}
	}
	if !found {
		return errNoVoice
	}
	if old == name {
		return nil
	}
	if err := os.Rename(s.wavPath(old), s.wavPath(name)); err != nil {
		return errors.New("The voice could not be renamed.")
	}
	if err := os.Rename(s.txtPath(old), s.txtPath(name)); err != nil && !os.IsNotExist(err) {
		os.Rename(s.wavPath(name), s.wavPath(old))
		return errors.New("The voice could not be renamed.")
	}
	return nil
}

// wavFor returns the WAV path of a voice by name, or "" if there is none.
func (s voiceStore) wavFor(name string) string {
	for _, v := range s.list() {
		if v.Name == name {
			return s.wavPath(name)
		}
	}
	return ""
}

func (s voiceStore) setTranscript(name, text string) error {
	text = strings.TrimSpace(text)
	if s.wavFor(name) == "" {
		return errNoVoice
	}
	if text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxTranscript {
		return errors.New("Write the words spoken in the recording (up to 2000 characters).")
	}
	return writeAtomic(s.txtPath(name), []byte(text+"\n"))
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// wavSeconds reads the length from the WAV header. Any chunk layout is accepted.
func wavSeconds(b []byte) (float64, error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, errors.New("not a WAV")
	}
	var byteRate uint32
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:p+8]))
		body := p + 8
		switch id {
		case "fmt ":
			if size < 16 || body+16 > len(b) {
				return 0, errors.New("bad fmt")
			}
			byteRate = binary.LittleEndian.Uint32(b[body+8 : body+12])
		case "data":
			if byteRate == 0 {
				return 0, errors.New("data before fmt")
			}
			if size > len(b)-body { // streamed files may claim more than they hold
				size = len(b) - body
			}
			return float64(size) / float64(byteRate), nil
		}
		p = body + size + size&1
	}
	return 0, errors.New("no audio data")
}
