// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func asMac(t *testing.T) {
	old, oldL, oldX, oldName, oldU, oldA := isMac, isLinux, isUnix, appExeName, uninstallName, macArch
	isMac, isLinux, isUnix, appExeName, uninstallName, macArch = true, false, true, "SimpleVoiceOBSRelay", "Uninstall", "arm64"
	t.Cleanup(func() {
		isMac, isLinux, isUnix, appExeName, uninstallName, macArch = old, oldL, oldX, oldName, oldU, oldA
	})
}

func makeTgz(t *testing.T, entries []tar.Header, bodies map[string]string) string {
	p := filepath.Join(t.TempDir(), "x.tar.gz")
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		h := h
		h.Size = int64(len(bodies[h.Name]))
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(bodies[h.Name]))
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
	return p
}

func TestUntarGzKeepsLinksAndRefusesEscapes(t *testing.T) {
	src := makeTgz(t, []tar.Header{
		{Name: "python/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "python/bin/python3.11", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "python/bin/python3", Typeflag: tar.TypeSymlink, Linkname: "python3.11"},
		{Name: "python/bin/py", Typeflag: tar.TypeLink, Linkname: "python/bin/python3.11"},
	}, map[string]string{"python/bin/python3.11": "bin"})
	dst := t.TempDir()
	if err := untarGz(src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "python", "bin", "python3"))
	if err != nil || string(b) != "bin" {
		t.Fatalf("link not followed: %v %q", err, b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "python", "bin", "py")); string(b) != "bin" {
		t.Fatal("hard link missing")
	}
	fi, _ := os.Stat(filepath.Join(dst, "python", "bin", "python3.11"))
	if fi.Mode()&0o100 == 0 {
		t.Fatal("not executable")
	}
	for _, bad := range []tar.Header{
		{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "python/l", Typeflag: tar.TypeSymlink, Linkname: "../../../etc/passwd"},
		{Name: "python/h", Typeflag: tar.TypeLink, Linkname: "../../x"},
	} {
		if err := untarGz(makeTgz(t, []tar.Header{bad}, map[string]string{"../evil": "x"}), t.TempDir()); err == nil {
			t.Errorf("accepted %s", bad.Name)
		}
	}
}

func TestMacPaths(t *testing.T) {
	asMac(t)
	i := newInstaller("/x/SimpleVoice-OBS", func(string) {})
	if got := i.pythonExe(); got != "/x/SimpleVoice-OBS/runtime/python/bin/python3" {
		t.Fatal(got)
	}
	if got := i.site(); got != "/x/SimpleVoice-OBS/runtime/python/lib/python3.11/site-packages" {
		t.Fatal(got)
	}
	if i.pythonURL != macPythonURL || i.pythonSHA != macPythonSHA {
		t.Fatal("python download is not the Mac one")
	}
	if !strings.HasSuffix(i.appExe(), "SimpleVoiceOBSRelay") || !strings.HasSuffix(i.uninstallExe(), "Uninstall") {
		t.Fatal(i.appExe(), i.uninstallExe())
	}
}

func TestRegisterAndUnregisterMac(t *testing.T) {
	asMac(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, "Desktop"), 0o755)
	i := newInstaller(filepath.Join(home, "Library", "SimpleVoice-OBS"), func(string) {})
	i.opts = options{Desktop: true, Autostart: true}
	if err := i.register(context.Background()); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join(i.macBundle(), "Contents", "MacOS", "SimpleVoiceOBS"))
	if err != nil || !strings.Contains(string(script), "'"+i.appExe()+"' --open-panel") {
		t.Fatalf("launcher: %v %q", err, script)
	}
	if fi, _ := os.Stat(filepath.Join(i.macBundle(), "Contents", "MacOS", "SimpleVoiceOBS")); fi.Mode()&0o100 == 0 {
		t.Fatal("launcher not executable")
	}
	if _, err := os.Lstat(i.macDesktopLink()); err != nil {
		t.Fatal("desktop link missing")
	}
	agent, _ := os.ReadFile(i.macAgent())
	if !strings.Contains(string(agent), "RunAtLoad") || !strings.Contains(string(agent), i.appExe()) {
		t.Fatalf("agent: %s", agent)
	}
	i.unregister(context.Background())
	for _, p := range i.unixOutside() {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was not removed", p)
		}
	}
}

