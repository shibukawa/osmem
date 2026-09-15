---
id: decision:bleve-engine
type: decision
title: bleve as Inverted Index Only
---
osmem uses bleve (pure Go, in-memory scorch) solely as an inverted index and BM25 scorer; mapping semantics, sorting, source filtering, highlighting and aggregations are implemented in Go over stored documents.

```yaml
summary:
  decided: 2026-09-11
  rationale:
    - OpenSearch (Java) cannot be embedded; a reimplementation of observable behaviour is the only in-process option
    - bleve provides analyzers, phrase/fuzzy/prefix queries and scoring without cgo
    - Go-side aggregation over stored docs gives OpenSearch-shaped responses without bleve facet limits
  consequences:
    - scores are relative, not equal to Lucene; tests must not assert score values
    - each search materializes every matching document; fixture-sized indices only
    - bleve.NewMemOnly avoided (upsidedown store ~10x slower); scorch with empty path instead
    - scorch without a path never merges segments; indices merge them themselves (decision:in-memory-segment-merge)
  references:
    - system:bleve
    - policy:fidelity-first
```
