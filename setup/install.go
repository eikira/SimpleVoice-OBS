// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Pinned downloads. Hashes come from python.org (windows-3.11.9.json) and PyPI.
// Newer 3.11 releases have no Windows binaries, so 3.11.9 is the last usable one.
const (
	pythonURL = "https://www.python.org/ftp/python/3.11.9/python-3.11.9-embeddable-amd64.zip"
	pythonSHA = "33b448f95fecb7c6f802157dbd5e6b40a2ad9bfc8b95ca634a06ba4073ad1ac0"
	pipURL    = "https://files.pythonhosted.org/packages/07/25/ee8c38ae72d66b844ce8eb9711ba905dba21ca1a8a0c960057c41cd23337/pip-26.3-py3-none-any.whl"
	pipSHA    = "f3b60b20ffaadd4415ed7957212a38ac63fa4c5fa375bd0f4a797777940fad13"

	modelRepo       = "k2-fsa/OmniVoice"
	asrRepo         = "openai/whisper-large-v3-turbo" // writes the transcript of a new voice
	omniPkg         = "omnivoice==0.2.1"
	ffmpegPkg       = "imageio-ffmpeg==0.8.1" // ships ffmpeg.exe; turns mp3, flac, aac... into WAV
	kokoroPkg       = "kokoro-onnx==0.6.1"    // the processor engine: works without an NVIDIA card
	kokoroBase      = "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/"
	kokoroModel     = "kokoro-v1.0.int8.onnx" // Kokoro-82M, Apache-2.0
	kokoroModelSHA  = "6e742170d309016e5891a994e1ce1559c702a2ccd0075e67ef7157974f6406cb"
	kokoroVoices    = "voices-v1.0.bin"
	kokoroVoicesSHA = "bca610b8308e8d99f32e6fe4197e7ec01679264efed0cac9140fe9c29f1fbf7d"
	minVRAMMiB      = 6000
	retryCount      = 20
	retryWait       = 10 * time.Second
	pipBoot         = `import sys; sys.path.insert(0, sys.argv[1]); from pip._internal.cli.main import main; sys.exit(main(["install", "--no-index", "--ignore-installed", "--no-warn-script-location", sys.argv[1]]))`
	probeScript     = `import sys, urllib.request, urllib.error
for h in sys.argv[1:]:
    try:
        urllib.request.urlopen(urllib.request.Request("https://" + h + "/", method="HEAD"), timeout=15)
        print("OK       " + h, flush=True)
    except urllib.error.HTTPError:
        print("OK       " + h, flush=True)
    except Exception as e:
        print("BLOCKED  " + h + ": " + str(getattr(e, "reason", e)), flush=True)`
	infoScript = `import sys, os, shutil
print("python", sys.version.split()[0], sys.executable)
print("sys.path", sys.path)
try:
    import pip
    print("pip", pip.__version__)
except Exception as e:
    print("pip cannot be imported:", e)
sp = os.path.join(sys.prefix, "Lib", "site-packages")
print("site-packages path:", sp, "-", len(sp), "characters. Windows allows 260 per file path unless long paths are on, and PyTorch has deep paths.")
print("free disk space:", shutil.disk_usage(sys.prefix).free // 2**30, "GB")
try:
    import winreg
    k = winreg.OpenKey(winreg.HKEY_LOCAL_MACHINE, r"SYSTEM\CurrentControlSet\Control\FileSystem")
    print("Windows long paths enabled:", winreg.QueryValueEx(k, "LongPathsEnabled")[0])
except Exception as e:
    print("Windows long paths setting unknown:", e)`
	modelScript = `import sys; from huggingface_hub import snapshot_download; snapshot_download(sys.argv[1], local_dir=sys.argv[2], allow_patterns=sys.argv[3:] or None)`
	torchScript = `import torch; ok = torch.cuda.is_available(); print("torch", torch.__version__, "- NVIDIA GPU available:", ok); ok or print("No NVIDIA GPU is available to PyTorch. The processor engine will be used.")`
)

// A download that receives nothing for this long is cut off and retried.
var idleTimeout = 60 * time.Second

var pipFlags = []string{"-m", "pip", "install", "--disable-pip-version-check", "--no-warn-script-location", "--progress-bar", "off", "--retries", "2", "--timeout", "30", "--resume-retries", "5"}

// The Whisper repo also holds other copies of the weights; only these files are needed.
var asrFiles = []string{"config.json", "generation_config.json", "model.safetensors", "preprocessor_config.json",
	"tokenizer.json", "tokenizer_config.json", "vocab.json", "merges.txt", "normalizer.json", "added_tokens.json", "special_tokens_map.json"}

// PyTorch wheels for the pinned Python 3.11 on Windows. setup.exe downloads them
// itself and pip installs the local files, so pip never talks to the PyTorch site.
var torchWheels = []string{"torch-2.8.0+cu128-cp311-cp311-win_amd64.whl", "torchaudio-2.8.0+cu128-cp311-cp311-win_amd64.whl"}

