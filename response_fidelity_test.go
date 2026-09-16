package osmem

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The expectations in this file were taken from OpenSearch 3.8.0 responses
// to the same requests (2.19.1 agrees unless noted). Values are compared as
// JSON text so number spelling (1.0, 12.989999771118164) is checked too.

type fidelityCheck struct {
	name, method, path string
	body               any
	status             int
	want               map[string]string // JSON pointer -> JSON text of the value
	absent             []string          // JSON pointers that must not exist
}

func runFidelityChecks(t *testing.T, c *Cluster, checks []fidelityCheck) {
	t.Helper()
	for _, fc := range checks {
		t.Run(fc.name, func(t *testing.T) {
			res, err := c.Do(fc.method, fc.path, fc.body)
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != fc.status {
				t.Fatalf("status %d, want %d: %s", res.StatusCode, fc.status, res.Body)
			}
			var doc any
			dec := json.NewDecoder(strings.NewReader(string(res.Body)))
			dec.UseNumber()
			if err := dec.Decode(&doc); err != nil {
				t.Fatalf("decode: %v: %s", err, res.Body)
			}
			for pointer, want := range fc.want {
				got, ok := compatibilityPointer(doc, pointer)
				if !ok {
					t.Errorf("%s missing, want %s: %s", pointer, want, res.Body)
					continue
				}
				var buf strings.Builder
				enc := json.NewEncoder(&buf)
				enc.SetEscapeHTML(false)
				_ = enc.Encode(got)
				if text := strings.TrimSuffix(buf.String(), "\n"); text != want {
					t.Errorf("%s = %s, want %s", pointer, text, want)
				}
			}
			for _, pointer := range fc.absent {
				if got, ok := compatibilityPointer(doc, pointer); ok {
					t.Errorf("%s = %v, want absent", pointer, got)
				}
			}
		})
	}
}

