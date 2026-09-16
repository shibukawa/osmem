---
id: doc:performance
type: doc
title: Performance and Linked Binary Footprint Page
---
Local benchmark results include Go embedded and Python/Java/Node server SDK startup with Japanese analysis enabled/disabled, clone creation for mutation tests, query samples, linked Go application delta, process and container memory, Docker Hub compressed image size, Go Testcontainers overhead with a 1 GiB data tmpfs, and Devbox service startup/memory. The homepage highlights Go embedded startup with Japanese analysis off/on alongside container paths; this page retains the full Python/Java/Node startup matrix, workloads, timer boundaries, scope, and exclusions.

```yaml
summary:
  sections:
    - local measurements and exact workloads
    - write/sort performance before-after (decision:in-memory-segment-merge, decision:sort-execution), and two same-session regressions found by re-measuring and fixed (decision:shared-route-table, decision:nested-filter-scoping), with one remaining known cost (bool-query search, an inherent Lucene-fidelity trade-off) left open and explained
    - startup matrix: Go embedded; Python, Java, and Node server SDKs; Japanese analysis on/off; Docker, Testcontainers, and Devbox context rows
    - clone creation average for in-process Go and server SDK APIs, with mutations excluded
    - Go Testcontainers per-container startup with runner RSS delta from pre-start baseline
    - Devbox services up startup and process-compose/Docker CLI plus engine RSS
    - homepage bar charts for Go embedded startup with Japanese analysis off/on, memory, linked Go binary increase, Devbox initial closure download, and Docker Hub compressed image size; full SDK startup matrix stays here
    - linked Go executable size delta compared on one MB scale with environment and image downloads, with distinct scope labels
    - reproducible Go benchmark command
  principles:
    - no cross-workload engine ranking
    - separate process readiness, schema setup, and query latency
    - report Docker Hub compressed image size for download comparison, not local expanded size
    - report library footprint as linked executable delta, not source or package artifact size
    - show RSS and Docker-reported container memory with process scope and fixture conditions; subtract the pre-start Go Testcontainers runner RSS before adding its increment
    - Devbox manages an OpenSearch service through process-compose; separate its resident manager/client RSS from the container
    - compare binary increment and download figures visually while stating that they are different artifact boundaries
    - disclose that Java server SDK startup includes default classpath binary extraction; language runner startup is excluded for all SDKs
    - omit write latency; focus on startup and queries over prebuilt indices
  references:
    - metric:local-benchmark
    - doc:docs-site
```
