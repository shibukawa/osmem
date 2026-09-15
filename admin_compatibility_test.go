package osmem

import (
	"net/http"
	"strings"
	"testing"
)

// Expectations in this file were taken from OpenSearch 3.8.0 responses.

func rootCauseReason(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	roots, _ := e["root_cause"].([]any)
	if len(roots) == 0 {
		return ""
	}
	s, _ := roots[0].(map[string]any)["reason"].(string)
	return s
}

func expectAdminError(t *testing.T, c *Cluster, method, path string, body any, wantStatus int, wantType, wantReason string) map[string]any {
	t.Helper()
	st, res := status(t, c, method, path, body)
	if st != wantStatus || errType(res) != wantType || (wantReason != "" && errReason(res) != wantReason) {
		t.Fatalf("%s %s: status=%d type=%q reason=%q, want %d %q %q (body %v)", method, path, st, errType(res), errReason(res), wantStatus, wantType, wantReason, res)
	}
	return res
}

func TestBuiltinASCIIFoldingInNormalizerAndAnalyzer(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/fold-norm", `{"settings":{"analysis":{"normalizer":{"lc":{"type":"custom","filter":["lowercase","asciifolding"]}}}},"mappings":{"properties":{"k":{"type":"keyword","normalizer":"lc"}}}}`)
	mustDo(t, c, http.MethodPut, "/fold-an", `{"settings":{"analysis":{"analyzer":{"a":{"type":"custom","tokenizer":"standard","filter":["lowercase","asciifolding"]}}}},"mappings":{"properties":{"t":{"type":"text","analyzer":"a"}}}}`)
	mustDo(t, c, http.MethodPut, "/fold-norm/_doc/1?refresh=true", `{"k":"Café"}`)
	mustDo(t, c, http.MethodPut, "/fold-an/_doc/1?refresh=true", `{"t":"Café"}`)
	if n := hitsTotal(mustDo(t, c, http.MethodPost, "/fold-norm/_search", `{"query":{"term":{"k":"CAFE"}}}`)); n != 1 {
		t.Fatalf("normalizer with asciifolding: hits=%v", n)
	}
	if n := hitsTotal(mustDo(t, c, http.MethodPost, "/fold-an/_search", `{"query":{"match":{"t":"cafe"}}}`)); n != 1 {
		t.Fatalf("analyzer with asciifolding: hits=%v", n)
	}
}

func hitsTotal(res map[string]any) float64 {
	hits, _ := res["hits"].(map[string]any)
	total, _ := hits["total"].(map[string]any)
	n, _ := total["value"].(float64)
	return n
}

