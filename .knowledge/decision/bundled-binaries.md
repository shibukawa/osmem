---
id: decision:bundled-binaries
type: decision
title: Binaries Bundled in Language Packages
---
Each language package ships the osmem-server binaries inside the package artifact; no download at first use.

```yaml
summary:
  decided: 2026-09-12
  rationale:
    - download-on-first-use adds supply-chain, proxy, offline-CI and checksum handling burdens
    - test dependencies are installed by package managers that already verify artifacts
  packaging:
    npm: org "osmem"; main package @osmem/core, binaries in @osmem/<platform> optionalDependencies (esbuild layout)
    pypi: one wheel per platform tag containing the binary
    maven: classifier jars per os/arch, resolved by a small launcher
  cost: each package artifact ~30 MB (stripped Go binary including system:kagome)
  scripts: scripts/build-binaries.sh (cross-compile), build-npm.sh, build-python-wheels.sh (setup.py forces platform wheel tags), build-java-binaries.sh (jar tool)
  publishing: not automated yet; no CI workflow
  references:
    - requirement:multi-language-clients
    - decision:kuromoji-as-plugin
```
