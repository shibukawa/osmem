---
id: system:bleve
type: system
title: bleve
---
Pure Go full-text index library (github.com/blevesearch/bleve/v2) used as osmem's inverted index and scorer.

```yaml
summary:
  version: v2.6.1
  used: scorch in-memory index, analyzers/tokenizers/token filters, term/phrase/fuzzy/prefix/regexp/numeric/date/geo queries, BM25
  avoided: bleve mapping (documents built via IndexAdvanced), facets, NewMemOnly store
  references:
    - decision:bleve-engine
```
