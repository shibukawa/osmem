package osmem

import (
	"math"
	"net/http"
	"testing"
)

// Expected values in this file were observed against a live OpenSearch
// 3.8.0 instance (rare_terms, significant_terms, significant_text,
// auto_date_histogram, variable_width_histogram were all previously
// unimplemented by osmem).

func TestRareTermsAggregationCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/rare-compat", `{"settings":{"number_of_replicas":0},"mappings":{"properties":{
		"str":{"type":"keyword"},"ip":{"type":"ip"},"boolean":{"type":"boolean"},"integer":{"type":"long"},"date":{"type":"date"},"dbl":{"type":"double"}
	}}}`)
	if err := c.BulkString(`{"index":{"_index":"rare-compat","_id":"1"}}
{"str":"abc","integer":1234,"boolean":true,"date":"2016-05-03","ip":"::1"}
{"index":{"_index":"rare-compat","_id":"2"}}
{"str":"abc","integer":5678,"boolean":false,"date":"2014-09-01","ip":"127.0.0.1"}
{"index":{"_index":"rare-compat","_id":"3"}}
{"str":"bcd","integer":1234,"boolean":true,"date":"2016-05-03","ip":"::1"}
`); err != nil {
		t.Fatal(err)
	}

	// basic: default max_doc_count is 1, key_as_string absent for a keyword field
	res := mustDo(t, c, http.MethodPost, "/rare-compat/_search", `{"size":0,"aggs":{"str_terms":{"rare_terms":{"field":"str","max_doc_count":1}}}}`)
	buckets := res["aggregations"].(map[string]any)["str_terms"].(map[string]any)["buckets"].([]any)
	if len(buckets) != 1 {
		t.Fatalf("str rare_terms buckets = %v, want 1", buckets)
	}
	b0 := buckets[0].(map[string]any)
	if b0["key"] != "bcd" || b0["doc_count"].(float64) != 1 {
		t.Fatalf("str rare_terms bucket = %v, want key bcd doc_count 1", b0)
	}
	if _, ok := b0["key_as_string"]; ok {
		t.Fatalf("str rare_terms bucket has key_as_string: %v", b0)
	}

	// ordering: ascending doc_count then key; boolean/date render key_as_string
	res = mustDo(t, c, http.MethodPost, "/rare-compat/_search", `{"size":0,"aggs":{"a":{"rare_terms":{"field":"integer"}}}}`)
	buckets = res["aggregations"].(map[string]any)["a"].(map[string]any)["buckets"].([]any)
	if len(buckets) != 1 || buckets[0].(map[string]any)["key"].(float64) != 5678 {
		t.Fatalf("integer rare_terms buckets = %v, want [5678]", buckets)
	}

	res = mustDo(t, c, http.MethodPost, "/rare-compat/_search", `{"size":0,"aggs":{"a":{"rare_terms":{"field":"boolean"}}}}`)
	b0 = res["aggregations"].(map[string]any)["a"].(map[string]any)["buckets"].([]any)[0].(map[string]any)
	if b0["key"].(float64) != 0 || b0["key_as_string"] != "false" || b0["doc_count"].(float64) != 1 {
		t.Fatalf("boolean rare_terms bucket = %v, want key 0/false doc_count 1", b0)
	}

	res = mustDo(t, c, http.MethodPost, "/rare-compat/_search", `{"size":0,"aggs":{"a":{"rare_terms":{"field":"date"}}}}`)
	b0 = res["aggregations"].(map[string]any)["a"].(map[string]any)["buckets"].([]any)[0].(map[string]any)
	if b0["key"].(float64) != 1409529600000 || b0["key_as_string"] != "2014-09-01T00:00:00.000Z" {
		t.Fatalf("date rare_terms bucket = %v", b0)
	}

	// include/exclude
	res = mustDo(t, c, http.MethodPost, "/rare-compat/_search", `{"size":0,"aggs":{"a":{"rare_terms":{"field":"ip","exclude":["127.0.0.1"]}}}}`)
	buckets = res["aggregations"].(map[string]any)["a"].(map[string]any)["buckets"].([]any)
	if len(buckets) != 0 {
		t.Fatalf("ip rare_terms with exclude = %v, want none", buckets)
	}
	res = mustDo(t, c, http.MethodPost, "/rare-compat/_search", `{"size":0,"aggs":{"a":{"rare_terms":{"field":"ip","include":["127.0.0.1"]}}}}`)
	buckets = res["aggregations"].(map[string]any)["a"].(map[string]any)["buckets"].([]any)
	if len(buckets) != 1 || buckets[0].(map[string]any)["key"] != "127.0.0.1" {
		t.Fatalf("ip rare_terms with include = %v, want [127.0.0.1]", buckets)
	}
	// regex include/exclude is rejected for IP (bytes-like but not "string")
	code, errRes := status(t, c, http.MethodPost, "/rare-compat/_search", `{"size":0,"aggs":{"ip_terms":{"rare_terms":{"field":"ip","exclude":"127.*"}}}}`)
	root := errRes["error"].(map[string]any)["root_cause"].([]any)[0].(map[string]any)
	wantReason := "Aggregation [ip_terms] cannot support regular expression style include/exclude settings as they can only be applied to string fields. Use an array of values for include/exclude clauses"
	if code != http.StatusBadRequest || errType(errRes) != "search_phase_execution_exception" || root["type"] != "illegal_argument_exception" || root["reason"] != wantReason {
		t.Fatalf("ip rare_terms regex exclude status/type/root = %d/%s/%v, want 400/search_phase_execution_exception/%q", code, errType(errRes), root, wantReason)
	}

	// typed_keys: srareterms for strings/ip, lrareterms for long/date/boolean
	res = mustDo(t, c, http.MethodPost, "/rare-compat/_search?typed_keys=true", `{"size":0,"aggs":{"a":{"rare_terms":{"field":"str"}}}}`)
	if _, ok := res["aggregations"].(map[string]any)["srareterms#a"]; !ok {
		t.Fatalf("string rare_terms typed key = %v, want srareterms#a", res["aggregations"])
	}
	res = mustDo(t, c, http.MethodPost, "/rare-compat/_search?typed_keys=true", `{"size":0,"aggs":{"a":{"rare_terms":{"field":"integer"}}}}`)
	if _, ok := res["aggregations"].(map[string]any)["lrareterms#a"]; !ok {
		t.Fatalf("long rare_terms typed key = %v, want lrareterms#a", res["aggregations"])
	}

	// floating point fields are rejected outright (wrapped as a shard failure,
	// like the live server's search_phase_execution_exception/root_cause)
	code, errRes = status(t, c, http.MethodPost, "/rare-compat/_search", `{"size":0,"aggs":{"a":{"rare_terms":{"field":"dbl"}}}}`)
	root = errRes["error"].(map[string]any)["root_cause"].([]any)[0].(map[string]any)
	if code != http.StatusBadRequest || errType(errRes) != "search_phase_execution_exception" || root["type"] != "illegal_argument_exception" {
		t.Fatalf("rare_terms on double field status/type/root = %d/%s/%v, want 400/search_phase_execution_exception/illegal_argument_exception", code, errType(errRes), root)
	}

	// max_doc_count bounds: (0,100]
	code, errRes = status(t, c, http.MethodPost, "/rare-compat/_search", `{"size":0,"aggs":{"a":{"rare_terms":{"field":"str","max_doc_count":101}}}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("max_doc_count=101 status = %d, want 400", code)
	}

	// sub-aggregation
	res = mustDo(t, c, http.MethodPost, "/rare-compat/_search", `{"size":0,"aggs":{"str_terms":{"rare_terms":{"field":"str","max_doc_count":1},"aggs":{"max_n":{"max":{"field":"integer"}}}}}}`)
	b0 = res["aggregations"].(map[string]any)["str_terms"].(map[string]any)["buckets"].([]any)[0].(map[string]any)
	if b0["max_n"].(map[string]any)["value"].(float64) != 1234 {
		t.Fatalf("rare_terms sub-aggregation = %v, want max_n.value 1234", b0)
	}
}

func TestSignificantTermsAggregationCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	// a multi-valued keyword field stands in for the OpenSearch fixture's
	// analyzed "good"/"good bad"/"bad" text (a doc's array of tags is
	// exactly the per-doc term set that fielddata:true text tokenization
	// would otherwise produce; term-frequency math is identical either way)
	mustDo(t, c, http.MethodPut, "/sigterms-compat", `{"settings":{"number_of_shards":1},"mappings":{"properties":{"tags":{"type":"keyword"},"class":{"type":"keyword"}}}}`)
	if err := c.BulkString(`{"index":{"_index":"sigterms-compat","_id":"1"}}
{"tags":["good"],"class":"good"}
{"index":{"_index":"sigterms-compat","_id":"2"}}
{"tags":["good"],"class":"good"}
{"index":{"_index":"sigterms-compat","_id":"3"}}
{"tags":["bad"],"class":"bad"}
{"index":{"_index":"sigterms-compat","_id":"4"}}
{"tags":["bad"],"class":"bad"}
{"index":{"_index":"sigterms-compat","_id":"5"}}
{"tags":["good","bad"],"class":"good"}
{"index":{"_index":"sigterms-compat","_id":"6"}}
{"tags":["good","bad"],"class":"bad"}
{"index":{"_index":"sigterms-compat","_id":"7"}}
{"tags":["bad"],"class":"bad"}
`); err != nil {
		t.Fatal(err)
	}

	find := func(t *testing.T, buckets []any, key string) map[string]any {
		t.Helper()
		for _, raw := range buckets {
			b := raw.(map[string]any)
			if b["key"] == key {
				return b
			}
		}
		t.Fatalf("bucket %q not found in %v", key, buckets)
		return nil
	}

	// default jlh heuristic: background is the whole index (7 docs)
	// regardless of the parent terms bucket's own doc count
	res := mustDo(t, c, http.MethodPost, "/sigterms-compat/_search", `{"size":0,"aggs":{"class":{"terms":{"field":"class"},"aggs":{"sig":{"significant_terms":{"field":"tags","min_doc_count":1}}}}}}`)
	buckets := res["aggregations"].(map[string]any)["class"].(map[string]any)["buckets"].([]any)

	bad := find(t, buckets, "bad")["sig"].(map[string]any)
	if bad["doc_count"].(float64) != 4 || bad["bg_count"].(float64) != 7 {
		t.Fatalf("bad sig = %v, want doc_count 4 bg_count 7", bad)
	}
	badBuckets := bad["buckets"].([]any)
	if len(badBuckets) != 1 {
		t.Fatalf("bad sig buckets = %v, want exactly 1 (only 'bad' is over-represented in class=bad)", badBuckets)
	}
	badTerm := badBuckets[0].(map[string]any)
	if badTerm["key"] != "bad" || badTerm["doc_count"].(float64) != 4 || badTerm["bg_count"].(float64) != 5 {
		t.Fatalf("bad term bucket = %v, want key bad doc_count 4 bg_count 5", badTerm)
	}
	if got := badTerm["score"].(float64); got != 0.39999999999999997 {
		t.Fatalf("bad jlh score = %v, want 0.39999999999999997", got)
	}

	good := find(t, buckets, "good")["sig"].(map[string]any)
	goodTerm := good["buckets"].([]any)[0].(map[string]any)
	if goodTerm["key"] != "good" || goodTerm["doc_count"].(float64) != 3 || goodTerm["bg_count"].(float64) != 4 {
		t.Fatalf("good term bucket = %v, want key good doc_count 3 bg_count 4", goodTerm)
	}
	if got := goodTerm["score"].(float64); got != 0.75 {
		t.Fatalf("good jlh score = %v, want 0.75", got)
	}

	// default min_doc_count is 3 (not 1 like terms); with foreground ==
	// background (no parent split) every term's ratio is identical, so
	// nothing is significant regardless
	res = mustDo(t, c, http.MethodPost, "/sigterms-compat/_search", `{"size":0,"aggs":{"sig":{"significant_terms":{"field":"tags"}}}}`)
	sig := res["aggregations"].(map[string]any)["sig"].(map[string]any)
	if len(sig["buckets"].([]any)) != 0 {
		t.Fatalf("whole-index significant_terms buckets = %v, want none", sig["buckets"])
	}

	// other heuristics, scored over the same bad/good buckets
	cases := []struct {
		name        string
		clause      string
		bad, good   float64
		approximate bool
	}{
		{"percentage", `"percentage":{}`, 0.8, 0.75, false},
		{"chi_square", `"chi_square":{}`, 3.7333333333333334, 3.9375, false},
		{"gnd", `"gnd":{}`, 0.6711623607546782, 0.7121057487429687, true},
		{"mutual_information", `"mutual_information":{}`, 0.4695652111147069, 0.5216406363433184, true},
	}
	for _, tc := range cases {
		body := `{"size":0,"aggs":{"class":{"terms":{"field":"class"},"aggs":{"sig":{"significant_terms":{"field":"tags","min_doc_count":1,` + tc.clause + `}}}}}}`
		res := mustDo(t, c, http.MethodPost, "/sigterms-compat/_search", body)
		buckets := res["aggregations"].(map[string]any)["class"].(map[string]any)["buckets"].([]any)
		badScore := find(t, buckets, "bad")["sig"].(map[string]any)["buckets"].([]any)[0].(map[string]any)["score"].(float64)
		goodScore := find(t, buckets, "good")["sig"].(map[string]any)["buckets"].([]any)[0].(map[string]any)["score"].(float64)
		if tc.approximate {
			if math.Abs(badScore-tc.bad) > 1e-9 || math.Abs(goodScore-tc.good) > 1e-9 {
				t.Fatalf("%s scores = %v/%v, want %v/%v", tc.name, badScore, goodScore, tc.bad, tc.good)
			}
		} else if badScore != tc.bad || goodScore != tc.good {
			t.Fatalf("%s scores = %v/%v, want %v/%v", tc.name, badScore, goodScore, tc.bad, tc.good)
		}
	}

	// typed_keys
	res = mustDo(t, c, http.MethodPost, "/sigterms-compat/_search?typed_keys=true", `{"size":0,"aggs":{"sig":{"significant_terms":{"field":"tags","min_doc_count":1}}}}`)
	if _, ok := res["aggregations"].(map[string]any)["sigsterms#sig"]; !ok {
		t.Fatalf("significant_terms typed key = %v, want sigsterms#sig", res["aggregations"])
	}

	// IP field: include/exclude by value, and regex include/exclude rejected
	mustDo(t, c, http.MethodPut, "/sigterms-ip-compat", `{"mappings":{"properties":{"ip":{"type":"ip"}}}}`)
	mustDo(t, c, http.MethodPut, "/sigterms-ip-compat/_doc/1", `{"ip":"::1"}`)
	mustDo(t, c, http.MethodPut, "/sigterms-ip-compat/_doc/2", `{}`)
	res = mustDo(t, c, http.MethodPost, "/sigterms-ip-compat/_search", `{"size":0,"query":{"exists":{"field":"ip"}},"aggs":{"ip_terms":{"significant_terms":{"field":"ip","min_doc_count":1}}}}`)
	ipBuckets := res["aggregations"].(map[string]any)["ip_terms"].(map[string]any)["buckets"].([]any)
	if len(ipBuckets) != 1 || ipBuckets[0].(map[string]any)["key"] != "::1" {
		t.Fatalf("ip significant_terms buckets = %v, want [::1]", ipBuckets)
	}
	res = mustDo(t, c, http.MethodPost, "/sigterms-ip-compat/_search", `{"size":0,"query":{"exists":{"field":"ip"}},"aggs":{"ip_terms":{"significant_terms":{"field":"ip","min_doc_count":1,"exclude":["::1"]}}}}`)
	if got := res["aggregations"].(map[string]any)["ip_terms"].(map[string]any)["buckets"].([]any); len(got) != 0 {
		t.Fatalf("ip significant_terms with exclude = %v, want none", got)
	}
	code, errRes := status(t, c, http.MethodPost, "/sigterms-ip-compat/_search", `{"size":0,"aggs":{"ip_terms":{"significant_terms":{"field":"ip","exclude":"127.*"}}}}`)
	root := errRes["error"].(map[string]any)["root_cause"].([]any)[0].(map[string]any)
	wantReason := "Aggregation [ip_terms] cannot support regular expression style include/exclude settings as they can only be applied to string fields. Use an array of values for include/exclude clauses"
	if code != http.StatusBadRequest || errType(errRes) != "search_phase_execution_exception" || root["type"] != "illegal_argument_exception" || root["reason"] != wantReason {
		t.Fatalf("ip significant_terms regex exclude status/type/root = %d/%s/%v, want 400/search_phase_execution_exception/%q", code, errType(errRes), root, wantReason)
	}
}

func TestSignificantTextAggregationCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	// fielddata:false: significant_text must work by re-analyzing _source,
	// not by reading doc values the way significant_terms does
	mustDo(t, c, http.MethodPut, "/sigtext-compat", `{"settings":{"number_of_shards":1},"mappings":{"properties":{"text":{"type":"text","fielddata":false},"class":{"type":"keyword"}}}}`)
	if err := c.BulkString(`{"index":{"_index":"sigtext-compat","_id":"1"}}
{"text":"good","class":"good"}
{"index":{"_index":"sigtext-compat","_id":"2"}}
{"text":"good","class":"good"}
{"index":{"_index":"sigtext-compat","_id":"3"}}
{"text":"bad","class":"bad"}
{"index":{"_index":"sigtext-compat","_id":"4"}}
{"text":"bad","class":"bad"}
{"index":{"_index":"sigtext-compat","_id":"5"}}
{"text":"good bad","class":"good"}
{"index":{"_index":"sigtext-compat","_id":"6"}}
{"text":"good bad","class":"bad"}
{"index":{"_index":"sigtext-compat","_id":"7"}}
{"text":"bad","class":"bad"}
`); err != nil {
		t.Fatal(err)
	}

	res := mustDo(t, c, http.MethodPost, "/sigtext-compat/_search", `{"size":0,"aggs":{"class":{"terms":{"field":"class"},"aggs":{"sig_text":{"significant_text":{"field":"text"}}}}}}`)
	buckets := res["aggregations"].(map[string]any)["class"].(map[string]any)["buckets"].([]any)
	for _, raw := range buckets {
		b := raw.(map[string]any)
		top := b["sig_text"].(map[string]any)["buckets"].([]any)[0].(map[string]any)["key"]
		if b["key"] == "bad" && top != "bad" {
			t.Fatalf("bad class top significant_text term = %v, want bad", top)
		}
		if b["key"] == "good" && top != "good" {
			t.Fatalf("good class top significant_text term = %v, want good", top)
		}
	}

	res = mustDo(t, c, http.MethodPost, "/sigtext-compat/_search?typed_keys=true", `{"size":0,"aggs":{"sig_text":{"significant_text":{"field":"text","min_doc_count":1}}}}`)
	if _, ok := res["aggregations"].(map[string]any)["sigsterms#sig_text"]; !ok {
		t.Fatalf("significant_text typed key = %v, want sigsterms#sig_text", res["aggregations"])
	}
}

func TestAutoDateHistogramAggregationCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/auto-date-compat", `{"settings":{"number_of_shards":1,"number_of_replicas":0},"mappings":{"properties":{"date":{"type":"date"}}}}`)
	if err := c.BulkString(`{"index":{"_index":"auto-date-compat","_id":"1"}}
{"date":"2020-03-01","v":1}
{"index":{"_index":"auto-date-compat","_id":"2"}}
{"date":"2020-03-02","v":2}
{"index":{"_index":"auto-date-compat","_id":"3"}}
{"date":"2020-03-08","v":3}
{"index":{"_index":"auto-date-compat","_id":"4"}}
{"date":"2020-03-09","v":4}
`); err != nil {
		t.Fatal(err)
	}

	// the finest rounding whose bucket count fits within the target: 7d
	// gives exactly 2 buckets for this span, while 1d would give 9
	res := mustDo(t, c, http.MethodPost, "/auto-date-compat/_search", `{"size":0,"aggs":{"histo":{"auto_date_histogram":{"field":"date","buckets":2}}}}`)
	histo := res["aggregations"].(map[string]any)["histo"].(map[string]any)
	if histo["interval"] != "7d" {
		t.Fatalf("interval = %v, want 7d", histo["interval"])
	}
	buckets := histo["buckets"].([]any)
	if len(buckets) != 2 {
		t.Fatalf("buckets = %v, want 2", buckets)
	}
	b0, b1 := buckets[0].(map[string]any), buckets[1].(map[string]any)
	if b0["key_as_string"] != "2020-03-01T00:00:00.000Z" || b0["doc_count"].(float64) != 2 {
		t.Fatalf("bucket0 = %v", b0)
	}
	if b1["key_as_string"] != "2020-03-08T00:00:00.000Z" || b1["doc_count"].(float64) != 2 {
		t.Fatalf("bucket1 = %v", b1)
	}

	// buckets=1: 7d still gives 2 buckets (too many), escalates to 1M
	res = mustDo(t, c, http.MethodPost, "/auto-date-compat/_search", `{"size":0,"aggs":{"histo":{"auto_date_histogram":{"field":"date","buckets":1}}}}`)
	histo = res["aggregations"].(map[string]any)["histo"].(map[string]any)
	if histo["interval"] != "1M" || len(histo["buckets"].([]any)) != 1 {
		t.Fatalf("buckets=1 histo = %v, want interval 1M with 1 bucket", histo)
	}

	// default buckets target is 10: 1d rounding gives exactly 9 buckets
	res = mustDo(t, c, http.MethodPost, "/auto-date-compat/_search", `{"size":0,"aggs":{"histo":{"auto_date_histogram":{"field":"date"}}}}`)
	histo = res["aggregations"].(map[string]any)["histo"].(map[string]any)
	if histo["interval"] != "1d" || len(histo["buckets"].([]any)) != 9 {
		t.Fatalf("default buckets histo = %v, want interval 1d with 9 buckets", histo)
	}

	// multi-unit tiers (multiple > 1) anchor to the data's own minimum value
	// rounded down to their base unit, not to a fixed reference since epoch:
	// data starting at 01:17 buckets a 12h tier from 01:00, not midnight/noon
	mustDo(t, c, http.MethodPut, "/auto-date-compat-2", `{"settings":{"number_of_shards":1,"number_of_replicas":0},"mappings":{"properties":{"date":{"type":"date"}}}}`)
	if err := c.BulkString(`{"index":{"_index":"auto-date-compat-2","_id":"1"}}
{"date":"2020-03-01T01:17:00Z"}
{"index":{"_index":"auto-date-compat-2","_id":"2"}}
{"date":"2020-03-01T05:17:00Z"}
{"index":{"_index":"auto-date-compat-2","_id":"3"}}
{"date":"2020-03-01T09:17:00Z"}
{"index":{"_index":"auto-date-compat-2","_id":"4"}}
{"date":"2020-03-01T13:17:00Z"}
`); err != nil {
		t.Fatal(err)
	}
	res = mustDo(t, c, http.MethodPost, "/auto-date-compat-2/_search", `{"size":0,"aggs":{"histo":{"auto_date_histogram":{"field":"date","buckets":4}}}}`)
	histo = res["aggregations"].(map[string]any)["histo"].(map[string]any)
	if histo["interval"] != "12h" {
		t.Fatalf("interval = %v, want 12h", histo["interval"])
	}
	buckets = histo["buckets"].([]any)
	if len(buckets) != 2 || buckets[0].(map[string]any)["key_as_string"] != "2020-03-01T01:00:00.000Z" || buckets[0].(map[string]any)["doc_count"].(float64) != 3 ||
		buckets[1].(map[string]any)["key_as_string"] != "2020-03-01T13:00:00.000Z" || buckets[1].(map[string]any)["doc_count"].(float64) != 1 {
		t.Fatalf("12h buckets = %v, want [01:00 doc_count 3, 13:00 doc_count 1]", buckets)
	}

	// sub-aggregation and an avg_bucket sibling pipeline over its buckets
	res = mustDo(t, c, http.MethodPost, "/auto-date-compat/_search",
		`{"size":0,"aggs":{"histo":{"auto_date_histogram":{"field":"date","buckets":2},"aggs":{"v":{"sum":{"field":"v"}}}},"histo_avg_v":{"avg_bucket":{"buckets_path":"histo.v"}}}}`)
	aggs := res["aggregations"].(map[string]any)
	hb := aggs["histo"].(map[string]any)["buckets"].([]any)
	if hb[0].(map[string]any)["v"].(map[string]any)["value"].(float64) != 3 || hb[1].(map[string]any)["v"].(map[string]any)["value"].(float64) != 7 {
		t.Fatalf("histo sub-aggregation sums = %v, want 3 and 7", hb)
	}
	if aggs["histo_avg_v"].(map[string]any)["value"].(float64) != 5 {
		t.Fatalf("histo_avg_v = %v, want 5", aggs["histo_avg_v"])
	}

	// buckets must be a positive integer
	code, errRes := status(t, c, http.MethodPost, "/auto-date-compat/_search", `{"size":0,"aggs":{"histo":{"auto_date_histogram":{"field":"date","buckets":0}}}}`)
	if code != http.StatusBadRequest || errType(errRes) != "x_content_parse_exception" {
		t.Fatalf("buckets=0 status/type = %d/%s, want 400/x_content_parse_exception", code, errType(errRes))
	}
}

func TestVariableWidthHistogramAggregationCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/vwh-compat", `{"settings":{"number_of_replicas":0},"mappings":{"properties":{"number":{"type":"integer"}}}}`)
	if err := c.BulkString(`{"index":{"_index":"vwh-compat","_id":"1"}}
{"number":-3}
{"index":{"_index":"vwh-compat","_id":"2"}}
{"number":-2}
{"index":{"_index":"vwh-compat","_id":"3"}}
{"number":1}
{"index":{"_index":"vwh-compat","_id":"4"}}
{"number":4}
{"index":{"_index":"vwh-compat","_id":"5"}}
{"number":5}
`); err != nil {
		t.Fatal(err)
	}
	// OpenSearch's own documented worked example for this aggregation
	res := mustDo(t, c, http.MethodPost, "/vwh-compat/_search", `{"size":0,"aggs":{"histo":{"variable_width_histogram":{"field":"number","buckets":3}}}}`)
	buckets := res["aggregations"].(map[string]any)["histo"].(map[string]any)["buckets"].([]any)
	if len(buckets) != 3 {
		t.Fatalf("buckets = %v, want 3", buckets)
	}
	want := []struct {
		key, min, max, count float64
	}{
		{-2.5, -3, -2, 2},
		{1.0, 1, 1, 1},
		{4.5, 4, 5, 2},
	}
	for i, w := range want {
		b := buckets[i].(map[string]any)
		if b["key"].(float64) != w.key || b["min"].(float64) != w.min || b["max"].(float64) != w.max || b["doc_count"].(float64) != w.count {
			t.Fatalf("bucket %d = %v, want key=%v min=%v max=%v doc_count=%v", i, b, w.key, w.min, w.max, w.count)
		}
	}
}

// TestTermsUnsignedLongTypedKeysCompatibility covers the typed_keys fix: an
// unsigned_long field's terms aggregation must use the ulterms#/
// UnsignedLongTerms class, not the plain long lterms# one.
func TestTermsUnsignedLongTypedKeysCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/ulterms-compat", `{"settings":{"number_of_replicas":0},"mappings":{"properties":{"name":{"type":"keyword"},"num":{"type":"unsigned_long"}}}}`)
	if err := c.BulkString(`{"index":{"_index":"ulterms-compat","_id":"1"}}
{"name":"one","num":1}
{"index":{"_index":"ulterms-compat","_id":"2"}}
{"name":"two","num":2}
`); err != nil {
		t.Fatal(err)
	}
	res := mustDo(t, c, http.MethodPost, "/ulterms-compat/_search?typed_keys=true", `{"size":0,"aggs":{"test_terms":{"terms":{"field":"num"}}}}`)
	if _, ok := res["aggregations"].(map[string]any)["ulterms#test_terms"]; !ok {
		t.Fatalf("unsigned_long terms typed key = %v, want ulterms#test_terms", res["aggregations"])
	}
}
