---
id: doc:compatibility
type: doc
title: Compatibility Page
---
What osmem implements, what it approximates, and what returns an error, so readers can judge whether a test on osmem proves anything about OpenSearch.

```yaml
summary:
  sections:
    - supported APIs, cat APIs, queries, aggregations, mapping types, analysis
    - approximations: BM25 scores, bleve analyzers for indexing, exact cardinality, t-digest percentile edge cases, numeric range/sort precision, text fielddata aggregation, ignored _doc_count, routing without shard placement, ignored search options (timeout, profile, stats, runtime_mappings)
    - known gaps: kNN search and suggesters on knn_vector/completion, regexp determinization limit, a different error reported when a body has several problems, search body key validation, validate explain/rewrite, zero statistics counters, JSON key order
    - unsupported (400): Painless scripts, suggesters, kNN, span/intervals queries, data streams, ingest pipelines, rollover/shrink/split, _tasks, node statistics, YAML/CBOR/SMILE bodies, ...
    - error shapes and status codes: root_cause/caused_by chains, search phase shard failures, parse error positions ([line:col], line/col), per-route URL parameter validation, Content-Type 406, flat 405 with Allow
    - client notes: opensearch-go, go-elasticsearch (product header), olivere (sniffing), compatibility mode
    - differential audit vs OpenSearch 3.8.0 (2026-09-16): 477 scenarios, 6139 requests; differing requests 3263 -> 308; harness not in repository
    - regression tests cover asserted cases; no full OpenSearch REST YAML differential runner
  references:
    - api:rest-compat
    - policy:fidelity-first
    - decision:bleve-engine
```
