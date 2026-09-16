package osmem

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The expectations of this file were recorded from OpenSearch 3.8.0.

func TestScrollValidationAndContinuation(t *testing.T) {
	c := New()
	defer c.Close()
	seedAuthors(t, c, "sc", "")
	validation := func(path, body, reason string) {
		t.Helper()
		expectRequestError(t, c, http.MethodPost, path, body, 400, "action_request_validation_exception", reason)
	}
	validation("/sc/_search?scroll=1m", `{"from":1,"size":1}`, "Validation Failed: 1: using [from] is not allowed in a scroll context;")
	validation("/sc/_search?scroll=1m", `{"size":0,"from":1,"rescore":{"query":{"rescore_query":{"match_all":{}}}}}`,
		"Validation Failed: 1: using [from] is not allowed in a scroll context;2: [size] cannot be [0] in a scroll context;3: using [rescore] is not allowed in a scroll context;")
	validation("/sc/_search?scroll=1m&request_cache=true", `{"size":1,"track_total_hits":false}`,
		"Validation Failed: 1: disabling [track_total_hits] is not allowed in a scroll context;2: [request_cache] cannot be used in a scroll context;")
	validation("/sc/_search?scroll=1m", `{"size":1,"track_total_hits":10000}`, "Validation Failed: 1: disabling [track_total_hits] is not allowed in a scroll context;")
	expectRequestError(t, c, http.MethodPost, "/sc/_search?scroll=2d", `{"size":1}`, 400, "search_phase_execution_exception",
		"Keep alive for request (2d) is too large. It must be less than (1d). This limit can be set by changing the [search.max_keep_alive] cluster level setting.")
	expectRequestError(t, c, http.MethodPost, "/sc/_search?scroll=25h", `{"size":1}`, 400, "search_phase_execution_exception",
		"Keep alive for request (1d) is too large. It must be less than (1d). This limit can be set by changing the [search.max_keep_alive] cluster level setting.")
	expectRequestError(t, c, http.MethodPost, "/sc/_search?scroll=abc", `{"size":1}`, 400, "illegal_argument_exception", "failed to parse setting [scroll] with value [abc] as a time value: unit is missing or unrecognized")
	expectRequestError(t, c, http.MethodPost, "/sc/_search?scroll=1m", `{"size":10001}`, 400, "search_phase_execution_exception",
		"Batch size is too large, size must be less than or equal to: [10000] but was [10001]. Scroll batch sizes cost as much memory as result windows so they are controlled by the [index.max_result_window] index level setting.")

	first := mustDo(t, c, http.MethodPost, "/sc/_search?scroll=1m&rest_total_hits_as_int=true", `{"size":2,"sort":["n"],"_source":false}`)
	id := first["_scroll_id"].(string)
	if !strings.HasPrefix(id, "FGluY2x1ZGVfY29udGV4dF91dWlk") || jsonAt(t, first, "/hits/total") != 5.0 {
		t.Fatalf("scroll id format and int total: %v", first)
	}
	// rest_total_hits_as_int applies per request; no scroll keep-alive, no id
	next := mustDo(t, c, http.MethodPost, "/_search/scroll", `{"scroll_id":"`+id+`"}`)
	if _, present := next["_scroll_id"]; present {
		t.Fatalf("continuation without scroll returns no scroll id: %v", next)
	}
	assertIDs(t, hitIDsOf(next), "3", "4")
	assertJSON(t, next["hits"].(map[string]any)["total"], `{"value":5,"relation":"eq"}`)
	next = mustDo(t, c, http.MethodPost, "/_search/scroll", `{"scroll_id":"`+id+`","scroll":"1m"}`)
	if next["_scroll_id"] != id {
		t.Fatalf("continuation with scroll returns the id: %v", next)
	}
	assertIDs(t, hitIDsOf(next), "5")

	expectRequestError(t, c, http.MethodPost, "/_search/scroll", `{"scroll_id":"`+id+`","scroll":60}`, 400, "illegal_argument_exception", "Unknown parameter [scroll] in request body or parameter is of the wrong type[VALUE_NUMBER] ")
	expectRequestError(t, c, http.MethodPost, "/_search/scroll", `{"scroll_id":["abc"]}`, 400, "illegal_argument_exception", "Unknown parameter [scroll_id] in request body or parameter is of the wrong type[START_ARRAY] ")
	expectRequestError(t, c, http.MethodPost, "/_search/scroll", `{}`, 400, "action_request_validation_exception", "Validation Failed: 1: scrollId is missing;")
	res := expectRequestError(t, c, http.MethodPost, "/_search/scroll", `{"scroll_id":"abc"}`, 400, "illegal_argument_exception", "Cannot parse scroll id")
	if jsonAt(t, res, "/error/caused_by/reason") != "attempting to read 105 bytes but only 1 bytes are available" {
		t.Fatalf("scroll id parse cause: %v", res)
	}
	expectRequestError(t, c, http.MethodPost, "/_search/scroll", `{"scroll_id":"abc!"}`, 400, "illegal_argument_exception", "Cannot parse scroll id")

	code, cleared := status(t, c, http.MethodDelete, "/_search/scroll", `{"scroll_id":["`+id+`"]}`)
	if code != 200 {
		t.Fatalf("clear scroll = %d %v", code, cleared)
	}
	assertJSON(t, cleared, `{"succeeded":true,"num_freed":1}`)
	code, cleared = status(t, c, http.MethodDelete, "/_search/scroll", `{"scroll_id":"`+id+`"}`)
	if code != 404 {
		t.Fatalf("clearing a freed scroll = %d %v", code, cleared)
	}
	assertJSON(t, cleared, `{"succeeded":true,"num_freed":0}`)
	expectRequestError(t, c, http.MethodDelete, "/_search/scroll", `{}`, 400, "action_request_validation_exception", "Validation Failed: 1: no scroll ids specified;")
	expectRequestError(t, c, http.MethodDelete, "/_search/scroll/abc", nil, 400, "illegal_argument_exception", "Cannot parse scroll id")
	res = expectRequestError(t, c, http.MethodPost, "/_search/scroll", `{"scroll_id":"`+id+`"}`, 404, "search_phase_execution_exception", "")
	if jsonAt(t, res, "/error/failed_shards/0/shard") != -1.0 || jsonAt(t, res, "/error/root_cause/0/type") != "search_context_missing_exception" {
		t.Fatalf("freed scroll: %v", res)
	}
}

