---
id: policy:fidelity-first
type: policy
title: Fidelity Over Convenience
---
When osmem cannot reproduce OpenSearch behaviour it fails loudly with an OpenSearch-shaped error instead of approximating silently, so a test that passes on osmem is expected to pass on OpenSearch.

```yaml
summary:
  rules:
    - unknown or unsupported query/aggregation -> 400 with reason "not supported by osmem"
    - response shapes, status codes and error types match OpenSearch 2.x
    - known approximations are documented (scores, analyzers)
    - regression tests capture every fidelity fix (review_test.go)
  references:
    - decision:bleve-engine
    - api:rest-compat
```