func TestWriteThroughAliasWithoutWriteIndex(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/wi-1", nil)
	mustDo(t, c, http.MethodPut, "/wi-2", nil)
	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"indices":["wi-1","wi-2"],"alias":"wi-al"}},{"add":{"index":"wi-1","alias":"wi-one","is_write_index":false}}]}`)
	noWrite := "no write index is defined for alias [wi-al]. The write index may be explicitly disabled using is_write_index=false or the alias points to multiple indices without one being designated as a write index"
	expectAdminError(t, c, http.MethodPut, "/wi-al/_doc/1", `{"a":1}`, 400, "illegal_argument_exception", noWrite)
	expectAdminError(t, c, http.MethodPost, "/wi-al/_doc", `{"a":1}`, 400, "illegal_argument_exception", noWrite)
	expectAdminError(t, c, http.MethodPut, "/wi-one/_doc/1", `{"a":1}`, 400, "illegal_argument_exception", strings.ReplaceAll(noWrite, "wi-al", "wi-one"))
	bulk := mustDo(t, c, http.MethodPost, "/_bulk", "{\"index\":{\"_index\":\"wi-al\",\"_id\":\"2\"}}\n{\"a\":2}\n")
	item := bulk["items"].([]any)[0].(map[string]any)["index"].(map[string]any)
	if item["status"].(float64) != 400 || item["error"].(map[string]any)["reason"] != noWrite {
		t.Fatalf("bulk through alias without write index: %v", item)
	}
	for _, name := range c.Indices() {
		if strings.HasPrefix(name, "wi-al") || name == "wi-one" {
			t.Fatalf("an index named after the alias was created: %v", c.Indices())
		}
	}
}

func TestUpdateAliasesValidation(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/av-1", `{"mappings":{"properties":{"f":{"type":"keyword"}}}}`)
	mustDo(t, c, http.MethodPut, "/av-2", `{"mappings":{"properties":{"f":{"type":"keyword"}}}}`)

	expectAdminError(t, c, http.MethodPost, "/_aliases", `{}`, 400, "illegal_argument_exception", "No action specified")
	expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[]}`, 400, "illegal_argument_exception", "No action specified")
	res := expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-1","alias":"z","foo":1}}]}`, 400, "x_content_parse_exception", "[1:54] [aliases] failed to parse field [actions]")
	if rootCauseReason(res) != "[1:48] [add] unknown field [foo]" {
		t.Fatalf("unknown action field root cause: %v", res)
	}
	res = expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-1","indices":["av-2"],"alias":"z"}}]}`, 400, "x_content_parse_exception", "")
	if rootCauseReason(res) != "[1:53] [add] failed to parse field [indices]" || !strings.Contains(strings.Join(causeReasons(res), "|"), "Only one of [index] and [indices] is supported") {
		t.Fatalf("index and indices: %v", res)
	}
	res = expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-1","alias":"z","is_write_index":"notbool"}}]}`, 400, "x_content_parse_exception", "")
	if rootCauseReason(res) != "[1:65] [add] failed to parse field [is_write_index]" {
		t.Fatalf("is_write_index notbool: %v", res)
	}
	expectAdminError(t, c, http.MethodPost, "/_aliases", `{"foo":1,"actions":[{"add":{"index":"av-1","alias":"z"}}]}`, 400, "x_content_parse_exception", "[1:2] [aliases] unknown field [foo]")
	expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-1","alias":"_under"}}]}`, 400, "invalid_alias_name_exception", "Invalid alias name [_under]: must not start with '_', '-', or '+'")
	expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-1","alias":"w*"}}]}`, 400, "invalid_alias_name_exception", `Invalid alias name [w*]: must not contain the following characters [ , ", *, \, <, |, ,, >, /, ?]`)
	expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-1","alias":"av-2"}}]}`, 400, "invalid_alias_name_exception", "Invalid alias name [av-2], an index exists with the same name as the alias")
	expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-1","alias":"badf","filter":{"no_such_query":{}}}}]}`, 400, "illegal_argument_exception", "failed to parse filter for alias [badf]")
	expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-nomatch*","alias":"z"}}]}`, 404, "index_not_found_exception", "no such index [av-nomatch*]")
	expectAdminError(t, c, http.MethodPut, "/av-1/_alias/p6", `{"foo":1}`, 400, "illegal_argument_exception", "unknown field [foo]")
	expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-1","alias":"r2","routing":"1,2"}}]}`, 400, "illegal_argument_exception", "alias [r2] has several index routing values associated with it")

	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-1","alias":"w","is_write_index":true}}]}`)
	res = expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-2","alias":"w","is_write_index":true}}]}`, 500, "illegal_state_exception", "")
	if !strings.HasPrefix(errReason(res), "alias [w] has more than one write index [") {
		t.Fatalf("second write index: %v", res)
	}
	expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"remove_index":{"index":"w"}}]}`, 400, "illegal_argument_exception", "The provided expression [w] matches an alias, specify the corresponding concrete indices instead.")

	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-1","alias":"hid","is_hidden":true}}]}`)
	res = expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"av-2","alias":"hid"}}]}`, 500, "illegal_state_exception", "")
	if !strings.HasSuffix(errReason(res), "alias must have the same is_hidden setting on all indices") {
		t.Fatalf("is_hidden mismatch: %v", res)
	}
	got := mustDo(t, c, http.MethodGet, "/av-1/_alias/hid", nil)
	if got["av-1"].(map[string]any)["aliases"].(map[string]any)["hid"].(map[string]any)["is_hidden"] != true {
		t.Fatalf("is_hidden is not returned: %v", got)
	}
}

func causeReasons(m map[string]any) []string {
	var out []string
	e, _ := m["error"].(map[string]any)
	for e != nil {
		if r, ok := e["reason"].(string); ok {
			out = append(out, r)
		}
		e, _ = e["caused_by"].(map[string]any)
	}
	return out
}

func TestUpdateAliasesAcceptsOpenSearchForms(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/aa-1", nil)
	mustDo(t, c, http.MethodPut, "/aa-2", nil)

	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions":[{"remove":{"index":"aa-1","alias":"nope","must_exist":false}}]}`)
	res := expectAdminError(t, c, http.MethodPost, "/_aliases", `{"actions":[{"remove":{"index":"aa-1","alias":"nope"}}]}`, 404, "aliases_not_found_exception", "aliases [nope] missing")
	if e := res["error"].(map[string]any); e["resource.type"] != "aliases" || e["resource.id"] != "nope" {
		t.Fatalf("aliases_not_found metadata: %v", res)
	}
	// the remove expands against the aliases that existed before the request
	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"aa-1","alias":"al"}},{"remove":{"index":"aa-1","alias":"al"}}]}`)
	mustDo(t, c, http.MethodGet, "/_alias/al", nil)
	// actions may be a single object
	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions":{"add":{"index":"aa-1","alias":"single"}}}`)
	mustDo(t, c, http.MethodPut, "/_alias/p4", `{"index":"aa-2"}`)
	mustDo(t, c, http.MethodPut, "/aa-2/_alias", `{"alias":"p5"}`)
	aliases := mustDo(t, c, http.MethodGet, "/aa-2/_alias", nil)["aa-2"].(map[string]any)["aliases"].(map[string]any)
	if _, ok := aliases["p4"]; !ok {
		t.Fatalf("PUT /_alias/{name} with index in body: %v", aliases)
	}
	if _, ok := aliases["p5"]; !ok {
		t.Fatalf("PUT /{index}/_alias with alias in body: %v", aliases)
	}
	if st, _ := status(t, c, http.MethodHead, "/aa-1/_alias", nil); st != 200 {
		t.Fatalf("HEAD /{index}/_alias: %d", st)
	}
	mustDo(t, c, http.MethodPut, "/aa-1/_alias/rt", `{"routing":"1"}`)
	rt := mustDo(t, c, http.MethodGet, "/aa-1/_alias/rt", nil)["aa-1"].(map[string]any)["aliases"].(map[string]any)["rt"].(map[string]any)
	if rt["index_routing"] != "1" || rt["search_routing"] != "1" || rt["routing"] != nil {
		t.Fatalf("routing is rendered as index_routing and search_routing: %v", rt)
	}
	all := mustDo(t, c, http.MethodGet, "/aa-1/_alias/_all", nil)["aa-1"].(map[string]any)["aliases"].(map[string]any)
	if len(all) != 3 {
		t.Fatalf("GET /{index}/_alias/_all: %v", all)
	}
	mustDo(t, c, http.MethodDelete, "/aa-1/_alias/_all", nil)
	if left := mustDo(t, c, http.MethodGet, "/aa-1/_alias", nil)["aa-1"].(map[string]any)["aliases"].(map[string]any); len(left) != 0 {
		t.Fatalf("DELETE /{index}/_alias/_all left %v", left)
	}
	expectAdminError(t, c, http.MethodDelete, "/aa-1/_alias/nope1,nope2", nil, 404, "aliases_not_found_exception", "")
	// legacy template aliases expand {index} and drop is_write_index
	mustDo(t, c, http.MethodPut, "/_template/leg", `{"index_patterns":["leg-*"],"aliases":{"{index}-alias":{},"leg-w":{"is_write_index":true}}}`)
	mustDo(t, c, http.MethodPut, "/leg-x", nil)
	legacy := mustDo(t, c, http.MethodGet, "/leg-x/_alias", nil)["leg-x"].(map[string]any)["aliases"].(map[string]any)
	if _, ok := legacy["leg-x-alias"]; !ok {
		t.Fatalf("{index} placeholder not expanded: %v", legacy)
	}
	if w := legacy["leg-w"].(map[string]any); len(w) != 0 {
		t.Fatalf("legacy template alias kept is_write_index: %v", w)
	}
}