func TestPointInTimeValidationAndShardDoc(t *testing.T) {
	c := New()
	defer c.Close()
	seedAuthors(t, c, "p", "")
	seedAuthors(t, c, "q", `"settings":{"number_of_shards":2},`)
	expectRequestError(t, c, http.MethodPost, "/p/_search/point_in_time", nil, 400, "action_request_validation_exception", "Validation Failed: 1: keep alive not specified;")
	expectRequestError(t, c, http.MethodPost, "/p/_search/point_in_time?keep_alive=abc", nil, 400, "illegal_argument_exception", "failed to parse setting [keep_alive] with value [abc] as a time value: unit is missing or unrecognized")
	expectRequestError(t, c, http.MethodPost, "/p/_search/point_in_time?keep_alive=36h", nil, 400, "illegal_argument_exception",
		"Keep alive for request (1.5d) is too large. It must be less than (1d). This limit can be set by changing the [point_in_time.max_keep_alive] cluster level setting.")
	created := mustDo(t, c, http.MethodPost, "/p,q/_search/point_in_time?keep_alive=10m", nil)
	id := created["pit_id"].(string)
	if jsonAt(t, created, "/_shards/total") != 3.0 {
		t.Fatalf("PIT shards: %v", created)
	}
	listed := mustDo(t, c, http.MethodGet, "/_search/point_in_time/_all", nil)
	if jsonAt(t, listed, "/pits/0/keep_alive") != 600000.0 || jsonAt(t, listed, "/pits/0/creation_time") != created["creation_time"] {
		t.Fatalf("PIT list: %v vs %v", listed, created)
	}
	res := mustDo(t, c, http.MethodPost, "/_search", `{"pit":{"id":"`+id+`"},"size":20,"sort":[{"_shard_doc":"asc"}],"_source":false}`)
	var got []string
	for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
		hm := h.(map[string]any)
		got = append(got, hm["_index"].(string)+"/"+hm["_id"].(string)+"="+stringNumber(hm["sort"].([]any)[0]))
	}
	want := "p/1=0 q/1=0 p/2=1 q/2=1 p/3=2 q/3=2 p/4=3 q/5=3 p/5=4 q/4=4294967296"
	if strings.Join(got, " ") != want {
		t.Fatalf("_shard_doc values: %s, want %s", strings.Join(got, " "), want)
	}
	res = mustDo(t, c, http.MethodPost, "/_search", `{"pit":{"id":"`+id+`"},"size":3,"sort":[{"n":"asc"},{"_shard_doc":"asc"}],"search_after":[1,0],"_source":false}`)
	if ids := hitIDsOf(res); strings.Join(ids, ",") != "2,2,3" {
		t.Fatalf("search_after with _shard_doc: %v", res)
	}
	expectRequestError(t, c, http.MethodPost, "/p/_search", `{"pit":{"id":"`+id+`"}}`, 400, "action_request_validation_exception", "Validation Failed: 1: [indices] cannot be used with point in time;")
	expectRequestError(t, c, http.MethodPost, "/_search?preference=_local&routing=1&expand_wildcards=open", `{"pit":{"id":"`+id+`"}}`, 400, "action_request_validation_exception",
		"Validation Failed: 1: [indicesOptions] cannot be used with point in time;2: [routing] cannot be used with point in time;3: [preference] cannot be used with point in time;")
	expectRequestError(t, c, http.MethodPost, "/_search?scroll=1m", `{"pit":{"id":"`+id+`"}}`, 400, "action_request_validation_exception", "Validation Failed: 1: using [point in time] is not allowed in a scroll context;")
	expectRequestError(t, c, http.MethodPost, "/_search", `{"pit":{"foo":1}}`, 400, "x_content_parse_exception", "[1:9] [pit] unknown field [foo]")
	expectRequestError(t, c, http.MethodPost, "/_search", `{"pit":"abc"}`, 400, "parsing_exception", "Unknown key for a VALUE_STRING in [pit].")
	expectRequestError(t, c, http.MethodPost, "/_search", `{"pit":{"id":"abc"}}`, 500, "unsupported_version_exception", "Unsupported version [ES 0.0.1]")
	expectRequestError(t, c, http.MethodPost, "/p/_search", `{"sort":["_shard_doc"]}`, 400, "action_request_validation_exception",
		"Validation Failed: 1: _shard_doc is only supported with point-in-time (PIT). Add a PIT or remove _shard_doc.;")
	expectRequestError(t, c, http.MethodDelete, "/_search/point_in_time", `{}`, 400, "action_request_validation_exception", "Validation Failed: 1: no pit ids specified;")

	// closing an index frees its reader contexts
	closing := mustDo(t, c, http.MethodPost, "/p,q/_search/point_in_time?keep_alive=1m", nil)["pit_id"].(string)
	mustDo(t, c, http.MethodPost, "/p/_close", nil)
	expectRequestError(t, c, http.MethodPost, "/_search", `{"pit":{"id":"`+closing+`"}}`, 400, "index_closed_exception", "closed")
	mustDo(t, c, http.MethodPost, "/p/_open", nil)
	res = mustDo(t, c, http.MethodPost, "/_search", `{"pit":{"id":"`+closing+`"},"_source":false}`)
	if jsonAt(t, res, "/_shards/failed") != 1.0 || jsonAt(t, res, "/hits/total/value") != 5.0 {
		t.Fatalf("PIT after the index was reopened: %v", res)
	}

	for i := 0; i < 2; i++ {
		deleted := mustDo(t, c, http.MethodDelete, "/_search/point_in_time", `{"pit_id":["`+id+`"]}`)
		if jsonAt(t, deleted, "/pits/0/successful") != true {
			t.Fatalf("delete PIT: %v", deleted)
		}
	}
	res = expectRequestError(t, c, http.MethodPost, "/_search", `{"pit":{"id":"`+id+`"}}`, 404, "search_phase_execution_exception", "")
	if jsonAt(t, res, "/error/root_cause/0/type") != "search_context_missing_exception" || jsonAt(t, res, "/error/failed_shards/0/index") != "p" {
		t.Fatalf("deleted PIT: %v", res)
	}
}

