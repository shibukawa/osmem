package osmem

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// The expectations of this file were recorded from OpenSearch 3.8.0.

func seedAuthors(t *testing.T, c *Cluster, index, settings string) {
	t.Helper()
	mustDo(t, c, http.MethodPut, "/"+index, `{`+settings+`"mappings":{"properties":{"author":{"type":"keyword"},"n":{"type":"integer"}}}}`)
	var sb strings.Builder
	for i, author := range []string{"alice", "bob", "alice", "carol", "dave"} {
		fmt.Fprintf(&sb, "{\"index\":{\"_index\":%q,\"_id\":\"%d\"}}\n{\"author\":%q,\"n\":%d}\n", index, i+1, author, i+1)
	}
	if err := c.BulkString(sb.String()); err != nil {
		t.Fatal(err)
	}
}

func hitIDsOf(res map[string]any) []string {
	var ids []string
	hits, _ := res["hits"].(map[string]any)
	list, _ := hits["hits"].([]any)
	for _, h := range list {
		ids = append(ids, h.(map[string]any)["_id"].(string))
	}
	return ids
}

func hitScoresOf(res map[string]any) []any {
	var scores []any
	for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
		scores = append(scores, h.(map[string]any)["_score"])
	}
	return scores
}

func sortedStrings(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func TestTerminateAfterCollectsEachShardInIndexOrder(t *testing.T) {
	c := New()
	defer c.Close()
	seedAuthors(t, c, "t", "")
	search := func(body string) map[string]any { return mustDo(t, c, http.MethodPost, "/t/_search", body) }

	// a size 0 match_all request counts every document
	res := search(`{"terminate_after":1,"size":0}`)
	assertJSON(t, res["hits"].(map[string]any)["total"], `{"value":5,"relation":"eq"}`)
	if res["terminated_early"] != true {
		t.Fatalf("terminated_early: %v", res)
	}
	res = search(`{"terminate_after":1}`)
	assertIDs(t, hitIDsOf(res), "1")
	assertJSON(t, res["hits"].(map[string]any)["total"], `{"value":1,"relation":"eq"}`)
	// the first documents in index order are sorted afterwards
	assertIDs(t, hitIDsOf(search(`{"terminate_after":2,"sort":[{"n":"desc"}]}`)), "2", "1")
	res = search(`{"terminate_after":0}`)
	if _, present := res["terminated_early"]; present || len(hitIDsOf(res)) != 5 {
		t.Fatalf("terminate_after 0 is the default: %v", res)
	}
	res = search(`{"terminate_after":3,"query":{"term":{"author":"alice"}}}`)
	if res["terminated_early"] != false || strings.Join(sortedStrings(hitIDsOf(res)), ",") != "1,3" {
		t.Fatalf("fewer matches than terminate_after: %v", res)
	}
	if search(`{"terminate_after":5,"size":0}`)["terminated_early"] != false || search(`{"terminate_after":4,"size":0}`)["terminated_early"] != true {
		t.Fatalf("terminated_early reports whether a shard had more documents")
	}
	// aggregations see the collected documents
	if got := jsonAt(t, search(`{"terminate_after":2,"size":0,"aggs":{"s":{"sum":{"field":"n"}}}}`), "/aggregations/s/value"); got != 3.0 {
		t.Fatalf("sum of the collected documents = %v", got)
	}
	// post_filter rejects documents before they are counted
	res = search(`{"terminate_after":2,"post_filter":{"term":{"author":"bob"}}}`)
	if res["terminated_early"] != false || strings.Join(hitIDsOf(res), ",") != "2" {
		t.Fatalf("terminate_after after post_filter: %v", res)
	}
	res = search(`{"terminate_after":-1}`)
	if res["terminated_early"] != true || len(hitIDsOf(res)) != 0 {
		t.Fatalf("a negative body value terminates at the first document: %v", res)
	}
	expectRequestError(t, c, http.MethodPost, "/t/_search", `{"terminate_after":"abc"}`, 400, "number_format_exception", `For input string: "abc"`)
	expectRequestError(t, c, http.MethodPost, "/t/_search?terminate_after=-1", nil, 400, "illegal_argument_exception", "terminateAfter must be > 0")
	expectRequestError(t, c, http.MethodPost, "/t/_search?terminate_after=abc", nil, 400, "illegal_argument_exception", "Failed to parse int parameter [terminate_after] with value [abc]")
	count := mustDo(t, c, http.MethodPost, "/t/_count?terminate_after=2", nil)
	if count["count"] != 5.0 || count["terminated_early"] != true {
		t.Fatalf("_count with terminate_after: %v", count)
	}
	// an update moves the document to the end of the index
	mustDo(t, c, http.MethodPost, "/t/_update/1", `{"doc":{"n":10}}`)
	assertIDs(t, hitIDsOf(search(`{"terminate_after":1}`)), "2")
}

func TestQueryRescorer(t *testing.T) {
	c := New()
	defer c.Close()
	seedAuthors(t, c, "r", "")
	cs := func(author, boost string) string {
		return `{"constant_score":{"filter":{"term":{"author":"` + author + `"}},"boost":` + boost + `}}`
	}
	search := func(body string) map[string]any { return mustDo(t, c, http.MethodPost, "/r/_search", body) }
	res := search(`{"query":{"match_all":{}},"size":2,"rescore":{"window_size":10,"query":{"rescore_query":` + cs("dave", "5") + `}}}`)
	assertIDs(t, hitIDsOf(res), "5", "1")
	assertJSON(t, hitScoresOf(res), `[6.0,1.0]`)
	if res["hits"].(map[string]any)["max_score"] != 6.0 {
		t.Fatalf("max_score after rescoring: %v", res)
	}
	// rescorers apply in order, each on its own window
	res = search(`{"query":{"match_all":{}},"rescore":[{"window_size":3,"query":{"rescore_query":` + cs("dave", "5") + `}},{"window_size":2,"query":{"rescore_query":` + cs("bob", "7") + `,"score_mode":"max"}}]}`)
	assertIDs(t, hitIDsOf(res), "2", "1", "3", "4", "5")
	assertJSON(t, hitScoresOf(res), `[7.0,1.0,1.0,1.0,1.0]`)
	// hits outside the window are scaled by query_weight
	assertJSON(t, hitScoresOf(search(`{"query":{"match_all":{}},"rescore":{"window_size":1,"query":{"rescore_query":`+cs("dave", "5")+`,"query_weight":3}}}`)), `[3.0,3.0,3.0,3.0,3.0]`)
	res = search(`{"query":{"match_all":{}},"min_score":0.5,"rescore":{"query":{"rescore_query":` + cs("dave", "5") + `,"query_weight":0}}}`)
	assertIDs(t, hitIDsOf(res), "5", "1", "2", "3", "4")
	assertJSON(t, hitScoresOf(res), `[5.0,0.0,0.0,0.0,0.0]`)
	for mode, want := range map[string]string{"multiply": "5.0", "avg": "3.0", "max": "5.0", "total": "6.0"} {
		res = search(`{"query":{"match_all":{}},"size":1,"rescore":{"query":{"rescore_query":` + cs("carol", "5") + `,"score_mode":"` + mode + `"}}}`)
		assertIDs(t, hitIDsOf(res), "4")
		assertJSON(t, hitScoresOf(res), `[`+want+`]`)
	}
	res = search(`{"query":{"match_all":{}},"rescore":{"query":{"rescore_query":` + cs("carol", "0.1") + `,"score_mode":"min"}}}`)
	if ids := hitIDsOf(res); ids[len(ids)-1] != "4" {
		t.Fatalf("min score mode: %v", res)
	}
	mustDo(t, c, http.MethodPost, "/r/_search", `{"query":{"match_all":{}},"sort":["_score"],"rescore":{"query":{"rescore_query":`+cs("dave", "5")+`}}}`)

	expectRequestError(t, c, http.MethodPost, "/r/_search", `{"rescore":{"query":{}}}`, 400, "illegal_argument_exception", "rescore_query cannot be null")
	res = expectRequestError(t, c, http.MethodPost, "/r/_search", `{"rescore":{"query":{"rescore_query":{"match_all":{}},"score_mode":"foo"}}}`, 400, "x_content_parse_exception", "[1:68] [query] failed to parse field [score_mode]")
	if jsonAt(t, res, "/error/caused_by/reason") != "illegal score_mode [foo]" {
		t.Fatalf("score_mode cause: %v", res)
	}
	expectRequestError(t, c, http.MethodPost, "/r/_search", `{"rescore":"abc"}`, 400, "parsing_exception", "Unknown key for a VALUE_STRING in [rescore].")
	expectRequestError(t, c, http.MethodPost, "/r/_search", `{"rescore":{"window_size":3}}`, 400, "parsing_exception", "missing rescore type")
	expectRequestError(t, c, http.MethodPost, "/r/_search", `{"rescore":{"query":{"rescore_query":{"match_all":{}}},"extra":1}}`, 400, "parsing_exception", "rescore doesn't support [extra]")
	expectRequestError(t, c, http.MethodPost, "/r/_search", `{"sort":[{"n":"asc"}],"rescore":{"query":{"rescore_query":{"match_all":{}}}}}`, 400, "search_phase_execution_exception", "Cannot use [sort] option in conjunction with [rescore].")
	expectRequestError(t, c, http.MethodPost, "/r/_search", `{"rescore":{"window_size":-1,"query":{"rescore_query":{"match_all":{}}}}}`, 500, "search_phase_execution_exception", "-1")
	expectRequestError(t, c, http.MethodPost, "/r/_search", `{"rescore":{"window_size":10001,"query":{"rescore_query":{"match_all":{}}}}}`, 400, "search_phase_execution_exception",
		"Rescore window [10001] is too large. It must be less than [10000]. This prevents allocating massive heaps for storing the results to be rescored. This limit can be set by changing the [index.max_rescore_window] index level setting.")
	expectRequestError(t, c, http.MethodPost, "/r/_search", `{"collapse":{"field":"author"},"rescore":{"query":{"rescore_query":{"match_all":{}}}}}`, 500, "search_phase_execution_exception", "cannot use `collapse` in conjunction with `rescore`")
	expectRequestError(t, c, http.MethodPost, "/r/_search?scroll=1m", `{"rescore":{"query":{"rescore_query":{"match_all":{}}}}}`, 400, "action_request_validation_exception", "Validation Failed: 1: using [rescore] is not allowed in a scroll context;")
}

func TestIndicesBoost(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/ia", `{"mappings":{"properties":{"author":{"type":"keyword"}}}}`)
	mustDo(t, c, http.MethodPut, "/ib", `{"mappings":{"properties":{"author":{"type":"keyword"}}}}`)
	if err := c.BulkString("{\"index\":{\"_index\":\"ia\",\"_id\":\"a1\"}}\n{\"author\":\"x\"}\n{\"index\":{\"_index\":\"ia\",\"_id\":\"a2\"}}\n{\"author\":\"y\"}\n{\"index\":{\"_index\":\"ib\",\"_id\":\"b1\"}}\n{\"author\":\"x\"}\n"); err != nil {
		t.Fatal(err)
	}
	mustDo(t, c, http.MethodPut, "/ia/_alias/al", nil)
	search := func(body string) map[string]any { return mustDo(t, c, http.MethodPost, "/ia,ib/_search", body) }
	for _, body := range []string{`{"indices_boost":[{"ia":2.0},{"ib":3.0}]}`, `{"indices_boost":{"ia":2.0,"ib":3.0}}`} {
		res := search(body)
		assertIDs(t, hitIDsOf(res), "b1", "a1", "a2")
		assertJSON(t, hitScoresOf(res), `[3.0,2.0,2.0]`)
	}
	// the first entry naming an index wins
	assertJSON(t, hitScoresOf(search(`{"indices_boost":[{"i*":1.5},{"ib":3.0}]}`)), `[1.5,1.5,1.5]`)
	assertJSON(t, hitScoresOf(search(`{"indices_boost":[{"al":4}]}`)), `[4.0,4.0,1.0]`)
	res := search(`{"indices_boost":[{"ia":-1}]}`)
	assertIDs(t, hitIDsOf(res), "b1")
	assertIDs(t, hitIDsOf(search(`{"indices_boost":[{"ia":0}]}`)), "b1", "a1", "a2")
	assertIDs(t, hitIDsOf(search(`{"indices_boost":[{"nope*":5}]}`)), "a1", "a2", "b1")
	expectRequestError(t, c, http.MethodPost, "/ia,ib/_search", `{"indices_boost":[{"nope":5}]}`, 404, "index_not_found_exception", "no such index [nope]")
	expectRequestError(t, c, http.MethodPost, "/ia,ib/_search", `{"indices_boost":[{"ia":"abc"}]}`, 400, "parsing_exception", "Expected [VALUE_NUMBER] in [indices_boost] but found [VALUE_STRING]")
	expectRequestError(t, c, http.MethodPost, "/ia,ib/_search", `{"indices_boost":[{"ia":2,"ib":3}]}`, 400, "parsing_exception", "Expected [END_OBJECT] in [indices_boost] but found [FIELD_NAME]")
	expectRequestError(t, c, http.MethodPost, "/ia,ib/_search", `{"indices_boost":["ia"]}`, 400, "parsing_exception", "Expected [START_OBJECT] in [null] but found [VALUE_STRING]")
}

func seedSliceIDs(t *testing.T, c *Cluster, index string, shards int) {
	t.Helper()
	mustDo(t, c, http.MethodPut, "/"+index, fmt.Sprintf(`{"settings":{"number_of_shards":%d},"mappings":{"properties":{"n":{"type":"integer"},"author":{"type":"keyword"}}}}`, shards))
	var sb strings.Builder
	for i, id := range []string{"1", "2", "3", "4", "5", "10", "abc", "x-5", "AAAA", "QQ", "123456789", "0123", "Zm9v", "a_b"} {
		fmt.Fprintf(&sb, "{\"index\":{\"_index\":%q,\"_id\":%q}}\n{\"n\":%d}\n", index, id, i)
	}
	if err := c.BulkString(sb.String()); err != nil {
		t.Fatal(err)
	}
}

func TestSliceScrollAndPointInTime(t *testing.T) {
	c := New()
	defer c.Close()
	seedSliceIDs(t, c, "s", 1)
	seedSliceIDs(t, c, "m", 3)
	slice := func(index, spec string) (string, map[string]any) {
		res := mustDo(t, c, http.MethodPost, "/"+index+"/_search?scroll=1m", `{"size":100,"_source":false,"slice":`+spec+`}`)
		return strings.Join(sortedStrings(hitIDsOf(res)), ","), res
	}
	for spec, want := range map[string]string{
		`{"id":0,"max":2}`:             "0123,10,3,4,5,AAAA,abc",
		`{"id":1,"max":2}`:             "1,123456789,2,QQ,Zm9v,a_b,x-5",
		`{"id":2,"max":3}`:             "Zm9v,a_b",
		`{"id":0,"max":2,"field":"n"}`: "1,10,123456789,4,5,QQ,Zm9v,a_b,abc,x-5",
	} {
		if got, _ := slice("s", spec); got != want {
			t.Fatalf("slice %s = %s, want %s", spec, got, want)
		}
	}
	// fewer slices than shards: whole shards; more: a terms slice within a shard
	got, res := slice("m", `{"id":0,"max":2}`)
	if got != "0123,1,123456789,5,AAAA,QQ,Zm9v,abc" || jsonAt(t, res, "/_shards/total") != 2.0 {
		t.Fatalf("slice 0/2 over 3 shards = %s %v", got, res["_shards"])
	}
	if got, _ = slice("m", `{"id":1,"max":3}`); got != "10,2,3,4,a_b,x-5" {
		t.Fatalf("slice 1/3 over 3 shards = %s", got)
	}
	if got, _ = slice("m", `{"id":3,"max":5}`); got != "Zm9v" {
		t.Fatalf("slice 3/5 over 3 shards = %s", got)
	}
	pit := mustDo(t, c, http.MethodPost, "/s/_search/point_in_time?keep_alive=1m", nil)["pit_id"].(string)
	res = mustDo(t, c, http.MethodPost, "/_search", `{"pit":{"id":"`+pit+`"},"size":100,"_source":false,"slice":{"id":0,"max":2}}`)
	if got := strings.Join(sortedStrings(hitIDsOf(res)), ","); got != "0123,10,3,4,5,AAAA,abc" {
		t.Fatalf("PIT slice = %s", got)
	}
	expectRequestError(t, c, http.MethodPost, "/s/_search", `{"slice":{"id":0,"max":2}}`, 500, "search_phase_execution_exception", "`slice` cannot be used outside of a scroll context or PIT context")
	res = expectRequestError(t, c, http.MethodPost, "/s/_search?scroll=1m", `{"slice":{"id":2,"max":2}}`, 400, "x_content_parse_exception", "[1:24] [slice] failed to parse field [max]")
	if jsonAt(t, res, "/error/caused_by/reason") != "max must be greater than id" {
		t.Fatalf("slice cause: %v", res)
	}
	expectRequestError(t, c, http.MethodPost, "/s/_search?scroll=1m", `{"slice":{"id":0,"max":2,"field":"nope"}}`, 400, "search_phase_execution_exception", "field nope not found")
	expectRequestError(t, c, http.MethodPost, "/s/_search?scroll=1m", `{"slice":{"id":0,"max":2,"field":"author"}}`, 400, "search_phase_execution_exception", "cannot load numeric doc values on author")
	expectRequestError(t, c, http.MethodPost, "/s/_search?scroll=1m", `{"slice":{"id":0,"max":1025}}`, 400, "search_phase_execution_exception",
		"The number of slices [1025] is too large. It must be less than [1024]. This limit can be set by changing the [index.max_slices_per_scroll] index level setting.")
}

func TestMaxResultWindowPerIndex(t *testing.T) {
	c := New()
	defer c.Close()
	seedAuthors(t, c, "w", `"settings":{"index.max_result_window":2},`)
	seedAuthors(t, c, "v", "")
	expectRequestError(t, c, http.MethodPost, "/w/_search", `{"size":3}`, 400, "search_phase_execution_exception",
		"Result window is too large, from + size must be less than or equal to: [2] but was [3]. See the scroll api for a more efficient way to request large data sets. This limit can be set by changing the [index.max_result_window] index level setting.")
	// the other index still answers: a partial result with a shard failure
	res := mustDo(t, c, http.MethodPost, "/w,v/_search", `{"size":3,"sort":["n"],"_source":false}`)
	shards := res["_shards"].(map[string]any)
	if shards["total"] != 2.0 || shards["successful"] != 1.0 || shards["failed"] != 1.0 || jsonAt(t, res, "/_shards/failures/0/index") != "w" {
		t.Fatalf("partial failure: %v", shards)
	}
	for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
		if h.(map[string]any)["_index"] != "v" {
			t.Fatalf("hits of the failed index: %v", res)
		}
	}
	expectRequestError(t, c, http.MethodPost, "/w/_search?scroll=1m", `{"size":3}`, 400, "search_phase_execution_exception",
		"Batch size is too large, size must be less than or equal to: [2] but was [3]. Scroll batch sizes cost as much memory as result windows so they are controlled by the [index.max_result_window] index level setting.")
	mustDo(t, c, http.MethodPut, "/w/_settings", `{"index.max_result_window":20000}`)
	mustDo(t, c, http.MethodPost, "/w/_search", `{"from":15000,"size":1}`)
	res = expectRequestError(t, c, http.MethodPost, "/w,v/_search", `{"size":25000}`, 400, "search_phase_execution_exception", "")
	if len(res["error"].(map[string]any)["failed_shards"].([]any)) != 2 || jsonAt(t, res, "/error/failed_shards/0/index") != "v" {
		t.Fatalf("every index reports its own failure: %v", res)
	}
}

