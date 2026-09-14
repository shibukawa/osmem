package osmem

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestRequireAliasAndRequiredRouting(t *testing.T) {
	c := New()
	defer c.Close()

	if statusCode, body := status(t, c, http.MethodPut, "/missing-alias/_doc/1?require_alias=true", `{}`); statusCode != http.StatusNotFound {
		t.Fatalf("require_alias on missing target: status=%d body=%v", statusCode, body)
	}
	if statusCode, _ := status(t, c, http.MethodHead, "/missing-alias", nil); statusCode != http.StatusNotFound {
		t.Fatalf("require_alias unexpectedly created target index, HEAD status=%d", statusCode)
	}

	mustDo(t, c, http.MethodPut, "/alias-target", `{}`)
	if statusCode, body := status(t, c, http.MethodPut, "/alias-target/_doc/1?require_alias=true", `{}`); statusCode != http.StatusBadRequest {
		t.Fatalf("require_alias on concrete index: status=%d body=%v", statusCode, body)
	}
	mustDo(t, c, http.MethodPut, "/alias-target/_alias/write-alias", nil)
	created := mustDo(t, c, http.MethodPut, "/write-alias/_doc/1?require_alias=true", `{"v":1}`)
	if created["result"] != "created" {
		t.Fatalf("write through alias with require_alias: %v", created)
	}

	mustDo(t, c, http.MethodPut, "/route-required", `{"mappings":{"_routing":{"required":true},"properties":{"v":{"type":"integer"}}}}`)
	if statusCode, body := status(t, c, http.MethodPut, "/route-required/_doc/1", `{"v":1}`); statusCode != http.StatusBadRequest || errType(body) != "routing_missing_exception" {
		t.Fatalf("write without required routing: status=%d body=%v", statusCode, body)
	}
	indexed := mustDo(t, c, http.MethodPut, "/route-required/_doc/1?routing=shard-a", `{"v":1}`)
	if indexed["result"] != "created" {
		t.Fatalf("write with required routing: %v", indexed)
	}
	if statusCode, body := status(t, c, http.MethodGet, "/route-required/_doc/1", nil); statusCode != http.StatusBadRequest || errType(body) != "routing_missing_exception" {
		t.Fatalf("get without required routing: status=%d body=%v", statusCode, body)
	}
	if statusCode, body := status(t, c, http.MethodHead, "/route-required/_doc/1", nil); statusCode != http.StatusBadRequest {
		t.Fatalf("HEAD without required routing: status=%d body=%v", statusCode, body)
	}
	if statusCode, body := status(t, c, http.MethodPost, "/route-required/_mget", `{"ids":["1"]}`); statusCode != http.StatusBadRequest || errType(body) != "routing_missing_exception" {
		t.Fatalf("mget without required routing: status=%d body=%v", statusCode, body)
	}
	got := mustDo(t, c, http.MethodGet, "/route-required/_doc/1?routing=shard-a", nil)
	if got["_source"].(map[string]any)["v"].(float64) != 1 {
		t.Fatalf("get with required routing: %v", got)
	}
}

func TestUpdateNoopStillChecksOCC(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/occ-noop", `{"settings":{"index":{"number_of_replicas":2}}}`)
	indexed := mustDo(t, c, http.MethodPut, "/occ-noop/_doc/1", `{"value":1}`)
	seqNo := int(indexed["_seq_no"].(float64))

	if statusCode, body := status(t, c, http.MethodPost, "/occ-noop/_update/1?if_seq_no=99&if_primary_term=1", `{"doc":{"value":1}}`); statusCode != http.StatusConflict || errType(body) != "version_conflict_engine_exception" {
		t.Fatalf("noop update with stale OCC: status=%d body=%v", statusCode, body)
	}
	result := mustDo(t, c, http.MethodPost, "/occ-noop/_update/1?if_seq_no="+strconv.Itoa(seqNo)+"&if_primary_term=1", `{"doc":{"value":1}}`)
	if result["result"] != "noop" {
		t.Fatalf("matching OCC on noop update: %v", result)
	}
	shards := result["_shards"].(map[string]any)
	if shards["total"].(float64) != 0 || shards["successful"].(float64) != 0 || shards["failed"].(float64) != 0 {
		t.Fatalf("noop should report zero shards written: %v", shards)
	}
}