func TestIndexSettingsValidation(t *testing.T) {
	c := New()
	defer c.Close()
	unknown := "unknown setting [index.foo] please check that any required plugins are installed, or check the breaking changes documentation for removed settings"
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index.foo":1}}`, 400, "settings_exception", unknown)
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index":{"number_of_shards":{"nested":1}}}}`, 400, "settings_exception", "unknown setting [index.number_of_shards.nested] did you mean [index.number_of_shards]?")
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"number_of_shards":0}}`, 400, "illegal_argument_exception", "Failed to parse value [0] for setting [index.number_of_shards] must be >= 1")
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"number_of_shards":2000}}`, 400, "illegal_argument_exception", "Failed to parse value [2000] for setting [index.number_of_shards] must be <= 1024")
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index.number_of_replicas":-1}}`, 400, "illegal_argument_exception", "Failed to parse value [-1] for setting [index.number_of_replicas] must be >= 0")
	res := expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index.max_result_window":"abc"}}`, 400, "illegal_argument_exception", "Failed to parse value [abc] for setting [index.max_result_window]")
	if cause := res["error"].(map[string]any)["caused_by"].(map[string]any); cause["type"] != "number_format_exception" || cause["reason"] != `For input string: "abc"` {
		t.Fatalf("number format cause: %v", res)
	}
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index.max_result_window":1.5e4}}`, 400, "illegal_argument_exception", "Failed to parse value [15000.0] for setting [index.max_result_window]")
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index.refresh_interval":"abc"}}`, 400, "illegal_argument_exception", "failed to parse setting [index.refresh_interval] with value [abc] as a time value: unit is missing or unrecognized")
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index.refresh_interval":"-5s"}}`, 400, "illegal_argument_exception", "failed to parse setting [index.refresh_interval] with value [-5s] as a time value: negative durations are not supported")
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index.translog.sync_interval":"10ms"}}`, 400, "illegal_argument_exception", "failed to parse value [10ms] for setting [index.translog.sync_interval], must be >= [100ms]")
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index.hidden":"notbool"}}`, 400, "illegal_argument_exception", "Failed to parse value [notbool] as only [true] or [false] are allowed.")
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index.number_of_shards":2,"index.number_of_routing_shards":3}}`, 400, "illegal_argument_exception", "the number of source shards [2] must be a factor of [3]")
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index.codec.compression_level":3}}`, 400, "settings_exception", "missing required setting [index.codec] for setting [index.codec.compression_level]")
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index.uuid":"abc"}}`, 400, "validation_exception", "Validation Failed: 1: private index setting [index.uuid] can not be set explicitly;")
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"unknown":1}`, 400, "parsing_exception", "unknown key [unknown] for create index")
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"mappings":{"_doc":{"properties":{"a":{"type":"keyword"}}}}}`, 400, "illegal_argument_exception", "The mapping definition cannot be nested under a type")

	mustDo(t, c, http.MethodPut, "/sv-1", `{"settings":{"number_of_replicas":0}}`)
	flat := mustDo(t, c, http.MethodGet, "/sv-1/_settings?flat_settings=true", nil)["sv-1"].(map[string]any)["settings"].(map[string]any)
	if flat["index.replication.type"] != "DOCUMENT" || flat["index.version.created"] != "137297827" {
		t.Fatalf("settings of a new index: %v", flat)
	}
	uuid := flat["index.uuid"].(string)
	expectAdminError(t, c, http.MethodPut, "/sv-1", nil, 400, "resource_already_exists_exception", "index [sv-1/"+uuid+"] already exists")
	// settings are validated before the index is looked up
	expectAdminError(t, c, http.MethodPut, "/sv-1", `{"settings":{"index.foo":1}}`, 400, "settings_exception", unknown)

	expectAdminError(t, c, http.MethodPut, "/sv-1/_settings", `{}`, 400, "action_request_validation_exception", "Validation Failed: 1: no settings to update;")
	expectAdminError(t, c, http.MethodPut, "/sv-1/_settings", `{"index.foo":5}`, 400, "settings_exception", unknown)
	expectAdminError(t, c, http.MethodPut, "/sv-1/_settings", `{"index.codec":"best_compression"}`, 400, "illegal_argument_exception", "Can't update non dynamic settings [[index.codec]] for open indices [[sv-1/"+uuid+"]]")
	expectAdminError(t, c, http.MethodPut, "/sv-1/_settings", `{"index.uuid":"x"}`, 400, "settings_exception", "can not update private setting [index.uuid]; this setting is managed by OpenSearch")
	expectAdminError(t, c, http.MethodPut, "/sv-1/_settings", `{"index.number_of_replicas":"abc"}`, 400, "illegal_argument_exception", "Failed to parse value [abc] for setting [index.number_of_replicas]")
	mustDo(t, c, http.MethodPut, "/sv-1/_settings", `{"settings":{"index.max_result_window":7},"ignored":1}`)
	if got := getFlatSetting(t, c, "sv-1", "index.max_result_window"); got != "7" {
		t.Fatalf("settings wrapper: max_result_window=%v", got)
	}
	mustDo(t, c, http.MethodPut, "/sv-1/_settings", `{"index.max_result_window":null}`)
	if got := getFlatSetting(t, c, "sv-1", "index.max_result_window"); got != nil {
		t.Fatalf("null did not reset the setting: %v", got)
	}
	if got := mustDo(t, c, http.MethodGet, "/sv-1/_settings/index.nope", nil); len(got) != 0 {
		t.Fatalf("filter matching nothing: %v", got)
	}
	if got := mustDo(t, c, http.MethodGet, "/sv-1/_settings/index", nil); len(got) != 0 {
		t.Fatalf("filter [index]: %v", got)
	}
	defaults := mustDo(t, c, http.MethodGet, "/sv-1/_settings?include_defaults=true&flat_settings=true", nil)["sv-1"].(map[string]any)["defaults"].(map[string]any)
	if defaults["index.max_result_window"] != "10000" || defaults["index.refresh_interval"] != "1s" {
		t.Fatalf("include_defaults: %v", defaults)
	}
}

