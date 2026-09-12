# npm packages

- `osmem/`: the `osmem` package (launcher + clone helpers, pure JavaScript).
- `platforms/<platform>/`: `osmem-server-<platform>` packages that only
  contain the binary; `osmem` lists them as optional dependencies so npm
  installs the one matching the host.

`scripts/build-npm.sh` builds the binaries with `scripts/build-binaries.sh`
and copies each into `platforms/<platform>/bin/`. Publish the platform
packages first, then `osmem`.
