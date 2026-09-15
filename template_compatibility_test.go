package osmem

import (
	"net/http"
	"strings"
	"testing"
)

// Expectations in this file were taken from OpenSearch 3.8.0 responses.

func indexTemplateBody(t *testing.T, c *Cluster, name string) map[string]any {
	t.Helper()
	list := mustDo(t, c, http.MethodGet, "/_index_template/"+name, nil)["index_templates"].([]any)
	if len(list) != 1 {
		t.Fatalf("GET /_index_template/%s: %v", name, list)
	}
	return list[0].(map[string]any)["index_template"].(map[string]any)
}

func TestComposableTemplateShapeAndValidation(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/_index_template/t1", `{"index_patterns":["t1-*"],"priority":10,"version":3,"_meta":{"owner":"me"},"template":{"settings":{"number_of_shards":2,"index.refresh_interval":"2s"},"mappings":{"properties":{"f":{"type":"keyword"}}},"aliases":{"t1-alias":{},"t1-falias":{"filter":{"term":{"f":"x"}},"routing":"r"}}}}`)
	t1 := indexTemplateBody(t, c, "t1")
	if composed, ok := t1["composed_of"].([]any); !ok || len(composed) != 0 {
		t.Fatalf("composed_of of a template with a template section: %v", t1)
	}
	falias := t1["template"].(map[string]any)["aliases"].(map[string]any)["t1-falias"].(map[string]any)
	if falias["index_routing"] != "r" || falias["search_routing"] != "r" || falias["routing"] != nil {
		t.Fatalf("template alias routing: %v", falias)
	}
	if settings := t1["template"].(map[string]any)["settings"].(map[string]any)["index"].(map[string]any); settings["number_of_shards"] != "2" || settings["refresh_interval"] != "2s" {
		t.Fatalf("template settings: %v", settings)
	}
	mustDo(t, c, http.MethodPut, "/_index_template/t3", `{"index_patterns":"t3-*"}`)
	t3 := indexTemplateBody(t, c, "t3")
	if _, ok := t3["composed_of"]; ok {
		t.Fatalf("composed_of without a template section: %v", t3)
	}
	if _, ok := t3["priority"]; ok {
		t.Fatalf("priority is rendered although it was not set: %v", t3)
	}
	mustDo(t, c, http.MethodPut, "/_index_template/t7", `{"index_patterns":["t7-*"],"priority":"5"}`)
	if p := indexTemplateBody(t, c, "t7")["priority"]; p != 5.0 {
		t.Fatalf("priority given as a string: %v", p)
	}

	expectAdminError(t, c, http.MethodPut, "/_index_template/t2", `{"index_patterns":["t1-*"],"priority":10}`, 400, "illegal_argument_exception",
		"index template [t2] has index patterns [t1-*] matching patterns from existing templates [t1] with patterns (t1 => [t1-*]) that have the same priority [10], multiple index templates may not match during index creation, please use a different priority")
	expectAdminError(t, c, http.MethodPut, "/_index_template/bad1", `{"index_patterns":["b-*"],"foo":1}`, 400, "x_content_parse_exception", "[1:27] [index_template] unknown field [foo]")
	expectAdminError(t, c, http.MethodPut, "/_index_template/bad2", `{}`, 400, "illegal_argument_exception", "Required [index_patterns]")
	expectAdminError(t, c, http.MethodPut, "/_index_template/bad3", `{"index_patterns":["b3-*"],"composed_of":["nonexistent"]}`, 400, "invalid_index_template_exception",
		"index_template [bad3] invalid, cause [index template [bad3] specifies component templates [nonexistent] that do not exist]")
	expectAdminError(t, c, http.MethodPut, "/_index_template/t1?create=true", `{"index_patterns":["t1-*"],"priority":10}`, 400, "illegal_argument_exception", "index template [t1] already exists")
	expectAdminError(t, c, http.MethodPut, "/_index_template/bad4", `{"index_patterns":["b4-*"],"template":{"settings":{"index.unknown_zzz":1}}}`, 400, "settings_exception",
		"unknown setting [index.unknown_zzz] please check that any required plugins are installed, or check the breaking changes documentation for removed settings")
	expectAdminError(t, c, http.MethodPut, "/_index_template/bad5", `{"index_patterns":["b5-*"],"template":{"settings":{"number_of_shards":"x"}}}`, 400, "invalid_index_template_exception",
		"index_template [bad5] invalid, cause [Validation Failed: 1: Failed to parse value [x] for setting [index.number_of_shards];]")
	expectAdminError(t, c, http.MethodPut, "/_index_template/bad6", `{"index_patterns":["b6-*"],"priority":-1}`, 400, "action_request_validation_exception", "Validation Failed: 1: index template priority must be >= 0;")
	res := expectAdminError(t, c, http.MethodPut, "/_index_template/bad7", `{"index_patterns":["b7-*"],"template":{"foo":{}}}`, 400, "x_content_parse_exception", "[1:46] [index_template] failed to parse field [template]")
	if rootCauseReason(res) != "[1:40] [template] unknown field [foo]" {
		t.Fatalf("unknown template field root cause: %v", res)
	}
	expectAdminError(t, c, http.MethodPut, "/_index_template/bad8", `{"index_patterns":["b8-*"],"template":{"aliases":{"b-*":{}}}}`, 400, "invalid_alias_name_exception",
		`Invalid alias name [b-*]: must not contain the following characters [ , ", *, \, <, |, ,, >, /, ?]`)
	expectAdminError(t, c, http.MethodPut, "/_index_template/UPPER", `{"index_patterns":["u-*"]}`, 400, "invalid_index_template_exception", "index_template [UPPER] invalid, cause [Validation Failed: 1: name must be lower cased;]")
	expectAdminError(t, c, http.MethodPut, "/_index_template/bad9", `{"index_patterns":["_x*"]}`, 400, "invalid_index_template_exception", "index_template [bad9] invalid, cause [Validation Failed: 1: index_pattern [_x*] must not start with '_';]")
	expectAdminError(t, c, http.MethodPut, "/_index_template/t6", `{"index_patterns":["t6-*"],"version":"abc"}`, 400, "x_content_parse_exception", "[1:38] [index_template] failed to parse field [version]")
	res = expectAdminError(t, c, http.MethodPut, "/_index_template/bad10", `{"index_patterns":["b10-*"],"template":{"mappings":{"properties":{"x":{"type":"nosuchtype"}}}}}`, 400, "illegal_argument_exception", "composable template [bad10] template after composition is invalid")
	if reasons := causeReasons(res); len(reasons) < 3 || reasons[1] != "invalid composite mappings for [bad10]" || !strings.HasPrefix(reasons[2], "Failed to parse mapping [_doc]: ") {
		t.Fatalf("invalid composite mapping chain: %v", reasons)
	}

	expectAdminError(t, c, http.MethodDelete, "/_index_template/nope", nil, 404, "index_template_missing_exception", "index_template [nope] missing")
	expectAdminError(t, c, http.MethodGet, "/_index_template/nope", nil, 404, "resource_not_found_exception", "index template matching [nope] not found")
	if st, body := status(t, c, http.MethodGet, "/_index_template/nomatch*", nil); st != 404 || len(body["index_templates"].([]any)) != 0 {
		t.Fatalf("GET wildcard matching nothing: %d %v", st, body)
	}
}