func getFlatSetting(t *testing.T, c *Cluster, index, key string) any {
	t.Helper()
	return mustDo(t, c, http.MethodGet, "/"+index+"/_settings?flat_settings=true", nil)[index].(map[string]any)["settings"].(map[string]any)[key]
}

func TestIndexBlocksOnSettingsAndMappings(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/blk", nil)
	mustDo(t, c, http.MethodPut, "/blk/_settings", `{"index.blocks.write":true}`)
	mustDo(t, c, http.MethodPut, "/blk/_settings", `{"index.refresh_interval":"2s"}`)
	mustDo(t, c, http.MethodPut, "/blk/_settings", `{"index.blocks.write":false,"index.blocks.read_only":true}`)
	expectAdminError(t, c, http.MethodPut, "/blk/_settings", `{"index.refresh_interval":"1s"}`, 403, "cluster_block_exception", "index [blk] blocked by: [FORBIDDEN/5/index read-only (api)];")
	mustDo(t, c, http.MethodPut, "/blk/_settings", `{"index.blocks.read_only":false}`)
	mustDo(t, c, http.MethodPut, "/blk/_settings", `{"index.blocks.read_only_allow_delete":true}`)
	expectAdminError(t, c, http.MethodPut, "/blk/_settings", `{"index.refresh_interval":"1s"}`, 429, "cluster_block_exception", "index [blk] blocked by: [TOO_MANY_REQUESTS/12/disk usage exceeded flood-stage watermark, index has read-only-allow-delete block];")
	mustDo(t, c, http.MethodPut, "/blk/_settings", `{"index.blocks.read_only_allow_delete":null}`)
	mustDo(t, c, http.MethodPut, "/blk/_settings", `{"index.blocks.metadata":true}`)
	expectAdminError(t, c, http.MethodGet, "/blk/_settings", nil, 403, "cluster_block_exception", "index [blk] blocked by: [FORBIDDEN/9/index metadata (api)];")
	expectAdminError(t, c, http.MethodPut, "/blk/_mapping", `{"properties":{"z":{"type":"keyword"}}}`, 403, "cluster_block_exception", "index [blk] blocked by: [FORBIDDEN/9/index metadata (api)];")
	mustDo(t, c, http.MethodPut, "/blk/_settings", `{"index.blocks.metadata":false}`)
	mustDo(t, c, http.MethodGet, "/blk/_settings", nil)
}

