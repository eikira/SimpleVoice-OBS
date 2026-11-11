//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
)

// openPath shows a folder in the system's file manager.
func openPath(p string) error {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	c := exec.Command(cmd, p)
	if err := c.Start(); err != nil {
		return err
	}
	go c.Wait()
	return nil
}

func initLog() {}

func fatal(msg string) { fmt.Fprintln(os.Stderr, msg) }

// No icon on macOS/Linux: run until stopped (--stop, Ctrl+C or a signal).
func runUI(r *relay, cfg config) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-r.quit:
	case <-sig:
	}
}

// dataDir holds files the program writes for itself (the engine script).
func dataDir() string {
	d, _ := os.UserConfigDir()
	d = filepath.Join(d, "SimpleVoiceOBS")
	os.MkdirAll(d, 0o755)
	return d
}
