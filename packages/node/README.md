# npm packages

- `core/`: the `@osmem/core` package (launcher + clone helpers, pure JavaScript).
- `platforms/<platform>/` (darwin-arm64, linux-x64, linux-arm64, win32-x64, win32-arm64): `@osmem/<platform>` packages that only
  contain the binary; `@osmem/core` lists them as optional dependencies so npm
  installs the one matching the host.

`scripts/build-npm.sh` builds the binaries with `scripts/build-binaries.sh`
and copies each into `platforms/<platform>/bin/`. Publish the platform
packages first, then `@osmem/core`.
