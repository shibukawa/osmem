package osmem

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// Regression tests for behaviour differences found in review.

func TestDateTermCoversInterval(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/d", `{"mappings": {"properties": {"d": {"type": "date"}}}}`)
	mustDo(t, c, http.MethodPut, "/d/_doc/1", `{"d": "2024-01-02T10:00:00Z"}`)
	mustDo(t, c, http.MethodPut, "/d/_doc/2", `{"d": "2024-02-02T10:00:00Z"}`)
	for q, want := range map[string]int{
		`{"term": {"d": "2024-01-02"}}`:                  1,
		`{"term": {"d": "2024-01"}}`:                     1,
		`{"term": {"d": "2024"}}`:                        2,
		`{"term": {"d": "2024-01-02T10:00:00Z"}}`:        1,
		`{"term": {"d": "2024-01-02T11:00:00Z"}}`:        0,
		`{"terms": {"d": ["2024-01-02", "2024-02-02"]}}`: 2,
		`{"match": {"d": "2024-02"}}`:                    1,
	} {
		n, err := c.Count("d", `{"query": `+q+`}`)
		if err != nil || n != want {
			t.Errorf("%s: got %d want %d (%v)", q, n, want, err)
		}
	}
}

func TestSearchAfterDatesAndSentinels(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/s", `{"mappings": {"properties": {"d": {"type": "date"}, "n": {"type": "long"}, "f": {"type": "double"}, "k": {"type": "keyword"}}}}`)
	mustDo(t, c, http.MethodPut, "/s/_doc/1", `{"d": "2024-01-01", "n": 1, "f": 1.5, "k": "a"}`)
	mustDo(t, c, http.MethodPut, "/s/_doc/2", `{"d": "2024-02-01", "n": 2, "f": 2.5, "k": "b"}`)
	mustDo(t, c, http.MethodPut, "/s/_doc/3", `{}`)
	page := func(body string) ([]string, []any) {
		res := mustDo(t, c, http.MethodPost, "/s/_search", body)
		var ids []string
		var last []any
		for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
			ids = append(ids, h.(map[string]any)["_id"].(string))
			last = h.(map[string]any)["sort"].([]any)
		}
		return ids, last
	}
	ids, last := page(`{"sort": [{"d": "asc"}], "size": 2}`)
	assertIDs(t, ids, "1", "2")
	if last[0].(float64) != 1706745600000 {
		t.Fatalf("date sort value %v", last)
	}
	// feed back the date as a string, as clients with a format may do
	ids, last = page(`{"sort": [{"d": "asc"}], "search_after": ["2024-01-01"]}`)
	assertIDs(t, ids, "2", "3")
	if last[0].(float64) != 9223372036854775807 {
		t.Fatalf("missing sentinel %v", last)
	}
	// feeding the sentinel back finishes paging
	ids, _ = page(`{"sort": [{"d": "asc"}], "search_after": [9223372036854775807]}`)
	assertIDs(t, ids)
	ids, last = page(`{"sort": [{"n": "desc"}], "size": 1, "search_after": [2]}`)
	assertIDs(t, ids, "1")
	ids, last = page(`{"sort": [{"n": "desc"}], "search_after": [1]}`)
	assertIDs(t, ids, "3")
	if last[0].(float64) != -9223372036854775808 {
		t.Fatalf("desc missing sentinel %v", last)
	}
	_, last = page(`{"sort": [{"f": {"order": "asc", "missing": "_first"}}], "size": 1}`)
	if last[0] != "-Infinity" {
		t.Fatalf("float sentinel %v", last)
	}
	ids, _ = page(`{"sort": [{"f": {"order": "asc", "missing": "_first"}}], "search_after": ["-Infinity"]}`)
	assertIDs(t, ids, "1", "2")
	_, last = page(`{"sort": [{"k": "asc"}], "search_after": ["b"]}`)
	if last[0] != nil {
		t.Fatalf("keyword missing should be null: %v", last)
	}
	st, body := status(t, c, http.MethodPost, "/s/_search", `{"sort": [{"d": "asc"}], "search_after": ["not a date"]}`)
	if st != 400 || errType(body) != "search_phase_execution_exception" {
		t.Fatalf("bad search_after %d %v", st, body)
	}
}

