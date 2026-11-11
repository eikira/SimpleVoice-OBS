// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var shellCalls []string

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// testInstaller never reaches the internet: only the local test servers answer.
func testInstaller(t *testing.T) *installer {
	i := newInstaller(t.TempDir(), func(string) {})
	i.appData = filepath.Join(t.TempDir(), "appdata")
	i.shell = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		var ov []string
		for _, e := range env {
			if strings.HasPrefix(e, "OV_") {
				ov = append(ov, e)
			}
		}
		shellCalls = append(shellCalls, name+" "+strings.Join(args, " ")+" "+strings.Join(ov, " "))
		return nil
	}
	shellCalls = nil
	i.wait = time.Millisecond
	i.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			return nil, errors.New("offline in tests: " + addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
	return i
}

func makeZip(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func TestRetryCounts(t *testing.T) {
	if d := newInstaller("", nil); d.retries != 20 || d.wait != 10*time.Second {
		t.Fatalf("defaults must be 20 retries, 10 s, got %d, %v", d.retries, d.wait)
	}
	i := testInstaller(t)
	n := 0
	if err := i.retry(context.Background(), "x", func() error {
		n++
		if n <= 3 {
			return errors.New("boom")
		}
		return nil
	}); err != nil || n != 4 {
		t.Fatalf("err=%v n=%d", err, n)
	}
	n = 0
	err := i.retry(context.Background(), "x", func() error { n++; return errors.New("boom") })
	if err == nil || n != 21 { // first try + 20 retries
		t.Fatalf("err=%v n=%d", err, n)
	}
}

func TestRetryWaitAndCancel(t *testing.T) {
	i := testInstaller(t)
	i.wait = 50 * time.Millisecond
	n := 0
	start := time.Now()
	i.retry(context.Background(), "x", func() error { n++; return errors.New("boom") }) // 20 waits would be 1 s
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("waits too short: %v", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	i.wait = time.Hour
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := i.retry(ctx, "x", func() error { return errors.New("boom") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
}

func TestDownloadHash(t *testing.T) {
	body := []byte("hello")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&hits, 1) {
		case 1:
			http.Error(w, "no", 500)
		case 2:
			w.Write([]byte("<html>firewall block page</html>"))
		default:
			w.Write(body)
		}
	}))
	defer srv.Close()
	i := testInstaller(t)
	dst := filepath.Join(t.TempDir(), "f")
	if err := i.retry(context.Background(), "dl", func() error { return i.fetch(context.Background(), srv.URL, dst, sum(body), "") }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "hello" || hits != 3 {
		t.Fatalf("got %q after %d hits", b, hits)
	}
	if _, err := os.Stat(dst + ".part"); err == nil {
		t.Fatal(".part left behind")
	}
}

func TestUnzipRejectsEscape(t *testing.T) {
	z := filepath.Join(t.TempDir(), "a.zip")
	os.WriteFile(z, makeZip(t, map[string]string{"../evil.txt": "x"}), 0o644)
	if err := unzip(z, filepath.Join(t.TempDir(), "out")); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("want unsafe path error, got %v", err)
	}
}

type fakeNet struct {
	hits      int32
	failModel atomic.Bool
}

var modelFiles = map[string]string{"config.json": "{}", "model.safetensors": "big weights", "tokenizer.json": "{}", "audio_tokenizer/model.safetensors": "weights"}

// asrServed has files the installer needs, and an extra copy of the weights that it must not download.
var asrServed = map[string]string{"config.json": "{}", "model.safetensors": "whisper", "preprocessor_config.json": "{}", "tokenizer.json": "{}", "flax_model.msgpack": "unwanted"}

// fakeSetup serves Python, pip, PyTorch wheels and the model, and stubs every program.
func fakeSetup(t *testing.T) (*installer, *[]string, *fakeNet) {
	pyZip := makeZip(t, map[string]string{"python.exe": "x", "python311._pth": "python311.zip\n.\n#import site\n"})
	pip := []byte("wheel")
	n := &fakeNet{}
	mux := http.NewServeMux()
	serve := func(w http.ResponseWriter, r *http.Request, b []byte) {
		atomic.AddInt32(&n.hits, 1)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	}
	mux.HandleFunc("/py.zip", func(w http.ResponseWriter, r *http.Request) { serve(w, r, pyZip) })
	mux.HandleFunc("/pip-26.3-py3-none-any.whl", func(w http.ResponseWriter, r *http.Request) { serve(w, r, pip) })
	mux.HandleFunc("/whl/cu128/{file}", func(w http.ResponseWriter, r *http.Request) { serve(w, r, []byte("wheel "+r.PathValue("file"))) })
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
		if n.failModel.Load() {
			http.Error(w, "blocked", http.StatusInternalServerError)
			return
		}
		serve(w, r, []byte(modelFiles[r.PathValue("path")]))
	})
	mux.HandleFunc("/"+kokoroModel, func(w http.ResponseWriter, r *http.Request) { serve(w, r, []byte("kokoro model")) })
	mux.HandleFunc("/"+kokoroVoices, func(w http.ResponseWriter, r *http.Request) { serve(w, r, []byte("kokoro voices")) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	i := testInstaller(t)
	i.kokoroBase = srv.URL + "/"
	i.kokoroSHA = map[string]string{kokoroModel: sum([]byte("kokoro model")), kokoroVoices: sum([]byte("kokoro voices"))}
	i.pythonURL, i.pythonSHA = srv.URL+"/py.zip", sum(pyZip)
	i.pipURL, i.pipSHA = srv.URL+"/pip-26.3-py3-none-any.whl", sum(pip)
	i.torchHosts, i.hf = []string{srv.URL}, srv.URL
	var calls []string
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		calls = append(calls, filepath.Base(name)+" "+strings.Join(args, " "))
		site := filepath.Join(i.runtimeDir(), "Lib", "site-packages")
		switch {
		case name == "nvidia-smi":
			logf("NVIDIA GeForce RTX 3050, 4096")
		case strings.Contains(strings.Join(args, " "), "snapshot_download") && n.failModel.Load():
			return errors.New("blocked")
		case len(args) > 1 && args[1] == pipBoot:
			os.MkdirAll(filepath.Join(site, "pip"), 0o755)
		case strings.Contains(strings.Join(args, " "), omniPkg):
			os.MkdirAll(filepath.Join(site, "omnivoice-0.2.1.dist-info"), 0o755)
		case strings.Contains(strings.Join(args, " "), ffmpegPkg):
			os.MkdirAll(filepath.Join(site, "imageio_ffmpeg-0.8.1.dist-info"), 0o755)
			os.MkdirAll(filepath.Join(site, "imageio_ffmpeg", "binaries"), 0o755)
			os.WriteFile(filepath.Join(site, "imageio_ffmpeg", "binaries", "ffmpeg-win.exe"), []byte("x"), 0o755)
		case strings.Contains(strings.Join(args, " "), kokoroPkg):
			os.MkdirAll(filepath.Join(site, "kokoro_onnx-0.6.1.dist-info"), 0o755)
		case len(args) > 2 && args[1] == "--make-starters":
			os.MkdirAll(args[2], 0o755)
			for _, n := range strings.Split(onlyArg(args), ",") {
				os.WriteFile(filepath.Join(args[2], n+".wav"), []byte("RIFF"+n), 0o644)
				os.WriteFile(filepath.Join(args[2], n+".txt"), []byte("words"), 0o644)
			}
		case strings.Contains(strings.Join(args, " "), "torch-2.8.0+cu128"):
			os.MkdirAll(filepath.Join(site, "torch-2.8.0+cu128.dist-info"), 0o755)
			os.MkdirAll(filepath.Join(site, "torchaudio-2.8.0+cu128.dist-info"), 0o755)
		}
		return nil
	}
	return i, &calls, n
}