// cmdFunc runs a program and sends each output line to logf.
type cmdFunc func(ctx context.Context, env []string, logf func(string), name string, args ...string) error

type installer struct {
	sel       *selection // what to install; nil means everything
	proxy     *proxy     // local download server for pip and the model downloader; nil in tests that do not need it
	explained bool       // the pip failure report was printed once
	torchInfo string     // what PyTorch said about the GPU
	dir       string
	logf      func(string)
	exec      cmdFunc
	shell     cmdFunc // Windows helpers (shortcuts, registry); separate so tests can record them
	opts      options
	appData   string // the relay's own data folder outside the install folder
	client    *http.Client
	wait      time.Duration
	retries   int

	pythonURL, pythonSHA, pipURL, pipSHA string
	kokoroBase                           string
	kokoroSHA                            map[string]string
	torchHosts                           []string
	hf                                   string
}

func newInstaller(dir string, logf func(string)) *installer {
	return &installer{
		dir:   dir,
		logf:  logf,
		exec:  runCmd,
		shell: runCmd,
		appData: func() string {
			if d := os.Getenv("LOCALAPPDATA"); d != "" {
				return filepath.Join(d, "SimpleVoiceOBS")
			}
			return ""
		}(),
		client: &http.Client{
			Timeout: 10 * time.Minute,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
		wait:      retryWait,
		retries:   retryCount,
		pythonURL: pythonURL, pythonSHA: pythonSHA,
		pipURL: pipURL, pipSHA: pipSHA,
		kokoroBase: kokoroBase,
		kokoroSHA:  map[string]string{kokoroModel: kokoroModelSHA, kokoroVoices: kokoroVoicesSHA},
		torchHosts: []string{"https://download.pytorch.org", "https://download-r2.pytorch.org"},
		hf:         "https://huggingface.co",
	}
}

// need says which program downloads from which sites. It is shown on the setup
// page, printed before each download, and used to check sites when one fails.
type need struct {
	Program string   `json:"program"`
	Purpose string   `json:"purpose"`
	Hosts   []string `json:"hosts"`
	Also    []string `json:"also,omitempty"` // may be used too, not checked
	python  bool     // checked with python.exe instead of setup.exe
}

func (i *installer) setupNeed() need {
	exe, err := os.Executable()
	if err != nil {
		exe = "setup.exe"
	}
	return need{Program: exe, Purpose: "downloads Python and pip", Hosts: []string{"www.python.org", "files.pythonhosted.org"}}
}

func (i *installer) torchNeed() need {
	n := i.setupNeed()
	n.Purpose, n.Hosts = "downloads PyTorch", []string{"download.pytorch.org", "download-r2.pytorch.org"}
	return n
}

// The same downloads through python.exe, for firewalls that treat the two programs differently.
func (i *installer) viaPython(n need) need {
	if i.proxy != nil {
		return need{} // python.exe gets its files through setup.exe, so it needs no internet access itself
	}
	n.Program, n.python = i.pythonExe(), true
	n.Purpose += " (used if setup.exe is blocked)"
	return n
}

func (i *installer) pipNeed() need {
	if i.proxy != nil {
		n := i.setupNeed()
		n.Purpose, n.Hosts = "downloads the PyTorch and OmniVoice packages for pip", []string{"pypi.org", "files.pythonhosted.org"}
		return n
	}
	return need{Program: i.pythonExe(), Purpose: "installs the PyTorch and OmniVoice packages", python: true,
		Hosts: []string{"pypi.org", "files.pythonhosted.org"}}
}

func (i *installer) modelNeed() need {
	n := i.setupNeed()
	n.Purpose, n.Hosts = "downloads the voice model", []string{"huggingface.co", "cas-bridge.xethub.hf.co"}
	n.Also = []string{"cdn-lfs.hf.co", "cdn-lfs-us-1.hf.co", "cdn-lfs-eu-1.hf.co"}
	return n
}

func (i *installer) kokoroNeed() need {
	n := i.setupNeed()
	n.Purpose, n.Hosts = "downloads the processor engine voices", []string{"github.com", "release-assets.githubusercontent.com"}
	n.Also = []string{"objects.githubusercontent.com"}
	return n
}

func (i *installer) needs() []need {
	if i.proxy != nil {
		// One program does all the downloading: this is the only entry a firewall has to allow.
		n := i.setupNeed()
		n.Purpose = "downloads everything (Python, packages, models)"
		seen := map[string]bool{}
		n.Hosts = nil
		for _, x := range []need{i.setupNeed(), i.pipNeed(), i.torchNeed(), i.modelNeed(), i.kokoroNeed()} {
			for _, h := range x.Hosts {
				if !seen[h] {
					seen[h] = true
					n.Hosts = append(n.Hosts, h)
				}
			}
			n.Also = append(n.Also, x.Also...)
		}
		return []need{n}
	}
	return []need{i.setupNeed(), i.torchNeed(), i.viaPython(i.torchNeed()), i.pipNeed(), i.modelNeed(), i.viaPython(i.modelNeed()), i.kokoroNeed()}
}

func (i *installer) runtimeDir() string { return filepath.Join(i.dir, "runtime", "python") }
func (i *installer) pythonExe() string  { return filepath.Join(i.runtimeDir(), "python.exe") }
func (i *installer) modelDir() string   { return filepath.Join(i.dir, "models", "OmniVoice") }
func (i *installer) asrDir() string     { return filepath.Join(i.dir, "models", "whisper-large-v3-turbo") }
func (i *installer) kokoroDir() string  { return filepath.Join(i.dir, "models", "kokoro") }
func (i *installer) tmpDir() string     { return filepath.Join(i.dir, "downloads") }
func (i *installer) site() string       { return filepath.Join(i.runtimeDir(), "Lib", "site-packages") }

// pkgDone tells if a pip package of exactly this version is in the install folder.
func (i *installer) pkgDone(name, version string) bool {
	_, err := os.Stat(filepath.Join(i.site(), name+"-"+version+".dist-info"))
	return err == nil
}

func has(dir string, names ...string) bool {
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			return false
		}
	}
	return true
}