func TestTemplateSimulation(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/_index_template/t1", `{"index_patterns":["t1-*"],"priority":10,"template":{"settings":{"number_of_shards":2},"mappings":{"properties":{"f":{"type":"keyword"}}}}}`)
	mustDo(t, c, http.MethodPut, "/_index_template/t1low", `{"index_patterns":["t1-*"],"priority":1,"template":{"settings":{"number_of_shards":3}}}`)
	mustDo(t, c, http.MethodPut, "/_template/leg", `{"index_patterns":["t1-*"],"order":5,"settings":{"number_of_shards":4}}`)

	sim := mustDo(t, c, http.MethodPost, "/_index_template/_simulate_index/t1-z", nil)
	overlapping := sim["overlapping"].([]any)
	names := map[string]bool{}
	for _, o := range overlapping {
		names[o.(map[string]any)["name"].(string)] = true
	}
	if len(overlapping) != 2 || !names["t1low"] || !names["leg"] {
		t.Fatalf("simulate_index overlapping: %v", overlapping)
	}
	tmpl := sim["template"].(map[string]any)
	if tmpl["settings"].(map[string]any)["index"].(map[string]any)["number_of_shards"] != "2" || tmpl["aliases"] == nil {
		t.Fatalf("simulate_index template: %v", tmpl)
	}
	if got := mustDo(t, c, http.MethodPost, "/_index_template/_simulate_index/nomatch", nil); len(got) != 0 {
		t.Fatalf("simulate_index without a matching template: %v", got)
	}
	named := mustDo(t, c, http.MethodPost, "/_index_template/_simulate/t1", nil)
	if len(named["overlapping"].([]any)) != 2 {
		t.Fatalf("simulate/{name} overlapping: %v", named)
	}
	body := mustDo(t, c, http.MethodPost, "/_index_template/_simulate", `{"index_patterns":["t1-*"],"priority":20,"template":{"settings":{"number_of_replicas":2}}}`)
	if _, hasMappings := body["template"].(map[string]any)["mappings"]; hasMappings || len(body["overlapping"].([]any)) != 3 {
		t.Fatalf("simulate with body: %v", body)
	}
	expectAdminError(t, c, http.MethodPost, "/_index_template/_simulate/nope", nil, 400, "illegal_argument_exception", "unable to simulate template [nope] that does not exist")
	expectAdminError(t, c, http.MethodPost, "/_index_template/_simulate", nil, 400, "action_request_validation_exception", "Validation Failed: 1: either index name or index template body must be specified for simulation;")
	res := expectAdminError(t, c, http.MethodPost, "/_index_template/_simulate", `{"index_patterns":["t1-*"],"priority":10}`, 400, "illegal_argument_exception", "")
	if !strings.Contains(errReason(res), "matching patterns from existing templates [t1]") {
		t.Fatalf("simulate with an overlapping priority: %v", res)
	}
}

