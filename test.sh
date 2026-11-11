#!/bin/sh
# Runs every check: vet, unit tests with the race detector, a Windows compile check, and the
# extension-protocol tests (needs Node.js 18 or newer; skipped when node is not installed).
set -e
cd "$(dirname "$0")"
echo "== relay"
(cd relay && gofmt -l . | (! grep .) && go vet ./... && GOOS=windows go vet . && go test -race -count=1 ./...)
if command -v node >/dev/null 2>&1; then
  echo "== extension protocol tests"
  (cd relay && go build -o /tmp/simplevoice-relay-test . && for f in tests/*.test.js; do RELAY_BIN=/tmp/simplevoice-relay-test node --test "$f"; done; rm -f /tmp/simplevoice-relay-test)
fi
echo "== setup (needs the payload: build first)"
./build.sh >/dev/null
(cd setup && gofmt -l . | (! grep .) && go vet . && GOOS=windows go vet . && go test -race -count=1 .)
echo "ALL CHECKS PASSED"