func TestSearchAfterValidation(t *testing.T) {
	c := New()
	defer c.Close()
	seedAuthors(t, c, "sa", "")
	expectRequestError(t, c, http.MethodPost, "/sa/_search", `{"search_after":[1]}`, 400, "search_phase_execution_exception", "Sort must contain at least one field.")
	expectRequestError(t, c, http.MethodPost, "/sa/_search", `{"sort":[{"n":"asc"}],"search_after":[]}`, 400, "illegal_argument_exception", "Values must contains at least one value.")
	expectRequestError(t, c, http.MethodPost, "/sa/_search", `{"sort":[{"n":"asc"}],"search_after":"1"}`, 400, "parsing_exception", "Unknown key for a VALUE_STRING in [search_after].")
	res := expectRequestError(t, c, http.MethodPost, "/sa/_search", `{"sort":[{"n":"asc"}],"search_after":[true]}`, 400, "search_phase_execution_exception", "Failed to parse search_after value for field [n].")
	if jsonAt(t, res, "/error/failed_shards/0/reason/caused_by/reason") != `For input string: "true"` {
		t.Fatalf("number format cause: %v", res)
	}
	expectRequestError(t, c, http.MethodPost, "/sa/_search", `{"sort":[{"n":"asc"}],"search_after":[null]}`, 500, "search_phase_execution_exception", `Cannot invoke "java.lang.Integer.intValue()" because "value" is null`)
	mustDo(t, c, http.MethodPost, "/sa/_search", `{"sort":[{"author":"asc"}],"search_after":[null]}`)
	expectRequestError(t, c, http.MethodPost, "/sa/_search", `{"sort":[{"n":"asc"}],"search_after":[1],"from":1}`, 500, "search_phase_execution_exception", "`from` parameter must be set to 0 when `search_after` is used.")
	expectRequestError(t, c, http.MethodPost, "/sa/_search?scroll=1m", `{"sort":[{"n":"asc"}],"search_after":[1]}`, 500, "search_phase_execution_exception", "`search_after` cannot be used in a scroll context.")
}

