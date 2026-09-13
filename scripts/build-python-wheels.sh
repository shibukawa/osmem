#!/usr/bin/env bash
# Builds one wheel per platform into dist/wheels/, each bundling the
# matching osmem-server. Uses `uv build` when available, else python -m build.
set -euo pipefail
cd "$(dirname "$0")/.."
scripts/build-binaries.sh
mkdir -p dist/wheels
build() {
  if command -v uv >/dev/null 2>&1; then
    (cd packages/python && uv build --wheel --out-dir ../../dist/wheels)
  else
    (cd packages/python && python3 -m build --wheel --outdir ../../dist/wheels)
  fi
}
for pair in darwin-arm64:macosx_11_0_arm64 linux-amd64:manylinux_2_17_x86_64 linux-arm64:manylinux_2_17_aarch64 windows-amd64:win_amd64 windows-arm64:win_arm64; do
  go=${pair%%:*}
  plat=${pair##*:}
  src="dist/$go/osmem-server"; [ -f "$src.exe" ] && src="$src.exe"
  rm -rf packages/python/os_mem/bin packages/python/build
  mkdir -p packages/python/os_mem/bin
  cp "$src" "packages/python/os_mem/bin/$(basename "$src")"
  OSMEM_PLAT_NAME=$plat build
done
rm -rf packages/python/os_mem/bin packages/python/build
ls -la dist/wheels
