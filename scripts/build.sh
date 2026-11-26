#!/usr/bin/env bash
# Build krystallos with the web UI inside: the web build first, then the Go
# build that embeds it (-tags webui).
#
#   scripts/build.sh [OUTPUT]
#
# OUTPUT defaults to bin/kry. GOOS and GOARCH pass through to go build,
# and the web build is the same for every platform, so
#
#   GOOS=linux GOARCH=amd64 scripts/build.sh bin/kry-linux-amd64
#
# cross-compiles (the SQLite driver is pure Go).
set -euo pipefail

out=${1:-bin/kry}
case $out in
/*) ;;
*) out=$PWD/$out ;;
esac
cd "$(dirname "$0")/.."

# Install the web dependencies from the lockfile when they are missing or
# older than it; otherwise keep them, so a rebuild stays quick.
if [ ! -f web/node_modules/.package-lock.json ] || [ web/package-lock.json -nt web/node_modules/.package-lock.json ]; then
	npm --prefix web ci
fi
npm --prefix web run build

go build -tags webui -o "$out" ./cmd/kry
echo "built $out"
