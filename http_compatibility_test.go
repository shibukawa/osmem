package osmem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// HTTP-layer behavior compared with OpenSearch 3.8: URL parameter
// validation, Content-Type handling, JSON strictness, filter_path, method
// routing and the source parameter. Expected bodies were taken from a
// running OpenSearch 3.8.0.

const jacksonAt = "\n at [Source: REDACTED (`StreamReadFeature.INCLUDE_SOURCE_IN_LOCATION` disabled); byte offset: #"

func newHTTPCompatCluster(t *testing.T) *Cluster {
	t.Helper()
	c := New()
	t.Cleanup(c.Close)
	mustDo(t, c, http.MethodPut, "/logs", `{"mappings":{"properties":{"author":{"type":"keyword"},"n":{"type":"integer"},"title":{"type":"text"}}}}`)
	bulk := `{"index":{"_index":"logs","_id":"1"}}
{"author":"alice","n":1,"title":"quick fox"}
{"index":{"_index":"logs","_id":"2"}}
{"author":"bob","n":2,"title":"lazy dog"}
{"index":{"_index":"logs","_id":"3"}}
{"author":"alice","n":3,"title":"quick dog"}
{"index":{"_index":"logs","_id":"4"}}
{"author":"carol","n":4,"title":null}
{"index":{"_index":"logs","_id":"5"}}
{"author":null,"n":5}
`
	res, err := c.Do(http.MethodPost, "/_bulk?refresh=true", bulk)
	if err != nil || res.IsError() {
		t.Fatalf("bulk: %v %s", err, res.Body)
	}
	return c
}

// rawDo sends a request with exactly the given headers.
func rawDo(c *Cluster, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, req)
	return rec
}

type httpError struct {
	status int
	body   map[string]any
}

func (e httpError) field(name string) any {
	inner, _ := e.body["error"].(map[string]any)
	return inner[name]
}

func (e httpError) cause() map[string]any {
	inner, _ := e.body["error"].(map[string]any)
	cause, _ := inner["caused_by"].(map[string]any)
	return cause
}

