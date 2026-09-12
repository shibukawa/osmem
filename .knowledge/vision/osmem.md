---
id: vision:osmem
type: vision
title: osmem Vision
---
OpenSearch-compatible fake that runs inside a test process so search-dependent code is tested without Docker, JVM, or disk state.

```yaml
summary:
  audience: developers writing automated tests against OpenSearch clients
  sibling: pgmem (same idea for PostgreSQL)
  non_goals:
    - production search
    - Lucene score parity
    - million-document indices
  references:
    - requirement:in-memory-operation
    - requirement:go-test-lifecycle
    - requirement:multi-language-clients
    - policy:fidelity-first
```
