#!/usr/bin/env bash
# Builds osmem-server for all platforms and places the binaries into the
# npm platform packages under packages/node/platforms/.
set -euo pipefail
cd "$(dirname "$0")/.."
scripts/build-binaries.sh
declare -A MAP=([darwin-arm64]=darwin-arm64 [darwin-amd64]=darwin-x64 [linux-amd64]=linux-x64 [linux-arm64]=linux-arm64 [windows-amd64]=win32-x64 [windows-arm64]=win32-arm64)
for go in "${!MAP[@]}"; do
  npm=${MAP[$go]}
  src="dist/$go/osmem-server"; [ -f "$src.exe" ] && src="$src.exe"
  dst="packages/node/platforms/$npm/bin/$(basename "$src")"
  mkdir -p "$(dirname "$dst")"
  cp "$src" "$dst"
  echo "$dst"
done
