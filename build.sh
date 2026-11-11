#!/bin/sh
# Builds dist/SimpleVoice-OBS-Setup.exe (needs Go 1.24 or newer; works on Linux, macOS and Windows with a POSIX shell).
# The installer contains the relay program, so the relay is built first and copied into setup/payload/.
set -e
cd "$(dirname "$0")"
mkdir -p dist setup/payload
(cd relay && GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o ../setup/payload/SimpleVoiceOBSRelay.exe .)
cp relay/worker.py LICENSE THIRD-PARTY-NOTICES.txt setup/payload/
(cd setup && GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o ../dist/SimpleVoice-OBS-Setup.exe .)
ls -la dist/SimpleVoice-OBS-Setup.exe