func TestClusterSettingsValidation(t *testing.T) {
	c := New()
	defer c.Close()
	expectAdminError(t, c, http.MethodPut, "/_cluster/settings", `{}`, 400, "action_request_validation_exception", "Validation Failed: 1: no settings to update;")
	expectAdminError(t, c, http.MethodPut, "/_cluster/settings", `{"foo":{"a":1}}`, 400, "action_request_validation_exception", "Validation Failed: 1: no settings to update;")
	expectAdminError(t, c, http.MethodPut, "/_cluster/settings", `{"persistent":{"zzz.unknown.setting":1}}`, 400, "settings_exception", "persistent setting [zzz.unknown.setting], not recognized")
	expectAdminError(t, c, http.MethodPut, "/_cluster/settings", `{"transient":{"index.number_of_replicas":2}}`, 400, "settings_exception", "transient setting [index.number_of_replicas], not recognized")
	expectAdminError(t, c, http.MethodPut, "/_cluster/settings", `{"transient":{"cluster.info.update.interval":"abc"}}`, 400, "illegal_argument_exception", "failed to parse setting [cluster.info.update.interval] with value [abc] as a time value: unit is missing or unrecognized")
	expectAdminError(t, c, http.MethodPut, "/_cluster/settings", `{"transient":{"search.max_buckets":"abc"}}`, 400, "illegal_argument_exception", "Failed to parse value [abc] for setting [search.max_buckets]")
	expectAdminError(t, c, http.MethodPut, "/_cluster/settings", `{"transient":{"cluster.max_shards_per_node":-5}}`, 400, "illegal_argument_exception", "Failed to parse value [-5] for setting [cluster.max_shards_per_node] must be >= 1")
	expectAdminError(t, c, http.MethodPut, "/_cluster/settings", `{"transient":{"cluster.routing.allocation.enable":"zz"}}`, 400, "illegal_argument_exception", "Illegal allocation.enable value [ZZ]")
	expectAdminError(t, c, http.MethodPut, "/_cluster/settings", `{"transient":{"indices.recovery.max_bytes_per_sec":"notasize"}}`, 400, "illegal_argument_exception", "failed to parse setting [indices.recovery.max_bytes_per_sec] with value [notasize] as a size in bytes: unit is missing or unrecognized")

	put := mustDo(t, c, http.MethodPut, "/_cluster/settings", `{"transient":{"cluster.info.update.interval":"31s"}}`)
	if len(put["persistent"].(map[string]any)) != 0 || put["transient"].(map[string]any)["cluster"].(map[string]any)["info"].(map[string]any)["update"].(map[string]any)["interval"] != "31s" {
		t.Fatalf("PUT response: %v", put)
	}
	put = mustDo(t, c, http.MethodPut, "/_cluster/settings?flat_settings=true", `{"persistent":{"indices.recovery.max_concurrent_file_chunks":3}}`)
	if put["persistent"].(map[string]any)["indices.recovery.max_concurrent_file_chunks"] != "3" || len(put["transient"].(map[string]any)) != 0 {
		t.Fatalf("PUT response echoes only the request settings: %v", put)
	}
	got := mustDo(t, c, http.MethodGet, "/_cluster/settings?flat_settings=true", nil)
	if got["transient"].(map[string]any)["cluster.info.update.interval"] != "31s" {
		t.Fatalf("GET flat_settings: %v", got)
	}
	mustDo(t, c, http.MethodPut, "/_cluster/settings", `{"transient":{"cluster.info.*":null},"persistent":{"indices.recovery.max_concurrent_file_chunks":null}}`)
	got = mustDo(t, c, http.MethodGet, "/_cluster/settings", nil)
	if len(got["transient"].(map[string]any)) != 0 || len(got["persistent"].(map[string]any)) != 0 {
		t.Fatalf("null resets: %v", got)
	}
	if defaults := mustDo(t, c, http.MethodGet, "/_cluster/settings?include_defaults=true&flat_settings=true", nil)["defaults"].(map[string]any); defaults["search.max_buckets"] != "65535" {
		t.Fatalf("include_defaults: %v", defaults["search.max_buckets"])
	}
}