func TestComponentTemplates(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/_component_template/c1", `{"template":{"settings":{"number_of_replicas":0,"index.refresh_interval":"3s"},"mappings":{"properties":{"g":{"type":"long"}}},"aliases":{"c-alias":{}}},"version":1,"_meta":{"x":1}}`)
	mustDo(t, c, http.MethodPut, "/_component_template/c2", `{"template":{"mappings":{"properties":{"g":{"type":"keyword"},"k":{"type":"keyword"}}}}}`)
	got := mustDo(t, c, http.MethodGet, "/_component_template/c1", nil)["component_templates"].([]any)[0].(map[string]any)
	ct := got["component_template"].(map[string]any)
	if got["name"] != "c1" || ct["version"] != 1.0 || ct["template"].(map[string]any)["settings"].(map[string]any)["index"].(map[string]any)["refresh_interval"] != "3s" {
		t.Fatalf("GET component template: %v", got)
	}
	if st, _ := status(t, c, http.MethodHead, "/_component_template/c1", nil); st != 200 {
		t.Fatalf("HEAD component template: %d", st)
	}
	expectAdminError(t, c, http.MethodGet, "/_component_template/nope", nil, 404, "resource_not_found_exception", "component template matching [nope] not found")
	expectAdminError(t, c, http.MethodPut, "/_component_template/c3", `{"foo":1}`, 400, "x_content_parse_exception", "[1:2] [component_template] unknown field [foo]")
	expectAdminError(t, c, http.MethodPut, "/_component_template/c3", `{}`, 400, "illegal_argument_exception", "Required [template]")
	expectAdminError(t, c, http.MethodPut, "/_component_template/c4", `{"template":{"settings":{"index.number_of_shards":"x"}}}`, 400, "illegal_argument_exception", "Failed to parse value [x] for setting [index.number_of_shards]")
	expectAdminError(t, c, http.MethodPut, "/_component_template/c1?create=true", `{"template":{}}`, 400, "illegal_argument_exception", "component template [c1] already exists")

	mustDo(t, c, http.MethodPut, "/_index_template/t5", `{"index_patterns":["t5-*"],"composed_of":["c1","c2"],"template":{"mappings":{"properties":{"h":{"type":"keyword"}}}}}`)
	mustDo(t, c, http.MethodPut, "/t5-a", nil)
	index := mustDo(t, c, http.MethodGet, "/t5-a", nil)["t5-a"].(map[string]any)
	props := index["mappings"].(map[string]any)["properties"].(map[string]any)
	if props["g"].(map[string]any)["type"] != "keyword" || props["h"] == nil || props["k"] == nil {
		t.Fatalf("composed mappings: %v", props)
	}
	if _, ok := index["aliases"].(map[string]any)["c-alias"]; !ok {
		t.Fatalf("composed aliases: %v", index["aliases"])
	}
	if settings := index["settings"].(map[string]any)["index"].(map[string]any); settings["number_of_replicas"] != "0" || settings["refresh_interval"] != "3s" {
		t.Fatalf("composed settings: %v", settings)
	}
	expectAdminError(t, c, http.MethodDelete, "/_component_template/c1", nil, 400, "illegal_argument_exception", "component templates [c1] cannot be removed as they are still in use by index templates [t5]")
	mustDo(t, c, http.MethodDelete, "/_index_template/t5", nil)
	mustDo(t, c, http.MethodDelete, "/_component_template/c1", nil)
	expectAdminError(t, c, http.MethodDelete, "/_component_template/nope", nil, 404, "index_template_missing_exception", "index_template [nope] missing")
}

