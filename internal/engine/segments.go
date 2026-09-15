package engine

import (
	"github.com/blevesearch/bleve/v2"
)

// Segment merging.
//
// Every committed bleve batch adds a segment to the index. scorch merges
// segments only while persisting them to disk, and osmem keeps indices in
// memory, so a stream of single-document writes would leave one segment per
// write and memory and search time would grow with the number of writes.
//
// The index therefore tracks which root documents each segment holds (a run)
// and merges runs itself: it re-indexes their current documents, which leaves
// the old segments without live documents, and scorch drops such segments.
// Runs are grouped into size classes by their live documents; runMergeFactor
// runs of one class are merged, so a class holds fewer runs than that and a
// document is re-indexed at most once per class. Larger runs, which bulk
// requests and index copies create, are rewritten only when more than half
// of the documents they indexed have been replaced or deleted.
//
// Runs indexed before a mapping update are left as they are: OpenSearch does
// not re-index existing documents when the mapping changes, and re-indexing
// them with the new mapping would make them match fields added since.
const (
	// runMergeFactor runs of one size class are merged into one.
	runMergeFactor = 10
	// runClasses is the number of size classes merged by size: runs with
	// fewer than 10, 100 and 1000 live documents.
	runClasses = 3
	// reindexBatchDocs documents are re-indexed per batch when runs are
	// merged or an index is rebuilt.
	reindexBatchDocs = 1000
)

// segmentRun stands for the segment a committed batch added: the root
// documents the batch indexed.
type segmentRun struct {
	ids    []string // root documents the batch indexed
	live   int      // how many of them this run still holds
	gen    int      // Index.mappingGen when the batch was committed
	pinned bool     // re-indexing one of its documents failed; not merged again
}

// docBatch is a bleve batch that also records, in order, the root documents
// it indexes and deletes.
type docBatch struct {
	*bleve.Batch
	roots []rootOp
}

type rootOp struct {
	id      string
	deleted bool
}

func (ix *Index) newBatch() *docBatch {
	return &docBatch{Batch: ix.bleve.NewBatch()}
}

// removeDocuments queues the deletion of a stored document and its nested
// objects.
func (ix *Index) removeDocuments(batch *docBatch, id string) {
	batch.Delete(id)
	for _, cid := range ix.children[id] {
		batch.Delete(cid)
	}
	batch.roots = append(batch.roots, rootOp{id: id, deleted: true})
}

// commit applies a batch to bleve and updates the runs: documents the batch
// replaces or deletes leave their previous run, runs left without live
// documents are forgotten (scorch has dropped their segments), and the
// documents the batch indexed form a new run.
func (ix *Index) commit(batch *docBatch) error {
	if batch.Size() == 0 {
		return nil
	}
	if err := ix.bleve.Batch(batch.Batch); err != nil {
		return err
	}
	var run *segmentRun
	for _, op := range batch.roots {
		if prev := ix.runOf[op.id]; prev != nil {
			if prev == run && !op.deleted {
				continue // indexed twice by this batch
			}
			prev.live--
			delete(ix.runOf, op.id)
		}
		if op.deleted {
			continue
		}
		if run == nil {
			run = &segmentRun{gen: ix.mappingGen}
		}
		run.ids = append(run.ids, op.id)
		run.live++
		ix.runOf[op.id] = run
	}
	runs := ix.runs[:0]
	for _, r := range ix.runs {
		if r.live > 0 {
			runs = append(runs, r)
		}
	}
	clear(ix.runs[len(runs):])
	ix.runs = runs
	if run != nil && run.live > 0 {
		ix.runs = append(ix.runs, run)
	}
	return nil
}

// compactRuns merges runs until no size class holds runMergeFactor mergeable
// runs and no large mergeable run is mostly obsolete. Writers call it after
// the stored documents reflect their writes, because merging re-indexes
// stored documents.
func (ix *Index) compactRuns() error {
	for {
		group := ix.runsToMerge()
		if group == nil {
			return nil
		}
		if err := ix.mergeRuns(group); err != nil {
			return err
		}
	}
}

// mergeable reports whether the documents of a run can be re-indexed as they
// were indexed: under the current mapping, without having failed before.
func (ix *Index) mergeable(r *segmentRun) bool {
	return !r.pinned && r.gen == ix.mappingGen
}

// runsToMerge returns the next runs to merge: the mergeable runs of a size
// class that holds runMergeFactor of them, or a large mergeable run whose
// batch indexed more than twice the documents it still holds.
func (ix *Index) runsToMerge() []*segmentRun {
	var counts [runClasses]int
	for _, r := range ix.runs {
		if !ix.mergeable(r) {
			continue
		}
		class := sizeClass(r.live)
		if class >= runClasses {
			if 2*r.live < len(r.ids) {
				return []*segmentRun{r}
			}
			continue
		}
		counts[class]++
		if counts[class] < runMergeFactor {
			continue
		}
		group := make([]*segmentRun, 0, runMergeFactor)
		for _, g := range ix.runs {
			if ix.mergeable(g) && sizeClass(g.live) == class {
				group = append(group, g)
			}
		}
		return group
	}
	return nil
}

// sizeClass returns the size class of a run with n live documents: 0 below
// runMergeFactor, 1 below runMergeFactor², and so on.
func sizeClass(n int) int {
	class := 0
	for ; n >= runMergeFactor; n /= runMergeFactor {
		class++
	}
	return class
}

// mergeRuns re-indexes the documents the runs hold, reindexBatchDocs per
// batch. When one of them can no longer be indexed (index.mapping.coerce
// turned off after it was written, for example), its run is pinned and keeps
// the documents not re-indexed yet.
func (ix *Index) mergeRuns(runs []*segmentRun) error {
	batch := ix.newBatch()
	queued := 0
	for _, r := range runs {
		for _, id := range r.ids {
			d := ix.docs[id]
			if d == nil || ix.runOf[id] != r {
				continue // replaced or deleted since
			}
			bds, err := ix.buildDocument(d, false)
			if err != nil {
				r.pinned = true
				return ix.commit(batch)
			}
			if err := ix.addDocuments(batch, id, bds); err != nil {
				return err
			}
			queued++
			if queued == reindexBatchDocs {
				if err := ix.commit(batch); err != nil {
					return err
				}
				batch, queued = ix.newBatch(), 0
			}
		}
	}
	return ix.commit(batch)
}
