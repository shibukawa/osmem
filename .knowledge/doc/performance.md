---
id: doc:performance
type: doc
title: Performance and Linked Binary Footprint Page
---
Local benchmark results with workloads, machine, startup/query timings, linked Go application binary delta, runtime RSS, Docker Hub compressed image download size, Testcontainers orchestration, and Devbox environment setup. The homepage presents average startup/RSS/binary metrics at a glance; this page retains workloads, scope, and exclusions.

```yaml
summary:
  sections:
    - local measurements and exact workloads
    - Testcontainers per-container startup and memory; Devbox warm command overhead and environment download footprint
    - compact homepage metrics link to this page for conditions and interpretation
    - linked Go executable size delta and Docker Hub compressed image download size
    - reproducible Go benchmark command
  principles:
    - no cross-workload engine ranking
    - separate process readiness, schema setup, and query latency
    - report Docker Hub compressed image size for download comparison, not local expanded size
    - report library footprint as linked executable delta, not source or package artifact size
    - show RSS with process scope and fixture conditions; distinguish container memory from the Testcontainers JVM
    - describe Devbox as environment management, not a search runtime
    - omit write latency; focus on startup and queries over prebuilt indices
  references:
    - metric:local-benchmark
    - doc:docs-site
```
