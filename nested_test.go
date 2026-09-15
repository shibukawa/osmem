package osmem

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Nested documents and collapse inner_hits. The expected responses were
// taken from OpenSearch 2.19 with the same index and requests; scores are
// compared only for presence (bleve's BM25 is not Lucene's).

func seedBlog(t *testing.T) *Cluster {
	t.Helper()
	c := New()
	mustDo(t, c, http.MethodPut, "/blog", `{"mappings": {"properties": {
	  "title": {"type": "keyword"}, "group": {"type": "keyword"}, "n": {"type": "integer"},
	  "comments": {"type": "nested", "properties": {
	    "author": {"type": "keyword"}, "text": {"type": "text"}, "stars": {"type": "integer"},
	    "votes": {"type": "nested", "properties": {"user": {"type": "keyword"}, "value": {"type": "integer"}}}
	  }}
	}}}`)
	mustDo(t, c, http.MethodPut, "/blog/_doc/1", `{"title": "A", "group": "g1", "n": 1, "comments": [
	  {"author": "alice", "text": "great post", "stars": 5, "votes": [{"user": "u1", "value": 1}, {"user": "u2", "value": 2}]},
	  {"author": "bob", "text": "bad post", "stars": 1, "votes": [{"user": "u1", "value": -1}]}]}`)
	mustDo(t, c, http.MethodPut, "/blog/_doc/2", `{"title": "B", "group": "g1", "n": 2, "comments": [{"author": "alice", "text": "bad post", "stars": 2}]}`)
	mustDo(t, c, http.MethodPut, "/blog/_doc/3", `{"title": "C", "group": "g2", "n": 3, "comments": [{"author": "carol", "text": "great great post", "stars": 4}, {"author": "alice", "text": "great", "stars": 3}]}`)
	mustDo(t, c, http.MethodPut, "/blog/_doc/4", `{"title": "D", "n": 4}`)
	return c
}

func blogSearch(t *testing.T, c *Cluster, body string) (ids []string, res map[string]any) {
	t.Helper()
	res = mustDo(t, c, http.MethodPost, "/blog/_search", body)
	for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
		ids = append(ids, h.(map[string]any)["_id"].(string))
	}
	return ids, res
}

func hitAt(res map[string]any, i int) map[string]any {
	return res["hits"].(map[string]any)["hits"].([]any)[i].(map[string]any)
}

// scrubScores replaces numeric scores by "S" so shapes can be compared.
func scrubScores(v any, key string) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range t {
			out[k] = scrubScores(e, k)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = scrubScores(e, "")
		}
		return out
	case float64:
		if key == "_score" || key == "max_score" {
			return "S"
		}
	}
	return v
}

func canonJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(scrubScores(parsed, ""))
	return string(out)
}

func assertJSON(t *testing.T, got any, want string) {
	t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad expectation: %v", err)
	}
	if g, e := canonJSON(t, got), canonJSON(t, w); g != e {
		t.Fatalf("got  %s\nwant %s", g, e)
	}
}

func rootCause(body map[string]any) (typ, reason string) {
	e, _ := body["error"].(map[string]any)
	rc, _ := e["root_cause"].([]any)
	if len(rc) == 0 {
		return "", ""
	}
	m := rc[0].(map[string]any)
	typ, _ = m["type"].(string)
	reason, _ = m["reason"].(string)
	return typ, reason
}

