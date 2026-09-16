package osmem

import (
	"net/http"
	"testing"
)

// adjacency_matrix's show_only_intersecting (added in 2.19.0), checked
// against OpenSearch 3.8.0 (search.aggregation/70_adjacency_matrix.yml).
func TestAdjacencyMatrixShowOnlyIntersectingCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/adj", `{"mappings":{"properties":{"num":{"type":"integer"}}}}`)
	if err := c.BulkString("" +
		`{"index":{"_index":"adj","_id":"1"}}` + "\n" + `{"num":[1,2]}` + "\n" +
		`{"index":{"_index":"adj","_id":"2"}}` + "\n" + `{"num":[2,3]}` + "\n" +
		`{"index":{"_index":"adj","_id":"3"}}` + "\n" + `{"num":[3,4]}` + "\n"); err != nil {
		t.Fatal(err)
	}

	buckets := func(body string) []map[string]any {
		t.Helper()
		res := mustDo(t, c, http.MethodPost, "/adj/_search", body)
		agg := res["aggregations"].(map[string]any)["conns"].(map[string]any)
		raw := agg["buckets"].([]any)
		out := make([]map[string]any, len(raw))
		for i, b := range raw {
			out[i] = b.(map[string]any)
		}
		return out
	}

	t.Run("default keeps non-intersecting buckets", func(t *testing.T) {
		bs := buckets(`{"size":0,"aggs":{"conns":{"adjacency_matrix":{"filters":{"1":{"term":{"num":1}},"2":{"term":{"num":2}},"4":{"term":{"num":4}}}}}}}`)
		if len(bs) != 4 {
			t.Fatalf("buckets = %v", bs)
		}
	})

	t.Run("show_only_intersecting keeps only the intersection bucket", func(t *testing.T) {
		bs := buckets(`{"size":0,"aggs":{"conns":{"adjacency_matrix":{"show_only_intersecting":true,"filters":{"1":{"term":{"num":1}},"2":{"term":{"num":2}},"4":{"term":{"num":4}}}}}}}`)
		if len(bs) != 1 {
			t.Fatalf("buckets = %v, want 1", bs)
		}
		if bs[0]["key"] != "1&2" {
			t.Fatalf("key = %v, want 1&2", bs[0]["key"])
		}
		if n, _ := bs[0]["doc_count"].(float64); n != 1 {
			t.Fatalf("doc_count = %v, want 1", bs[0]["doc_count"])
		}
	})
}
