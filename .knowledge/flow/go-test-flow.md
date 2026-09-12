---
id: flow:go-test-flow
type: flow
title: Go Test Flow
---
Lifecycle of a Go test package using osmem.

```yaml
flow:
  - step: TestMain creates base with osmem.New()
  - step: TestMain applies mappings, analyzers and bulk seed
  - step: m.Run()
  - step: each Test calls base.Clone(t); Cleanup registered
    branch:
      read_only: may use base directly
      parallel: t.Parallel tests each clone
  - step: test serves clone (Serve or Handler) and points client at URL
  - step: t.Cleanup closes server and clone
  - step: TestMain closes base after m.Run
references:
  - requirement:go-test-lifecycle
  - api:go-cluster-api
```
