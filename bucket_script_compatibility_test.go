package osmem

import (
	"net/http"
	"testing"
)

// bucket_script and bucket_selector pipeline aggregations, backed by the
// same read-only Painless runtime as script_compatibility_test.go. Unlike a
// document script, these see only params: the script's own static params
// merged with the buckets_path values, never doc[...].

func bucketScriptIndex(t *testing.T, c *Cluster) {
	t.Helper()
	dslIndex(t, c, "sales", `{"mappings":{"properties":{"category":{"type":"keyword"},"price":{"type":"double"}}}}`,
		`{"category":"fruit","price":10}`,
		`{"category":"fruit","price":20}`,
		`{"category":"veg","price":5}`,
		`{"category":"veg","price":8}`,
	)
}

func bucketsByKey(t *testing.T, res map[string]any, path ...string) map[string]map[string]any {
	t.Helper()
	cur := res
	for _, p := range path {
		cur = cur[p].(map[string]any)
	}
	out := map[string]map[string]any{}
	for _, b := range cur["buckets"].([]any) {
		bm := b.(map[string]any)
		out[bm["key"].(string)] = bm
	}
	return out
}

func TestBucketScript(t *testing.T) {
	c := New()
	defer c.Close()
	bucketScriptIndex(t, c)

	t.Run("computes a value from named buckets_path", func(t *testing.T) {
		res := mustDo(t, c, http.MethodPost, "/sales/_search", `{"size": 0, "aggs": {"cat": {"terms": {"field": "category"},
			"aggs": {"total": {"sum": {"field": "price"}}, "doubled": {"bucket_script": {"buckets_path": {"t": "total"}, "script": {"source": "params.t * 2"}}}}}}}`)
		buckets := bucketsByKey(t, res["aggregations"].(map[string]any), "cat")
		if v := buckets["fruit"]["doubled"].(map[string]any)["value"]; v != 60.0 {
			t.Fatalf("fruit doubled = %v, want 60", v)
		}
		if v := buckets["veg"]["doubled"].(map[string]any)["value"]; v != 26.0 {
			t.Fatalf("veg doubled = %v, want 26", v)
		}
	})

	t.Run("a bare string buckets_path is bound to _value", func(t *testing.T) {
		res := mustDo(t, c, http.MethodPost, "/sales/_search", `{"size": 0, "aggs": {"cat": {"terms": {"field": "category"},
			"aggs": {"total": {"sum": {"field": "price"}}, "same": {"bucket_script": {"buckets_path": "total", "script": {"source": "params._value"}}}}}}}`)
		buckets := bucketsByKey(t, res["aggregations"].(map[string]any), "cat")
		if v := buckets["fruit"]["same"].(map[string]any)["value"]; v != 30.0 {
			t.Fatalf("fruit same = %v, want 30", v)
		}
	})

	t.Run("an array buckets_path is bound to _value0, _value1, ...", func(t *testing.T) {
		res := mustDo(t, c, http.MethodPost, "/sales/_search", `{"size": 0, "aggs": {"cat": {"terms": {"field": "category"},
			"aggs": {"total": {"sum": {"field": "price"}}, "count": {"value_count": {"field": "price"}},
			"avg": {"bucket_script": {"buckets_path": ["total", "count"], "script": {"source": "params._value0 / params._value1"}}}}}}}`)
		buckets := bucketsByKey(t, res["aggregations"].(map[string]any), "cat")
		if v := buckets["fruit"]["avg"].(map[string]any)["value"]; v != 15.0 {
			t.Fatalf("fruit avg = %v, want 15 (30/2)", v)
		}
	})

	t.Run("static script params merge with the buckets_path values", func(t *testing.T) {
		res := mustDo(t, c, http.MethodPost, "/sales/_search", `{"size": 0, "aggs": {"cat": {"terms": {"field": "category"},
			"aggs": {"total": {"sum": {"field": "price"}}, "scaled": {"bucket_script": {"buckets_path": {"t": "total"}, "script": {"source": "params.t * params.factor", "params": {"factor": 3}}}}}}}}`)
		buckets := bucketsByKey(t, res["aggregations"].(map[string]any), "cat")
		if v := buckets["fruit"]["scaled"].(map[string]any)["value"]; v != 90.0 {
			t.Fatalf("fruit scaled = %v, want 90", v)
		}
	})

	t.Run("a buckets_path value wins over a same-named static param", func(t *testing.T) {
		res := mustDo(t, c, http.MethodPost, "/sales/_search", `{"size": 0, "aggs": {"cat": {"terms": {"field": "category"},
			"aggs": {"total": {"sum": {"field": "price"}}, "x": {"bucket_script": {"buckets_path": {"t": "total"}, "script": {"source": "params.t", "params": {"t": -1}}}}}}}}`)
		buckets := bucketsByKey(t, res["aggregations"].(map[string]any), "cat")
		if v := buckets["fruit"]["x"].(map[string]any)["value"]; v != 30.0 {
			t.Fatalf("fruit x = %v, want 30 (buckets_path wins over the static params.t)", v)
		}
	})

	t.Run("a script returning null leaves the bucket without a value", func(t *testing.T) {
		res := mustDo(t, c, http.MethodPost, "/sales/_search", `{"size": 0, "aggs": {"cat": {"terms": {"field": "category"},
			"aggs": {"total": {"sum": {"field": "price"}}, "big": {"bucket_script": {"buckets_path": {"t": "total"}, "script": {"source": "params.t > 15 ? params.t : null"}}}}}}}`)
		buckets := bucketsByKey(t, res["aggregations"].(map[string]any), "cat")
		if v := buckets["fruit"]["big"].(map[string]any)["value"]; v != 30.0 {
			t.Fatalf("fruit big = %v, want 30", v)
		}
		if _, ok := buckets["veg"]["big"]; ok {
			t.Fatalf("veg should have no [big] value (total 13 is not > 15): %v", buckets["veg"])
		}
	})

	t.Run("chains after another pipeline in the same level", func(t *testing.T) {
		// cumulative_sum requires a histogram/date_histogram parent, so this
		// exercises resolvePipelineOrder ordering bucket_script after it
		// rather than reusing the terms-bucketed index above. interval 5
		// over prices 5,8,10,20 makes an empty bucket at key 15: cum still
		// gets a value there (cumulative_sum always inserts zeros for a
		// gap), but resolveBucketValue treats *any* doc_count-0 bucket as a
		// gap for buckets_path purposes regardless of the value it finds,
		// so doubled_cum (default gap_policy: skip) is legitimately absent
		// only on that one bucket — the same convention moving_avg/
		// derivative already use.
		res := mustDo(t, c, http.MethodPost, "/sales/_search", `{"size": 0, "aggs": {"h": {"histogram": {"field": "price", "interval": 5},
			"aggs": {"s": {"sum": {"field": "price"}}, "cum": {"cumulative_sum": {"buckets_path": "s"}},
			"doubled_cum": {"bucket_script": {"buckets_path": {"c": "cum"}, "script": {"source": "params.c * 2"}}}}}}}`)
		for _, b := range res["aggregations"].(map[string]any)["h"].(map[string]any)["buckets"].([]any) {
			bm := b.(map[string]any)
			if bm["doc_count"].(float64) == 0 {
				if _, ok := bm["doubled_cum"]; ok {
					t.Fatalf("key %v: doubled_cum should be absent on an empty (gap) bucket: %v", bm["key"], bm)
				}
				continue
			}
			cum := bm["cum"].(map[string]any)["value"].(float64)
			if v := bm["doubled_cum"].(map[string]any)["value"]; v != cum*2 {
				t.Fatalf("key %v doubled_cum = %v, want %v (2x cumulative_sum)", bm["key"], v, cum*2)
			}
		}
	})

	t.Run("doc is unavailable to a bucket script", func(t *testing.T) {
		st, res := status(t, c, http.MethodPost, "/sales/_search", `{"size": 0, "aggs": {"cat": {"terms": {"field": "category"},
			"aggs": {"total": {"sum": {"field": "price"}}, "bad": {"bucket_script": {"buckets_path": {"t": "total"}, "script": {"source": "doc['price'].value"}}}}}}}`)
		if st != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%v", st, res)
		}
	})

	t.Run("must be declared inside another aggregation", func(t *testing.T) {
		st, res := status(t, c, http.MethodPost, "/sales/_search", `{"size": 0, "aggs": {"top": {"bucket_script": {"buckets_path": {"t": "total"}, "script": {"source": "params.t"}}}}}`)
		if st != http.StatusBadRequest {
			t.Fatalf("status=%d body=%v", st, res)
		}
		if typ := errType(res); typ != "action_request_validation_exception" {
			t.Fatalf("type=%s body=%v", typ, res)
		}
	})
}

