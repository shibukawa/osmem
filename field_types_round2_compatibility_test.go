package osmem

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// The expectations in this file were captured from OpenSearch 3.8.0.

// ft2Error returns the reason of an error response and of its cause.
func ft2Error(body map[string]any) (reason, causeType, causeReason string) {
	e, _ := body["error"].(map[string]any)
	reason, _ = e["reason"].(string)
	if c, ok := e["caused_by"].(map[string]any); ok {
		causeType, _ = c["type"].(string)
		causeReason, _ = c["reason"].(string)
	}
	return reason, causeType, causeReason
}

// ft2ExpectDocError indexes a document and checks the mapper failure.
func ft2ExpectDocError(t *testing.T, c *Cluster, path, doc, reason, causeType, causeReason string) {
	t.Helper()
	code, body := status(t, c, http.MethodPut, path, doc)
	r, ct, cr := ft2Error(body)
	if code != http.StatusBadRequest || r != reason || ct != causeType || cr != causeReason {
		t.Fatalf("%s %s: status=%d reason=%q cause=%s %q", path, doc, code, r, ct, cr)
	}
}

// ft2IDs runs a search and returns the sorted ids of the hits.
func ft2IDs(t *testing.T, c *Cluster, index, body string) []string {
	t.Helper()
	res := mustDo(t, c, http.MethodPost, "/"+index+"/_search", body)
	var ids []string
	for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
		ids = append(ids, h.(map[string]any)["_id"].(string))
	}
	sort.Strings(ids)
	return ids
}

func ft2ExpectIDs(t *testing.T, c *Cluster, index, body string, want ...string) {
	t.Helper()
	if got := ft2IDs(t, c, index, body); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: hits %v, want %v", body, got, want)
	}
}

// ft2Scores runs a search and returns the scores by id.
func ft2Scores(t *testing.T, c *Cluster, index, body string) map[string]float64 {
	t.Helper()
	res := mustDo(t, c, http.MethodPost, "/"+index+"/_search", body)
	scores := map[string]float64{}
	for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
		hit := h.(map[string]any)
		scores[hit["_id"].(string)], _ = hit["_score"].(float64)
	}
	return scores
}

// ft2ExpectSearchError runs a search and checks the root cause.
func ft2ExpectSearchError(t *testing.T, c *Cluster, index, body string, code int, rootType, rootReason string) {
	t.Helper()
	st, res := status(t, c, http.MethodPost, "/"+index+"/_search", body)
	e, _ := res["error"].(map[string]any)
	roots, _ := e["root_cause"].([]any)
	var rt, rr string
	if len(roots) > 0 {
		root := roots[0].(map[string]any)
		rt, _ = root["type"].(string)
		rr, _ = root["reason"].(string)
	}
	if st != code || rt != rootType || rr != rootReason {
		t.Fatalf("%s: status=%d root=%s %q", body, st, rt, rr)
	}
}

func TestFT2RangeFieldValues(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/ranges", `{"mappings":{"properties":{"ir":{"type":"integer_range"},"dr":{"type":"date_range"},"ipr":{"type":"ip_range"},"fr":{"type":"float_range"}}}}`)
	for id, doc := range map[string]string{
		"ir0": `{"ir":{"gte":1,"lte":10}}`, "ir1": `{"ir":{"gt":1,"lt":10}}`, "ir6": `{"ir":{"gte":20}}`, "ir7": `{"ir":{}}`,
		"ir8": `{"ir":{"gte":1.5}}`, "dr0": `{"dr":{"gte":"2024-01-01","lte":"2024-12-31"}}`,
		"ipr0": `{"ipr":"10.0.0.0/24"}`, "ipr1": `{"ipr":{"gte":"10.0.0.0","lte":"10.0.0.255"}}`,
	} {
		mustDo(t, c, http.MethodPut, "/ranges/_doc/"+id, doc)
	}
	mustDo(t, c, http.MethodPost, "/ranges/_refresh", nil)

	ft2ExpectDocError(t, c, "/ranges/_doc/ir2", `{"ir":{"gte":10,"lte":1}}`,
		"failed to parse field [ir] of type [integer_range] in document with id 'ir2'. Preview of field's value: 'null'",
		"illegal_argument_exception", "min value (10) is greater than max value (1)")
	ft2ExpectDocError(t, c, "/ranges/_doc/ir3", `{"ir":{"gte":"a"}}`,
		"failed to parse field [ir] of type [integer_range] in document with id 'ir3'. Preview of field's value: 'a'",
		"number_format_exception", `For input string: "a"`)
	ft2ExpectDocError(t, c, "/ranges/_doc/ir4", `{"ir":5}`,
		"failed to parse field [ir] of type [integer_range] in document with id 'ir4'. Preview of field's value: '5'",
		"mapper_parsing_exception", "error parsing field [ir], expected an object but got ir")
	ft2ExpectDocError(t, c, "/ranges/_doc/ir5", `{"ir":{"foo":1}}`,
		"failed to parse field [ir] of type [integer_range] in document with id 'ir5'. Preview of field's value: '1'",
		"mapper_parsing_exception", "error parsing field [ir], with unknown parameter [foo]")
	ft2ExpectDocError(t, c, "/ranges/_doc/fr1", `{"fr":{"gt":1.5,"lt":1.5}}`,
		"failed to parse field [fr] of type [float_range] in document with id 'fr1'. Preview of field's value: 'null'",
		"illegal_argument_exception", "min value (1.5000001) is greater than max value (1.4999999)")
	ft2ExpectDocError(t, c, "/ranges/_doc/ipr2", `{"ipr":"10.0.0.1"}`,
		"failed to parse field [ipr] of type [ip_range] in document with id 'ipr2'. Preview of field's value: '10.0.0.1'",
		"illegal_argument_exception", "Expected [ip/prefix] but was [10.0.0.1]")
	ft2ExpectDocError(t, c, "/ranges/_doc/dr1", `{"dr":{"gte":"now"}}`,
		"failed to parse field [dr] of type [date_range] in document with id 'dr1'. Preview of field's value: 'now'",
		"parse_exception", "could not read the current timestamp")

	ft2ExpectIDs(t, c, "ranges", `{"query":{"term":{"ir":5}}}`, "ir0", "ir1", "ir7", "ir8")
	ft2ExpectIDs(t, c, "ranges", `{"query":{"range":{"ir":{"gte":3,"lte":4,"relation":"within"}}}}`)
	ft2ExpectIDs(t, c, "ranges", `{"query":{"range":{"ir":{"gte":12,"lte":18,"relation":"contains"}}}}`, "ir7", "ir8")
	ft2ExpectIDs(t, c, "ranges", `{"query":{"range":{"ir":{"gt":4,"lt":6}}}}`, "ir0", "ir1", "ir7", "ir8")
	ft2ExpectIDs(t, c, "ranges", `{"query":{"term":{"dr":"2024-06-01"}}}`, "dr0")
	ft2ExpectIDs(t, c, "ranges", `{"query":{"term":{"ipr":"10.0.0.7"}}}`, "ipr0", "ipr1")
	ft2ExpectIDs(t, c, "ranges", `{"query":{"exists":{"field":"ir"}}}`, "ir0", "ir1", "ir6", "ir7", "ir8")
	ft2ExpectIDs(t, c, "ranges", `{"query":{"match":{"ir":"20"}}}`, "ir6", "ir7", "ir8")

	ft2ExpectSearchError(t, c, "ranges", `{"query":{"range":{"ir":{"gte":2,"lte":1}}}}`, http.StatusBadRequest,
		"query_shard_exception", "failed to create query: Range query `from` value (2) is greater than `to` value (1)")
	ft2ExpectSearchError(t, c, "ranges", `{"query":{"term":{"ir":5.5}}}`, http.StatusBadRequest,
		"query_shard_exception", "failed to create query: Value [5.5] has a decimal part")
	ft2ExpectSearchError(t, c, "ranges", `{"query":{"range":{"ir":{"gte":1,"relation":"disjoint"}}}}`, http.StatusBadRequest,
		"illegal_argument_exception", "[range] query does not support relation [disjoint]")
	ft2ExpectSearchError(t, c, "ranges", `{"query":{"prefix":{"ir":"1"}}}`, http.StatusBadRequest,
		"query_shard_exception", "Can only use prefix queries on keyword and text fields - not on [ir] which is of type [integer_range]")

	res := mustDo(t, c, http.MethodPost, "/ranges/_search", `{"query":{"ids":{"values":["ir8","dr0"]}},"fields":["ir","dr"],"_source":false}`)
	for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
		hit := h.(map[string]any)
		fields, _ := hit["fields"].(map[string]any)
		switch hit["_id"] {
		case "ir8":
			if r, _ := fields["ir"].([]any); len(r) != 1 || r[0].(map[string]any)["gte"] != 1.0 {
				t.Fatalf("fields ir: %v", fields)
			}
		case "dr0":
			if r, _ := fields["dr"].([]any); len(r) != 1 || r[0].(map[string]any)["lte"] != "2024-12-31T00:00:00.000Z" {
				t.Fatalf("fields dr: %v", fields)
			}
		}
	}
}