// skip logs that a part is already there. Downloads are written as .part files and renamed when
// complete, so a file that exists under its real name is complete.
func (i *installer) skip(what string, done bool) bool {
	if done {
		i.logf("Already installed: " + what + ". Skipping.")
	}
	return done
}

func (i *installer) modelDone() bool {
	return has(i.modelDir(), "config.json", "model.safetensors", "tokenizer.json", "audio_tokenizer")
}
func (i *installer) asrDone() bool {
	return has(i.asrDir(), "config.json", "model.safetensors", "preprocessor_config.json", "tokenizer.json")
}
func (i *installer) ffmpegDone() bool {
	m, _ := filepath.Glob(filepath.Join(i.site(), "imageio_ffmpeg", "binaries", "ffmpeg*"))
	return i.pkgDone("imageio_ffmpeg", "0.8.1") && len(m) > 0
}

func (i *installer) kokoroDone() bool {
	return i.pkgDone("kokoro_onnx", "0.6.1") && has(i.kokoroDir(), kokoroModel, kokoroVoices)
}

func (i *installer) cacheDir() string  { return filepath.Join(i.dir, "cache") }
func (i *installer) voicesDir() string { return filepath.Join(i.dir, "voices") }

// env keeps every cache inside the install folder, so uninstall leaves nothing behind.
func (i *installer) env() []string {
	e := i.baseEnv()
	if i.proxy != nil && i.proxy.host != "" {
		e = append(e, i.proxy.env()...)
	}
	return e
}

func (i *installer) baseEnv() []string {
	return append(os.Environ(), "PYTHONNOUSERSITE=1", "PYTHONUTF8=1", "HF_HUB_DISABLE_TELEMETRY=1",
		"PIP_CACHE_DIR="+filepath.Join(i.cacheDir(), "pip"), "HF_HOME="+filepath.Join(i.cacheDir(), "hf"))
}

// owned lists the folders setup creates. Voices are the user's own files.
func (i *installer) owned(withVoices bool) []string {
	d := []string{filepath.Join(i.dir, "runtime"), filepath.Join(i.dir, "models"), i.cacheDir(), i.tmpDir()}
	if withVoices {
		d = append(d, i.voicesDir())
	}
	return d
}

func (i *installer) installed(withVoices bool) bool {
	for _, d := range append(i.owned(withVoices), i.ownedFiles()...) {
		if _, err := os.Stat(d); err == nil {
			return true
		}
	}
	return false
}

// removeOwned deletes only the folders setup created, never anything else in dir.
func (i *installer) removeOwned(withVoices bool) error {
	for _, d := range i.owned(withVoices) {
		if _, err := os.Stat(d); err != nil {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("could not remove %s (is the engine still running?): %w", d, err)
		}
		i.logf("Removed " + d)
	}
	files := append(i.ownedFiles(), i.ownedOutside()...)
	if inv, ok := i.loadInventory(); ok { // what setup listed, in case an older build placed more
		for _, rel := range inv.Files {
			if p := i.inside(rel); p != "" {
				files = append(files, p)
			}
		}
	}
	for _, f := range files {
		if f == i.uninstallExe() && sameFile(f, mustExe()) {
			continue // the running uninstaller removes itself when it quits
		}
		if _, err := os.Lstat(f); err != nil {
			continue
		}
		if err := os.RemoveAll(f); err != nil {
			return fmt.Errorf("could not remove %s: %w", f, err)
		}
		i.logf("Removed " + f)
	}
	os.Remove(i.dir) // only works when empty
	return nil
}

func mustExe() string { p, _ := os.Executable(); return p }

