#!/usr/bin/env bash
# Builds one wheel per platform, each bundling the matching osmem-server.
set -euo pipefail
cd "$(dirname "$0")/.."
scripts/build-binaries.sh
declare -A PLAT=([darwin-arm64]=macosx_11_0_arm64 [darwin-amd64]=macosx_10_13_x86_64 [linux-amd64]=manylinux_2_17_x86_64 [linux-arm64]=manylinux_2_17_aarch64 [windows-amd64]=win_amd64 [windows-arm64]=win_arm64)
for go in "${!PLAT[@]}"; do
  src="dist/$go/osmem-server"; [ -f "$src.exe" ] && src="$src.exe"
  rm -rf packages/python/osmem/bin packages/python/build
  mkdir -p packages/python/osmem/bin
  cp "$src" "packages/python/osmem/bin/$(basename "$src")"
  (cd packages/python && OSMEM_PLAT_NAME=${PLAT[$go]} python3 -m build --wheel --outdir ../../dist/wheels)
done
rm -rf packages/python/osmem/bin
ls dist/wheels
