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
