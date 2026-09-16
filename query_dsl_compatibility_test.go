package osmem

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
)

// Query DSL behaviour checked against OpenSearch 3.8 responses.

func dslIndex(t *testing.T, c *Cluster, name, body string, docs ...string) {
	t.Helper()
	if st, res := status(t, c, http.MethodPut, "/"+name, body); st != http.StatusOK {
		t.Fatalf("create %s: %d %v", name, st, res)
	}
	var sb strings.Builder
	for i, d := range docs {
		fmt.Fprintf(&sb, "{\"index\":{\"_index\":%q,\"_id\":\"%d\"}}\n%s\n", name, i+1, d)
	}
	if st, res := status(t, c, http.MethodPost, "/_bulk?refresh=true", sb.String()); st != http.StatusOK || res["errors"] == true {
		t.Fatalf("bulk %s: %d %v", name, st, res)
	}
}

// dslSearch runs a query sorted by _id and returns the ids and hits.
func dslSearch(t *testing.T, c *Cluster, index, query string) ([]string, []any, int, map[string]any) {
	t.Helper()
	st, res := status(t, c, http.MethodPost, "/"+index+"/_search", `{"query": `+query+`, "sort": ["_id"], "size": 100, "track_scores": true}`)
	if st != http.StatusOK {
		return nil, nil, st, res
	}
	hits := res["hits"].(map[string]any)["hits"].([]any)
	ids := []string{}
	for _, h := range hits {
		ids = append(ids, h.(map[string]any)["_id"].(string))
	}
	return ids, hits, st, res
}

type dslCase struct {
	name  string
	query string
	ids   []string // expected ids (sorted by _id)
	root  string   // expected root cause type of a 400 error
	why   string   // expected root cause reason (prefix)
}

func runDSLCases(t *testing.T, c *Cluster, index string, cases []dslCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids, _, st, res := dslSearch(t, c, index, tc.query)
			if tc.root != "" {
				typ, reason := rootCause(res)
				if st != http.StatusBadRequest || typ != tc.root || !strings.HasPrefix(reason, tc.why) {
					t.Fatalf("status=%d root=%s reason=%q body=%v", st, typ, reason, res)
				}
				return
			}
			if st != http.StatusOK {
				t.Fatalf("status=%d body=%v", st, res)
			}
			if strings.Join(ids, ",") != strings.Join(tc.ids, ",") {
				t.Fatalf("ids = %v, want %v", ids, tc.ids)
			}
		})
	}
}

func TestQueryStringLuceneSemantics(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "qs", `{"mappings":{"properties":{"d":{"type":"text"},"k":{"type":"keyword"},"n":{"type":"integer"}}}}`,
		`{"d":"lazy dog"}`, `{"d":"twist","k":"al*ce"}`, `{"d":"lazy","k":"alice","n":5}`, `{"d":"quick brown fox"}`, `{"d":"quick lazy dog"}`)
	runDSLCases(t, c, "qs", []dslCase{
		{name: "AND binds the previous clause", query: `{"query_string":{"query":"d:lazy AND d:dog OR d:twist"}}`, ids: []string{"1", "5"}},
		{name: "OR with default AND", query: `{"query_string":{"query":"lazy OR twist dog","default_field":"d","default_operator":"AND"}}`, ids: []string{"1", "5"}},
		{name: "bang is NOT", query: `{"query_string":{"query":"d:lazy && !d:dog"}}`, ids: []string{"3"}},
		{name: "bare bang is a term", query: `{"query_string":{"query":"lazy && ! dog","default_field":"d"}}`, ids: []string{"1", "3", "5"}},
		{name: "phrase slop", query: `{"query_string":{"query":"\"quick fox\"~1","default_field":"d"}}`, ids: []string{"4"}},
		{name: "regexp term", query: `{"query_string":{"query":"/qu.*k/","default_field":"d"}}`, ids: []string{"4", "5"}},
		{name: "escaped wildcard", query: `{"query_string":{"query":"k:al\\*ce"}}`, ids: []string{"2"}},
		{name: "whitespace terms analyzed together", query: `{"query_string":{"query":"quick lazy dog","default_field":"d","minimum_should_match":"2"}}`, ids: []string{"1", "5"}},
		{name: "pure negative group", query: `{"query_string":{"query":"(-lazy)","default_field":"d"}}`, ids: []string{"2", "4"}},
		{name: "explicit numeric field is not lenient", query: `{"query_string":{"query":"abc","default_field":"n"}}`, root: "query_shard_exception", why: `failed to create query: For input string: "abc"`},
		{name: "leading wildcard", query: `{"query_string":{"query":"*ick","default_field":"d","allow_leading_wildcard":false}}`, root: "query_shard_exception", why: "Failed to parse query [*ick]"},
		{name: "trailing operator", query: `{"query_string":{"query":"quick AND"}}`, root: "query_shard_exception", why: "Failed to parse query [quick AND]"},
		{name: "unknown parameter", query: `{"query_string":{"query":"quick","split_on_whitespace":false}}`, root: "parsing_exception", why: "[query_string] query does not support [split_on_whitespace]"},
	})
}