func stringNumber(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return "?"
}

func TestMultiSearchFramingAndValidation(t *testing.T) {
	c := New()
	defer c.Close()
	seedAuthors(t, c, "m", "")
	msearch := func(path, body string, want int, errorType, rootReason string) map[string]any {
		t.Helper()
		if want != 200 {
			return expectRequestError(t, c, http.MethodPost, path, body, want, errorType, rootReason)
		}
		return expectStatus(t, c, http.MethodPost, path, body, want)
	}
	msearch("/_msearch", "{\"index\":\"m\"}\n{\"size\":0}", 400, "illegal_argument_exception", "The msearch request must be terminated by a newline [\n]")
	msearch("/_msearch", "", 400, "parse_exception", "request body or source parameter is required")
	msearch("/_msearch", "\n", 400, "action_request_validation_exception", "Validation Failed: 1: no requests added;")
	msearch("/m/_msearch", "{}\n\n{\"size\":0}\n", 400, "parsing_exception", "Expected [START_OBJECT] but found [null]")
	if res := msearch("/m/_msearch", "{}\n{\"size\":0}\n\n{\"size\":0}\n", 200, "", ""); len(res["responses"].([]any)) != 2 {
		t.Fatalf("an empty header line uses the defaults: %v", res)
	}
	if res := msearch("/_msearch", "{\"index\":\"m\"}\n{\"size\":0}\n{\"index\":\"m\"}\n", 200, "", ""); len(res["responses"].([]any)) != 1 {
		t.Fatalf("a trailing header without body is ignored: %v", res)
	}
	msearch("/_msearch", "{\"index\":\"m\",\"foo\":\"bar\"}\n{}\n", 400, "illegal_argument_exception", "key [foo] is not supported in the metadata section")
	msearch("/_msearch", "{\"index\":\"m\",\"search_type\":\"foo\"}\n{}\n", 400, "illegal_argument_exception", "No search type for [foo]")
	msearch("/_msearch", "{\"index\":\"m\",\"ignore_unavailable\":\"maybe\"}\n{}\n", 400, "illegal_argument_exception", "Could not convert [ignore_unavailable] to boolean")
	msearch("/_msearch", "{\"index\":\"m\"}\n{\"size\":-1}\n{\"index\":\"m\"}\n{\"size\":0}\n", 400, "illegal_argument_exception", "[size] parameter cannot be negative, found [-1]")
	msearch("/_msearch", "{\"index\":\"m\"}\n{\"query\":{\"foo\":{}}}\n{\"index\":\"m\"}\n{\"size\":0}\n", 400, "parsing_exception", "")
	msearch("/_msearch", "{\"index\":\"m\"}\n{\"size\":0,\"pit\":{\"id\":\"abc\"}}\n", 400, "action_request_validation_exception", "Validation Failed: 1: [indices] cannot be used with point in time;")
	msearch("/_msearch?max_concurrent_searches=0", "{\"index\":\"m\"}\n{\"size\":0}\n", 400, "illegal_argument_exception", "maxConcurrentSearchRequests must be positive")
	res := msearch("/_msearch", "{\"index\":\"missing\"}\n{}\n{\"indices\":[\"m\"]}\n{\"size\":0}\n", 200, "", "")
	if jsonAt(t, res, "/responses/0/status") != 404.0 || jsonAt(t, res, "/responses/0/error/type") != "index_not_found_exception" || jsonAt(t, res, "/responses/1/hits/total/value") != 5.0 {
		t.Fatalf("execution errors are reported per search: %v", res)
	}
}
