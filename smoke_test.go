package osmem

import (
	"encoding/json"
	"net/http"
	"testing"
)

func mustDo(t *testing.T, c *Cluster, method, path string, body any) map[string]any {
	t.Helper()
	res, err := c.Do(method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	var out map[string]any
	if len(res.Body) > 0 {
		if err := json.Unmarshal(res.Body, &out); err != nil {
			t.Fatalf("%s %s: bad json %q: %v", method, path, res.Body, err)
		}
	}
	if res.IsError() {
		t.Fatalf("%s %s: status %d: %s", method, path, res.StatusCode, res.Body)
	}
	return out
}

func TestSmoke(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/products", `{
	  "settings": {"number_of_shards": 1},
	  "mappings": {"properties": {
	    "name": {"type": "text", "fields": {"keyword": {"type": "keyword"}}},
	    "price": {"type": "double"},
	    "tags": {"type": "keyword"},
	    "created": {"type": "date"},
	    "active": {"type": "boolean"}
	  }}
	}`)
	if err := c.BulkString(`
{"index": {"_index": "products", "_id": "1"}}
{"name": "Red Apple", "price": 1.5, "tags": ["fruit", "red"], "created": "2024-01-05T10:00:00Z", "active": true, "stock": 10}
{"index": {"_index": "products", "_id": "2"}}
{"name": "Green Apple", "price": 1.2, "tags": ["fruit", "green"], "created": "2024-02-10", "active": false, "stock": 0}
{"index": {"_index": "products", "_id": "3"}}
{"name": "Banana", "price": 0.5, "tags": ["fruit", "yellow"], "created": "2024-02-20T00:00:00Z", "active": true, "stock": 3}
`); err != nil {
		t.Fatal(err)
	}
	res := mustDo(t, c, http.MethodPost, "/products/_search", `{"query": {"match": {"name": "apple"}}, "sort": [{"price": "asc"}]}`)
	hits := res["hits"].(map[string]any)
	if hits["total"].(map[string]any)["value"].(float64) != 2 {
		t.Fatalf("total: %v", hits["total"])
	}
	list := hits["hits"].([]any)
	if list[0].(map[string]any)["_id"] != "2" {
		t.Fatalf("sort: %v", list)
	}
	res = mustDo(t, c, http.MethodPost, "/products/_search", `{"size": 0, "query": {"bool": {"filter": [{"term": {"tags": "fruit"}}, {"range": {"price": {"gte": 1}}}]}},
	  "aggs": {"by_tag": {"terms": {"field": "tags"}, "aggs": {"avg_price": {"avg": {"field": "price"}}}},
	           "by_month": {"date_histogram": {"field": "created", "calendar_interval": "month"}},
	           "stats": {"stats": {"field": "price"}}}}`)
	aggs := res["aggregations"].(map[string]any)
	byTag := aggs["by_tag"].(map[string]any)["buckets"].([]any)
	if byTag[0].(map[string]any)["key"] != "fruit" || byTag[0].(map[string]any)["doc_count"].(float64) != 2 {
		t.Fatalf("terms: %v", byTag)
	}
	byMonth := aggs["by_month"].(map[string]any)["buckets"].([]any)
	if len(byMonth) != 2 || byMonth[0].(map[string]any)["key_as_string"] != "2024-01-01T00:00:00.000Z" {
		t.Fatalf("date_histogram: %v", byMonth)
	}
	// dynamic mapping inferred stock as long
	m := mustDo(t, c, http.MethodGet, "/products/_mapping", nil)
	props := m["products"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)
	if props["stock"].(map[string]any)["type"] != "long" {
		t.Fatalf("mapping: %v", props["stock"])
	}
	// clone: copy on write
	clone := c.Clone()
	defer clone.Close()
	mustDo(t, clone, http.MethodDelete, "/products/_doc/1", nil)
	n, _ := clone.Count("products", nil)
	m0, _ := c.Count("products", nil)
	if n != 2 || m0 != 3 {
		t.Fatalf("clone counts: clone=%d base=%d", n, m0)
	}
	res = mustDo(t, c, http.MethodPost, "/products/_search", `{"query": {"match": {"name": "apple"}}, "highlight": {"fields": {"name": {}}}}`)
	hl := res["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["highlight"]
	t.Logf("highlight: %v", hl)
	res = mustDo(t, c, http.MethodGet, "/products/_doc/3", nil)
	if res["_source"].(map[string]any)["name"] != "Banana" {
		t.Fatalf("get: %v", res)
	}
	r, _ := c.Do(http.MethodGet, "/products/_doc/99", nil)
	if r.StatusCode != 404 {
		t.Fatalf("missing doc status %d", r.StatusCode)
	}
	res = mustDo(t, c, http.MethodPost, "/products/_search", `{"query": {"query_string": {"query": "name:apple AND active:true"}}}`)
	if res["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 1 {
		t.Fatalf("query_string: %v", res["hits"])
	}
	res = mustDo(t, c, http.MethodPost, "/products/_search", `{"query": {"range": {"created": {"gte": "2024-02-01", "lt": "2024-03"}}}}`)
	if res["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 2 {
		t.Fatalf("date range: %v", res["hits"])
	}
}