func TestBulkCompatibilityChecksAndReindexLimitsVersions(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/bulk-require-alias", `{}`)
	bulk := mustDo(t, c, http.MethodPost, "/_bulk", "{\"index\":{\"_index\":\"bulk-require-alias\",\"_id\":\"1\",\"_require_alias\":true}}\n{\"v\":1}\n")
	item := bulk["items"].([]any)[0].(map[string]any)["index"].(map[string]any)
	if item["status"].(float64) != http.StatusBadRequest {
		t.Fatalf("bulk _require_alias item: %v", item)
	}
	if count, _ := c.Count("bulk-require-alias", nil); count != 0 {
		t.Fatalf("bulk _require_alias indexed a document in a concrete index: count=%d", count)
	}
	if statusCode, body := status(t, c, http.MethodPost, "/_bulk", "{\"index\":{\"_index\":\"missing-newline\",\"_id\":\"1\"}}\n{\"v\":1}"); statusCode != http.StatusBadRequest || errType(body) != "illegal_argument_exception" {
		t.Fatalf("bulk without final newline: status=%d body=%v", statusCode, body)
	}

	for i := 1; i <= 3; i++ {
		mustDo(t, c, http.MethodPut, "/reindex-limited/_doc/"+strconv.Itoa(i), `{"v":1}`)
	}
	limited := mustDo(t, c, http.MethodPost, "/_reindex", `{"max_docs":1,"source":{"index":"reindex-limited"},"dest":{"index":"reindex-limited-copy"}}`)
	if limited["created"].(float64) != 1 {
		t.Fatalf("reindex max_docs: %v", limited)
	}
	if count, _ := c.Count("reindex-limited-copy", nil); count != 1 {
		t.Fatalf("reindex max_docs destination count = %d, want 1", count)
	}

	mustDo(t, c, http.MethodPut, "/reindex-external-source/_doc/1?version=4&version_type=external", `{"v":"source"}`)
	mustDo(t, c, http.MethodPut, "/reindex-external-dest/_doc/1?version=5&version_type=external", `{"v":"newer"}`)
	if statusCode, body := status(t, c, http.MethodPost, "/_reindex", `{"source":{"index":"reindex-external-source"},"dest":{"index":"reindex-external-dest","version_type":"external"}}`); statusCode != http.StatusConflict || errType(body) != "version_conflict_engine_exception" {
		t.Fatalf("reindex external version conflict: status=%d body=%v", statusCode, body)
	}
	mustDo(t, c, http.MethodPut, "/reindex-external-source/_doc/1?version=6&version_type=external", `{"v":"source"}`)
	reindexed := mustDo(t, c, http.MethodPost, "/_reindex", `{"source":{"index":"reindex-external-source"},"dest":{"index":"reindex-external-dest","version_type":"external"}}`)
	if reindexed["updated"].(float64) != 1 {
		t.Fatalf("reindex external version update: %v", reindexed)
	}
	got := mustDo(t, c, http.MethodGet, "/reindex-external-dest/_doc/1", nil)
	if got["_version"].(float64) != 6 || got["_source"].(map[string]any)["v"] != "source" {
		t.Fatalf("reindex should preserve source version: %v", got)
	}
}

