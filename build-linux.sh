#!/bin/sh
# Builds the Linux (64 bit Intel/AMD) installer: dist/SimpleVoice-OBS-Setup-linux-amd64
# Needs Go 1.24 or newer. Works on Linux, macOS and Windows (Git Bash); it cross-compiles.
set -e
cd "$(dirname "$0")"
mkdir -p dist setup/payload
rm -f setup/payload/SimpleVoiceOBSRelay.exe setup/payload/SimpleVoiceOBSRelay
(cd relay && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ../setup/payload/SimpleVoiceOBSRelay .)
cp relay/worker.py LICENSE THIRD-PARTY-NOTICES.txt setup/payload/
(cd setup && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ../dist/SimpleVoice-OBS-Setup-linux-amd64 .)
rm -f setup/payload/SimpleVoiceOBSRelay
ls -la dist/SimpleVoice-OBS-Setup-linux-amd64