func TestLegacyTemplateValidation(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/_template/l1", `{"index_patterns":["l-*"],"order":1,"version":7,"settings":{"number_of_shards":2,"index.refresh_interval":"4s"},"aliases":{"l-alias":{"is_write_index":true,"routing":"1"}}}`)
	flat := mustDo(t, c, http.MethodGet, "/_template/l1?flat_settings=true", nil)["l1"].(map[string]any)
	if flat["settings"].(map[string]any)["index.number_of_shards"] != "2" || flat["order"] != 1.0 || flat["version"] != 7.0 {
		t.Fatalf("legacy template flat_settings: %v", flat)
	}
	alias := flat["aliases"].(map[string]any)["l-alias"].(map[string]any)
	if _, ok := alias["is_write_index"]; ok || alias["index_routing"] != "1" || alias["search_routing"] != "1" {
		t.Fatalf("legacy template alias: %v", alias)
	}
	if st, body := status(t, c, http.MethodGet, "/_template/nope", nil); st != 404 || len(body) != 0 {
		t.Fatalf("GET missing legacy template: %d %v", st, body)
	}
	expectAdminError(t, c, http.MethodPut, "/_template/l3", `{"order":1}`, 400, "action_request_validation_exception", "Validation Failed: 1: index patterns are missing;")
	expectAdminError(t, c, http.MethodPut, "/_template/l1?create=true", `{"index_patterns":["l-*"]}`, 400, "illegal_argument_exception", "index_template [l1] already exists")
	expectAdminError(t, c, http.MethodPut, "/_template/l4", `{"index_patterns":["l4-*"],"foo":1}`, 400, "parse_exception", "unknown key [foo] in the template ")
	expectAdminError(t, c, http.MethodPut, "/_template/l6", `{"index_patterns":["l6-*"],"mappings":{"_doc":{"properties":{"a":{"type":"keyword"}}}}}`, 400, "illegal_argument_exception", "The mapping definition cannot be nested under a type")
	expectAdminError(t, c, http.MethodPut, "/_template/l7", `{"index_patterns":["l7-*"],"order":"x"}`, 400, "number_format_exception", `For input string: "x"`)
	expectAdminError(t, c, http.MethodPut, "/_template/l8", `{"index_patterns":["l8-*"],"settings":{"index.zzz_unknown":1}}`, 400, "settings_exception",
		"unknown setting [index.zzz_unknown] please check that any required plugins are installed, or check the breaking changes documentation for removed settings")
	expectAdminError(t, c, http.MethodPut, "/_template/l9", `{"index_patterns":["l9-*"],"version":"3"}`, 400, "illegal_argument_exception", "Malformed [version] value, should be an integer")
	expectAdminError(t, c, http.MethodDelete, "/_template/nope", nil, 404, "index_template_missing_exception", "index_template [nope] missing")
}

func TestDataStreamTemplatesAreRejectedOnUse(t *testing.T) {
	c := New()
	defer c.Close()
	expectAdminError(t, c, http.MethodPut, "/_data_stream/logs-x", nil, 400, "illegal_argument_exception", "no matching index template found for data stream [logs-x]")
	mustDo(t, c, http.MethodPut, "/_index_template/ds", `{"index_patterns":["ds-*"],"data_stream":{},"priority":1}`)
	if ds := indexTemplateBody(t, c, "ds")["data_stream"].(map[string]any); ds["timestamp_field"].(map[string]any)["name"] != "@timestamp" {
		t.Fatalf("data_stream rendering: %v", ds)
	}
	expectAdminError(t, c, http.MethodPut, "/ds-1", nil, 400, "illegal_argument_exception", "cannot create index with name [ds-1], because it matches with template [ds] that creates data streams only, use create data stream api instead")
	expectAdminError(t, c, http.MethodPost, "/ds-1/_doc", `{"@timestamp":"2024-01-01"}`, 400, "unsupported_operation_exception", "data streams are not supported by osmem")
	expectAdminError(t, c, http.MethodPut, "/_data_stream/ds-1", nil, 400, "unsupported_operation_exception", "data streams are not supported by osmem")
	expectAdminError(t, c, http.MethodGet, "/_data_stream/ds-1", nil, 404, "index_not_found_exception", "no such index [ds-1]")
	if got := mustDo(t, c, http.MethodGet, "/_data_stream", nil); len(got["data_streams"].([]any)) != 0 {
		t.Fatalf("GET /_data_stream: %v", got)
	}
}