func TestMappingLimitsAndImmutableNorms(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/field-limit", `{"settings":{"index":{"mapping":{"total_fields":{"limit":1}}}}}`)
	if statusCode, body := status(t, c, http.MethodPut, "/field-limit/_doc/1", `{"one":1,"two":2}`); statusCode != http.StatusBadRequest || errType(body) != "mapper_parsing_exception" {
		t.Fatalf("total_fields.limit enforcement: status=%d body=%v", statusCode, body)
	}
	mapping := mustDo(t, c, http.MethodGet, "/field-limit/_mapping", nil)
	if props := mapping["field-limit"].(map[string]any)["mappings"].(map[string]any)["properties"]; props != nil {
		t.Fatalf("failed field-limit write changed dynamic mapping: %v", props)
	}

	mustDo(t, c, http.MethodPut, "/nested-limit", `{"settings":{"index":{"mapping":{"nested_objects":{"limit":1}}}},"mappings":{"properties":{"items":{"type":"nested","properties":{"v":{"type":"integer"}}}}}}`)
	if statusCode, body := status(t, c, http.MethodPut, "/nested-limit/_doc/1", `{"items":[{"v":1},{"v":2}]}`); statusCode != http.StatusBadRequest || errType(body) != "mapper_parsing_exception" {
		t.Fatalf("nested_objects.limit enforcement: status=%d body=%v", statusCode, body)
	}

	mustDo(t, c, http.MethodPut, "/immutable-norms", `{"mappings":{"properties":{"title":{"type":"text","norms":false}}}}`)
	if statusCode, body := status(t, c, http.MethodPut, "/immutable-norms/_mapping", `{"properties":{"title":{"type":"text","norms":true}}}`); statusCode != http.StatusBadRequest || errType(body) != "illegal_argument_exception" {
		t.Fatalf("immutable norms update: status=%d body=%v", statusCode, body)
	}

	mustDo(t, c, http.MethodPut, "/disable-norms", `{"mappings":{"properties":{"title":{"type":"text"}}}}`)
	if statusCode, body := status(t, c, http.MethodPut, "/disable-norms/_mapping", `{"properties":{"title":{"type":"text","norms":false}}}`); statusCode != http.StatusOK {
		t.Fatalf("disabling norms on an existing field is allowed: status=%d body=%v", statusCode, body)
	}
}

func TestMappingRejectsImmutableParameters(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/immutable-parameters", `{"mappings":{"properties":{
		"unindexed":{"type":"keyword","index":false},
		"no_doc_values":{"type":"keyword","doc_values":false},
		"stored":{"type":"keyword","store":true},
		"with_freqs":{"type":"keyword","index_options":"freqs"},
		"with_null":{"type":"keyword","null_value":"missing"},
		"with_similarity":{"type":"keyword","similarity":"BM25"},
		"with_normalizer":{"type":"keyword","normalizer":"lowercase"},
		"with_vectors":{"type":"text","term_vector":"yes"},
		"payload":{"type":"object","enabled":false},
		"nested":{"type":"nested","include_in_parent":true}
	}}}`)

	cases := []struct {
		name string
		body string
	}{
		{"index", `{"properties":{"unindexed":{"type":"keyword","index":true}}}`},
		{"doc_values", `{"properties":{"no_doc_values":{"type":"keyword","doc_values":true}}}`},
		{"store", `{"properties":{"stored":{"type":"keyword","store":false}}}`},
		{"index_options", `{"properties":{"with_freqs":{"type":"keyword","index_options":"docs"}}}`},
		{"null_value", `{"properties":{"with_null":{"type":"keyword","null_value":"unknown"}}}`},
		{"similarity", `{"properties":{"with_similarity":{"type":"keyword","similarity":"boolean"}}}`},
		{"normalizer", `{"properties":{"with_normalizer":{"type":"keyword","normalizer":"other_normalizer"}}}`},
		{"term_vector", `{"properties":{"with_vectors":{"type":"text","term_vector":"no"}}}`},
		{"object enabled", `{"properties":{"payload":{"type":"object","enabled":true}}}`},
		{"object enabled omitted on disabled field", `{"properties":{"payload":{"type":"object","properties":{"added":{"type":"keyword"}}}}}`},
		{"nested include", `{"properties":{"nested":{"type":"nested","include_in_parent":false}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if statusCode, body := status(t, c, http.MethodPut, "/immutable-parameters/_mapping", tc.body); statusCode != http.StatusBadRequest {
				t.Fatalf("immutable mapping parameter update: status=%d body=%v", statusCode, body)
			}
		})
	}
	mustDo(t, c, http.MethodPut, "/immutable-parameters/_mapping", `{"properties":{
		"unindexed":{"type":"keyword","index":false},
		"no_doc_values":{"type":"keyword","doc_values":false},
		"stored":{"type":"keyword","store":true},
		"with_freqs":{"type":"keyword","index_options":"freqs"},
		"with_null":{"type":"keyword","null_value":"missing"},
		"with_similarity":{"type":"keyword","similarity":"BM25"},
		"with_normalizer":{"type":"keyword","normalizer":"lowercase"},
		"with_vectors":{"type":"text","term_vector":"yes"},
		"payload":{"type":"object","enabled":false},
		"nested":{"type":"nested","include_in_parent":true}
	}}`)

	mustDo(t, c, http.MethodPut, "/immutable-root-enabled", `{"mappings":{"enabled":false}}`)
	if statusCode, body := status(t, c, http.MethodPut, "/immutable-root-enabled/_mapping", `{"enabled":true}`); statusCode != http.StatusBadRequest || errType(body) != "mapper_exception" {
		t.Fatalf("root enabled change: status=%d body=%v", statusCode, body)
	}
	if statusCode, body := status(t, c, http.MethodPut, "/immutable-root-enabled/_mapping", `{"properties":{"added":{"type":"keyword"}}}`); statusCode != http.StatusBadRequest || errType(body) != "mapper_exception" {
		t.Fatalf("root enabled change via implicit default: status=%d body=%v", statusCode, body)
	}
	mustDo(t, c, http.MethodPut, "/immutable-root-enabled/_mapping", `{"enabled":false}`)
}