func TestSimpleQueryStringOperators(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "sqs", `{"mappings":{"properties":{"d":{"type":"text"}}}}`,
		`{"d":"quick lazy"}`, `{"d":"quick"}`, `{"d":"other"}`, `{"d":"quick very lazy"}`)
	runDSLCases(t, c, "sqs", []dslCase{
		{name: "negation is optional", query: `{"simple_query_string":{"query":"quick -lazy","fields":["d"]}}`, ids: []string{"1", "2", "3", "4"}},
		{name: "operator precedence", query: `{"simple_query_string":{"query":"quick | other +lazy","fields":["d"]}}`, ids: []string{"1", "4"}},
		{name: "phrase slop", query: `{"simple_query_string":{"query":"\"quick lazy\"~1","fields":["d"]}}`, ids: []string{"1", "4"}},
		{name: "bare fuzzy", query: `{"simple_query_string":{"query":"qxxck~","fields":["d"]}}`, ids: []string{"1", "2", "4"}},
		{name: "unclosed quote", query: `{"simple_query_string":{"query":"\"unclosed quick","fields":["d"]}}`, ids: []string{"1", "2", "4"}},
		{name: "flags NONE", query: `{"simple_query_string":{"query":"quick -lazy","fields":["d"],"flags":"NONE"}}`, ids: []string{"1", "2", "4"}},
		{name: "minimum_should_match", query: `{"simple_query_string":{"query":"quick lazy","fields":["d"],"minimum_should_match":"2"}}`, ids: []string{"1", "4"}},
		{name: "unknown flag", query: `{"simple_query_string":{"query":"quick","flags":"FOO"}}`, root: "illegal_argument_exception", why: "Unknown simple_query_string flag [FOO]"},
	})
}

func TestMatchQueryCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "mq", `{"mappings":{"properties":{"d":{"type":"text"},"k":{"type":"keyword"},"w":{"type":"text"},"y":{"type":"integer"},"s":{"type":"text","analyzer":"stop"},"g":{"type":"text","position_increment_gap":0},"b":{"type":"text"}}}}`,
		`{"d":"quick lazy dog","k":"alice","w":"abcd","s":"fox a dog","b":"quick fox"}`,
		`{"d":"quick","k":"bob","w":"abce","s":"fox dog","b":["the lazy","dog barks"]}`,
		`{"w":"abcf","g":["the lazy","dog"],"y":2015}`)
	runDSLCases(t, c, "mq", []dslCase{
		{name: "msm above the clause count", query: `{"match":{"d":{"query":"quick lazy dog","minimum_should_match":4}}}`, ids: []string{}},
		{name: "msm percentage above 100", query: `{"match":{"d":{"query":"quick lazy dog","minimum_should_match":"150%"}}}`, ids: []string{}},
		{name: "msm conditional", query: `{"match":{"d":{"query":"quick lazy dog","minimum_should_match":"2 < 50%"}}}`, ids: []string{"1", "2"}},
		{name: "msm with operator and", query: `{"match":{"d":{"query":"quick lazy dog","operator":"and","minimum_should_match":2}}}`, ids: []string{}},
		{name: "keyword with analyzer", query: `{"match":{"k":{"query":"ALICE bob","analyzer":"standard"}}}`, ids: []string{"1", "2"}},
		{name: "keyword fuzziness", query: `{"match":{"k":{"query":"alcie","fuzziness":1}}}`, ids: []string{"1"}},
		{name: "max_expansions", query: `{"match":{"w":{"query":"abcx","fuzziness":1,"max_expansions":1}}}`, ids: []string{"1"}},
		{name: "fuzzy transpositions off", query: `{"fuzzy":{"k":{"value":"lacie","transpositions":false,"fuzziness":1}}}`, ids: []string{}},
		{name: "zero_terms_query is case insensitive", query: `{"match":{"d":{"query":"the","analyzer":"stop","zero_terms_query":"ALL"}}}`, ids: []string{"1", "2", "3"}},
		{name: "phrase repeated term", query: `{"match_phrase":{"b":{"query":"fox fox","slop":1}}}`, ids: []string{}},
		{name: "phrase across values", query: `{"match_phrase":{"b":{"query":"lazy dog","slop":3}}}`, ids: []string{}},
		{name: "phrase stopword gap", query: `{"match_phrase":{"s":"fox the dog"}}`, ids: []string{"1"}},
		{name: "phrase without value gap", query: `{"match_phrase":{"g":"lazy dog"}}`, ids: []string{"3"}},
		{name: "phrase prefix is positional", query: `{"match_phrase_prefix":{"b":"fox qu"}}`, ids: []string{}},
		{name: "phrase zero terms", query: `{"match_phrase":{"d":{"query":"the","analyzer":"stop","zero_terms_query":"all"}}}`, ids: []string{"1", "2", "3"}},
		{name: "bool prefix on keyword", query: `{"match_bool_prefix":{"k":"ali"}}`, ids: []string{"1"}},
		{name: "fuzziness on integer", query: `{"match":{"y":{"query":2015,"fuzziness":1}}}`, root: "query_shard_exception", why: "failed to create query: Can only use fuzzy queries on keyword and text fields - not on [y] which is of type [integer]"},
		{name: "invalid operator", query: `{"match":{"d":{"query":"quick","operator":"xor"}}}`, root: "illegal_argument_exception", why: "No enum constant org.opensearch.index.query.Operator.XOR"},
		{name: "invalid fuzziness", query: `{"match":{"d":{"query":"quick","fuzziness":"abc"}}}`, root: "illegal_argument_exception", why: "Invalid fuzziness value: abc"},
		{name: "unknown parameter", query: `{"match":{"d":{"query":"quick","slop":1}}}`, root: "parsing_exception", why: "[match] query does not support [slop]"},
		{name: "invalid msm", query: `{"match":{"d":{"query":"quick lazy dog","minimum_should_match":"75.5%"}}}`, root: "query_shard_exception", why: `failed to create query: For input string: "75.5"`},
	})
}

func TestTermLevelQueryCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "tl", `{"mappings":{"properties":{"k":{"type":"keyword"},"t":{"type":"text"},"n":{"type":"integer"}}}}`,
		`{"k":"alice","t":"Quick","n":1}`, `{"k":"bob"}`, `{"k":"a5"}`, `{"k":"a*b"}`, `{"k":"a\\b"}`)
	runDSLCases(t, c, "tl", []dslCase{
		{name: "regexp intersection", query: `{"regexp":{"k":"al.*&.*ce"}}`, ids: []string{"1"}},
		{name: "regexp anystring", query: `{"regexp":{"k":"@"}}`, ids: []string{"1", "2", "3", "4", "5"}},
		{name: "regexp interval", query: `{"regexp":{"k":"a<1-9>"}}`, ids: []string{"3"}},
		{name: "regexp has no inline flags", query: `{"regexp":{"k":"(?i)ALICE"}}`, ids: []string{}},
		{name: "regexp anchors are literal", query: `{"regexp":{"k":"^alice$"}}`, ids: []string{}},
		{name: "complement needs the flag", query: `{"regexp":{"k":{"value":"~(alice)","flags":"COMPLEMENT"}}}`, ids: []string{"2", "3", "4", "5"}},
		{name: "wildcard escaped star", query: `{"wildcard":{"k":"a\\*b"}}`, ids: []string{"4"}},
		{name: "wildcard escaped backslash", query: `{"wildcard":{"k":"a\\\\b"}}`, ids: []string{"5"}},
		{name: "case insensitive term on text", query: `{"term":{"t":{"value":"QUICK","case_insensitive":true}}}`, ids: []string{"1"}},
		{name: "unknown regexp flag", query: `{"regexp":{"k":{"value":"a.*","flags":"FOO"}}}`, root: "illegal_argument_exception", why: "Unknown regexp flag [FOO]"},
		{name: "regexp syntax", query: `{"regexp":{"k":"["}}`, root: "query_shard_exception", why: "failed to create query: unexpected end-of-string"},
		{name: "prefix on integer", query: `{"prefix":{"n":"1"}}`, root: "query_shard_exception", why: "Can only use prefix queries on keyword and text fields - not on [n] which is of type [integer]"},
		{name: "case insensitive integer", query: `{"term":{"n":{"value":1,"case_insensitive":true}}}`, root: "query_shard_exception", why: "[n] field which is of type [integer], does not support case insensitive term queries"},
		{name: "term array", query: `{"term":{"k":["alice"]}}`, root: "parsing_exception", why: "[term] query does not support array of values"},
		{name: "extra key", query: `{"term":{"k":"alice","boost":2}}`, root: "parsing_exception", why: "[term] query doesn't support multiple fields, found [k] and [boost]"},
		{name: "range without bounds", query: `{"range":{"n":{}}}`, ids: []string{"1"}},
		{name: "range scalar", query: `{"range":{"n":5}}`, root: "parsing_exception", why: "[range] query does not support [n]"},
		{name: "range two lower bounds", query: `{"range":{"n":{"gte":1,"gt":0}}}`, root: "parsing_exception", why: "invalid lower bound for [range] query"},
		{name: "ids empty id", query: `{"ids":{"values":[""]}}`, root: "query_shard_exception", why: "failed to create query: Ids can't be empty"},
	})
}

