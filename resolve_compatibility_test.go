package osmem

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// The expectations of this file were recorded from OpenSearch 3.8.0.

func jsonAt(t *testing.T, body map[string]any, pointer string) any {
	t.Helper()
	v, ok := compatibilityPointer(body, pointer)
	if !ok {
		t.Fatalf("%s missing in %v", pointer, body)
	}
	return v
}

func expectStatus(t *testing.T, c *Cluster, method, path string, body any, want int) map[string]any {
	t.Helper()
	code, res := status(t, c, method, path, body)
	if code != want {
		t.Fatalf("%s %s = %d %v, want %d", method, path, code, res, want)
	}
	return res
}

func expectRequestError(t *testing.T, c *Cluster, method, path string, body any, want int, errorType, rootReason string) map[string]any {
	t.Helper()
	res := expectStatus(t, c, method, path, body, want)
	if got := errType(res); got != errorType {
		t.Fatalf("%s %s error type = %q, want %q: %v", method, path, got, errorType, res)
	}
	if rootReason != "" {
		if got := jsonAt(t, res, "/error/root_cause/0/reason"); got != rootReason {
			t.Fatalf("%s %s root cause = %q, want %q", method, path, got, rootReason)
		}
	}
	return res
}

func doText(t *testing.T, c *Cluster, method, path string) string {
	t.Helper()
	res, err := c.Do(method, path, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return string(res.Body)
}

func headStatus(t *testing.T, c *Cluster, path string) int {
	t.Helper()
	res, err := c.Do(http.MethodHead, path, nil)
	if err != nil {
		t.Fatalf("HEAD %s: %v", path, err)
	}
	return res.StatusCode
}

func seedLogs(t *testing.T) *Cluster {
	t.Helper()
	c := New()
	mustDo(t, c, http.MethodPut, "/logs-1", `{"mappings":{"properties":{"author":{"type":"keyword"},"n":{"type":"integer"}}}}`)
	if err := c.BulkString(`{"index":{"_index":"logs-1","_id":"1"}}
{"author":"alice","n":1}
{"index":{"_index":"logs-1","_id":"2"}}
{"author":"bob","n":2}
{"index":{"_index":"logs-1","_id":"3"}}
{"author":"alice","n":3}
{"index":{"_index":"logs-1","_id":"4"}}
{"author":"carol","n":4}
{"index":{"_index":"logs-1","_id":"5"}}
{"author":"dave","n":5}
`); err != nil {
		t.Fatal(err)
	}
	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions":[
	  {"add":{"index":"logs-1","alias":"logs"}},
	  {"add":{"index":"logs-1","alias":"alice","filter":{"term":{"author":"alice"}}}},
	  {"add":{"index":"logs-1","alias":"bob","filter":{"term":{"author":"bob"}}}}]}`)
	return c
}

func TestIndexExpressionReachesEachIndexOnce(t *testing.T) {
	c := seedLogs(t)
	defer c.Close()
	sorted := `{"size":10,"sort":["_id"],"_source":false}`
	ids, res := mustDoIDs(t, c, "/logs*/_search", sorted)
	assertIDs(t, ids, "1", "2", "3", "4", "5")
	if total := jsonAt(t, res, "/_shards/total"); total != 1.0 {
		t.Fatalf("an index matched as index and alias counts once: _shards.total %v", total)
	}
	if count := mustDo(t, c, http.MethodPost, "/logs*/_count", nil)["count"]; count != 5.0 {
		t.Fatalf("count through index and alias names = %v, want 5", count)
	}
	// naming the index removes the filter of its alias
	ids, _ = mustDoIDs(t, c, "/logs-1,alice/_search", sorted)
	assertIDs(t, ids, "1", "2", "3", "4", "5")
	ids, _ = mustDoIDs(t, c, "/alice/_search", sorted)
	assertIDs(t, ids, "1", "3")
	// the filters of several aliases of one index are combined with OR
	ids, _ = mustDoIDs(t, c, "/alice,bob/_search", sorted)
	assertIDs(t, ids, "1", "2", "3")
	// a non-filtering alias of the index removes the filters as well
	ids, _ = mustDoIDs(t, c, "/logs,alice/_search", sorted)
	assertIDs(t, ids, "1", "2", "3", "4", "5")
}

func TestIndexExpressionExclusionsAndMissingNames(t *testing.T) {
	c := seedLogs(t)
	defer c.Close()
	// "-name" excludes only after a wildcard; before, it is a name
	res := expectRequestError(t, c, http.MethodPost, "/logs-1,-nope/_search", nil, 404, "index_not_found_exception", "no such index [-nope]")
	if jsonAt(t, res, "/error/resource.type") != "index_or_alias" {
		t.Fatalf("resource.type: %v", res)
	}
	expectRequestError(t, c, http.MethodPost, "/logs-1,-logs-1/_search", nil, 404, "index_not_found_exception", "no such index [-logs-1]")
	expectRequestError(t, c, http.MethodPost, "/-logs-1,logs*/_search", nil, 404, "index_not_found_exception", "no such index [-logs-1]")
	// an excluded name must exist unless unavailable indices are ignored
	expectRequestError(t, c, http.MethodPost, "/logs*,-nope/_search", nil, 404, "index_not_found_exception", "no such index [nope]")
	expectStatus(t, c, http.MethodPost, "/logs*,-nope/_search?ignore_unavailable=true", nil, 200)
	res = expectStatus(t, c, http.MethodPost, "/logs*,-logs-1/_search", nil, 200)
	if jsonAt(t, res, "/_shards/total") != 0.0 {
		t.Fatalf("wildcard minus its only index: %v", res)
	}
	expectRequestError(t, c, http.MethodPost, "/logs-1,nope*/_search?allow_no_indices=false", nil, 404, "index_not_found_exception", "no such index [nope*]")
	expectRequestError(t, c, http.MethodPost, "/logs-1,_all/_search", nil, 400, "invalid_index_name_exception", "Invalid index name [_all], must not start with '_'.")
	expectRequestError(t, c, http.MethodPost, "/logs-1,,logs-1/_search", nil, 404, "index_not_found_exception", "no such index []")
	// expand_wildcards=none leaves names unresolved; a missing single name is allowed
	res = expectStatus(t, c, http.MethodPost, "/nope/_search?expand_wildcards=none", nil, 200)
	if jsonAt(t, res, "/_shards/total") != 0.0 {
		t.Fatalf("expand_wildcards=none: %v", res)
	}
	res = expectRequestError(t, c, http.MethodPost, "/nope/_search?ignore_unavailable=true&allow_no_indices=false", nil, 404, "index_not_found_exception", "no such index [nope]")
	if jsonAt(t, res, "/error/resource.type") != "index_expression" {
		t.Fatalf("resource.type: %v", res)
	}
	expectRequestError(t, c, http.MethodPost, "/logs-1/_search?expand_wildcards=foo", nil, 400, "illegal_argument_exception", "No valid expand wildcard value [foo]")
	res = expectRequestError(t, c, http.MethodPost, "/logs-1/_search?ignore_unavailable=maybe", nil, 400, "illegal_argument_exception", "Could not convert [ignore_unavailable] to boolean")
	if jsonAt(t, res, "/error/caused_by/reason") != "Failed to parse value [maybe] as only [true] or [false] are allowed." {
		t.Fatalf("caused_by: %v", res)
	}
}

func TestIndexExpressionStatusCodes(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/x1", `{"aliases":{"xa":{}}}`)
	for path, want := range map[string]int{
		"/nomatch*": 200, "/x1,nope": 404, "/x1,nope?ignore_unavailable=true": 200, "/nomatch*?allow_no_indices=false": 404, "/xa": 200,
	} {
		if got := headStatus(t, c, path); got != want {
			t.Fatalf("HEAD %s = %d, want %d", path, got, want)
		}
	}
	expectStatus(t, c, http.MethodDelete, "/nope?ignore_unavailable=true", nil, 200)
	expectStatus(t, c, http.MethodDelete, "/nomatch*,-x1", nil, 200)
	if count := mustDo(t, c, http.MethodGet, "/nomatch*,-x1/_count", nil)["count"]; count != 0.0 {
		t.Fatalf("count = %v", count)
	}
	expectRequestError(t, c, http.MethodDelete, "/xa", nil, 400, "illegal_argument_exception", "The provided expression [xa] matches an alias, specify the corresponding concrete indices instead.")
	resolved := mustDo(t, c, http.MethodGet, "/_resolve/index/nope", nil)
	if len(resolved["indices"].([]any)) != 0 || len(resolved["aliases"].([]any)) != 0 {
		t.Fatalf("missing names resolve to nothing: %v", resolved)
	}
	resolved = mustDo(t, c, http.MethodGet, "/_resolve/index/x1,nope", nil)
	if got := jsonAt(t, resolved, "/indices/0/name"); got != "x1" || len(resolved["indices"].([]any)) != 1 {
		t.Fatalf("partial resolution: %v", resolved)
	}
	expectRequestError(t, c, http.MethodGet, "/_resolve/index/x*?expand_wildcards=bogus", nil, 400, "illegal_argument_exception", "No valid expand wildcard value [bogus]")
}

func TestHiddenIndicesInWildcards(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/h1", `{"settings":{"index.hidden":true,"number_of_replicas":0}}`)
	mustDo(t, c, http.MethodPut, "/h2", `{"settings":{"number_of_replicas":0}}`)
	mustDo(t, c, http.MethodPut, "/h1/_doc/1?refresh=true", `{"a":1}`)
	mustDo(t, c, http.MethodPut, "/h2/_doc/1?refresh=true", `{"a":1}`)
	for path, want := range map[string]float64{
		"/h*/_count": 1, "/h*/_count?expand_wildcards=all": 2, "/h*/_count?expand_wildcards=open,hidden": 2, "/h*/_count?expand_wildcards=none": 0,
		"/h*/_count?expand_wildcards=closed": 0, "/h1/_count": 1,
	} {
		if got := mustDo(t, c, http.MethodGet, path, nil)["count"]; got != want {
			t.Fatalf("GET %s count = %v, want %v", path, got, want)
		}
	}
	if got := doText(t, c, http.MethodGet, "/_cat/indices/h*?h=index"); got != "h2\n" {
		t.Fatalf("_cat/indices lists visible indices only: %q", got)
	}
	if mapping := mustDo(t, c, http.MethodGet, "/h*/_mapping", nil); !reflect.DeepEqual(keysOf(mapping), []string{"h2"}) {
		t.Fatalf("wildcard mappings: %v", mapping)
	}
	if settings := mustDo(t, c, http.MethodGet, "/h*/_settings/index.hidden?expand_wildcards=all", nil); !reflect.DeepEqual(keysOf(settings), []string{"h1"}) {
		t.Fatalf("indices without a matching setting are left out: %v", settings)
	}
	// a visible alias reaches the hidden index; a hidden alias needs hidden
	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"h1","alias":"visible-alias"}},{"add":{"index":"h2","alias":"hidden-alias","is_hidden":true}}]}`)
	if got := mustDo(t, c, http.MethodGet, "/visible-*/_count", nil)["count"]; got != 1.0 {
		t.Fatalf("visible alias of a hidden index: %v", got)
	}
	if got := mustDo(t, c, http.MethodGet, "/hidden-*/_count", nil)["count"]; got != 0.0 {
		t.Fatalf("hidden alias through a wildcard: %v", got)
	}
	if got := mustDo(t, c, http.MethodGet, "/hidden-*/_count?expand_wildcards=open,hidden", nil)["count"]; got != 1.0 {
		t.Fatalf("hidden alias with expand_wildcards=hidden: %v", got)
	}
	if got := jsonAt(t, mustDo(t, c, http.MethodGet, "/h2/_alias", nil), "/h2/aliases/hidden-alias/is_hidden"); got != true {
		t.Fatalf("is_hidden is reported: %v", got)
	}
	// hidden indices starting with a dot match dot patterns
	mustDo(t, c, http.MethodPut, "/.dot-hidden", `{"settings":{"index.hidden":true}}`)
	if got := jsonAt(t, mustDo(t, c, http.MethodGet, "/.dot*/_count", nil), "/_shards/total"); got != 1.0 {
		t.Fatalf("dot pattern includes the hidden dot index: %v", got)
	}
	if got := jsonAt(t, mustDo(t, c, http.MethodGet, "/*dot-hidden/_count", nil), "/_shards/total"); got != 0.0 {
		t.Fatalf("a pattern without a leading dot excludes it: %v", got)
	}
	mustDo(t, c, http.MethodDelete, "/h*", nil)
	if headStatus(t, c, "/h1") != 200 || headStatus(t, c, "/h2") != 404 {
		t.Fatalf("DELETE /h* removes visible indices only")
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	if len(keys) > 1 {
		for i := 1; i < len(keys); i++ {
			for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
				keys[j], keys[j-1] = keys[j-1], keys[j]
			}
		}
	}
	return keys
}