func seedFidelity(t *testing.T) *Cluster {
	t.Helper()
	c := New()
	mustDo(t, c, http.MethodPut, "/fidelity", `{"mappings":{"properties":{
		"title":{"type":"text"},"year":{"type":"integer"},"pages":{"type":"long"},"price":{"type":"float"},
		"half":{"type":"half_float"},"score_s":{"type":"scaled_float","scaling_factor":100},"rating":{"type":"double"},
		"published":{"type":"date"},"tags":{"type":"keyword"},"location":{"type":"geo_point"},"in_stock":{"type":"boolean"}}}}`)
	if err := c.BulkString(`{"index":{"_index":"fidelity","_id":"10"}}
{"title":"The Quick Brown Fox","year":2001,"pages":320,"price":12.99,"half":1.1,"score_s":3.14159,"rating":4.5,"published":"2001-03-15","tags":["fiction","animals"],"location":{"lat":35.68,"lon":139.76},"in_stock":true}
{"index":{"_index":"fidelity","_id":"1"}}
{"title":"Search Engines in Action","year":2015,"pages":512,"price":45.0,"half":3.3,"score_s":9.99,"rating":4.8,"published":"2015-06-01T10:00:00.000+09:00","tags":["tech","search"],"location":[2.35,48.85],"in_stock":true}
{"index":{"_index":"fidelity","_id":"x-5"}}
{"title":"Untitled"}
`); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestResponseNumbersAndFieldValues(t *testing.T) {
	c := seedFidelity(t)
	defer c.Close()
	ids1 := `{"ids":{"values":["1"]}}`
	ids10 := `{"ids":{"values":["10"]}}`
	runFidelityChecks(t, c, []fidelityCheck{
		{name: "scores are floats", method: http.MethodPost, path: "/fidelity/_search", body: `{"size":1}`, status: 200,
			want: map[string]string{"/hits/max_score": "1.0", "/hits/hits/0/_score": "1.0"}},
		{name: "match_all boost", method: http.MethodPost, path: "/fidelity/_search", body: `{"size":1,"query":{"match_all":{"boost":3}}}`, status: 200,
			want: map[string]string{"/hits/hits/0/_score": "3.0"}},
		{name: "must_not only scores zero", method: http.MethodPost, path: "/fidelity/_search", body: `{"size":1,"query":{"bool":{"must_not":[{"ids":{"values":["1"]}}]}}}`, status: 200,
			want: map[string]string{"/hits/max_score": "0.0"}},
		{name: "integer missing sentinel", method: http.MethodPost, path: "/fidelity/_search", body: `{"sort":[{"year":"asc"}]}`, status: 200,
			want: map[string]string{"/hits/hits/2/sort": "[2147483647]", "/hits/hits/0/sort": "[2001]"}},
		{name: "long missing sentinel", method: http.MethodPost, path: "/fidelity/_search", body: `{"sort":[{"pages":"desc"}]}`, status: 200,
			want: map[string]string{"/hits/hits/2/sort": "[-9223372036854775808]"}},
		{name: "float sort values", method: http.MethodPost, path: "/fidelity/_search", body: `{"sort":[{"price":"asc"}]}`, status: 200,
			want: map[string]string{"/hits/hits/0/sort": "[12.99]", "/hits/hits/1/sort": "[45.0]", "/hits/hits/2/sort": `["Infinity"]`}},
		{name: "half_float sort values", method: http.MethodPost, path: "/fidelity/_search", body: `{"sort":[{"half":"asc"}]}`, status: 200,
			want: map[string]string{"/hits/hits/0/sort": "[1.0996094]"}},
		{name: "docvalue_fields precision and order", method: http.MethodPost, path: "/fidelity/_search",
			body: `{"_source":false,"query":` + ids10 + `,"docvalue_fields":["price","half","score_s","tags","published","year","in_stock"]}`, status: 200,
			want: map[string]string{
				"/hits/hits/0/fields/price":     "[12.989999771118164]",
				"/hits/hits/0/fields/half":      "[1.099609375]",
				"/hits/hits/0/fields/score_s":   "[3.14]",
				"/hits/hits/0/fields/tags":      `["animals","fiction"]`,
				"/hits/hits/0/fields/published": `["2001-03-15T00:00:00.000Z"]`,
				"/hits/hits/0/fields/year":      "[2001]",
				"/hits/hits/0/fields/in_stock":  "[true]",
			}},
		{name: "docvalue_fields dates and geo points", method: http.MethodPost, path: "/fidelity/_search",
			body: `{"_source":false,"query":` + ids1 + `,"docvalue_fields":["published","location","price",{"field":"rating","format":"0.00"}]}`, status: 200,
			want: map[string]string{
				"/hits/hits/0/fields/published": `["2015-06-01T01:00:00.000Z"]`,
				"/hits/hits/0/fields/location":  `["48.8499999884516, 2.3499999940395355"]`,
				"/hits/hits/0/fields/price":     "[45.0]",
				"/hits/hits/0/fields/rating":    `["4.80"]`,
			}},
		{name: "fields option", method: http.MethodPost, path: "/fidelity/_search",
			body: `{"_source":false,"query":` + ids1 + `,"fields":["published","location","price","score_s","half"]}`, status: 200,
			want: map[string]string{
				"/hits/hits/0/fields/published": `["2015-06-01T01:00:00.000Z"]`,
				"/hits/hits/0/fields/location":  `[{"coordinates":[2.35,48.85],"type":"Point"}]`,
				"/hits/hits/0/fields/price":     "[45.0]",
				"/hits/hits/0/fields/score_s":   "[9.99]",
				"/hits/hits/0/fields/half":      "[3.3]",
			}},
		{name: "aggregation numbers", method: http.MethodPost, path: "/fidelity/_search",
			body: `{"size":0,"aggs":{"t":{"terms":{"field":"price"}},"s":{"sum":{"field":"year"}},"e":{"sum":{"field":"price","missing":0}},"m":{"min":{"field":"in_stock"}}}}`, status: 200,
			want: map[string]string{
				"/aggregations/t/buckets/0/key":   "12.989999771118164",
				"/aggregations/t/buckets/1/key":   "45.0",
				"/aggregations/s/value":           "4016.0",
				"/aggregations/m/value":           "1.0",
				"/aggregations/m/value_as_string": `"true"`,
			}},
		{name: "empty sum is a double", method: http.MethodPost, path: "/fidelity/_search",
			body: `{"size":0,"query":{"term":{"tags":"none"}},"aggs":{"s":{"sum":{"field":"price"}},"a":{"avg":{"field":"price"}}}}`, status: 200,
			want: map[string]string{"/aggregations/s/value": "0.0", "/aggregations/a/value": "null"}},
		{name: "no index to search", method: http.MethodPost, path: "/fidelity-missing*/_search", body: `{}`, status: 200,
			want: map[string]string{"/hits/max_score": "0.0", "/hits/total/value": "0"}},
	})
}

func TestSearchErrorShapesAndValidation(t *testing.T) {
	c := seedFidelity(t)
	defer c.Close()
	runFidelityChecks(t, c, []fidelityCheck{
		{name: "sort on text", method: http.MethodPost, path: "/fidelity/_search", body: `{"sort":[{"title":"asc"}]}`, status: 400,
			want: map[string]string{
				"/error/type":                        `"search_phase_execution_exception"`,
				"/error/root_cause/0/type":           `"illegal_argument_exception"`,
				"/error/failed_shards/0/index":       `"fidelity"`,
				"/error/failed_shards/0/reason/type": `"illegal_argument_exception"`,
				"/error/caused_by/type":              `"illegal_argument_exception"`,
				"/error/caused_by/caused_by/type":    `"illegal_argument_exception"`,
			},
			absent: []string{"/error/root_cause/0/index"}},
		{name: "sort on unmapped field", method: http.MethodPost, path: "/fidelity/_search", body: `{"sort":[{"nope":"asc"}]}`, status: 400,
			want: map[string]string{
				"/error/root_cause/0/type":  `"query_shard_exception"`,
				"/error/root_cause/0/index": `"fidelity"`,
				"/error/caused_by/type":     `"query_shard_exception"`,
				"/error/caused_by/reason":   `"No mapping found for [nope] in order to sort on"`,
			}},
		{name: "sort order enum", method: http.MethodPost, path: "/fidelity/_search", body: `{"sort":[{"year":"sideways"}]}`, status: 400,
			want: map[string]string{"/error/type": `"illegal_argument_exception"`, "/error/reason": `"No enum constant org.opensearch.search.sort.SortOrder.SIDEWAYS"`}},
		{name: "field sort has no format", method: http.MethodPost, path: "/fidelity/_search", body: `{"sort":[{"published":{"order":"asc","format":"yyyy"}}]}`, status: 400,
			want: map[string]string{"/error/type": `"x_content_parse_exception"`}},
		{name: "geo_point sort", method: http.MethodPost, path: "/fidelity/_search", body: `{"sort":[{"location":"asc"}]}`, status: 400,
			want: map[string]string{"/error/root_cause/0/reason": `"can't sort on geo_point field without using specific sorting feature, like geo_distance"`}},
		{name: "number parse chain", method: http.MethodPost, path: "/fidelity/_search", body: `{"query":{"term":{"year":"abc"}}}`, status: 400,
			want: map[string]string{
				"/error/root_cause/0/type":                     `"query_shard_exception"`,
				"/error/failed_shards/0/reason/caused_by/type": `"number_format_exception"`,
				"/error/caused_by/caused_by/reason":            `"For input string: \"abc\""`,
			}},
		{name: "date parse chain", method: http.MethodPost, path: "/fidelity/_search", body: `{"query":{"term":{"published":"not-a-date"}}}`, status: 400,
			want: map[string]string{
				"/error/root_cause/0/type":                  `"parse_exception"`,
				"/error/failed_shards/0/reason/type":        `"query_shard_exception"`,
				"/error/caused_by/type":                     `"parse_exception"`,
				"/error/caused_by/caused_by/caused_by/type": `"date_time_parse_exception"`,
			}},
		{name: "query_string syntax", method: http.MethodPost, path: "/fidelity/_search", body: `{"query":{"query_string":{"query":"title:(foo"}}}`, status: 400,
			want: map[string]string{
				"/error/root_cause/0/type":        `"query_shard_exception"`,
				"/error/root_cause/0/reason":      `"Failed to parse query [title:(foo]"`,
				"/error/caused_by/caused_by/type": `"parse_exception"`,
			}},
		{name: "malformed JSON", method: http.MethodPost, path: "/fidelity/_search", body: `{not json`, status: 400,
			want: map[string]string{"/error/type": `"json_parse_exception"`, "/error/root_cause/0/type": `"json_parse_exception"`}},
		{name: "term with two fields", method: http.MethodPost, path: "/fidelity/_search", body: `{"query":{"term":{"a":"x","b":"y"}}}`, status: 400,
			want: map[string]string{"/error/type": `"parsing_exception"`, "/error/reason": `"[term] query doesn't support multiple fields, found [a] and [b]"`}},
		{name: "match without text", method: http.MethodPost, path: "/fidelity/_search", body: `{"query":{"match":{}}}`, status: 400,
			want: map[string]string{"/error/reason": `"No text specified for text query"`}},
		{name: "track_total_hits disabled by -1", method: http.MethodPost, path: "/fidelity/_search", body: `{"track_total_hits":-1,"size":0}`, status: 200,
			absent: []string{"/hits/total"}},
		{name: "track_total_hits below -1", method: http.MethodPost, path: "/fidelity/_search", body: `{"track_total_hits":-5}`, status: 400,
			want: map[string]string{
				"/error/type":             `"search_phase_execution_exception"`,
				"/error/phase":            `"fetch"`,
				"/error/root_cause":       "[]",
				"/error/failed_shards":    "[]",
				"/error/caused_by/reason": `"value must be >= 0, got -5"`,
			}},
		{name: "missing index", method: http.MethodPost, path: "/fidelity-nope/_search", body: `{}`, status: 404,
			want: map[string]string{
				"/error/root_cause/0/resource.type": `"index_or_alias"`,
				"/error/root_cause/0/resource.id":   `"fidelity-nope"`,
			}},
		{name: "negative from", method: http.MethodPost, path: "/fidelity/_search", body: `{"from":-1}`, status: 400,
			want: map[string]string{"/error/reason": `"[from] parameter cannot be negative, found [-1]"`}},
		// OpenSearch 3.x rejects this; 2.19 still accepted it
		{name: "source include and exclude conflict", method: http.MethodPost, path: "/fidelity/_search?_source_includes=title,year&_source_excludes=year", body: `{}`, status: 400,
			want: map[string]string{"/error/reason": `"The same entry [year] cannot be both included and excluded in _source."`}},
	})
}

func TestStoredFieldsAndWriteResponses(t *testing.T) {
	c := seedFidelity(t)
	defer c.Close()
	runFidelityChecks(t, c, []fidelityCheck{
		{name: "stored_fields none", method: http.MethodPost, path: "/fidelity/_search", body: `{"stored_fields":"_none_","query":{"ids":{"values":["1"]}}}`, status: 200,
			want:   map[string]string{"/hits/hits/0/_index": `"fidelity"`},
			absent: []string{"/hits/hits/0/_id", "/hits/hits/0/_source"}},
		{name: "stored_fields hides source", method: http.MethodPost, path: "/fidelity/_search", body: `{"stored_fields":["title"],"query":{"ids":{"values":["1"]}}}`, status: 200,
			want:   map[string]string{"/hits/hits/0/_id": `"1"`},
			absent: []string{"/hits/hits/0/_source"}},
		{name: "get with stored_fields", method: http.MethodGet, path: "/fidelity/_doc/1?stored_fields=title", status: 200,
			want:   map[string]string{"/found": "true"},
			absent: []string{"/_source"}},
		{name: "get on missing index", method: http.MethodGet, path: "/fidelity-nope/_doc/1", status: 404,
			want: map[string]string{"/error/resource.type": `"index_expression"`, "/error/root_cause/0/resource.type": `"index_expression"`}},
		{name: "mget without documents", method: http.MethodPost, path: "/fidelity/_mget", body: `{}`, status: 400,
			want: map[string]string{"/error/type": `"action_request_validation_exception"`, "/error/reason": `"Validation Failed: 1: no documents to get;"`}},
		{name: "create conflict carries the shard", method: http.MethodPut, path: "/fidelity/_create/1", body: `{"title":"again"}`, status: 409,
			want: map[string]string{"/error/shard": `"0"`, "/error/root_cause/0/shard": `"0"`}},
		{name: "forced refresh", method: http.MethodPut, path: "/fidelity/_doc/2?refresh=true", body: `{"title":"new"}`, status: 201,
			want: map[string]string{"/forced_refresh": "true"}},
		{name: "forced refresh in bulk", method: http.MethodPost, path: "/_bulk?refresh", body: "{\"index\":{\"_index\":\"fidelity\",\"_id\":\"3\"}}\n{\"title\":\"bulk\"}\n", status: 200,
			want: map[string]string{"/items/0/index/forced_refresh": "true"}},
		{name: "no forced refresh without the parameter", method: http.MethodPut, path: "/fidelity/_doc/4", body: `{"title":"plain"}`, status: 201,
			absent: []string{"/forced_refresh"}},
	})
}