func TestNestedQueryMatchesWithinOneObject(t *testing.T) {
	c := seedBlog(t)
	defer c.Close()
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"same object", `{"nested": {"path": "comments", "query": {"bool": {"must": [{"term": {"comments.author": "alice"}}, {"match": {"comments.text": "great"}}]}}}}`, []string{"1", "3"}},
		{"across objects", `{"nested": {"path": "comments", "query": {"bool": {"must": [{"term": {"comments.author": "bob"}}, {"match": {"comments.text": "great"}}]}}}}`, nil},
		{"must_not inside", `{"nested": {"path": "comments", "query": {"bool": {"must": [{"term": {"comments.author": "alice"}}], "must_not": [{"match": {"comments.text": "bad"}}]}}}}`, []string{"1", "3"}},
		{"filter and must_not outside", `{"bool": {"filter": [{"nested": {"path": "comments", "query": {"term": {"comments.author": "alice"}}}}], "must_not": [{"nested": {"path": "comments", "query": {"term": {"comments.author": "bob"}}}}]}}`, []string{"2", "3"}},
		{"deep path from the root", `{"nested": {"path": "comments.votes", "query": {"term": {"comments.votes.user": "u1"}}}}`, []string{"1"}},
		{"exists inside", `{"nested": {"path": "comments", "query": {"exists": {"field": "comments.stars"}}}}`, []string{"1", "2", "3"}},
		{"exists on a deeper nested path sees nothing", `{"nested": {"path": "comments", "query": {"exists": {"field": "comments.votes"}}}}`, nil},
		{"plain term sees nothing", `{"term": {"comments.author": "alice"}}`, nil},
		{"plain exists sees nothing", `{"exists": {"field": "comments.author"}}`, nil},
		{"exists on the path sees nothing", `{"exists": {"field": "comments"}}`, nil},
		{"ignore_unmapped", `{"nested": {"path": "nope", "query": {"match_all": {}}, "ignore_unmapped": true}}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids, _ := blogSearch(t, c, `{"query": `+tc.query+`, "sort": ["_doc"], "_source": false}`)
			assertSet(t, ids, tc.want...)
		})
	}
	// score_mode sum favours the document with two matching comments
	ids, _ := blogSearch(t, c, `{"query": {"nested": {"path": "comments", "query": {"match": {"comments.text": "great"}}, "score_mode": "sum"}}, "_source": false}`)
	assertIDs(t, ids, "3", "1")
	_, res := blogSearch(t, c, `{"query": {"nested": {"path": "comments", "query": {"match": {"comments.text": "great"}}, "score_mode": "none"}}, "_source": false}`)
	if hitAt(res, 0)["_score"] != 0.0 || res["hits"].(map[string]any)["max_score"] != 0.0 {
		t.Fatalf("score_mode none: %v", res["hits"])
	}
	errors := []struct {
		name, body, typ, root, reason string
	}{
		{"unmapped path", `{"query": {"nested": {"path": "nope", "query": {"match_all": {}}}}}`, "search_phase_execution_exception", "query_shard_exception", "failed to create query: [nested] failed to find nested object under path [nope]"},
		{"not nested", `{"query": {"nested": {"path": "title", "query": {"match_all": {}}}}}`, "search_phase_execution_exception", "query_shard_exception", "failed to create query: [nested] failed to find nested object under path [title]"},
		{"unknown key", `{"query": {"nested": {"path": "comments", "query": {"match_all": {}}, "bogus": 1}}}`, "parsing_exception", "parsing_exception", "[nested] query does not support [bogus]"},
	}
	for _, tc := range errors {
		t.Run(tc.name, func(t *testing.T) {
			st, body := status(t, c, http.MethodPost, "/blog/_search", tc.body)
			typ, reason := rootCause(body)
			if st != 400 || errType(body) != tc.typ || typ != tc.root || reason != tc.reason {
				t.Fatalf("status=%d body=%v", st, body)
			}
		})
	}
	// root_cause of a query_shard_exception names the index
	_, body := status(t, c, http.MethodPost, "/blog/_search", errors[0].body)
	if rc := body["error"].(map[string]any)["root_cause"].([]any)[0].(map[string]any); rc["index"] != "blog" {
		t.Fatalf("root_cause %v", rc)
	}
}

func TestNestedInnerHits(t *testing.T) {
	c := seedBlog(t)
	defer c.Close()

	// default name is the path; _source is the object; _nested identifies it
	_, res := blogSearch(t, c, `{"query": {"nested": {"path": "comments", "query": {"match": {"comments.text": "great"}}, "inner_hits": {"sort": [{"comments.stars": "desc"}], "track_scores": true}}}, "sort": ["_doc"], "_source": false}`)
	assertJSON(t, hitAt(res, 0)["inner_hits"], `{"comments": {"hits": {"total": {"value": 1, "relation": "eq"}, "max_score": 1, "hits": [
	  {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 0}, "_score": 1, "sort": [5],
	   "_source": {"author": "alice", "text": "great post", "stars": 5, "votes": [{"user": "u1", "value": 1}, {"user": "u2", "value": 2}]}}]}}}`)
	assertJSON(t, hitAt(res, 1)["inner_hits"], `{"comments": {"hits": {"total": {"value": 2, "relation": "eq"}, "max_score": 1, "hits": [
	  {"_index": "blog", "_id": "3", "_nested": {"field": "comments", "offset": 0}, "_score": 1, "sort": [4], "_source": {"author": "carol", "text": "great great post", "stars": 4}},
	  {"_index": "blog", "_id": "3", "_nested": {"field": "comments", "offset": 1}, "_score": 1, "sort": [3], "_source": {"author": "alice", "text": "great", "stars": 3}}]}}}`)

	// name, from, size, sort (scores hidden), _source with full paths
	_, res = blogSearch(t, c, `{"query": {"nested": {"path": "comments", "query": {"match_all": {}}, "inner_hits": {"name": "cm", "size": 1, "from": 1, "sort": [{"comments.stars": "desc"}], "_source": ["comments.author"]}}}, "sort": ["_doc"], "_source": false}`)
	assertJSON(t, hitAt(res, 0)["inner_hits"], `{"cm": {"hits": {"total": {"value": 2, "relation": "eq"}, "max_score": null, "hits": [
	  {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 1}, "_score": null, "_source": {"author": "bob"}, "sort": [1]}]}}}`)
	assertJSON(t, hitAt(res, 1)["inner_hits"], `{"cm": {"hits": {"total": {"value": 1, "relation": "eq"}, "max_score": null, "hits": []}}}`)

	// _source false, size 0, version, relative include paths select nothing
	_, res = blogSearch(t, c, `{"query": {"nested": {"path": "comments", "query": {"match_all": {}}, "inner_hits": {"_source": false}}}, "size": 1, "_source": false}`)
	assertJSON(t, hitAt(res, 0)["inner_hits"], `{"comments": {"hits": {"total": {"value": 2, "relation": "eq"}, "max_score": 1, "hits": [
	  {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 0}, "_score": 1},
	  {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 1}, "_score": 1}]}}}`)
	_, res = blogSearch(t, c, `{"query": {"nested": {"path": "comments", "query": {"match_all": {}}, "inner_hits": {"name": "x", "size": 0}}}, "size": 1, "_source": false}`)
	assertJSON(t, hitAt(res, 0)["inner_hits"], `{"x": {"hits": {"total": {"value": 2, "relation": "eq"}, "max_score": null, "hits": []}}}`)
	_, res = blogSearch(t, c, `{"query": {"nested": {"path": "comments", "query": {"term": {"comments.author": "carol"}}, "inner_hits": {"_source": {"includes": ["author"]}, "version": true}}}, "_source": false}`)
	assertJSON(t, hitAt(res, 0)["inner_hits"], `{"comments": {"hits": {"total": {"value": 1, "relation": "eq"}, "max_score": 1, "hits": [
	  {"_index": "blog", "_id": "3", "_nested": {"field": "comments", "offset": 0}, "_version": 1, "_score": 1, "_source": {}}]}}}`)

	// highlight, fields and docvalue_fields apply to the object
	_, res = blogSearch(t, c, `{"query": {"nested": {"path": "comments", "query": {"match": {"comments.text": "great"}}, "inner_hits": {"highlight": {"fields": {"comments.text": {}}}, "docvalue_fields": ["comments.stars"], "fields": ["comments.author"], "_source": false, "sort": [{"comments.stars": "asc"}]}}}, "size": 1, "_source": false, "sort": ["_doc"]}`)
	assertJSON(t, hitAt(res, 0)["inner_hits"], `{"comments": {"hits": {"total": {"value": 1, "relation": "eq"}, "max_score": null, "hits": [
	  {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 0}, "_score": null, "sort": [5],
	   "fields": {"comments.author": ["alice"], "comments.stars": [5]}, "highlight": {"comments.text": ["<em>great</em> post"]}}]}}}`)

	// a document matched through another clause gets empty inner hits
	_, res = blogSearch(t, c, `{"query": {"bool": {"should": [{"term": {"title": "D"}}, {"nested": {"path": "comments", "query": {"term": {"comments.author": "carol"}}, "inner_hits": {}}}]}}, "sort": ["_doc"], "_source": false}`)
	assertJSON(t, hitAt(res, 1)["inner_hits"], `{"comments": {"hits": {"total": {"value": 0, "relation": "eq"}, "max_score": null, "hits": []}}}`)

	// two levels: inner hits of the deeper query alone are reported at the
	// root with the full identity; with both, they nest
	_, res = blogSearch(t, c, `{"query": {"nested": {"path": "comments.votes", "query": {"term": {"comments.votes.user": "u1"}}, "inner_hits": {}}}, "_source": false}`)
	assertJSON(t, hitAt(res, 0)["inner_hits"], `{"comments.votes": {"hits": {"total": {"value": 2, "relation": "eq"}, "max_score": 1, "hits": [
	  {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 0, "_nested": {"field": "votes", "offset": 0}}, "_score": 1, "_source": {"user": "u1", "value": 1}},
	  {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 1, "_nested": {"field": "votes", "offset": 0}}, "_score": 1, "_source": {"user": "u1", "value": -1}}]}}}`)
	_, res = blogSearch(t, c, `{"query": {"nested": {"path": "comments", "query": {"nested": {"path": "comments.votes", "query": {"term": {"comments.votes.user": "u1"}}, "inner_hits": {"_source": false}}}, "inner_hits": {"_source": false}}}, "_source": false}`)
	assertJSON(t, hitAt(res, 0)["inner_hits"], `{"comments": {"hits": {"total": {"value": 2, "relation": "eq"}, "max_score": 1, "hits": [
	  {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 0}, "_score": 1, "inner_hits": {"comments.votes": {"hits": {"total": {"value": 1, "relation": "eq"}, "max_score": 1, "hits": [
	    {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 0, "_nested": {"field": "votes", "offset": 0}}, "_score": 1}]}}}},
	  {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 1}, "_score": 1, "inner_hits": {"comments.votes": {"hits": {"total": {"value": 1, "relation": "eq"}, "max_score": 1, "hits": [
	    {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 1, "_nested": {"field": "votes", "offset": 0}}, "_score": 1}]}}}}]}}}`)

	// errors
	errors := []struct {
		name, body string
		st         int
		typ, root  string
	}{
		{"seq_no", `{"query": {"nested": {"path": "comments", "query": {"match_all": {}}, "inner_hits": {"seq_no_primary_term": true}}}}`, 500, "search_phase_execution_exception", "unsupported_operation_exception"},
		{"unknown field", `{"query": {"nested": {"path": "comments", "query": {"match_all": {}}, "inner_hits": {"bogus": 1}}}}`, 400, "x_content_parse_exception", "x_content_parse_exception"},
		{"duplicate name", `{"query": {"nested": {"path": "comments", "query": {"match_all": {}}, "inner_hits": {"name": "x"}}}, "post_filter": {"nested": {"path": "comments", "query": {"match_all": {}}, "inner_hits": {"name": "x"}}}}`, 400, "search_phase_execution_exception", "illegal_argument_exception"},
	}
	for _, tc := range errors {
		t.Run(tc.name, func(t *testing.T) {
			st, body := status(t, c, http.MethodPost, "/blog/_search", tc.body)
			typ, _ := rootCause(body)
			if st != tc.st || errType(body) != tc.typ || typ != tc.root {
				t.Fatalf("status=%d body=%v", st, body)
			}
		})
	}
}

func TestNestedAggregations(t *testing.T) {
	c := seedBlog(t)
	defer c.Close()
	res := mustDo(t, c, http.MethodPost, "/blog/_search", `{"size": 0, "aggs": {
	  "a": {"terms": {"field": "comments.author"}},
	  "n": {"nested": {"path": "comments"}, "aggs": {
	    "a": {"terms": {"field": "comments.author"}},
	    "v": {"nested": {"path": "comments.votes"}, "aggs": {
	      "s": {"sum": {"field": "comments.votes.value"}},
	      "back": {"reverse_nested": {}, "aggs": {"c": {"terms": {"field": "comments.author"}}}},
	      "root": {"reverse_nested": {"path": "comments"}, "aggs": {"t": {"terms": {"field": "title"}}}}}},
	    "r": {"reverse_nested": {}, "aggs": {"t": {"terms": {"field": "title"}}}}}}}}`)
	assertJSON(t, res["aggregations"], `{
	  "a": {"doc_count_error_upper_bound": 0, "sum_other_doc_count": 0, "buckets": []},
	  "n": {"doc_count": 5,
	    "a": {"doc_count_error_upper_bound": 0, "sum_other_doc_count": 0, "buckets": [{"key": "alice", "doc_count": 3}, {"key": "bob", "doc_count": 1}, {"key": "carol", "doc_count": 1}]},
	    "r": {"doc_count": 3, "t": {"doc_count_error_upper_bound": 0, "sum_other_doc_count": 0, "buckets": [{"key": "A", "doc_count": 1}, {"key": "B", "doc_count": 1}, {"key": "C", "doc_count": 1}]}},
	    "v": {"doc_count": 3, "s": {"value": 2.0},
	      "root": {"doc_count": 2, "t": {"doc_count_error_upper_bound": 0, "sum_other_doc_count": 0, "buckets": []}},
	      "back": {"doc_count": 1, "c": {"doc_count_error_upper_bound": 0, "sum_other_doc_count": 0, "buckets": []}}}}}`)
	res = mustDo(t, c, http.MethodPost, "/blog/_search", `{"size": 0, "aggs": {"n": {"nested": {"path": "nope"}}, "m": {"nested": {"path": "title"}}}}`)
	assertJSON(t, res["aggregations"], `{"n": {"doc_count": 0}, "m": {"doc_count": 0}}`)
	res = mustDo(t, c, http.MethodPost, "/blog/_search", `{"size": 0, "aggs": {"n": {"nested": {"path": "comments"}, "aggs": {"top": {"top_hits": {"size": 2, "sort": [{"comments.stars": "desc"}], "_source": ["comments.author"]}}}}}}`)
	assertJSON(t, res["aggregations"], `{"n": {"doc_count": 5, "top": {"hits": {"total": {"value": 5, "relation": "eq"}, "max_score": null, "hits": [
	  {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 0}, "_score": null, "_source": {"author": "alice"}, "sort": [5]},
	  {"_index": "blog", "_id": "3", "_nested": {"field": "comments", "offset": 0}, "_score": null, "_source": {"author": "carol"}, "sort": [4]}]}}}}`)
	st, body := status(t, c, http.MethodPost, "/blog/_search", `{"size": 0, "aggs": {"n": {"reverse_nested": {}}}}`)
	if typ, reason := rootCause(body); st != 400 || typ != "illegal_argument_exception" || reason != "Reverse nested aggregation [n] can only be used inside a [nested] aggregation" {
		t.Fatalf("status=%d body=%v", st, body)
	}
}

func TestNestedSortAndFields(t *testing.T) {
	c := seedBlog(t)
	defer c.Close()
	// without the nested option the field is missing on every document
	ids, res := blogSearch(t, c, `{"sort": [{"comments.stars": "desc"}, "_doc"], "_source": false}`)
	assertIDs(t, ids, "1", "2", "3", "4")
	if s := hitAt(res, 0)["sort"].([]any); s[0] == 5.0 {
		t.Fatalf("nested value used without nested option: %v", s)
	}
	ids, res = blogSearch(t, c, `{"sort": [{"comments.stars": {"order": "desc", "nested": {"path": "comments"}}}], "_source": false}`)
	assertIDs(t, ids, "1", "3", "2", "4")
	if s := hitAt(res, 0)["sort"].([]any); s[0] != 5.0 {
		t.Fatalf("sort values %v", s)
	}
	ids, _ = blogSearch(t, c, `{"sort": [{"comments.stars": {"order": "desc", "nested": {"path": "comments", "filter": {"term": {"comments.author": "alice"}}}}}], "_source": false}`)
	assertIDs(t, ids, "1", "3", "2", "4")
	_, res = blogSearch(t, c, `{"sort": [{"comments.stars": {"order": "asc", "nested": {"path": "comments", "filter": {"term": {"comments.author": "alice"}}}}}], "_source": false, "size": 1}`)
	if s := hitAt(res, 0)["sort"].([]any); hitAt(res, 0)["_id"] != "2" || s[0] != 2.0 {
		t.Fatalf("filtered nested sort: %v", hitAt(res, 0))
	}
	// "fields" reads nested values from the source; docvalue_fields do not
	_, res = blogSearch(t, c, `{"query": {"match_all": {}}, "_source": false, "fields": ["comments.author"], "docvalue_fields": ["comments.stars"], "sort": ["_doc"], "size": 1}`)
	assertJSON(t, hitAt(res, 0)["fields"], `{"comments.author": ["alice", "bob"]}`)
	// collapse on a nested field: no value on the root, so one group
	ids, res = blogSearch(t, c, `{"collapse": {"field": "comments.author"}, "_source": false, "sort": ["_doc"]}`)
	assertIDs(t, ids, "1")
	if _, ok := hitAt(res, 0)["fields"]; ok || res["hits"].(map[string]any)["total"].(map[string]any)["value"] != 4.0 {
		t.Fatalf("collapse on nested field: %v", res["hits"])
	}
}

func TestNestedWritesAndClones(t *testing.T) {
	c := seedBlog(t)
	defer c.Close()
	// update: the old objects disappear, the new ones are searchable
	mustDo(t, c, http.MethodPut, "/blog/_doc/1", `{"title": "A", "group": "g1", "n": 1, "comments": [{"author": "zed", "text": "updated", "stars": 9}]}`)
	ids, res := blogSearch(t, c, `{"query": {"nested": {"path": "comments", "query": {"match_all": {}}, "inner_hits": {"_source": ["comments.author"]}}}, "_source": false, "sort": ["_id"]}`)
	assertIDs(t, ids, "1", "2", "3")
	assertJSON(t, hitAt(res, 0)["inner_hits"], `{"comments": {"hits": {"total": {"value": 1, "relation": "eq"}, "max_score": 1, "hits": [
	  {"_index": "blog", "_id": "1", "_nested": {"field": "comments", "offset": 0}, "_score": 1, "_source": {"author": "zed"}}]}}}`)
	ids, _ = blogSearch(t, c, `{"query": {"nested": {"path": "comments.votes", "query": {"match_all": {}}}}, "_source": false}`)
	assertIDs(t, ids)
	// delete removes the objects too
	mustDo(t, c, http.MethodDelete, "/blog/_doc/3", nil)
	ids, _ = blogSearch(t, c, `{"query": {"nested": {"path": "comments", "query": {"term": {"comments.author": "alice"}}}}, "_source": false}`)
	assertIDs(t, ids, "2")
	if n, err := c.Count("blog", `{"query": {"nested": {"path": "comments", "query": {"match_all": {}}}}}`); err != nil || n != 2 {
		t.Fatalf("count %d %v", n, err)
	}
	// a clone re-indexes the nested objects with its own copy
	clone := c.Clone()
	defer clone.Close()
	mustDo(t, clone, http.MethodPut, "/blog/_doc/9", `{"title": "Z", "comments": [{"author": "alice", "text": "cloned", "stars": 1}]}`)
	ids, _ = blogSearch(t, clone, `{"query": {"nested": {"path": "comments", "query": {"term": {"comments.author": "alice"}}}}, "_source": false, "sort": ["_doc"]}`)
	assertIDs(t, ids, "2", "9")
	ids, _ = blogSearch(t, c, `{"query": {"nested": {"path": "comments", "query": {"term": {"comments.author": "alice"}}}}, "_source": false}`)
	assertIDs(t, ids, "2")
	// OpenSearch refuses to change a dynamically mapped object into nested
	// (illegal_argument_exception "cannot change object mapping from non-nested to nested")
	mustDo(t, c, http.MethodPut, "/dyn/_doc/1", `{"items": [{"name": "a", "qty": 1}, {"name": "b", "qty": 2}]}`)
	ids, _ = mustDoIDs(t, c, "/dyn/_search", `{"query": {"term": {"items.name": "a"}}}`)
	assertIDs(t, ids, "1")
	if statusCode, body := status(t, c, http.MethodPut, "/dyn/_mapping", `{"properties": {"items": {"type": "nested"}}}`); statusCode != http.StatusBadRequest || errType(body) != "illegal_argument_exception" {
		t.Fatalf("object to nested mapping change: status=%d body=%v", statusCode, body)
	}
	ids, _ = mustDoIDs(t, c, "/dyn/_search", `{"query": {"term": {"items.name": "a"}}}`)
	assertIDs(t, ids, "1")
}

func mustDoIDs(t *testing.T, c *Cluster, path, body string) (ids []string, res map[string]any) {
	t.Helper()
	res = mustDo(t, c, http.MethodPost, path, body)
	for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
		ids = append(ids, h.(map[string]any)["_id"].(string))
	}
	return ids, res
}

func TestCollapseInnerHits(t *testing.T) {
	c := seedBlog(t)
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/blog/_doc/5", `{"title": "E", "n": 5}`)

	// total counts the documents before collapsing; documents without a
	// value form one group and report no fields; without a query the
	// group members score 0
	ids, res := blogSearch(t, c, `{"collapse": {"field": "group", "inner_hits": {"name": "by_group", "size": 5, "_source": false}}, "sort": [{"n": "desc"}], "_source": false}`)
	assertIDs(t, ids, "5", "3", "2")
	if tot := res["hits"].(map[string]any)["total"].(map[string]any); tot["value"] != 5.0 {
		t.Fatalf("total %v", tot)
	}
	if _, ok := hitAt(res, 0)["fields"]; ok {
		t.Fatalf("fields on a document without a value: %v", hitAt(res, 0))
	}
	assertJSON(t, hitAt(res, 0)["inner_hits"], `{"by_group": {"hits": {"total": {"value": 2, "relation": "eq"}, "max_score": 0, "hits": [
	  {"_index": "blog", "_id": "4", "_score": 0}, {"_index": "blog", "_id": "5", "_score": 0}]}}}`)
	if s := hitAt(res, 2)["inner_hits"].(map[string]any)["by_group"].(map[string]any)["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["_score"]; s != 0.0 {
		t.Fatalf("group member score %v", s)
	}
	assertJSON(t, hitAt(res, 2)["fields"], `{"group": ["g1"]}`)
	assertJSON(t, hitAt(res, 2)["inner_hits"], `{"by_group": {"hits": {"total": {"value": 2, "relation": "eq"}, "max_score": 0, "hits": [
	  {"_index": "blog", "_id": "1", "_score": 0}, {"_index": "blog", "_id": "2", "_score": 0}]}}}`)

	// several inner_hits with their own sort, size and _source; sorted
	// inner hits hide the score unless track_scores is set
	ids, res = blogSearch(t, c, `{"query": {"match_all": {}}, "collapse": {"field": "group", "inner_hits": [
	  {"name": "oldest", "size": 1, "sort": [{"n": "asc"}], "_source": ["title"]},
	  {"name": "newest", "size": 1, "sort": [{"n": "desc"}], "_source": false, "track_scores": true}]}, "_source": false}`)
	assertIDs(t, ids, "1", "3", "4")
	assertJSON(t, hitAt(res, 0)["inner_hits"], `{
	  "oldest": {"hits": {"total": {"value": 2, "relation": "eq"}, "max_score": null, "hits": [{"_index": "blog", "_id": "1", "_score": null, "_source": {"title": "A"}, "sort": [1]}]}},
	  "newest": {"hits": {"total": {"value": 2, "relation": "eq"}, "max_score": 1, "hits": [{"_index": "blog", "_id": "2", "_score": 1, "sort": [2]}]}}}`)
	if s := hitAt(res, 0)["inner_hits"].(map[string]any)["newest"].(map[string]any)["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["_score"]; s != 1.0 {
		t.Fatalf("tracked score %v", s)
	}

	// the group is the documents matching the query
	_, res = blogSearch(t, c, `{"query": {"term": {"title": "A"}}, "collapse": {"field": "n", "inner_hits": {"name": "i", "_source": false}}, "_source": false}`)
	assertJSON(t, hitAt(res, 0)["inner_hits"], `{"i": {"hits": {"total": {"value": 1, "relation": "eq"}, "max_score": 1, "hits": [{"_index": "blog", "_id": "1", "_score": 1}]}}}`)
	assertJSON(t, hitAt(res, 0)["fields"], `{"n": [1]}`)

	errors := []struct {
		name, path, body string
		st               int
		typ, root        string
	}{
		{"name required", "/blog/_search", `{"collapse": {"field": "group", "inner_hits": {"size": 1}}}`, 400, "illegal_argument_exception", "illegal_argument_exception"},
		{"scroll", "/blog/_search?scroll=1m", `{"collapse": {"field": "group"}}`, 500, "search_phase_execution_exception", "search_exception"},
		{"search_after", "/blog/_search", `{"collapse": {"field": "group"}, "search_after": [1], "sort": [{"n": "desc"}]}`, 500, "search_phase_execution_exception", "search_exception"},
	}
	for _, tc := range errors {
		t.Run(tc.name, func(t *testing.T) {
			st, body := status(t, c, http.MethodPost, tc.path, tc.body)
			typ, _ := rootCause(body)
			if st != tc.st || errType(body) != tc.typ || typ != tc.root {
				t.Fatalf("status=%d body=%v", st, body)
			}
		})
	}
}
