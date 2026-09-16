---
id: doc:getting-started
type: doc
title: Getting Started Page
---
Entry page: what osmem is, the shared model (base cluster, clones, frozen base), and where to go per language.

```yaml
summary:
  sections:
    - what it is and is not (no Docker/JVM; scores relative; Painless scripts read-only, mutating scripts unsupported)
    - core model: base -> seed once -> clone per test -> discard (concept:cluster, concept:clone)
    - choose your path: Go in-process vs osmem-server for Node/Python/Java
    - install commands for osmem and official language clients
    - link to local benchmark conditions and artifact sizes
    - links to language guides, seed data, management API, compatibility
  references:
    - requirement:seed-once-reuse
    - decision:copy-on-write-clone
```
