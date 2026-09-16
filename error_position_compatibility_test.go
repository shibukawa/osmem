package osmem

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

// Parse errors report where OpenSearch's parser was in the request body: a
// "[line:col] " reason prefix (x_content_parse_exception,
// named_object_not_found_exception), line and col metadata
// (parsing_exception) or a location inside the message, with 1-based lines
// and columns counting UTF-8 bytes. Bodies that OpenSearch writes out again
// before parsing them (delete and update by query bodies, reindex sources,
// alias filters) are located in that compact rendering. The expected error
// chains are the ones OpenSearch 3.8 returned for the same requests.

type posCase struct {
	name, method, path, body string
	status                   int
	chain                    []string // type|reason|line:col of the error and its causes
	root                     string   // the first root cause
}

func posEntry(e map[string]any) string {
	loc := ""
	if line, ok := e["line"]; ok {
		loc = fmt.Sprintf("%v:%v", line, e["col"])
	}
	return fmt.Sprintf("%v|%v|%s", e["type"], e["reason"], loc)
}

func posErrorChain(t *testing.T, raw []byte) (chain []string, root string) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	e, _ := body["error"].(map[string]any)
	for cur := e; cur != nil; {
		chain = append(chain, posEntry(cur))
		cur, _ = cur["caused_by"].(map[string]any)
	}
	if roots, _ := e["root_cause"].([]any); len(roots) > 0 {
		r, _ := roots[0].(map[string]any)
		root = posEntry(r)
	}
	return chain, root
}

func TestErrorPositionCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	for _, setup := range []struct{ method, path, body string }{
		{http.MethodPut, "/r2pos-q", `{"mappings":{"properties":{"author":{"type":"keyword"},"body":{"type":"text"},"year":{"type":"integer"},"loc":{"type":"geo_point"}}}}`},
		{http.MethodPut, "/r2pos-d/_doc/1?refresh=true", `{"a":1}`},
		{http.MethodPut, "/r2pos-a", `{"mappings":{"properties":{"f":{"type":"keyword"}}}}`},
	} {
		if res, err := c.Do(setup.method, setup.path, setup.body); err != nil || res.IsError() {
			t.Fatalf("%s %s: %v %v", setup.method, setup.path, err, res)
		}
	}
	for _, tc := range posCases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := c.Do(tc.method, tc.path, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			chain, root := posErrorChain(t, res.Body)
			if res.StatusCode != tc.status || !reflect.DeepEqual(chain, tc.chain) || root != tc.root {
				t.Fatalf("%s %s %s\n got %d %q root %q\nwant %d %q root %q", tc.method, tc.path, tc.body, res.StatusCode, chain, root, tc.status, tc.chain, tc.root)
			}
		})
	}
}