func (i *installer) uninstall(deleteVoices bool) error {
	ctx := context.Background()
	i.stopApp(ctx)
	i.unregister(ctx)
	if err := i.removeOwned(deleteVoices); err != nil {
		return err
	}
	if _, err := os.Stat(i.voicesDir()); err == nil {
		i.logf("Kept your voices in " + i.voicesDir())
	}
	i.logf("Uninstall finished.")
	return nil
}

// run installs everything. If it fails on a clean start, everything it did is
// reversed. If an earlier install exists, nothing is removed.
func (i *installer) run(ctx context.Context) error {
	i.logf("SimpleVoice OBS Setup " + setupBuild)
	clean := !i.installed(false)
	_, verr := os.Stat(i.voicesDir())
	voicesNew := verr != nil
	if i.proxy != nil {
		if err := i.proxy.start(); err != nil {
			return fmt.Errorf("could not start the local download server: %w", err)
		}
		defer i.proxy.stop()
	}
	err := i.install(ctx)
	if err == nil {
		return nil
	}
	i.logf("Setup did not work.")
	if !clean {
		return fmt.Errorf("%w. Files from an earlier setup were left in place; use Uninstall to remove them", err)
	}
	i.logf("Reversing everything setup did...")
	i.unregister(context.Background())
	if voicesNew {
		os.RemoveAll(i.voicesDir()) // only the starter voices setup made; the folder did not exist before
	}
	if rerr := i.removeOwned(false); rerr != nil {
		return fmt.Errorf("%w. Could not reverse everything: %v", err, rerr)
	}
	i.logf("Everything was removed. Nothing from setup is left on this PC. Allow rules that your firewall created when you clicked Allow are not removed by setup.")
	return fmt.Errorf("%w. Setup reversed everything it did", err)
}

func (i *installer) install(ctx context.Context) error {
	sel := i.selected()
	type step struct {
		name string
		fn   func(context.Context) error
	}
	var steps []step
	add := func(on bool, name string, fn func(context.Context) error) {
		if on {
			steps = append(steps, step{name, fn})
		}
	}
	add(sel.GPU, "Checking the NVIDIA GPU", i.checkGPU)
	add(true, "Installing SimpleVoice OBS Relay", i.installProgram)
	add(true, "Installing Python", i.installPython)
	add(sel.needLibs(), "Installing PyTorch (large download)", i.installTorch)
	add(sel.needLibs(), "Installing OmniVoice", i.installOmniVoice)
	add(true, "Installing ffmpeg (audio conversion)", i.installFFmpeg)
	add(sel.CPU, "Installing the processor engine (works without an NVIDIA card)", i.installKokoro)
	add(sel.needLibs(), "Checking PyTorch", i.checkTorch)
	add(sel.GPU, "Downloading the OmniVoice model (large download)", i.downloadModel)
	add(sel.CPU, "Downloading the processor engine voices", i.downloadKokoro)
	add(sel.ASR, "Downloading the transcription model (Whisper)", i.downloadASR)
	add(i.processorAvailable(), "Creating the sample voices", i.makeStarters)
	add(true, "Creating shortcuts", i.register)
	for n, s := range steps {
		i.logf(fmt.Sprintf("Step %d of %d: %s", n+1, len(steps), s.name))
		if err := s.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	if err := os.MkdirAll(i.voicesDir(), 0o755); err != nil {
		return err
	}
	os.RemoveAll(i.tmpDir())
	i.tidy()
	i.logf("FINISHED. SimpleVoice OBS is installed in " + i.dir)
	if i.torchInfo != "" {
		i.logf("GPU check: " + i.torchInfo)
	}
	i.logf("SimpleVoice OBS is ready. Start it from the Start menu. Add your voices in its control panel.")
	return nil
}

// retry runs fn up to i.retries more times, i.wait apart. ns say which programs
// need which sites: that is printed first, and checked after every failure.
func (i *installer) retry(ctx context.Context, what string, fn func() error, ns ...need) error {
	for _, n := range ns {
		if len(n.Hosts) == 0 {
			continue
		}
		msg := "INTERNET ACCESS NEEDED: " + n.Program + " must reach " + strings.Join(n.Hosts, ", ")
		if len(n.Also) > 0 {
			msg += " (and maybe " + strings.Join(n.Also, ", ") + ")"
		}
		i.logf(msg + ". If your firewall or security program asks, allow it.")
	}
	for try := 0; ; try++ {
		err := fn()
		if err == nil || ctx.Err() != nil {
			return err
		}
		for _, n := range ns {
			i.diagnose(ctx, n)
		}
		if try == i.retries {
			return fmt.Errorf("%s did not work after %d retries.%s Last error: %w", what, i.retries, allowHint(ns), err)
		}
		i.logf(fmt.Sprintf("%s failed: %v", what, err))
		if blocked(err) {
			i.logf("Windows refused the connection (error 10013). A firewall or security program on this PC is blocking it.")
		}
		i.logf(fmt.Sprintf("Retry %d of %d in %d s.", try+1, i.retries, int(i.wait.Seconds())))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.wait):
		}
	}
}

