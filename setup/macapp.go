// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	macBundleName = "SimpleVoice OBS.app"
	macAgentLabel = "com.simplevoiceobs.relay"
)

func userHome() string { h, _ := os.UserHomeDir(); return h }

func (i *installer) macBundle() string {
	return filepath.Join(userHome(), "Applications", macBundleName)
}
func (i *installer) macDesktopLink() string {
	return filepath.Join(userHome(), "Desktop", macBundleName)
}
func (i *installer) macAgent() string {
	return filepath.Join(userHome(), "Library", "LaunchAgents", macAgentLabel+".plist")
}

func (i *installer) linuxMenu() string {
	return filepath.Join(userHome(), ".local", "share", "applications", "simplevoice-obs.desktop")
}
func (i *installer) linuxDesktop() string {
	return filepath.Join(userHome(), "Desktop", "SimpleVoice OBS.desktop")
}
func (i *installer) linuxAutostart() string {
	return filepath.Join(userHome(), ".config", "autostart", "simplevoice-obs.desktop")
}

// unixOutside lists what setup makes outside the install folder on a Mac or on Linux.
func (i *installer) unixOutside() []string {
	switch {
	case isMac:
		return []string{i.macBundle(), i.macDesktopLink(), i.macAgent()}
	case isLinux:
		return []string{i.linuxMenu(), i.linuxDesktop(), i.linuxAutostart()}
	}
	return nil
}

func (i *installer) registerUnix() error {
	if isMac {
		return i.registerMac()
	}
	return i.registerLinux()
}

func (i *installer) unregisterUnix() {
	for _, p := range i.unixOutside() {
		os.RemoveAll(p)
	}
}

// desktopEntry is a freedesktop.org launcher file. Exec has the program quoted for the format.
func (i *installer) desktopEntry(args string) string {
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", "$", `\$`).Replace(i.appExe())
	return "[Desktop Entry]\nType=Application\nName=SimpleVoice OBS\nComment=Voices for OBS chat commands\n" +
		"Exec=\"" + q + "\"" + args + "\nTerminal=false\nCategories=AudioVideo;Audio;\n"
}

// registerLinux makes the applications-menu entry, the optional Desktop entry and the optional
// start-at-login entry.
func (i *installer) registerLinux() error {
	write := func(path, text string, mode os.FileMode) error {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(text), mode)
	}
	if err := write(i.linuxMenu(), i.desktopEntry(" --open-panel"), 0o755); err != nil {
		return fmt.Errorf("menu entry: %w", err)
	}
	if i.opts.Desktop {
		if _, err := os.Stat(filepath.Dir(i.linuxDesktop())); err == nil {
			if err := write(i.linuxDesktop(), i.desktopEntry(" --open-panel"), 0o755); err != nil {
				i.logf("The desktop shortcut could not be made (" + err.Error() + "). SimpleVoice OBS is in your applications menu.")
			}
		}
	}
	if i.opts.Autostart {
		if err := write(i.linuxAutostart(), i.desktopEntry(""), 0o644); err != nil {
			return fmt.Errorf("start at login: %w", err)
		}
	}
	return nil
}

func xmlEsc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

const infoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleName</key><string>SimpleVoice OBS</string>
<key>CFBundleDisplayName</key><string>SimpleVoice OBS</string>
<key>CFBundleIdentifier</key><string>com.simplevoiceobs.launcher</string>
<key>CFBundleExecutable</key><string>SimpleVoiceOBS</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleVersion</key><string>%s</string>
<key>CFBundleShortVersionString</key><string>%s</string>
<key>LSUIElement</key><true/>
</dict></plist>
`

// registerMac makes a small launcher app (Launchpad and Spotlight find it), the optional Desktop
// entry, and the optional start-at-login item.
func (i *installer) registerMac() error {
	bundle := i.macBundle()
	os.RemoveAll(bundle)
	if err := os.MkdirAll(filepath.Join(bundle, "Contents", "MacOS"), 0o755); err != nil {
		return fmt.Errorf("launcher app: %w", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte(fmt.Sprintf(infoPlist, setupBuild, setupBuild)), 0o644); err != nil {
		return err
	}
	script := "#!/bin/sh\nnohup " + shQuote(i.appExe()) + " --open-panel >/dev/null 2>&1 &\n"
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "MacOS", "SimpleVoiceOBS"), []byte(script), 0o755); err != nil {
		return err
	}
	if i.opts.Desktop {
		os.Remove(i.macDesktopLink())
		if err := os.Symlink(bundle, i.macDesktopLink()); err != nil { // the Desktop may need a permission; not worth failing for
			i.logf("The desktop shortcut could not be made (" + err.Error() + "). SimpleVoice OBS is in Launchpad and in your Applications folder.")
		}
	}
	if i.opts.Autostart {
		plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>` + macAgentLabel + `</string>
<key>ProgramArguments</key><array><string>` + xmlEsc(i.appExe()) + `</string></array>
<key>RunAtLoad</key><true/>
</dict></plist>
`
		if err := os.MkdirAll(filepath.Dir(i.macAgent()), 0o755); err != nil {
			return fmt.Errorf("start at login: %w", err)
		}
		if err := os.WriteFile(i.macAgent(), []byte(plist), 0o644); err != nil {
			return fmt.Errorf("start at login: %w", err)
		}
	}
	return nil
}
