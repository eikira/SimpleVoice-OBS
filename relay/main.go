// SimpleVoice OBS Relay: connects the VoiceStudio+SimpleVoice OBS Connector Chrome extension to OBS and
// speaks with its own OmniVoice engine. See README.
//
// Copyright (C) 2026 eikira.
// SPDX-License-Identifier: GPL-3.0-or-later
// Free software: you can redistribute it and/or modify it under the terms of the GNU General
// Public License v3 or later. It comes with NO WARRANTY. See the LICENSE file.
// Inspired by Social Stream Ninja.
package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed worker.py
var workerPy []byte

const version = "1.0.0"

var logOut io.Writer = os.Stdout

func logf(format string, args ...interface{}) {
	fmt.Fprintf(logOut, "["+time.Now().Format("15:04:05")+"] "+format+"\n", args...)
}

type config struct {
	host, port                string
	engineDir, voices, engine string
	python, ffmpeg            string
	steps                     int
}

// appDir is the folder the program runs from (where settings.txt normally lives too).
func appDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// settings.txt lives next to the program (PORT=8787 / HOST=127.0.0.1). If that folder cannot be
// written (for example Program Files), the settings are kept in the user's config folder instead.
func settingsPaths() (primary, fallback string) {
	if exe, err := os.Executable(); err == nil {
		primary = filepath.Join(filepath.Dir(exe), "settings.txt")
	}
	if d, err := os.UserConfigDir(); err == nil {
		fallback = filepath.Join(d, "SimpleVoice OBS", "settings.txt")
	}
	return
}

func parseSettings(path string, into map[string]string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, "="); i > 0 {
			into[strings.ToUpper(strings.TrimSpace(line[:i]))] = strings.TrimSpace(line[i+1:])
		}
	}
}

func readSettings() map[string]string {
	out := map[string]string{}
	p, f := settingsPaths()
	parseSettings(p, out)
	parseSettings(f, out) // the config-folder file only exists if the program folder was read-only
	return out
}

var settingsMu sync.Mutex

// writeSetting changes one KEY=value line, keeping the comments around it.
func writeSetting(key, value string) error {
	settingsMu.Lock() // two changes at once must not overwrite each other
	defer settingsMu.Unlock()
	try := func(path string) error {
		if path == "" {
			return fmt.Errorf("no path")
		}
		var lines []string
		if data, err := os.ReadFile(path); err == nil {
			lines = strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
		}
		found := false
		for i, l := range lines {
			t := strings.TrimSpace(l)
			if !strings.HasPrefix(t, "#") && strings.HasPrefix(strings.ToUpper(t), key+"=") {
				lines[i] = key + "=" + value
				found = true
			}
		}
		if !found {
			lines = append(lines, key+"="+value)
		}
		os.MkdirAll(filepath.Dir(path), 0o755)
		return os.WriteFile(path, []byte(strings.Join(lines, "\r\n")+"\r\n"), 0o644)
	}
	p, f := settingsPaths()
	if err := try(p); err == nil {
		os.Remove(f) // so there is only one place that decides
		return nil
	}
	return try(f)
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1024 && n <= 65535
}

func loadConfig(flagHost, flagPort string) config {
	s := readSettings()
	pick := func(flagVal, env, key, def string) string {
		if flagVal != "" {
			return flagVal
		}
		if v := os.Getenv(env); v != "" {
			return v
		}
		if v := s[key]; v != "" {
			return v
		}
		return def
	}
	steps, _ := strconv.Atoi(pick("", "STEPS", "STEPS", "32"))
	if steps < 4 || steps > 64 {
		steps = 32
	}
	eng := pick("", "ENGINEDIR", "ENGINEDIR", appDir())
	py := pick("", "PYTHON", "PYTHON", filepath.Join(eng, "runtime", "python", "python.exe"))
	return config{host: pick(flagHost, "HOST", "HOST", "127.0.0.1"), port: pick(flagPort, "PORT", "PORT", "8787"),
		engineDir: eng, python: py, ffmpeg: pick("", "FFMPEG", "FFMPEG", ""), steps: steps, voices: pick("", "VOICES", "VOICES", defaultVoicesDir()), engine: strings.ToLower(pick("", "ENGINE", "ENGINE", "auto"))}
}

// doStop asks a running relay (on this computer) to close. Returns the exit code.
func doStop(cfg config) int {
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+cfg.port+"/shutdown", nil)
	req.Header.Set("X-Relay-Stop", "1")
	c := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := c.Do(req)
	if err != nil {
		fmt.Println("The relay was not running.")
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Println("Something else is using port " + cfg.port + " (not this relay).")
		return 2
	}
	// wait until it is really gone
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+cfg.port, 300*time.Millisecond); err != nil {
			break
		} else {
			conn.Close()
		}
	}
	fmt.Println("The relay has been stopped.")
	return 0
}

