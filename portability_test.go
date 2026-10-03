package osmem

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// doStatus runs a request and returns its status and decoded body (nil for
// an empty body).
func portDo(t *testing.T, c *Cluster, method, path string, body any) (int, map[string]any) {
	t.Helper()
	res, err := c.Do(method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Body) == 0 {
		return res.StatusCode, nil
	}
	var m map[string]any
	if err := json.Unmarshal(res.Body, &m); err != nil {
		t.Fatalf("%s %s: %v: %s", method, path, err, res.Body)
	}
	return res.StatusCode, m
}

// expectError asserts a 400 whose error has the given type and reason.
func expectPortError(t *testing.T, c *Cluster, method, path string, body any, typ, reason string) {
	t.Helper()
	status, m := portDo(t, c, method, path, body)
	if status != http.StatusBadRequest {
		t.Fatalf("%s %s: status %d, want 400: %v", method, path, status, m)
	}
	if got := jsonAt(t, m, "/error/type"); got != typ {
		t.Fatalf("%s %s: error type %v, want %s", method, path, got, typ)
	}
	if got := jsonAt(t, m, "/error/reason"); got != reason {
		t.Fatalf("%s %s: reason %q, want %q", method, path, got, reason)
	}
}

func TestDotPrefixedIndexNamesRejected(t *testing.T) {
	c := New()
	defer c.Close()
	const reason = "Invalid index name [.foo], must not start with '.'"
	expectPortError(t, c, http.MethodPut, "/.foo", nil, "invalid_index_name_exception", reason)
	expectPortError(t, c, http.MethodPut, "/.foo", `{"settings":{"index.hidden":true}}`, "invalid_index_name_exception", reason)
	expectPortError(t, c, http.MethodPost, "/.foo/_doc", `{"a":1}`, "invalid_index_name_exception", reason)
	if headStatus(t, c, "/.foo") != http.StatusNotFound {
		t.Fatal("a rejected write must not auto-create the index")
	}
	// bulk: only the item for the dot index fails
	res := mustDo(t, c, http.MethodPost, "/_bulk", "{\"index\":{\"_index\":\".foo\"}}\n{\"a\":1}\n{\"index\":{\"_index\":\"bar\"}}\n{\"a\":1}\n")
	if got := jsonAt(t, res, "/items/0/index/error/reason"); got != reason {
		t.Fatalf("bulk item for .foo: %v", got)
	}
	if got := jsonAt(t, res, "/items/1/index/status"); got != 201.0 {
		t.Fatalf("bulk item for bar: %v", got)
	}
	// the OpenSearch rules still come first
	expectPortError(t, c, http.MethodPut, "/_foo", nil, "invalid_index_name_exception", "Invalid index name [_foo], must not start with '_', '-', or '+'")
	expectPortError(t, c, http.MethodPut, "/..", nil, "invalid_index_name_exception", "Invalid index name [..], must not be '.' or '..'")
	// reading a dot name keeps the OpenSearch answer
	if status, _ := portDo(t, c, http.MethodGet, "/.foo/_search", nil); status != http.StatusNotFound {
		t.Fatalf("search on a missing dot index: %d", status)
	}
}