var posCases = []posCase{
	{name: "aggs unknown field", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"aggs":{"a":{"terms":{"field":"author","foo":1}}}}`,
		chain: []string{`x_content_parse_exception|[1:41] [terms] unknown field [foo]|`},
		root:  `x_content_parse_exception|[1:41] [terms] unknown field [foo]|`},
	{name: "aggs value check", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"aggs":{"a":{"terms":{"field":"author","size":-1}}}}`,
		chain: []string{`x_content_parse_exception|[1:48] [terms] failed to parse field [size]|`, `illegal_argument_exception|[size] must be greater than 0. Found [-1] in [a]|`},
		root:  `x_content_parse_exception|[1:48] [terms] failed to parse field [size]|`},
	{name: "aggs value type", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"aggs":{"a":{"terms":{"field":["author"]}}}}`,
		chain: []string{`x_content_parse_exception|[1:32] [terms] field doesn't support values of type: START_ARRAY|`},
		root:  `x_content_parse_exception|[1:32] [terms] field doesn't support values of type: START_ARRAY|`},
	{name: "aggs object checked at its end", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"aggs":{"a":{"terms":{"field":"author","include":{"partition":0}}}}}`,
		chain: []string{`x_content_parse_exception|[1:65] [terms] failed to parse field [include]|`, `illegal_argument_exception|Missing [num_partitions] parameter for partition-based include|`},
		root:  `x_content_parse_exception|[1:65] [terms] failed to parse field [include]|`},
	{name: "aggs nested unknown field", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"aggs":{"c":{"composite":{"sources":[{"k":{"terms":{"field":"author","format":"x"}}}]}}}}`,
		chain: []string{`x_content_parse_exception|[1:80] [composite] failed to parse field [sources]|`, `x_content_parse_exception|[1:71] [terms] unknown field [format]|`},
		root:  `x_content_parse_exception|[1:71] [terms] unknown field [format]|`},
	{name: "aggs unknown type", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"aggs":{"x":{"top_metrics":{"metrics":{"field":"year"}}}}}`,
		chain: []string{`parsing_exception|Unknown aggregation type [top_metrics] did you mean [top_hits]?|1:29`, `named_object_not_found_exception|[1:29] unknown field [top_metrics]|`},
		root:  `parsing_exception|Unknown aggregation type [top_metrics] did you mean [top_hits]?|1:29`},
	{name: "aggs invalid name", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"aggs":{"a>b":{"terms":{"field":"author"}}}}`,
		chain: []string{`parsing_exception|Invalid aggregation name [a>b]. Aggregation names can contain any character except '[', ']', and '>'|1:10`},
		root:  `parsing_exception|Invalid aggregation name [a>b]. Aggregation names can contain any character except '[', ']', and '>'|1:10`},
	{name: "aggs missing definition", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"aggs":{"a":{}}}`,
		chain: []string{`parsing_exception|Missing definition for aggregation [a]|1:15`},
		root:  `parsing_exception|Missing definition for aggregation [a]|1:15`},
	{name: "aggs bucket_sort build", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"aggs":{"h":{"histogram":{"field":"year","interval":1},"aggs":{"bs":{"bucket_sort":{"size":0}}}}}}`,
		chain: []string{`x_content_parse_exception|[1:93] failed to build [bucket_sort] after last required field arrived|`, `x_content_parse_exception|[1:93] [bucket_sort] failed to parse field [size]|`, `illegal_argument_exception|[size] must be a positive integer: [0]|`},
		root:  `x_content_parse_exception|[1:93] [bucket_sort] failed to parse field [size]|`},
	{name: "aggs order direction", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"aggs":{"a":{"terms":{"field":"author","order":{"_count":"up"}}}}}`,
		chain: []string{`x_content_parse_exception|[1:59] [terms] failed to parse field [order]|`, `parsing_exception|Unknown order direction [up]|1:59`},
		root:  `parsing_exception|Unknown order direction [up]|1:59`},
	{name: "aggs filter empty clause", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"aggs":{"f":{"filter":{}}}}`,
		chain: []string{`illegal_argument_exception|query malformed, empty clause found at [1:25]|`},
		root:  `illegal_argument_exception|query malformed, empty clause found at [1:25]|`},
	{name: "query unknown", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"bogus":{}}}`,
		chain: []string{`parsing_exception|unknown query [bogus]|1:19`, `named_object_not_found_exception|[1:19] unknown field [bogus]|`},
		root:  `parsing_exception|unknown query [bogus]|1:19`},
	{name: "query parameter", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"term":{"author":{"value":"x","foo":1}}}}`,
		chain: []string{`parsing_exception|[term] query does not support [foo]|1:47`},
		root:  `parsing_exception|[term] query does not support [foo]|1:47`},
	{name: "query multiple fields", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"match":{"author":"x","body":"y"}}}`,
		chain: []string{`parsing_exception|[match] query doesn't support multiple fields, found [author] and [body]|1:40`},
		root:  `parsing_exception|[match] query doesn't support multiple fields, found [author] and [body]|1:40`},
	{name: "query end of object", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"match":{}}}`,
		chain: []string{`parsing_exception|No text specified for text query|1:20`},
		root:  `parsing_exception|No text specified for text query|1:20`},
	{name: "query second key", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"match_all":{},"term":{"author":"x"}}}`,
		chain: []string{`parsing_exception|[match_all] malformed query, expected [END_OBJECT] but found [FIELD_NAME]|1:26`},
		root:  `parsing_exception|[match_all] malformed query, expected [END_OBJECT] but found [FIELD_NAME]|1:26`},
	{name: "query no start object", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"match":"x"}}`,
		chain: []string{`parsing_exception|[match] query malformed, no start_object after query name|1:19`},
		root:  `parsing_exception|[match] query malformed, no start_object after query name|1:19`},
	{name: "query unknown token", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"exists":{"field":["author"]}}}`,
		chain: []string{`parsing_exception|[exists] unknown token [START_ARRAY] after [field]|1:29`},
		root:  `parsing_exception|[exists] unknown token [START_ARRAY] after [field]|1:29`},
	{name: "query range bound conflict", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"range":{"year":{"from":1,"include_lower":true,"gt":2}}}}`,
		chain: []string{`parsing_exception|invalid lower bound for [range] query|1:63`},
		root:  `parsing_exception|invalid lower bound for [range] query|1:63`},
	{name: "query terms fields", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"terms":{"body":["x"],"author":["y"]}}}`,
		chain: []string{`parsing_exception|[terms] query does not support multiple fields|1:42`},
		root:  `parsing_exception|[terms] query does not support multiple fields|1:42`},
	{name: "query bool unknown field", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"bool":{"foo":[]}}}`,
		chain: []string{`x_content_parse_exception|[1:19] [bool] unknown field [foo]|`},
		root:  `x_content_parse_exception|[1:19] [bool] unknown field [foo]|`},
	{name: "query bool clause", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"bool":{"must":{"match":{"author":{"foo":1}}}}}}`,
		chain: []string{`x_content_parse_exception|[1:52] [bool] failed to parse field [must]|`, `parsing_exception|[match] query does not support [foo]|1:52`},
		root:  `parsing_exception|[match] query does not support [foo]|1:52`},
	{name: "query bool empty clause", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"bool":{"must":[{}]}}}`,
		chain: []string{`x_content_parse_exception|[1:28] [bool] failed to parse field [must]|`, `illegal_argument_exception|query malformed, empty clause found at [1:28]|`},
		root:  `x_content_parse_exception|[1:28] [bool] failed to parse field [must]|`},
	{name: "query match_all field", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"match_all":{"boost":"abc"}}}`,
		chain: []string{`parsing_exception|[1:32] [match_all] failed to parse field [boost]|1:32`, `x_content_parse_exception|[1:32] [match_all] failed to parse field [boost]|`, `number_format_exception|For input string: "abc"|`},
		root:  `parsing_exception|[1:32] [match_all] failed to parse field [boost]|1:32`},
	{name: "query ids values", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"ids":{"values":[{"a":1}]}}}`,
		chain: []string{`parsing_exception|[1:28] [ids] failed to parse field [values]|1:28`, `x_content_parse_exception|[1:28] [ids] failed to parse field [values]|`, `illegal_state_exception|Can't get text on a START_OBJECT at 1:28|`},
		root:  `parsing_exception|[1:28] [ids] failed to parse field [values]|1:28`},
	{name: "query text of array", method: "POST", path: "/r2pos-q/_search", status: 500,
		body:  `{"query":{"match":{"author":["x"]}}}`,
		chain: []string{`illegal_state_exception|Can't get text on a START_ARRAY at 1:29|`},
		root:  `illegal_state_exception|Can't get text on a START_ARRAY at 1:29|`},
	{name: "query function_score function", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"function_score":{"functions":[{"foo":{}}]}}}`,
		chain: []string{`parsing_exception|unknown query [function_score] did you mean any of [random_score, script_score]?|1:49`, `named_object_not_found_exception|[1:49] unknown field [foo]|`},
		root:  `parsing_exception|unknown query [function_score] did you mean any of [random_score, script_score]?|1:49`},
	{name: "query wrapper source", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"wrapper":{"query":"eyJ0ZXJtIjogeyJhIjogeyJib2d1cyI6IDF9fX0="}}}`,
		chain: []string{`parsing_exception|[term] query does not support [bogus]|1:26`},
		root:  `parsing_exception|[term] query does not support [bogus]|1:26`},
	{name: "query pretty body", method: "POST", path: "/r2pos-q/_search", status: 400,
		body: `{
  "query": {
    "bool": {
      "must": [
        {"term": {"author": {"value": "x", "foo": 1}}}
      ]
    }
  }
}`,
		chain: []string{`x_content_parse_exception|[5:51] [bool] failed to parse field [must]|`, `parsing_exception|[term] query does not support [foo]|5:51`},
		root:  `parsing_exception|[term] query does not support [foo]|5:51`},
	{name: "query multibyte key", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"query":{"match":{"日本":{"query":"x","foo":1}}}}`,
		chain: []string{`parsing_exception|[match] query does not support [foo]|1:48`},
		root:  `parsing_exception|[match] query does not support [foo]|1:48`},
	{name: "query crlf lines", method: "POST", path: "/r2pos-q/_search", status: 400,
		body: `{
"query": {
"term": {"author": "x", "body": "y"}}}`,
		chain: []string{`parsing_exception|[term] query doesn't support multiple fields, found [author] and [body]|3:33`},
		root:  `parsing_exception|[term] query doesn't support multiple fields, found [author] and [body]|3:33`},
	{name: "search unknown key", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"foo":1}`,
		chain: []string{`parsing_exception|Unknown key for a VALUE_NUMBER in [foo].|1:8`},
		root:  `parsing_exception|Unknown key for a VALUE_NUMBER in [foo].|1:8`},
	{name: "search after element", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"sort":[{"year":"asc"}],"search_after":[{"a":1}]}`,
		chain: []string{`parsing_exception|Expected [VALUE_STRING] or [VALUE_NUMBER] or [VALUE_BOOLEAN] or [VALUE_NULL] but found [START_OBJECT] inside search_after.|1:42`},
		root:  `parsing_exception|Expected [VALUE_STRING] or [VALUE_NUMBER] or [VALUE_BOOLEAN] or [VALUE_NULL] but found [START_OBJECT] inside search_after.|1:42`},
	{name: "search sort option", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"sort":[{"year":{"order":"asc","format":"yyyy"}}]}`,
		chain: []string{`x_content_parse_exception|[1:33] [field_sort] unknown field [format]|`},
		root:  `x_content_parse_exception|[1:33] [field_sort] unknown field [format]|`},
	{name: "search collapse inner hits", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"collapse":{"field":"author","inner_hits":{"name":"g","foo":1}}}`,
		chain: []string{`x_content_parse_exception|[1:62] [collapse] failed to parse field [inner_hits]|`, `x_content_parse_exception|[1:56] [inner_hits] unknown field [foo]|`},
		root:  `x_content_parse_exception|[1:56] [inner_hits] unknown field [foo]|`},
	{name: "search pit", method: "POST", path: "/_search", status: 400,
		body:  `{"pit":{"foo":1}}`,
		chain: []string{`x_content_parse_exception|[1:9] [pit] unknown field [foo]|`},
		root:  `x_content_parse_exception|[1:9] [pit] unknown field [foo]|`},
	{name: "search slice", method: "POST", path: "/r2pos-q/_search?scroll=1m", status: 400,
		body:  `{"slice":{"id":2,"max":2}}`,
		chain: []string{`x_content_parse_exception|[1:24] [slice] failed to parse field [max]|`, `illegal_argument_exception|max must be greater than id|`},
		root:  `x_content_parse_exception|[1:24] [slice] failed to parse field [max]|`},
	{name: "search rescore", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"rescore":{"window_size":10,"foo":{}}}`,
		chain: []string{`named_object_not_found_exception|[1:36] unknown field [foo]|`},
		root:  `named_object_not_found_exception|[1:36] unknown field [foo]|`},
	{name: "search indices boost", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"indices_boost":[{"r2pos-q":"abc"}]}`,
		chain: []string{`parsing_exception|Expected [VALUE_NUMBER] in [indices_boost] but found [VALUE_STRING]|1:30`},
		root:  `parsing_exception|Expected [VALUE_NUMBER] in [indices_boost] but found [VALUE_STRING]|1:30`},
	{name: "search highlight field", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"highlight":{"fields":{"body":{"fragment_size":"abc"}}}}`,
		chain: []string{`x_content_parse_exception|[1:49] [highlight] failed to parse field [fields]|`, `x_content_parse_exception|[1:49] [fields] failed to parse field [body]|`, `x_content_parse_exception|[1:49] [highlight_field] failed to parse field [fragment_size]|`, `number_format_exception|For input string: "abc"|`},
		root:  `x_content_parse_exception|[1:49] [highlight_field] failed to parse field [fragment_size]|`},
	{name: "search highlight post tags", method: "POST", path: "/r2pos-q/_search", status: 400,
		body:  `{"highlight":{"pre_tags":["<x>"],"fields":{"body":{}}}}`,
		chain: []string{`parsing_exception|pre_tags are set but post_tags are not set|1:54`},
		root:  `parsing_exception|pre_tags are set but post_tags are not set|1:54`},
	{name: "count body key", method: "POST", path: "/r2pos-q/_count", status: 400,
		body:  `{"query":{"match_all":{}},"size":1}`,
		chain: []string{`parsing_exception|request does not support [size]|1:27`},
		root:  `parsing_exception|request does not support [size]|1:27`},
	{name: "msearch line", method: "POST", path: "/_msearch", status: 400,
		body: `{"index":"r2pos-q"}
{"size":0,"aggs":{"x":{"nope":{}}}}
`,
		chain: []string{`parsing_exception|Unknown aggregation type [nope]|1:31`, `named_object_not_found_exception|[1:31] unknown field [nope]|`},
		root:  `parsing_exception|Unknown aggregation type [nope]|1:31`},
	{name: "msearch empty line", method: "POST", path: "/r2pos-q/_msearch", status: 400,
		body: `{}

