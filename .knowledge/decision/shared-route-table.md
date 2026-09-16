---
id: decision:shared-route-table
type: decision
title: Shared Route Table
---
newHTTPHandler rebuilt the several-hundred-route table and its lookup trie on every call, so every New() and Clone() paid that cost; both are now built once per process and shared, because no handler closure or trie node carries state specific to one Cluster.

```yaml
summary:
  decided: 2026-09-16
  cause:
    - wrap() (used by both New and Clone) called newHTTPHandler(eng), which called h.routes = buildRoutes() and h.router = newRouter(h.routes) unconditionally
    - buildRoutes constructs one route per REST endpoint (over 150) with a handlerFunc closure; commit 71962c6 (OpenSearch 3.8 REST-API compatibility) added per-route parameter validation and other metadata to that construction, making it markedly more expensive to repeat
    - profiling (pprof alloc_space on BenchmarkCloneReadOnly) showed buildRoutes.func1, addRouteMethods and (*router).insert as the largest allocators, ahead of the query the benchmark also runs
  mechanism:
    - every handlerFunc takes h *httpHandler as its first argument and reaches the cluster through h.c; buildRoutes and newRouter never close over a Cluster or engine.Cluster, so their output is identical on every call
    - sharedRoutes and sharedRouter are package-level vars computed once at init; newHTTPHandler now just points a fresh httpHandler at them
    - routes/router are read-only after construction (grepped for other writers: none), so sharing them across concurrent Clusters is safe; go test -race ./... passes
  measured:
    setup: Apple M3 with other load, go1.27.0, before and after binaries interleaved
    clone_alone_ns: [60100, 800]  # Go embedded Cluster.Clone(), pure, no query (before, after)
    clone_readonly_us: [405, 175]  # BenchmarkCloneReadOnly: Clone() + a _count query (before, after)
  tests: existing suite (no dedicated test: routes/router carry no per-instance state to assert on)
  references:
    - decision:in-memory-segment-merge
    - decision:copy-on-write-clone
```
