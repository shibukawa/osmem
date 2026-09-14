---
id: doc:compatibility
type: doc
title: Compatibility Page
---
What osmem implements, what it approximates, and what returns an error, so readers can judge whether a test on osmem proves anything about OpenSearch.

```yaml
summary:
  sections:
    - supported APIs, queries, aggregations, mapping types, analysis
    - approximations: scores, analyzers, cardinality exactness, numeric range/sort precision, ignored ignore_malformed, text fielddata aggregation, ignored _doc_count, multi-shard terminate_after
    - unsupported (400): Painless scripts, suggesters, kNN, span queries, ...
    - accepted but ignored: function_score functions, selected search options
    - error shapes and status codes
    - client notes: opensearch-go, go-elasticsearch (product header), olivere (sniffing), compatibility mode
    - regression tests cover asserted cases; no full OpenSearch REST YAML differential runner
  references:
    - api:rest-compat
    - policy:fidelity-first
    - decision:bleve-engine
```
