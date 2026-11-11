// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// The relay program itself travels inside setup.exe, so one file installs everything.
//
//go:embed all:payload
var payload embed.FS

const (
	appName = "SimpleVoice OBS"
	// setupBuild numbers the test builds of the installer. It shows in the window, the log, the Apps list and the file name.
	setupBuild = "1.0.0"
	appExeName = "SimpleVoiceOBSRelay.exe"
	regUninst  = `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\SimpleVoiceOBS`
	runValue   = "SimpleVoiceOBS"
	regRun     = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
)

// options are the choices on the setup page.
type options struct{ Desktop, Autostart bool }

func (i *installer) appExe() string       { return filepath.Join(i.dir, appExeName) }
func (i *installer) uninstallExe() string { return filepath.Join(i.dir, "Uninstall.exe") }

// ownedFiles are the files setup puts into the install folder. voices\ and anything else the
// user adds there are never touched.
func (i *installer) ownedFiles() []string {
	return []string{i.appExe(), i.uninstallExe(), filepath.Join(i.dir, "LICENSE"),
		filepath.Join(i.dir, "THIRD-PARTY-NOTICES.txt"), filepath.Join(i.dir, "settings.txt"), i.inventoryPath(),
	}
}

// ownedOutside are folders the program makes outside the install folder (its log and engine script).
func (i *installer) ownedOutside() []string {
	if i.appData == "" {
		return nil
	}
	return []string{i.appData}
}

// stopApp asks a running relay to close, so its files can be replaced or removed.
func (i *installer) stopApp(ctx context.Context) {
	if _, err := os.Stat(i.appExe()); err == nil {
		i.shell(ctx, nil, func(string) {}, i.appExe(), "--stop")
	}
}

// installProgram puts the relay program, the licence texts and the uninstaller into the folder.
func (i *installer) installProgram(ctx context.Context) error {
	i.stopApp(ctx)
	if err := os.MkdirAll(i.dir, 0o755); err != nil {
		return err
	}
	for _, f := range []struct{ from, to string }{
		{"payload/" + appExeName, i.appExe()},
		{"payload/LICENSE", filepath.Join(i.dir, "LICENSE")},
		{"payload/THIRD-PARTY-NOTICES.txt", filepath.Join(i.dir, "THIRD-PARTY-NOTICES.txt")},
	} {
		b, err := payload.ReadFile(f.from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(f.to, b, 0o755); err != nil {
			return err
		}
	}
	// The uninstaller is this very program; it opens in uninstall mode (Windows Apps list).
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if sameFile(self, i.uninstallExe()) {
		return nil
	}
	return copyFile(self, i.uninstallExe())
}

func sameFile(a, b string) bool {
	x, err1 := os.Stat(a)
	y, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(x, y)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// The Start menu entry always; the desktop one only if chosen. Windows knows where the user's
// folders really are (OneDrive can move the desktop), so PowerShell is asked for them.
const shortcutScript = `$ErrorActionPreference = 'Stop'
$ws = New-Object -ComObject WScript.Shell
$exe = $env:OV_EXE
$places = @(Join-Path ([Environment]::GetFolderPath('Programs')) 'SimpleVoice OBS.lnk')
$places += Join-Path ([Environment]::GetFolderPath('Desktop')) 'SimpleVoice OBS.lnk'
foreach ($p in $places) {
  if ($env:OV_ACTION -eq 'remove') { Remove-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue; continue }
  if ($p -eq $places[1] -and $env:OV_DESKTOP -ne '1') { continue }
  $l = $ws.CreateShortcut($p)
  $l.TargetPath = $exe
  $l.Arguments = '--open-panel'
  $l.WorkingDirectory = Split-Path $exe
  $l.IconLocation = $exe
  $l.Description = 'SimpleVoice OBS'
  $l.Save()
}
`

func (i *installer) shortcuts(ctx context.Context, action string) error {
	f, err := os.CreateTemp("", "ov-shortcuts-*.ps1")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	f.WriteString(shortcutScript)
	f.Close()
	d := "0"
	if i.opts.Desktop {
		d = "1"
	}
	env := append(os.Environ(), "OV_EXE="+i.appExe(), "OV_ACTION="+action, "OV_DESKTOP="+d)
	return i.shell(ctx, env, i.logf, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", f.Name())
}

func (i *installer) reg(ctx context.Context, key, name, value string) error {
	return i.shell(ctx, nil, func(string) {}, "reg.exe", "add", key, "/v", name, "/t", "REG_SZ", "/d", value, "/f")
}

// register adds the entry in Windows' list of installed apps, the shortcuts, and the optional
// start with Windows.
func (i *installer) register(ctx context.Context) error {
	if err := i.shortcuts(ctx, "create"); err != nil {
		return fmt.Errorf("shortcuts: %w", err)
	}
	for _, kv := range [][2]string{{"DisplayName", appName}, {"DisplayVersion", setupBuild}, {"Publisher", "eikira"},
		{"DisplayIcon", i.appExe()}, {"InstallLocation", i.dir}, {"UninstallString", `"` + i.uninstallExe() + `" --uninstall`}} {
		if err := i.reg(ctx, regUninst, kv[0], kv[1]); err != nil {
			return fmt.Errorf("Windows app list: %w", err)
		}
	}
	if i.opts.Autostart {
		if err := i.reg(ctx, regRun, runValue, `"`+i.appExe()+`"`); err != nil {
			return fmt.Errorf("start with Windows: %w", err)
		}
	}
	return nil
}

// unregister removes everything register made. Missing entries are fine.
func (i *installer) unregister(ctx context.Context) {
	i.shortcuts(ctx, "remove")
	i.shell(ctx, nil, func(string) {}, "reg.exe", "delete", regUninst, "/f")
	i.shell(ctx, nil, func(string) {}, "reg.exe", "delete", regRun, "/v", runValue, "/f")
}

// launch starts the relay and opens its control panel. The program keeps running after setup ends.
func (i *installer) launch() error {
	if _, err := os.Stat(i.appExe()); err != nil {
		return errors.New("SimpleVoice OBS is not installed")
	}
	c := exec.Command(i.appExe(), "--open-panel")
	c.Dir = i.dir
	if err := c.Start(); err != nil {
		return err
	}
	go c.Wait()
	return nil
}

// selfDelete removes the uninstaller after it has quit (a running program cannot delete itself).
func selfDelete(dir string) {
	self, err := os.Executable()
	if err != nil || filepath.Dir(self) != dir {
		return
	}
	script := `ping 127.0.0.1 -n 4 >nul & del /f /q "` + self + `" & rmdir "` + dir + `"`
	c := exec.Command("cmd.exe", "/c", script)
	hideWindow(c)
	c.Start()
}
