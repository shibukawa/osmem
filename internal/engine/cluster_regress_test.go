package engine

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// _refresh only records the refresh point: an index shared with another
// cluster is not copied (no copy-on-write re-index) and the clone's refresh
// is invisible to the base.
func TestRefreshDoesNotCopySharedIndex(t *testing.T) {
	c := segmentsCluster(t, segmentsMapping)
	indexSource(t, c, "1", `{"title": "a"}`)
	clone := c.Clone()
	t.Cleanup(clone.Close)
	shared := clone.indices["docs"]
	if shared != c.indices["docs"] {
		t.Fatal("the clone should share the index")
	}
	refs := shared.refs.Load()
	if _, err := clone.Refresh("docs", Params{}); err != nil {
		t.Fatal(err)
	}
	if clone.indices["docs"] != shared || shared.refs.Load() != refs {
		t.Fatal("refresh copied the shared index")
	}
	found := func(cl *Cluster, id string) any {
		t.Helper()
		res, err := cl.TermVectors("docs", id, nil, Params{"realtime": "false"})
		if err != nil {
			t.Fatal(err)
		}
		return res.Body.(M)["found"]
	}
	if found(clone, "1") != true {
		t.Fatal("refreshed document not visible with realtime=false in the clone")
	}
	if found(c, "1") != false {
		t.Fatal("the clone's refresh leaked into the base")
	}
	// a write after the refresh (copying the index, same uuid) is not
	// visible until the next refresh
	indexSource(t, clone, "2", `{"title": "b"}`)
	if clone.indices["docs"] == shared {
		t.Fatal("the write should have copied the shared index")
	}
	if found(clone, "1") != true || found(clone, "2") != false {
		t.Fatal("refresh point lost or advanced by the copy")
	}
	if _, err := clone.Refresh("docs", Params{}); err != nil {
		t.Fatal(err)
	}
	if found(clone, "2") != true {
		t.Fatal("document not visible after the second refresh")
	}
}

// Mapping an unmapped object field (dynamic: false) as nested through PUT
// _mapping re-indexes into a fresh copy of the index: nested queries see
// the objects as documents of their own and the tombstones carry over.
func TestPutMappingNestedPromotionSwapsIndex(t *testing.T) {
	c := segmentsCluster(t, `{"mappings": {"dynamic": false, "properties": {"title": {"type": "text"}}}}`)
	indexSource(t, c, "1", `{"title": "a", "comments": [{"author": "x", "stars": 1}, {"author": "y", "stars": 2}]}`)
	indexSource(t, c, "2", `{"title": "b"}`)
	if _, err := c.DeleteDoc("docs", "2", DocParams{}); err != nil {
		t.Fatal(err)
	}
	before := c.indices["docs"]
	nested := `{"properties": {"comments": {"type": "nested", "properties": {"author": {"type": "keyword"}, "stars": {"type": "integer"}}}}}`
	if _, err := c.PutMapping("docs", parseM(t, nested), Params{}); err != nil {
		t.Fatal(err)
	}
	if c.indices["docs"] == before {
		t.Fatal("the promotion should have re-indexed into a copy")
	}
	if before.refs.Load() != 0 || !before.closed {
		t.Fatal("the replaced index was not released")
	}
	same := `{"query": {"nested": {"path": "comments", "query": {"bool": {"must": [{"term": {"comments.author": "x"}}, {"term": {"comments.stars": 1}}]}}}}}`
	cross := `{"query": {"nested": {"path": "comments", "query": {"bool": {"must": [{"term": {"comments.author": "x"}}, {"term": {"comments.stars": 2}}]}}}}}`
	if n := countQuery(t, c, same); n != 1 {
		t.Fatalf("nested query after the promotion: %d hits", n)
	}
	if n := countQuery(t, c, cross); n != 0 {
		t.Fatalf("nested objects were not separated: %d hits", n)
	}
	// the deleted document's version continues from its tombstone
	res, err := c.IndexDoc("docs", "2", []byte(`{"title": "c"}`), DocParams{})
	if err != nil {
		t.Fatal(err)
	}
	if v := res.Body.(M)["_version"]; v != int64(3) {
		t.Fatalf("version after re-indexing a deleted document: %v", v)
	}
}

// Uid.encodeId encodes every all-digit id numerically, leading zeros
// included (isPositiveNumeric only checks the characters).
func TestEncodeDocIDLeadingZeros(t *testing.T) {
	for _, id := range []string{"0", "007", "0123", "10"} {
		if b := encodeDocID(id); b[0] != 0xfe {
			t.Errorf("encodeDocID(%q) = % x, want the numeric encoding", id, b)
		}
	}
	if b := encodeDocID("0x"); b[0] == 0xfe {
		t.Errorf("encodeDocID(%q) = % x, want a non-numeric encoding", "0x", b)
	}
	if sliceOf("0123", 5) != sliceOf("0123", 5) {
		t.Fatal("sliceOf is not deterministic")
	}
}

