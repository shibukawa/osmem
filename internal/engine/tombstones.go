package engine

import (
	"runtime"
	"sync"
	"time"
	"weak"
)

// Delete tombstones.
//
// OpenSearch keeps the version and sequence number of deleted documents in
// the engine's version map for index.gc_deletes (60s by default): indexing
// the id again continues the version (PUT, PUT, DELETE, PUT gives version 4)
// and external versions are compared against the tombstone. osmem does the
// same, measuring the window with the cluster clock.
//
// Tombstones belong to an index's copy-on-write lineage. They are kept in a
// registry keyed by weak index pointers (an entry disappears with its index)
// with a short history of snapshots, so a copy made when a shared index is
// first written inherits the tombstones that existed at the copy's sequence
// number.

type tombstone struct {
	version int64
	seqNo   int64
	at      time.Time
}

type tombstoneSnapshot struct {
	seqNo int64
	items map[string]tombstone // never mutated
}

type tombstoneState struct {
	history []tombstoneSnapshot // newest last
}

const tombstoneHistory = 8

func (st *tombstoneState) current() map[string]tombstone {
	if st == nil || len(st.history) == 0 {
		return nil
	}
	return st.history[len(st.history)-1].items
}

// itemsAt returns the newest snapshot not newer than seqNo.
func (st *tombstoneState) itemsAt(seqNo int64) map[string]tombstone {
	for i := len(st.history) - 1; i >= 0; i-- {
		if st.history[i].seqNo <= seqNo {
			return st.history[i].items
		}
	}
	return nil
}

var tombstones = struct {
	sync.Mutex
	byIndex  map[weak.Pointer[Index]]*tombstoneState
	lastSeen map[weak.Pointer[Cluster]]map[string]weak.Pointer[Index]
}{
	byIndex:  map[weak.Pointer[Index]]*tombstoneState{},
	lastSeen: map[weak.Pointer[Cluster]]map[string]weak.Pointer[Index]{},
}

// gcDeletes is the index.gc_deletes window.
func gcDeletes(ix *Index) time.Duration {
	v, ok := getNested(ix.Settings, "index.gc_deletes")
	if !ok {
		return 60 * time.Second
	}
	s, _ := v.(string)
	if d, ok := parseDuration(s); ok {
		return d
	}
	if s == "-1" {
		return -1
	}
	return 60 * time.Second
}

// stateLocked returns the tombstones of ix, registering an empty state.
func stateLocked(ix *Index) *tombstoneState {
	key := weak.Make(ix)
	st := tombstones.byIndex[key]
	if st == nil {
		st = &tombstoneState{}
		tombstones.byIndex[key] = st
		runtime.AddCleanup(ix, func(k weak.Pointer[Index]) {
			tombstones.Lock()
			delete(tombstones.byIndex, k)
			tombstones.Unlock()
		}, key)
	}
	return st
}

// inheritTombstones gives a copy of an index the tombstones its source had
// at the copy's sequence number.
func inheritTombstones(from, to *Index) {
	if from == nil || to == nil || from == to || from.UUID != to.UUID {
		return
	}
	tombstones.Lock()
	defer tombstones.Unlock()
	inheritLocked(from, to)
}

func inheritLocked(from, to *Index) {
	if _, ok := tombstones.byIndex[weak.Make(to)]; ok {
		return
	}
	src := tombstones.byIndex[weak.Make(from)]
	if src == nil {
		return
	}
	items := src.itemsAt(to.seqNo)
	if len(items) == 0 {
		return
	}
	st := stateLocked(to)
	st.history = append(st.history, tombstoneSnapshot{seqNo: to.seqNo, items: items})
}

// noteIndex records the index a cluster uses for a name; if the cluster's
// copy was replaced (a copy-on-write copy made by an index-level write),
// the new copy inherits the tombstones.
func (c *Cluster) noteIndex(ix *Index) {
	if ix == nil {
		return
	}
	tombstones.Lock()
	defer tombstones.Unlock()
	ck := weak.Make(c)
	seen := tombstones.lastSeen[ck]
	if seen == nil {
		seen = map[string]weak.Pointer[Index]{}
		tombstones.lastSeen[ck] = seen
		runtime.AddCleanup(c, func(k weak.Pointer[Cluster]) {
			tombstones.Lock()
			delete(tombstones.lastSeen, k)
			tombstones.Unlock()
		}, ck)
	}
	ik := weak.Make(ix)
	if prev, ok := seen[ix.Name]; ok && prev != ik {
		if p := prev.Value(); p != nil && p.UUID == ix.UUID {
			inheritLocked(p, ix)
		}
	}
	seen[ix.Name] = ik
}

