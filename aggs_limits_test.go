package osmem

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// status and body of a request, whatever its outcome.
func doStatus(t *testing.T, c *Cluster, method, path string, body any) (int, string) {
	t.Helper()
	res, err := c.Do(method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return res.StatusCode, string(res.Body)
}

// A histogram whose interval is below the precision of its keys, or whose
// bounds span more buckets than search.max_buckets, fails with
// too_many_buckets instead of looping forever or overflowing.
func TestHistogramFillDoesNotHang(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/h", `{"mappings":{"properties":{"x":{"type":"double"}}}}`)
	mustDo(t, c, http.MethodPut, "/h/_doc/1?refresh=true", `{"x":1e17}`)
	for _, body := range []string{
		`{"size":0,"aggs":{"h":{"histogram":{"field":"x","interval":1,"min_doc_count":0,"extended_bounds":{"min":1e17,"max":1.00000000000000016e17}}}}}`,
		`{"size":0,"aggs":{"h":{"histogram":{"field":"x","interval":1,"min_doc_count":0,"extended_bounds":{"min":-1e308,"max":1e308}}}}}`,
		`{"size":0,"aggs":{"h":{"histogram":{"field":"x","interval":1,"min_doc_count":0,"extended_bounds":{"min":"-Infinity","max":0}}}}}`,
		`{"size":0,"query":{"match_none":{}},"aggs":{"h":{"histogram":{"field":"x","interval":1e-300,"min_doc_count":0,"extended_bounds":{"min":0,"max":1}}}}}`,
	} {
		start := time.Now()
		st, res := doStatus(t, c, http.MethodPost, "/h/_search", body)
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("%s took %v", body, d)
		}
		if st != http.StatusServiceUnavailable || !strings.Contains(res, "too_many_buckets_exception") {
			t.Fatalf("%s: status %d %s", body, st, res)
		}
	}
	// NaN bounds add nothing, and a normal fill still works
	res := mustDo(t, c, http.MethodPost, "/h/_search", `{"size":0,"aggs":{"h":{"histogram":{"field":"x","interval":1,"min_doc_count":0,"extended_bounds":{"min":"NaN","max":"NaN"}}}}}`)
	if n := len(res["aggregations"].(map[string]any)["h"].(map[string]any)["buckets"].([]any)); n != 1 {
		t.Fatalf("NaN bounds: %d buckets", n)
	}
	mustDo(t, c, http.MethodPut, "/h/_doc/2?refresh=true", `{"x":3}`)
	res = mustDo(t, c, http.MethodPost, "/h/_search", `{"size":0,"query":{"term":{"_id":"2"}},"aggs":{"h":{"histogram":{"field":"x","interval":2,"offset":1,"min_doc_count":0,"extended_bounds":{"min":-2,"max":8}}}}}`)
	var keys []float64
	for _, b := range res["aggregations"].(map[string]any)["h"].(map[string]any)["buckets"].([]any) {
		keys = append(keys, b.(map[string]any)["key"].(float64))
	}
	if want := []float64{-3, -1, 1, 3, 5, 7}; len(keys) != len(want) || func() bool {
		for i := range keys {
			if keys[i] != want[i] {
				return true
			}
		}
		return false
	}() {
		t.Fatalf("filled keys %v, want %v", keys, want)
	}
}

// A percentiles request with an absurd compression is served instead of
// allocating gigabytes or panicking.
func TestPercentilesHugeCompression(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/p", `{"mappings":{"properties":{"x":{"type":"double"}}}}`)
	for i := 1; i <= 5; i++ {
		mustDo(t, c, http.MethodPut, "/p/_doc/"+string(rune('0'+i))+"?refresh=true", `{"x":`+string(rune('0'+i))+`}`)
	}
	for _, body := range []string{
		`{"size":0,"aggs":{"p":{"percentiles":{"field":"x","percents":[50],"tdigest":{"compression":1e300}}}}}`,
		`{"size":0,"aggs":{"p":{"percentiles":{"field":"x","percents":[50],"tdigest":{"compression":1e7}}}}}`,
		`{"size":0,"aggs":{"p":{"median_absolute_deviation":{"field":"x","compression":1e300}}}}`,
	} {
		res := mustDo(t, c, http.MethodPost, "/p/_search", body)
		out, _ := json.Marshal(res["aggregations"])
		if !strings.Contains(string(out), `"50.0":3`) && !strings.Contains(string(out), `"value":1`) {
			t.Fatalf("%s: %s", body, out)
		}
	}
}