func TestBroadcastShardCountsAndStats(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/st-1", `{"settings":{"number_of_shards":2,"number_of_replicas":0}}`)
	mustDo(t, c, http.MethodPut, "/st-2", `{"settings":{"number_of_shards":1,"number_of_replicas":1}}`)
	check := func(path string, total, successful float64) {
		t.Helper()
		sh := mustDo(t, c, http.MethodPost, path, nil)["_shards"].(map[string]any)
		if sh["total"] != total || sh["successful"] != successful || sh["failed"] != 0.0 {
			t.Fatalf("%s: _shards=%v, want %v/%v", path, sh, total, successful)
		}
		if _, ok := sh["skipped"]; ok {
			t.Fatalf("%s: unexpected skipped: %v", path, sh)
		}
	}
	check("/st-1/_refresh", 2, 2)
	check("/st-2/_refresh", 2, 1)
	check("/st-1,st-2/_flush", 4, 3)
	check("/st-1/_forcemerge", 2, 2)
	check("/st-1,st-2/_cache/clear", 4, 3)
	check("/st-missing/_refresh?ignore_unavailable=true", 0, 0)

	stats := mustDo(t, c, http.MethodGet, "/st-1,st-2/_stats", nil)
	if sh := stats["_shards"].(map[string]any); sh["total"] != 4.0 || sh["successful"] != 3.0 {
		t.Fatalf("_stats _shards: %v", sh)
	}
	primaries := stats["_all"].(map[string]any)["primaries"].(map[string]any)
	for _, section := range []string{"docs", "store", "indexing", "get", "search", "merges", "refresh", "flush", "warmer", "query_cache", "fielddata", "completion", "segments", "translog", "request_cache", "recovery"} {
		if _, ok := primaries[section]; !ok {
			t.Fatalf("_stats misses section %s: %v", section, primaries)
		}
	}
	expectAdminError(t, c, http.MethodGet, "/st-1/_stats/foo", nil, 400, "illegal_argument_exception", "request [/st-1/_stats/foo] contains unrecognized metric: [foo]")
	expectAdminError(t, c, http.MethodGet, "/st-1/_stats?level=foo", nil, 400, "illegal_argument_exception", "level parameter must be one of [cluster] or [indices] or [shards] but was [foo]")
	docsOnly := mustDo(t, c, http.MethodGet, "/st-1/_stats/docs", nil)["_all"].(map[string]any)["primaries"].(map[string]any)
	if len(docsOnly) != 1 {
		t.Fatalf("metric selection: %v", docsOnly)
	}
}

