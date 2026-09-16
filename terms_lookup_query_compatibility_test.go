package osmem

import "testing"

// terms_lookup's "query" form (OpenSearch 3.2+, search/171_terms_lookup_query.yml):
// {"terms": {"<field>": {"index", "path", "query"}}} unions the path values
// of every document of index matching query, instead of a single "id".
func TestTermsLookupByQueryCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "lookup_index", `{}`,
		`{"group":"g1","followers":["foo","bar"],"tag":"a"}`,
		`{"group":"g1","followers":["baz"],"tag":"b"}`,
		`{"group":"g2","followers":null,"tag":"c"}`,
		`{"group":"g1","tag":"d"}`,
		`{"group":"g1","followers":["baz"],"tag":"e"}`,
		`{"group":"g3","followers":[],"tag":"f"}`,
	)
	dslIndex(t, c, "main_index", `{}`,
		`{"user":"foo"}`,
		`{"user":"bar"}`,
		`{"user":"baz"}`,
		`{"user":"qux"}`,
		`{"user":"foo"}`,
	)
	runDSLCases(t, c, "main_index", []dslCase{
		{name: "term query matches 4 lookup docs", query: `{"terms":{"user":{"index":"lookup_index","path":"followers","query":{"term":{"group":"g1"}}}}}`, ids: []string{"1", "2", "3", "5"}},
		{name: "some lookup docs have missing/null/empty field", query: `{"terms":{"user":{"index":"lookup_index","path":"followers","query":{"terms":{"tag":["a","c","f"]}}}}}`, ids: []string{"1", "2", "5"}},
		{name: "field always empty for the one matching doc", query: `{"terms":{"user":{"index":"lookup_index","path":"followers","query":{"term":{"tag":"d"}}}}}`, ids: []string{}},
		{name: "query matches no lookup docs", query: `{"terms":{"user":{"index":"lookup_index","path":"followers","query":{"term":{"tag":"zzz"}}}}}`, ids: []string{}},
		{name: "duplicate values across lookup docs deduplicated", query: `{"terms":{"user":{"index":"lookup_index","path":"followers","query":{"terms":{"tag":["a","b"]}}}}}`, ids: []string{"1", "2", "3", "5"}},
		{name: "path does not exist on any matching doc", query: `{"terms":{"user":{"index":"lookup_index","path":"not_a_field","query":{"match_all":{}}}}}`, ids: []string{}},
	})

	t.Run("id and query together is rejected", func(t *testing.T) {
		_, _, st, res := dslSearch(t, c, "main_index", `{"terms":{"user":{"index":"lookup_index","id":"1","path":"tag","query":{"match_all":{}}}}}`)
		typ, _ := rootCause(res)
		if st != 400 || typ != "x_content_parse_exception" {
			t.Fatalf("status=%d root=%s body=%v", st, typ, res)
		}
	})
}
