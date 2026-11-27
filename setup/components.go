// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// selection is what the user ticked on the setup page. A nil selection means everything.
type selection struct {
	CPU, GPU, ASR bool
	Voices        []string // sample voices to carry over to the graphics card engine; nil means all
}

func (i *installer) selected() selection {
	if i.sel == nil {
		return selection{CPU: true, GPU: true, ASR: true}
	}
	return *i.sel
}

// needLibs: PyTorch and the OmniVoice code are used by the graphics card engine and also by the
// transcription model, so either one needs them.
func (s selection) needLibs() bool { return s.GPU || s.ASR }

// Sizes are estimates in MB (PyTorch with its CUDA libraries is most of it). They are shown
// before installing; the real files can differ by a few percent.
type compInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Detail    string `json:"detail"`
	Required  bool   `json:"required"`
	DiskMB    int    `json:"diskMB"`
	LoadMB    int    `json:"downloadMB"`
	Installed bool   `json:"installed"`
}

func (i *installer) libsDone() bool {
	return i.pkgDone("torch", pickStr(isUnix, "2.8.0", "2.8.0+cu128")) && i.pkgDone("omnivoice", "0.2.1")
}

func (i *installer) components() (list []compInfo, libs compInfo) {
	libs = compInfo{ID: "libs", Name: "PyTorch and OmniVoice code", DiskMB: 6300, LoadMB: 3400, Installed: i.libsDone()}
	if isMac { // the Mac build of PyTorch has no CUDA libraries; it and its Python packages are smaller
		libs.DiskMB, libs.LoadMB = 2600, 900
	}
	list = []compInfo{
		{ID: "base", Name: "Basic program", Detail: "SimpleVoice OBS Relay with soundbites (sound clips for emotes), Python, audio conversion (ffmpeg). Always installed. On its own it plays sound clips but does not read text aloud.",
			Required: true, DiskMB: 250, LoadMB: 100, Installed: i.pkgDone("imageio_ffmpeg", "0.8.1") && has(i.dir, appExeName)},
		{ID: "cpu", Name: "Processor engine", Detail: "Works on any PC. Built-in voices, no cloning. Also makes the sample voices you tick below for the graphics card engine.",
			DiskMB: 220, LoadMB: 160, Installed: i.kokoroDone()},
		{ID: "gpu", Name: "Graphics card engine", Detail: "Best quality and voice cloning. Minimum: NVIDIA GTX 16 series, RTX 20 series or newer, with 6 GB video memory. GTX 16 series cards work but are very slow and not recommended; RTX 20 series or newer is the sensible choice. Older cards (GTX 10 series and before) cannot use it. Includes PyTorch.",
			DiskMB: libs.DiskMB + 3000, LoadMB: libs.LoadMB + 3000, Installed: i.libsDone() && i.modelDone()},
		{ID: "asr", Name: "Transcription", Detail: "Writes down the words of a new voice recording for you. Needs PyTorch: adds about 6.3 GB if the graphics card engine is not ticked.",
			DiskMB: 1600, LoadMB: 1600, Installed: i.libsDone() && i.asrDone()},
	}
	if isMac {
		for k := range list {
			switch list[k].ID {
			case "base":
				list[k].Detail = "SimpleVoice OBS Relay with soundbites (sound clips for emotes), Python, audio conversion (ffmpeg). Always installed. On its own it plays sound clips but does not read text aloud."
			case "cpu":
				list[k].Detail = "Built-in voices, no cloning. Also makes the sample voices you tick below for the cloning engine."
			case "gpu":
				list[k].Name = "Cloning engine (Apple GPU)"
				list[k].Detail = "Best quality and voice cloning, runs on the Apple GPU of your M-series chip. Needs 16 GB of memory or more to be comfortable."
			case "asr":
				list[k].Detail = "Writes down the words of a new voice recording for you. Needs PyTorch: adds about 2.6 GB if the cloning engine is not ticked."
			}
		}
	}
	return list, libs
}

// planned adds up what a selection would still download and write, leaving out what is installed.
func (i *installer) planned(s selection) (diskMB, loadMB int) {
	list, libs := i.components()
	add := func(c compInfo) {
		if !c.Installed {
			diskMB += c.DiskMB
			loadMB += c.LoadMB
		}
	}
	for _, c := range list {
		switch {
		case c.ID == "base", c.ID == "cpu" && s.CPU, c.ID == "asr" && s.ASR, c.ID == "gpu" && s.GPU:
			if c.ID == "gpu" {
				// the graphics card engine line includes the libraries; count them once
				m := compInfo{DiskMB: 3000, LoadMB: 3000, Installed: i.modelDone()}
				add(m)
				add(libs)
			} else if c.ID == "asr" {
				add(compInfo{DiskMB: 1600, LoadMB: 1600, Installed: i.asrDone()})
				if !s.GPU {
					add(libs)
				}
			} else {
				add(c)
			}
		}
	}
	return
}