func TestRegisterMacWithoutOptionalParts(t *testing.T) {
	asMac(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	i := newInstaller(filepath.Join(home, "Library", "SimpleVoice-OBS"), func(string) {})
	if err := i.register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(i.macDesktopLink()); err == nil {
		t.Error("desktop link made without being asked")
	}
	if _, err := os.Lstat(i.macAgent()); err == nil {
		t.Error("start-at-login made without being asked")
	}
}

func TestMacShellQuoting(t *testing.T) {
	if got := shQuote("/a b/it's"); got != `'/a b/it'\''s'` {
		t.Fatal(got)
	}
}

// A whole install and uninstall in Mac mode, with fake downloads and a fake Python.
func TestMacFullFlowAndUninstall(t *testing.T) {
	asMac(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	pyTgz, _ := os.ReadFile(makeTgz(t, []tar.Header{
		{Name: "python/bin/python3.11", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "python/bin/python3", Typeflag: tar.TypeSymlink, Linkname: "python3.11"},
		{Name: "python/lib/python3.11/site-packages/pip/__init__.py", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"python/bin/python3.11": "py", "python/lib/python3.11/site-packages/pip/__init__.py": "pip"}))
	mux := http.NewServeMux()
	serve := func(w http.ResponseWriter, r *http.Request, b []byte) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	}
	mux.HandleFunc("/py.tgz", func(w http.ResponseWriter, r *http.Request) { serve(w, r, pyTgz) })
	mux.HandleFunc("/"+kokoroModel, func(w http.ResponseWriter, r *http.Request) { serve(w, r, []byte("kokoro model")) })
	mux.HandleFunc("/"+kokoroVoices, func(w http.ResponseWriter, r *http.Request) { serve(w, r, []byte("kokoro voices")) })
	mux.HandleFunc("/api/models/k2-fsa/OmniVoice", func(w http.ResponseWriter, r *http.Request) {
		var sib []map[string]any
		for name, body := range modelFiles {
			e := map[string]any{"rfilename": name, "size": len(body)}
			if strings.Contains(name, "/") {
				e["lfs"] = map[string]string{"sha256": sum([]byte(body))}
			}
			sib = append(sib, e)
		}
		json.NewEncoder(w).Encode(map[string]any{"siblings": sib})
	})
	mux.HandleFunc("/api/models/openai/whisper-large-v3-turbo", func(w http.ResponseWriter, r *http.Request) {
		var sib []map[string]any
		for name, body := range asrServed {
			sib = append(sib, map[string]any{"rfilename": name, "size": len(body)})
		}
		json.NewEncoder(w).Encode(map[string]any{"siblings": sib})
	})
	mux.HandleFunc("/openai/whisper-large-v3-turbo/resolve/main/{path...}", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, []byte(asrServed[r.PathValue("path")]))
	})
	mux.HandleFunc("/k2-fsa/OmniVoice/resolve/main/{path...}", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, []byte(modelFiles[r.PathValue("path")]))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	i := newInstaller(filepath.Join(home, "Library", "SimpleVoice-OBS"), func(string) {})
	i.client = srv.Client()
	i.wait, i.retries = time.Millisecond, 1
	i.opts = options{Desktop: true, Autostart: true}
	i.pythonURL, i.pythonSHA = srv.URL+"/py.tgz", sum(pyTgz)
	i.kokoroBase = srv.URL + "/"
	i.kokoroSHA = map[string]string{kokoroModel: sum([]byte("kokoro model")), kokoroVoices: sum([]byte("kokoro voices"))}
	i.hf = srv.URL
	var logs, calls []string
	i.logf = func(l string) { logs = append(logs, l) }
	i.shell = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		return nil
	}
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		joined := strings.Join(args, " ")
		calls = append(calls, filepath.Base(name)+" "+joined)
		site := i.site()
		switch {
		case strings.Contains(joined, "torch==2.8.0"):
			os.MkdirAll(filepath.Join(site, "torch-2.8.0.dist-info"), 0o755)
			os.MkdirAll(filepath.Join(site, "torchaudio-2.8.0.dist-info"), 0o755)
		case strings.Contains(joined, omniPkg):
			os.MkdirAll(filepath.Join(site, "omnivoice-0.2.1.dist-info"), 0o755)
		case strings.Contains(joined, ffmpegPkg):
			os.MkdirAll(filepath.Join(site, "imageio_ffmpeg-0.8.1.dist-info"), 0o755)
			os.MkdirAll(filepath.Join(site, "imageio_ffmpeg", "binaries"), 0o755)
			os.WriteFile(filepath.Join(site, "imageio_ffmpeg", "binaries", "ffmpeg-macos-aarch64-v7.1"), []byte("x"), 0o755)
		case strings.Contains(joined, kokoroPkg):
			os.MkdirAll(filepath.Join(site, "kokoro_onnx-0.6.1.dist-info"), 0o755)
		case strings.Contains(joined, "--make-starters"):
			os.MkdirAll(i.voicesDir(), 0o755)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Join(home, "Desktop"), 0o755); err != nil {
		t.Fatal(err)
	}
	// the relay program is embedded; the test payload may hold empty stand-ins
	if _, err := payload.ReadFile("payload/" + appExeName); err != nil {
		t.Skip("payload for the Mac build is not present (run build-mac.sh first)")
	}
	if err := i.run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(logs, "\n"))
	}
	all := strings.Join(logs, "\n")
	for _, want := range []string{"Apple Silicon found", "FINISHED", "Launchpad"} {
		if !strings.Contains(all, want) {
			t.Errorf("log lacks %q", want)
		}
	}
	j := strings.Join(calls, "\n")
	for _, want := range []string{"python3 -m pip install", "torch==2.8.0 torchaudio==2.8.0", omniPkg, "-c import torch; ok = torch.backends.mps"} {
		if !strings.Contains(j, want) {
			t.Errorf("no call containing %q in:\n%s", want, j)
		}
	}
	if strings.Contains(j, "cu128") || strings.Contains(j, "nvidia") {
		t.Errorf("Windows-only things were used:\n%s", j)
	}
	for _, p := range []string{i.appExe(), i.uninstallExe(), i.macBundle(), i.macDesktopLink(), i.macAgent()} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	if fi, err := os.Stat(i.appExe()); err != nil || fi.Mode()&0o100 == 0 {
		t.Error("relay not executable")
	}
	before := len(calls)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls[before:] {
		if strings.Contains(c, "pip install") { // nothing is installed twice
			t.Errorf("rerun installed again: %s", c)
		}
	}
	if err := i.uninstall(false); err != nil {
		t.Fatal(err)
	}
	for _, p := range append(i.unixOutside(), i.appExe(), i.runtimeDir(), filepath.Join(i.dir, "models")) {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s left after uninstall", p)
		}
	}
}

