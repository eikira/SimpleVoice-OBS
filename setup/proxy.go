// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// proxy is a small download server that only listens on this PC. While setup runs, pip and the
// model downloader are pointed at it, so setup.exe is the only program that talks to the internet.
// That is the one program a firewall has to allow, and every download gets the same retries.
//
//	/pypi/simple/<name>/   the PyPI index page, with file links changed to point back here
//	/pyfiles/<path>        the package files (files.pythonhosted.org)
//	/hf/<path>             huggingface.co (redirects are kept inside the proxy)
//	/ext/<id>              a download address that huggingface.co sent us to
type proxy struct {
	pypi, files, hf string // upstream sites, without a trailing slash
	client          *http.Client
	logf            func(string)
	tries           int
	wait            time.Duration

	ln   net.Listener
	srv  *http.Server
	host string
	mu   sync.Mutex
	ext  map[string]string
	seen map[string]bool
}

func newProxy(client *http.Client, logf func(string)) *proxy {
	// The proxy follows redirects itself, so that every hop stays inside setup.exe.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.Timeout = 0
	return &proxy{pypi: "https://pypi.org", files: "https://files.pythonhosted.org", hf: "https://huggingface.co",
		client: &c, logf: logf, tries: 3, wait: 2 * time.Second, ext: map[string]string{}, seen: map[string]bool{}}
}

func (p *proxy) start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	p.ln, p.host = ln, ln.Addr().String()
	p.srv = &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: 30 * time.Second}
	go p.srv.Serve(ln)
	return nil
}

func (p *proxy) stop() {
	if p.srv != nil {
		p.srv.Close()
	}
}

func (p *proxy) base() string { return "http://" + p.host }

// env tells pip and the Hugging Face downloader to use this proxy.
func (p *proxy) env() []string {
	return []string{"PIP_INDEX_URL=" + p.base() + "/pypi/simple/", "PIP_TRUSTED_HOST=127.0.0.1",
		"HF_ENDPOINT=" + p.base() + "/hf", "HF_HUB_DISABLE_XET=1", "PIP_NO_INPUT=1"}
}

func (p *proxy) serve(w http.ResponseWriter, r *http.Request) {
	// Only programs on this PC, not web pages: wrong Host (DNS rebinding) or an Origin header means a browser.
	if r.Host != p.host || r.Header.Get("Origin") != "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/pypi/simple/"):
		p.index(w, r, strings.TrimPrefix(r.URL.Path, "/pypi/simple/"))
	case strings.HasPrefix(r.URL.Path, "/pyfiles/"):
		p.stream(w, r, p.files+"/"+cleanRel(strings.TrimPrefix(r.URL.Path, "/pyfiles/")), true)
	case strings.HasPrefix(r.URL.Path, "/hf/"):
		u := p.hf + "/" + strings.TrimPrefix(r.URL.Path, "/hf/")
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		p.stream(w, r, u, r.Method == http.MethodGet)
	case strings.HasPrefix(r.URL.Path, "/ext/"):
		p.mu.Lock()
		u, ok := p.ext[strings.TrimPrefix(r.URL.Path, "/ext/")]
		p.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		p.stream(w, r, u, r.Method == http.MethodGet)
	default:
		http.NotFound(w, r)
	}
}

// cleanRel keeps a path from climbing out of the upstream site.
func cleanRel(s string) string { return strings.TrimPrefix(path.Clean("/"+s), "/") }

func (p *proxy) note(u string) {
	pu, err := url.Parse(u)
	if err != nil {
		return
	}
	p.mu.Lock()
	first := !p.seen[pu.Host]
	p.seen[pu.Host] = true
	p.mu.Unlock()
	if first && p.logf != nil {
		p.logf("Setup is downloading from " + pu.Host)
	}
}

// do sends one request and retries network errors and server errors a few times.
func (p *proxy) do(ctx context.Context, method, u string, hdr http.Header) (*http.Response, error) {
	var lastErr error
	for try := 0; try < p.tries; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(p.wait):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return nil, err
		}
		for _, k := range []string{"Range", "If-Range", "Accept", "Accept-Encoding", "User-Agent"} {
			if v := hdr.Get(k); v != "" {
				req.Header.Set(k, v)
			}
		}
		resp, err := p.client.Do(req)
		if err == nil && resp.StatusCode < 500 {
			return resp, nil
		}
		if err == nil {
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
			resp.Body.Close()
		}
		lastErr = err
	}
	return nil, lastErr
}

func (p *proxy) index(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || strings.ContainsAny(strings.TrimSuffix(name, "/"), "/?#\\") {
		http.NotFound(w, r)
		return
	}
	u := p.pypi + "/simple/" + name
	p.note(u)
	h := http.Header{"Accept": {"text/html"}}
	resp, err := p.do(r.Context(), http.MethodGet, u, h)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, resp.Status, resp.StatusCode)
		return
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out := strings.ReplaceAll(string(b), p.files+"/", p.base()+"/pyfiles/")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, out)
}

// stream sends a download on to the caller. follow says whether redirects stay inside the proxy
// (a download) or are handed back with a changed address (a HEAD request that wants the headers).
func (p *proxy) stream(w http.ResponseWriter, r *http.Request, u string, follow bool) {
	for hop := 0; hop < 8; hop++ {
		p.note(u)
		resp, err := p.do(r.Context(), r.Method, u, r.Header)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "" {
			loc, perr := url.Parse(resp.Header.Get("Location"))
			resp.Body.Close()
			if perr != nil {
				http.Error(w, perr.Error(), http.StatusBadGateway)
				return
			}
			next := (&url.URL{}).ResolveReference(loc)
			if base, e := url.Parse(u); e == nil {
				next = base.ResolveReference(loc)
			}
			if follow {
				u = next.String()
				continue
			}
			// HEAD: give the headers back, but make the next address lead through the proxy again.
			for k, v := range resp.Header {
				if k != "Location" {
					w.Header()[k] = v
				}
			}
			w.Header().Set("Location", p.rewrite(next.String()))
			w.WriteHeader(resp.StatusCode)
			return
		}
		defer resp.Body.Close()
		for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Etag", "Last-Modified", "Content-Disposition",
			"X-Repo-Commit", "X-Linked-Etag", "X-Linked-Size", "X-Xet-Hash"} {
			if v := resp.Header.Get(k); v != "" {
				w.Header().Set(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		if r.Method != http.MethodHead {
			io.Copy(w, resp.Body)
		}
		return
	}
	http.Error(w, "too many redirects", http.StatusBadGateway)
}

// rewrite turns an address on the upstream site into one that goes through the proxy.
func (p *proxy) rewrite(u string) string {
	if strings.HasPrefix(u, p.hf+"/") {
		return p.base() + "/hf/" + strings.TrimPrefix(u, p.hf+"/")
	}
	b := make([]byte, 12)
	rand.Read(b)
	id := hex.EncodeToString(b)
	p.mu.Lock()
	p.ext[id] = u
	p.mu.Unlock()
	return p.base() + "/ext/" + id
}
