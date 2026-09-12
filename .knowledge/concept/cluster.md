---
id: concept:cluster
type: concept
title: Cluster
---
A Cluster is one complete OpenSearch-like state: indices with settings, mappings, documents and aliases, plus index templates, cluster settings, scroll and PIT contexts.

```yaml
summary:
  identity: independent object; many clusters coexist in one process
  serving: exposes http.Handler; zero or more HTTP listeners may front one cluster
  lifecycle: New -> (seed) -> Clone* -> Close
  references:
    - concept:clone
    - api:go-cluster-api
```
