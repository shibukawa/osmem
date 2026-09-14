---
id: decision:nested-child-documents
type: decision
title: Nested Objects as Child Documents
---
Each object of a `nested` field is indexed as a bleve document of its own (id `root\0path\0offset`, field `_nested_path`), the root keeps a `_root` marker and no nested fields, and a `nested` query joins the matching objects back to the enclosing level; this replaced the earlier flattening so nested semantics, `inner_hits`, nested sorts and nested aggregations match OpenSearch.

```yaml
summary:
  decided: 2026-09-14
  rationale:
    - flattening matched clauses across different objects and could not produce inner_hits
    - Lucene's block-join layout is the model OpenSearch users reason about; mirroring it keeps every edge (plain queries never see nested fields, exists on the path matches nothing, collapse on a nested field forms one group)
  behaviour:
    - nested query: score_mode avg/sum/min/max/none, ignore_unmapped, inner_hits (name defaults to the path, _nested identities, full-path _source filtering, sort, from/size, fields, docvalue_fields, highlight, version); deeper inner_hits are hoisted or nested like OpenSearch
    - collapse inner_hits: one or many, group total, sort/size/_source/track_scores; members score 0 when the request has no query; documents without a value form one group and report no fields; total counts documents before collapsing
    - nested/reverse_nested aggregations count objects; sorts on nested fields need the nested option (path, filter)
    - responses were diffed structurally against OpenSearch 2.19 (scratch scripts in the session); the remaining difference is score values
  consequences:
    - a document write deletes the previous nested ids (Index.children) before re-indexing; clones rebuild them
    - promoting an inferred object to nested through _mapping re-indexes the index
    - inner_hits of nested queries are collected while building the query (queryBuilder.inner) and rendered per hit
  references:
    - decision:bleve-engine
    - policy:fidelity-first
```
