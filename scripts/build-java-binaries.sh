#!/usr/bin/env bash
# Packs each cross-compiled osmem-server into a classifier jar:
# dist/java/osmem-server-binaries-<version>-<os>-<arch>.jar containing
# osmem/bin/<os>-<arch>/osmem-server[.exe].
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${VERSION:-0.1.0}
scripts/build-binaries.sh
mkdir -p dist/java
for dir in dist/*-*/; do
  target=$(basename "$dir")
  stage=$(mktemp -d)
  mkdir -p "$stage/osmem/bin/$target"
  cp "$dir"/osmem-server* "$stage/osmem/bin/$target/"
  jar --create --file "dist/java/osmem-server-binaries-$VERSION-$target.jar" -C "$stage" .
  rm -rf "$stage"
  echo "dist/java/osmem-server-binaries-$VERSION-$target.jar"
done
