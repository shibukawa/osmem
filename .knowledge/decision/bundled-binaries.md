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
    pypi: project osmem-server (PyPI name osmem is taken by an unrelated tool and os-mem is rejected as too similar), import osmem_server; one wheel per platform tag containing the binary
    maven: groupId io.github.shibukawa.osmem (namespace auto-verified via GitHub sign-in; decided 2026-09-12 over jp.shibu), Java package io.github.shibukawa.osmem, artifacts osmem + osmem-server-binaries with os-arch classifier jars; multi-module pom with release profile (sources, javadoc, gpg, central-publishing-maven-plugin 0.11.0)
  cost: each package artifact ~30 MB (stripped Go binary including system:kagome)
  scripts: scripts/build-binaries.sh (cross-compile), build-npm.sh, build-python-wheels.sh (setup.py forces platform wheel tags), build-java-binaries.sh (jar tool)
  publishing: release.yml on tag vX.Y.Z; npm via OIDC trusted publishing (first version published by hand, done 2026-09-12), PyPI via pending trusted publisher, Maven Central via portal token + GPG secrets
  references:
    - requirement:multi-language-clients
    - decision:kuromoji-as-plugin
```