// Expired scroll contexts are swept when a scroll is created or read, not
// only when their own id comes back.
func TestScrollContextsSwept(t *testing.T) {
	c := segmentsCluster(t, segmentsMapping)
	for i := 0; i < 3; i++ {
		indexSource(t, c, string(rune('a'+i)), `{"title": "t"}`)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	open := func() string {
		t.Helper()
		res, err := c.Search("docs", parseM(t, `{"size": 1}`), Params{"scroll": "1m"})
		if err != nil {
			t.Fatal(err)
		}
		return res.Body.(M)["_scroll_id"].(string)
	}
	first := open()
	now = now.Add(2 * time.Minute)
	second := open()
	c.scrollMu.Lock()
	_, firstKept := c.scrolls[first]
	_, secondKept := c.scrolls[second]
	n := len(c.scrolls)
	c.scrollMu.Unlock()
	if firstKept || !secondKept || n != 1 {
		t.Fatalf("expired scroll not swept: first=%v second=%v n=%d", firstKept, secondKept, n)
	}
	if _, err := c.Scroll(nil, Params{"scroll_id": first, "scroll": "1m"}); err == nil {
		t.Fatal("expired scroll still served")
	}
	if _, err := c.Scroll(nil, Params{"scroll_id": second, "scroll": "1m"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := c.Scroll(nil, Params{"scroll_id": second, "scroll": "1m"}); err == nil {
		t.Fatal("expired scroll still served")
	}
	c.scrollMu.Lock()
	n = len(c.scrolls)
	c.scrollMu.Unlock()
	if n != 0 {
		t.Fatalf("%d scroll contexts left", n)
	}
}

// The outcomes of tasks run with wait_for_completion=false are bounded.
func TestBulkByScrollTaskResultsBounded(t *testing.T) {
	var last string
	for i := 0; i < maxBulkByScrollTaskResults+10; i++ {
		res := asTask("indices:data/write/delete/byquery", "test", Response{Status: 200, Body: M{"took": 1}}, nil, false)
		last = res.Body.(M)["task"].(string)
	}
	bulkByScrollTasks.Lock()
	n, order := len(bulkByScrollTasks.results), len(bulkByScrollTasks.order)
	_, lastKept := bulkByScrollTasks.results[last]
	bulkByScrollTasks.Unlock()
	if n != maxBulkByScrollTaskResults || order != maxBulkByScrollTaskResults || !lastKept {
		t.Fatalf("results=%d order=%d lastKept=%v", n, order, lastKept)
	}
}

// A source without dotted keys is not copied by expandDots; dotted keys
// are still expanded.
func TestSourceFromOrderedExpandsOnlyDottedKeys(t *testing.T) {
	render := func(raw string) string {
		t.Helper()
		doc, err := parseSourceDocument([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		src, compact := sourceFromOrdered([]byte(raw), doc)
		if string(compact) != raw {
			t.Fatalf("compact source %s, want %s", compact, raw)
		}
		out, err := json.Marshal(src)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	plain := `{"a":{"b":[{"c":1}]},"d":"x"}`
	if doc, _ := parseSourceDocument([]byte(plain)); hasDottedKeys(doc) {
		t.Fatal("no key is dotted")
	}
	if got := render(plain); got != plain {
		t.Fatalf("plain source rendered as %s", got)
	}
	if doc, _ := parseSourceDocument([]byte(`{"a":[{"b.c":1}]}`)); !hasDottedKeys(doc) {
		t.Fatal("dotted key inside a list not seen")
	}
	if got, want := render(`{"a.b":1,"a":{"c":{"d.e":2}}}`), `{"a":{"b":1,"c":{"d":{"e":2}}}}`; got != want {
		t.Fatalf("dotted source rendered as %s, want %s", got, want)
	}
}

// Shard document ordinals are computed once per index state.
func TestShardDocOrdinalsCached(t *testing.T) {
	c := segmentsCluster(t, segmentsMapping)
	indexSource(t, c, "1", `{"title": "a", "comments": [{"author": "x"}]}`)
	indexSource(t, c, "2", `{"title": "b"}`)
	ix := c.indices["docs"]
	first := shardDocOrdinals(ix)
	if again := shardDocOrdinals(ix); reflect.ValueOf(again).UnsafePointer() != reflect.ValueOf(first).UnsafePointer() {
		t.Fatal("ordinals recomputed for an unchanged index")
	}
	if first["1"] != [2]int64{0, 1} || first["2"] != [2]int64{0, 2} {
		t.Fatalf("ordinals %v", first)
	}
	indexSource(t, c, "3", `{"title": "c"}`)
	ix = c.indices["docs"]
	next := shardDocOrdinals(ix)
	if reflect.ValueOf(next).UnsafePointer() == reflect.ValueOf(first).UnsafePointer() || len(next) != 3 {
		t.Fatalf("stale ordinals after a write: %v", next)
	}
	if _, err := c.DeleteDoc("docs", "1", DocParams{}); err != nil {
		t.Fatal(err)
	}
	if after := shardDocOrdinals(c.indices["docs"]); len(after) != 2 || after["2"] != [2]int64{0, 0} {
		t.Fatalf("stale ordinals after a delete: %v", after)
	}
}
