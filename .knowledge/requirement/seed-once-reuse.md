---
id: requirement:seed-once-reuse
type: requirement
title: Seed Once, Reuse Everywhere
---
Initialization (mappings, analyzers, seed documents) happens once per process; every test reuses that state through cheap clones or direct sharing.

```yaml
summary:
  why: search fixtures are stable; re-seeding per test wastes seconds
  mechanisms:
    - decision:copy-on-write-clone makes unchanged indices free to share
    - process-level singleton base created in TestMain (Go) or session fixture (other languages)
  constraints:
    - base must never be mutated after tests start, or clones taken later differ
    - parallel tests (t.Parallel) must each hold their own clone when writing
  references:
    - requirement:go-test-lifecycle
    - requirement:multi-language-clients
```