func TestClosedIndexLifecycle(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/c1", `{"mappings":{"properties":{"f":{"type":"keyword"}}}}`)
	mustDo(t, c, http.MethodPut, "/c1/_doc/1?refresh=true", `{"f":"x"}`)
	closed := mustDo(t, c, http.MethodPost, "/c1/_close", nil)
	assertJSON(t, closed, `{"acknowledged":true,"shards_acknowledged":true,"indices":{"c1":{"closed":true}}}`)
	assertJSON(t, mustDo(t, c, http.MethodPost, "/c1/_close", nil), `{"acknowledged":true,"shards_acknowledged":false,"indices":{}}`)

	res := expectRequestError(t, c, http.MethodPost, "/c1/_search", nil, 400, "index_closed_exception", "closed")
	if jsonAt(t, res, "/error/index") != "c1" {
		t.Fatalf("index metadata: %v", res)
	}
	if got := jsonAt(t, mustDo(t, c, http.MethodPost, "/c*/_search", nil), "/_shards/total"); got != 0.0 {
		t.Fatalf("wildcards skip closed indices: %v", got)
	}
	expectRequestError(t, c, http.MethodPost, "/c*/_search?expand_wildcards=closed", nil, 400, "illegal_argument_exception", "To expand [CLOSE] wildcard, please set forbid_closed_indices to `false`")
	expectRequestError(t, c, http.MethodPost, "/c*/_search?expand_wildcards=all", nil, 400, "index_closed_exception", "closed")
	if got := jsonAt(t, mustDo(t, c, http.MethodPost, "/c1/_search?ignore_unavailable=true", nil), "/_shards/total"); got != 0.0 {
		t.Fatalf("ignore_unavailable skips the closed index: %v", got)
	}
	res = expectRequestError(t, c, http.MethodPost, "/c1/_search?ignore_unavailable=true&allow_no_indices=false", nil, 404, "index_not_found_exception", "no such index [null]")
	if jsonAt(t, res, "/error/resource.id") != "c1" || jsonAt(t, res, "/error/resource.type") != "index_expression" {
		t.Fatalf("resources: %v", res)
	}
	for _, req := range []struct{ method, path, body string }{
		{http.MethodPut, "/c1/_doc/2", `{"f":"y"}`}, {http.MethodGet, "/c1/_doc/1", ""}, {http.MethodPost, "/c1/_count", ""},
		{http.MethodPost, "/c1/_refresh", ""}, {http.MethodPost, "/c1/_update/1", `{"doc":{"f":"z"}}`}, {http.MethodDelete, "/c1/_doc/1", ""},
		{http.MethodGet, "/c1/_stats", ""}, {http.MethodGet, "/c1/_source/1", ""},
	} {
		var body any
		if req.body != "" {
			body = req.body
		}
		expectRequestError(t, c, req.method, req.path, body, 400, "index_closed_exception", "closed")
	}
	if got := headStatus(t, c, "/c1/_doc/1"); got != 400 {
		t.Fatalf("HEAD a document of a closed index = %d", got)
	}
	bulk := mustDo(t, c, http.MethodPost, "/_bulk", "{\"index\":{\"_index\":\"c1\",\"_id\":\"3\"}}\n{\"f\":\"x\"}\n")
	if jsonAt(t, bulk, "/items/0/index/error/type") != "index_closed_exception" || jsonAt(t, bulk, "/items/0/index/status") != 400.0 {
		t.Fatalf("bulk item on a closed index: %v", bulk)
	}
	mget := mustDo(t, c, http.MethodPost, "/_mget", `{"docs":[{"_index":"c1","_id":"1"}]}`)
	if jsonAt(t, mget, "/docs/0/error/type") != "index_closed_exception" {
		t.Fatalf("mget of a closed index: %v", mget)
	}
	if got := jsonAt(t, mustDo(t, c, http.MethodGet, "/c1/_settings", nil), "/c1/settings/index/verified_before_close"); got != "true" {
		t.Fatalf("verified_before_close: %v", got)
	}
	mustDo(t, c, http.MethodGet, "/c1/_mapping", nil)
	if got := doText(t, c, http.MethodGet, "/_cat/indices/c1?h=index,status,health"); got != "c1 close red\n" {
		t.Fatalf("_cat/indices: %q", got)
	}
	if got := doText(t, c, http.MethodGet, "/_cat/indices/c*?h=index,status&expand_wildcards=open"); got != "" {
		t.Fatalf("_cat/indices with expand_wildcards=open: %q", got)
	}
	health := mustDo(t, c, http.MethodGet, "/_cluster/health/c1", nil)
	if health["status"] != "red" || health["active_primary_shards"] != 0.0 || health["unassigned_shards"] != 2.0 {
		t.Fatalf("health of a closed index: %v", health)
	}
	if got := jsonAt(t, mustDo(t, c, http.MethodGet, "/_resolve/index/c1", nil), "/indices/0/attributes/0"); got != "closed" {
		t.Fatalf("resolve attributes: %v", got)
	}
	mustDo(t, c, http.MethodPut, "/c1/_settings", `{"analysis":{"analyzer":{"my":{"type":"standard"}}}}`)
	caps := mustDo(t, c, http.MethodGet, "/c1/_field_caps?fields=*", nil)
	if len(caps["indices"].([]any)) != 0 {
		t.Fatalf("field caps of a closed index: %v", caps)
	}
	validate := mustDo(t, c, http.MethodPost, "/c1/_validate/query", nil)
	assertJSON(t, validate, `{"_shards":{"total":0,"successful":0,"failed":0},"valid":false,"explanations":[{"valid":false,"error":"index [c1] blocked by: [FORBIDDEN/4/index closed];"}]}`)
	expectRequestError(t, c, http.MethodPost, "/c1/_close?wait_for_active_shards=index-setting", nil, 400, "illegal_argument_exception", "cannot parse ActiveShardCount[index-setting]")

	assertJSON(t, mustDo(t, c, http.MethodPost, "/c*/_open", nil), `{"acknowledged":true,"shards_acknowledged":true}`)
	if got := mustDo(t, c, http.MethodGet, "/c1/_count", nil)["count"]; got != 1.0 {
		t.Fatalf("count after reopening = %v", got)
	}
	if settings := mustDo(t, c, http.MethodGet, "/c1/_settings", nil); strings.Contains(stringify(settings), "verified_before_close") {
		t.Fatalf("verified_before_close is removed when the index opens: %v", settings)
	}
	if analyzed := mustDo(t, c, http.MethodPost, "/c1/_analyze", `{"analyzer":"my","text":"Quick Fox"}`); len(analyzed["tokens"].([]any)) != 2 {
		t.Fatalf("analysis settings changed while closed apply after opening: %v", analyzed)
	}
	expectRequestError(t, c, http.MethodPost, "/nope/_open", nil, 404, "index_not_found_exception", "no such index [nope]")
	assertJSON(t, mustDo(t, c, http.MethodPost, "/nope*/_close", nil), `{"acknowledged":true,"shards_acknowledged":false,"indices":{}}`)
}

func stringify(v any) string {
	var sb strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, e := range t {
				sb.WriteString(k)
				sb.WriteByte(' ')
				walk(e)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return sb.String()
}