func TestFullFlowAndRerun(t *testing.T) {
	i, calls, net := fakeSetup(t)
	var logs []string
	i.logf = func(l string) { logs = append(logs, l) }
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	pth, _ := os.ReadFile(filepath.Join(i.runtimeDir(), "python311._pth"))
	if !strings.Contains(string(pth), "\nimport site") {
		t.Fatalf("import site not enabled: %q", pth)
	}
	want := []string{"nvidia-smi", "python.exe -c", "torch-2.8.0+cu128", "omnivoice==0.2.1", "imageio-ffmpeg==0.8.1", "kokoro-onnx==0.6.1", "python.exe -c import torch", "worker.py --make-starters"}
	if len(*calls) != len(want) {
		t.Fatalf("calls: %q", *calls)
	}
	for k, w := range want {
		if !strings.Contains((*calls)[k], w) {
			t.Errorf("call %d = %q, want %q", k, (*calls)[k], w)
		}
	}
	if !strings.Contains(strings.Join(logs, "\n"), "less than 6 GB") {
		t.Error("no low VRAM warning")
	}
	if st, err := os.Stat(filepath.Join(i.dir, "voices")); err != nil || !st.IsDir() {
		t.Error("voices folder missing")
	}
	all := strings.Join(logs, "\n")
	for _, want := range []string{"FINISHED", "Start menu"} {
		if !strings.Contains(all, want) {
			t.Errorf("finish message lacks %q", want)
		}
	}
	for name, body := range modelFiles {
		if b, err := os.ReadFile(filepath.Join(i.modelDir(), filepath.FromSlash(name))); err != nil || string(b) != body {
			t.Errorf("model file %s: %q %v", name, b, err)
		}
	}
	// Second run downloads nothing again.
	before := atomic.LoadInt32(&net.hits)
	if err := i.run(context.Background()); err != nil || atomic.LoadInt32(&net.hits) != before {
		t.Fatalf("rerun: err=%v extra downloads=%d", err, atomic.LoadInt32(&net.hits)-before)
	}
}

func TestNoGPUOnlyWarns(t *testing.T) {
	i, calls, _ := fakeSetup(t)
	var logs []string
	i.logf = func(l string) { logs = append(logs, l) }
	base := i.exec
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		if name == "nvidia-smi" {
			return errors.New("not found")
		}
		return base(ctx, env, logf, name, args...)
	}
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "No NVIDIA GPU or driver found") || len(*calls) != 7 {
		t.Fatalf("logs=%q calls=%v", logs, *calls)
	}
}

func TestBrokenTorchStillFails(t *testing.T) {
	i, _, _ := fakeSetup(t)
	base := i.exec
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		if len(args) > 1 && args[1] == torchScript {
			return errors.New("import error")
		}
		return base(ctx, env, logf, name, args...)
	}
	if err := i.run(context.Background()); err == nil || !strings.Contains(err.Error(), "Checking PyTorch") {
		t.Fatalf("err=%v", err)
	}
}

func TestPipRetriesWhenNetworkFails(t *testing.T) {
	i, _, _ := fakeSetup(t)
	fails := 0
	base := i.exec
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "torch-2.8.0") && fails < 5 {
			fails++
			return errors.New("network blocked")
		}
		return base(ctx, env, logf, name, args...)
	}
	if err := i.run(context.Background()); err != nil || fails != 5 {
		t.Fatalf("err=%v fails=%d", err, fails)
	}
}

func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{logf: func(l string) { got = append(got, l) }}
	w.Write([]byte("a\r\nb"))
	w.Write([]byte("c\n\nd"))
	w.flush()
	if strings.Join(got, "|") != "a|bc|d" {
		t.Fatalf("got %q", got)
	}
}

var gotSel *selection

func testServer(t *testing.T, run func(context.Context) error) (*server, *httptest.Server) {
	s := &server{ctx: context.Background(), dir: "D", run: run, setOpts: func(options) {}, launch: func() error { return nil }, state: "idle", quit: make(chan struct{}, 1),
		present:   func() bool { return true },
		access:    func() []need { return []need{{Program: "p", Hosts: []string{"h"}}} },
		uninstall: func(bool) error { return nil },
		parts: func() ([]compInfo, compInfo) {
			return []compInfo{{ID: "base", Required: true}, {ID: "cpu"}, {ID: "gpu"}, {ID: "asr"}}, compInfo{ID: "libs"}
		},
		setSel: func(v selection) { gotSel = &v },
		hw:     func() hardware { return hardware{GPU: true, Usable: true, Tier: "good", FreeMB: 50000} }}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	s.host = strings.TrimPrefix(ts.URL, "http://")
	return s, ts
}

