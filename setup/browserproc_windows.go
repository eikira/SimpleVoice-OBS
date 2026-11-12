// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// psKillByProfile stops every process whose command line contains the folder in SV_MATCH (the app
// window of one setup run: Edge and Chrome start several processes that all carry --user-data-dir=...).
const psKillByProfile = `if (-not $env:SV_MATCH) { exit }
Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -and $_.CommandLine.IndexOf($env:SV_MATCH, [StringComparison]::OrdinalIgnoreCase) -ge 0 } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`

// psKillOtherSetups stops every other SimpleVoice-OBS-Setup.exe (not this one) and every browser window
// that an earlier setup run left behind (their folder names start with SV_MATCH).
const psKillOtherSetups = `if (-not $env:SV_MATCH -or -not $env:SV_NAME) { exit }
$me = [int]$env:SV_PID
Get-CimInstance Win32_Process | Where-Object { $_.ProcessId -ne $me -and ( $_.Name -eq $env:SV_NAME -or ($_.CommandLine -and $_.CommandLine.IndexOf($env:SV_MATCH, [StringComparison]::OrdinalIgnoreCase) -ge 0) ) } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`

func runPS(script string, env ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	c.Env = append(os.Environ(), env...)
	hideWindow(c)
	c.Run()
}

// closeBrowser ends the app window and everything it started, so nothing keeps the temporary profile folder.
func closeBrowser(profile string) {
	if profile != "" {
		runPS(psKillByProfile, "SV_MATCH="+profile)
	}
}

// endOtherSetups makes sure no earlier SimpleVoice-OBS-Setup.exe (and no window of one) is still around.
func endOtherSetups() {
	runPS(psKillOtherSetups, "SV_PID="+strconv.Itoa(os.Getpid()), "SV_MATCH="+profilePrefix(), "SV_NAME="+productionName)
}
