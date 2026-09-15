package osmem

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Expectations in this file are OpenSearch 3.8.0 responses to the same
// requests.

func aggFidelityCluster(t *testing.T, index, mapping string, docs ...string) *Cluster {
	t.Helper()
	c := New()
	t.Cleanup(c.Close)
	mustDo(t, c, http.MethodPut, "/"+index, mapping)
	var bulk strings.Builder
	for i, doc := range docs {
		bulk.WriteString(`{"index":{"_index":"` + index + `","_id":"` + string(rune('1'+i)) + `"}}` + "\n" + doc + "\n")
	}
	if err := c.BulkString(bulk.String()); err != nil {
		t.Fatal(err)
	}
	return c
}

func aggFidelityJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func aggFidelityAggs(t *testing.T, c *Cluster, path, body string) map[string]any {
	t.Helper()
	return mustDo(t, c, http.MethodPost, path, body)["aggregations"].(map[string]any)
}

func TestAggregationFidelityTermsMinDocCountZeroAndTypedKeys(t *testing.T) {
	c := aggFidelityCluster(t, "tf", `{"mappings":{"properties":{"kw":{"type":"keyword"},"n":{"type":"long"},"q":{"type":"keyword"},"ip":{"type":"ip"}}}}`,
		`{"kw":"a","n":1,"q":"red","ip":"9.0.0.1"}`, `{"kw":"b","n":2,"q":"red","ip":"10.0.0.1"}`, `{"kw":"c","n":10,"q":"blue","ip":"2001:db8::1"}`,
		`{"kw":"d","n":9,"q":"green","ip":"1.2.3.4"}`)
	aggs := aggFidelityAggs(t, c, "/tf/_search", `{"size":0,"query":{"term":{"q":"red"}},"aggs":{"a":{"terms":{"field":"kw","min_doc_count":0}}}}`)
	if got := aggFidelityJSON(t, aggs["a"].(map[string]any)["buckets"]); got != `[{"doc_count":1,"key":"a"},{"doc_count":1,"key":"b"},{"doc_count":0,"key":"c"},{"doc_count":0,"key":"d"}]` {
		t.Fatalf("min_doc_count 0 buckets = %s", got)
	}
	// ip terms sort by the binary address
	aggs = aggFidelityAggs(t, c, "/tf/_search", `{"size":0,"aggs":{"a":{"terms":{"field":"ip","order":{"_key":"asc"}}}}}`)
	var keys []string
	for _, b := range aggs["a"].(map[string]any)["buckets"].([]any) {
		keys = append(keys, b.(map[string]any)["key"].(string))
	}
	if strings.Join(keys, ",") != "1.2.3.4,9.0.0.1,10.0.0.1,2001:db8::1" {
		t.Fatalf("ip key order = %v", keys)
	}
	res := mustDo(t, c, http.MethodPost, "/tf/_search?typed_keys=true", `{"size":0,"aggs":{"s":{"terms":{"field":"kw"},"aggs":{"m":{"avg":{"field":"n"}}}},"l":{"terms":{"field":"n"}},"p":{"percentiles":{"field":"n","percents":[50]}},"x":{"max_bucket":{"buckets_path":"s>_count"}},"y":{"sum_bucket":{"buckets_path":"s>_count"}}}}`)
	aggs = res["aggregations"].(map[string]any)
	for _, key := range []string{"sterms#s", "lterms#l", "tdigest_percentiles#p", "bucket_metric_value#x", "simple_value#y"} {
		if _, ok := aggs[key]; !ok {
			t.Fatalf("typed_keys: missing %s in %v", key, aggs)
		}
	}
	first := aggs["sterms#s"].(map[string]any)["buckets"].([]any)[0].(map[string]any)
	if _, ok := first["avg#m"]; !ok {
		t.Fatalf("typed_keys sub-aggregation: %v", first)
	}
}

func TestAggregationFidelityValidation(t *testing.T) {
	c := aggFidelityCluster(t, "tv", `{"mappings":{"properties":{"kw":{"type":"keyword"},"n":{"type":"long"}}}}`, `{"kw":"a","n":1}`)
	cases := []struct {
		body, typ, reason, cause string
		status                   int
	}{
		{`{"aggs":{"a":{"terms":{"field":"kw","size":-1}}}}`, "x_content_parse_exception", "[1:44] [terms] failed to parse field [size]", "[size] must be greater than 0. Found [-1] in [a]", 400},
		{`{"aggs":{"a[":{"terms":{"field":"kw"}}}}`, "parsing_exception", "Invalid aggregation name [a[]. Aggregation names can contain any character except '[', ']', and '>'", "", 400},
		{`{"aggs":{"a":{"avg":{"field":"n"},"aggs":{"b":{"max":{"field":"n"}}}}}}`, "aggregation_initialization_exception", "Aggregator [a] of type [avg] cannot accept sub-aggregations", "", 500},
		{`{"aggs":{"h":{"histogram":{"field":"n","interval":1}},"a":{"avg_bucket":{"buckets_path":"h>nope"}}}}`, "action_request_validation_exception", "Validation Failed: 1: No aggregation [nope] found for path [h>nope];", "", 400},
		{`{"aggs":{"a":{"date_histogram":{"field":"n","fixed_interval":"500micros"}}}}`, "search_phase_execution_exception", "all shards failed", "Zero or negative time interval not supported", 400},
		{`{"aggs":{"a":{"percentiles":{"field":"n","percents":[150]}}}}`, "x_content_parse_exception", "Failed to build [percentiles] after last required field arrived", "percent must be in [0,100], got [150.0]: [a]", 400},
		{`{"aggs":{"a":{"terms":{"field":"kw","order":{"nope":"desc"}}}}}`, "search_phase_execution_exception", "all shards failed", "Invalid aggregation order path [nope]. The provided aggregation [nope] either does not exist, or is a pipeline aggregation and cannot be used to sort the buckets.", 500},
	}
	for _, tc := range cases {
		code, res := status(t, c, http.MethodPost, "/tv/_search", tc.body)
		e, _ := res["error"].(map[string]any)
		if code != tc.status || e == nil || e["type"] != tc.typ || e["reason"] != tc.reason {
			t.Fatalf("%s: %d %v", tc.body, code, res)
		}
		if tc.cause != "" {
			cause, _ := e["caused_by"].(map[string]any)
			if cause == nil || cause["reason"] != tc.cause {
				t.Fatalf("%s: caused_by %v", tc.body, e["caused_by"])
			}
		}
	}
}

