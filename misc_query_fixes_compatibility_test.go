package osmem

import (
	"net/http"
	"testing"
)

// Small, independently-confirmed OpenSearch 3.8.0 compatibility fixes.

// search/310_match_bool_prefix.yml: "multi_match multiple fields with
// cutoff_frequency throws exception".
func TestMultiMatchBoolPrefixRejectsCutoffFrequency(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "mmbp", `{"mappings":{"properties":{"my_field1":{"type":"text"},"my_field2":{"type":"text"}}}}`,
		`{"my_field1":"quick brown fox"}`)
	runDSLCases(t, c, "mmbp", []dslCase{
		{name: "cutoff_frequency rejected for bool_prefix", query: `{"multi_match":{"query":"brown","type":"bool_prefix","fields":["my_field1","my_field2"],"cutoff_frequency":0.001}}`,
			root: "parsing_exception", why: "[cutoff_frequency] not allowed for type [bool_prefix]"},
		{name: "cutoff_frequency still allowed for best_fields", query: `{"multi_match":{"query":"brown","fields":["my_field1","my_field2"],"cutoff_frequency":0.001}}`, ids: []string{"1"}},
	})
}

// search/10_source_filtering.yml: "docvalue_fields with default format".
func TestDocvalueFieldsUseFieldMappingFormat(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "dvf", `{"mappings":{"properties":{"count":{"type":"integer"}}}}`, `{"count":1}`)
	res := mustDo(t, c, http.MethodPost, "/dvf/_search", `{"docvalue_fields":[{"field":"count","format":"use_field_mapping"}]}`)
	hits := res["hits"].(map[string]any)["hits"].([]any)
	fields := hits[0].(map[string]any)["fields"].(map[string]any)
	got := fields["count"].([]any)
	if len(got) != 1 || got[0].(float64) != 1 {
		t.Fatalf("fields.count = %v, want [1]", got)
	}
}

// search.aggregation/20_terms.yml: "Unmapped unsigned longs".
func TestTermsAggregationUnsignedLongValueType(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/ult", `{"mappings":{"properties":{"unsigned":{"type":"unsigned_long"}}}}`)
	mustDo(t, c, http.MethodPost, "/ult/_doc/1?refresh=true", `{}`)
	res := mustDo(t, c, http.MethodPost, "/ult/_search",
		`{"size":0,"aggs":{"unsigned_terms":{"terms":{"field":"unmapped_unsigned","value_type":"unsigned_long","missing":3}}}}`)
	buckets := res["aggregations"].(map[string]any)["unsigned_terms"].(map[string]any)["buckets"].([]any)
	if len(buckets) != 1 {
		t.Fatalf("buckets = %v, want 1 bucket", buckets)
	}
	if key, _ := buckets[0].(map[string]any)["key"].(float64); key != 3 {
		t.Fatalf("key = %v, want 3", buckets[0].(map[string]any)["key"])
	}
}