func TestReindexHonorsMappingSourceFiltering(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/source-reindex", `{"mappings":{"_source":{"excludes":["private"]}}}`)
	mustDo(t, c, http.MethodPut, "/source-reindex/_doc/1", `{"public":"keep","private":"omit"}`)
	mustDo(t, c, http.MethodPost, "/_reindex", `{"source":{"index":"source-reindex"},"dest":{"index":"reindexed"}}`)
	get := mustDo(t, c, http.MethodGet, "/reindexed/_doc/1", nil)
	source := get["_source"].(map[string]any)
	if source["public"] != "keep" || source["private"] != nil {
		t.Fatalf("reindexed source should preserve mapping-level filtering: %v", source)
	}
}

func TestReindexRejectsIndexWithoutSource(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/no-source-reindex", `{"mappings":{"_source":{"enabled":false}}}`)
	mustDo(t, c, http.MethodPut, "/no-source-reindex/_doc/1", `{"v":1}`)
	if statusCode, body := status(t, c, http.MethodPost, "/_reindex", `{"source":{"index":"no-source-reindex"},"dest":{"index":"reindex-should-not-exist"}}`); statusCode != http.StatusBadRequest || errType(body) != "illegal_argument_exception" {
		t.Fatalf("reindex without source: status=%d body=%v", statusCode, body)
	}
	if statusCode, _ := status(t, c, http.MethodHead, "/reindex-should-not-exist", nil); statusCode != http.StatusNotFound {
		t.Fatalf("failed reindex created destination index: HEAD status=%d", statusCode)
	}
}

func TestNgramTokenizerRespectsTokenChars(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/ngram-tokenizer", `{"settings":{"index":{"analysis":{"tokenizer":{"letters":{"type":"ngram","min_gram":2,"max_gram":2,"token_chars":["letter"]}},"analyzer":{"letter_grams":{"type":"custom","tokenizer":"letters"}}}}},"mappings":{"properties":{"text":{"type":"text","analyzer":"letter_grams"}}}}`)
	result := mustDo(t, c, http.MethodGet, "/ngram-tokenizer/_analyze", `{"field":"text","text":"ab12cd"}`)
	tokens := result["tokens"].([]any)
	var got []string
	for _, token := range tokens {
		got = append(got, token.(map[string]any)["token"].(string))
	}
	if strings.Join(got, ",") != "ab,cd" {
		t.Fatalf("ngram token_chars tokens = %v, want [ab cd]", got)
	}
}

func TestAsciifoldingTokenFilterRemovesAccents(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/ascii-folding", `{"settings":{"index":{"analysis":{"filter":{"fold":{"type":"asciifolding"}},"analyzer":{"folded":{"type":"custom","tokenizer":"standard","filter":["lowercase","fold"]}}}}},"mappings":{"properties":{"text":{"type":"text","analyzer":"folded"}}}}`)
	result := mustDo(t, c, http.MethodGet, "/ascii-folding/_analyze", `{"field":"text","text":"café résumé"}`)
	tokens := result["tokens"].([]any)
	var got []string
	for _, token := range tokens {
		got = append(got, token.(map[string]any)["token"].(string))
	}
	if strings.Join(got, ",") != "cafe,resume" {
		t.Fatalf("asciifolding tokens = %v, want [cafe resume]", got)
	}

	mustDo(t, c, http.MethodPut, "/ascii-folding-preserve", `{"settings":{"index":{"analysis":{"filter":{"fold":{"type":"asciifolding","preserve_original":true}},"analyzer":{"folded":{"type":"custom","tokenizer":"standard","filter":["lowercase","fold"]}}}}},"mappings":{"properties":{"text":{"type":"text","analyzer":"folded"}}}}`)
	result = mustDo(t, c, http.MethodGet, "/ascii-folding-preserve/_analyze", `{"field":"text","text":"café"}`)
	tokens = result["tokens"].([]any)
	seen := map[string]bool{}
	for _, token := range tokens {
		seen[token.(map[string]any)["token"].(string)] = true
	}
	if !seen["cafe"] || !seen["café"] {
		t.Fatalf("asciifolding preserve_original tokens = %v", tokens)
	}
}