func TestBucketSelector(t *testing.T) {
	c := New()
	defer c.Close()
	bucketScriptIndex(t, c)

	t.Run("drops buckets the script rejects", func(t *testing.T) {
		res := mustDo(t, c, http.MethodPost, "/sales/_search", `{"size": 0, "aggs": {"cat": {"terms": {"field": "category"},
			"aggs": {"total": {"sum": {"field": "price"}}, "keep": {"bucket_selector": {"buckets_path": {"t": "total"}, "script": {"source": "params.t > 15"}}}}}}}`)
		buckets := bucketsByKey(t, res["aggregations"].(map[string]any), "cat")
		if len(buckets) != 1 {
			t.Fatalf("buckets = %v, want only fruit (total 30 > 15)", buckets)
		}
		if _, ok := buckets["fruit"]; !ok {
			t.Fatalf("buckets = %v, want fruit", buckets)
		}
	})

	t.Run("a non-boolean result fails the request", func(t *testing.T) {
		st, res := status(t, c, http.MethodPost, "/sales/_search", `{"size": 0, "aggs": {"cat": {"terms": {"field": "category"},
			"aggs": {"total": {"sum": {"field": "price"}}, "keep": {"bucket_selector": {"buckets_path": {"t": "total"}, "script": {"source": "params.t"}}}}}}}`)
		if st != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%v", st, res)
		}
	})

	t.Run("an empty bucket's gap value still runs the script, unlike bucket_script", func(t *testing.T) {
		// histogram(interval:5, min_doc_count:0) over prices 5,8,10,20 makes
		// an empty bucket at key 15 ([15,20)): its sum sub-agg has docCount
		// 0, so resolveBucketValue reports a NaN gap under the default skip
		// gap_policy. BucketSelectorPipelineAggregator has no skip branch —
		// it runs the script with that NaN and keeps the bucket iff the
		// script (here, always true) says so.
		res := mustDo(t, c, http.MethodPost, "/sales/_search", `{"size": 0, "aggs": {"h": {"histogram": {"field": "price", "interval": 5, "min_doc_count": 0},
			"aggs": {"s": {"sum": {"field": "price"}}, "keep": {"bucket_selector": {"buckets_path": {"t": "s"}, "script": {"source": "!(params.t > 1000)"}}}}}}}`)
		buckets := res["aggregations"].(map[string]any)["h"].(map[string]any)["buckets"].([]any)
		found := false
		for _, b := range buckets {
			bm := b.(map[string]any)
			if bm["key"].(float64) == 15 {
				found = true
				if bm["doc_count"].(float64) != 0 {
					t.Fatalf("bucket 15 doc_count = %v, want 0 (this test needs a genuine gap)", bm["doc_count"])
				}
			}
		}
		if !found {
			t.Fatalf("empty bucket at key 15 was dropped instead of kept: %v", buckets)
		}
	})
}
