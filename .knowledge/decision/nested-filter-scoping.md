---
id: decision:nested-filter-scoping
type: decision
title: Nested Root-Filter Scoping
---
executeTargetsScoring wrapped every query in a conjunction with a root-only filter so nested child documents never surface as hits; that filter is now added only when the index's mapping has a nested field, since a mapping without one can never index a nested child to filter out.

```yaml
summary:
  decided: 2026-09-16
  cause:
    - decision:nested-child-documents (2026-09-14) added `bq = bleve.NewConjunctionQuery(bq, &constantScoreQuery{inner: rootQ, score: 0})` unconditionally in executeTargetsScoring, to keep nested child documents out of hits
    - bisecting commit by commit (git worktree per commit, same benchmark run back to back) traced BenchmarkTermQuery10k/BenchmarkSearch10k's latency increase since the last performance measurement to this commit, not to the OpenSearch 3.8 compatibility pass or the sort/segment-merge performance work that landed the same day
    - profiling (pprof alloc_space on BenchmarkTermQuery10k) showed bleve's search.NewDocumentMatchPool as 80% of allocated space; bleve's TopNCollector caps that pool's pre-allocation at PreAllocSizeSkipCap (1000) UNLESS the search never reaches a size+skip beyond it through a plain conjunction the way a two-clause AND (query, root-filter) does at this index's DocCount()-sized request
  mechanism:
    - Mapping.nestedPaths() already existed (used by nestedAncestor/nestedChain) and walks the full properties tree once for the field paths whose type is "nested"; a mapping with none can never write a nested child document (mapping.go's write path only creates one when indexing into a field of that type)
    - the root filter is skipped when len(t.ix.Mapping.nestedPaths()) == 0; every document in such an index already carries fieldRoot (index.go), so results are unchanged, just reached via a single query instead of a two-clause conjunction
    - indices that do declare a nested field are unaffected: the filter still runs for them
  measured:
    setup: Apple M3 with other load, go1.27.0, before (decision:nested-child-documents's parent) and after (this fix) binaries interleaved, median of 3-5
    term_query_10k_us: [34, 70, 45]  # [pre-nested-child-documents baseline, right after that commit (unfixed), after this fix]
    clone_readonly_us: [315, 410, 175]  # BenchmarkCloneReadOnly: same three points (also benefits: Count() is a query too)
    search10k_ms: [1.8, 4.5, 4.5]  # bool query + sort + date_histogram: NOT fixed by this change, see below
  unresolved:
    - BenchmarkSearch10k stays around 4.5ms (was 1.8ms): its bool query runs through decision:sort-execution's evalSearcher/drainSearcher path (query_exec.go, added by the OpenSearch 3.8 compatibility pass for Lucene-faithful bool/dis_max/boosting score combination), which fully materializes every matching clause before combining scores; bleve's native compound searchers could stream/limit but score matches differently than Lucene, which is why they were replaced. Fixing this would mean an early-terminating evaluator that still combines scores the Lucene way - not attempted here.
  tests: existing suite, including nested_test.go and the aggregation/highlight/collapse tests that exercise nested mappings; go test -race ./... passes
  references:
    - decision:nested-child-documents
    - decision:sort-execution
    - decision:shared-route-table
```