func TestClusterHealthParameters(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/ch-1", `{"settings":{"number_of_shards":2,"number_of_replicas":0}}`)
	mustDo(t, c, http.MethodPut, "/ch-2", `{"settings":{"number_of_replicas":1}}`)
	res := mustDo(t, c, http.MethodGet, "/_cluster/health/ch-1,ch-2?level=indices", nil)
	indices := res["indices"].(map[string]any)
	if indices["ch-1"].(map[string]any)["status"] != "green" || indices["ch-2"].(map[string]any)["status"] != "yellow" {
		t.Fatalf("level=indices: %v", res)
	}
	shards := mustDo(t, c, http.MethodGet, "/_cluster/health/ch-1?level=shards", nil)["indices"].(map[string]any)["ch-1"].(map[string]any)["shards"].(map[string]any)
	if len(shards) != 2 {
		t.Fatalf("level=shards: %v", shards)
	}
	expectAdminError(t, c, http.MethodGet, "/_cluster/health/ch-1?wait_for_status=purple", nil, 400, "illegal_argument_exception", "No enum constant org.opensearch.cluster.health.ClusterHealthStatus.PURPLE")
	expectAdminError(t, c, http.MethodGet, "/_cluster/health/ch-1?timeout=abc", nil, 400, "illegal_argument_exception", "failed to parse setting [timeout] with value [abc] as a time value: unit is missing or unrecognized")
	expectAdminError(t, c, http.MethodGet, "/_cluster/health/ch-1?wait_for_nodes=abc", nil, 400, "number_format_exception", `For input string: "abc"`)
	st, body := status(t, c, http.MethodGet, "/_cluster/health/ch-2?wait_for_status=green&timeout=100ms", nil)
	if st != 408 || body["timed_out"] != true || body["status"] != "yellow" {
		t.Fatalf("wait_for_status=green on a yellow index: %d %v", st, body)
	}
	st, body = status(t, c, http.MethodGet, "/_cluster/health/ch-missing?timeout=100ms", nil)
	if st != 408 || body["timed_out"] != true || body["status"] != "red" {
		t.Fatalf("missing index: %d %v", st, body)
	}
	if st, _ := status(t, c, http.MethodGet, "/_cluster/health/ch-1?wait_for_status=green&timeout=1s", nil); st != 200 {
		t.Fatalf("green index: %d", st)
	}
}

