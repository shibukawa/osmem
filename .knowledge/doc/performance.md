---
id: doc:performance
type: doc
title: Performance and Linked Binary Footprint Page
---
Local benchmark results with workloads, machine, startup/query timings, linked Go application binary delta, and Docker Hub compressed image download size. Distinguish native osmem, Docker OpenSearch, Testcontainers orchestration, and Devbox environment management; label unmeasured paths rather than infer values.

```yaml
summary:
  sections:
    - local measurements and exact workloads
    - Testcontainers and Devbox limits / measurement status
    - linked Go executable size delta and Docker Hub compressed image download size
    - reproducible Go benchmark command
  principles:
    - no cross-workload engine ranking
    - separate process readiness, schema setup, and query latency
    - report Docker Hub compressed image size for download comparison, not local expanded size
    - report library footprint as linked executable delta, not source or package artifact size
    - omit write latency; focus on startup and queries over prebuilt indices
  references:
    - metric:local-benchmark
    - doc:docs-site
```