func TestFT2StructuredValueTypes(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/structured", `{"mappings":{"properties":{"kv":{"type":"knn_vector","dimension":2},"comp":{"type":"completion"},
		"flat":{"type":"flat_object"},"rf":{"type":"rank_feature"},"rfs":{"type":"rank_features"},"bin":{"type":"binary"}}}}`)

	ft2ExpectDocError(t, c, "/structured/_doc/kv1", `{"kv":[1.0,2.0,3.0]}`,
		"failed to parse field [kv] of type [knn_vector] in document with id 'kv1'. Preview of field's value: 'null'",
		"illegal_argument_exception", "Vector dimension mismatch. Expected: 2, Given: 3")
	ft2ExpectDocError(t, c, "/structured/_doc/kv2", `{"kv":"abc"}`,
		"failed to parse field [kv] of type [knn_vector] in document with id 'kv2'. Preview of field's value: 'abc'",
		"illegal_argument_exception", "Base64 encoded vector for field [kv] has invalid byte length [2], must be a multiple of 4 (float size)")
	ft2ExpectDocError(t, c, "/structured/_doc/kv3", `{"kv":[1.0,"NaN"]}`,
		"failed to parse field [kv] of type [knn_vector] in document with id 'kv3'. Preview of field's value: 'NaN'",
		"illegal_argument_exception", "KNN vector values cannot be NaN")
	ft2ExpectDocError(t, c, "/structured/_doc/comp1", `{"comp":{"input":"x","weight":-1}}`,
		"failed to parse", "illegal_argument_exception", "weight must be in the interval [0..2147483647], but was [-1]")
	ft2ExpectDocError(t, c, "/structured/_doc/comp2", `{"comp":{"bogus":1}}`,
		"failed to parse", "illegal_argument_exception", "unknown field name [bogus], must be one of [input, weight, contexts]")
	ft2ExpectDocError(t, c, "/structured/_doc/comp3", `{"comp":5}`,
		"failed to parse", "parsing_exception", "failed to parse [comp]: expected text or object, but got VALUE_NUMBER")
	ft2ExpectDocError(t, c, "/structured/_doc/flat1", `{"flat":"notobject"}`,
		"failed to parse field [flat] of type [flat_object] in document with id 'flat1'. Preview of field's value: 'notobject'",
		"parsing_exception", "[flat] unexpected token [VALUE_STRING] in flat_object field value")
	ft2ExpectDocError(t, c, "/structured/_doc/rf1", `{"rf":-1}`,
		"failed to parse field [rf] of type [rank_feature] in document with id 'rf1'. Preview of field's value: '-1'",
		"illegal_argument_exception", "featureValue must be a positive normal float, got: -1.0 for feature rf on field _feature which is less than the minimum positive normal float: 1.1754944E-38")
	ft2ExpectDocError(t, c, "/structured/_doc/rf2", `{"rf":[1,2]}`,
		"failed to parse field [rf] of type [rank_feature] in document with id 'rf2'. Preview of field's value: '2'",
		"illegal_argument_exception", "[rank_feature] fields do not support indexing multiple values for the same field [rf] in the same document")
	ft2ExpectDocError(t, c, "/structured/_doc/rfs1", `{"rfs":5}`,
		"failed to parse", "illegal_argument_exception", "[rank_features] fields must be json objects, expected a START_OBJECT but got: VALUE_NUMBER")
	ft2ExpectDocError(t, c, "/structured/_doc/bin1", `{"bin":{"a":1}}`,
		"failed to parse", "illegal_argument_exception", "Malformed content, found extra data after parsing: END_OBJECT")

	mustDo(t, c, http.MethodPut, "/structured/_doc/ok1", `{"kv":[1.0,2.0],"comp":{"input":["abc","abd"],"weight":3},"flat":{"a":{"b":"c"}},"rf":5.5,"rfs":{"a":1.5}}`)
	st, body := status(t, c, http.MethodPut, "/structured/_doc/ok2?refresh=true", `{"comp":"","flat":{"a":[1,2]}}`)
	if st != http.StatusCreated {
		t.Fatalf("ok2: %d %v", st, body)
	}
	ft2ExpectIDs(t, c, "structured", `{"query":{"term":{"flat.a.b":"c"}}}`, "ok1")
	ft2ExpectIDs(t, c, "structured", `{"query":{"term":{"flat":"c"}}}`, "ok1")
	ft2ExpectIDs(t, c, "structured", `{"query":{"term":{"flat.a":"2"}}}`, "ok2")
	ft2ExpectIDs(t, c, "structured", `{"query":{"exists":{"field":"flat.a.b"}}}`, "ok1")
	ft2ExpectIDs(t, c, "structured", `{"query":{"exists":{"field":"comp"}}}`, "ok1", "ok2")
	ft2ExpectIDs(t, c, "structured", `{"query":{"term":{"_ignored":"comp"}}}`, "ok2")
	ft2ExpectSearchError(t, c, "structured", `{"query":{"term":{"kv":1}}}`, http.StatusBadRequest,
		"query_shard_exception", "KNN vector do not support exact searching, use KNN queries instead: [kv]")
	ft2ExpectSearchError(t, c, "structured", `{"query":{"term":{"rfs":"a"}}}`, http.StatusBadRequest,
		"query_shard_exception", "failed to create query: Queries on [rank_features] fields are not supported")
	ft2ExpectSearchError(t, c, "structured", `{"size":0,"aggs":{"t":{"terms":{"field":"comp"}}}}`, http.StatusBadRequest,
		"illegal_argument_exception", "Fielddata is not supported on field [comp] of type [completion]")
}

func TestFT2JoinField(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/join", `{"mappings":{"properties":{"j":{"type":"join","relations":{"a":"b","b":"c"}}}}}`)
	mustDo(t, c, http.MethodPut, "/join/_doc/1", `{"j":"a"}`)
	mustDo(t, c, http.MethodPut, "/join/_doc/2?routing=1", `{"j":{"name":"b","parent":"1"}}`)
	mustDo(t, c, http.MethodPut, "/join/_doc/3?refresh=true", `{"j":{"name":["a"]}}`)
	ft2ExpectDocError(t, c, "/join/_doc/4", `{"j":{"name":"b"}}`, "failed to parse", "illegal_argument_exception", "[parent] is missing for join field [j]")
	ft2ExpectDocError(t, c, "/join/_doc/5", `{"j":{"name":"b","parent":"1"}}`, "failed to parse", "illegal_argument_exception", "[routing] is missing for join field [j]")
	ft2ExpectDocError(t, c, "/join/_doc/6", `{"j":"unknown"}`, "failed to parse", "illegal_argument_exception", "unknown join name [unknown] for field [j]")
	ft2ExpectDocError(t, c, "/join/_doc/7", `{"j":5}`, "failed to parse", "illegal_state_exception", "[null] expected START_OBJECT or VALUE_STRING but was: VALUE_NUMBER")

	ft2ExpectIDs(t, c, "join", `{"query":{"term":{"j":"a"}}}`, "1", "3")
	ft2ExpectIDs(t, c, "join", `{"query":{"term":{"j#a":"1"}}}`, "1", "2")

	// children may be added to an existing parent, never removed
	mustDo(t, c, http.MethodPut, "/join/_mapping", `{"properties":{"j":{"type":"join","relations":{"a":"b","b":["c","a"]}}}}`)
	rel := mustDo(t, c, http.MethodGet, "/join/_mapping", nil)["join"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)["j"].(map[string]any)["relations"].(map[string]any)
	if kids, _ := rel["b"].([]any); len(kids) != 2 || kids[0] != "a" || kids[1] != "c" || rel["a"] != "b" {
		t.Fatalf("relations after update: %v", rel)
	}
	code, body := status(t, c, http.MethodPut, "/join/_mapping", `{"properties":{"j":{"type":"join","relations":{"a":["b","d"],"b":"c"}}}}`)
	if r, _, _ := ft2Error(body); code != http.StatusBadRequest || r != "Mapper for [j] conflicts with existing mapping:\n[cannot remove child [a] in join field [j]]" {
		t.Fatalf("relations update: %d %v", code, body)
	}
	code, body = status(t, c, http.MethodPut, "/join/_mapping", `{"properties":{"j":{"type":"join","relations":{"a":"b","b":["a","c"],"d":"e","f":"e"}}}}`)
	if r, _, _ := ft2Error(body); code != http.StatusBadRequest || errType(body) != "illegal_argument_exception" || r != "invalid definition for join field [j]:\n[[e] cannot have multiple parents]" {
		t.Fatalf("relations update with multiple parents: %d %v", code, body)
	}
	code, body = status(t, c, http.MethodPut, "/join2", `{"mappings":{"properties":{"j":{"type":"join","relations":{"a":"b","c":"b"}}}}}`)
	if r, ct, _ := ft2Error(body); code != http.StatusBadRequest || ct != "illegal_argument_exception" ||
		r != "Failed to parse mapping [_doc]: invalid definition for join field [j]:\n[[b] cannot have multiple parents]" {
		t.Fatalf("multiple parents: %d %v", code, body)
	}
	code, body = status(t, c, http.MethodPut, "/join3", `{"mappings":{"properties":{"j":{"type":"join","relations":{"a":"b"}},"k":{"type":"join","relations":{"x":"y"}}}}}`)
	if r, _, _ := ft2Error(body); code != http.StatusBadRequest || r != "Failed to parse mapping [_doc]: Field [_parent_join] is defined more than once" {
		t.Fatalf("two join fields: %d %v", code, body)
	}
}

func TestFT2NestedIncludeInParentAndRoot(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/nip", `{"mappings":{"properties":{"p":{"type":"nested","include_in_parent":true,"properties":{"a":{"type":"keyword"},"n":{"type":"long"},
		"q":{"type":"nested","include_in_parent":true,"properties":{"x":{"type":"keyword"}}}}},"r":{"type":"nested","include_in_root":true,"properties":{"a":{"type":"keyword"}}},
		"s":{"type":"nested","properties":{"a":{"type":"keyword"}}}}}}`)
	mustDo(t, c, http.MethodPut, "/nip/_doc/1?refresh=true", `{"p":[{"a":"x","n":5,"q":[{"x":"qx"}]},{"a":"y","n":7}],"r":[{"a":"z"}],"s":[{"a":"sz"}]}`)

	ft2ExpectIDs(t, c, "nip", `{"query":{"term":{"p.a":"x"}}}`, "1")
	ft2ExpectIDs(t, c, "nip", `{"query":{"bool":{"must":[{"term":{"p.a":"x"}},{"term":{"p.n":7}}]}}}`, "1")
	ft2ExpectIDs(t, c, "nip", `{"query":{"term":{"p.q.x":"qx"}}}`, "1")
	ft2ExpectIDs(t, c, "nip", `{"query":{"term":{"r.a":"z"}}}`, "1")
	ft2ExpectIDs(t, c, "nip", `{"query":{"term":{"s.a":"sz"}}}`)
	ft2ExpectIDs(t, c, "nip", `{"query":{"exists":{"field":"p"}}}`, "1")
	res := mustDo(t, c, http.MethodPost, "/nip/_search", `{"size":0,"aggs":{"t":{"terms":{"field":"p.a"}},"n":{"sum":{"field":"p.n"}}}}`)
	aggs := res["aggregations"].(map[string]any)
	if buckets := aggs["t"].(map[string]any)["buckets"].([]any); len(buckets) != 2 || aggs["n"].(map[string]any)["value"] != 12.0 {
		t.Fatalf("aggregations: %v", aggs)
	}
}

func TestFT2CopyToTargets(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/cts", `{"mappings":{"dynamic":"strict","properties":{"a":{"type":"keyword","copy_to":"missing"}}}}`)
	code, body := status(t, c, http.MethodPut, "/cts/_doc/1", `{"a":"x"}`)
	if r, _, _ := ft2Error(body); code != http.StatusBadRequest || errType(body) != "strict_dynamic_mapping_exception" ||
		r != "mapping set to strict, dynamic introduction of [missing] within [_doc] is not allowed" {
		t.Fatalf("strict copy_to: %d %v", code, body)
	}
	mustDo(t, c, http.MethodPut, "/ctf", `{"mappings":{"dynamic":false,"properties":{"a":{"type":"keyword","copy_to":"missing"}}}}`)
	mustDo(t, c, http.MethodPut, "/ctf/_doc/1", `{"a":"x"}`)
	mapping := mustDo(t, c, http.MethodGet, "/ctf/_mapping", nil)
	if props := mapping["ctf"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any); props["missing"] != nil {
		t.Fatalf("dynamic false copy_to target mapped: %v", props)
	}

	mustDo(t, c, http.MethodPut, "/ct4", `{"mappings":{"properties":{"n":{"type":"nested","properties":{"a":{"type":"keyword","copy_to":["all","n.b"]},"b":{"type":"keyword"}}},
		"all":{"type":"keyword"},"k":{"type":"keyword","ignore_above":3,"copy_to":"all"},"num":{"type":"long","copy_to":"all"}}}}`)
	mustDo(t, c, http.MethodPut, "/ct4/_doc/1?refresh=true", `{"n":[{"a":"x1"},{"a":"x2"}],"k":"longer","num":[3,1]}`)
	res := mustDo(t, c, http.MethodPost, "/ct4/_search", `{"size":0,"aggs":{"all":{"terms":{"field":"all"}}}}`)
	var keys []string
	for _, b := range res["aggregations"].(map[string]any)["all"].(map[string]any)["buckets"].([]any) {
		keys = append(keys, b.(map[string]any)["key"].(string))
	}
	if strings.Join(keys, ",") != "1,3,longer,x1,x2" {
		t.Fatalf("copy_to doc values: %v", keys)
	}
	ft2ExpectIDs(t, c, "ct4", `{"query":{"term":{"all":"x1"}}}`, "1")
	res = mustDo(t, c, http.MethodPost, "/ct4/_search", `{"_source":false,"fields":["all"]}`)
	if hit := res["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any); hit["fields"] != nil {
		t.Fatalf("fields option reads the source only: %v", hit)
	}
}

func TestFT2MappingValidation(t *testing.T) {
	c := New()
	defer c.Close()
	for name, spec := range map[string]string{
		"pig-docs":      `{"type":"text","index_options":"docs","position_increment_gap":5}`,
		"pig-unindexed": `{"type":"text","index":false,"position_increment_gap":5}`,
	} {
		code, body := status(t, c, http.MethodPut, "/"+name, `{"mappings":{"properties":{"t":`+spec+`}}}`)
		if r, ct, _ := ft2Error(body); code != http.StatusBadRequest || ct != "illegal_argument_exception" ||
			r != "Failed to parse mapping [_doc]: Cannot set position_increment_gap on field [t] without positions enabled" {
			t.Fatalf("%s: %d %v", name, code, body)
		}
	}
	code, body := status(t, c, http.MethodPut, "/phrases", `{"mappings":{"properties":{"t":{"type":"text","index_options":"freqs","index_phrases":true}}}}`)
	if r, _, _ := ft2Error(body); code != http.StatusBadRequest || r != "Failed to parse mapping [_doc]: Cannot set index_phrases on field [t] if positions are not enabled" {
		t.Fatalf("index_phrases: %d %v", code, body)
	}

	templates := `[{"s":{"match":"s_*","mapping":{"type":"keyword"}}},{"o":{"match":"obj*","match_mapping_type":"object","mapping":{"type":"object"}}}]`
	mustDo(t, c, http.MethodPut, "/sat", `{"mappings":{"dynamic":"strict_allow_templates","dynamic_templates":`+templates+`}}`)
	mustDo(t, c, http.MethodPut, "/sat/_doc/1", `{"s_a":"x","objx":{"s_b":"y"}}`)
	code, body = status(t, c, http.MethodPut, "/sat/_doc/2", `{"objy":{"z":"y"}}`)
	if r, _, _ := ft2Error(body); code != http.StatusBadRequest || r != "mapping set to strict_allow_templates, dynamic introduction of [z] within [objy] is not allowed" {
		t.Fatalf("strict_allow_templates: %d %v", code, body)
	}
	mustDo(t, c, http.MethodPut, "/fat", `{"mappings":{"dynamic":"false_allow_templates","dynamic_templates":`+templates+`}}`)
	mustDo(t, c, http.MethodPut, "/fat/_doc/1?refresh=true", `{"s_a":"x","other":"y","plain":{"s_r":"z"}}`)
	props := mustDo(t, c, http.MethodGet, "/fat/_mapping", nil)["fat"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)
	if len(props) != 1 || props["s_a"] == nil {
		t.Fatalf("false_allow_templates mapping: %v", props)
	}

	mustDo(t, c, http.MethodPut, "/star", `{"mappings":{"dynamic_templates":[{"all":{"match_mapping_type":"*","mapping":{"type":"{dynamic_type}","meta":{"t":"{dynamic_type}"}}}}]}}`)
	mustDo(t, c, http.MethodPut, "/star/_doc/1", `{"o":{"x":1}}`)

	mustDo(t, c, http.MethodPut, "/imi", `{"settings":{"index.mapping.ignore_malformed":true},"mappings":{"properties":{"n":{"type":"integer"},"b":{"type":"boolean"}}}}`)
	mustDo(t, c, http.MethodPut, "/imi/_doc/1", `{"n":"abc"}`)
	mustDo(t, c, http.MethodPut, "/imi/_doc/2?refresh=true", `{"b":"notbool"}`)
	res := mustDo(t, c, http.MethodPost, "/imi/_search", `{"sort":[{"_id":"asc"}]}`)
	hits := res["hits"].(map[string]any)["hits"].([]any)
	if len(hits) != 2 || hits[0].(map[string]any)["_ignored"] == nil || hits[1].(map[string]any)["_ignored"] != nil {
		t.Fatalf("index level ignore_malformed: %v", hits)
	}

	code, body = status(t, c, http.MethodPut, "/mixed/_doc/1", `{"a":1,"z":[1,2.5]}`)
	if r, _, _ := ft2Error(body); code != http.StatusBadRequest || r != "mapper [z] cannot be changed from type [float] to [long]" {
		t.Fatalf("mixed array: %d %v", code, body)
	}
	code, body = status(t, c, http.MethodPut, "/mixed/_doc/2", `{"m":[1,2.5]}`)
	if r, _, _ := ft2Error(body); code != http.StatusBadRequest || r != "mapper [m] cannot be changed from type [long] to [float]" {
		t.Fatalf("mixed array first field: %d %v", code, body)
	}
}

func TestFT2MetadataAndSpecialQueries(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/meta", `{"mappings":{"properties":{"n":{"type":"long","ignore_malformed":true},"d":{"type":"date"},"b":{"type":"boolean"},
		"ip":{"type":"ip"},"t":{"type":"text"},"al":{"type":"alias","path":"t"},"c":{"type":"constant_keyword","value":"x"}}}}`)
	mustDo(t, c, http.MethodPut, "/meta/_doc/1", `{"n":"x","b":true,"t":"Quick Brown fox"}`)
	mustDo(t, c, http.MethodPut, "/meta/_doc/2?refresh=true", `{"n":1,"d":1705312800000.5}`)

	ft2ExpectIDs(t, c, "meta", `{"query":{"term":{"_ignored":"n"}}}`, "1")
	ft2ExpectIDs(t, c, "meta", `{"query":{"exists":{"field":"_ignored"}}}`, "1")
	ft2ExpectSearchError(t, c, "meta", `{"size":0,"aggs":{"t":{"terms":{"field":"_ignored"}}}}`, http.StatusBadRequest,
		"illegal_argument_exception", "Fielddata is not supported on field [_ignored] of type [_ignored]")
	ft2ExpectSearchError(t, c, "meta", `{"sort":[{"_ignored":"asc"}]}`, http.StatusBadRequest,
		"illegal_argument_exception", "Fielddata is not supported on field [_ignored] of type [_ignored]")
	ft2ExpectSearchError(t, c, "meta", `{"query":{"exists":{"field":"*"}}}`, http.StatusBadRequest,
		"query_shard_exception", "failed to create query: Cannot run exists query on [_feature]")
	ft2ExpectSearchError(t, c, "meta", `{"query":{"exists":{"field":"_*"}}}`, http.StatusBadRequest,
		"query_shard_exception", "failed to create query: Cannot run exists() query against the nested field path")
	ft2ExpectSearchError(t, c, "meta", `{"query":{"exists":{"field":"_s*"}}}`, http.StatusBadRequest,
		"query_shard_exception", "The _source field is not searchable")
	ft2ExpectSearchError(t, c, "meta", `{"query":{"ids":{"values":["2"]}},"fields":["d"]}`, http.StatusBadRequest,
		"illegal_argument_exception", "failed to parse date field [1.7053128000005E12] with format [strict_date_optional_time||epoch_millis]")

	ft2ExpectIDs(t, c, "meta", `{"query":{"term":{"b":{"value":"true","case_insensitive":true}}}}`, "1")
	ft2ExpectSearchError(t, c, "meta", `{"query":{"term":{"ip":{"value":"1.2.3.4","case_insensitive":true}}}}`, http.StatusBadRequest,
		"query_shard_exception", "[ip] field which is of type [ip], does not support case insensitive term queries")

	ft2ExpectIDs(t, c, "meta", `{"query":{"match":{"al":"quick"}}}`, "1")
	ft2ExpectIDs(t, c, "meta", `{"query":{"match_phrase":{"al":"brown fox"}}}`, "1")
	ft2ExpectIDs(t, c, "meta", `{"query":{"prefix":{"al":"qui"}}}`, "1")

	res := mustDo(t, c, http.MethodPost, "/meta/_search", `{"_source":false,"sort":["_id"],"docvalue_fields":["c"]}`)
	for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
		if got := h.(map[string]any)["fields"].(map[string]any)["c"].([]any); len(got) != 1 || got[0] != "meta" {
			t.Fatalf("constant_keyword field data: %v", h)
		}
	}
}

func TestFT2MappingRendering(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/render", `{"mappings":{"properties":{
		"k":{"type":"keyword","doc_values":true,"store":false,"eager_global_ordinals":false,"boost":2},
		"t":{"type":"text","search_analyzer":"simple","boost":2,"fields":{"raw":{"type":"keyword","doc_values":true}}},
		"t2":{"type":"text","analyzer":"standard","search_analyzer":"standard"},
		"n":{"type":"integer","boost":2,"coerce":true},
		"kv":{"type":"knn_vector","dimension":2,"data_type":"byte"}}}}`)
	props := mustDo(t, c, http.MethodGet, "/render/_mapping", nil)["render"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)
	k := props["k"].(map[string]any)
	if len(k) != 2 || k["boost"] != 2.0 {
		t.Fatalf("keyword: %v", k)
	}
	tf := props["t"].(map[string]any)
	if tf["analyzer"] != "default" || tf["search_analyzer"] != "simple" || tf["boost"] != 2.0 || len(tf["fields"].(map[string]any)["raw"].(map[string]any)) != 1 {
		t.Fatalf("text: %v", tf)
	}
	if t2 := props["t2"].(map[string]any); t2["analyzer"] != "standard" || t2["search_analyzer"] != nil {
		t.Fatalf("text with same analyzers: %v", t2)
	}
	if n := props["n"].(map[string]any); n["boost"] != nil || n["coerce"] != true {
		t.Fatalf("integer: %v", n)
	}
	if kv := props["kv"].(map[string]any); kv["data_type"] != "BYTE" {
		t.Fatalf("knn_vector: %v", kv)
	}
}

func TestFT2RoutingRequiredWithoutID(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/rr", `{"mappings":{"_routing":{"required":true}}}`)
	npe := func(code int, body map[string]any) bool {
		e, _ := body["error"].(map[string]any)
		_, hasIndex := e["index"]
		return code == http.StatusInternalServerError && e["type"] == "null_pointer_exception" && e["reason"] == "id must not be null" && !hasIndex
	}
	if code, body := status(t, c, http.MethodPost, "/rr/_doc", `{"a":1}`); !npe(code, body) {
		t.Fatalf("index request without id and routing: %d %v", code, body)
	}
	// the bulk request fails as a whole before any item runs
	bulk := "{\"index\":{\"_index\":\"rr\",\"_id\":\"1\",\"routing\":\"x\"}}\n{\"a\":1}\n{\"create\":{\"_index\":\"rr\"}}\n{\"a\":1}\n"
	if code, body := status(t, c, http.MethodPost, "/_bulk", bulk); !npe(code, body) {
		t.Fatalf("bulk: %d %v", code, body)
	}
	mustDo(t, c, http.MethodPost, "/rr/_doc?routing=x&refresh=true", `{"a":1}`)
	if res := mustDo(t, c, http.MethodPost, "/rr/_count", nil); res["count"] != float64(1) {
		t.Fatalf("count = %v", res["count"])
	}
	if code, body := status(t, c, http.MethodPut, "/rr/_doc/2", `{"a":1}`); code != http.StatusBadRequest || errType(body) != "routing_missing_exception" {
		t.Fatalf("explicit id: %d %v", code, body)
	}
}

func TestFT2FieldNameLengthOrder(t *testing.T) {
	c := New()
	defer c.Close()
	expect := func(code int, body map[string]any, name, limit string) {
		t.Helper()
		e, _ := body["error"].(map[string]any)
		if want := "Field name [" + name + "] is longer than the limit of [" + limit + "] characters"; code != http.StatusBadRequest || e["type"] != "illegal_argument_exception" || e["reason"] != want {
			t.Fatalf("status=%d error=%v, want %q", code, e, want)
		}
	}
	for _, tc := range []struct{ limit, props, name string }{
		// metadata mappers count once the mapping has fields
		{"20", `{"abcdefghijklmnopqrstuvwxy":{"type":"keyword"}}`, "_data_stream_timestamp"},
		// object mappers are checked before field mappers
		{"20", `{"abcdefghijklmnopqrstuvwxy":{"properties":{"a":{"type":"keyword"}}}}`, "abcdefghijklmnopqrstuvwxy"},
		{"22", `{"abcdefghijklmnopqrstuvw":{"properties":{"zyxwvutsrqponmlkjihgfedcba":{"type":"keyword"}}}}`, "abcdefghijklmnopqrstuvw"},
		{"20", `{"nestednestednestednested":{"type":"nested","properties":{"x":{"type":"keyword"}}}}`, "nestednestednestednested"},
		// field mappers in hash map order of their full paths
		{"22", `{"aaaaaaaaaaaaaaaaaaaaaaaa":{"type":"keyword"},"bbbbbbbbbbbbbbbbbbbbbbbbb":{"type":"keyword"},"ccccccccccccccccccccccccccc":{"type":"long"}}`, "bbbbbbbbbbbbbbbbbbbbbbbbb"},
		{"22", `{"o":{"properties":{"pppppppppppppppppppppppp":{"type":"keyword"},"qqqqqqqqqqqqqqqqqqqqqqqqqqqq":{"type":"keyword"}}},"rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr":{"type":"keyword"}}`, "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr"},
		{"22", `{"k":{"type":"keyword","fields":{"rawrawrawrawrawrawrawraw":{"type":"keyword"}}}}`, "rawrawrawrawrawrawrawraw"},
		{"22", `{"k":{"type":"keyword"},"aliasaliasaliasaliasalias":{"type":"alias","path":"k"}}`, "aliasaliasaliasaliasalias"},
	} {
		code, body := status(t, c, http.MethodPut, "/fnl", `{"settings":{"index.mapping.field_name_length.limit":`+tc.limit+`},"mappings":{"properties":`+tc.props+`}}`)
		expect(code, body, tc.name, tc.limit)
	}
	mustDo(t, c, http.MethodPut, "/fnl", `{"settings":{"index.mapping.field_name_length.limit":20}}`)
	code, body := status(t, c, http.MethodPut, "/fnl/_doc/1", `{"abcdefghijklmnopqrstuvwxy":1}`)
	expect(code, body, "_data_stream_timestamp", "20")
	code, body = status(t, c, http.MethodPut, "/fnl/_doc/2", `{"short":{"abcdefghijklmnopqrstuvwxy":1}}`)
	expect(code, body, "abcdefghijklmnopqrstuvwxy", "20")
}

func TestFT2SearchAsYouTypeSubfields(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/sayt", `{"mappings":{"properties":{"s":{"type":"search_as_you_type"},"s2":{"type":"search_as_you_type","max_shingle_size":2,"analyzer":"english"}}}}`)
	mustDo(t, c, http.MethodPut, "/sayt/_doc/1", `{"s":"quick brown fox jumps","s2":"the quick brown foxes"}`)
	mustDo(t, c, http.MethodPut, "/sayt/_doc/2", `{"s":"quick red","s2":"quick red"}`)
	mustDo(t, c, http.MethodPut, "/sayt/_doc/3?refresh=true", `{"s":"Brown fox","s2":"brown fox"}`)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{`{"match":{"s._2gram":"quick brown"}}`, []string{"1"}},
		{`{"match":{"s._2gram":"quick brown fox"}}`, []string{"1", "3"}},
		{`{"match_phrase":{"s._2gram":"quick brown fox"}}`, []string{"1"}},
		{`{"match":{"s._3gram":"quick brown fox"}}`, []string{"1"}},
		{`{"match":{"s._3gram":"quick brown"}}`, nil},
		{`{"match":{"s._4gram":"quick brown fox jumps"}}`, nil},
		{`{"match":{"s._2gram":{"query":"quick brown","analyzer":"standard"}}}`, nil},
		{`{"wildcard":{"s._2gram":"quick*"}}`, []string{"1", "2"}},
		{`{"exists":{"field":"s._2gram"}}`, []string{"1", "2", "3"}},
		// the prefix field searches shingles of max_shingle_size
		{`{"match":{"s._index_prefix":"qui"}}`, nil},
		{`{"match":{"s._index_prefix":"quick brown fox"}}`, []string{"1"}},
		{`{"match_phrase":{"s._index_prefix":"qui"}}`, []string{"1", "2"}},
		// and indexes the edge n-grams of shingles padded at the end of the text
		{`{"term":{"s._index_prefix":"qui"}}`, []string{"1", "2"}},
		{`{"term":{"s._index_prefix":"fox "}}`, []string{"1", "3"}},
		{`{"term":{"s._index_prefix":"fox  "}}`, []string{"3"}},
		{`{"match":{"s2._2gram":"quick brown"}}`, []string{"1"}},
		{`{"match":{"s2._index_prefix":"the qu"}}`, nil},
		{`{"match":{"s2._index_prefix":"quick bro"}}`, []string{"1"}},
		{`{"multi_match":{"query":"quick br","type":"bool_prefix","fields":["s","s._2gram","s._3gram"]}}`, []string{"1", "2", "3"}},
	} {
		ft2ExpectIDs(t, c, "sayt", `{"query":`+tc.query+`}`, tc.want...)
	}
	ft2ExpectSearchError(t, c, "sayt", `{"query":{"exists":{"field":"s._index_prefix"}}}`, 400, "query_shard_exception", "failed to create query: null")
	ft2ExpectSearchError(t, c, "sayt", `{"query":{"match_phrase":{"s._index_prefix":"quick brown fox"}}}`, 400, "query_shard_exception",
		"failed to create query: Can only use phrase queries on text fields - not on [s._index_prefix] which is of type [prefix]")
	ft2ExpectSearchError(t, c, "sayt", `{"query":{"match_phrase_prefix":{"s._index_prefix":"qui"}}}`, 400, "query_shard_exception",
		"failed to create query: Can only use phrase prefix queries on text fields - not on [s._index_prefix] which is of type [prefix]")
	ft2ExpectSearchError(t, c, "sayt", `{"sort":[{"s":"asc"}]}`, 400, "illegal_argument_exception", "Fielddata is not supported on field [s] of type [search_as_you_type]")
	ft2ExpectSearchError(t, c, "sayt", `{"aggs":{"t":{"terms":{"field":"s._2gram"}}}}`, 400, "illegal_argument_exception", "Fielddata is not supported on field [s._2gram] of type [search_as_you_type]")
	ft2ExpectSearchError(t, c, "sayt", `{"docvalue_fields":["s._index_prefix"]}`, 400, "illegal_argument_exception", "Fielddata is not supported on field [s._index_prefix] of type [prefix]")

	res := mustDo(t, c, http.MethodPost, "/sayt/_search", `{"query":{"ids":{"values":["1"]}},"fields":["s*"],"_source":false}`)
	fields := res["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["fields"].(map[string]any)
	for _, name := range []string{"s", "s._2gram", "s._3gram", "s._index_prefix"} {
		if got := fmt.Sprint(fields[name]); got != "[quick brown fox jumps]" {
			t.Fatalf("fields %s = %s", name, got)
		}
	}
	res = mustDo(t, c, http.MethodPost, "/sayt/_search", `{"query":{"match":{"s._2gram":"quick brown"}},"highlight":{"fields":{"s._2gram":{}}}}`)
	if got := fmt.Sprint(res["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["highlight"]); got != "map[s._2gram:[<em>quick brown</em> fox jumps]]" {
		t.Fatalf("highlight = %s", got)
	}
	caps := mustDo(t, c, http.MethodGet, "/sayt/_field_caps?fields=s,s._index_prefix,s._3gram", nil)["fields"]
	if got := fmt.Sprint(caps); got != "map[s:map[search_as_you_type:map[aggregatable:false searchable:true type:search_as_you_type]] "+
		"s._3gram:map[search_as_you_type:map[aggregatable:false searchable:true type:search_as_you_type]] "+
		"s._index_prefix:map[prefix:map[aggregatable:false searchable:true type:prefix]]]" {
		t.Fatalf("field caps = %s", got)
	}
	if got := fmt.Sprint(mustDo(t, c, http.MethodGet, "/sayt/_mapping/field/s._2gram,s._index_prefix", nil)["sayt"]); got != "map[mappings:map["+
		"s._2gram:map[full_name:s._2gram mapping:map[s._2gram:map[doc_values:false type:shingle]]] "+
		"s._index_prefix:map[full_name:s._index_prefix mapping:map[s._index_prefix:map[doc_values:false type:prefix]]]]]" {
		t.Fatalf("field mapping = %s", got)
	}
}

func TestFT2BooleanSimilarity(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/sim", `{"settings":{"index.similarity.my":{"type":"boolean"}},"mappings":{"properties":{"t":{"type":"text","similarity":"boolean"},"m":{"type":"text","similarity":"my"},"k":{"type":"keyword","similarity":"boolean"}}}}`)
	mustDo(t, c, http.MethodPut, "/sim/_doc/1", `{"t":"fox fox fox dog","m":"fox dog","k":"a"}`)
	mustDo(t, c, http.MethodPut, "/sim/_doc/2?refresh=true", `{"t":"fox","m":"fox","k":"b"}`)
	// every matching term scores its boost, whatever its frequency
	for _, tc := range []struct {
		query string
		want  map[string]float64
	}{
		{`{"match":{"t":"fox dog"}}`, map[string]float64{"1": 2, "2": 1}},
		{`{"match":{"t":{"query":"fox","boost":2.5}}}`, map[string]float64{"1": 2.5, "2": 2.5}},
		{`{"match_phrase":{"t":"fox dog"}}`, map[string]float64{"1": 1}},
		{`{"match_phrase":{"t":{"query":"fox fox","slop":2}}}`, map[string]float64{"1": 1}},
		{`{"term":{"t":{"value":"fox","boost":3}}}`, map[string]float64{"1": 3, "2": 3}},
		{`{"term":{"k":"a"}}`, map[string]float64{"1": 1}},
		{`{"query_string":{"query":"t:fox OR t:dog"}}`, map[string]float64{"1": 2, "2": 1}},
		{`{"multi_match":{"query":"fox","fields":["t^3","m"]}}`, map[string]float64{"1": 3, "2": 3}},
		{`{"match":{"m":"fox dog"}}`, map[string]float64{"1": 2, "2": 1}},
		{`{"fuzzy":{"t":"fax"}}`, map[string]float64{"1": 0.6666666, "2": 0.6666666}},
		{`{"match":{"t":{"query":"fax","fuzziness":1}}}`, map[string]float64{"1": 0.6666666, "2": 0.6666666}},
		{`{"common":{"t":{"query":"fox dog"}}}`, map[string]float64{"1": 2}},
	} {
		got := ft2Scores(t, c, "sim", `{"query":`+tc.query+`}`)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: scores %v, want %v", tc.query, got, tc.want)
		}
		for id, want := range tc.want {
			if d := got[id] - want; d < -1e-6 || d > 1e-6 {
				t.Fatalf("%s: scores %v, want %v", tc.query, got, tc.want)
			}
		}
	}
}

func TestFT2LongDocValuePrecision(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/lp", `{"mappings":{"properties":{"x":{"type":"long"},"u":{"type":"unsigned_long"},"xs":{"type":"long","store":true}}}}`)
	mustDo(t, c, http.MethodPut, "/lp/_doc/1?refresh=true", `{"x":[9007199254740993,9007199254740992,"9223372036854775807",-9007199254740993],"u":[9223372036854775809,18446744073709551614],"xs":9007199254740995}`)
	for _, tc := range []struct{ body, want string }{
		// doc values are sorted and keep every digit
		{`{"docvalue_fields":["x"],"_source":false}`, `"fields":{"x":[-9007199254740993,9007199254740992,9007199254740993,9223372036854775807]}`},
		{`{"docvalue_fields":["u"],"_source":false}`, `"fields":{"u":[9223372036854775809,18446744073709551614]}`},
		// unsigned_long doc values ignore the format
		{`{"docvalue_fields":[{"field":"u","format":"0.00"}],"_source":false}`, `"fields":{"u":[9223372036854775809,18446744073709551614]}`},
		{`{"docvalue_fields":[{"field":"x","format":"#"}],"_source":false}`, `"fields":{"x":["-9007199254740993","9007199254740992","9007199254740993","9223372036854775807"]}`},
		// the fields option parses the source values
		{`{"fields":["x"],"_source":false}`, `"fields":{"x":[9007199254740993,9007199254740992,9223372036854775807,-9007199254740993]}`},
		{`{"stored_fields":["xs"],"_source":false}`, `"fields":{"xs":[9007199254740995]}`},
	} {
		res, err := c.Do(http.MethodPost, "/lp/_search", tc.body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(res.Body), tc.want) {
			t.Fatalf("%s: %s", tc.body, res.Body)
		}
	}
}

// ft2ExpectScores checks the scores of the hits of a query.
func ft2ExpectScores(t *testing.T, c *Cluster, index, query string, want map[string]float64) {
	t.Helper()
	got := ft2Scores(t, c, index, `{"query":`+query+`}`)
	if len(got) != len(want) {
		t.Fatalf("%s: scores %v, want %v", query, got, want)
	}
	for id, w := range want {
		s, ok := got[id]
		if d := s - w; !ok || d < -1e-6 || d > 1e-6 {
			t.Fatalf("%s: scores %v, want %v", query, got, want)
		}
	}
}

// ft2ExpectParseError checks a request failure and its cause (the reason
// without its location prefix).
func ft2ExpectParseError(t *testing.T, c *Cluster, index, body, typ, reason, cause string) {
	t.Helper()
	code, res := status(t, c, http.MethodPost, "/"+index+"/_search", body)
	e, _ := res["error"].(map[string]any)
	r, _ := e["reason"].(string)
	cb, _ := e["caused_by"].(map[string]any)
	cr, _ := cb["reason"].(string)
	if code != http.StatusBadRequest || e["type"] != typ || !strings.HasSuffix(r, reason) || cr != cause {
		t.Fatalf("%s: status=%d error=%v", body, code, e)
	}
}

func TestFT2RankFeatureQuery(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/rf", `{"mappings":{"properties":{"pr":{"type":"rank_feature"},"neg":{"type":"rank_feature","positive_score_impact":false},"rfs":{"type":"rank_features"},"k":{"type":"keyword"}}}}`)
	mustDo(t, c, http.MethodPut, "/rf/_doc/d1", `{"pr":8,"neg":2,"rfs":{"a":1.5,"b":10}}`)
	mustDo(t, c, http.MethodPut, "/rf/_doc/d2", `{"pr":0.5,"neg":0.1,"rfs":{"a":3}}`)
	mustDo(t, c, http.MethodPut, "/rf/_doc/d3", `{"pr":100}`)
	mustDo(t, c, http.MethodPut, "/rf/_doc/d4?refresh=true", `{"k":"x"}`)
	for _, tc := range []struct {
		query string
		want  map[string]float64
	}{
		// the default saturation pivot is the decoded average term frequency
		{`{"rank_feature":{"field":"pr"}}`, map[string]float64{"d1": 0.51926976, "d2": 0.063241124, "d3": 0.9310445}},
		{`{"rank_feature":{"field":"pr","saturation":{"pivot":10}}}`, map[string]float64{"d1": 0.44444442, "d2": 0.047619045, "d3": 0.9090909}},
		{`{"rank_feature":{"field":"pr","log":{"scaling_factor":4}}}`, map[string]float64{"d1": 2.4849067, "d2": 1.5040774, "d3": 4.644391}},
		{`{"rank_feature":{"field":"pr","sigmoid":{"pivot":7,"exponent":0.6}}}`, map[string]float64{"d1": 0.520019, "d2": 0.17030963, "d3": 0.83139634}},
		{`{"rank_feature":{"field":"pr","linear":{}}}`, map[string]float64{"d1": 8, "d2": 0.5, "d3": 100}},
		{`{"rank_feature":{"field":"pr","boost":2}}`, map[string]float64{"d1": 1.0385395, "d2": 0.12648225, "d3": 1.862089}},
		// fields with a negative score impact index the inverse value
		{`{"rank_feature":{"field":"neg"}}`, map[string]float64{"d1": 0.18181819, "d2": 0.8163265}},
		{`{"rank_feature":{"field":"neg","linear":{}}}`, map[string]float64{"d1": 0.5, "d2": 10}},
		{`{"rank_feature":{"field":"rfs.a"}}`, map[string]float64{"d1": 0.4285714, "d2": 0.6}},
		{`{"rank_feature":{"field":"rfs.b","log":{"scaling_factor":1}}}`, map[string]float64{"d1": 2.3978953}},
		{`{"rank_feature":{"field":"rfs.zz"}}`, map[string]float64{}},
		{`{"rank_feature":{"field":"nope"}}`, map[string]float64{}},
		{`{"bool":{"should":[{"match_all":{}},{"rank_feature":{"field":"pr","saturation":{"pivot":8}}}]}}`, map[string]float64{"d1": 1.5, "d2": 1.0588236, "d3": 1.925926, "d4": 1}},
	} {
		ft2ExpectScores(t, c, "rf", tc.query, tc.want)
	}
	ft2ExpectSearchError(t, c, "rf", `{"query":{"rank_feature":{"field":"k"}}}`, 400, "query_shard_exception",
		"failed to create query: [rank_feature] query only works on [rank_feature] fields and features of [rank_features] fields, not [keyword]")
	ft2ExpectSearchError(t, c, "rf", `{"query":{"rank_feature":{"field":"rfs"}}}`, 400, "query_shard_exception",
		"failed to create query: [rank_feature] query only works on [rank_feature] fields and features of [rank_features] fields, not [rank_features]")
	ft2ExpectSearchError(t, c, "rf", `{"query":{"rank_feature":{"field":"neg","log":{"scaling_factor":2}}}}`, 400, "query_shard_exception",
		"failed to create query: Cannot use the [log] function with a field that has a negative score impact as it would trigger negative scores")
	ft2ExpectSearchError(t, c, "rf", `{"query":{"rank_feature":{"field":"pr","saturation":{"pivot":-1}}}}`, 400, "query_shard_exception", "failed to create query: pivot must be > 0, got: -1.0")
	ft2ExpectSearchError(t, c, "rf", `{"query":{"rank_feature":{"field":"pr","log":{"scaling_factor":0}}}}`, 400, "query_shard_exception", "failed to create query: scalingFactor must be >= 1, got: 0.0")
	ft2ExpectSearchError(t, c, "rf", `{"query":{"rank_feature":{"field":"pr","sigmoid":{"pivot":1,"exponent":0}}}}`, 400, "query_shard_exception", "failed to create query: exp must be > 0, got: 0.0")
	ft2ExpectParseError(t, c, "rf", `{"query":{"rank_feature":{}}}`, "illegal_argument_exception", "Required [field]", "")
	ft2ExpectParseError(t, c, "rf", `{"query":{"rank_feature":{"field":"pr","log":{}}}}`, "x_content_parse_exception", "[feature] failed to parse field [log]", "Required [scaling_factor]")
	ft2ExpectParseError(t, c, "rf", `{"query":{"rank_feature":{"field":"pr","sigmoid":{}}}}`, "x_content_parse_exception", "[feature] failed to parse field [sigmoid]", "Required [pivot, exponent]")
	ft2ExpectParseError(t, c, "rf", `{"query":{"rank_feature":{"field":"pr","saturation":{},"log":{"scaling_factor":1}}}}`, "x_content_parse_exception",
		"Failed to build [feature] after last required field arrived", "Can only specify one of [log], [saturation], [sigmoid] and [linear]")
	ft2ExpectParseError(t, c, "rf", `{"query":{"rank_feature":{"field":"pr","fied":"x"}}}`, "x_content_parse_exception", "[feature] unknown field [fied] did you mean [field]?", "")
}

func TestFT2JoinQueries(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/jq", `{"mappings":{"properties":{"rel":{"type":"join","relations":{"q":["a","c"],"a":"g"}},"t":{"type":"keyword"},"n":{"type":"integer"}}}}`)
	for _, doc := range []struct{ path, body string }{
		{"/jq/_doc/q1", `{"rel":"q","t":"question1","n":7}`},
		{"/jq/_doc/q2", `{"rel":{"name":"q"},"t":"question2","n":2}`},
		{"/jq/_doc/a1?routing=q1", `{"rel":{"name":"a","parent":"q1"},"t":"answer","n":1}`},
		{"/jq/_doc/a2?routing=q1", `{"rel":{"name":"a","parent":"q1"},"t":"answer","n":5}`},
		{"/jq/_doc/a3?routing=q2", `{"rel":{"name":"a","parent":"q2"},"t":"answer","n":3}`},
		{"/jq/_doc/c1?routing=q2", `{"rel":{"name":"c","parent":"q2"},"t":"comment"}`},
		{"/jq/_doc/g1?routing=q1&refresh=true", `{"rel":{"name":"g","parent":"a1"},"t":"grand"}`},
	} {
		mustDo(t, c, http.MethodPut, doc.path, doc.body)
	}
	// every document of the inner query scores its n
	scoreN := `{"bool":{"should":[`
	for i, n := range []string{"1", "2", "3", "5", "7"} {
		if i > 0 {
			scoreN += ","
		}
		scoreN += `{"constant_score":{"filter":{"term":{"n":` + n + `}},"boost":` + n + `}}`
	}
	scoreN += `]}}`
	for _, tc := range []struct {
		query string
		want  map[string]float64
	}{
		{`{"has_child":{"type":"a","query":{"match_all":{}}}}`, map[string]float64{"q1": 1, "q2": 1}},
		{`{"has_child":{"type":"a","query":` + scoreN + `,"score_mode":"max"}}`, map[string]float64{"q1": 5, "q2": 3}},
		{`{"has_child":{"type":"a","query":` + scoreN + `,"score_mode":"sum"}}`, map[string]float64{"q1": 6, "q2": 3}},
		{`{"has_child":{"type":"a","query":` + scoreN + `,"score_mode":"avg"}}`, map[string]float64{"q1": 3, "q2": 3}},
		{`{"has_child":{"type":"a","query":` + scoreN + `,"score_mode":"min","boost":2}}`, map[string]float64{"q1": 2, "q2": 6}},
		{`{"has_child":{"type":"a","query":{"match_all":{}},"min_children":2}}`, map[string]float64{"q1": 1}},
		{`{"has_child":{"type":"a","query":{"match_all":{}},"max_children":1}}`, map[string]float64{"q2": 1}},
		{`{"has_child":{"type":"g","query":{"match_all":{}}}}`, map[string]float64{"a1": 1}},
		{`{"has_child":{"type":"a","query":{"has_child":{"type":"g","query":{"match_all":{}}}}}}`, map[string]float64{"q1": 1}},
		{`{"has_parent":{"parent_type":"q","query":{"term":{"t":"question1"}}}}`, map[string]float64{"a1": 1, "a2": 1}},
		{`{"has_parent":{"parent_type":"q","query":` + scoreN + `,"score":true}}`, map[string]float64{"a1": 7, "a2": 7, "a3": 2, "c1": 2}},
		{`{"has_parent":{"parent_type":"a","query":{"match_all":{}}}}`, map[string]float64{"g1": 1}},
		// parent_id scores the parent id term with BM25 (no frequencies, no norms)
		{`{"parent_id":{"type":"a","id":"q1"}}`, map[string]float64{"a1": 0.31506687, "a2": 0.31506687}},
		{`{"parent_id":{"type":"g","id":"a1"}}`, map[string]float64{"g1": 0.31506687}},
		{`{"has_child":{"type":"zz","query":{"match_all":{}},"ignore_unmapped":true}}`, map[string]float64{}},
	} {
		ft2ExpectScores(t, c, "jq", tc.query, tc.want)
	}
	ft2ExpectSearchError(t, c, "jq", `{"query":{"has_child":{"type":"zz","query":{"match_all":{}}}}}`, 400, "query_shard_exception", "[has_child] join field [rel] doesn't hold [zz] as a child")
	ft2ExpectSearchError(t, c, "jq", `{"query":{"has_parent":{"parent_type":"g","query":{"match_all":{}}}}}`, 400, "query_shard_exception", "[has_parent] join field [rel] doesn't hold [g] as a parent")
	ft2ExpectSearchError(t, c, "jq", `{"query":{"parent_id":{"type":"q","id":"q1"}}}`, 400, "query_shard_exception", "[parent_id] no relation found for child [q]")
	ft2ExpectSearchError(t, c, "jq", `{"query":{"has_child":{"type":"a"}}}`, 400, "illegal_argument_exception", "[has_child] requires 'query' field")
	ft2ExpectSearchError(t, c, "jq", `{"query":{"has_parent":{"query":{"match_all":{}}}}}`, 400, "illegal_argument_exception", "[has_parent] requires 'parent_type' field")
	ft2ExpectSearchError(t, c, "jq", `{"query":{"has_child":{"type":"a","query":{"match_all":{}},"min_children":3,"max_children":2}}}`, 400, "illegal_argument_exception", "[has_child] 'max_children' is less than 'min_children'")
	ft2ExpectSearchError(t, c, "jq", `{"query":{"has_child":{"type":"a","query":{"match_all":{}},"score_mode":"bad"}}}`, 400, "illegal_argument_exception", "No score mode for child query [bad] found")
	ft2ExpectSearchError(t, c, "jq", `{"query":{"has_child":{"type":"a","query":{"match_all":{}},"inner_hits":{}}}}`, 400, "unsupported_operation_exception", "[inner_hits] of [has_child] queries is not supported by osmem")
	mustDo(t, c, http.MethodPut, "/jq2", `{"mappings":{"properties":{"t":{"type":"keyword"}}}}`)
	ft2ExpectSearchError(t, c, "jq2", `{"query":{"has_child":{"type":"a","query":{"match_all":{}}}}}`, 400, "query_shard_exception", "[has_child] no join field has been configured")
	ft2ExpectSearchError(t, c, "jq2", `{"query":{"parent_id":{"type":"a","id":"q1"}}}`, 400, "query_shard_exception", "[parent_id] no join field found for index [jq2]")
}

func TestFT2NumericQueryValueBytes(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/nq", `{"mappings":{"properties":{"i":{"type":"integer"},"l":{"type":"long"},"u":{"type":"unsigned_long"},"ir":{"type":"integer_range"}}}}`)
	mustDo(t, c, http.MethodPut, "/nq/_doc/1?refresh=true", `{"i":1,"l":1,"u":1,"ir":{"gte":1}}`)
	// string values reach the number parser as a BytesRef, which the
	// out of range messages print as bytes; JSON numbers print as numbers
	bytes := "[33 30 30 30 30 30 30 30 30 30]"
	for _, tc := range []struct{ query, value, typ string }{
		{`{"match":{"i":3000000000}}`, bytes, "an integer"},
		{`{"term":{"i":"3000000000"}}`, bytes, "an integer"},
		{`{"term":{"i":3000000000}}`, "3000000000", "an integer"},
		{`{"terms":{"i":["3000000000"]}}`, bytes, "an integer"},
		{`{"range":{"i":{"gte":"1.5","lte":"3000000000"}}}`, bytes, "an integer"},
		{`{"multi_match":{"query":"3000000000","fields":["i"]}}`, bytes, "an integer"},
		{`{"range":{"ir":{"gte":"3000000000"}}}`, bytes, "an integer"},
		{`{"match":{"l":"99999999999999999999"}}`, "[39 39 39 39 39 39 39 39 39 39 39 39 39 39 39 39 39 39 39 39]", "a long"},
		{`{"match":{"u":-1}}`, "[2d 31]", "an unsigned long"},
	} {
		ft2ExpectSearchError(t, c, "nq", `{"query":`+tc.query+`}`, 400, "query_shard_exception",
			"failed to create query: Value ["+tc.value+"] is out of range for "+tc.typ)
	}
}
