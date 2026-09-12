#!/usr/bin/env bash
# Builds osmem-server for all platforms and places the binaries into the
# npm platform packages under packages/node/platforms/.
set -euo pipefail
cd "$(dirname "$0")/.."
scripts/build-binaries.sh
# go target -> npm package suffix (process.platform-process.arch)
for pair in darwin-arm64:darwin-arm64 linux-amd64:linux-x64 linux-arm64:linux-arm64 windows-amd64:win32-x64 windows-arm64:win32-arm64; do
  go=${pair%%:*}
  npm=${pair##*:}
  src="dist/$go/osmem-server"; [ -f "$src.exe" ] && src="$src.exe"
  dst="packages/node/platforms/$npm/bin/$(basename "$src")"
  mkdir -p "$(dirname "$dst")"
  cp "$src" "$dst"
  echo "$dst"
done