// docWritable is writable for document writes, carrying tombstones over to
// a copy-on-write copy.
func (c *Cluster) docWritable(name string) (*Index, error) {
	before := c.indices[name]
	ix, err := c.writable(name)
	if err != nil {
		return nil, err
	}
	inheritTombstones(before, ix)
	c.noteIndex(ix)
	return ix, nil
}

// docEnsureIndex is ensureIndex for document writes.
func (c *Cluster) docEnsureIndex(name string) (*Index, error) {
	before := c.indices[name]
	if before == nil {
		w, err := c.resolveWriteIndex(name)
		if err != nil && len(c.aliasTargets(name)) > 0 {
			// An alias without a usable write index fails the write; it
			// must never auto-create an index named after the alias.
			return nil, err
		}
		before = w
	}
	ix, err := c.ensureIndex(name)
	if err != nil {
		return nil, err
	}
	if err := c.checkBlock(ix, blockWrite); err != nil {
		return nil, err
	}
	if before != nil {
		inheritTombstones(before, ix)
	}
	c.noteIndex(ix)
	return ix, nil
}

// docTx collects the tombstones written by one request so later operations
// of the same request see them; commit publishes them.
type docTx struct {
	now     time.Time
	pending map[*Index]map[string]tombstone
	removed map[*Index]map[string]bool
}

func (c *Cluster) newDocTx() *docTx {
	return &docTx{now: c.now(), pending: map[*Index]map[string]tombstone{}, removed: map[*Index]map[string]bool{}}
}

// tombstone returns the live tombstone of a document id.
func (tx *docTx) tombstone(ix *Index, id string) (tombstone, bool) {
	if tx.removed[ix][id] {
		return tombstone{}, false
	}
	t, ok := tx.pending[ix][id]
	if !ok {
		tombstones.Lock()
		st := tombstones.byIndex[weak.Make(ix)]
		t, ok = st.current()[id]
		tombstones.Unlock()
	}
	if !ok || tx.now.Sub(t.at) > gcDeletes(ix) {
		return tombstone{}, false
	}
	return t, true
}

func (tx *docTx) addTombstone(ix *Index, id string, t tombstone) {
	m := tx.pending[ix]
	if m == nil {
		m = map[string]tombstone{}
		tx.pending[ix] = m
	}
	m[id] = t
	delete(tx.removed[ix], id)
}

// clearTombstone drops a tombstone once the id is indexed again.
func (tx *docTx) clearTombstone(ix *Index, id string) {
	if _, ok := tx.pending[ix][id]; ok {
		delete(tx.pending[ix], id)
	}
	r := tx.removed[ix]
	if r == nil {
		r = map[string]bool{}
		tx.removed[ix] = r
	}
	r[id] = true
}

func (tx *docTx) commit() {
	if len(tx.pending) == 0 && len(tx.removed) == 0 {
		return
	}
	tombstones.Lock()
	defer tombstones.Unlock()
	indices := map[*Index]bool{}
	for ix := range tx.pending {
		indices[ix] = true
	}
	for ix := range tx.removed {
		indices[ix] = true
	}
	for ix := range indices {
		st := tombstones.byIndex[weak.Make(ix)]
		cur := st.current()
		if len(tx.pending[ix]) == 0 {
			hit := false
			for id := range tx.removed[ix] {
				if _, ok := cur[id]; ok {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		gc := gcDeletes(ix)
		next := make(map[string]tombstone, len(cur)+len(tx.pending[ix]))
		for id, t := range cur {
			if tx.now.Sub(t.at) > gc || tx.removed[ix][id] {
				continue
			}
			next[id] = t
		}
		for id, t := range tx.pending[ix] {
			next[id] = t
		}
		if st == nil {
			st = stateLocked(ix)
		}
		st.history = append(st.history, tombstoneSnapshot{seqNo: ix.seqNo, items: next})
		if len(st.history) > tombstoneHistory {
			st.history = append([]tombstoneSnapshot(nil), st.history[len(st.history)-tombstoneHistory:]...)
		}
	}
	tx.pending = map[*Index]map[string]tombstone{}
	tx.removed = map[*Index]map[string]bool{}
}
