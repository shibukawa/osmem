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
    - approximations: BM25 scores, bleve analyzers for indexing, exact cardinality, t-digest percentile edge cases, numeric range/sort precision, text fielddata aggregation, ignored _doc_count, ignored search options (timeout, profile timings, stats, runtime_mappings), phrase suggester scoring (stupid-backoff n-gram, not Lucene's smoothed LM)
    - routing: get/exists/delete/update/mget now hash routing like OpenSearch and miss on a wrong/missing value for multi-shard indices; storage is still not re-keyed by (routing, id), so the same _id under another routing updates rather than coexists
    - known gaps: kNN search on knn_vector, hdr method of percentiles, geo_shape against an actual geo_shape field (bounding-box only), regexp determinization limit, a different error reported when a body has several problems, search body key validation, validate explain/rewrite, zero statistics counters, JSON key order, completion suggestion contexts/typed_keys, aggregation profile always empty
    - unsupported (400): Painless scripts, kNN, most span queries (span_or/first/not/containing/within, field_masking_span), data streams, ingest pipelines, rollover/shrink/split, _tasks, node statistics, YAML/CBOR/SMILE bodies, ...
    - error shapes and status codes: root_cause/caused_by chains, search phase shard failures, parse error positions ([line:col], line/col), per-route URL parameter validation, Content-Type 406, flat 405 with Allow
    - client notes: opensearch-go, go-elasticsearch (product header), olivere (sniffing), compatibility mode
    - differential audit vs OpenSearch 3.8.0 (2026-09-16): 477 scenarios, 6139 requests; differing requests 3263 -> 308; harness not in repository
    - second differential audit, OpenSearch's own REST YAML suite (2026-09-16): 1200 scenarios; differing 495 -> 244 (excl. score-only/profile-timing diffs) after adding intervals, suggest (term/phrase/completion+contexts), rare_terms/significant_terms/significant_text/auto_date_histogram/variable_width_histogram, more_like_this, distance_feature, geo_shape (geo_point), span_term/near/multi, _search_shards, term vectors, routing shard-mismatch, and fixing unsigned_long aggregations, date_range bounds, dynamic search.max_buckets; harness not in repository
    - regression tests cover asserted cases; no full OpenSearch REST YAML differential runner
  references:
    - api:rest-compat
    - policy:fidelity-first
    - decision:bleve-engine
```
