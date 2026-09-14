package osmem

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestHistogramAggregationOrderCompatibility(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/agg-compat", `{"mappings":{"properties":{"left":{"type":"keyword"},"right":{"type":"keyword"},"price":{"type":"double"},"created":{"type":"date"},"value":{"type":"double"},"weight":{"type":"double"}}}}`)
	docs := []string{
		`{"left":"ab","right":"c","price":0.2,"created":"2024-01-01","value":1,"weight":1}`,
		`{"left":"a","right":"bc","price":0.3,"created":"2024-02-01","value":2,"weight":1}`,
		`{"left":"a","right":"d","price":1.1,"created":"2024-03-01","value":3,"weight":1}`,
		`{"left":"e","right":"f","price":2.5,"created":"2024-04-01","value":4,"weight":1}`,
		`{"left":"g","right":"h","price":3.1,"created":"2024-05-01","value":5,"weight":1}`,
		`{"left":"i","right":"j","price":3.2,"created":"2024-06-01","value":6,"weight":1}`,
		`{"left":"k","right":"l","price":3.3,"created":"2024-07-01","value":7,"weight":1}`,
		`{"left":"m","right":"n","price":3.4,"created":"2024-08-01","value":8,"weight":[1,2]}`,
	}
	var bulk strings.Builder
	for i, doc := range docs {
		fmt.Fprintf(&bulk, `{"index":{"_index":"agg-compat","_id":"%d"}}`+"\n%s\n", i+1, doc)
	}
	if err := c.BulkString(bulk.String()); err != nil {
		t.Fatal(err)
	}

	res := mustDo(t, c, http.MethodPost, "/agg-compat/_search", `{"size":0,"aggs":{"key_desc":{"histogram":{"field":"price","interval":1,"min_doc_count":1,"order":{"_key":"desc"}}},"count_desc":{"histogram":{"field":"price","interval":1,"min_doc_count":1,"order":{"_count":"desc"}}},"date_desc":{"date_histogram":{"field":"created","calendar_interval":"day","min_doc_count":1,"format":"yyyy-MM-dd","order":{"_key":"desc"}}}}}`)
	aggs := res["aggregations"].(map[string]any)
	keys := func(name string) []float64 {
		t.Helper()
		buckets := aggs[name].(map[string]any)["buckets"].([]any)
		out := make([]float64, len(buckets))
		for i, bucket := range buckets {
			out[i] = bucket.(map[string]any)["key"].(float64)
		}
		return out
	}
	assertFloatKeys := func(name string, got, want []float64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s keys = %v, want %v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s keys = %v, want %v", name, got, want)
			}
		}
	}
	assertFloatKeys("key_desc", keys("key_desc"), []float64{3, 2, 1, 0})
	assertFloatKeys("count_desc", keys("count_desc"), []float64{3, 0, 1, 2})

	dateBuckets := aggs["date_desc"].(map[string]any)["buckets"].([]any)
	if len(dateBuckets) != len(docs) || dateBuckets[0].(map[string]any)["key_as_string"] != "2024-08-01" || dateBuckets[len(dateBuckets)-1].(map[string]any)["key_as_string"] != "2024-01-01" {
		t.Fatalf("date_desc buckets = %v", dateBuckets)
	}
}

func TestCompositeAggregationTupleKeysDoNotCollide(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/composite-compat", `{"mappings":{"properties":{"left":{"type":"keyword"},"right":{"type":"keyword"}}}}`)
	if err := c.BulkString("{" + `"index":{"_index":"composite-compat","_id":"1"}}` + "\n" + `{"left":"ab","right":"c"}` + "\n" + "{" + `"index":{"_index":"composite-compat","_id":"2"}}` + "\n" + `{"left":"a","right":"bc"}` + "\n"); err != nil {
		t.Fatal(err)
	}

	res := mustDo(t, c, http.MethodPost, "/composite-compat/_search", `{"size":0,"aggs":{"tuples":{"composite":{"sources":[{"left":{"terms":{"field":"left"}}},{"right":{"terms":{"field":"right"}}}]}}}}`)
	buckets := res["aggregations"].(map[string]any)["tuples"].(map[string]any)["buckets"].([]any)
	if len(buckets) != 2 {
		t.Fatalf("composite buckets = %v, want two distinct tuples", buckets)
	}
	seen := map[string]bool{}
	for _, raw := range buckets {
		bucket := raw.(map[string]any)
		key := bucket["key"].(map[string]any)
		seen[key["left"].(string)+"/"+key["right"].(string)] = true
		if bucket["doc_count"].(float64) != 1 {
			t.Fatalf("composite bucket = %v, want doc_count 1", bucket)
		}
	}
	if !seen["ab/c"] || !seen["a/bc"] {
		t.Fatalf("composite tuple keys = %v", seen)
	}
}

