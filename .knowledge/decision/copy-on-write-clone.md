---
id: decision:copy-on-write-clone
type: decision
title: Copy-on-Write Clone per Index
---
Clone copies the cluster map only; each index is reference-counted and duplicated (documents re-indexed into a fresh bleve index) the first time any holder writes to it.

```yaml
summary:
  decided: 2026-09-11
  rationale: read-only clones in microseconds; writers pay once per touched index; base stays immutable in practice
  invariants:
    - shared index is never mutated; writer detaches first
    - refcount 0 closes the bleve index (frees goroutines/memory)
    - aliases, mappings, docs map are part of the copied unit
  measured:
    clone_read_only_ms: 0.4
    clone_first_write_10k_docs_ms: 400
  references:
    - concept:clone
    - requirement:go-test-lifecycle
```