func doError(t *testing.T, c *Cluster, method, path string, body any) httpError {
	t.Helper()
	res, err := c.Do(method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(res.Body, &m)
	return httpError{status: res.StatusCode, body: m}
}

func expectError(t *testing.T, got httpError, status int, typ, reason string) {
	t.Helper()
	if got.status != status || got.field("type") != typ || got.field("reason") != reason {
		t.Fatalf("got %d %v %q, want %d %s %q (body %v)", got.status, got.field("type"), got.field("reason"), status, typ, reason, got.body)
	}
	roots, _ := got.field("root_cause").([]any)
	if len(roots) == 0 {
		t.Fatalf("missing root_cause: %v", got.body)
	}
}

func expectCause(t *testing.T, got httpError, typ, reason string) {
	t.Helper()
	cause := got.cause()
	if cause["type"] != typ || cause["reason"] != reason {
		t.Fatalf("caused_by = %v, want %s %q", cause, typ, reason)
	}
}

func TestHTTPUnrecognizedParameters(t *testing.T) {
	c := newHTTPCompatCluster(t)
	unrecognized := func(path, detail string) string { return "request [" + path + "] contains unrecognized " + detail }
	cases := []struct {
		method, uri, path, detail string
		body                      any
	}{
		{"GET", "/logs/_search?foo=bar", "/logs/_search", "parameter: [foo]", nil},
		{"GET", "/logs/_search?foo=bar&zzz=1", "/logs/_search", "parameters: [foo], [zzz]", nil},
		{"GET", "/logs/_search?siz=1", "/logs/_search", "parameter: [siz]", nil},
		{"GET", "/logs/_search?sorts=1", "/logs/_search", "parameter: [sorts] -> did you mean any of [sort, stats]?", nil},
		{"GET", "/logs/_search?prety", "/logs/_search", "parameter: [prety] -> did you mean [pretty]?", nil},
		{"GET", "/logs/_search?sourc=1", "/logs/_search", "parameter: [sourc] -> did you mean any of [_source, sort]?", nil},
		{"GET", "/logs/_search?filter_pat=1", "/logs/_search", "parameter: [filter_pat] -> did you mean [filter_path]?", nil},
		{"GET", "/logs/_search?size=1&siz=1", "/logs/_search", "parameter: [siz] -> did you mean [size]?", nil},
		{"GET", "/logs/_search?df=author", "/logs/_search", "parameter: [df]", nil},
		{"GET", "/logs/_search?lenient=true", "/logs/_search", "parameter: [lenient]", nil},
		{"GET", "/logs/_search?suggest_text=abc", "/logs/_search", "parameter: [suggest_text] -> did you mean [suggest_field]?", nil},
		{"GET", "/logs/_search?_source_include=author", "/logs/_search", "parameter: [_source_include] -> did you mean any of [_source_includes, _source_excludes]?", nil},
		{"GET", "/logs%2Clogs/_search?foo=1", "/logs,logs/_search", "parameter: [foo]", nil},
		{"GET", "/logs/_count?df=author", "/logs/_count", "parameter: [df]", nil},
		{"GET", "/logs/_validate/query?df=author", "/logs/_validate/query", "parameter: [df]", nil},
		{"GET", "/logs/_doc/1?foo=bar", "/logs/_doc/1", "parameter: [foo]", nil},
		{"PUT", "/logs/_doc/99?foo=bar", "/logs/_doc/99", "parameter: [foo]", map[string]any{"a": 1}},
		{"GET", "/?foo=bar", "/", "parameter: [foo]", nil},
		{"GET", "/_cat/indices?foo=bar", "/_cat/indices", "parameter: [foo]", nil},
		{"GET", "/_cluster/health?foo=bar", "/_cluster/health", "parameter: [foo]", nil},
		{"GET", "/logs/_mapping?include_type_name=true", "/logs/_mapping", "parameter: [include_type_name]", nil},
		{"POST", "/_bulk?batch_size=1", "/_bulk", "parameter: [batch_size]", "{\"delete\":{\"_index\":\"logs\",\"_id\":\"nope\"}}\n"},
		{"POST", "/_reindex?conflicts=proceed", "/_reindex", "parameter: [conflicts]", map[string]any{"source": map[string]any{"index": "logs"}, "dest": map[string]any{"index": "copy"}}},
		{"GET", "/_stats/foo", "/_stats/foo", "metric: [foo]", nil},
		{"GET", "/_stats/doc", "/_stats/doc", "metric: [doc] -> did you mean [docs]?", nil},
		{"GET", "/_stats/foo,docs,bar", "/_stats/foo,docs,bar", "metrics: [bar], [foo]", nil},
	}
	for _, tc := range cases {
		got := doError(t, c, tc.method, tc.uri, tc.body)
		expectError(t, got, 400, "illegal_argument_exception", unrecognized(tc.path, tc.detail))
	}

	got := doError(t, c, "GET", "/_stats/_all,docs", nil)
	expectError(t, got, 400, "illegal_argument_exception", "request [/_stats/_all,docs] contains _all and individual metrics [_all,docs]")

	// nothing runs: the index survives a DELETE with an unknown parameter
	if got := doError(t, c, "DELETE", "/logs?foo=bar", nil); got.status != 400 {
		t.Fatalf("DELETE with unknown parameter = %d", got.status)
	}
	if code, _ := status(t, c, http.MethodHead, "/logs", nil); code != 200 {
		t.Fatalf("index deleted despite the unknown parameter: HEAD = %d", code)
	}
	if rec := rawDo(c, "HEAD", "/logs?foo=bar", "", nil); rec.Code != 400 || rec.Body.Len() != 0 {
		t.Fatalf("HEAD with unknown parameter = %d %q", rec.Code, rec.Body.String())
	}

	// known parameters, including the q-dependent ones and global ones
	for _, uri := range []string{
		"/logs/_search?q=alice&df=author&analyzer=keyword&analyze_wildcard=true&lenient=true&default_operator=OR&size=0",
		"/logs/_search?size=0&typed_keys=true&rest_total_hits_as_int=false&pretty&human=true&error_trace=false&format=json&filter_path=hits",
		"/logs/_search?size=0&preference=_local&routing=a&request_cache=true&search_type=dfs_query_then_fetch&allow_partial_search_results=true&ccs_minimize_roundtrips=false&phase_took=true&cancel_after_time_interval=10s&batched_reduce_size=2",
		"/_cat/indices?v&h=index&s=index&format=json&bytes=b&pri&help=false",
		"/_cluster/health?timeout=1s&wait_for_status=yellow&level=indices&local=true",
		"/logs/_doc/1?realtime=true&refresh=false&_source_includes=author&routing=r&preference=p",
	} {
		if res, err := c.Do("GET", uri, nil); err != nil || res.IsError() {
			t.Fatalf("GET %s: %v %s", uri, err, res.Body)
		}
	}
}

func TestHTTPInvalidParameterValues(t *testing.T) {
	c := newHTTPCompatCluster(t)
	boolMsg := "Failed to parse value [abc] as only [true] or [false] are allowed."
	nfe := func(v string) string { return `For input string: "` + v + `"` }
	cases := []struct {
		method, uri string
		body        any
		status      int
		typ, reason string
		causeType   string
		causeReason string
	}{
		{"GET", "/logs/_search?q=alice&lenient=abc", nil, 400, "illegal_argument_exception", boolMsg, "", ""},
		{"GET", "/logs/_search?version=abc", nil, 400, "illegal_argument_exception", boolMsg, "", ""},
		{"GET", "/logs/_search?request_cache=abc", nil, 400, "illegal_argument_exception", boolMsg, "", ""},
		{"GET", "/logs/_search?allow_partial_search_results=abc", nil, 400, "illegal_argument_exception", boolMsg, "", ""},
		{"GET", "/logs/_search?version=TRUE", nil, 400, "illegal_argument_exception", "Failed to parse value [TRUE] as only [true] or [false] are allowed.", "", ""},
		{"GET", "/logs/_search?ignore_unavailable=abc", nil, 400, "illegal_argument_exception", "Could not convert [ignore_unavailable] to boolean", "illegal_argument_exception", boolMsg},
		{"GET", "/logs/_search?expand_wildcards=foo", nil, 400, "illegal_argument_exception", "No valid expand wildcard value [foo]", "", ""},
		{"GET", "/logs/_search?track_total_hits=abc", nil, 400, "illegal_argument_exception", "Failed to parse int parameter [track_total_hits] with value [abc]", "number_format_exception", nfe("abc")},
		{"GET", "/logs/_search?size=abc", nil, 400, "illegal_argument_exception", "Failed to parse int parameter [size] with value [abc]", "number_format_exception", nfe("abc")},
		{"GET", "/logs/_search?size=1.5", nil, 400, "illegal_argument_exception", "Failed to parse int parameter [size] with value [1.5]", "number_format_exception", nfe("1.5")},
		{"GET", "/logs/_search?size=99999999999", nil, 400, "illegal_argument_exception", "Failed to parse int parameter [size] with value [99999999999]", "number_format_exception", nfe("99999999999")},
		{"GET", "/logs/_search?from=abc", nil, 400, "illegal_argument_exception", "Failed to parse int parameter [from] with value [abc]", "number_format_exception", nfe("abc")},
		{"GET", "/logs/_search?size=-1", nil, 400, "illegal_argument_exception", "[size] parameter cannot be negative, found [-1]", "", ""},
		{"GET", "/logs/_search?terminate_after=abc", nil, 400, "illegal_argument_exception", "Failed to parse int parameter [terminate_after] with value [abc]", "number_format_exception", nfe("abc")},
		{"GET", "/logs/_search?terminate_after=-1", nil, 400, "illegal_argument_exception", "terminateAfter must be > 0", "", ""},
		{"GET", "/logs/_search?max_concurrent_shard_requests=abc", nil, 400, "illegal_argument_exception", "Failed to parse int parameter [max_concurrent_shard_requests] with value [abc]", "number_format_exception", nfe("abc")},
		{"GET", "/logs/_search?max_concurrent_shard_requests=0", nil, 400, "illegal_argument_exception", "maxConcurrentShardRequests must be >= 1", "", ""},
		{"GET", "/logs/_search?pre_filter_shard_size=0", nil, 400, "illegal_argument_exception", "preFilterShardSize must be >= 1", "", ""},
		{"GET", "/logs/_search?batched_reduce_size=1", nil, 400, "illegal_argument_exception", "batchedReduceSize must be >= 2", "", ""},
		{"GET", "/logs/_search?timeout=abc", nil, 400, "illegal_argument_exception", "failed to parse setting [timeout] with value [abc] as a time value: unit is missing or unrecognized", "", ""},
		{"GET", "/logs/_search?timeout=-5s", nil, 400, "illegal_argument_exception", "failed to parse setting [timeout] with value [-5s] as a time value: negative durations are not supported", "", ""},
		{"GET", "/logs/_search?timeout=1.5s", nil, 400, "illegal_argument_exception", "failed to parse [1.5s], fractional time values are not supported", "number_format_exception", nfe("1.5")},
		{"GET", "/logs/_search?cancel_after_time_interval=abc", nil, 400, "illegal_argument_exception", "failed to parse setting [cancel_after_time_interval] with value [abc] as a time value: unit is missing or unrecognized", "", ""},
		{"GET", "/logs/_search?scroll=", nil, 400, "illegal_argument_exception", "failed to parse setting [scroll] with value [] as a time value: unit is missing or unrecognized", "", ""},
		{"GET", "/logs/_search?search_type=foo", nil, 400, "illegal_argument_exception", "No search type for [foo]", "", ""},
		{"GET", "/logs/_search?search_type=query_and_fetch", nil, 400, "illegal_argument_exception", "Unsupported search type [query_and_fetch]", "", ""},
		{"GET", "/logs/_search?q=alice&default_operator=foo", nil, 400, "illegal_argument_exception", "No enum constant org.opensearch.index.query.Operator.FOO", "", ""},
		{"GET", "/logs/_search?rest_total_hits_as_int=true&track_total_hits=2", nil, 400, "illegal_argument_exception", "[rest_total_hits_as_int] cannot be used if the tracking of total hits is not accurate, got 2", "", ""},
		{"POST", "/logs/_search?rest_total_hits_as_int=true", map[string]any{"size": 0, "track_total_hits": 2}, 400, "illegal_argument_exception", "[rest_total_hits_as_int] cannot be used if the tracking of total hits is not accurate, got 2", "", ""},
		{"GET", "/logs/_search?search_pipeline=nope", nil, 400, "illegal_argument_exception", "Pipeline nope is not defined", "", ""},
		{"GET", "/logs/_search?allow_partial_search_results=", nil, 500, "null_pointer_exception", `Cannot invoke "java.lang.Boolean.booleanValue()" because the return value of "org.opensearch.rest.RestRequest.paramAsBoolean(String, java.lang.Boolean)" is null`, "", ""},
		{"GET", "/logs/_search?size=abc&from=abc", nil, 400, "illegal_argument_exception", "Failed to parse int parameter [from] with value [abc]", "number_format_exception", nfe("abc")},
		{"GET", "/logs/_search?size=abc&foo=bar", nil, 400, "illegal_argument_exception", "Failed to parse int parameter [size] with value [abc]", "number_format_exception", nfe("abc")},
		{"GET", "/logs/_count?min_score=abc", nil, 400, "illegal_argument_exception", "Failed to parse float parameter [min_score] with value [abc]", "number_format_exception", nfe("abc")},
		{"GET", "/logs/_validate/query?explain=abc", nil, 400, "illegal_argument_exception", boolMsg, "", ""},
		{"GET", "/logs/_field_caps?fields=*&include_unmapped=abc", nil, 400, "illegal_argument_exception", boolMsg, "", ""},
		{"GET", "/logs/_field_caps", nil, 400, "illegal_argument_exception", "specified fields can't be null or empty", "", ""},
		{"POST", "/_msearch?max_concurrent_searches=0", "{\"index\":\"logs\"}\n{\"size\":0}\n", 400, "illegal_argument_exception", "maxConcurrentSearchRequests must be positive", "", ""},
		{"PUT", "/logs/_doc/p?refresh=abc", map[string]any{"a": 1}, 400, "illegal_argument_exception", "Unknown value for refresh: [abc].", "", ""},
		{"PUT", "/logs/_doc/p?version=abc", map[string]any{"a": 1}, 400, "illegal_argument_exception", "Failed to parse long parameter [version] with value [abc]", "number_format_exception", nfe("abc")},
		{"PUT", "/logs/_doc/p?op_type=foo", map[string]any{"a": 1}, 400, "illegal_argument_exception", "opType must be 'create' or 'index', found: [foo]", "", ""},
		{"PUT", "/logs/_create/p?op_type=index", map[string]any{"a": 1}, 400, "illegal_argument_exception", "opType must be 'create', found: [index]", "", ""},
		{"PUT", "/logs/_doc/p?version_type=foo", map[string]any{"a": 1}, 400, "illegal_argument_exception", "No version type match [foo]", "", ""},
		{"PUT", "/logs/_doc/p?wait_for_active_shards=abc", map[string]any{"a": 1}, 400, "illegal_argument_exception", "cannot parse ActiveShardCount[abc]", "number_format_exception", nfe("abc")},
		{"PUT", "/logs/_doc/p?pipeline=nope", map[string]any{"a": 1}, 400, "illegal_argument_exception", "pipeline with id [nope] does not exist", "", ""},
		{"POST", "/logs/_update/1?version=1", map[string]any{"doc": map[string]any{}}, 400, "action_request_validation_exception", "Validation Failed: 1: internal versioning can not be used for optimistic concurrency control. Please use `if_seq_no` and `if_primary_term` instead;", "", ""},
		{"GET", "/logs/_doc/1?fields=a", nil, 400, "illegal_argument_exception", "the parameter [fields] is no longer supported, please use [stored_fields] to retrieve stored fields or [_source] to load the field from _source", "", ""},
		{"GET", "/_cluster/health?wait_for_status=foo", nil, 400, "illegal_argument_exception", "No enum constant org.opensearch.cluster.health.ClusterHealthStatus.FOO", "", ""},
		{"GET", "/_cluster/health?wait_for_events=foo", nil, 400, "illegal_argument_exception", "No enum constant org.opensearch.common.Priority.FOO", "", ""},
		{"GET", "/_cluster/health?wait_for_active_shards=-2", nil, 400, "illegal_argument_exception", "shard count cannot be a negative value", "", ""},
		{"GET", "/_cluster/stats?timeout=abc", nil, 400, "illegal_argument_exception", "failed to parse setting [ClusterStatsRequest.timeout] with value [abc] as a time value: unit is missing or unrecognized", "", ""},
		{"GET", "/_stats?level=foo", nil, 400, "illegal_argument_exception", "level parameter must be one of [cluster] or [indices] or [shards] but was [foo]", "", ""},
		{"POST", "/logs/_forcemerge?max_num_segments=abc", nil, 400, "illegal_argument_exception", "Failed to parse int parameter [max_num_segments] with value [abc]", "number_format_exception", nfe("abc")},
		{"PUT", "/logs?wait_for_active_shards=index-setting", nil, 400, "illegal_argument_exception", "cannot parse ActiveShardCount[index-setting]", "number_format_exception", nfe("index-setting")},
		{"DELETE", "/logs?master_timeout=1s&cluster_manager_timeout=1s", nil, 400, "parse_exception", "Please only use one of the request parameters [cluster_manager_timeout, master_timeout].", "", ""},
		{"POST", "/logs/_delete_by_query?scroll_size=-1", map[string]any{"query": map[string]any{"match_all": map[string]any{}}}, 400, "illegal_argument_exception", "[size] parameter cannot be negative, found [-1]", "", ""},
		{"POST", "/logs/_delete_by_query?slices=0", map[string]any{"query": map[string]any{"match_all": map[string]any{}}}, 400, "illegal_argument_exception", `[slices] must be a positive integer or the string "auto", but was [0]`, "", ""},
		{"POST", "/_reindex?requests_per_second=0", map[string]any{"source": map[string]any{"index": "logs"}, "dest": map[string]any{"index": "copy"}}, 400, "illegal_argument_exception", "[requests_per_second] must be a float greater than 0. Use -1 to disable throttling.", "", ""},
		{"POST", "/_reindex?pipeline=x", map[string]any{"source": map[string]any{"index": "logs"}, "dest": map[string]any{"index": "copy"}}, 400, "illegal_argument_exception", "_reindex doesn't support [pipeline] as a query parameter. Specify it in the [dest] object instead.", "", ""},
		{"GET", "/_cat/indices?health=foo", nil, 400, "illegal_argument_exception", "unknown cluster health status [foo]", "", ""},
		{"GET", "/logs?flat_settings=abc", nil, 400, "illegal_argument_exception", boolMsg, "", ""},
	}
	for _, tc := range cases {
		got := doError(t, c, tc.method, tc.uri, tc.body)
		expectError(t, got, tc.status, tc.typ, tc.reason)
		if tc.causeType != "" {
			expectCause(t, got, tc.causeType, tc.causeReason)
		}
	}

	// conflicts=foo must fail before anything is deleted
	got := doError(t, c, "POST", "/logs/_delete_by_query?conflicts=foo", map[string]any{"query": map[string]any{"match_all": map[string]any{}}})
	expectError(t, got, 400, "illegal_argument_exception", `conflicts may only be "proceed" or "abort" but was [foo]`)
	if n, err := c.Count("logs", nil); err != nil || n != 5 {
		t.Fatalf("documents after rejected delete_by_query = %d, %v", n, err)
	}

	// channel parameters are parsed before routing
	got = doError(t, c, "PUT", "/logs/_search?pretty=abc", map[string]any{})
	expectError(t, got, 400, "illegal_argument_exception", boolMsg)
	got = doError(t, c, "GET", "/a/b/c/d/e?human=abc", nil)
	expectError(t, got, 400, "illegal_argument_exception", boolMsg)

	// OpenSearch leaves unset what these values mean to unset
	var hits struct {
		Hits struct {
			Total json.RawMessage  `json:"total"`
			Hits  []map[string]any `json:"hits"`
		} `json:"hits"`
	}
	for _, tc := range []struct{ uri, check string }{
		{"/logs/_search?version=&size=1", "no _version"},
		{"/logs/_search?terminate_after=0&size=0", "terminate_after=0 accepted"},
		{"/logs/_search?sort=n:foo&size=1", "unknown sort order ignored"},
		{"/logs/_search?size=1&size=2", "last value wins"},
	} {
		res, err := c.Do("GET", tc.uri, nil)
		if err != nil || res.IsError() {
			t.Fatalf("%s (%s): %v %s", tc.uri, tc.check, err, res.Body)
		}
		if err := res.JSON(&hits); err != nil {
			t.Fatal(err)
		}
		switch tc.check {
		case "no _version":
			if _, ok := hits.Hits.Hits[0]["_version"]; ok {
				t.Fatalf("version= rendered _version: %s", res.Body)
			}
		case "unknown sort order ignored":
			if _, ok := hits.Hits.Hits[0]["sort"]; ok || hits.Hits.Hits[0]["_score"] == nil {
				t.Fatalf("sort=n:foo applied a sort: %s", res.Body)
			}
		case "last value wins":
			if len(hits.Hits.Hits) != 2 {
				t.Fatalf("size=1&size=2 returned %d hits", len(hits.Hits.Hits))
			}
		}
	}

	res, err := c.Do("POST", "/logs/_search?rest_total_hits_as_int=true", map[string]any{"size": 0, "track_total_hits": false})
	if err != nil || res.IsError() {
		t.Fatalf("rest_total_hits_as_int with untracked hits: %v %s", err, res.Body)
	}
	_ = res.JSON(&hits)
	if string(hits.Hits.Total) != "-1" {
		t.Fatalf("hits.total = %s, want -1", hits.Hits.Total)
	}

	rec := rawDo(c, "GET", "/logs/_search?size=%zz", "", nil)
	var bad map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &bad)
	badErr := httpError{status: rec.Code, body: bad}
	expectError(t, badErr, 400, "bad_parameter_exception", "java.lang.IllegalArgumentException: invalid escape sequence `%zz' at index 0 of: %zz")
	expectCause(t, badErr, "illegal_argument_exception", "invalid escape sequence `%zz' at index 0 of: %zz")
}