func TestCollapseValidation(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/cl", `{"mappings":{"properties":{"author":{"type":"keyword"},"tags":{"type":"keyword"},"b":{"type":"boolean"},"d":{"type":"date"},"t":{"type":"text"},"n":{"type":"integer"}}}}`)
	if err := c.BulkString(`{"index":{"_index":"cl","_id":"1"}}
{"author":"alice","tags":["x","y"],"b":true,"d":"2001-01-01","t":"a","n":1}
{"index":{"_index":"cl","_id":"2"}}
{"author":"bob","tags":"z","b":false,"n":2}
{"index":{"_index":"cl","_id":"3"}}
{"author":"alice","n":3}
`); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"b", "d", "t"} {
		expectRequestError(t, c, http.MethodPost, "/cl/_search", `{"collapse":{"field":"`+field+`"}}`, 400, "search_phase_execution_exception",
			"unknown type for collapse field `"+field+"`, only keywords and numbers are accepted")
	}
	expectRequestError(t, c, http.MethodPost, "/cl/_search", `{"collapse":{"field":"tags"},"sort":["_id"]}`, 500, "search_phase_execution_exception", "failed to collapse 0, the collapse field must be single valued")
	expectRequestError(t, c, http.MethodPost, "/cl/_search", `{"collapse":{"field":"nope"}}`, 400, "search_phase_execution_exception", "no mapping found for `nope` in order to collapse on")
	expectRequestError(t, c, http.MethodPost, "/cl/_search", `{"collapse":{"field":"author","foo":1}}`, 400, "x_content_parse_exception", "[1:31] [collapse] unknown field [foo]")
	res := expectRequestError(t, c, http.MethodPost, "/cl/_search", `{"collapse":{"field":"author","max_concurrent_group_searches":0}}`, 400, "x_content_parse_exception", "[1:63] [collapse] failed to parse field [max_concurrent_group_searches]")
	if jsonAt(t, res, "/error/caused_by/reason") != "maxConcurrentGroupRequests` must be positive" {
		t.Fatalf("max_concurrent_group_searches cause: %v", res)
	}
	expectRequestError(t, c, http.MethodPost, "/cl/_search", `{"collapse":{}}`, 500, "search_phase_execution_exception", `Cannot invoke "Object.hashCode()" because "key" is null`)
	expectRequestError(t, c, http.MethodPost, "/cl/_search?scroll=1m", `{"collapse":{"field":"author"}}`, 500, "search_phase_execution_exception", "cannot use `collapse` in a scroll context")
	// OpenSearch 3.8 accepts search_after when the sort is the collapse field
	ids, _ := mustDoIDs(t, c, "/cl/_search", `{"collapse":{"field":"author"},"sort":[{"author":"asc"}],"search_after":["alice"],"_source":false}`)
	assertIDs(t, ids, "2")
	expectRequestError(t, c, http.MethodPost, "/cl/_search", `{"collapse":{"field":"author"},"sort":[{"n":"asc"}],"search_after":[1]}`, 500, "search_phase_execution_exception",
		"collapse field and sort field must be the same when use `collapse` in conjunction with `search_after`")
}