// The buckets target of auto_date_histogram is capped at search.max_buckets
// divided by the largest interval multiplier of the roundings left by
// minimum_interval (AutoDateHistogramAggregationBuilder.innerBuild).
func TestAutoDateHistogramBucketsCeiling(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/adh", `{"mappings":{"properties":{"d":{"type":"date"}}}}`)
	mustDo(t, c, http.MethodPut, "/adh/_doc/1?refresh=true", `{"d":"2024-01-01T00:00:00Z"}`)
	for _, tc := range []struct {
		body    string
		ceiling int
	}{
		{`{"size":0,"aggs":{"h":{"auto_date_histogram":{"field":"d","buckets":2185}}}}`, 2184},
		{`{"size":0,"aggs":{"h":{"auto_date_histogram":{"field":"d","buckets":5462,"minimum_interval":"hour"}}}}`, 5461},
		{`{"size":0,"aggs":{"h":{"auto_date_histogram":{"field":"d","buckets":9363,"minimum_interval":"day"}}}}`, 9362},
		{`{"size":0,"aggs":{"h":{"auto_date_histogram":{"field":"d","buckets":21846,"minimum_interval":"month"}}}}`, 21845},
	} {
		st, res := doStatus(t, c, http.MethodPost, "/adh/_search", tc.body)
		if want := `"buckets must be less than ` + strconv.Itoa(tc.ceiling) + `"`; st != http.StatusBadRequest || !strings.Contains(res, `"illegal_argument_exception"`) || !strings.Contains(res, want) {
			t.Fatalf("%s: status %d %s", tc.body, st, res)
		}
	}
	// at the ceiling the request is served
	for _, body := range []string{
		`{"size":0,"aggs":{"h":{"auto_date_histogram":{"field":"d","buckets":2184}}}}`,
		`{"size":0,"aggs":{"h":{"auto_date_histogram":{"field":"d","buckets":5461,"minimum_interval":"hour"}}}}`,
	} {
		if st, res := doStatus(t, c, http.MethodPost, "/adh/_search", body); st != http.StatusOK {
			t.Fatalf("%s: status %d %s", body, st, res)
		}
	}
}

// A filter that is not an object is a parsing error, not a panic.
func TestFiltersRejectNonObjectFilters(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/f", `{"mappings":{"properties":{"x":{"type":"keyword"}}}}`)
	mustDo(t, c, http.MethodPut, "/f/_doc/1?refresh=true", `{"x":"a"}`)
	for _, body := range []string{
		`{"size":0,"aggs":{"f":{"filters":{"filters":[true]}}}}`,
		`{"size":0,"aggs":{"f":{"filters":{"filters":[{"match_all":{}},"x"]}}}}`,
		`{"size":0,"aggs":{"f":{"filters":{"filters":{"a":null}}}}}`,
		`{"size":0,"aggs":{"f":{"filters":{"filters":{"a":{"match_all":{}},"b":1}}}}}`,
		`{"size":0,"aggs":{"f":{"adjacency_matrix":{"filters":{"a":null}}}}}`,
	} {
		st, res := doStatus(t, c, http.MethodPost, "/f/_search", body)
		if st != http.StatusBadRequest || !strings.Contains(res, "[_na] query malformed, must start with start_object") {
			t.Fatalf("%s: status %d %s", body, st, res)
		}
	}
}

// Bucket aggregations fail with too_many_buckets as soon as their buckets
// exceed search.max_buckets, before the rest are materialized.
func TestBucketLimitFailsEarly(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/lim", `{"mappings":{"properties":{"k":{"type":"keyword"},"n":{"type":"integer"},"d":{"type":"date"}}}}`)
	for i := 0; i < 6; i++ {
		mustDo(t, c, http.MethodPut, "/lim/_doc/"+strconv.Itoa(i)+"?refresh=true", `{"k":"k`+strconv.Itoa(i)+`","n":`+strconv.Itoa(i)+`,"d":"2024-01-0`+strconv.Itoa(i+1)+`T00:00:00Z"}`)
	}
	mustDo(t, c, http.MethodPut, "/_cluster/settings", `{"transient":{"search.max_buckets":3}}`)
	for _, body := range []string{
		`{"size":0,"aggs":{"a":{"terms":{"field":"k"}}}}`,
		`{"size":0,"aggs":{"a":{"multi_terms":{"terms":[{"field":"k"},{"field":"n"}]}}}}`,
		`{"size":0,"aggs":{"a":{"composite":{"sources":[{"k":{"terms":{"field":"k"}}}]}}}}`,
		`{"size":0,"aggs":{"a":{"filters":{"filters":[{"match_all":{}},{"match_all":{}},{"match_all":{}},{"match_all":{}}]}}}}`,
		`{"size":0,"aggs":{"a":{"filters":{"other_bucket":true,"filters":[{"match_all":{}},{"match_all":{}},{"match_all":{}}]}}}}`,
		`{"size":0,"aggs":{"a":{"date_histogram":{"field":"d","calendar_interval":"day"}}}}`,
		`{"size":0,"aggs":{"a":{"histogram":{"field":"n","interval":1}}}}`,
		// a fill over a span of billions of buckets must fail without materializing them
		`{"size":0,"query":{"match_none":{}},"aggs":{"a":{"date_histogram":{"field":"d","fixed_interval":"1s","min_doc_count":0,"extended_bounds":{"min":"1970-01-01","max":"2100-01-01"}}}}}`,
		`{"size":0,"query":{"term":{"k":"k0"}},"aggs":{"a":{"date_histogram":{"field":"d","fixed_interval":"1ms","min_doc_count":0,"extended_bounds":{"min":"1970-01-01","max":"2100-01-01"}}}}}`,
	} {
		start := time.Now()
		st, res := doStatus(t, c, http.MethodPost, "/lim/_search", body)
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("%s took %v", body, d)
		}
		if st != http.StatusServiceUnavailable || !strings.Contains(res, "too_many_buckets_exception") || !strings.Contains(res, "Must be less than or equal to: [3]") {
			t.Fatalf("%s: status %d %s", body, st, res)
		}
	}
	// within the limit the aggregations are served
	for _, body := range []string{
		`{"size":0,"aggs":{"a":{"terms":{"field":"k","size":3}}}}`,
		`{"size":0,"aggs":{"a":{"composite":{"size":3,"sources":[{"k":{"terms":{"field":"k"}}}]}}}}`,
		`{"size":0,"aggs":{"a":{"filters":{"filters":[{"match_all":{}},{"match_all":{}},{"match_all":{}}]}}}}`,
		`{"size":0,"query":{"term":{"k":"k0"}},"aggs":{"a":{"date_histogram":{"field":"d","calendar_interval":"day","min_doc_count":0,"extended_bounds":{"min":"2024-01-01","max":"2024-01-03"}}}}}`,
	} {
		if st, res := doStatus(t, c, http.MethodPost, "/lim/_search", body); st != http.StatusOK {
			t.Fatalf("%s: status %d %s", body, st, res)
		}
	}
}

