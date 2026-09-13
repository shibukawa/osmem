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
    - approximations: scores, nested flattening, analyzers, cardinality exactness
    - unsupported (400): scripts, suggesters, kNN, span queries, ...
    - error shapes and status codes
    - client notes: opensearch-go, go-elasticsearch (product header), olivere (sniffing), compatibility mode
  references:
    - api:rest-compat
    - policy:fidelity-first
    - decision:bleve-engine
```
