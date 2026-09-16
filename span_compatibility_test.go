package osmem

import "testing"

// span_term / span_near / span_multi, checked against OpenSearch 3.8.0
// (search/190_index_prefix_search.yml, "search index prefixes with span_multi").
func TestSpanQueryCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "span", `{"mappings":{"properties":{"text":{"type":"text"}}}}`,
		`{"text":"some short words with a stupendously long one"}`,
		`{"text":"sentence with UPPERCASE WORDS"}`,
		`{"text":["foo","b-12"]}`,
	)
	runDSLCases(t, c, "span", []dslCase{
		{name: "span_term alone", query: `{"span_term":{"text":"short"}}`, ids: []string{"1"}},
		{name: "span_near with span_multi prefix", query: `{"span_near":{"clauses":[{"span_term":{"text":"short"}},{"span_multi":{"match":{"prefix":{"text":"word"}}}}]}}`, ids: []string{"1"}},
		{name: "span_near out of order fails default in_order", query: `{"span_near":{"clauses":[{"span_multi":{"match":{"prefix":{"text":"word"}}}},{"span_term":{"text":"short"}}]}}`, ids: []string{}},
		{name: "span_near unordered matches either order", query: `{"span_near":{"clauses":[{"span_multi":{"match":{"prefix":{"text":"word"}}}},{"span_term":{"text":"short"}}],"in_order":false}}`, ids: []string{"1"}},
	})
}