func post(t *testing.T, url, body string, hdr bool, host string) int {
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if hdr {
		req.Header.Set("X-Setup", "1")
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestServerNeedsCheckbox(t *testing.T) {
	var ran int32
	s, ts := testServer(t, func(context.Context) error { atomic.AddInt32(&ran, 1); return nil })
	for _, c := range []struct {
		body string
		hdr  bool
		host string
		want int
	}{
		{`{"accepted":false}`, true, "", 400},
		{`{}`, true, "", 400},
		{`junk`, true, "", 400},
		{`{"accepted":true}`, false, "", 403},
		{`{"accepted":true}`, true, "evil.example", 403},
	} {
		if got := post(t, ts.URL+"/start", c.body, c.hdr, c.host); got != c.want {
			t.Errorf("%+v: got %d", c, got)
		}
	}
	if atomic.LoadInt32(&ran) != 0 {
		t.Fatal("installer ran without the checkbox")
	}
	if got := post(t, ts.URL+"/start", `{"accepted":true}`, true, ""); got != 200 {
		t.Fatalf("start: %d", got)
	}
	for k := 0; k < 100; k++ {
		s.mu.Lock()
		st := s.state
		s.mu.Unlock()
		if st == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.state != "done" || atomic.LoadInt32(&ran) != 1 {
		t.Fatalf("state=%s ran=%d", s.state, ran)
	}
}

func TestServerOneJobAtATime(t *testing.T) {
	release := make(chan struct{})
	var removed atomic.Bool
	s, ts := testServer(t, func(context.Context) error { <-release; return nil })
	s.uninstall = func(bool) error { removed.Store(true); return nil }
	if got := post(t, ts.URL+"/start", `{"accepted":true}`, true, ""); got != 200 {
		t.Fatalf("start: %d", got)
	}
	if got := post(t, ts.URL+"/start", `{"accepted":true}`, true, ""); got != 409 {
		t.Fatalf("second start: %d", got)
	}
	if got := post(t, ts.URL+"/uninstall", `{}`, true, ""); got != 409 || removed.Load() {
		t.Fatalf("uninstall while installing: %d removed=%v", got, removed.Load())
	}
	close(release)
}

func TestServerUninstall(t *testing.T) {
	var gotVoices atomic.Bool
	s, ts := testServer(t, func(context.Context) error { return nil })
	s.uninstall = func(v bool) error { gotVoices.Store(v); return nil }
	if got := post(t, ts.URL+"/uninstall", `{}`, false, ""); got != 403 {
		t.Fatalf("no header: %d", got)
	}
	if got := post(t, ts.URL+"/uninstall", `{}`, true, "evil.example"); got != 403 {
		t.Fatalf("bad host: %d", got)
	}
	if got := post(t, ts.URL+"/uninstall", `junk`, true, ""); got != 400 {
		t.Fatalf("junk: %d", got)
	}
	if got := post(t, ts.URL+"/uninstall", `{"deleteVoices":true}`, true, ""); got != 200 {
		t.Fatalf("uninstall: %d", got)
	}
	for k := 0; k < 100; k++ {
		s.mu.Lock()
		st := s.state
		s.mu.Unlock()
		if st == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.state != "done" || s.mode != "uninstall" || !gotVoices.Load() {
		t.Fatalf("state=%s mode=%s voices=%v", s.state, s.mode, gotVoices.Load())
	}
}

func TestUninstallRemovesOnlyOwnedFolders(t *testing.T) {
	i, _, _ := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(i.dir), "outside.txt")
	os.WriteFile(outside, []byte("x"), 0o644)
	os.WriteFile(filepath.Join(i.dir, "notes.txt"), []byte("mine"), 0o644)
	os.WriteFile(filepath.Join(i.voicesDir(), "anna.wav"), []byte("w"), 0o644)
	os.MkdirAll(i.cacheDir(), 0o755)
	if !i.installed(true) {
		t.Fatal("should report installed")
	}

	if err := i.uninstall(false); err != nil {
		t.Fatal(err)
	}
	for _, d := range i.owned(false) {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("%s still exists", d)
		}
	}
	for _, f := range []string{filepath.Join(i.voicesDir(), "anna.wav"), filepath.Join(i.dir, "notes.txt"), outside} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s must be kept: %v", f, err)
		}
	}

	if err := i.uninstall(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(i.voicesDir()); err == nil {
		t.Error("voices should be deleted when asked")
	}
	if _, err := os.Stat(filepath.Join(i.dir, "notes.txt")); err != nil {
		t.Error("unrelated file in dir was deleted")
	}
	os.Remove(filepath.Join(i.dir, "notes.txt"))
	if i.installed(true) {
		t.Error("nothing should be left")
	}
	i.uninstall(true)
	if _, err := os.Stat(i.dir); err == nil {
		t.Error("empty install folder should be removed")
	}
}

func TestCachesStayInsideInstallFolder(t *testing.T) {
	i := testInstaller(t)
	env := strings.Join(i.env(), "\n")
	for _, want := range []string{"PIP_CACHE_DIR=" + filepath.Join(i.dir, "cache", "pip"), "HF_HOME=" + filepath.Join(i.dir, "cache", "hf")} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
}

func TestServerFailureReported(t *testing.T) {
	s, ts := testServer(t, func(context.Context) error { return errors.New("nope") })
	post(t, ts.URL+"/start", `{"accepted":true}`, true, "")
	for k := 0; k < 100 && func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.state != "failed" }(); k++ {
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := http.Get(ts.URL + "/state?from=0")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	resp.Body.Close()
	if !strings.Contains(b.String(), `"state":"failed"`) || !strings.Contains(b.String(), "nope") {
		t.Fatalf("state: %s", b.String())
	}
	if r, _ := http.Get(ts.URL + "/"); r.StatusCode != 200 {
		t.Fatal("index not served")
	}
}

func TestRetryNamesProgramAndSites(t *testing.T) {
	i := testInstaller(t)
	i.retries = 2
	var logs []string
	i.logf = func(l string) { logs = append(logs, l) }
	var probes int
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		probes++
		logf("BLOCKED  pypi.org: timed out")
		return nil
	}
	n := need{Program: `C:\x\python.exe`, Hosts: []string{"pypi.org", "download.pytorch.org"}, python: true}
	err := i.retry(context.Background(), "PyTorch install", func() error { return errors.New("exit status 1") }, n)
	if err == nil || !strings.Contains(err.Error(), `C:\x\python.exe`) || !strings.Contains(err.Error(), "download.pytorch.org") || !strings.Contains(err.Error(), "after 2 retries") {
		t.Fatalf("err=%v", err)
	}
	all := strings.Join(logs, "\n")
	if !strings.Contains(all, "INTERNET ACCESS NEEDED: C:\\x\\python.exe must reach pypi.org, download.pytorch.org") || probes != 3 || !strings.Contains(all, "BLOCKED  pypi.org") {
		t.Fatalf("probes=%d logs:\n%s", probes, all)
	}
}

func TestProbeSetupExe(t *testing.T) {
	ok := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer ok.Close()
	i := testInstaller(t)
	i.client = ok.Client()
	host := strings.TrimPrefix(ok.URL, "https://")
	if got := i.probe(context.Background(), host); !strings.HasPrefix(got, "OK ") { // an HTTP 404 still means reachable
		t.Fatalf("got %q", got)
	}
	if got := i.probe(context.Background(), "127.0.0.1:1"); !strings.HasPrefix(got, "BLOCKED  127.0.0.1:1: ") {
		t.Fatalf("got %q", got)
	}
}

func TestEveryDownloadStepHasANeed(t *testing.T) {
	i := testInstaller(t)
	exe := filepath.Join(i.runtimeDir(), "python.exe")
	for _, n := range i.needs() {
		if n.Program == "" || len(n.Hosts) == 0 || n.Purpose == "" {
			t.Errorf("incomplete: %+v", n)
		}
		if n.python && n.Program != exe {
			t.Errorf("python need points at %s", n.Program)
		}
	}
	for _, h := range []string{"www.python.org", "files.pythonhosted.org", "pypi.org", "download.pytorch.org", "download-r2.pytorch.org", "huggingface.co", "cas-bridge.xethub.hf.co"} {
		found := false
		for _, n := range i.needs() {
			for _, x := range n.Hosts {
				found = found || x == h
			}
		}
		if !found {
			t.Errorf("%s missing", h)
		}
	}
}