func TestBoolAndCompoundScoring(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "cs", `{"mappings":{"properties":{"k":{"type":"keyword"},"n":{"type":"integer"}}}}`,
		`{"k":"a","n":1}`, `{"k":"b","n":10}`, `{"k":["a","b"],"n":100}`, `{"k":"c"}`)
	runDSLCases(t, c, "cs", []dslCase{
		{name: "should only with msm 0", query: `{"bool":{"should":[{"term":{"k":"a"}}],"minimum_should_match":0}}`, ids: []string{"1", "3"}},
		{name: "msm above should count", query: `{"bool":{"should":[{"term":{"k":"a"}},{"term":{"k":"b"}}],"minimum_should_match":3}}`, ids: []string{}},
		{name: "msm without should", query: `{"bool":{"must":[{"term":{"k":"a"}}],"minimum_should_match":1}}`, ids: []string{}},
		{name: "pure negative without adjustment", query: `{"bool":{"must_not":[{"term":{"k":"a"}}],"adjust_pure_negative":false}}`, ids: []string{}},
		{name: "unknown bool field", query: `{"bool":{"foo":[]}}`, root: "x_content_parse_exception", why: "[1:20] [bool] unknown field [foo]"},
		{name: "boosting needs negative", query: `{"boosting":{"positive":{"match_all":{}},"negative_boost":0.5}}`, root: "parsing_exception", why: "[boosting] query requires 'negative' query to be set'"},
		{name: "dis_max tie breaker range", query: `{"dis_max":{"queries":[{"match_all":{}}],"tie_breaker":2}}`, root: "query_shard_exception", why: "failed to create query: tieBreakerMultiplier must be in [0, 1]"},
		{name: "function_score min_score", query: `{"function_score":{"query":{"match_all":{}},"functions":[{"filter":{"term":{"k":"a"}},"weight":10}],"min_score":5}}`, ids: []string{"1", "3"}},
		{name: "function_score boost_mode", query: `{"function_score":{"query":{"match_all":{}},"boost_mode":"foo"}}`, root: "illegal_argument_exception", why: "No enum constant org.opensearch.common.lucene.search.function.CombineFunction.FOO"},
		{name: "terms_set", query: `{"terms_set":{"k":{"terms":["a","b"],"minimum_should_match_field":"n"}}}`, ids: []string{"1"}},
	})
	scores := func(query string) map[string]float64 {
		_, hits, st, res := dslSearch(t, c, "cs", query)
		if st != http.StatusOK {
			t.Fatalf("%s: status=%d body=%v", query, st, res)
		}
		out := map[string]float64{}
		for _, h := range hits {
			hm := h.(map[string]any)
			f, _ := toFloatValue(hm["_score"])
			out[hm["_id"].(string)] = f
		}
		return out
	}
	expect := func(name, query string, want map[string]float64) {
		t.Run(name, func(t *testing.T) {
			got := scores(query)
			for id, w := range want {
				if math.Abs(got[id]-w) > 1e-5 {
					t.Fatalf("scores = %v, want %v", got, want)
				}
			}
		})
	}
	expect("dis_max tie", `{"dis_max":{"queries":[{"constant_score":{"filter":{"term":{"k":"a"}},"boost":2}},{"constant_score":{"filter":{"range":{"n":{"lte":10}}},"boost":3}}],"tie_breaker":0.5}}`,
		map[string]float64{"1": 4, "2": 3, "3": 2})
	expect("boosting", `{"boosting":{"positive":{"constant_score":{"filter":{"match_all":{}},"boost":2}},"negative":{"term":{"k":"a"}},"negative_boost":0.25}}`,
		map[string]float64{"1": 0.5, "2": 2, "4": 2})
	expect("function_score sum", `{"function_score":{"query":{"match_all":{}},"functions":[{"filter":{"term":{"k":"a"}},"weight":10},{"filter":{"range":{"n":{"lte":10}}},"weight":4}],"score_mode":"sum"}}`,
		map[string]float64{"1": 14, "2": 4, "3": 10, "4": 1})
	expect("function_score avg boost mode", `{"function_score":{"query":{"constant_score":{"filter":{"match_all":{}},"boost":3}},"functions":[{"filter":{"term":{"k":"a"}},"weight":10}],"boost_mode":"avg"}}`,
		map[string]float64{"1": 6.5, "2": 2})
	expect("function_score max_boost", `{"function_score":{"query":{"match_all":{}},"functions":[{"weight":10}],"max_boost":4}}`,
		map[string]float64{"1": 4, "4": 4})
	expect("field_value_factor", `{"function_score":{"query":{"exists":{"field":"n"}},"field_value_factor":{"field":"n","modifier":"log1p"}}}`,
		map[string]float64{"1": 0.30103})
}

func toFloatValue(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return float64(float32(t)), true
	case json.Number:
		f, err := t.Float64()
		return float64(float32(f)), err == nil
	}
	return 0, false
}

func TestMultiFieldQueryCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "mf", `{"settings":{"index.query.default_field":["a"]},"mappings":{"properties":{"a":{"type":"text"},"b":{"type":"text"},"y":{"type":"integer"},"t":{"type":"text"},"k":{"type":"keyword"}}}}`,
		`{"a":"x","y":2015,"t":"quick","k":"alice"}`, `{"b":"x"}`)
	runDSLCases(t, c, "mf", []dslCase{
		{name: "default_field setting query_string", query: `{"query_string":{"query":"x"}}`, ids: []string{"1"}},
		{name: "default_field setting multi_match", query: `{"multi_match":{"query":"x"}}`, ids: []string{"1"}},
		{name: "all fields include numbers", query: `{"multi_match":{"query":"2015","fields":["*"]}}`, ids: []string{"1"}},
		{name: "patterns are not lenient", query: `{"multi_match":{"query":"a","fields":["y*","t"]}}`, root: "query_shard_exception", why: `failed to create query: For input string: "a"`},
		{name: "cross_fields groups by analyzer", query: `{"multi_match":{"query":"alice quick","fields":["t","k"],"type":"cross_fields","operator":"and"}}`, ids: []string{}},
		{name: "fuzziness with cross_fields", query: `{"multi_match":{"query":"x","fields":["a"],"type":"cross_fields","fuzziness":1}}`, root: "parsing_exception", why: "Fuzziness not allowed for type [cross_fields]"},
	})
}

func TestGeoQueryFormats(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "geo", `{"mappings":{"properties":{"loc":{"type":"geo_point"}}}}`, `{"loc":{"lat":35.68,"lon":139.76}}`)
	runDSLCases(t, c, "geo", []dslCase{
		{name: "top_right and bottom_left", query: `{"geo_bounding_box":{"loc":{"top_right":{"lat":40,"lon":140},"bottom_left":{"lat":30,"lon":130}}}}`, ids: []string{"1"}},
		{name: "wkt bbox", query: `{"geo_bounding_box":{"loc":{"wkt":"BBOX (130, 140, 40, 30)"}}}`, ids: []string{"1"}},
		{name: "geohash corners", query: `{"geo_bounding_box":{"loc":{"top_left":"xn","bottom_right":"xn"}}}`, ids: []string{"1"}},
		{name: "geohash center", query: `{"geo_distance":{"distance":"100km","loc":"xn76u"}}`, ids: []string{"1"}},
		{name: "nautical miles", query: `{"geo_distance":{"distance":"54nmi","loc":{"lat":35.7,"lon":139.7}}}`, ids: []string{"1"}},
		{name: "top below bottom", query: `{"geo_bounding_box":{"loc":{"top_left":{"lat":30,"lon":130},"bottom_right":{"lat":40,"lon":140}}}}`, root: "illegal_argument_exception", why: "top is below bottom corner: 30.0 vs. 40.0"},
		{name: "negative distance", query: `{"geo_distance":{"distance":"-10km","loc":{"lat":35.7,"lon":139.7}}}`, root: "illegal_argument_exception", why: "distance must be greater than zero"},
	})
}

func TestMatchedQueries(t *testing.T) {
	c := New()
	defer c.Close()
	dslIndex(t, c, "nq", `{"mappings":{"properties":{"t":{"type":"text"},"k":{"type":"keyword"}}}}`,
		`{"t":"quick fox","k":"a"}`, `{"t":"quick brown","k":"b"}`, `{"t":"lazy","k":"c"}`)
	_, hits, st, res := dslSearch(t, c, "nq", `{"bool":{"should":[{"term":{"k":{"value":"a","_name":"ka"}}},{"match":{"t":{"query":"quick","_name":"mq"}}}],"_name":"outer"}}`)
	if st != http.StatusOK || len(hits) != 2 {
		t.Fatalf("status=%d body=%v", st, res)
	}
	want := [][]string{{"mq", "ka", "outer"}, {"mq", "outer"}}
	for i, h := range hits {
		got := fmt.Sprint(h.(map[string]any)["matched_queries"])
		if got != fmt.Sprint(want[i]) {
			t.Fatalf("hit %d matched_queries = %s, want %v", i, got, want[i])
		}
	}
}