func TestIntelMacIsRefused(t *testing.T) {
	asMac(t)
	macArch = "amd64"
	i := newInstaller(t.TempDir(), func(string) {})
	err := i.checkGPU(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Apple Silicon") {
		t.Fatalf("%v", err)
	}
	if h := detectMac(); h.GPU || h.Usable {
		t.Fatal("Intel Mac rated usable")
	}
}

func asLinux(t *testing.T) {
	old, oldL, oldX, oldName, oldU := isMac, isLinux, isUnix, appExeName, uninstallName
	isMac, isLinux, isUnix, appExeName, uninstallName = false, true, true, "SimpleVoiceOBSRelay", "Uninstall"
	t.Cleanup(func() { isMac, isLinux, isUnix, appExeName, uninstallName = old, oldL, oldX, oldName, oldU })
}

func TestLinuxPathsRegisterAndUnregister(t *testing.T) {
	asLinux(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, "Desktop"), 0o755)
	i := newInstaller(filepath.Join(home, ".local", "share", "SimpleVoice-OBS"), func(string) {})
	if i.pythonExe() != filepath.Join(i.dir, "runtime", "python", "bin", "python3") || i.pythonURL != linuxPythonURL || i.pythonSHA != linuxPythonSHA {
		t.Fatal("Linux python is not used", i.pythonExe(), i.pythonURL)
	}
	i.opts = options{Desktop: true, Autostart: true}
	if err := i.register(context.Background()); err != nil {
		t.Fatal(err)
	}
	menu, _ := os.ReadFile(i.linuxMenu())
	if !strings.Contains(string(menu), `Exec="`+i.appExe()+`" --open-panel`) || !strings.Contains(string(menu), "Type=Application") {
		t.Fatalf("menu entry: %s", menu)
	}
	auto, _ := os.ReadFile(i.linuxAutostart())
	if strings.Contains(string(auto), "--open-panel") || !strings.Contains(string(auto), `Exec="`+i.appExe()+`"`) {
		t.Fatalf("autostart: %s", auto)
	}
	if fi, err := os.Stat(i.linuxDesktop()); err != nil || fi.Mode()&0o100 == 0 {
		t.Fatal("desktop entry missing or not executable")
	}
	i.unregister(context.Background())
	for _, p := range i.unixOutside() {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was not removed", p)
		}
	}
}

func TestLinuxLibsAreChecked(t *testing.T) {
	asLinux(t)
	i := newInstaller(t.TempDir(), func(string) {})
	os.MkdirAll(filepath.Join(i.site(), "torch-2.8.0.dist-info"), 0o755)
	os.MkdirAll(filepath.Join(i.site(), "omnivoice-0.2.1.dist-info"), 0o755)
	if !i.libsDone() {
		t.Fatal("installed libs not recognised")
	}
}

// The older tests describe the Windows installer, so they run in Windows mode whatever machine runs
// them. The Mac and Linux tests switch their own mode on (asMac, asLinux).
func TestMain(m *testing.M) {
	isMac, isLinux, isUnix = false, false, false
	appExeName, uninstallName = "SimpleVoiceOBSRelay.exe", "Uninstall.exe"
	os.Exit(m.Run())
}

func TestLinuxArmIsRefused(t *testing.T) {
	asLinux(t)
	old := linuxArch
	linuxArch = "arm64"
	defer func() { linuxArch = old }()
	i := newInstaller(t.TempDir(), func(string) {})
	if err := i.installPython(context.Background()); err == nil || !strings.Contains(err.Error(), "Intel/AMD") {
		t.Fatalf("%v", err)
	}
}
