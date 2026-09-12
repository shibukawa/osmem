---
id: decision:subprocess-for-other-languages
type: decision
title: Subprocess Model for Non-Go Languages
---
Non-Go test suites connect to a Go-built osmem binary started as a child process over HTTP; no in-process embedding (JNI, native extensions, wasm) is attempted.

```yaml
summary:
  decided: 2026-09-12
  rationale:
    - the REST API is the compatibility surface already; a subprocess reuses it unchanged
    - one implementation to maintain; no per-language port of bleve/engine
    - startup of the Go binary is tens of milliseconds, acceptable per test session
  consequences:
    - clone lifecycle must be reachable over HTTP (api:clone-admin-api)
    - seed data must be loadable via CLI flags or REST, not Go calls (api:server-cli)
    - binary distribution per language ecosystem becomes a packaging task
  rejected:
    - GraalVM/wasm embedding: OpenSearch itself is not embeddable; osmem is Go, embedding Go in JVM/CPython/Node adds cgo/FFI complexity for little gain
  references:
    - requirement:multi-language-clients
```