func failAt(i *installer, substr string) {
	base := i.exec
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), substr) {
			return errors.New("network blocked")
		}
		return base(ctx, env, logf, name, args...)
	}
}

func TestFailureReversesEverything(t *testing.T) {
	i, _, _ := fakeSetup(t)
	i.retries = 2
	var logs []string
	i.logf = func(l string) { logs = append(logs, l) }
	failAt(i, "omnivoice==")
	err := i.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not work after 2 retries") || !strings.Contains(err.Error(), "reversed everything") {
		t.Fatalf("err=%v", err)
	}
	if i.installed(true) {
		t.Fatal("files left behind")
	}
	if _, err := os.Stat(i.dir); err == nil {
		t.Fatal("empty install folder left behind")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "Reversing everything") {
		t.Fatal("user was not told")
	}
}

func TestReverseKeepsUsersVoices(t *testing.T) {
	i, _, net := fakeSetup(t)
	i.retries = 1
	net.failModel.Store(true)
	os.MkdirAll(i.voicesDir(), 0o755)
	os.WriteFile(filepath.Join(i.voicesDir(), "anna.wav"), []byte("w"), 0o644)
	if err := i.run(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(filepath.Join(i.voicesDir(), "anna.wav")); err != nil {
		t.Fatal("voice deleted")
	}
	if i.installed(false) {
		t.Fatal("files left behind")
	}
}

func TestNoReverseOverEarlierInstall(t *testing.T) {
	i, _, net := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	i.retries = 1
	os.RemoveAll(i.modelDir()) // the earlier model download is incomplete
	net.failModel.Store(true)
	err := i.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "left in place") {
		t.Fatalf("err=%v", err)
	}
	if _, e := os.Stat(i.pythonExe()); e != nil {
		t.Fatal("earlier install was removed")
	}
}

func TestCancelReversesToo(t *testing.T) {
	i, _, _ := fakeSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	base := i.exec
	i.exec = func(c context.Context, env []string, logf func(string), name string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "torch-2.8.0") {
			cancel()
			return context.Canceled
		}
		return base(c, env, logf, name, args...)
	}
	if err := i.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if i.installed(true) {
		t.Fatal("files left behind after cancel")
	}
}

func TestSilentPipFailureCollectsDetails(t *testing.T) {
	i, _, _ := fakeSetup(t)
	i.retries = 2
	var logs, calls []string
	i.logf = func(l string) { logs = append(logs, l) }
	base := i.exec
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		j := strings.Join(args, " ")
		calls = append(calls, j)
		if strings.Contains(j, "torch-2.8.0") && !strings.Contains(j, "--dry-run") {
			return errors.New("exit status 1") // no output at all
		}
		return base(ctx, env, logf, name, args...)
	}
	err := i.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pip printed nothing") {
		t.Fatalf("err=%v", err)
	}
	count := func(sub string) (n int) {
		for _, c := range calls {
			if strings.Contains(c, sub) {
				n++
			}
		}
		return
	}
	if count("free disk space") != 1 || count("-m pip --version") != 1 || count("--dry-run -vv") != 1 {
		t.Fatalf("details must be collected exactly once:\n%s", strings.Join(calls, "\n---\n"))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "pip stopped without saying why") {
		t.Fatal("user not told")
	}
}

func TestPipFailureShowsLastOutput(t *testing.T) {
	i, _, _ := fakeSetup(t)
	i.retries = 1
	base := i.exec
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "omnivoice==") {
			logf("ERROR: No matching distribution found for omnivoice==0.2.1")
			return errors.New("exit status 1")
		}
		return base(ctx, env, logf, name, args...)
	}
	err := i.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "last pip output: ERROR: No matching distribution") {
		t.Fatalf("err=%v", err)
	}
}

func TestPipBootstrapMustReallyInstallPip(t *testing.T) {
	i, _, _ := fakeSetup(t)
	i.retries = 1
	base := i.exec
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		if len(args) > 1 && args[1] == pipBoot {
			if !strings.HasSuffix(args[2], "pip-26.3-py3-none-any.whl") {
				t.Errorf("wheel needs its real file name, got %s", args[2])
			}
			return nil // exits 0 but installs nothing
		}
		return base(ctx, env, logf, name, args...)
	}
	if err := i.run(context.Background()); err == nil || !strings.Contains(err.Error(), "pip was not installed") {
		t.Fatalf("err=%v", err)
	}
}

func TestFetchResumesPartialDownload(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 1000)
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	i := testInstaller(t)
	dst := filepath.Join(t.TempDir(), "w.whl")
	os.WriteFile(dst+".part", body[:4000], 0o644)
	if err := i.fetch(context.Background(), srv.URL, dst, sum(body), "w"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, body) || gotRange != "bytes=4000-" {
		t.Fatalf("range=%q len=%d", gotRange, len(b))
	}
}

func TestFetchServerWithoutRangeSupport(t *testing.T) {
	body := []byte("complete file")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	i := testInstaller(t)
	dst := filepath.Join(t.TempDir(), "f")
	os.WriteFile(dst+".part", []byte("old junk that is longer than nothing"), 0o644)
	if err := i.fetch(context.Background(), srv.URL, dst, sum(body), ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, body) {
		t.Fatalf("got %q", b)
	}
}

func TestFetchKeepsPartAfterEarlyClose(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 5000)
	var first atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			w.Header().Set("Content-Length", "5000")
			w.Write(body[:2000])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler) // connection dies mid-download
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	i := testInstaller(t)
	dst := filepath.Join(t.TempDir(), "f")
	if err := i.fetch(context.Background(), srv.URL, dst, sum(body), ""); err == nil {
		t.Fatal("want error on early close")
	}
	if st, err := os.Stat(dst + ".part"); err != nil || st.Size() == 0 || st.Size() >= 5000 {
		t.Fatalf("partial file must be kept: %v %v", st, err)
	}
	if err := i.fetch(context.Background(), srv.URL, dst, sum(body), ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, body) {
		t.Fatal("resumed file is wrong")
	}
}

func TestFetchStallIsCutOff(t *testing.T) {
	old := idleTimeout
	idleTimeout = 150 * time.Millisecond
	defer func() { idleTimeout = old }()
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		<-stop
	}))
	defer srv.Close()
	defer close(stop)
	i := testInstaller(t)
	start := time.Now()
	err := i.fetch(context.Background(), srv.URL, filepath.Join(t.TempDir(), "f"), "", "")
	if err == nil || !strings.Contains(err.Error(), "no data received") || time.Since(start) > 3*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestFindInIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><a href="torch-2.8.0%2Bcu128-cp312-cp312-win_amd64.whl#sha256=bad">x</a>