type hardware struct {
	GPU    bool    `json:"gpuFound"`  // an NVIDIA card was found
	Usable bool    `json:"gpuUsable"` // and this build's PyTorch can use it
	Name   string  `json:"gpuName"`
	VRAMMB int     `json:"vramMB"`
	Cap    float64 `json:"capability"` // compute capability, 0 when the driver does not say
	Tier   string  `json:"tier"`       // none, unsupported, low, ok, good
	Info   string  `json:"info"`       // the rough guide shown on mouseover
	OS     string  `json:"os"`
	FreeMB int     `json:"freeMB"`
}

// minCapability: the PyTorch build that setup installs (2.8.0 with CUDA 12.8) contains code for
// compute capability 7.0 and newer (Turing, RTX 20 series, and up). GTX 10 (Pascal) is older.
const minCapability = 7.0

// isGTX16 reports the GTX 16 series (1650, 1660 and their Super/Ti versions): they run PyTorch, but slowly.
func isGTX16(name string) bool {
	n := strings.ToUpper(name)
	return strings.Contains(n, "GTX 16") || strings.Contains(n, "GTX16")
}

// rate turns what nvidia-smi said into a rough guide. It is a guess from the card's generation
// and video memory, not a measurement.
func rate(name string, vramMB int, capability float64) (usable bool, tier, info string) {
	card := name
	if card == "" {
		return false, "none", "No NVIDIA graphics card was found."
	}
	if vramMB > 0 {
		card += fmt.Sprintf(", %.0f GB video memory", float64(vramMB)/1024)
	}
	const tail = " Rough guide only: it is estimated from the card's generation and memory, not measured."
	switch {
	case isGTX16(name) && (capability <= 0 || capability >= minCapability):
		return true, "low", card + ". GTX 16 series cards work, but they are very slow with this engine. This is not recommended: a new voice or a long message can take a long time. The processor engine is faster to start with." + tail
	case capability > 0 && capability < minCapability:
		return false, "unsupported", card + ". This card is too old for the PyTorch build that setup installs (minimum: GTX 16 series, RTX 20 series or newer). The processor engine will be used." + tail
	case vramMB > 0 && vramMB < minVRAMMiB:
		return true, "low", card + ". Less than 6 GB of video memory: the graphics card engine may not load or may be slow. The processor engine is the safe choice." + tail
	case vramMB > 0 && vramMB < 8000:
		return true, "ok", card + ". Should work, with little room left for other programs on the card." + tail
	}
	return true, "good", card + ". Should work comfortably." + tail
}

// detectGPU asks nvidia-smi, which the NVIDIA driver installs. Older drivers do not know the
// compute_cap field; then the generation is unknown (0) and the card is given the benefit of the doubt.
func detectGPU() hardware {
	if isMac {
		return detectMac()
	}
	h := detectNvidia()
	if isLinux {
		h.OS = "linux"
	}
	return h
}

func detectNvidia() hardware {
	query := func(fields string) []string {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu="+fields, "--format=csv,noheader,nounits")
		hideWindow(c)
		out, err := c.Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n")
	}
	lines := query("name,memory.total,compute_cap")
	if lines == nil {
		lines = query("name,memory.total")
	}
	h := hardware{Tier: "none"}
	h.Info = "No NVIDIA graphics card was found."
	best := -1
	for _, l := range lines { // several cards: use the one with the most memory
		p := strings.Split(l, ",")
		if len(p) < 2 {
			continue
		}
		mem, _ := strconv.Atoi(strings.TrimSpace(p[1]))
		var capb float64
		if len(p) > 2 {
			capb, _ = strconv.ParseFloat(strings.TrimSpace(p[2]), 64)
		}
		if mem > best {
			best, h.Name, h.VRAMMB, h.Cap = mem, strings.TrimSpace(p[0]), mem, capb
		}
	}
	if best >= 0 {
		h.GPU = true
		h.Usable, h.Tier, h.Info = rate(h.Name, h.VRAMMB, h.Cap)
	}
	return h
}

// detectMac: an Apple Silicon Mac always has a GPU that PyTorch can use (Metal). Memory is shared
// with the system, so the rating follows the installed memory.
func detectMac() hardware {
	h := hardware{OS: "darwin", Tier: "none"}
	if macCheckMachine() != nil {
		h.Info = "This Mac has no Apple Silicon chip (M1 or newer)."
		return h
	}
	h.GPU, h.Usable, h.Name = true, true, "Apple Silicon"
	if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		h.Name = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		if b, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
			h.VRAMMB = int(b >> 20)
		}
	}
	const tail = " Rough guide only."
	switch {
	case h.VRAMMB > 0 && h.VRAMMB < 12000:
		h.Tier, h.Info = "low", fmt.Sprintf("%s, %.0f GB memory. Less than 16 GB: the cloning engine may be slow or run out of memory. The processor engine is the safe choice.", h.Name, float64(h.VRAMMB)/1024)+tail
	default:
		h.Tier, h.Info = "good", fmt.Sprintf("%s, %.0f GB memory. Should work.", h.Name, float64(h.VRAMMB)/1024)+tail
	}
	return h
}
