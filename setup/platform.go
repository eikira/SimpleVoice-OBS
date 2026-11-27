// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) eikira

package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// isMac is true on a Mac. Everything that differs from Windows is behind this one switch, so the
// Windows build behaves exactly as before. Only Apple Silicon Macs are supported.
var isMac = runtime.GOOS == "darwin"

// isLinux is true on Linux (tested target: Ubuntu, 64 bit Intel/AMD). isUnix is either of the two.
var isLinux = runtime.GOOS == "linux"
var isUnix = isMac || isLinux

func unixPick(mac, linux, other string) string {
	switch {
	case isMac:
		return mac
	case isLinux:
		return linux
	}
	return other
}

// Python for the Mac: the python-build-standalone project (Astral) ships a normal, relocatable
// Python with pip included. Hash from the release's SHA256SUMS file.
const (
	macPythonURL = "https://github.com/astral-sh/python-build-standalone/releases/download/20261110/cpython-3.11.18%2B20261110-aarch64-apple-darwin-install_only.tar.gz"
	macPythonSHA = "73f1ed14486aa278d331e95f7d6d452f5cfb69d90b418e26e62fd4846d53557d"
)

// Python for Linux (64 bit Intel/AMD), same project and release.
const (
	linuxPythonURL = "https://github.com/astral-sh/python-build-standalone/releases/download/20261110/cpython-3.11.18%2B20261110-x86_64-unknown-linux-gnu-install_only.tar.gz"
	linuxPythonSHA = "ac0b758b6ae4ba1cb87f7091eb6bb17c9a383a9bcdb5c18493cdd4f34e35ae8c"
)

// On the Mac and on Linux, PyPI itself carries PyTorch for Apple Silicon (with the Metal "MPS" backend), so pip
// installs it from there. The version is the same as on Windows.
var macTorchPkgs = []string{"torch==2.8.0", "torchaudio==2.8.0"}

// untarGz unpacks a .tar.gz into dst. It refuses paths that leave dst, and keeps symbolic links
// that point inside dst (Python's folder has some).
func untarGz(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	root, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	inside := func(p string) bool { return p == root || strings.HasPrefix(p, root+string(filepath.Separator)) }
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(root, filepath.FromSlash(h.Name))
		if !inside(target) {
			return fmt.Errorf("unsafe path in archive: %s", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(target), 0o755); err == nil {
				err = writeTarFile(target, tr, os.FileMode(h.Mode)&0o777|0o200)
			}
		case tar.TypeSymlink:
			link := h.Linkname
			resolved := link
			if !filepath.IsAbs(link) {
				resolved = filepath.Join(filepath.Dir(target), link)
			}
			if !inside(filepath.Clean(resolved)) {
				return fmt.Errorf("unsafe link in archive: %s -> %s", h.Name, h.Linkname)
			}
			if err = os.MkdirAll(filepath.Dir(target), 0o755); err == nil {
				os.Remove(target)
				err = os.Symlink(link, target)
			}
		case tar.TypeLink:
			from := filepath.Join(root, filepath.FromSlash(h.Linkname))
			if !inside(from) {
				return fmt.Errorf("unsafe link in archive: %s", h.Name)
			}
			if err = os.MkdirAll(filepath.Dir(target), 0o755); err == nil {
				os.Remove(target)
				if err = os.Link(from, target); err != nil { // no hard links here: copy instead
					err = copyFile(from, target)
				}
			}
		}
		if err != nil {
			return err
		}
	}
}

func writeTarFile(path string, r io.Reader, mode os.FileMode) error {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// macCheckMachine stops early on a Mac that is not Apple Silicon (no PyTorch exists for it).
// linuxArch is the processor type on Linux; a variable so tests can change it.
var linuxArch = runtime.GOARCH

// macArch is the processor type; a variable so tests can change it.
var macArch = runtime.GOARCH

func macCheckMachine() error {
	if macArch != "arm64" {
		return errors.New("this Mac does not have an Apple Silicon chip (M1 or newer). PyTorch has no version for Intel Macs, so SimpleVoice OBS cannot run here")
	}
	return nil
}