<a href="/whl/cu128/torch-2.8.0%2Bcu128-cp311-cp311-win_amd64.whl#sha256=abc123">y</a>
<a href="other/torch-2.7.0%2Bcu128-cp311-cp311-win_amd64.whl">z</a>`))
	}))
	defer srv.Close()
	i := testInstaller(t)
	u, sha, err := i.findInIndex(context.Background(), srv.URL+"/whl/cu128/torch/", "torch-2.8.0+cu128-cp311-cp311-win_amd64.whl")
	if err != nil || sha != "abc123" || u != srv.URL+"/whl/cu128/torch-2.8.0%2Bcu128-cp311-cp311-win_amd64.whl" {
		t.Fatalf("u=%q sha=%q err=%v", u, sha, err)
	}
	if _, _, err := i.findInIndex(context.Background(), srv.URL+"/x/", "torch-9.9.9.whl"); err == nil {
		t.Fatal("unknown file must be an error")
	}
}

func TestWheelUsesIndexChecksumAndFallsBackToDirectURL(t *testing.T) {
	body := []byte("the wheel")
	file := "torch-2.8.0+cu128-cp311-cp311-win_amd64.whl"
	var direct atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/whl/cu128/torch/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "closed", 500) })
	mux.HandleFunc("/whl/cu128/{f}", func(w http.ResponseWriter, r *http.Request) {
		direct.Add(1)
		if r.PathValue("f") != strings.ReplaceAll(file, "+", "%2B") && r.PathValue("f") != file {
			t.Errorf("unexpected file %q", r.PathValue("f"))
		}
		w.Write(body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	i := testInstaller(t)
	i.torchHosts = []string{srv.URL}
	dst := filepath.Join(t.TempDir(), file)
	if err := i.fetchWheel(context.Background(), file, dst); err != nil || direct.Load() != 1 {
		t.Fatalf("err=%v direct=%d", err, direct.Load())
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, body) {
		t.Fatal("wrong content")
	}

	// A listed checksum that does not match is an error and leaves no wheel.
	mux2 := http.NewServeMux()
	var base string
	mux2.HandleFunc("/whl/cu128/torch/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<a href="` + base + `/whl/cu128/torch-2.8.0%2Bcu128-cp311-cp311-win_amd64.whl#sha256=` + strings.Repeat("0", 64) + `">w</a>`))
	})
	mux2.HandleFunc("/whl/cu128/{f}", func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()
	base = srv2.URL
	i.torchHosts = []string{srv2.URL}
	dst2 := filepath.Join(t.TempDir(), file)
	if err := i.fetchWheel(context.Background(), file, dst2); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(dst2); err == nil {
		t.Fatal("corrupt wheel was kept")
	}
}

func TestModelRejectsUnsafeFileNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"siblings":[{"rfilename":"../../evil.txt","size":1}]}`))
	}))
	defer srv.Close()
	i := testInstaller(t)
	i.hf, i.retries = srv.URL, 1
	i.exec = func(context.Context, []string, func(string), string, ...string) error { return errors.New("not used") }
	if err := i.downloadModel(context.Background()); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(i.dir, "evil.txt")); err == nil {
		t.Fatal("file written outside the model folder")
	}
}

func TestFetchStallBeforeAnyDataIsCutOff(t *testing.T) {
	old := idleTimeout
	idleTimeout = 150 * time.Millisecond
	defer func() { idleTimeout = old }()
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-stop }))
	defer srv.Close()
	defer close(stop)
	i := testInstaller(t)
	start := time.Now()
	err := i.fetch(context.Background(), srv.URL, filepath.Join(t.TempDir(), "f"), "", "")
	if err == nil || !strings.Contains(err.Error(), "no data received") || time.Since(start) > 3*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestPythonIsUsedWhenSetupExeIsBlocked(t *testing.T) {
	i, calls, _ := fakeSetup(t)
	var logs []string
	i.logf = func(l string) { logs = append(logs, l) }
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // nothing listens: the connection is refused
	i.torchHosts = []string{dead.URL, "http://r2.example"}
	i.hf = dead.URL
	base := i.exec
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		j := strings.Join(args, " ")
		if strings.Contains(j, "pip download") || strings.Contains(j, "-m pip download") {
			os.MkdirAll(i.tmpDir(), 0o755)
			os.WriteFile(filepath.Join(i.tmpDir(), filepath.Base(strings.ReplaceAll(args[len(args)-1], "%2B", "+"))), []byte("wheel"), 0o644)
			*calls = append(*calls, "DOWNLOAD "+args[len(args)-1])
			return nil
		}
		if j := strings.Join(args, " "); strings.Contains(j, "snapshot_download") {
			repo, dir := args[2], args[3]
			names := map[string]bool{}
			if repo == asrRepo {
				names = map[string]bool{"config.json": true, "model.safetensors": true, "preprocessor_config.json": true, "tokenizer.json": true}
			} else {
				for n := range modelFiles {
					names[n] = true
				}
			}
			for n := range names {
				os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(n))), 0o755)
				os.WriteFile(filepath.Join(dir, filepath.FromSlash(n)), []byte("x"), 0o644)
			}
			*calls = append(*calls, "snapshot_download "+repo)
			return nil
		}
		return base(ctx, env, logf, name, args...)
	}
	if err := i.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	var downloads []string
	var model bool
	for _, c := range *calls {
		if strings.HasPrefix(c, "DOWNLOAD ") {
			downloads = append(downloads, c)
		}
		model = model || strings.Contains(c, "snapshot_download")
	}
	// two wheels, each asked from the second (Cloudflare) host first
	if len(downloads) != 2 || !strings.HasPrefix(downloads[0], "DOWNLOAD http://r2.example/whl/cu128/torch-2.8.0%2Bcu128") {
		t.Fatalf("downloads=%v", downloads)
	}
	if !model {
		t.Fatal("model was not fetched through python.exe")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "Trying python.exe instead") {
		t.Fatal("user was not told")
	}
}

func TestBothProgramsBlockedIsReportedAndReversed(t *testing.T) {
	i, _, _ := fakeSetup(t)
	i.retries = 1
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	i.torchHosts = []string{dead.URL}
	base := i.exec
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "-m pip download") {
			return errors.New("exit status 1")
		}
		return base(ctx, env, logf, name, args...)
	}
	err := i.run(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"setup.exe", "python.exe", "download.pytorch.org", "reversed everything"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
	if i.installed(true) {
		t.Fatal("files left behind")
	}
}

func TestFirewallErrorIsExplained(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://x", Err: &net.OpError{Op: "dial", Err: os.NewSyscallError("connectex", syscall.Errno(10013))}}
	if !blocked(err) || blocked(errors.New("other")) {
		t.Fatal("blocked() wrong")
	}
	i := testInstaller(t)
	i.retries = 1
	var logs []string
	i.logf = func(l string) { logs = append(logs, l) }
	i.retry(context.Background(), "x", func() error { return err }, need{Program: "setup.exe", Hosts: []string{"h"}})
	if !strings.Contains(strings.Join(logs, "\n"), "error 10013") {
		t.Fatalf("no hint: %q", logs)
	}
}

func joined() string { return strings.Join(shellCalls, "\n") }

func TestInstallPlacesProgramShortcutsAndEntries(t *testing.T) {
	i, _, _ := fakeSetup(t)
	i.opts = options{Desktop: true, Autostart: true}
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want, _ := payload.ReadFile("payload/" + appExeName)
	if got, err := os.ReadFile(i.appExe()); err != nil || !bytes.Equal(got, want) || len(want) < 1000 {
		t.Fatalf("relay program not installed: %v", err)
	}
	for _, f := range []string{i.uninstallExe(), filepath.Join(i.dir, "LICENSE"), filepath.Join(i.dir, "THIRD-PARTY-NOTICES.txt")} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	all := joined()
	for _, w := range []string{"powershell.exe", "OV_ACTION=create", "OV_DESKTOP=1", "OV_EXE=" + i.appExe(),
		"reg.exe add " + regUninst + " /v DisplayName", "--uninstall", "reg.exe add " + regRun + " /v " + runValue} {
		if !strings.Contains(all, w) {
			t.Errorf("missing call %q in\n%s", w, all)
		}
	}
}

func TestOptionsOffMeansNoDesktopIconAndNoAutostart(t *testing.T) {
	i, _, _ := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := joined()
	if !strings.Contains(all, "OV_DESKTOP=0") || strings.Contains(all, "reg.exe add "+regRun) {
		t.Fatalf("options not respected:\n%s", all)
	}
}

func TestUninstallRemovesProgramEntriesAndKeepsVoices(t *testing.T) {
	i, _, _ := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(i.appData, 0o755)
	os.WriteFile(filepath.Join(i.appData, "worker.py"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(i.dir, "settings.txt"), []byte("PORT=1"), 0o644)
	os.WriteFile(filepath.Join(i.voicesDir(), "Anna.wav"), []byte("x"), 0o644)
	shellCalls = nil
	if err := i.uninstall(false); err != nil {
		t.Fatal(err)
	}
	for _, f := range append(i.ownedFiles(), i.appData, filepath.Join(i.dir, "runtime"), filepath.Join(i.dir, "models")) {
		if _, err := os.Stat(f); err == nil && f != i.uninstallExe() {
			t.Errorf("%s was not removed", f)
		}
	}
	if _, err := os.Stat(filepath.Join(i.voicesDir(), "Anna.wav")); err != nil {
		t.Error("voices were deleted")
	}
	all := joined()
	for _, w := range []string{"--stop", "OV_ACTION=remove", "reg.exe delete " + regUninst, "reg.exe delete " + regRun + " /v " + runValue} {
		if !strings.Contains(all, w) {
			t.Errorf("missing call %q in\n%s", w, all)
		}
	}
}

func TestFailedInstallRemovesTheProgramToo(t *testing.T) {
	i, _, _ := fakeSetup(t)
	i.pythonURL = "http://127.0.0.1:1/nothing.zip"
	i.retries = 1
	if err := i.run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	for _, f := range append(i.ownedFiles(), i.appData) {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s left behind after a failed install", f)
		}
	}
	if !strings.Contains(joined(), "reg.exe delete "+regUninst) {
		t.Error("registry entry not cleaned up on failure")
	}
}

func TestRerunSkipsEverythingAlreadyInstalled(t *testing.T) {
	i, calls, net := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var logs []string
	i.logf = func(l string) { logs = append(logs, l) }
	*calls = nil
	hits := atomic.LoadInt32(&net.hits)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(logs, "\n")
	for _, what := range []string{"Python", "PyTorch", "OmniVoice", "ffmpeg", "the OmniVoice model", "the transcription model"} {
		if !strings.Contains(all, "Already installed: "+what) && !strings.Contains(all, what+" is already installed") {
			t.Errorf("%s was not recognised as installed:\n%s", what, all)
		}
	}
	if atomic.LoadInt32(&net.hits) != hits {
		t.Error("downloaded again")
	}
	for _, c := range *calls {
		if strings.Contains(c, "pip install") || strings.Contains(c, "pip._internal") {
			t.Errorf("pip ran again: %s", c)
		}
	}
}

func TestEarlierInstallGetsOnlyTheNewParts(t *testing.T) {
	// An install from before ffmpeg and the transcription model existed.
	i, calls, net := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(i.asrDir())
	os.RemoveAll(filepath.Join(i.site(), "imageio_ffmpeg"))
	os.RemoveAll(filepath.Join(i.site(), "imageio_ffmpeg-0.8.1.dist-info"))
	*calls = nil
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(*calls, "\n")
	if !strings.Contains(all, ffmpegPkg) || strings.Contains(all, omniPkg) || strings.Contains(all, "torch-2.8.0") {
		t.Fatalf("wrong parts installed:\n%s", all)
	}
	if !i.asrDone() || !i.ffmpegDone() {
		t.Fatal("new parts missing")
	}
	if _, err := os.Stat(filepath.Join(i.asrDir(), "flax_model.msgpack")); err == nil {
		t.Error("downloaded a file that is not needed")
	}
	_ = net
}

func TestProcessorEngineAndStarterVoicesAreInstalledOnce(t *testing.T) {
	i, calls, _ := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{kokoroModel, kokoroVoices} {
		if _, err := os.Stat(filepath.Join(i.kokoroDir(), f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	if _, err := os.Stat(filepath.Join(i.voicesDir(), "Heart.wav")); err != nil {
		t.Error("starter voice was not made")
	}
	*calls = nil
	var logs []string
	i.logf = func(l string) { logs = append(logs, l) }
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(logs, "\n")
	for _, w := range []string{"Already installed: the processor engine.", "Already installed: the processor engine voices.", "Already installed: the sample voices."} {
		if !strings.Contains(all, w) {
			t.Errorf("second run did not skip: %s", w)
		}
	}
	for _, c := range *calls {
		if strings.Contains(c, "kokoro") || strings.Contains(c, "make-starters") {
			t.Errorf("second run repeated work: %s", c)
		}
	}
}

func TestStarterVoiceProblemDoesNotStopSetup(t *testing.T) {
	i, _, _ := fakeSetup(t)
	base := i.exec
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		if len(args) > 1 && args[1] == "--make-starters" {
			return errors.New("boom")
		}
		return base(ctx, env, logf, name, args...)
	}
	var logs []string
	i.logf = func(l string) { logs = append(logs, l) }
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "sample voices could not be made") {
		t.Fatalf("%q", logs)
	}
	if inv, ok := i.loadInventory(); ok && len(inv.Voices) > 0 {
		t.Fatal("voices recorded although none were made")
	}
}

func TestWrongProcessorEngineFileIsRejected(t *testing.T) {
	i, _, _ := fakeSetup(t)
	i.kokoroSHA[kokoroModel] = strings.Repeat("0", 64)
	i.retries = 1
	if err := i.run(context.Background()); err == nil {
		t.Fatal("a file with the wrong checksum must not be accepted")
	}
	if _, err := os.Stat(i.dir + "/models"); err == nil {
		t.Fatal("rollback left the models folder")
	}
}

func TestRollbackRemovesStarterVoicesItMade(t *testing.T) {
	i, _, _ := fakeSetup(t)
	base := i.shell
	_ = base
	i.shell = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		return errors.New("shortcuts failed")
	}
	if err := i.run(context.Background()); err == nil {
		t.Skip("shortcut step did not fail in this setup")
	}
	if _, err := os.Stat(i.voicesDir()); err == nil {
		t.Fatal("voices folder created by the failed setup was left behind")
	}
}

func TestSelectionLeavesOutWhatIsNotTicked(t *testing.T) {
	// processor engine only: no PyTorch, no OmniVoice, no big models, no Whisper
	i, calls, _ := fakeSetup(t)
	i.sel = &selection{CPU: true}
	var logs []string
	i.logf = func(l string) { logs = append(logs, l) }
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(*calls, "\n")
	for _, no := range []string{"torch", "omnivoice==", "snapshot_download"} {
		if strings.Contains(all, no) {
			t.Errorf("%s must not be installed for a processor-only setup:\n%s", no, all)
		}
	}
	if _, err := os.Stat(i.modelDir()); err == nil {
		t.Error("OmniVoice model was downloaded")
	}
	if _, err := os.Stat(i.asrDir()); err == nil {
		t.Error("Whisper was downloaded")
	}
	if !i.kokoroDone() {
		t.Error("processor engine missing")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "of 7:") {
		t.Errorf("steps: %q", logs)
	}
}

func TestSelectionTranscriptionNeedsLibrariesButNotTheGraphicsModel(t *testing.T) {
	i, calls, _ := fakeSetup(t)
	i.sel = &selection{CPU: true, ASR: true}
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(*calls, "\n")
	if !strings.Contains(all, "torch-2.8.0+cu128") || !strings.Contains(all, "omnivoice==") {
		t.Errorf("transcription needs PyTorch and the OmniVoice code:\n%s", all)
	}
	if !i.asrDone() || i.modelDone() {
		t.Errorf("asr=%v model=%v", i.asrDone(), i.modelDone())
	}
}

func TestPlannedSizesCountSharedPartsOnceAndSkipInstalled(t *testing.T) {
	i := testInstaller(t)
	disk, load := i.planned(selection{CPU: true, GPU: true, ASR: true})
	if disk != 250+220+6300+3000+1600 || load != 100+160+3400+3000+1600 {
		t.Fatalf("everything: %d %d", disk, load)
	}
	d2, _ := i.planned(selection{CPU: true})
	if d2 != 250+220 {
		t.Fatalf("processor only: %d", d2)
	}
	d3, _ := i.planned(selection{CPU: true, ASR: true})
	if d3 != 250+220+6300+1600 {
		t.Fatalf("transcription alone adds the libraries: %d", d3)
	}
	os.MkdirAll(filepath.Join(i.site(), "torch-2.8.0+cu128.dist-info"), 0o755)
	os.MkdirAll(filepath.Join(i.site(), "omnivoice-0.2.1.dist-info"), 0o755)
	d4, _ := i.planned(selection{CPU: true, GPU: true, ASR: true})
	if d4 != disk-6300 {
		t.Fatalf("installed libraries are not counted again: %d vs %d", d4, disk-6300)
	}
}

func TestStartNeedsAnEngineAndPassesTheSelection(t *testing.T) {
	_, ts := testServer(t, func(context.Context) error { return nil })
	host := strings.TrimPrefix(ts.URL, "http://")
	if c := post(t, ts.URL+"/start", `{"accepted":true,"components":["asr"]}`, true, host); c != 400 {
		t.Fatalf("no engine ticked must be refused: %d", c)
	}
	gotSel = nil
	if c := post(t, ts.URL+"/start", `{"accepted":true,"components":["cpu","asr"]}`, true, host); c != 200 && c != 204 {
		t.Fatalf("%d", c)
	}
	if gotSel == nil || !gotSel.CPU || gotSel.GPU || !gotSel.ASR {
		t.Fatalf("%+v", gotSel)
	}
}

func TestBuildNumberIsShownEverywhere(t *testing.T) {
	if setupBuild == "" {
		t.Fatal("no build number")
	}
	_, ts := testServer(t, func(context.Context) error { return nil })
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	resp.Body.Close()
	if !strings.Contains(b.String(), "Setup "+setupBuild) || strings.Contains(b.String(), "{{BUILD}}") {
		t.Fatal("page does not show the build number")
	}
	i, _, _ := fakeSetup(t)
	var logs []string
	i.logf = func(l string) { logs = append(logs, l) }
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 || logs[0] != "SimpleVoice OBS Setup "+setupBuild {
		t.Fatalf("first log line: %q", logs)
	}
}

func TestSetupPageIsMarkedNotToBeTranslated(t *testing.T) {
	_, ts := testServer(t, func(context.Context) error { return nil })
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	resp.Body.Close()
	if !strings.Contains(b.String(), `translate="no"`) || !strings.Contains(b.String(), `content="notranslate"`) || resp.Header.Get("Content-Language") != "en" {
		t.Fatal("setup page can trigger the browser translate popup")
	}
}

// An upgrade makes the sample voices that are missing, and only those.
func TestUpgradeMakesTheMissingSampleVoices(t *testing.T) {
	i, calls, _ := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, x := i.voiceFiles("Bella")
	os.Remove(w)
	os.Remove(x)
	*calls = nil
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var made []string
	for _, c := range *calls {
		if strings.Contains(c, "--make-starters") {
			made = append(made, c)
		}
	}
	if len(made) != 1 || !strings.HasSuffix(made[0], "--only Bella") {
		t.Fatalf("calls: %q", made)
	}
	if !i.voiceExists("Bella") {
		t.Fatal("Bella was not made")
	}
}

func TestOnlyTheTickedSampleVoicesAreMade(t *testing.T) {
	i, _, _ := fakeSetup(t)
	i.sel = &selection{CPU: true, Voices: []string{"Puck", "Fable"}}
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, n := range sampleVoices {
		want := n == "Puck" || n == "Fable"
		if i.voiceExists(n) != want {
			t.Errorf("%s exists=%v", n, !want)
		}
	}
}

func TestNoSampleVoicesWithoutTheProcessorEngine(t *testing.T) {
	i, calls, _ := fakeSetup(t)
	i.sel = &selection{GPU: true}
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if strings.Contains(c, "--make-starters") {
			t.Fatal("sample voices were made without the processor engine")
		}
	}
}

func TestInventoryIsWrittenAfterASuccessfulInstall(t *testing.T) {
	i, _, _ := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	inv, ok := i.loadInventory()
	if !ok || inv.Build != setupBuild || len(inv.Voices) != len(sampleVoices) {
		t.Fatalf("inventory: %+v %v", inv, ok)
	}
	have := strings.Join(inv.Files, " ")
	for _, f := range []string{appExeName, "Uninstall.exe", "LICENSE"} {
		if !strings.Contains(have, f) {
			t.Errorf("%s not listed", f)
		}
	}
}

func TestUpgradeRemovesWhatTheNewBuildNoLongerShips(t *testing.T) {
	i, _, _ := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(i.dir, "OldHelper.dll")
	mine := filepath.Join(i.dir, "my-notes.txt")
	os.WriteFile(old, []byte("x"), 0o644)
	os.WriteFile(mine, []byte("x"), 0o644)
	inv, _ := i.loadInventory()
	inv.Files = append(inv.Files, "OldHelper.dll", "../outside.txt", "voices/Heart.wav")
	outside := filepath.Join(filepath.Dir(i.dir), "outside.txt")
	os.WriteFile(outside, []byte("x"), 0o644)
	defer os.Remove(outside)
	i.saveInventory(inv)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("a file the new build does not ship was kept")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Error("a file setup never listed was removed")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("a path outside the install folder was removed")
	}
	if !i.voiceExists("Heart") {
		t.Error("a voice was removed through the file list")
	}
}

func TestWithoutAnInventoryNothingIsRemoved(t *testing.T) {
	i, _, _ := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.Remove(i.inventoryPath())
	stray := filepath.Join(i.dir, "OldHelper.dll")
	os.WriteFile(stray, []byte("x"), 0o644)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Error("a file was removed without an inventory")
	}
	if !i.voiceExists("Heart") {
		t.Error("voices were touched")
	}
	os.WriteFile(i.inventoryPath(), []byte("{broken"), 0o644)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Error("a file was removed with an unreadable inventory")
	}
}

func TestUntickingRemovesOnlyUnchangedSetupVoices(t *testing.T) {
	i, _, _ := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Bella was edited by the user; Nicole was renamed away; Aoede is the user's own recording of the same name.
	w, _ := i.voiceFiles("Bella")
	os.WriteFile(w, []byte("my own take"), 0o644)
	nw, nt := i.voiceFiles("Nicole")
	os.Rename(nw, filepath.Join(i.voicesDir(), "Anna.wav"))
	os.Rename(nt, filepath.Join(i.voicesDir(), "Anna.txt"))
	i.sel = &selection{CPU: true, Voices: []string{"Heart"}}
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !i.voiceExists("Heart") {
		t.Error("a ticked voice was removed")
	}
	if !i.voiceExists("Bella") {
		t.Error("a voice the user changed was removed")
	}
	if _, err := os.Stat(filepath.Join(i.voicesDir(), "Anna.wav")); err != nil {
		t.Error("a renamed voice was removed")
	}
	for _, n := range []string{"Kore", "Sarah", "Fable"} {
		if i.voiceExists(n) {
			t.Errorf("%s was not removed", n)
		}
	}
	inv, _ := i.loadInventory()
	if _, ok := inv.Voices["Bella"]; ok {
		t.Error("a voice the user changed is still listed as setup's")
	}
	// A later run with all ticked makes the missing ones again but keeps the user's Bella.
	i.sel = nil
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(w); string(b) != "my own take" {
		t.Error("the user's Bella was overwritten")
	}
	if !i.voiceExists("Kore") {
		t.Error("Kore was not made again")
	}
}

func TestVoiceChoiceIsRememberedForTheNextRun(t *testing.T) {
	i, _, _ := fakeSetup(t)
	i.sel = &selection{CPU: true, Voices: []string{"Emma"}}
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, v := range i.voiceList() {
		if v.Selected != (v.Name == "Emma") {
			t.Errorf("%s selected=%v", v.Name, v.Selected)
		}
		if v.Name == "Emma" && v.State != "made" {
			t.Errorf("Emma state %q", v.State)
		}
	}
}

func TestUninstallRemovesTheInventory(t *testing.T) {
	i, _, _ := fakeSetup(t)
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := i.uninstall(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(i.inventoryPath()); err == nil {
		t.Fatal("the inventory was left behind")
	}
	if !i.voiceExists("Heart") {
		t.Fatal("voices must stay unless asked")
	}
}

func onlyArg(args []string) string {
	for k, a := range args {
		if a == "--only" && k+1 < len(args) {
			return args[k+1]
		}
	}
	return "Heart"
}

func TestGraphicsCardGuide(t *testing.T) {
	cases := []struct {
		name   string
		vram   int
		capab  float64
		usable bool
		tier   string
	}{
		{"", 0, 0, false, "none"},
		{"NVIDIA GeForce GTX 1060", 6144, 6.1, false, "unsupported"},
		{"NVIDIA GeForce GTX 1660", 6144, 7.5, true, "ok"},
		{"NVIDIA GeForce RTX 3050", 4096, 8.6, true, "low"},
		{"NVIDIA GeForce RTX 3060", 12288, 8.6, true, "good"},
		{"NVIDIA GeForce RTX 5070", 12227, 12.0, true, "good"},
		{"NVIDIA GeForce RTX 3070", 8192, 0, true, "good"}, // an old driver does not say the generation
	}
	for _, c := range cases {
		u, tier, info := rate(c.name, c.vram, c.capab)
		if u != c.usable || tier != c.tier || info == "" {
			t.Errorf("%s: usable %v tier %s", c.name, u, tier)
		}
		if c.name != "" && !strings.Contains(info, "Rough guide") {
			t.Errorf("%s: the guide must say it is not measured", c.name)
		}
	}
}

// With no usable card, nothing that only the graphics card engine needs is selected,
// even if the page asks for it.
func TestNoUsableCardMeansProcessorEngineOnly(t *testing.T) {
	i, _, _ := fakeSetup(t)
	var got selection
	s := &server{ctx: context.Background(), host: "127.0.0.1:1", state: "idle", quit: make(chan struct{}, 1)}
	s.hw = func() hardware { return hardware{Tier: "none"} }
	s.parts = i.components
	s.setSel = func(v selection) { got = v }
	s.setOpts = func(options) {}
	s.run = func(context.Context) error { return nil }
	s.voices = i.voiceList
	req := httptest.NewRequest("POST", "/start", strings.NewReader(`{"accepted":true,"components":["cpu","gpu","asr"]}`))
	rec := httptest.NewRecorder()
	s.handleStart(rec, req)
	if rec.Code != 200 || !got.CPU || got.GPU || got.ASR {
		t.Fatalf("code %d, selection %+v", rec.Code, got)
	}
	s.wg.Wait()
}

// Setup's list of sample voices must be the worker's list.
func TestSampleVoiceListMatchesTheWorker(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	b, err := payload.ReadFile("payload/worker.py")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "worker.py"), b, 0o644)
	out, err := exec.Command(py, "-I", "-c", "import sys, json\nsys.path.insert(0, sys.argv[1])\nimport worker\nprint(json.dumps(worker.STARTERS))", dir).Output()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	if json.Unmarshal(out, &names) != nil || strings.Join(names, ",") != strings.Join(sampleVoices, ",") {
		t.Fatalf("worker has %v, setup has %v", names, sampleVoices)
	}
}
