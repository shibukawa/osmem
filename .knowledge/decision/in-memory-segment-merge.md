---
id: decision:in-memory-segment-merge
type: decision
title: In-Memory Segment Merging
---
Each index tracks which root documents every bleve segment holds (a run) and merges runs by re-indexing their current documents, because the in-memory scorch index of decision:bleve-engine never merges segments and one-document writes otherwise kept one segment each.

```yaml
summary:
  decided: 2026-09-16
  cause:
    - scorch starts persisterLoop and mergerLoop only with a path (bleve v2.6.1 Scorch.Open)
    - every committed batch adds a segment; IndexDoc, UpdateDoc and DeleteDoc commit one batch per request
    - zapx v17 pre-sizes a new segment buffer for len(results)+NewSegmentBufferNumResultsBump(100) documents, so a one-document segment keeps ~100 documents of capacity (bleve only, 2000 one-document batches 331.6 MiB, bump 0 9.4 MiB, one batch 1.1 MiB)
    - scorch stat num_root_memorysegments counts the previous root including dropped segments; count segments with IndexSnapshot.Segments
  mechanism:
    - Index.runs mirrors the segments of the current snapshot, oldest first; Index.runOf maps a root document to the run indexing its current version
    - Index.commit applies a batch and moves documents between runs; a run without live documents is forgotten because the scorch introducer drops segments with LiveSize 0
    - size classes by live documents 1-9, 10-99, 100-999; runMergeFactor (10) runs of one class are re-indexed together, so a document is re-indexed at most once per class
    - runs with 1000+ live documents (bulk requests, rebuilds) are rewritten only when fewer than half of the documents they indexed remain
    - merging re-indexes stored documents with buildDocument(infer=false), reindexBatchDocs (1000) per batch, inside the write request after the stored documents reflect it
    - PutMapping increments Index.mappingGen; runs of older generations are never merged, because OpenSearch does not re-index existing documents for fields added to the mapping
    - a document that no longer indexes (index.mapping.coerce turned off) pins its run; the write still succeeds
  invariants:
    - segments <= documents/1000 + 27, excluding pinned runs and runs from before the last mapping update
    - writes stay visible to the next search
    - only writable (unshared) indices merge, so clones and point-in-time contexts keep their snapshot
  measured:
    setup: Apple M3, go1.27.0, BenchmarkSingleDocWrites, before and after binaries interleaved, best of 3, [before, after]
    single_writes_1000: {heap_mib: [438, 13.1], term_query_us: [675, 38], write_us: [435, 476]}
    single_writes_5000: {heap_mib: [2194, 24.3], term_query_us: [6697, 34], write_us: [1472, 472]}
    unchanged: BulkIndex10k ~0.43 s, CloneReadOnly ~0.48 ms, CloneFirstWrite10k ~0.42 s, Search10k ~2 ms, TermQuery10k ~70 us (median of 5 on a loaded host)
    bulk_heap_per_document: +1-2% (runOf and run ids)
  rejected:
    - full rebuild at a segment threshold: O(documents) stall per threshold
    - NewSegmentBufferNumResultsBump=0 at init: process-global dependency state; with merging it changed benchmark heap by about 1 MiB
    - scorch with a path: writes to disk (requirement:in-memory-operation)
    - deferring commits to the next search: segments still accumulate when writes and searches alternate
  tests: internal/engine/segments_test.go, BenchmarkSingleDocWrites
  references:
    - decision:bleve-engine
    - decision:copy-on-write-clone
    - requirement:in-memory-operation
```
