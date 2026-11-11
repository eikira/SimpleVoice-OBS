// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// sampleVoices are the processor engine voices that Setup can turn into sample voices for the
// graphics card engine. The names are the file names in the voices folder.
var sampleVoices = []string{"Heart", "Bella", "Nicole", "Aoede", "Kore", "Sarah", "Michael", "Fenrir", "Puck", "Emma", "Isabella", "George", "Fable"}

type voiceHash struct{ WAV, TXT string }

// inventory is written after a successful install. It lists what Setup placed, so that a later
// build can remove what it no longer ships. Voices are listed only with the checksums of the files
// as Setup made them: a voice that was renamed, replaced or edited is the user's and stays.
type inventory struct {
	Build  string               `json:"build"`
	Files  []string             `json:"files"`            // relative to the install folder
	Voices map[string]voiceHash `json:"voices"`           // sample voices Setup made
	Picked *[]string            `json:"picked,omitempty"` // the sample voices chosen last time; nil: never chosen
}

func (i *installer) inventoryPath() string { return filepath.Join(i.dir, "inventory.json") }

func (i *installer) loadInventory() (*inventory, bool) {
	b, err := os.ReadFile(i.inventoryPath())
	if err != nil {
		return nil, false
	}
	var inv inventory
	if json.Unmarshal(b, &inv) != nil || inv.Build == "" && inv.Files == nil && inv.Voices == nil {
		return nil, false
	}
	return &inv, true
}

func (i *installer) saveInventory(inv *inventory) error {
	b, err := json.MarshalIndent(inv, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(i.dir, 0o755); err != nil {
		return err
	}
	tmp := i.inventoryPath() + ".part"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, i.inventoryPath())
}

// shipped lists the files this build puts into the install folder, relative to it.
func (i *installer) shipped() []string {
	files := []string{appExeName, "Uninstall.exe", "LICENSE", "THIRD-PARTY-NOTICES.txt"}
	for _, f := range []string{kokoroModel, kokoroVoices} {
		if has(i.kokoroDir(), f) {
			files = append(files, filepath.ToSlash(filepath.Join("models", "kokoro", f)))
		}
	}
	return files
}

// inside turns a path from the inventory into a path in the install folder, or "" when it would
// point anywhere else.
func (i *installer) inside(rel string) string {
	if rel == "" || filepath.IsAbs(rel) {
		return ""
	}
	p := filepath.Join(i.dir, filepath.FromSlash(rel))
	if r, err := filepath.Rel(i.dir, p); err != nil || r == "." || strings.HasPrefix(r, "..") {
		return ""
	}
	return p
}

func fileHash(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (i *installer) voiceFiles(name string) (wav, txt string) {
	return filepath.Join(i.voicesDir(), name+".wav"), filepath.Join(i.voicesDir(), name+".txt")
}

func (i *installer) voiceExists(name string) bool {
	w, t := i.voiceFiles(name)
	return has(filepath.Dir(w), filepath.Base(w), filepath.Base(t))
}

// wantedVoices is the sample voices of this run. Without a choice it is all of them.
func (i *installer) wantedVoices() []string {
	if i.sel == nil || i.sel.Voices == nil {
		return sampleVoices
	}
	var out []string
	for _, v := range sampleVoices {
		for _, w := range i.sel.Voices {
			if strings.EqualFold(v, w) {
				out = append(out, v)
				break
			}
		}
	}
	return out
}

// processorAvailable: the sample voices are made by the processor engine, so it must be ticked or
// already installed.
func (i *installer) processorAvailable() bool { return i.selected().CPU || i.kokoroDone() }

// recordVoices remembers which sample voices Setup made, right when it made them, so that a
// later stop does not lose track of them.
func (i *installer) recordVoices(made []string) {
	inv, ok := i.loadInventory()
	if !ok {
		inv = &inventory{}
	}
	if inv.Voices == nil {
		inv.Voices = map[string]voiceHash{}
	}
	for _, n := range made {
		w, t := i.voiceFiles(n)
		inv.Voices[n] = voiceHash{fileHash(w), fileHash(t)}
	}
	if err := i.saveInventory(inv); err != nil {
		i.logf("Could not write the inventory: " + err.Error())
	}
}

// tidy runs after a successful install: it removes what the earlier build placed and this one does
// not, writes the new inventory, and removes sample voices that were unticked. With no readable
// inventory it removes nothing.
func (i *installer) tidy() {
	old, have := i.loadInventory()
	files := i.shipped()
	keep := map[string]bool{}
	for _, f := range files {
		keep[filepath.ToSlash(f)] = true
	}
	if have {
		for _, rel := range old.Files {
			if keep[filepath.ToSlash(rel)] {
				continue
			}
			p := i.inside(rel)
			if p == "" || p == i.voicesDir() || strings.HasPrefix(p, i.voicesDir()+string(filepath.Separator)) {
				continue
			}
			if os.Remove(p) == nil {
				i.logf("Removed a file the new build no longer needs: " + rel)
				os.Remove(filepath.Dir(p)) // only if now empty
			}
		}
	}
	inv := &inventory{Build: setupBuild, Files: files, Voices: map[string]voiceHash{}}
	if have {
		for n, h := range old.Voices {
			inv.Voices[n] = h
		}
		inv.Picked = old.Picked
	}
	if i.processorAvailable() {
		want := i.wantedVoices()
		picked := append([]string{}, want...)
		inv.Picked = &picked
		wanted := map[string]bool{}
		for _, n := range want {
			wanted[n] = true
		}
		for n, h := range inv.Voices {
			if wanted[n] {
				continue
			}
			w, t := i.voiceFiles(n)
			if fileHash(w) == h.WAV && fileHash(t) == h.TXT && h.WAV != "" {
				os.Remove(w)
				os.Remove(t)
				i.logf("Removed the sample voice " + n + " (not ticked).")
			} else if i.voiceExists(n) {
				i.logf("Kept " + n + ": it was changed after setup made it.")
			}
			delete(inv.Voices, n)
		}
	}
	if err := i.saveInventory(inv); err != nil {
		i.logf("Could not write the inventory: " + err.Error())
	}
}

type voiceInfo struct {
	Name     string `json:"name"`
	State    string `json:"state"` // "", "made" (by Setup, unticking removes it) or "yours"
	Selected bool   `json:"selected"`
	SizeMB   string `json:"size"`
}

// voiceList is what the setup page shows under the graphics card engine.
func (i *installer) voiceList() []voiceInfo {
	inv, have := i.loadInventory()
	out := []voiceInfo{}
	for _, n := range sampleVoices {
		v := voiceInfo{Name: n, SizeMB: "0.4", Selected: true}
		if i.voiceExists(n) {
			v.State = "yours"
			if have {
				if h, ok := inv.Voices[n]; ok && h.WAV != "" {
					v.State = "made"
				}
			}
		}
		if have && inv.Picked != nil {
			v.Selected = false
			for _, p := range *inv.Picked {
				if p == n {
					v.Selected = true
				}
			}
		}
		out = append(out, v)
	}
	return out
}