func TestAliasNamePortability(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/idx", nil)
	expectPortError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"idx","alias":".a"}}]}`,
		"invalid_alias_name_exception", "Invalid alias name [.a]: must not start with '.'")
	expectPortError(t, c, http.MethodPut, "/idx/_alias/MyAlias", nil,
		"invalid_alias_name_exception", "Invalid alias name [MyAlias]: must be lowercase")
	expectPortError(t, c, http.MethodPut, "/idx2", `{"aliases":{"Upper":{}}}`,
		"invalid_alias_name_exception", "Invalid alias name [Upper]: must be lowercase")
	if headStatus(t, c, "/idx2") != http.StatusNotFound {
		t.Fatal("index with a rejected alias must not be created")
	}
	// template aliases are checked once {index} is replaced
	mustDo(t, c, http.MethodPut, "/_index_template/t", `{"index_patterns":["tpl-*"],"template":{"aliases":{".{index}-alias":{}}}}`)
	expectPortError(t, c, http.MethodPut, "/tpl-1", nil,
		"invalid_alias_name_exception", "Invalid alias name [.tpl-1-alias]: must not start with '.'")
}

func TestWithDotNames(t *testing.T) {
	c := New(WithDotNames())
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/.kibana", nil)
	mustDo(t, c, http.MethodPost, "/.kibana/_doc/1", `{"a":1}`)
	mustDo(t, c, http.MethodPut, "/.kibana/_alias/.kibana_alias", nil)
	// lowercase aliases are still required
	expectPortError(t, c, http.MethodPut, "/.kibana/_alias/Kibana", nil,
		"invalid_alias_name_exception", "Invalid alias name [Kibana]: must be lowercase")
	clone := c.Clone()
	defer clone.Close()
	mustDo(t, clone, http.MethodPut, "/.other", nil)
}

func TestNonPortableNameWarns(t *testing.T) {
	var warnings []string
	c := New(WithWarnings(func(msg string) { warnings = append(warnings, msg) }))
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/"+"%E6%97%A5%E6%9C%AC", nil)
	mustDo(t, c, http.MethodPut, "/a@b", nil)
	if len(warnings) != 2 || !strings.Contains(warnings[0], "not portable to OpenSearch Serverless") {
		t.Fatalf("warnings: %q", warnings)
	}
}

func TestServerlessNamesAndSettings(t *testing.T) {
	var warnings []string
	c := New(WithServerless(ServerlessSearch), WithWarnings(func(msg string) { warnings = append(warnings, msg) }))
	defer c.Close()
	const charset = "must contain only [a-z0-9+.~=_;&$%-] and must start with [a-z0-9;&$%]"
	expectPortError(t, c, http.MethodPut, "/"+"%E6%97%A5%E6%9C%AC", nil, "invalid_index_name_exception", "Invalid index name [日本], "+charset)
	expectPortError(t, c, http.MethodPut, "/a@b", nil, "invalid_index_name_exception", "Invalid index name [a@b], "+charset)
	expectPortError(t, c, http.MethodPut, "/~a", nil, "invalid_index_name_exception", "Invalid index name [~a], "+charset)
	mustDo(t, c, http.MethodPut, "/logs-2026.10.03", `{"settings":{"number_of_shards":2,"index":{"refresh_interval":"1s"}}}`)
	expectPortError(t, c, http.MethodPut, "/logs-2026.10.03/_alias/a@b", nil, "invalid_alias_name_exception", "Invalid alias name [a@b]: "+charset)
	mustDo(t, c, http.MethodPut, "/logs-2026.10.03/_settings", `{"index.number_of_replicas":0}`)
	if len(warnings) != 3 {
		t.Fatalf("warnings: %q", warnings)
	}
	for i, k := range []string{"number_of_shards", "refresh_interval", "number_of_replicas"} {
		if !strings.Contains(warnings[i], "[index."+k+"] is managed by OpenSearch Serverless") {
			t.Fatalf("warning %d: %q", i, warnings[i])
		}
	}
	// a dot name stays rejected; WithDotNames lifts only the dot rule
	expectPortError(t, c, http.MethodPut, "/.x", nil, "invalid_index_name_exception", "Invalid index name [.x], must not start with '.'")
	d := New(WithServerless(ServerlessSearch), WithDotNames())
	defer d.Close()
	mustDo(t, d, http.MethodPut, "/.x", nil)
}

func TestServerlessAPISurface(t *testing.T) {
	c := New(WithServerless(ServerlessSearch))
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/idx", nil)
	for _, req := range [][2]string{
		{http.MethodGet, "/"},
		{http.MethodPost, "/idx/_refresh"},
		{http.MethodPost, "/_refresh"},
		{http.MethodPost, "/idx/_delete_by_query"},
		{http.MethodPost, "/idx/_update_by_query"},
		{http.MethodPost, "/_reindex"},
		{http.MethodPost, "/_search/scroll"},
		{http.MethodGet, "/_cluster/health"},
		{http.MethodGet, "/_nodes"},
		{http.MethodGet, "/_cat/health"},
		{http.MethodGet, "/_cat/shards"},
		{http.MethodPost, "/idx/_close"},
		{http.MethodGet, "/idx/_stats"},
		{http.MethodGet, "/_template"},
		{http.MethodGet, "/idx/_termvectors/1"},
	} {
		status, body := portDo(t, c, req[0], req[1], nil)
		if status != http.StatusNotFound || body != nil {
			t.Fatalf("%s %s: %d %v, want 404 with an empty body", req[0], req[1], status, body)
		}
	}
	// supported operations keep working
	mustDo(t, c, http.MethodPut, "/idx/_doc/1", `{"a":1}`)
	mustDo(t, c, http.MethodPost, "/idx/_update/1", `{"doc":{"a":2}}`)
	mustDo(t, c, http.MethodPost, "/idx/_search", `{"query":{"match_all":{}}}`)
	mustDo(t, c, http.MethodGet, "/idx/_mapping", nil)
	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"idx","alias":"al"}}]}`)
	if res, _ := c.Do(http.MethodGet, "/_cat/aliases", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("_cat/aliases: %d", res.StatusCode)
	}
	mustDo(t, c, http.MethodPut, "/idx/_doc/2?refresh=false", `{"a":1}`)
	expectPortError(t, c, http.MethodPut, "/idx/_doc/3?refresh=true", `{"a":1}`, "status_exception", "true refresh policy is not supported.")
	expectPortError(t, c, http.MethodPost, "/_bulk?refresh", "{\"index\":{\"_index\":\"idx\"}}\n{\"a\":1}\n", "status_exception", "true refresh policy is not supported.")
	expectPortError(t, c, http.MethodDelete, "/idx/_doc/1?refresh=wait_for", nil, "status_exception", "wait_for refresh policy is not supported.")
	// _cat/indices has no health and status columns
	res, err := c.Do(http.MethodGet, "/_cat/indices?v", nil)
	if err != nil {
		t.Fatal(err)
	}
	if header := strings.Fields(strings.SplitN(string(res.Body), "\n", 2)[0]); header[0] != "index" {
		t.Fatalf("_cat/indices header: %v", header)
	}
	// a clone keeps the restrictions
	clone := c.Clone()
	defer clone.Close()
	if status, _ := portDo(t, clone, http.MethodPost, "/idx/_refresh", nil); status != http.StatusNotFound {
		t.Fatalf("clone _refresh: %d", status)
	}
}

