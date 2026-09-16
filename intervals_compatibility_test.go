package osmem

import (
	"testing"
)

// The intervals query, checked against OpenSearch 3.8.0 responses (see
// search/230_interval_query.yml in the OpenSearch REST API test suite, plus
// extra "via mode" variants of the deprecated boolean "ordered" field probed
// directly against a real 3.8.0 server).
func TestIntervalsQueryCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "iv", `{"mappings":{"properties":{"text":{"type":"text","analyzer":"standard"}}}}`,
		`{"text":"Some like hot and dry, some like it cold and wet"}`,
		`{"text":"Its cold outside, theres no kind of atmosphere"}`,
		`{"text":"Baby its cold there outside"}`,
		`{"text":"Outside it is cold and wet"}`,
		`{"text":"cold rain makes it wet"}`,
		`{"text":"that is some cold cold rain"}`,
	)
	runDSLCases(t, c, "iv", []dslCase{
		{name: "regexp", query: `{"intervals":{"text":{"regexp":{"pattern":"at[a-z]{2,}here"}}}}`, ids: []string{"2"}},
		{name: "regexp case sensitive", query: `{"intervals":{"text":{"regexp":{"pattern":"AT[a-z]{2,}HERE","case_insensitive":false}}}}`, ids: []string{}},
		{name: "regexp case insensitive", query: `{"intervals":{"text":{"regexp":{"pattern":"AT[a-z]{2,}HERE","case_insensitive":true}}}}`, ids: []string{"2"}},
		{name: "ordered via mode", query: `{"intervals":{"text":{"match":{"query":"cold outside","mode":"ordered"}}}}`, ids: []string{"2", "3"}},
		{name: "default unordered", query: `{"intervals":{"text":{"match":{"query":"cold outside"}}}}`, ids: []string{"2", "3", "4"}},
		{name: "explicit unordered via mode", query: `{"intervals":{"text":{"match":{"query":"cold outside","mode":"unordered"}}}}`, ids: []string{"2", "3", "4"}},
		{name: "unordered with overlap", query: `{"intervals":{"text":{"match":{"query":"cold wet it","mode":"unordered"}}}}`, ids: []string{"1", "4", "5"}},
		{name: "unordered no overlap", query: `{"intervals":{"text":{"match":{"query":"cold wet it","mode":"unordered_no_overlap"}}}}`, ids: []string{"1", "4"}},
		{name: "phrase matching", query: `{"intervals":{"text":{"match":{"query":"cold outside","mode":"ordered","max_gaps":0}}}}`, ids: []string{"2"}},
		{name: "unordered max_gaps", query: `{"intervals":{"text":{"match":{"query":"cold outside","max_gaps":1}}}}`, ids: []string{"2", "3"}},
		{name: "ordered max_gaps", query: `{"intervals":{"text":{"match":{"query":"cold outside","max_gaps":0,"mode":"ordered"}}}}`, ids: []string{"2"}},
		{name: "ordered combination with disjunction", query: `{"intervals":{"text":{"all_of":{"intervals":[{"any_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"outside"}}]}},{"match":{"query":"atmosphere"}}],"mode":"ordered"}}}}`, ids: []string{"2"}},
		{name: "ordered combination with max_gaps", query: `{"intervals":{"text":{"all_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"outside"}}],"max_gaps":0,"mode":"ordered"}}}}`, ids: []string{"2"}},
		{name: "ordered combination", query: `{"intervals":{"text":{"all_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"outside"}}],"mode":"ordered"}}}}`, ids: []string{"2", "3"}},
		{name: "unordered combination via mode", query: `{"intervals":{"text":{"all_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"outside"}}],"max_gaps":1,"mode":"unordered"}}}}`, ids: []string{"2", "3"}},
		{name: "unordered combination with overlap", query: `{"intervals":{"text":{"all_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"wet"}},{"match":{"query":"it"}}],"mode":"unordered"}}}}`, ids: []string{"1", "4", "5"}},
		{name: "unordered combination no overlap", query: `{"intervals":{"text":{"all_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"wet"}},{"match":{"query":"it"}}],"mode":"unordered_no_overlap"}}}}`, ids: []string{"1", "4"}},
		{name: "nested unordered combination with overlap", query: `{"intervals":{"text":{"all_of":{"intervals":[{"any_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"hot"}}]}},{"match":{"query":"cold"}}],"mode":"unordered"}}}}`, ids: []string{"1", "2", "3", "4", "5", "6"}},
		{name: "nested unordered combination no overlap", query: `{"intervals":{"text":{"all_of":{"intervals":[{"any_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"hot"}}]}},{"match":{"query":"cold"}}],"mode":"unordered_no_overlap"}}}}`, ids: []string{"1", "6"}},
		{name: "containing", query: `{"intervals":{"text":{"all_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"outside"}}],"mode":"unordered","filter":{"containing":{"match":{"query":"is"}}}}}}}`, ids: []string{"4"}},
		{name: "not containing", query: `{"intervals":{"text":{"all_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"outside"}}],"mode":"unordered","filter":{"not_containing":{"match":{"query":"is"}}}}}}}`, ids: []string{"2", "3"}},
		{name: "contained_by", query: `{"intervals":{"text":{"match":{"query":"is","filter":{"contained_by":{"all_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"outside"}}],"mode":"unordered"}}}}}}}`, ids: []string{"4"}},
		{name: "not_contained_by", query: `{"intervals":{"text":{"match":{"query":"it","filter":{"not_contained_by":{"all_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"outside"}}]}}}}}}}`, ids: []string{"1", "5"}},
		{name: "not_overlapping", query: `{"intervals":{"text":{"all_of":{"intervals":[{"match":{"query":"cold"}},{"match":{"query":"outside"}}],"mode":"ordered","filter":{"not_overlapping":{"all_of":{"intervals":[{"match":{"query":"baby"}},{"match":{"query":"there"}}],"mode":"unordered"}}}}}}}`, ids: []string{"2"}},
		{name: "overlapping", query: `{"intervals":{"text":{"match":{"query":"cold outside","mode":"ordered","filter":{"overlapping":{"match":{"query":"baby there","mode":"unordered"}}}}}}}`, ids: []string{"3"}},
		{name: "before", query: `{"intervals":{"text":{"match":{"query":"cold","filter":{"before":{"match":{"query":"outside"}}}}}}}`, ids: []string{"2", "3"}},
		{name: "after", query: `{"intervals":{"text":{"match":{"query":"cold","filter":{"after":{"match":{"query":"outside"}}}}}}}`, ids: []string{"4"}},
		{name: "prefix", query: `{"intervals":{"text":{"all_of":{"intervals":[{"match":{"query":"cold"}},{"prefix":{"prefix":"out"}}]}}}}`, ids: []string{"2", "3", "4"}},
		{name: "wildcard", query: `{"intervals":{"text":{"all_of":{"intervals":[{"match":{"query":"cold"}},{"wildcard":{"pattern":"out?ide"}}]}}}}`, ids: []string{"2", "3", "4"}},
		{name: "fuzzy", query: `{"intervals":{"text":{"all_of":{"intervals":[{"fuzzy":{"term":"cald"}},{"prefix":{"prefix":"out"}}]}}}}`, ids: []string{"2", "3", "4"}},
	})
}