func TestCrossFieldsAndScores(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/p/_doc/1", `{"first": "john", "last": "smith"}`)
	mustDo(t, c, http.MethodPut, "/p/_doc/2", `{"first": "john smith", "last": "doe"}`)
	mustDo(t, c, http.MethodPut, "/p/_doc/3", `{"first": "jane", "last": "smith"}`)
	n, _ := c.Count("p", `{"query": {"multi_match": {"query": "john smith", "type": "cross_fields", "fields": ["first", "last"], "operator": "and"}}}`)
	if n != 2 {
		t.Fatalf("cross_fields and: %d", n)
	}
	n, _ = c.Count("p", `{"query": {"multi_match": {"query": "john smith", "type": "cross_fields", "fields": ["first", "last"]}}}`)
	if n != 3 {
		t.Fatalf("cross_fields or: %d", n)
	}
	// filters do not change scores
	res := mustDo(t, c, http.MethodPost, "/p/_search", `{"query": {"bool": {"must": [{"match_all": {}}], "filter": [{"term": {"last.keyword": "smith"}}]}}}`)
	hits := res["hits"].(map[string]any)["hits"].([]any)
	if len(hits) != 2 || hits[0].(map[string]any)["_score"].(float64) != 1 {
		t.Fatalf("filtered match_all score: %v", hits)
	}
	direct := mustDo(t, c, http.MethodPost, "/p/_search", `{"query": {"match": {"first": "john"}}, "sort": ["_doc"]}`)
	filtered := mustDo(t, c, http.MethodPost, "/p/_search", `{"query": {"bool": {"must": {"match": {"first": "john"}}, "filter": {"exists": {"field": "last"}}}}, "sort": ["_doc"]}`)
	ds := direct["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["_score"]
	fs := filtered["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["_score"]
	if ds != fs {
		t.Fatalf("filter changed score: %v vs %v", ds, fs)
	}
	res = mustDo(t, c, http.MethodPost, "/p/_search", `{"query": {"constant_score": {"filter": {"term": {"last.keyword": "smith"}}, "boost": 3}}}`)
	if res["hits"].(map[string]any)["max_score"].(float64) != 3 {
		t.Fatalf("constant_score boost: %v", res["hits"])
	}
}

func TestPhraseSlop(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/t/_doc/1", `{"txt": "the quick brown fox"}`)
	mustDo(t, c, http.MethodPut, "/t/_doc/2", `{"txt": "the fox is quick"}`)
	mustDo(t, c, http.MethodPut, "/t/_doc/3", `{"txt": "quick red fox"}`)
	for q, want := range map[string]int{
		`{"match_phrase": {"txt": "quick fox"}}`:                             0,
		`{"match_phrase": {"txt": {"query": "quick fox", "slop": 1}}}`:       2, // one word in between (docs 1 and 3)
		`{"match_phrase": {"txt": {"query": "quick fox", "slop": 2}}}`:       2,
		`{"match_phrase": {"txt": {"query": "fox quick", "slop": 2}}}`:       1, // "fox is quick"; reversed order needs 3
		`{"match_phrase": {"txt": {"query": "fox quick", "slop": 3}}}`:       3,
		`{"match_phrase": {"txt": {"query": "quick brown fox", "slop": 0}}}`: 1,
	} {
		n, err := c.Count("t", `{"query": `+q+`}`)
		if err != nil || n != want {
			t.Errorf("%s: got %d want %d (%v)", q, n, want, err)
		}
	}
}

func TestHTTPEdgeCases(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	// unknown uri -> 400 with a string error
	res, _ := c.Do(http.MethodGet, "/_ingest/pipeline/x", nil)
	if res.StatusCode != 400 || !strings.Contains(string(res.Body), `"error":"no handler found for uri [/_ingest/pipeline/x] and method [GET]"`) {
		t.Fatalf("unknown uri: %d %s", res.StatusCode, res.Body)
	}
	res, _ = c.Do(http.MethodPatch, "/products/_search", nil)
	if res.StatusCode != 405 {
		t.Fatalf("wrong method: %d", res.StatusCode)
	}
	// numeric ids
	n, _ := c.Count("products", `{"query": {"ids": {"values": [1, "2"]}}}`)
	if n != 2 {
		t.Fatalf("numeric ids: %d", n)
	}
	// rest_total_hits_as_int
	r := mustDo(t, c, http.MethodPost, "/products/_search?rest_total_hits_as_int=true", `{"size": 0}`)
	if r["hits"].(map[string]any)["total"].(float64) != 5 {
		t.Fatalf("total as int: %v", r["hits"])
	}
	// gzip request body
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(`{"query": {"term": {"tags": "red"}}}`))
	_ = gz.Close()
	srv := c.MustServe()
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/products/_search", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 1 {
		t.Fatalf("gzip search: %v", out)
	}
	// nodes report the real address for sniffing clients
	nodes := mustDo(t, c, http.MethodGet, "/_nodes/http", nil)
	addr := nodes["nodes"].(map[string]any)["osmem-node"].(map[string]any)["http"].(map[string]any)["publish_address"].(string)
	if "http://"+addr != srv.URL {
		t.Fatalf("publish_address %q vs %q", addr, srv.URL)
	}
	// deleting through an alias name is refused
	mustDo(t, c, http.MethodPut, "/products/_alias/prod", nil)
	st, body := status(t, c, http.MethodDelete, "/prod", nil)
	if st != 400 || errType(body) != "illegal_argument_exception" {
		t.Fatalf("delete alias: %d %v", st, body)
	}
	// bulk without index fails as a whole
	st, body = status(t, c, http.MethodPost, "/_bulk", "{\"index\": {\"_id\": \"1\"}}\n{\"a\": 1}\n")
	if st != 400 || errType(body) != "action_request_validation_exception" {
		t.Fatalf("bulk validation: %d %v", st, body)
	}
	// terms missing keeps its numeric type on unmapped fields
	r = mustDo(t, c, http.MethodPost, "/products/_search", `{"size": 0, "aggs": {"t": {"terms": {"field": "nope", "missing": 0}}}}`)
	b := r["aggregations"].(map[string]any)["t"].(map[string]any)["buckets"].([]any)
	if b[0].(map[string]any)["key"].(float64) != 0 {
		t.Fatalf("missing key type: %v", b)
	}
	// compatibility mode for old Elasticsearch clients
	mustDo(t, c, http.MethodPut, "/_cluster/settings", `{"persistent": {"compatibility.override_main_response_version": true}}`)
	info := mustDo(t, c, http.MethodGet, "/", nil)
	if info["version"].(map[string]any)["number"] != "7.10.2" {
		t.Fatalf("compat info: %v", info)
	}
	// index template is reported in OpenSearch's shape
	mustDo(t, c, http.MethodPut, "/_index_template/tpl", `{"index_patterns": ["tpl-*"], "template": {"settings": {"number_of_shards": 1}}}`)
	r = mustDo(t, c, http.MethodGet, "/_index_template/tpl", nil)
	tpl := r["index_templates"].([]any)[0].(map[string]any)["index_template"].(map[string]any)
	if tpl["priority"].(float64) != 0 || tpl["template"].(map[string]any)["settings"].(map[string]any)["index"].(map[string]any)["number_of_shards"] != "1" {
		t.Fatalf("template shape: %v", tpl)
	}
}

func TestConcurrentScrollAndSearch(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			clone := c.Clone()
			defer clone.Close()
			for j := 0; j < 5; j++ {
				res := mustDo(t, clone, http.MethodPost, "/products/_search?scroll=1m", `{"size": 2, "sort": [{"created": "desc"}], "query": {"range": {"created": {"gte": "now-1y/d"}}}}`)
				id := res["_scroll_id"].(string)
				mustDo(t, clone, http.MethodPost, "/_search/scroll", `{"scroll": "1m", "scroll_id": "`+id+`"}`)
				mustDo(t, clone, http.MethodPut, "/products/_doc/x", `{"name": "x", "created": "2024-03-10"}`)
				mustDo(t, c, http.MethodPost, "/products/_search", `{"aggs": {"d": {"date_histogram": {"field": "created", "calendar_interval": "day", "format": "yyyy-MM-dd"}}}}`)
			}
		}()
	}
	wg.Wait()
}
