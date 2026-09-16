package osmem

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Custom routing is stored with the document: it survives later writes
// (which merge segments and copy the document structs) and copy-on-write
// copies of the index made by a clone.
func TestRoutingSurvivesWritesAndClones(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/r", `{"settings":{"number_of_shards":3}}`)
	mustDo(t, c, http.MethodPut, "/r/_doc/1?routing=k", `{"n":1}`)
	for i := 2; i <= 16; i++ {
		mustDo(t, c, http.MethodPut, fmt.Sprintf("/r/_doc/%d", i), fmt.Sprintf(`{"n":%d}`, i))
	}
	if res := mustDo(t, c, http.MethodGet, "/r/_doc/1?routing=k", nil); res["_routing"] != "k" {
		t.Fatalf("routing after 15 more writes: %v", res)
	}

	clone := c.Clone()
	defer clone.Close()
	mustDo(t, clone, http.MethodPut, "/r/_doc/17", `{"n":17}`)
	if res := mustDo(t, clone, http.MethodGet, "/r/_doc/1?routing=k", nil); res["_routing"] != "k" {
		t.Fatalf("routing in the written clone: %v", res)
	}
	if res := mustDo(t, c, http.MethodGet, "/r/_doc/1?routing=k", nil); res["_routing"] != "k" {
		t.Fatalf("routing in the base after the clone wrote: %v", res)
	}
	if code, _ := status(t, clone, http.MethodGet, "/r/_doc/17", nil); code != http.StatusOK {
		t.Fatalf("clone doc: status %d", code)
	}
	if code, _ := status(t, c, http.MethodGet, "/r/_doc/17", nil); code != http.StatusNotFound {
		t.Fatalf("clone doc leaked into the base: status %d", code)
	}
}

// A join-field index (parent/child documents routed to the parent) can be
// cloned and written to: the children keep their routing in the copy.
func TestJoinIndexCloneWrite(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/join", `{"settings":{"number_of_shards":2},"mappings":{"properties":{"j":{"type":"join","relations":{"a":"b"}}}}}`)
	mustDo(t, c, http.MethodPut, "/join/_doc/1", `{"j":"a"}`)
	mustDo(t, c, http.MethodPut, "/join/_doc/2?routing=1", `{"j":{"name":"b","parent":"1"}}`)

	clone := c.Clone()
	defer clone.Close()
	mustDo(t, clone, http.MethodPut, "/join/_doc/3?routing=1", `{"j":{"name":"b","parent":"1"}}`)
	res := mustDo(t, clone, http.MethodPost, "/join/_search", `{"query":{"parent_id":{"type":"b","id":"1"}}}`)
	if n := res["hits"].(map[string]any)["total"].(map[string]any)["value"]; n != float64(2) {
		t.Fatalf("children in the clone: %v", res)
	}
	if res := mustDo(t, clone, http.MethodGet, "/join/_doc/2?routing=1", nil); res["_routing"] != "1" {
		t.Fatalf("child routing in the clone: %v", res)
	}
	res = mustDo(t, c, http.MethodPost, "/join/_search", `{"query":{"parent_id":{"type":"b","id":"1"}}}`)
	if n := res["hits"].(map[string]any)["total"].(map[string]any)["value"]; n != float64(1) {
		t.Fatalf("children in the base: %v", res)
	}
}

// An absurd slices count is rejected up front instead of being allocated.
func TestDeleteByQuerySlicesLimit(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/s/_doc/1", `{"n":1}`)
	start := time.Now()
	code, body := status(t, c, http.MethodPost, "/s/_delete_by_query?slices=2000000000", `{"query":{"match_all":{}}}`)
	if code != http.StatusBadRequest || errType(body) != "illegal_argument_exception" {
		t.Fatalf("status %d: %v", code, body)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("rejecting the request took %v", d)
	}
	if res := mustDo(t, c, http.MethodGet, "/s/_count", nil); res["count"] != float64(1) {
		t.Fatalf("document deleted by the rejected request: %v", res)
	}
}