// allowHint says what to allow in the firewall.
func allowHint(ns []need) string {
	var parts []string
	for _, n := range ns {
		if len(n.Hosts) > 0 {
			parts = append(parts, n.Program+" to reach "+strings.Join(n.Hosts, ", "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " Allow " + strings.Join(parts, " (or) ") + " in your firewall or security program."
}

// blocked reports Windows error 10013, which a firewall causes.
func blocked(err error) bool { return errors.Is(err, syscall.Errno(10013)) }

// diagnose reports, site by site, whether the program can connect.
func (i *installer) diagnose(ctx context.Context, n need) {
	if len(n.Hosts) == 0 {
		return
	}
	i.logf("Checking which sites " + n.Program + " can reach:")
	if n.python {
		i.exec(ctx, i.env(), i.logf, i.pythonExe(), append([]string{"-c", probeScript}, n.Hosts...)...)
		return
	}
	for _, h := range n.Hosts {
		i.logf(i.probe(ctx, h))
	}
}

func (i *installer) probe(ctx context.Context, host string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+"/", nil)
	if err != nil {
		return "BLOCKED  " + host + ": " + err.Error()
	}
	c := *i.client
	c.Timeout = 15 * time.Second
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if blocked(err) {
			return "BLOCKED  " + host + ": a firewall or security program refuses this connection (Windows error 10013)"
		}
		return "BLOCKED  " + host + ": " + err.Error()
	}
	resp.Body.Close()
	return "OK       " + host
}

func (i *installer) checkGPU(ctx context.Context) error {
	var lines []string
	err := i.exec(ctx, i.env(), func(l string) { lines = append(lines, l) },
		"nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits")
	if err != nil || len(lines) == 0 {
		i.logf("No NVIDIA GPU or driver found. That is fine: the processor engine (built-in voices, no cloning) will be used.")
		return nil
	}
	for _, l := range lines {
		i.logf("GPU: " + l + " MiB")
		if k := strings.LastIndex(l, ","); k >= 0 {
			if mib, err := strconv.Atoi(strings.TrimSpace(l[k+1:])); err == nil && mib < minVRAMMiB {
				i.logf("Warning: this GPU has less than 6 GB of video memory. The graphics card engine may not run on it; the processor engine will be used if it does not.")
			}
		}
	}
	return nil
}

func (i *installer) installPython(ctx context.Context) error {
	if err := os.MkdirAll(i.tmpDir(), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(i.pythonExe()); err != nil {
		zipPath := filepath.Join(i.tmpDir(), "python.zip")
		if err := i.retry(ctx, "Download of Python", func() error {
			return i.fetch(ctx, i.pythonURL, zipPath, i.pythonSHA, "")
		}, i.setupNeed()); err != nil {
			return err
		}
		partial := i.runtimeDir() + ".partial"
		os.RemoveAll(partial)
		if err := unzip(zipPath, partial); err != nil {
			return err
		}
		if err := enableSite(partial); err != nil {
			return err
		}
		if err := os.Rename(partial, i.runtimeDir()); err != nil {
			return err
		}
	}
	pipDir := filepath.Join(i.runtimeDir(), "Lib", "site-packages", "pip")
	if _, err := os.Stat(pipDir); err == nil {
		i.skip("Python", true)
		return nil
	}
	wheel := filepath.Join(i.tmpDir(), path.Base(i.pipURL)) // pip needs the real wheel file name
	if err := i.retry(ctx, "Download of pip", func() error {
		return i.fetch(ctx, i.pipURL, wheel, i.pipSHA, "")
	}, i.setupNeed()); err != nil {
		return err
	}
	if err := i.exec(ctx, i.env(), i.logf, i.pythonExe(), "-c", pipBoot, wheel); err != nil {
		return err
	}
	if _, err := os.Stat(pipDir); err != nil {
		return errors.New("pip was not installed")
	}
	return nil
}

func (i *installer) pip(ctx context.Context, what string, args ...string) error {
	full := append(append([]string{}, pipFlags...), args...)
	return i.retry(ctx, what, func() error {
		lines, last := 0, ""
		err := i.exec(ctx, i.env(), func(l string) { lines++; last = l; i.logf(l) }, i.pythonExe(), full...)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if lines == 0 {
			i.explainSilentFailure(ctx, full)
			return fmt.Errorf("%w (pip printed nothing)", err)
		}
		return fmt.Errorf("%w (last pip output: %s)", err, last)
	}, i.pipNeed())
}

// explainSilentFailure runs once when pip fails without saying why.
func (i *installer) explainSilentFailure(ctx context.Context, full []string) {
	if i.explained {
		return
	}
	i.explained = true
	i.logf("pip stopped without saying why. Collecting details:")
	py, env := i.pythonExe(), i.env()
	i.exec(ctx, env, i.logf, py, "-c", infoScript)
	i.exec(ctx, env, i.logf, py, "-m", "pip", "--version")
	i.logf("Same install as a dry run, with detailed output:")
	i.exec(ctx, env, i.logf, py, append(append([]string{}, full[:len(pipFlags)]...), append([]string{"--dry-run", "-vv"}, full[len(pipFlags):]...)...)...)
}

func (i *installer) installTorch(ctx context.Context) error {
	site, done := filepath.Join(i.runtimeDir(), "Lib", "site-packages"), true
	for _, w := range torchWheels {
		parts := strings.SplitN(w, "-", 3) // name, version, rest
		_, err := os.Stat(filepath.Join(site, parts[0]+"-"+parts[1]+".dist-info"))
		done = done && err == nil
	}
	if done {
		i.logf("PyTorch is already installed.")
		return nil
	}
	var paths []string
	for _, w := range torchWheels {
		dst := filepath.Join(i.tmpDir(), w)
		paths = append(paths, dst)
		pkg := w[:strings.Index(w, "-")]
		if err := i.retry(ctx, "Download of "+pkg, func() error { return i.fetchWheelAny(ctx, w, dst) },
			i.torchNeed(), i.viaPython(i.torchNeed())); err != nil {
			return err
		}
	}
	if err := i.pip(ctx, "PyTorch install", paths...); err != nil {
		return err
	}
	for _, p := range paths {
		os.Remove(p)
	}
	return nil
}

// fetchWheelAny tries setup.exe first, then python.exe.
func (i *installer) fetchWheelAny(ctx context.Context, file, dst string) error {
	err := i.fetchWheel(ctx, file, dst)
	if err == nil || ctx.Err() != nil {
		return err
	}
	i.logf("setup.exe could not download it. Trying python.exe instead.")
	if perr := i.pipDownload(ctx, file, dst); perr != nil {
		return fmt.Errorf("setup.exe: %v; python.exe: %w", err, perr)
	}
	return nil
}

// pipDownload lets python.exe download the wheel from the direct addresses.
func (i *installer) pipDownload(ctx context.Context, file, dst string) error {
	var last error
	for k := len(i.torchHosts) - 1; k >= 0; k-- { // the Cloudflare host first
		u := i.torchHosts[k] + "/whl/cu128/" + strings.ReplaceAll(file, "+", "%2B")
		args := []string{"-m", "pip", "download", "--disable-pip-version-check", "--no-cache-dir", "--progress-bar", "off",
			"--retries", "2", "--timeout", "30", "--resume-retries", "5", "--no-deps", "--dest", filepath.Dir(dst), u}
		if last = i.exec(ctx, i.env(), i.logf, i.pythonExe(), args...); last == nil {
			if _, err := os.Stat(dst); err != nil {
				return fmt.Errorf("pip did not save %s", file)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return last
}

// fetchWheel finds a PyTorch wheel on the PyTorch sites and downloads it. It
// reads the file list for the checksum, and also tries the direct file address.
func (i *installer) fetchWheel(ctx context.Context, file, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	pkg := file[:strings.Index(file, "-")]
	var urls []string
	sha := ""
	for _, host := range i.torchHosts {
		u, s, err := i.findInIndex(ctx, host+"/whl/cu128/"+pkg+"/", file)
		if err != nil {
			i.logf("File list " + host + "/whl/cu128/" + pkg + "/: " + err.Error())
			continue
		}
		urls, sha = append(urls, u), s
	}
	for _, host := range i.torchHosts {
		urls = append(urls, host+"/whl/cu128/"+strings.ReplaceAll(file, "+", "%2B"))
	}
	var last error
	for _, u := range urls {
		if last = i.fetch(ctx, u, dst, sha, pkg); last == nil || ctx.Err() != nil {
			return last
		}
		i.logf(u + ": " + last.Error())
	}
	return last
}

var hrefRe = regexp.MustCompile(`href="([^"]*)"`)

// findInIndex reads a PyPI-style file list page and returns the address and
// SHA-256 (if listed) of the named file.
func (i *installer) findInIndex(ctx context.Context, indexURL, file string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		return "", "", err
	}
	c := *i.client
	c.Timeout = 60 * time.Second
	resp, err := c.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", "", err
	}
	base, _ := url.Parse(indexURL)
	for _, m := range hrefRe.FindAllStringSubmatch(string(page), -1) {
		u, err := url.Parse(html.UnescapeString(m[1]))
		if err != nil {
			continue
		}
		u = base.ResolveReference(u)
		if path.Base(u.Path) == file {
			sha := strings.TrimPrefix(u.Fragment, "sha256=")
			if sha == u.Fragment {
				sha = ""
			}
			u.Fragment = ""
			return u.String(), sha, nil
		}
	}
	return "", "", errors.New("file not listed")
}

func (i *installer) installOmniVoice(ctx context.Context) error {
	if i.skip("OmniVoice", i.pkgDone("omnivoice", "0.2.1")) {
		return nil
	}
	return i.pip(ctx, "OmniVoice install", omniPkg)
}

func (i *installer) installFFmpeg(ctx context.Context) error {
	if i.skip("ffmpeg", i.ffmpegDone()) {
		return nil
	}
	if err := i.pip(ctx, "ffmpeg install", ffmpegPkg); err != nil {
		return err
	}
	if !i.ffmpegDone() {
		return errors.New("ffmpeg was not installed")
	}
	return nil
}

func (i *installer) installKokoro(ctx context.Context) error {
	if i.skip("the processor engine", i.pkgDone("kokoro_onnx", "0.6.1")) {
		return nil
	}
	return i.pip(ctx, "processor engine install", kokoroPkg)
}

func (i *installer) downloadKokoro(ctx context.Context) error {
	if i.skip("the processor engine voices", has(i.kokoroDir(), kokoroModel, kokoroVoices)) {
		return nil
	}
	if err := os.MkdirAll(i.kokoroDir(), 0o755); err != nil {
		return err
	}
	for _, f := range [][2]string{{kokoroModel, i.kokoroSHA[kokoroModel]}, {kokoroVoices, i.kokoroSHA[kokoroVoices]}} {
		dst := filepath.Join(i.kokoroDir(), f[0])
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := i.retry(ctx, "Download of "+f[0], func() error {
			return i.fetch(ctx, i.kokoroBase+f[0], dst, f[1], f[0])
		}, i.kokoroNeed()); err != nil {
			return err
		}
	}
	return nil
}

// makeStarters lets the processor engine read a short text in each of its voices. They become ready-made
// voices (a recording and its exact words) for the graphics card engine. A problem here does not
// stop the install: the starter voices are a convenience.
func (i *installer) makeStarters(ctx context.Context) error {
	var missing []string
	for _, n := range i.wantedVoices() {
		if !i.voiceExists(n) {
			missing = append(missing, n)
		}
	}
	if len(i.wantedVoices()) == 0 || i.skip("the sample voices", len(missing) == 0) {
		return nil
	}
	script := filepath.Join(i.tmpDir(), "worker.py")
	b, err := payload.ReadFile("payload/worker.py")
	if err == nil {
		os.MkdirAll(i.tmpDir(), 0o755)
		err = os.WriteFile(script, b, 0o644)
	}
	if err == nil {
		err = i.exec(ctx, i.env(), i.logf, i.pythonExe(), script, "--make-starters", i.voicesDir(), "--kokoro", i.kokoroDir(), "--only", strings.Join(missing, ","))
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		i.logf("The sample voices could not be made (" + err.Error() + "). Setup continues. The processor engine still has its built-in voices.")
		return nil
	}
	var made []string
	for _, n := range missing {
		if i.voiceExists(n) {
			made = append(made, n)
		}
	}
	i.recordVoices(made)
	return nil
}

func (i *installer) checkTorch(ctx context.Context) error {
	return i.exec(ctx, i.env(), func(l string) {
		if strings.HasPrefix(l, "torch ") {
			i.torchInfo = l
		}
		i.logf(l)
	}, i.pythonExe(), "-c", torchScript)
}

type modelFile struct {
	Path string `json:"rfilename"`
	Size int64  `json:"size"`
	LFS  *struct {
		SHA256 string `json:"sha256"`
	} `json:"lfs"`
}

func (i *installer) downloadModel(ctx context.Context) error {
	if i.skip("the OmniVoice model", i.modelDone()) {
		return nil
	}
	return i.downloadRepo(ctx, "Model download", modelRepo, i.modelDir(), nil)
}

func (i *installer) downloadASR(ctx context.Context) error {
	if i.skip("the transcription model", i.asrDone()) {
		return nil
	}
	if err := i.downloadRepo(ctx, "Transcription model download", asrRepo, i.asrDir(), asrFiles); err != nil {
		return err
	}
	if !i.asrDone() {
		return errors.New("the transcription model is incomplete")
	}
	return nil
}

// downloadRepo fetches a model repo (only the listed files, or all) into dir, with setup.exe
// first and python.exe as the fallback.
func (i *installer) downloadRepo(ctx context.Context, what, repo, dir string, only []string) error {
	return i.retry(ctx, what, func() error {
		err := i.repoViaSetup(ctx, repo, dir, only)
		if err == nil || ctx.Err() != nil {
			return err
		}
		i.logf("setup.exe could not download " + repo + ": " + err.Error())
		i.logf("Trying python.exe instead.")
		env := append(i.env(), "HF_HUB_DISABLE_XET=1")
		if perr := i.exec(ctx, env, i.logf, i.pythonExe(), append([]string{"-c", modelScript, repo, dir}, only...)...); perr != nil {
			return fmt.Errorf("setup.exe: %v; python.exe: %w", err, perr)
		}
		return nil
	}, i.modelNeed(), i.viaPython(i.modelNeed()))
}

func (i *installer) repoViaSetup(ctx context.Context, repo, dir string, only []string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.hf+"/api/models/"+repo+"?blobs=true", nil)
	if err != nil {
		return err
	}
	c := *i.client
	c.Timeout = 60 * time.Second
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Siblings []modelFile `json:"siblings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&body); err != nil {
		return err
	}
	var files []modelFile
	for _, f := range body.Siblings {
		if only == nil || slices.Contains(only, f.Path) {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return errors.New("the model has no files listed")
	}
	for n, f := range files {
		clean := path.Clean(f.Path)
		if strings.HasPrefix(clean, "..") || path.IsAbs(clean) {
			return fmt.Errorf("unsafe file name in the model: %s", f.Path)
		}
		dst := filepath.Join(dir, filepath.FromSlash(clean))
		if st, err := os.Stat(dst); err == nil && st.Size() == f.Size {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		sha := ""
		if f.LFS != nil {
			sha = f.LFS.SHA256
		}
		segs := strings.Split(clean, "/")
		for k := range segs {
			segs[k] = url.PathEscape(segs[k])
		}
		src := i.hf + "/" + repo + "/resolve/main/" + strings.Join(segs, "/")
		if err := i.fetch(ctx, src, dst, sha, fmt.Sprintf("%s: file %d of %d, %s", repo, n+1, len(files), clean)); err != nil {
			return err
		}
	}
	return nil
}

// fetch downloads urlStr into dst and resumes an interrupted download. If
// wantSHA is set it is checked; a wrong hash is retried, because a firewall
// block page can arrive instead of the file. A label turns on progress lines.
func (i *installer) fetch(ctx context.Context, urlStr, dst, wantSHA, label string) (err error) {
	part := dst + ".part"
	var have int64
	if st, err := os.Stat(part); err == nil {
		have = st.Size()
	}
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(idleTimeout, cancel)
	defer idle.Stop()
	defer func() {
		if err != nil && ctx.Err() != nil && parent.Err() == nil {
			err = fmt.Errorf("no data received for %d s", int(idleTimeout.Seconds()))
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	c := *i.client
	c.Timeout = 0
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		have = 0 // no resume: start again
	case http.StatusPartialContent:
	case http.StatusRequestedRangeNotSatisfiable: // the part file may be complete
		return finish(part, dst, wantSHA)
	default:
		return fmt.Errorf("%s: HTTP %d", urlStr, resp.StatusCode)
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if have > 0 {
		flags = os.O_WRONLY | os.O_APPEND
		i.logf(fmt.Sprintf("Resuming at %d MB", have>>20))
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	total := have + max(resp.ContentLength, 0)
	buf, done, next := make([]byte, 1<<20), have, have+100<<20
	for {
		n, rerr := resp.Body.Read(buf)
		idle.Reset(idleTimeout)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return err
			}
			if done += int64(n); label != "" && done >= next {
				i.logf(fmt.Sprintf("%s: %d of %d MB", label, done>>20, total>>20))
				next = done + 100<<20
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr // the part file is kept, so the next try resumes
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if resp.ContentLength >= 0 && done != total {
		return fmt.Errorf("%s: connection closed early", urlStr)
	}
	return finish(part, dst, wantSHA)
}

func finish(part, dst, wantSHA string) error {
	if wantSHA != "" {
		f, err := os.Open(part)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != wantSHA {
			os.Remove(part)
			return errors.New("checksum does not match")
		}
	}
	return os.Rename(part, dst)
}

func unzip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	root := filepath.Clean(dst) + string(os.PathSeparator)
	for _, f := range r.File {
		p := filepath.Join(dst, f.Name)
		if !strings.HasPrefix(p, root) {
			return fmt.Errorf("unsafe path in zip: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := extract(f, p); err != nil {
			return err
		}
	}
	return nil
}

func extract(f *zip.File, path string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, rc)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// enableSite turns on "import site" in the embeddable Python so pip can install packages.
func enableSite(dir string) error {
	files, _ := filepath.Glob(filepath.Join(dir, "python*._pth"))
	if len(files) != 1 {
		return errors.New("python ._pth file not found")
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		return err
	}
	return os.WriteFile(files[0], []byte(strings.Replace(string(b), "#import site", "import site", 1)), 0o644)
}

func runCmd(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
	w := &lineWriter{logf: logf}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = w, w
	hideWindow(cmd)
	err := cmd.Run()
	w.flush()
	return err
}

// lineWriter splits program output into lines.
type lineWriter struct {
	mu   sync.Mutex
	logf func(string)
	buf  string
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf += string(p)
	for {
		k := strings.IndexByte(w.buf, '\n')
		if k < 0 {
			return len(p), nil
		}
		w.emit(w.buf[:k])
		w.buf = w.buf[k+1:]
	}
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emit(w.buf)
	w.buf = ""
}

func (w *lineWriter) emit(l string) {
	if l = strings.TrimSpace(l); l != "" {
		w.logf(l)
	}
}