func TestWeightedAvgRejectsMultiValuedWeight(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/weighted-compat", `{"mappings":{"properties":{"value":{"type":"double"},"weight":{"type":"double"}}}}`)
	mustDo(t, c, http.MethodPut, "/weighted-compat/_doc/1", `{"value":10,"weight":[1,2]}`)

	code, res := status(t, c, http.MethodPost, "/weighted-compat/_search", `{"size":0,"aggs":{"weighted":{"weighted_avg":{"value":{"field":"value"},"weight":{"field":"weight"}}}}}`)
	if code != http.StatusInternalServerError || errType(res) != "search_phase_execution_exception" {
		t.Fatalf("weighted_avg response status/type = %d/%s, want 500/search_phase_execution_exception: %v", code, errType(res), res)
	}
	err := res["error"].(map[string]any)
	root := err["root_cause"].([]any)[0].(map[string]any)
	if root["type"] != "aggregation_execution_exception" || root["reason"] != "[weighted_avg] weight field [weight] has more than one value" {
		t.Fatalf("weighted_avg root cause = %v", root)
	}
}

func TestSamplerAggregationsApplyDefaultSamplingLimits(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/sampler-compat", `{"mappings":{"properties":{"group":{"type":"keyword"}}}}`)
	var bulk strings.Builder
	for i := 1; i <= 101; i++ {
		fmt.Fprintf(&bulk, `{"index":{"_index":"sampler-compat","_id":"%03d"}}`+"\n", i)
		fmt.Fprintf(&bulk, `{"group":"g-%03d"}`+"\n", i)
	}
	if err := c.BulkString(bulk.String()); err != nil {
		t.Fatal(err)
	}
	res := mustDo(t, c, http.MethodPost, "/sampler-compat/_search", `{"size":0,"aggs":{"sample":{"sampler":{},"aggs":{"groups":{"terms":{"field":"group","size":200}}}}}}`)
	sample := res["aggregations"].(map[string]any)["sample"].(map[string]any)
	if sample["doc_count"].(float64) != 100 || len(sample["groups"].(map[string]any)["buckets"].([]any)) != 100 {
		t.Fatalf("default sampler sample: %v", sample)
	}

	mustDo(t, c, http.MethodPut, "/diversified-compat", `{"mappings":{"properties":{"group":{"type":"keyword"}}}}`)
	for id, group := range map[string]string{"1": "same", "2": "same", "3": "other"} {
		mustDo(t, c, http.MethodPut, "/diversified-compat/_doc/"+id, `{"group":"`+group+`"}`)
	}
	res = mustDo(t, c, http.MethodPost, "/diversified-compat/_search", `{"size":0,"aggs":{"sample":{"diversified_sampler":{"field":"group"},"aggs":{"groups":{"terms":{"field":"group"}}}}}}`)
	sample = res["aggregations"].(map[string]any)["sample"].(map[string]any)
	if sample["doc_count"].(float64) != 2 {
		t.Fatalf("diversified sampler doc_count: %v", sample)
	}
	buckets := sample["groups"].(map[string]any)["buckets"].([]any)
	if len(buckets) != 2 {
		t.Fatalf("diversified sampler buckets: %v", buckets)
	}
	for _, raw := range buckets {
		if raw.(map[string]any)["key"] == "same" && raw.(map[string]any)["doc_count"].(float64) != 1 {
			t.Fatalf("same bucket should be capped at one doc: %v", raw)
		}
	}
}