`,
		chain: []string{`parsing_exception|Expected [START_OBJECT] but found [null]|1:0`},
		root:  `parsing_exception|Expected [START_OBJECT] but found [null]|1:0`},
	{name: "mget key", method: "POST", path: "/r2pos-d/_mget", status: 400,
		body:  `{"ids":"1"}`,
		chain: []string{`parsing_exception|unexpected token [VALUE_STRING], expected [FIELD_NAME] or [START_ARRAY]|1:8`},
		root:  `parsing_exception|unexpected token [VALUE_STRING], expected [FIELD_NAME] or [START_ARRAY]|1:8`},
	{name: "update unknown field", method: "POST", path: "/r2pos-d/_update/1", status: 400,
		body:  `{"doc":{"a":2},"foo":1}`,
		chain: []string{`x_content_parse_exception|[1:16] [UpdateRequest] unknown field [foo]|`},
		root:  `x_content_parse_exception|[1:16] [UpdateRequest] unknown field [foo]|`},
	{name: "aliases action", method: "POST", path: "/_aliases", status: 400,
		body:  `{"actions":[{"add":{"index":"r2pos-a","alias":"r2pos-z","foo":1}}]}`,
		chain: []string{`x_content_parse_exception|[1:63] [aliases] failed to parse field [actions]|`, `x_content_parse_exception|[1:63] [alias_action] failed to parse field [add]|`, `x_content_parse_exception|[1:57] [add] unknown field [foo]|`},
		root:  `x_content_parse_exception|[1:57] [add] unknown field [foo]|`},
	{name: "aliases filter", method: "POST", path: "/_aliases", status: 400,
		body:  `{"actions":[{"add":{"index":"r2pos-a","alias":"r2pos-f","filter":{"term":{"f":{"bogus":1}}}}}]}`,
		chain: []string{`illegal_argument_exception|failed to parse filter for alias [r2pos-f]|`, `parsing_exception|[term] query does not support [bogus]|1:23`},
		root:  `illegal_argument_exception|failed to parse filter for alias [r2pos-f]|`},
	{name: "index template section", method: "PUT", path: "/_index_template/r2pos-t", status: 400,
		body:  `{"index_patterns":["r2pos-t-*"],"template":{"foo":{}}}`,
		chain: []string{`x_content_parse_exception|[1:51] [index_template] failed to parse field [template]|`, `x_content_parse_exception|[1:45] [template] unknown field [foo]|`},
		root:  `x_content_parse_exception|[1:45] [template] unknown field [foo]|`},
	{name: "component template field", method: "PUT", path: "/_component_template/r2pos-c", status: 400,
		body:  `{"version":"x","template":{}}`,
		chain: []string{`x_content_parse_exception|[1:12] [component_template] failed to parse field [version]|`, `illegal_argument_exception|For input string: "x"|`},
		root:  `x_content_parse_exception|[1:12] [component_template] failed to parse field [version]|`},
	{name: "reindex dest", method: "POST", path: "/_reindex", status: 400,
		body:  `{"source":{"index":"r2pos-d"},"dest":{"index":"r2pos-x","op_type":"bogus"}}`,
		chain: []string{`x_content_parse_exception|[1:67] [reindex] failed to parse field [dest]|`, `x_content_parse_exception|[1:67] [dest] failed to parse field [op_type]|`, `illegal_argument_exception|opType must be 'create' or 'index', found: [bogus]|`},
		root:  `x_content_parse_exception|[1:67] [dest] failed to parse field [op_type]|`},
	{name: "reindex source", method: "POST", path: "/_reindex", status: 400,
		body:  `{"source":{"index":"r2pos-d","foo":1},"dest":{"index":"r2pos-x"}}`,
		chain: []string{`x_content_parse_exception|[1:37] [reindex] failed to parse field [source]|`, `parsing_exception|Unknown key for a VALUE_NUMBER in [foo].|1:8`},
		root:  `parsing_exception|Unknown key for a VALUE_NUMBER in [foo].|1:8`},
	{name: "delete by query body", method: "POST", path: "/r2pos-d/_delete_by_query", status: 400,
		body:  `{"conflicts":"proceed","query":{"match_all":{}},"foo":1}`,
		chain: []string{`parsing_exception|Unknown key for a VALUE_NUMBER in [foo].|1:33`},
		root:  `parsing_exception|Unknown key for a VALUE_NUMBER in [foo].|1:33`},
	{name: "update by query body", method: "POST", path: "/r2pos-d/_update_by_query", status: 400,
		body: `{
  "query": {"match": {"a": {"query": "x", "foo": 1}}}
}`,
		chain: []string{`parsing_exception|[match] query does not support [foo]|1:43`},
		root:  `parsing_exception|[match] query does not support [foo]|1:43`},
	{name: "document text", method: "PUT", path: "/r2pos-a/_doc/2", status: 400,
		body:  `{"f": {"a": 1}}`,
		chain: []string{`mapper_parsing_exception|failed to parse field [f] of type [keyword] in document with id '2'. Preview of field's value: '{a=1}'|`, `illegal_state_exception|Can't get text on a START_OBJECT at 1:7|`},
		root:  `mapper_parsing_exception|failed to parse field [f] of type [keyword] in document with id '2'. Preview of field's value: '{a=1}'|`},
}