func main() {
	showVersion := flag.Bool("version", false, "print the version")
	stop := flag.Bool("stop", false, "stop a running relay")
	setPort := flag.String("set-port", "", "save a new port (1024-65535) to settings.txt")
	restarted := flag.Bool("restarted", false, "internal: started by the port-change restart")
	openFolder := flag.Bool("open-folder", false, "open the folder this program runs from")
	host := flag.String("host", "", "address to listen on (default 127.0.0.1)")
	openPanel := flag.Bool("open-panel", false, "open the control panel in the browser once running")
	port := flag.String("port", "", "port to listen on (default 8787)")
	flag.Parse()
	cfg := loadConfig(*host, *port)

	if *showVersion {
		fmt.Println("SimpleVoice OBS Relay " + version)
		fmt.Println("Free and open source software (GPL-3.0). Inspired by Social Stream Ninja.")
		return
	}
	if *setPort != "" {
		if !validPort(*setPort) {
			fmt.Println("The port must be a number from 1024 to 65535.")
			os.Exit(2)
		}
		if err := writeSetting("PORT", *setPort); err != nil {
			fmt.Println("Could not save the setting:", err)
			os.Exit(1)
		}
		fmt.Println("Port saved: " + *setPort + ". Restart the relay. Then set the same address in the extension settings (Relay address) and use the new OBS URL.")
		return
	}
	if *stop {
		os.Exit(doStop(cfg))
	}
	if *openFolder {
		if err := openPath(appDir()); err != nil {
			fmt.Println("Could not open the folder:", err)
			fmt.Println("It is: " + appDir())
			os.Exit(1)
		}
		fmt.Println("Opened " + appDir())
		return
	}
	initLog()
	os.MkdirAll(cfg.voices, 0o755)
	script := filepath.Join(dataDir(), "worker.py")
	os.WriteFile(script, workerPy, 0o644)
	eng := newEngine(engineCfg{python: cfg.python, model: filepath.Join(cfg.engineDir, "models", "OmniVoice"), kokoro: filepath.Join(cfg.engineDir, "models", "kokoro"), script: script, asr: filepath.Join(cfg.engineDir, "models", "whisper-large-v3-turbo"), steps: cfg.steps, startWait: 5 * time.Minute})
	eng.setMode(cfg.engine)
	eng.setOptions(validLanguage(strings.ToLower(readSettings()["LANGUAGE"])), strings.EqualFold(readSettings()["NUMBERS"], "on"))
	r := newRelay(eng, voiceStore{dir: cfg.voices})
	r.ffmpeg = findFFmpeg(cfg.engineDir, cfg.ffmpeg)
	r.drafts = filepath.Join(dataDir(), "drafts")
	eng.start()
	var ln net.Listener
	var err error
	for try := 0; try < 40; try++ { // after a port change the old copy needs a moment to let go
		ln, err = net.Listen("tcp", net.JoinHostPort(cfg.host, cfg.port))
		if err == nil || !*restarted {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		if runningHere(cfg) { // started a second time (Start menu, shortcut): just show the panel
			openPath("http://127.0.0.1:" + cfg.port + "/")
			return
		}
		fatal("Port " + cfg.port + " is already in use by another program. Change the port in settings.txt.")
		os.Exit(1)
	}
	srv := &http.Server{Handler: r}
	go srv.Serve(ln)
	go func() {
		for {
			select {
			case <-time.After(15 * time.Second):
				r.hub.ping() // keeps OBS connections alive
			case <-r.quit:
				return
			}
		}
	}()
	if *openPanel {
		openPath("http://127.0.0.1:" + cfg.port + "/")
	}
	logf("SimpleVoice OBS Relay %s running on http://%s:%s", version, cfg.host, cfg.port)
	if cfg.host != "127.0.0.1" && cfg.host != "localhost" {
		logf("WARNING: listening beyond this PC. Anyone on your network who knows your ID can send speech.")
	}

	runUI(r, cfg) // blocks until the relay should close
	srv.Close()
	eng.stop()
	logf("Relay stopped.")
}

// defaultVoicesDir keeps voices next to the program (the install folder), where setup made it.
func defaultVoicesDir() string { return filepath.Join(appDir(), "voices") }

// runningHere tells if this relay is already answering on the port.
func runningHere(cfg config) bool {
	c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := c.Get("http://127.0.0.1:" + cfg.port + "/info")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var m struct{ Protocol int }
	return json.NewDecoder(resp.Body).Decode(&m) == nil && m.Protocol >= 2
}
