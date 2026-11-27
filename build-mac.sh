#!/bin/sh
# Builds the macOS (Apple Silicon only) installer: dist/SimpleVoice-OBS-Setup-macOS-arm64
# Needs Go 1.24 or newer. Works on Linux, macOS and Windows (Git Bash); it cross-compiles.
set -e
cd "$(dirname "$0")"
mkdir -p dist setup/payload
rm -f setup/payload/SimpleVoiceOBSRelay.exe setup/payload/SimpleVoiceOBSRelay
(cd relay && GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ../setup/payload/SimpleVoiceOBSRelay .)
cp relay/worker.py LICENSE THIRD-PARTY-NOTICES.txt setup/payload/
(cd setup && GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ../dist/SimpleVoice-OBS-Setup-macOS-arm64 .)
rm -f setup/payload/SimpleVoiceOBSRelay
ls -la dist/SimpleVoice-OBS-Setup-macOS-arm64