func TestGlobalSettingsAndStatsPathFilters(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/global-path-filters", `{"settings":{"index":{"number_of_replicas":0}}}`)

	settings := mustDo(t, c, http.MethodGet, "/_settings/index.number_of_replicas", nil)
	gotSettings := settings["global-path-filters"].(map[string]any)["settings"].(map[string]any)["index"].(map[string]any)
	if gotSettings["number_of_replicas"] != "0" || len(gotSettings) != 1 {
		t.Fatalf("global settings path filter = %v", gotSettings)
	}

	stats := mustDo(t, c, http.MethodGet, "/_stats/docs", nil)
	index := stats["indices"].(map[string]any)["global-path-filters"].(map[string]any)
	primaries := index["primaries"].(map[string]any)
	if primaries["docs"] == nil || primaries["store"] != nil {
		t.Fatalf("global stats metric path filter = %v", primaries)
	}
	if statusCode, body := status(t, c, http.MethodGet, "/global-path-filters/_stats/search", nil); statusCode != http.StatusOK || errType(body) != "" {
		t.Fatalf("index stats metric route: status=%d body=%v", statusCode, body)
	}
}

func TestValidateQueryAPI(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/validate-query", `{"mappings":{"properties":{"name":{"type":"keyword"}}}}`)

	valid := mustDo(t, c, http.MethodPost, "/validate-query/_validate/query", `{"query":{"term":{"name":"widget"}}}`)
	if valid["valid"] != true || valid["_shards"].(map[string]any)["total"].(float64) != 1 {
		t.Fatalf("valid query response = %v", valid)
	}
	valid = mustDo(t, c, http.MethodGet, "/_validate/query?q=name%3Awidget", nil)
	if valid["valid"] != true {
		t.Fatalf("global q parameter query response = %v", valid)
	}

	invalid := mustDo(t, c, http.MethodPost, "/validate-query/_validate/query?explain=true", `{"query":{"not_a_query":{"name":"widget"}}}`)
	if invalid["valid"] != false {
		t.Fatalf("invalid query response = %v", invalid)
	}
	if explanations, ok := invalid["explanations"].([]any); !ok || len(explanations) != 1 {
		t.Fatalf("invalid query explanations = %v", invalid["explanations"])
	}
}

