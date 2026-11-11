// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Any common recording is turned into the WAV the model wants with ffmpeg, which setup installs.

const maxClipSeconds = "21" // converting stops here, so a recording over the 20 second limit is noticed without converting all of it

// findFFmpeg looks for the ffmpeg that setup installed, then for one on the PATH.
func findFFmpeg(engineDir, override string) string {
	if override != "" {
		return override
	}
	m, _ := filepath.Glob(filepath.Join(engineDir, "runtime", "python", "Lib", "site-packages", "imageio_ffmpeg", "binaries", "ffmpeg*"))
	if len(m) > 0 {
		return m[0]
	}
	p, _ := exec.LookPath("ffmpeg")
	return p
}

// sniffAudio accepts only real audio containers: WAV, FLAC, MP3, AAC (ADTS), MP4/M4A, Ogg.
// ffmpeg also reads text playlists that point at other files; those are refused here.
func sniffAudio(b []byte) bool {
	switch {
	case len(b) < 12:
		return false
	case string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE", string(b[:4]) == "fLaC", string(b[:4]) == "OggS", string(b[:3]) == "ID3", string(b[4:8]) == "ftyp":
		return true
	}
	return b[0] == 0xFF && b[1]&0xE0 == 0xE0 // MP3 or AAC frame header
}

var errNoFFmpeg = errors.New("ffmpeg was not found. Run Setup again to install it.")

// toWAV converts a recording to mono 24 kHz 16-bit WAV. Only the first 21 seconds are read: that is enough to see that a recording is over the limit.
func toWAV(ctx context.Context, ffmpeg, in, out string) error {
	if ffmpeg == "" {
		return errNoFFmpeg
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-y", "-v", "error", "-i", in, "-vn", "-map_metadata", "-1",
		"-ac", "1", "-ar", "24000", "-t", maxClipSeconds, "-c:a", "pcm_s16le", "-f", "wav", out)
	hideWindow(c)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("converting the recording took too long")
		}
		return errors.New("This recording could not be read: " + strings.TrimSpace(lastLine(stderr.String())))
	}
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if k := strings.LastIndexByte(s, '\n'); k >= 0 {
		s = s[k+1:]
	}
	return s
}

// What the test sentence can be saved as. All are mono and 24 kHz; the lossy ones use a fixed 128 kbps.
type saveFormat struct {
	ext, encoder, container string
	lossy                   bool
}

var saveFormats = []saveFormat{
	{"wav", "", "", false},
	{"flac", "flac", "flac", false},
	{"mp3", "libmp3lame", "mp3", true},
	{"aac", "aac", "adts", true},
}

// savable lists the formats this ffmpeg can make. WAV needs no ffmpeg at all.
func savable(ctx context.Context, ffmpeg string) []string {
	out := []string{"wav"}
	if ffmpeg == "" {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-encoders")
	hideWindow(c)
	b, err := c.Output()
	if err != nil {
		return out
	}
	for _, f := range saveFormats[1:] {
		if strings.Contains(string(b), " "+f.encoder+" ") {
			out = append(out, f.ext)
		}
	}
	return out
}

// convertSpeech turns a WAV into one of the formats above.
func convertSpeech(ctx context.Context, ffmpeg string, wav []byte, ext string) ([]byte, error) {
	var f *saveFormat
	for i := range saveFormats {
		if saveFormats[i].ext == ext {
			f = &saveFormats[i]
		}
	}
	if f == nil || f.encoder == "" {
		return nil, errors.New("Choose MP3, WAV, FLAC or AAC.")
	}
	if ffmpeg == "" {
		return nil, errNoFFmpeg
	}
	if secs, err := wavSeconds(wav); err != nil || secs <= 0 {
		return nil, errors.New("This is not a WAV file.")
	}
	dir, err := os.MkdirTemp("", "ovr-save")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "in.wav"), filepath.Join(dir, "out."+f.ext)
	if err := os.WriteFile(in, wav, 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	args := []string{"-nostdin", "-y", "-v", "error", "-i", in, "-vn", "-map_metadata", "-1", "-ac", "1", "-ar", "24000", "-c:a", f.encoder}
	if f.lossy {
		args = append(args, "-b:a", "128k")
	}
	args = append(args, "-f", f.container, out)
	c := exec.CommandContext(ctx, ffmpeg, args...)
	hideWindow(c)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return nil, errors.New("The file could not be made: " + strings.TrimSpace(lastLine(stderr.String())))
	}
	return os.ReadFile(out)
}
