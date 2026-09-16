---
id: decision:sort-execution
type: decision
title: Sort Execution
---
Searches compute typed sort keys with sort specs resolved once per index, select only the first from+size hits with a bounded heap, and take bleve matches without its top-n store, returning exactly the hits and sort values of the full stable sort they replaced.

```yaml
summary:
  decided: 2026-09-16
  cause:
    - profile before (100k documents, match_all, sort, size 10): sort.SliceStable 55%, key extraction 28% (resolve, path split and []any allocations per hit), bleve top-n collection 16% of search time
    - bleve's top-n store sorts every collected match in Final although osmem orders hits itself
  mechanism:
    - sortField resolves a spec per index (field, nested ancestor, date format, missing key) and reads keys straight from the source; fielddata, _seq_no/_version and nested sorts go through sortFieldValues
    - sortKey holds a number, string, missing or other value; compareKeys equals compareSortValues
    - orderHits with limit from+size selects with a heap (topHits); scroll and collapse order every hit; size 0 only validates the sort
    - total order: sort keys, index name, sequence number, nested position, then position before sorting, which equals the stable sort
    - specs whose keys mix kinds or hold NaN keep sort.SliceStable over every hit, filtered by search_after afterwards
    - executeTargets collects matches through search.MakeDocumentMatchHandlerKey in index order; ranked (terminate_after) sorts them with search.CompareScoreDescending, the order of the top-n store
    - reported sort values are computed for returned hits only
    - aggregations no longer see hits reordered in place; sampler breaks score ties by index and document order itself
  fixed:
    - search_after filtered hits in place, corrupting the hit list aggregations read (documents counted twice or missing)
  measured:
    setup: Apple M3 with other load, go1.27.0, before and after binaries interleaved, median of 3, [before, after]
    sort100k_ms: {double: [243, 81], date: [282, 118], keyword: [254, 59], two_keys: [308, 85]}
    sort100k_allocs: {double: [1699591, 399613], two_keys: [2599602, 399635]}
    search10k_ms: [2.20, 1.75]
    clone_read_only_us: [499, 380]
    remaining: sortField.key 65% of sorted search time (source map lookups, date parsing), bleve collection 17%
  tests: internal/engine/sort_test.go (reference stable sort over random documents, specs, pages and search_after), BenchmarkSort100k
  references:
    - decision:bleve-engine
    - policy:fidelity-first
```