// max_shingle_size of search_as_you_type must be in [2, 4].
func TestSearchAsYouTypeMaxShingleSizeRange(t *testing.T) {
	c := New()
	defer c.Close()
	for _, n := range []int{1, 5, 0, -1} {
		st, res := doStatus(t, c, http.MethodPut, "/sayt"+strconv.Itoa(n+1), `{"mappings":{"properties":{"t":{"type":"search_as_you_type","max_shingle_size":`+strconv.Itoa(n)+`}}}}`)
		if st != http.StatusBadRequest || !strings.Contains(res, "mapper_parsing_exception") || !strings.Contains(res, "[max_shingle_size] must be at least [2] and at most [4], got ["+strconv.Itoa(n)+"]") {
			t.Fatalf("max_shingle_size %d: status %d %s", n, st, res)
		}
	}
	for _, n := range []int{2, 3, 4} {
		if st, res := doStatus(t, c, http.MethodPut, "/sayt-ok"+strconv.Itoa(n), `{"mappings":{"properties":{"t":{"type":"search_as_you_type","max_shingle_size":`+strconv.Itoa(n)+`}}}}`); st != http.StatusOK {
			t.Fatalf("max_shingle_size %d: status %d %s", n, st, res)
		}
	}
}

// Range aggregations read the values of a hit once and still count it in
// every overlapping range it falls in.
func TestRangeAggregationsOverlappingRanges(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/rng", `{"mappings":{"properties":{"n":{"type":"integer"},"ip":{"type":"ip"},"loc":{"type":"geo_point"}}}}`)
	mustDo(t, c, http.MethodPut, "/rng/_doc/1?refresh=true", `{"n":[5,50],"ip":["10.0.0.5","192.168.1.1"],"loc":{"lat":35.68,"lon":139.76}}`)
	mustDo(t, c, http.MethodPut, "/rng/_doc/2?refresh=true", `{"n":15,"ip":"10.0.0.200","loc":{"lat":34.69,"lon":135.50}}`)
	counts := func(res map[string]any) []float64 {
		var out []float64
		for _, b := range res["aggregations"].(map[string]any)["a"].(map[string]any)["buckets"].([]any) {
			out = append(out, b.(map[string]any)["doc_count"].(float64))
		}
		return out
	}
	same := func(got, want []float64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	res := mustDo(t, c, http.MethodPost, "/rng/_search", `{"size":0,"aggs":{"a":{"range":{"field":"n","ranges":[{"to":10},{"from":0,"to":20},{"from":10},{"from":40,"to":60}]}}}}`)
	if got := counts(res); !same(got, []float64{1, 2, 2, 1}) {
		t.Fatalf("range counts %v", got)
	}
	res = mustDo(t, c, http.MethodPost, "/rng/_search", `{"size":0,"aggs":{"a":{"ip_range":{"field":"ip","ranges":[{"to":"10.0.0.100"},{"from":"10.0.0.0","to":"10.0.1.0"},{"from":"192.168.0.0"}]}}}}`)
	if got := counts(res); !same(got, []float64{1, 2, 1}) {
		t.Fatalf("ip_range counts %v", got)
	}
	res = mustDo(t, c, http.MethodPost, "/rng/_search", `{"size":0,"aggs":{"a":{"geo_distance":{"field":"loc","origin":"35.68,139.76","unit":"km","ranges":[{"to":100},{"from":0,"to":1000},{"from":300}]}}}}`)
	if got := counts(res); !same(got, []float64{1, 2, 1}) {
		t.Fatalf("geo_distance counts %v", got)
	}
}