func TestServerlessDocumentIDs(t *testing.T) {
	for _, typ := range []ServerlessCollection{ServerlessTimeSeries, ServerlessVectorSearch} {
		t.Run(string(typ), func(t *testing.T) {
			c := New(WithServerless(typ))
			defer c.Close()
			const indexReason = "Document ID is not supported in create/index operation request"
			expectPortError(t, c, http.MethodPut, "/idx/_doc/1", `{"a":1}`, "illegal_argument_exception", indexReason)
			expectPortError(t, c, http.MethodPut, "/idx/_create/1", `{"a":1}`, "illegal_argument_exception", indexReason)
			expectPortError(t, c, http.MethodPost, "/idx/_update/1", `{"doc":{"a":1}}`, "illegal_argument_exception", "Document ID is not supported in update operation request")
			doc := mustDo(t, c, http.MethodPost, "/idx/_doc", `{"a":1}`)
			res := mustDo(t, c, http.MethodPost, "/_bulk", "{\"index\":{\"_index\":\"idx\",\"_id\":\"x\"}}\n{\"a\":1}\n"+
				"{\"create\":{\"_index\":\"idx\"}}\n{\"a\":1}\n"+
				"{\"update\":{\"_index\":\"idx\",\"_id\":\"x\"}}\n{\"doc\":{\"a\":1}}\n"+
				"{\"delete\":{\"_index\":\"idx\",\"_id\":\""+doc["_id"].(string)+"\"}}\n")
			if got := jsonAt(t, res, "/items/0/index/error/reason"); got != indexReason {
				t.Fatalf("bulk index with _id: %v", got)
			}
			if got := jsonAt(t, res, "/items/1/create/status"); got != 201.0 {
				t.Fatalf("bulk create without _id: %v", got)
			}
			if got := jsonAt(t, res, "/items/2/update/status"); got != 400.0 {
				t.Fatalf("bulk update: %v", got)
			}
			if got := jsonAt(t, res, "/items/3/delete/result"); got != "deleted" {
				t.Fatalf("bulk delete: %v", got)
			}
		})
	}
	// search collections accept IDs
	c := New(WithServerless(ServerlessSearch))
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/idx/_doc/1", `{"a":1}`)
}

func TestParseServerlessCollection(t *testing.T) {
	if got, err := ParseServerlessCollection("TimeSeries"); err != nil || got != ServerlessTimeSeries {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := ParseServerlessCollection("logs"); err == nil {
		t.Fatal("unknown collection type accepted")
	}
}