func TestAggregationFidelityPercentilesTDigest(t *testing.T) {
	c := aggFidelityCluster(t, "tp", `{"mappings":{"properties":{"price":{"type":"float"}}}}`,
		`{"price":12.99}`, `{"price":8.5}`, `{"price":45.0}`, `{"price":19.95}`)
	aggs := aggFidelityAggs(t, c, "/tp/_search", `{"size":0,"aggs":{"p":{"percentiles":{"field":"price","percents":[1,25,50,75]}},"r":{"percentile_ranks":{"field":"price","values":[10,20]}},"mad":{"median_absolute_deviation":{"field":"price"}}}}`)
	if got := aggFidelityJSON(t, aggs["p"]); got != `{"values":{"1.0":8.5,"25.0":12.989999771118164,"50.0":19.950000762939453,"75.0":45}}` {
		t.Fatalf("percentiles = %s", got)
	}
	if got := aggFidelityJSON(t, aggs["r"]); got != `{"values":{"10.0":25,"20.0":75}}` {
		t.Fatalf("percentile_ranks = %s", got)
	}
	if got := aggFidelityJSON(t, aggs["mad"]); got != `{"value":11.450000762939453}` {
		t.Fatalf("median_absolute_deviation = %s", got)
	}
}

func TestAggregationFidelityDatesRangesPipelines(t *testing.T) {
	c := aggFidelityCluster(t, "td", `{"mappings":{"properties":{"dt":{"type":"date"},"n":{"type":"long"},"v":{"type":"double"}}}}`,
		`{"dt":"2020-11-01T05:30:00Z","n":1,"v":10}`, `{"dt":"2020-11-01T06:30:00Z","n":10,"v":20}`, `{"dt":"2020-11-03T00:00:00Z","n":20}`, `{"dt":"2020-11-04T00:00:00Z","n":5,"v":60}`)
	// the repeated 01:00 hour of the DST end in New York is two buckets
	aggs := aggFidelityAggs(t, c, "/td/_search", `{"size":0,"query":{"range":{"dt":{"lt":"2020-11-02"}}},"aggs":{"h":{"date_histogram":{"field":"dt","calendar_interval":"hour","time_zone":"America/New_York"}}}}`)
	if got := aggFidelityJSON(t, aggs["h"].(map[string]any)["buckets"]); got != `[{"doc_count":1,"key":1604206800000,"key_as_string":"2020-11-01T01:00:00.000-04:00"},{"doc_count":1,"key":1604210400000,"key_as_string":"2020-11-01T01:00:00.000-05:00"}]` {
		t.Fatalf("DST hours = %s", got)
	}
	// ranges are sorted by from and to
	aggs = aggFidelityAggs(t, c, "/td/_search", `{"size":0,"aggs":{"r":{"range":{"field":"n","ranges":[{"from":10},{"to":5},{"from":5,"to":10}]}}}}`)
	var keys []string
	for _, b := range aggs["r"].(map[string]any)["buckets"].([]any) {
		keys = append(keys, b.(map[string]any)["key"].(string))
	}
	if strings.Join(keys, ",") != "*-5.0,5.0-10.0,10.0-*" {
		t.Fatalf("range keys = %v", keys)
	}
	// gap policies of sibling and parent pipelines
	aggs = aggFidelityAggs(t, c, "/td/_search", `{"size":0,"aggs":{"d":{"date_histogram":{"field":"dt","calendar_interval":"day"},"aggs":{"av":{"avg":{"field":"v"}},"dv":{"derivative":{"buckets_path":"av"}}}},"z":{"avg_bucket":{"buckets_path":"d>av","gap_policy":"insert_zeros"}},"m":{"min_bucket":{"buckets_path":"d>av","gap_policy":"insert_zeros"}}}}`)
	if got := aggFidelityJSON(t, aggs["z"]); got != `{"value":18.75}` {
		t.Fatalf("avg_bucket insert_zeros = %s", got)
	}
	if got := aggFidelityJSON(t, aggs["m"]); got != `{"keys":["2020-11-02T00:00:00.000Z","2020-11-03T00:00:00.000Z"],"value":0}` {
		t.Fatalf("min_bucket insert_zeros = %s", got)
	}
	buckets := aggs["d"].(map[string]any)["buckets"].([]any)
	if got := aggFidelityJSON(t, buckets[1].(map[string]any)["dv"]); got != `{"value":null}` {
		t.Fatalf("derivative over a gap = %s", got)
	}
}
