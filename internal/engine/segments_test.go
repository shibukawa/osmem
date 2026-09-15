package engine

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/blevesearch/bleve/v2/index/scorch"
)

const segmentsMapping = `{"mappings": {"properties": {
	"title": {"type": "text", "fields": {"keyword": {"type": "keyword"}}},
	"comments": {"type": "nested", "properties": {"author": {"type": "keyword"}}}}}}`

func parseM(t *testing.T, s string) M {
	t.Helper()
	var m M
	if err := decodeJSON([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func segmentsCluster(t *testing.T, mapping string) *Cluster {
	t.Helper()
	c := New()
	t.Cleanup(c.Close)
	if _, err := c.CreateIndex("docs", parseM(t, mapping)); err != nil {
		t.Fatal(err)
	}
	return c
}

func indexSource(t *testing.T, c *Cluster, id, source string) {
	t.Helper()
	if _, err := c.IndexDoc("docs", id, []byte(source), DocParams{}); err != nil {
		t.Fatal(err)
	}
}

func countQuery(t *testing.T, c *Cluster, query string) int {
	t.Helper()
	res, err := c.Count("docs", parseM(t, query), Params{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Body.(M)["count"].(int)
}

// checkRuns verifies that the runs of the docs index match the segments of
// its current bleve snapshot and its stored documents, and returns the
// number of segments.
func checkRuns(t *testing.T, c *Cluster) int {
	t.Helper()
	ix := c.indices["docs"]
	advanced, err := ix.bleve.Advanced()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := advanced.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	segments := reader.(*scorch.IndexSnapshot).Segments()
	if len(segments) != len(ix.runs) {
		t.Fatalf("%d segments but %d runs", len(segments), len(ix.runs))
	}
	// runs follow the order of the segments they stand for
	live, roots := 0, 0
	for i, s := range segments {
		r := ix.runs[i]
		if r.live <= 0 || int(s.LiveSize()) < r.live {
			t.Fatalf("segment %d holds %d live documents, its run %d roots", i, s.LiveSize(), r.live)
		}
		live += int(s.LiveSize())
		roots += r.live
	}
	indexed := len(ix.docs)
	for _, ids := range ix.children {
		indexed += len(ids)
	}
	if live != indexed {
		t.Fatalf("segments hold %d live documents, the index has %d with nested objects", live, indexed)
	}
	if roots != len(ix.docs) || len(ix.runOf) != len(ix.docs) {
		t.Fatalf("runs hold %d documents and runOf %d, the index %d", roots, len(ix.runOf), len(ix.docs))
	}
	return len(segments)
}

// maxSegments bounds the segments of an index with n documents: fewer than
// runMergeFactor runs per size class, plus runs of at least 1000 documents.
func maxSegments(n int) int {
	return n/1000 + runClasses*(runMergeFactor-1)
}

func TestSingleDocumentWritesMergeSegments(t *testing.T) {
	c := segmentsCluster(t, segmentsMapping)
	const n = 2345
	for i := 0; i < n; i++ {
		indexSource(t, c, strconv.Itoa(i), fmt.Sprintf(`{"title": "document %d"}`, i))
		if i%101 == 0 {
			checkRuns(t, c)
		}
	}
	if segments := checkRuns(t, c); segments > maxSegments(n) {
		t.Errorf("%d segments after %d single-document writes", segments, n)
	}
	if got := countQuery(t, c, `{"query": {"match_all": {}}}`); got != n {
		t.Errorf("match_all counted %d documents, want %d", got, n)
	}
	for _, i := range []int{0, 9, 10, 999, 1000, n - 1} {
		query := fmt.Sprintf(`{"query": {"term": {"title.keyword": "document %d"}}}`, i)
		if got := countQuery(t, c, query); got != 1 {
			t.Errorf("document %d matched %d times", i, got)
		}
	}
}

func TestReplacedAndDeletedDocumentsReleaseSegments(t *testing.T) {
	c := segmentsCluster(t, segmentsMapping)
	var bulk strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&bulk, "{\"index\":{\"_id\":\"%d\"}}\n{\"title\":\"old %d\"}\n", i, i)
	}
	if _, err := c.Bulk("docs", []byte(bulk.String()), Params{}); err != nil {
		t.Fatal(err)
	}
	if segments := checkRuns(t, c); segments != 1 {
		t.Fatalf("the bulk request left %d segments", segments)
	}
	// replacing most documents of the bulk run one at a time rewrites it
	for i := 0; i < 2000; i++ {
		body := parseM(t, fmt.Sprintf(`{"doc": {"title": "new %d"}}`, i))
		if _, err := c.UpdateDoc("docs", strconv.Itoa(i), body, Params{}); err != nil {
			t.Fatal(err)
		}
	}
	if segments := checkRuns(t, c); segments > maxSegments(3000) {
		t.Errorf("%d segments after 2000 updates", segments)
	}
	for i := 1000; i < 3000; i++ {
		if _, err := c.DeleteDoc("docs", strconv.Itoa(i), DocParams{}); err != nil {
			t.Fatal(err)
		}
	}
	if segments := checkRuns(t, c); segments > maxSegments(1000) {
		t.Errorf("%d segments after deleting 2000 documents", segments)
	}
	for query, want := range map[string]int{
		`{"query": {"match_all": {}}}`:           1000,
		`{"query": {"match": {"title": "new"}}}`: 1000,
		`{"query": {"match": {"title": "old"}}}`: 0,
	} {
		if got := countQuery(t, c, query); got != want {
			t.Errorf("%s counted %d documents, want %d", query, got, want)
		}
	}
}

func TestMergedDocumentsKeepTheirNestedObjects(t *testing.T) {
	c := segmentsCluster(t, segmentsMapping)
	const n = 250
	for i := 0; i < n; i++ {
		indexSource(t, c, strconv.Itoa(i), fmt.Sprintf(`{"title": "post %d", "comments": [{"author": "a%d"}, {"author": "shared"}]}`, i, i))
	}
	// replace every other document with different nested objects
	for i := 0; i < n; i += 2 {
		indexSource(t, c, strconv.Itoa(i), fmt.Sprintf(`{"title": "post %d", "comments": [{"author": "b%d"}]}`, i, i))
	}
	checkRuns(t, c)
	nested := func(author string) string {
		return fmt.Sprintf(`{"query": {"nested": {"path": "comments", "query": {"term": {"comments.author": %q}}}}}`, author)
	}
	for query, want := range map[string]int{
		`{"query": {"match_all": {}}}`: n,
		nested("shared"):               n / 2,
		nested("a0"):                   0,
		nested("a1"):                   1,
		nested("b0"):                   1,
		nested("b1"):                   0,
	} {
		if got := countQuery(t, c, query); got != want {
			t.Errorf("%s counted %d documents, want %d", query, got, want)
		}
	}
}

func TestMergingLeavesSharedIndicesAlone(t *testing.T) {
	base := segmentsCluster(t, segmentsMapping)
	for i := 0; i < 30; i++ {
		indexSource(t, base, strconv.Itoa(i), fmt.Sprintf(`{"title": "base %d"}`, i))
	}
	shared := base.indices["docs"]
	baseSegments := checkRuns(t, base)

	clone := base.Clone()
	t.Cleanup(clone.Close)
	res, err := clone.CreatePIT("docs", Params{"keep_alive": "1m"})
	if err != nil {
		t.Fatal(err)
	}
	pit := res.Body.(M)["pit_id"].(string)
	for i := 30; i < 530; i++ {
		indexSource(t, clone, strconv.Itoa(i), fmt.Sprintf(`{"title": "clone %d"}`, i))
	}
	checkRuns(t, clone)
	if base.indices["docs"] != shared || checkRuns(t, base) != baseSegments {
		t.Errorf("writes to the clone changed the base index")
	}
	if got := countQuery(t, base, `{"query": {"match_all": {}}}`); got != 30 {
		t.Errorf("base counted %d documents, want 30", got)
	}
	if got := countQuery(t, clone, `{"query": {"match_all": {}}}`); got != 530 {
		t.Errorf("clone counted %d documents, want 530", got)
	}
	res, err = clone.Search("", M{"pit": M{"id": pit}, "size": 0}, Params{})
	if err != nil {
		t.Fatal(err)
	}
	if total := res.Body.(M)["hits"].(M)["total"].(M)["value"]; fmt.Sprint(total) != "30" {
		t.Errorf("the point in time sees %v documents, want 30", total)
	}
}

func TestMergesKeepDocumentsIndexedUnderTheirMapping(t *testing.T) {
	c := segmentsCluster(t, `{"mappings": {"properties": {"title": {"type": "text"}}}}`)
	for i := 0; i < 9; i++ {
		indexSource(t, c, strconv.Itoa(i), fmt.Sprintf(`{"title": "early %d"}`, i))
	}
	mapping := parseM(t, `{"properties": {"title": {"type": "text", "fields": {"raw": {"type": "keyword"}}}}}`)
	if _, err := c.PutMapping("docs", mapping, Params{}); err != nil {
		t.Fatal(err)
	}
	for i := 9; i < 200; i++ {
		indexSource(t, c, strconv.Itoa(i), fmt.Sprintf(`{"title": "late %d"}`, i))
	}
	if segments := checkRuns(t, c); segments > 9+maxSegments(200) {
		t.Errorf("%d segments after 191 writes following a mapping update", segments)
	}
	// as on OpenSearch, a field added to the mapping leaves the documents
	// written before unindexed
	for query, want := range map[string]int{
		`{"query": {"term": {"title.raw": "early 0"}}}`: 0,
		`{"query": {"term": {"title.raw": "late 9"}}}`:  1,
		`{"query": {"match": {"title": "early"}}}`:      9,
	} {
		if got := countQuery(t, c, query); got != want {
			t.Errorf("%s counted %d documents, want %d", query, got, want)
		}
	}
}

func TestDocumentsTheIndexNoLongerAcceptsPinTheirRun(t *testing.T) {
	c := segmentsCluster(t, `{"mappings": {"properties": {"n": {"type": "integer"}}}}`)
	for i := 0; i < 10; i++ {
		indexSource(t, c, strconv.Itoa(i), `{"n": "7"}`)
	}
	if _, err := c.PutSettings("docs", M{"index": M{"mapping": M{"coerce": false}}}, Params{}); err != nil {
		t.Fatal(err)
	}
	// the 100th write fills size class 1, whose oldest run holds documents
	// the index no longer coerces
	for i := 10; i < 100; i++ {
		indexSource(t, c, strconv.Itoa(i), `{"n": 7}`)
	}
	checkRuns(t, c)
	pinned := 0
	for _, r := range c.indices["docs"].runs {
		if r.pinned {
			pinned++
		}
	}
	if pinned != 1 {
		t.Errorf("%d pinned runs, want 1", pinned)
	}
	if got := countQuery(t, c, `{"query": {"term": {"n": 7}}}`); got != 100 {
		t.Errorf("n matched %d documents, want 100", got)
	}
}
