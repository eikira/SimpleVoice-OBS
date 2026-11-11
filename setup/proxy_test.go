// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// upstream pretends to be PyPI, the file host and Hugging Face (with its redirect to a download host).
func testProxy(t *testing.T) (*proxy, *httptest.Server, *int32) {
	var flaky int32
	mux := http.NewServeMux()
	var srv *httptest.Server
	var cdnURL string
	mux.HandleFunc("/simple/demo/", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&flaky, 1) == 1 {
			http.Error(w, "busy", 503)
			return
		}
		io.WriteString(w, `<a href="`+srv.URL+`/packages/ab/demo-1.0-py3-none-any.whl#sha256=00">demo-1.0</a>`)
	})
	mux.HandleFunc("/packages/ab/demo-1.0-py3-none-any.whl", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x.whl", time.Time{}, strings.NewReader("0123456789"))
	})
	mux.HandleFunc("/org/model/resolve/main/model.bin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Repo-Commit", "abc")
		w.Header().Set("X-Linked-Etag", "etag1")
		http.Redirect(w, r, cdnURL+"/blob1?sig=1", http.StatusFound)
	})
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "m", time.Time{}, strings.NewReader("MODELDATA"))
	}))
	t.Cleanup(cdn.Close)
	cdnURL = cdn.URL
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := newProxy(&http.Client{}, func(string) {})
	p.pypi, p.files, p.hf, p.wait = srv.URL, srv.URL, srv.URL, time.Millisecond
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)
	return p, srv, &flaky
}

func get(t *testing.T, method, u string, hdr map[string]string) (*http.Response, string) {
	req, _ := http.NewRequest(method, u, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestProxyIndexLinksPointBackAndRetries(t *testing.T) {
	p, srv, _ := testProxy(t)
	resp, body := get(t, "GET", p.base()+"/pypi/simple/demo/", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, p.base()+"/pyfiles/packages/ab/demo-1.0-py3-none-any.whl#sha256=00") || strings.Contains(body, srv.URL) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestProxyFilesSupportResume(t *testing.T) {
	p, _, _ := testProxy(t)
	resp, body := get(t, "GET", p.base()+"/pyfiles/packages/ab/demo-1.0-py3-none-any.whl", map[string]string{"Range": "bytes=4-"})
	if resp.StatusCode != 206 || body != "456789" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
}

func TestProxyHuggingFaceKeepsRedirectsInside(t *testing.T) {
	p, srv, _ := testProxy(t)
	// HEAD: headers of the first answer, and a Location that leads back through the proxy.
	resp, _ := get(t, "HEAD", p.base()+"/hf/org/model/resolve/main/model.bin", nil)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != 302 || resp.Header.Get("X-Repo-Commit") != "abc" || !strings.HasPrefix(loc, p.base()+"/ext/") || strings.Contains(loc, srv.URL) {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	resp, body := get(t, "GET", loc, nil)
	if resp.StatusCode != 200 || body != "MODELDATA" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	// GET: the proxy follows the redirect itself.
	resp, body = get(t, "GET", p.base()+"/hf/org/model/resolve/main/model.bin", nil)
	if resp.StatusCode != 200 || body != "MODELDATA" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
}

func TestProxyRefusesWebPagesAndStrangers(t *testing.T) {
	p, _, _ := testProxy(t)
	if resp, _ := get(t, "GET", p.base()+"/pyfiles/packages/ab/demo-1.0-py3-none-any.whl", map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != 403 {
		t.Fatalf("a web page must not use the proxy: %d", resp.StatusCode)
	}
	if resp, _ := get(t, "GET", p.base()+"/pyfiles/x", map[string]string{"Host": "evil.example"}); resp.StatusCode == 200 {
		t.Fatal("wrong host")
	}
	req, _ := http.NewRequest("GET", p.base()+"/pyfiles/x", nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("%v %v", resp, err)
	}
	if resp, _ := get(t, "GET", p.base()+"/ext/doesnotexist", nil); resp.StatusCode != 404 {
		t.Fatalf("only addresses the proxy issued: %d", resp.StatusCode)
	}
	if resp, _ := get(t, "POST", p.base()+"/hf/x", nil); resp.StatusCode != 403 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if cleanRel("../../etc/x") != "etc/x" || cleanRel("a/../../b") != "b" {
		t.Fatal("paths must stay below the file host root")
	}
}

func TestProxyUpstreamDownGivesBadGateway(t *testing.T) {
	p, srv, _ := testProxy(t)
	srv.Close()
	if resp, _ := get(t, "GET", p.base()+"/pyfiles/a.whl", nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestPipAndModelToolsAreSentThroughTheProxy(t *testing.T) {
	i, _, _ := fakeSetup(t)
	i.proxy = newProxy(i.client, func(string) {})
	var envs [][]string
	base := i.exec
	i.exec = func(ctx context.Context, env []string, logf func(string), name string, args ...string) error {
		envs = append(envs, env)
		return base(ctx, env, logf, name, args...)
	}
	if err := i.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, e := range envs {
		joined += strings.Join(e, "\n") + "\n--\n"
	}
	if !strings.Contains(joined, "PIP_INDEX_URL=http://127.0.0.1:") || !strings.Contains(joined, "HF_ENDPOINT=http://127.0.0.1:") {
		t.Fatal("pip and the model downloader must be pointed at the proxy")
	}
	needs := i.needs()
	if len(needs) != 1 || !strings.Contains(strings.Join(needs[0].Hosts, ","), "pypi.org") || !strings.Contains(strings.Join(needs[0].Hosts, ","), "huggingface.co") {
		t.Fatalf("one program must be listed for the firewall: %+v", needs)
	}
}

// Real pip against the real PyPI through the proxy. Only runs when asked: OV_REAL_NET=1 go test -run RealPip
func TestRealPipThroughProxy(t *testing.T) {
	if os.Getenv("OV_REAL_NET") != "1" {
		t.Skip("set OV_REAL_NET=1 to run against the internet")
	}
	p := newProxy(&http.Client{}, func(l string) { t.Log(l) })
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	defer p.stop()
	dir := t.TempDir()
	cmd := exec.Command("python3", "-m", "pip", "download", "--no-deps", "-d", dir, "--disable-pip-version-check", "kokoro-onnx==0.6.1", "numpy==2.2.6")
	cmd.Env = append(os.Environ(), p.env()...)
	cmd.Env = append(cmd.Env, "HTTPS_PROXY=http://127.0.0.1:1", "https_proxy=http://127.0.0.1:1", "HTTP_PROXY=", "NO_PROXY=127.0.0.1")
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 2 {
		t.Fatalf("%v", files)
	}
}