func TestReindexRejectsInvalidOptions(t *testing.T) {
	c := New()
	defer c.Close()
	cases := []struct {
		name string
		body string
	}{
		{"fractional max_docs", `{"max_docs":1.5,"source":{"index":"source"},"dest":{"index":"bad-reindex"}}`},
		{"string max_docs", `{"max_docs":"1","source":{"index":"source"},"dest":{"index":"bad-reindex"}}`},
		{"boolean max_docs", `{"max_docs":true,"source":{"index":"source"},"dest":{"index":"bad-reindex"}}`},
		{"negative max_docs", `{"max_docs":-1,"source":{"index":"source"},"dest":{"index":"bad-reindex"}}`},
		{"invalid version_type", `{"source":{"index":"source"},"dest":{"index":"bad-reindex","version_type":"mystery"}}`},
		{"invalid op_type", `{"source":{"index":"source"},"dest":{"index":"bad-reindex","op_type":"upsert"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if statusCode, body := status(t, c, http.MethodPost, "/_reindex", tc.body); statusCode != http.StatusBadRequest {
				t.Fatalf("invalid reindex option: status=%d body=%v", statusCode, body)
			}
		})
	}
	if statusCode, _ := status(t, c, http.MethodHead, "/bad-reindex", nil); statusCode != http.StatusNotFound {
		t.Fatalf("invalid reindex request created destination index: HEAD status=%d", statusCode)
	}
}

func TestSimulateIndexTemplateDoesNotWriteTemplate(t *testing.T) {
	c := New()
	defer c.Close()

	preview := mustDo(t, c, http.MethodPost, "/_index_template/_simulate", `{"index_patterns":["preview-*"],"template":{"settings":{"number_of_replicas":0}}}`)
	if preview["template"] == nil || preview["overlapping"] == nil {
		t.Fatalf("inline template simulation response = %v", preview)
	}
	templates := mustDo(t, c, http.MethodGet, "/_index_template", nil)["index_templates"].([]any)
	if len(templates) != 0 {
		t.Fatalf("inline simulation persisted a template: %v", templates)
	}

	mustDo(t, c, http.MethodPut, "/_index_template/preview-low", `{"index_patterns":["preview-*"],"priority":1,"template":{"settings":{"number_of_replicas":1}}}`)
	mustDo(t, c, http.MethodPut, "/_index_template/preview-high", `{"index_patterns":["preview-*"],"priority":2,"template":{"settings":{"number_of_replicas":0}}}`)
	resolved := mustDo(t, c, http.MethodPost, "/_index_template/_simulate_index/preview-0001", nil)
	resolvedTemplate := resolved["template"].(map[string]any)
	settings := resolvedTemplate["settings"].(map[string]any)["index"].(map[string]any)
	if settings["number_of_replicas"] != "0" {
		t.Fatalf("simulation did not choose highest-priority template: %v", resolved)
	}
	overlapping := resolved["overlapping"].([]any)
	if len(overlapping) != 1 || overlapping[0].(map[string]any)["name"] != "preview-low" {
		t.Fatalf("simulation overlapping templates = %v", overlapping)
	}
	if statusCode, _ := status(t, c, http.MethodHead, "/preview-0001", nil); statusCode != http.StatusNotFound {
		t.Fatalf("simulate_index created an index: HEAD status=%d", statusCode)
	}
}

func TestGeoDistanceUnmappedValidationAndCoercion(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/geo-validation", `{"mappings":{"properties":{"loc":{"type":"geo_point"}}}}`)
	mustDo(t, c, http.MethodPut, "/geo-validation/_doc/1", `{"loc":{"lat":80,"lon":180}}`)

	query := `{"query":{"geo_distance":{"distance":"1km","missing":{"lat":0,"lon":0}}}}`
	if statusCode, body := status(t, c, http.MethodPost, "/geo-validation/_search", query); statusCode != http.StatusBadRequest {
		t.Fatalf("unmapped geo_distance field: status=%d body=%v", statusCode, body)
	}
	query = `{"query":{"geo_distance":{"distance":"1km","ignore_unmapped":true,"missing":{"lat":0,"lon":0}}}}`
	noHits := mustDo(t, c, http.MethodPost, "/geo-validation/_search", query)
	if hits := noHits["hits"].(map[string]any)["hits"].([]any); len(hits) != 0 {
		t.Fatalf("ignore_unmapped geo_distance hits = %v", hits)
	}
	query = `{"query":{"geo_distance":{"distance":"1km","loc":{"lat":100,"lon":0}}}}`
	if statusCode, body := status(t, c, http.MethodPost, "/geo-validation/_search", query); statusCode != http.StatusBadRequest {
		t.Fatalf("strict geo_distance coordinates: status=%d body=%v", statusCode, body)
	}
	query = `{"query":{"geo_distance":{"distance":"1km","validation_method":"COERCE","loc":{"lat":100,"lon":0}}}}`
	coerced := mustDo(t, c, http.MethodPost, "/geo-validation/_search", query)
	hits := coerced["hits"].(map[string]any)["hits"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["_id"] != "1" {
		t.Fatalf("coerced geo_distance hits = %v", hits)
	}
}

func TestFieldCapsAcrossIndices(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/caps-west", `{"mappings":{"properties":{"product":{"type":"text"},"amount":{"type":"long","doc_values":false},"title":{"type":"keyword"}}}}`)
	mustDo(t, c, http.MethodPut, "/caps-east", `{"mappings":{"properties":{"product":{"type":"keyword"},"amount":{"type":"long"}}}}`)

	response := mustDo(t, c, http.MethodPost, "/caps-*/_field_caps?include_unmapped=true", `{"fields":["product","amount","title"]}`)
	var indexNames []string
	for _, index := range response["indices"].([]any) {
		indexNames = append(indexNames, index.(string))
	}
	if got := strings.Join(indexNames, ","); got != "caps-east,caps-west" {
		t.Fatalf("field caps indices = %v", response["indices"])
	}
	fields := response["fields"].(map[string]any)
	product := fields["product"].(map[string]any)
	textCap := product["text"].(map[string]any)
	keywordCap := product["keyword"].(map[string]any)
	if textCap["searchable"] != true || textCap["aggregatable"] != false || textCap["indices"].([]any)[0] != "caps-west" {
		t.Fatalf("text capabilities = %v", textCap)
	}
	if keywordCap["searchable"] != true || keywordCap["aggregatable"] != true || keywordCap["indices"].([]any)[0] != "caps-east" {
		t.Fatalf("keyword capabilities = %v", keywordCap)
	}
	amount := fields["amount"].(map[string]any)["long"].(map[string]any)
	if amount["aggregatable"] != false || amount["non_aggregatable_indices"].([]any)[0] != "caps-west" {
		t.Fatalf("long capabilities = %v", amount)
	}
	title := fields["title"].(map[string]any)
	if title["unmapped"].(map[string]any)["indices"].([]any)[0] != "caps-east" {
		t.Fatalf("include_unmapped capabilities = %v", title)
	}
}

func TestWriteEnumValidationAndExternalGT(t *testing.T) {
	c := New()
	defer c.Close()

	for _, path := range []string{
		"/invalid-op/_doc/1?op_type=upsert",
		"/invalid-version/_doc/1?version=1&version_type=mystery",
		"/invalid-version-without-number/_doc/1?version_type=mystery",
	} {
		if statusCode, body := status(t, c, http.MethodPut, path, `{}`); statusCode != http.StatusBadRequest || errType(body) != "illegal_argument_exception" {
			t.Fatalf("invalid write option %s: status=%d body=%v", path, statusCode, body)
		}
	}
	if statusCode, _ := status(t, c, http.MethodHead, "/invalid-op", nil); statusCode != http.StatusNotFound {
		t.Fatalf("invalid op_type created an index: HEAD status=%d", statusCode)
	}
	bulk := mustDo(t, c, http.MethodPost, "/_bulk", "{\"index\":{\"_index\":\"bulk-bad-version\",\"_id\":\"1\",\"version_type\":\"mystery\"}}\n{\"v\":1}\n")
	item := bulk["items"].([]any)[0].(map[string]any)["index"].(map[string]any)
	if item["status"].(float64) != http.StatusBadRequest {
		t.Fatalf("bulk invalid version_type without version = %v", item)
	}

	if result := mustDo(t, c, http.MethodPut, "/external-gt-source/_doc/1?version=6&version_type=external_gt", `{"v":"source"}`); result["_version"].(float64) != 6 {
		t.Fatalf("external_gt write = %v", result)
	}
	mustDo(t, c, http.MethodPut, "/external-gt-dest/_doc/1?version=5&version_type=external", `{"v":"old"}`)
	reindexed := mustDo(t, c, http.MethodPost, "/_reindex", `{"source":{"index":"external-gt-source"},"dest":{"index":"external-gt-dest","version_type":"external_gt"}}`)
	if reindexed["updated"].(float64) != 1 {
		t.Fatalf("external_gt reindex = %v", reindexed)
	}
	if statusCode, body := status(t, c, http.MethodPost, "/_reindex", `{"source":{"index":"external-gt-source"},"dest":{"index":"external-gt-dest","version_type":"external_gt"}}`); statusCode != http.StatusConflict {
		t.Fatalf("equal external_gt reindex should conflict: status=%d body=%v", statusCode, body)
	}
}

func TestPreserveExistingSettingsAndDynamicEnumValidation(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/settings-preserve", `{"settings":{"index":{"number_of_replicas":0}}}`)
	mustDo(t, c, http.MethodPut, "/settings-preserve/_settings?preserve_existing=true", `{"index":{"number_of_replicas":3,"refresh_interval":"5s"}}`)
	settings := mustDo(t, c, http.MethodGet, "/settings-preserve/_settings?flat_settings=true", nil)["settings-preserve"].(map[string]any)["settings"].(map[string]any)
	if settings["index.number_of_replicas"] != "0" || settings["index.refresh_interval"] != "5s" {
		t.Fatalf("preserve_existing settings = %v", settings)
	}

	for _, body := range []string{
		`{"mappings":{"dynamic":"typo"}}`,
		`{"mappings":{"dynamic":"runtime"}}`,
		`{"mappings":{"properties":{"obj":{"dynamic":1}}}}`,
	} {
		if statusCode, response := status(t, c, http.MethodPut, "/invalid-dynamic", body); statusCode != http.StatusBadRequest || errType(response) != "mapper_parsing_exception" {
			t.Fatalf("invalid dynamic value %s: status=%d body=%v", body, statusCode, response)
		}
	}
}

func TestResolveIndexForIndicesAndAliases(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/resolve-first", `{}`)
	mustDo(t, c, http.MethodPut, "/resolve-second", `{}`)
	mustDo(t, c, http.MethodPut, "/resolve-first/_alias/resolve-alias", nil)
	mustDo(t, c, http.MethodPut, "/resolve-second/_alias/resolve-alias", nil)

	resolved := mustDo(t, c, http.MethodGet, "/_resolve/index/resolve-alias", nil)
	aliases := resolved["aliases"].([]any)
	if len(aliases) != 1 {
		t.Fatalf("resolved aliases = %v", aliases)
	}
	alias := aliases[0].(map[string]any)
	if alias["name"] != "resolve-alias" || strings.Join(anyStrings(alias["indices"]), ",") != "resolve-first,resolve-second" {
		t.Fatalf("resolved alias = %v", alias)
	}
	indices := resolved["indices"].([]any)
	if len(indices) != 2 || indices[0].(map[string]any)["attributes"].([]any)[0] != "open" {
		t.Fatalf("resolved indices = %v", indices)
	}
	if streams := resolved["data_streams"].([]any); len(streams) != 0 {
		t.Fatalf("modeled cluster returned data streams: %v", streams)
	}
}

func TestResolveIndexWildcardExpansion(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/resolve-open-visible", `{}`)
	mustDo(t, c, http.MethodPut, "/resolve-open-hidden", `{"settings":{"index.hidden":true}}`)
	mustDo(t, c, http.MethodPut, "/resolve-open-hidden/_alias/resolve-hidden-alias", nil)

	response := mustDo(t, c, http.MethodGet, "/_resolve/index/resolve-open-*", nil)
	if got := len(response["indices"].([]any)); got != 1 {
		t.Fatalf("default wildcard expansion returned %d indices, want only visible open index: %v", got, response)
	}
	response = mustDo(t, c, http.MethodGet, "/_resolve/index/resolve-open-*?expand_wildcards=open,hidden", nil)
	if got := len(response["indices"].([]any)); got != 2 {
		t.Fatalf("open,hidden wildcard expansion returned %d indices, want 2: %v", got, response)
	}
	response = mustDo(t, c, http.MethodGet, "/_resolve/index/resolve-open-*?expand_wildcards=closed", nil)
	if got := len(response["indices"].([]any)); got != 0 {
		t.Fatalf("closed wildcard expansion returned %d open indices, want none: %v", got, response)
	}
	response = mustDo(t, c, http.MethodGet, "/_resolve/index/resolve-hidden-alias?expand_wildcards=closed", nil)
	if got := len(response["indices"].([]any)); got != 1 {
		t.Fatalf("an explicit alias should resolve independently of wildcard expansion: %v", response)
	}
	if statusCode, body := status(t, c, http.MethodGet, "/_resolve/index/resolve-open-*?expand_wildcards=none", nil); statusCode != http.StatusBadRequest || errType(body) != "illegal_argument_exception" {
		t.Fatalf("none should reject wildcard expressions: status=%d body=%v", statusCode, body)
	}
}

func TestExternalVersionRequiresVersion(t *testing.T) {
	c := New()
	defer c.Close()
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		statusCode, body := status(t, c, method, "/external-version-required/_doc/1?version_type=external", `{}`)
		if statusCode != http.StatusBadRequest || errType(body) != "action_request_validation_exception" {
			t.Fatalf("%s without version_type's required version: status=%d body=%v", method, statusCode, body)
		}
	}
}

func anyStrings(v any) []string {
	values, _ := v.([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if s, ok := value.(string); ok {
			result = append(result, s)
		}
	}
	return result
}
