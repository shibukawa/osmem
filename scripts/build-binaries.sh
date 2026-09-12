#!/usr/bin/env bash
# Cross-compiles osmem-server for every supported platform into
# dist/<os>-<arch>/osmem-server[.exe]. Pure Go, so this runs on any host
# (macOS included) without extra toolchains.
#
#   scripts/build-binaries.sh            # all targets
#   scripts/build-binaries.sh host       # only the current platform
#   scripts/build-binaries.sh linux-amd64 darwin-arm64
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
ALL="darwin-arm64 linux-amd64 linux-arm64 windows-amd64 windows-arm64"
if [ $# -eq 0 ]; then
  TARGETS=$ALL
elif [ "$1" = host ]; then
  TARGETS="$(go env GOOS)-$(go env GOARCH)"
else
  TARGETS="$*"
fi

for target in $TARGETS; do
  os=${target%-*}
  arch=${target#*-}
  out="dist/$target/osmem-server"
  [ "$os" = windows ] && out="$out.exe"
  mkdir -p "dist/$target"
  echo "building $out ($VERSION)"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o "$out" ./cmd/osmem-server
done