func TestClusterStateStatsAndNodes(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/cs-1", `{"settings":{"number_of_shards":2,"number_of_replicas":1},"mappings":{"properties":{"a":{"type":"keyword"}}},"aliases":{"cs-al":{}}}`)
	state := mustDo(t, c, http.MethodGet, "/_cluster/state/metadata/cs-1", nil)
	if _, ok := state["routing_table"]; ok {
		t.Fatalf("metric selection: %v", state)
	}
	index := state["metadata"].(map[string]any)["indices"].(map[string]any)["cs-1"].(map[string]any)
	if index["state"] != "open" || index["aliases"].([]any)[0] != "cs-al" || index["mappings"].(map[string]any)["_doc"] == nil {
		t.Fatalf("cluster state metadata: %v", index)
	}
	routing := mustDo(t, c, http.MethodGet, "/_cluster/state/routing_table/cs-1", nil)["routing_table"].(map[string]any)["indices"].(map[string]any)["cs-1"].(map[string]any)["shards"].(map[string]any)
	shard0 := routing["0"].([]any)
	if len(routing) != 2 || len(shard0) != 2 || shard0[0].(map[string]any)["state"] != "STARTED" || shard0[1].(map[string]any)["state"] != "UNASSIGNED" {
		t.Fatalf("cluster state routing_table: %v", routing)
	}
	if unknown := mustDo(t, c, http.MethodGet, "/_cluster/state/foo", nil); len(unknown) != 2 {
		t.Fatalf("unknown cluster state metric: %v", unknown)
	}
	stats := mustDo(t, c, http.MethodGet, "/_cluster/stats", nil)
	if stats["status"] != "yellow" || stats["indices"].(map[string]any)["count"] != 1.0 || stats["_nodes"].(map[string]any)["total"] != 1.0 {
		t.Fatalf("cluster stats: %v", stats)
	}
	if nodes := mustDo(t, c, http.MethodGet, "/_nodes/nosuchnode", nil)["_nodes"].(map[string]any); nodes["total"] != 0.0 {
		t.Fatalf("node filter matching nothing: %v", nodes)
	}
	if nodes := mustDo(t, c, http.MethodGet, "/_nodes/_local", nil)["_nodes"].(map[string]any); nodes["total"] != 1.0 {
		t.Fatalf("_local node filter: %v", nodes)
	}
}