func TestHTTPContentTypeHandling(t *testing.T) {
	c := newHTTPCompatCluster(t)
	flat := func(rec *httptest.ResponseRecorder) map[string]any {
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	for _, tc := range []struct {
		header  string
		present bool
		want    string
	}{
		{"", false, "Content-Type header [] is not supported"},
		{"text/plain", true, "Content-Type header [text/plain] is not supported"},
		{"application/x-www-form-urlencoded", true, "Content-Type header [application/x-www-form-urlencoded] is not supported"},
	} {
		headers := map[string]string{}
		if tc.present {
			headers["Content-Type"] = tc.header
		}
		rec := rawDo(c, "POST", "/logs/_search?foo=1", `{"size":0}`, headers)
		if body := flat(rec); rec.Code != 406 || body["error"] != tc.want || body["status"] != float64(406) {
			t.Fatalf("Content-Type %q: %d %s", tc.header, rec.Code, rec.Body)
		}
	}
	for _, header := range []string{"garbage", "application/json/x"} {
		rec := rawDo(c, "POST", "/logs/_search", `{"size":0}`, map[string]string{"Content-Type": header})
		got := httpError{status: rec.Code, body: flat(rec)}
		expectError(t, got, 400, "content_type_header_exception", "java.lang.IllegalArgumentException: invalid Content-Type header ["+header+"]")
		expectCause(t, got, "illegal_argument_exception", "invalid Content-Type header ["+header+"]")
	}
	for _, header := range []string{"application/json; charset=UTF-8", "application/x-ndjson", "APPLICATION/JSON", "application/vnd.opensearch+json;compatible-with=7"} {
		if rec := rawDo(c, "POST", "/logs/_search?size=0", `{"size":0}`, map[string]string{"Content-Type": header}); rec.Code != 200 {
			t.Fatalf("Content-Type %q: %d %s", header, rec.Code, rec.Body)
		}
	}
	if rec := rawDo(c, "GET", "/logs/_search?size=0", "", map[string]string{"Content-Type": "text/plain"}); rec.Code != 200 {
		t.Fatalf("GET without body and text/plain: %d", rec.Code)
	}
	if rec := rawDo(c, "HEAD", "/logs", "{}", map[string]string{"Content-Type": "text/plain"}); rec.Code != 406 || rec.Body.Len() != 0 {
		t.Fatalf("HEAD with text/plain body: %d %q", rec.Code, rec.Body)
	}
	if rec := rawDo(c, "PUT", "/logs/_search", "{}", map[string]string{"Content-Type": "text/plain"}); rec.Code != 405 {
		t.Fatalf("405 comes before 406: %d", rec.Code)
	}
	if rec := rawDo(c, "POST", "/_bulk", "{\"delete\":{\"_index\":\"logs\",\"_id\":\"nope\"}}\n", map[string]string{"Content-Type": "text/plain"}); rec.Code != 406 {
		t.Fatalf("bulk with text/plain: %d", rec.Code)
	}
	// osmem only parses JSON bodies
	if rec := rawDo(c, "POST", "/logs/_search", "size: 0\n", map[string]string{"Content-Type": "application/yaml"}); rec.Code != 406 {
		t.Fatalf("YAML body: %d %s", rec.Code, rec.Body)
	}
}

func TestHTTPJSONBodyStrictness(t *testing.T) {
	c := newHTTPCompatCluster(t)
	parsing := func(body, reason string, line, col float64) {
		t.Helper()
		got := doError(t, c, "POST", "/logs/_search", body)
		expectError(t, got, 400, "parsing_exception", reason)
		if got.field("line") != line || got.field("col") != col {
			t.Fatalf("%q: line/col = %v/%v, want %v/%v", body, got.field("line"), got.field("col"), line, col)
		}
	}
	parsing("null", "Expected [START_OBJECT] but found [VALUE_NULL]", 1, 1)
	parsing("   ", "Expected [START_OBJECT] but found [null]", 1, 0)
	parsing("[]", "Expected [START_OBJECT] but found [START_ARRAY]", 1, 1)
	parsing(`"abc"`, "Expected [START_OBJECT] but found [VALUE_STRING]", 1, 1)
	parsing(`{"size":1}{"size":2}`, "Unexpected token [START_OBJECT] found after the main object.", 1, 11)
	parsing(`{"size":0} 1`, "Unexpected token [VALUE_NUMBER] found after the main object.", 1, 12)
	parsing("{\"size\":0}\ntrue", "Unexpected token [VALUE_BOOLEAN] found after the main object.", 2, 1)

	jsonParse := func(method, path, body, reason string, offset string) {
		t.Helper()
		got := doError(t, c, method, path, body)
		want := reason + jacksonAt + offset + "]"
		expectError(t, got, 400, "json_parse_exception", want)
		expectCause(t, got, "stream_read_exception", want)
	}
	jsonParse("POST", "/logs/_search", `{}garbage`, "Unrecognized token 'garbage': was expecting (JSON String, Number, Array, Object or token 'null', 'true' or 'false')", "2")
	jsonParse("POST", "/logs/_search", `{"size":0}}`, "Unexpected close marker '}': no open Object to close", "10")
	jsonParse("POST", "/logs/_search", `{"size":0} ,`, "Unexpected character (',' (code 44)): expected a value", "11")
	jsonParse("POST", "/logs/_search", `{"size":tru}`, "Unrecognized token 'tru': was expecting (JSON String, Number, Array, Object or token 'null', 'true' or 'false')", "8")
	jsonParse("POST", "/logs/_search", `{"size":01}`, "Invalid numeric value: Leading zeroes not allowed", "9")
	jsonParse("POST", "/logs/_search", `{"a":}`, "Unexpected character ('}' (code 125)): expected a value", "5")
	jsonParse("POST", "/logs/_search", `{"query":{"match_all":{}},"query":{"match_all":{}}}`, `Duplicate Object property "query"`, "33")
	jsonParse("POST", "/logs/_search", `{"query":{"term":{"author":"a","author":"b"}}}`, `Duplicate Object property "author"`, "39")
	jsonParse("POST", "/logs/_search", `{"size":0,"aggs":{"a":{"terms":{"field":"author"}},"a":{"max":{"field":"n"}}}}`, `Duplicate Object property "a"`, "54")
	jsonParse("POST", "/logs/_search", "{\"size\":0,\"s\x5cu0069ze\":1}", `Duplicate Object property "size"`, "21")
	jsonParse("POST", "/logs/_search", "{\"query\":{\"term\":{\"author\":\"\u3042\u3044\"}},\"query\":{}}", `Duplicate Object property "query"`, "45")
	jsonParse("POST", "/_msearch", "{\"index\":\"logs\"}\n{\"size\":0,\"size\":1}\n", `Duplicate Object property "size"`, "16")
	jsonParse("POST", "/logs/_update/1", `{"doc":{"n":1},"doc":{"n":1}}`, `Duplicate Object property "doc"`, "20")

	dup := `Duplicate Object property "a"` + jacksonAt + "10]"
	got := doError(t, c, "PUT", "/logs/_doc/dup", `{"a":1,"a":2}`)
	expectError(t, got, 400, "mapper_parsing_exception", "failed to parse")
	expectCause(t, got, "json_parse_exception", dup)
	got = doError(t, c, "PUT", "/logs/_doc/two", `{"a":1}{"b":2}`)
	expectError(t, got, 400, "mapper_parsing_exception", "failed to parse")
	expectCause(t, got, "illegal_argument_exception", "Malformed content, found extra data after parsing: START_OBJECT")
	got = doError(t, c, "PUT", "/logs/_doc/null", `null`)
	expectCause(t, got, "not_x_content_exception", "Compressor detection can only be called on some xcontent bytes or compressed xcontent bytes")

	got = doError(t, c, "PUT", "/logs/_mapping", `{"properties":{"x":{"type":"keyword"},"x":{"type":"long"}}}`)
	expectError(t, got, 400, "parse_exception", "Failed to parse content to map")
	expectCause(t, got, "json_parse_exception", `Duplicate Object property "x"`+jacksonAt+"41]")

	got = doError(t, c, "POST", "/logs/_count", `{"query":{"match_all":{}},"query":{"match_all":{}}}`)
	expectError(t, got, 400, "parsing_exception", "Failed to parse")
	if got.field("line") != float64(1) || got.field("col") != float64(25) {
		t.Fatalf("count duplicate location = %v", got.body)
	}

	got = doError(t, c, "POST", "/_msearch", "{}\n[]\n")
	expectError(t, got, 400, "parsing_exception", "Expected [START_OBJECT] but found [START_ARRAY]")

	got = doError(t, c, "POST", "/_search/scroll", `{"scroll_id":"a","scroll_id":"b"}`)
	expectError(t, got, 400, "illegal_argument_exception", "Failed to parse request body")

	got = doError(t, c, "PUT", "/_cluster/settings", "")
	expectError(t, got, 400, "parse_exception", "request body is required")
	got = doError(t, c, "POST", "/_mget", "")
	expectError(t, got, 400, "parse_exception", "request body or source parameter is required")

	// _count ignores content after the query object, like OpenSearch
	if res, err := c.Do("POST", "/logs/_count", `{}garbage`); err != nil || res.IsError() {
		t.Fatalf("count with trailing content: %v %s", err, res.Body)
	}
}

func TestHTTPFilterPathCompatibility(t *testing.T) {
	c := newHTTPCompatCluster(t)
	get := func(method, uri string, body any) string {
		t.Helper()
		res, err := c.Do(method, uri, body)
		if err != nil {
			t.Fatal(err)
		}
		return string(res.Body)
	}
	for _, tc := range []struct {
		uri  string
		body any
		want string
	}{
		{"/logs/_search?filter_path=-hits.hits._source,-took,-_shards", map[string]any{"size": 1, "sort": []any{"n"}},
			`{"hits":{"hits":[{"_id":"1","_index":"logs","_score":null,"sort":[1]}],"max_score":null,"total":{"relation":"eq","value":5}},"timed_out":false}`},
		{"/logs/_search?filter_path=hits.total,-hits.total.relation", map[string]any{"size": 0}, `{"hits":{"total":{"value":5}}}`},
		{"/logs/_search?filter_path=hits.total,-hits.total", map[string]any{"size": 0}, `{}`},
		{"/logs/_search?filter_path=-hits.hits", map[string]any{"size": 0, "sort": []any{"n"}}, ""},
		{"/logs/_search?filter_path=hits.hits._source.author", map[string]any{"query": map[string]any{"ids": map[string]any{"values": []any{"5"}}}},
			`{"hits":{"hits":[{"_source":{"author":null}}]}}`},
		{"/logs/_search?filter_path=**.total", map[string]any{"size": 0}, `{"_shards":{"total":1},"hits":{"total":{"relation":"eq","value":5}}}`},
		{"/logs/_search?filter_path=hi?s", map[string]any{"size": 0}, `{}`},
		{"/logs/_search?filter_path=hits.hits.sort", map[string]any{"size": 2, "sort": []any{"n"}}, `{"hits":{"hits":[{"sort":[1]},{"sort":[2]}]}}`},
		{"/logs/_search?filter_path=-hits.hits._source", map[string]any{"size": 0}, ""},
		{"/_cat/indices/logs?format=json&filter_path=docs\\.count", nil, `[{"docs.count":"5"}]`},
		{"/_cat/indices/logs?format=json&filter_path=nothing", nil, ``},
	} {
		got := get("POST", tc.uri, tc.body)
		if strings.HasPrefix(tc.uri, "/_cat") {
			got = get("GET", tc.uri, nil)
		}
		if tc.want == "" && strings.Contains(tc.uri, "-hits.hits") {
			if strings.Contains(got, `"hits":[`) || !strings.Contains(got, `"took"`) {
				t.Fatalf("%s = %s", tc.uri, got)
			}
			continue
		}
		if got != tc.want {
			t.Fatalf("%s = %s, want %s", tc.uri, got, tc.want)
		}
	}

	// error responses are never filtered
	got := doError(t, c, "POST", "/logs/_search?filter_path=status", map[string]any{"query": map[string]any{"foo": map[string]any{}}})
	if got.status != 400 || got.field("type") != "parsing_exception" || got.body["status"] != float64(400) {
		t.Fatalf("filtered error response: %v", got.body)
	}
	got = doError(t, c, "POST", "/logs/_search?filter_path=-", map[string]any{"size": 0})
	expectError(t, got, 400, "illegal_argument_exception", "filters cannot be null or empty")
	if body := get("POST", "/logs/_search?filter_path=,&size=0", map[string]any{}); !strings.Contains(body, `"took"`) {
		t.Fatalf("filter_path=, filtered the response: %s", body)
	}
}

func TestHTTPMethodAndPathHandling(t *testing.T) {
	c := newHTTPCompatCluster(t)
	flat := func(rec *httptest.ResponseRecorder) map[string]any {
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	for _, tc := range []struct{ method, uri, allowed string }{
		{"PUT", "/logs/_search", "GET, POST"},
		{"DELETE", "/logs/_search?size=1&foo=bar", "GET, POST"},
		{"PUT", "/logs/_doc", "POST"},
		{"GET", "/_bulk", "PUT, POST"},
		{"POST", "/logs", "HEAD, DELETE, PUT, GET"},
		{"PATCH", "/logs/_mapping", "PUT, POST, GET"},
		{"PUT", "/_search/scroll", "DELETE, GET, POST"},
		{"POST", "/_search/point_in_time/_all", "DELETE, GET"},
		{"DELETE", "/_cluster/health", "GET"},
		{"GET", "/logs/_update/1", "POST"},
		{"PUT", "/logs/_refresh", "GET, POST"},
	} {
		rec := rawDo(c, tc.method, tc.uri, "", nil)
		want := "Incorrect HTTP method for uri [" + tc.uri + "] and method [" + tc.method + "], allowed: [" + tc.allowed + "]"
		body := flat(rec)
		if rec.Code != 405 || body["error"] != want || body["status"] != float64(405) || rec.Header().Get("Allow") != strings.ReplaceAll(tc.allowed, ", ", ",") {
			t.Fatalf("%s %s: %d %s allow=%q", tc.method, tc.uri, rec.Code, rec.Body, rec.Header().Get("Allow"))
		}
	}
	for _, path := range []string{"/logs/_search", "/_msearch", "/logs/_count", "/logs/_field_caps", "/logs/_validate/query", "/_search/scroll"} {
		if rec := rawDo(c, "HEAD", path, "", nil); rec.Code != 405 || rec.Body.Len() != 0 {
			t.Fatalf("HEAD %s: %d %q", path, rec.Code, rec.Body)
		}
	}
	if rec := rawDo(c, "OPTIONS", "/logs/_search", "", nil); rec.Code != 200 || rec.Header().Get("Allow") != "GET,POST" || rec.Body.Len() != 0 {
		t.Fatalf("OPTIONS: %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	if rec := rawDo(c, "OPTIONS", "/a/b/c/d/e", "", nil); rec.Code != 200 || rec.Header().Get("Allow") != "" {
		t.Fatalf("OPTIONS unknown path: %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
	if rec := rawDo(c, "GET", "/_cat/nope?x=1", "", nil); rec.Code != 400 || flat(rec)["error"] != "no handler found for uri [/_cat/nope] and method [GET]" {
		t.Fatalf("unknown path: %d %s", rec.Code, rec.Body)
	}
	got := doError(t, c, "PUT", "/_bad", nil)
	expectError(t, got, 400, "invalid_index_name_exception", "Invalid index name [_bad], must not start with '_', '-', or '+'")
	if res, err := c.Do("GET", "/_flush", nil); err != nil || res.IsError() {
		t.Fatalf("GET /_flush: %v %s", err, res.Body)
	}
	// the management API keeps its own routing
	if rec := rawDo(c, "GET", "/_osmem?foo=bar", "", nil); rec.Code != 200 {
		t.Fatalf("GET /_osmem: %d %s", rec.Code, rec.Body)
	}
}

func TestHTTPSourceParameter(t *testing.T) {
	c := newHTTPCompatCluster(t)
	q := url.QueryEscape(`{"query":{"term":{"author":"bob"}},"_source":false}`)
	res, err := c.Do("GET", "/logs/_search?source="+q+"&source_content_type=application/json&filter_path=hits.hits._id", nil)
	if err != nil || string(res.Body) != `{"hits":{"hits":[{"_id":"2"}]}}` {
		t.Fatalf("source parameter: %v %s", err, res.Body)
	}
	res, err = c.Do("GET", "/logs/_count?source="+url.QueryEscape(`{"query":{"term":{"author":"alice"}}}`)+"&source_content_type=application/json&filter_path=count", nil)
	if err != nil || string(res.Body) != `{"count":2}` {
		t.Fatalf("count source parameter: %v %s", err, res.Body)
	}
	got := doError(t, c, "GET", "/logs/_search?source=%7B%7D", nil)
	expectError(t, got, 500, "illegal_state_exception", "source and source_content_type parameters are required")
	got = doError(t, c, "GET", "/logs/_search?source=%7B%7D&source_content_type=text/plain", nil)
	expectError(t, got, 500, "illegal_state_exception", "Unknown value for source_content_type [text/plain]")
	got = doError(t, c, "POST", "/logs/_search?source="+q+"&source_content_type=application/json", map[string]any{"size": 0})
	expectError(t, got, 400, "illegal_argument_exception", "request [/logs/_search] contains unrecognized parameters: [source] -> did you mean [_source]?, [source_content_type]")
	got = doError(t, c, "GET", "/_cluster/health?source=%7B%7D&source_content_type=application/json", nil)
	expectError(t, got, 400, "illegal_argument_exception", "request [/_cluster/health] contains unrecognized parameters: [source], [source_content_type]")
}
