package osmem

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func seedCluster(t *testing.T) *Cluster {
	t.Helper()
	c := New(WithClock(func() time.Time { return time.Date(2024, 3, 15, 12, 0, 0, 0, time.UTC) }))
	mustDo(t, c, http.MethodPut, "/products", `{
	  "settings": {"index": {"number_of_replicas": 0}, "analysis": {
	    "normalizer": {"lc": {"type": "custom", "filter": ["lowercase"]}},
	    "analyzer": {"autocomplete": {"tokenizer": "standard", "filter": ["lowercase", "edge"]}},
	    "filter": {"edge": {"type": "edge_ngram", "min_gram": 2, "max_gram": 10}}
	  }},
	  "mappings": {"properties": {
	    "name": {"type": "text", "fields": {"keyword": {"type": "keyword"}, "auto": {"type": "text", "analyzer": "autocomplete"}}},
	    "sku": {"type": "keyword", "normalizer": "lc"},
	    "price": {"type": "double"},
	    "stock": {"type": "integer"},
	    "tags": {"type": "keyword"},
	    "created": {"type": "date"},
	    "active": {"type": "boolean"},
	    "location": {"type": "geo_point"},
	    "vendor": {"properties": {"name": {"type": "keyword"}, "rating": {"type": "float"}}},
	    "desc": {"type": "text"}
	  }}
	}`)
	docs := []string{
		`{"name": "Red Apple", "sku": "APL-R", "price": 1.5, "stock": 10, "tags": ["fruit", "red"], "created": "2024-01-05T10:00:00Z", "active": true, "location": {"lat": 35.68, "lon": 139.76}, "vendor": {"name": "acme", "rating": 4.5}, "desc": "a crisp red apple from the north"}`,
		`{"name": "Green Apple", "sku": "APL-G", "price": 1.2, "stock": 0, "tags": ["fruit", "green"], "created": "2024-02-10", "active": false, "location": "34.69,135.50", "vendor": {"name": "acme", "rating": 3.0}, "desc": "a sour green apple"}`,
		`{"name": "Banana", "sku": "BAN-1", "price": 0.5, "stock": 3, "tags": ["fruit", "yellow"], "created": "2024-02-20T00:00:00Z", "active": true, "location": [139.0, 35.0], "vendor": {"name": "fruitco", "rating": 4.0}, "desc": "bananas are yellow when ripe"}`,
		`{"name": "Carrot", "sku": "CAR-1", "price": 0.3, "stock": 100, "tags": ["vegetable", "orange"], "created": "2023-12-31T23:59:59Z", "active": true, "vendor": {"name": "fruitco", "rating": 2.5}, "desc": "orange root vegetable"}`,
		`{"name": "Dragon Fruit", "sku": "DRG-1", "price": 4.0, "tags": ["fruit", "exotic"], "created": "2024-03-01T08:30:00+09:00", "active": true, "vendor": {"name": "exotic-imports", "rating": 5.0}, "desc": "pink exotic fruit"}`,
	}
	var sb strings.Builder
	for i, d := range docs {
		fmt.Fprintf(&sb, "{\"index\":{\"_index\":\"products\",\"_id\":\"%d\"}}\n%s\n", i+1, d)
	}
	if err := c.BulkString(sb.String()); err != nil {
		t.Fatal(err)
	}
	return c
}

func search(t *testing.T, c *Cluster, body string) (ids []string, res map[string]any) {
	t.Helper()
	res = mustDo(t, c, http.MethodPost, "/products/_search", body)
	for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
		ids = append(ids, h.(map[string]any)["_id"].(string))
	}
	return ids, res
}

func assertIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func assertSet(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

func status(t *testing.T, c *Cluster, method, path string, body any) (int, map[string]any) {
	t.Helper()
	res, err := c.Do(method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(res.Body, &out)
	return res.StatusCode, out
}

func errType(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	s, _ := e["type"].(string)
	return s
}

func TestQueries(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	cases := []struct {
		name  string
		query string
		want  []string
		order bool
	}{
		{"match_all", `{"match_all": {}}`, []string{"1", "2", "3", "4", "5"}, false},
		{"match text", `{"match": {"name": "apple"}}`, []string{"1", "2"}, false},
		{"match and", `{"match": {"name": {"query": "red apple", "operator": "and"}}}`, []string{"1"}, false},
		{"match msm", `{"match": {"desc": {"query": "crisp sour orange banana", "minimum_should_match": 1}}}`, []string{"1", "2", "4"}, false},
		{"match keyword", `{"match": {"tags": "fruit"}}`, []string{"1", "2", "3", "5"}, false},
		{"match numeric", `{"match": {"stock": 10}}`, []string{"1"}, false},
		{"match bool", `{"match": {"active": false}}`, []string{"2"}, false},
		{"match fuzzy", `{"match": {"name": {"query": "aple", "fuzziness": "AUTO"}}}`, []string{"1", "2"}, false},
		{"match_phrase", `{"match_phrase": {"desc": "red apple"}}`, []string{"1"}, false},
		{"match_phrase reversed", `{"match_phrase": {"desc": "apple red"}}`, nil, false},
		{"match_phrase_prefix", `{"match_phrase_prefix": {"desc": "sour gr"}}`, []string{"2"}, false},
		{"match_bool_prefix", `{"match_bool_prefix": {"desc": "yellow ba"}}`, []string{"3"}, false},
		{"multi_match", `{"multi_match": {"query": "orange", "fields": ["name", "desc"]}}`, []string{"4"}, false},
		{"multi_match wildcard fields", `{"multi_match": {"query": "pink", "fields": ["*"]}}`, []string{"5"}, false},
		{"multi_match boost", `{"multi_match": {"query": "apple", "fields": ["name^3", "desc"]}}`, []string{"1", "2"}, false},
		{"term keyword", `{"term": {"tags": "red"}}`, []string{"1"}, false},
		{"term keyword.sub", `{"term": {"name.keyword": "Red Apple"}}`, []string{"1"}, false},
		{"term keyword.sub miss", `{"term": {"name.keyword": "red apple"}}`, nil, false},
		{"term normalizer", `{"term": {"sku": "apl-r"}}`, []string{"1"}, false},
		{"term normalizer upper", `{"term": {"sku": "APL-R"}}`, []string{"1"}, false},
		{"term text token", `{"term": {"name": "apple"}}`, []string{"1", "2"}, false},
		{"term text raw", `{"term": {"name": "Apple"}}`, nil, false},
		{"term numeric", `{"term": {"stock": {"value": 100}}}`, []string{"4"}, false},
		{"term numeric string", `{"term": {"stock": "3"}}`, []string{"3"}, false},
		{"term bool", `{"term": {"active": true}}`, []string{"1", "3", "4", "5"}, false},
		{"term date", `{"term": {"created": "2024-02-10"}}`, []string{"2"}, false},
		{"term _id", `{"term": {"_id": "3"}}`, []string{"3"}, false},
		{"term nested", `{"term": {"vendor.name": "acme"}}`, []string{"1", "2"}, false},
		{"term unmapped", `{"term": {"nope": "x"}}`, nil, false},
		{"term case_insensitive", `{"term": {"tags": {"value": "RED", "case_insensitive": true}}}`, []string{"1"}, false},
		{"terms", `{"terms": {"tags": ["red", "yellow"]}}`, []string{"1", "3"}, false},
		{"terms numeric", `{"terms": {"stock": [0, 3]}}`, []string{"2", "3"}, false},
		{"terms lookup", `{"terms": {"tags": {"index": "products", "id": "1", "path": "tags"}}}`, []string{"1", "2", "3", "5"}, false},
		{"ids", `{"ids": {"values": ["2", "4"]}}`, []string{"2", "4"}, false},
		{"range numeric", `{"range": {"price": {"gte": 1, "lt": 4}}}`, []string{"1", "2"}, false},
		{"range numeric string", `{"range": {"price": {"gt": "1.2"}}}`, []string{"1", "5"}, false},
		{"range date", `{"range": {"created": {"gte": "2024-02-01", "lte": "2024-02-29"}}}`, []string{"2", "3", "5"}, false},
		{"range date lt month", `{"range": {"created": {"lt": "2024-02"}}}`, []string{"1", "4"}, false},
		{"range date lte partial", `{"range": {"created": {"lte": "2024-02-10"}}}`, []string{"1", "2", "4"}, false},
		{"range date math", `{"range": {"created": {"gte": "now-30d/d"}}}`, []string{"3", "5"}, false},
		{"range date tz", `{"range": {"created": {"gte": "2024-03-01", "time_zone": "+09:00"}}}`, []string{"5"}, false},
		{"range date format", `{"range": {"created": {"gte": "05/01/2024", "lt": "06/01/2024", "format": "dd/MM/yyyy"}}}`, []string{"1"}, false},
		{"range keyword", `{"range": {"sku": {"gte": "b", "lt": "d"}}}`, []string{"3", "4"}, false},
		{"exists", `{"exists": {"field": "stock"}}`, []string{"1", "2", "3", "4"}, false},
		{"exists object", `{"exists": {"field": "vendor"}}`, []string{"1", "2", "3", "4", "5"}, false},
		{"exists subfield", `{"exists": {"field": "name.keyword"}}`, []string{"1", "2", "3", "4", "5"}, false},
		{"exists missing", `{"exists": {"field": "location"}}`, []string{"1", "2", "3"}, false},
		{"prefix", `{"prefix": {"sku": "apl"}}`, []string{"1", "2"}, false},
		{"prefix text", `{"prefix": {"name": "ban"}}`, []string{"3"}, false},
		{"wildcard", `{"wildcard": {"tags": "gr*n"}}`, []string{"2"}, false},
		{"wildcard ci", `{"wildcard": {"name.keyword": {"value": "*apple", "case_insensitive": true}}}`, []string{"1", "2"}, false},
		{"regexp", `{"regexp": {"tags": "(red|yellow)"}}`, []string{"1", "3"}, false},
		{"fuzzy", `{"fuzzy": {"tags": {"value": "yelow"}}}`, []string{"3"}, false},
		{"bool must+filter", `{"bool": {"must": [{"match": {"desc": "apple"}}], "filter": [{"term": {"active": true}}]}}`, []string{"1"}, false},
		{"bool should", `{"bool": {"should": [{"term": {"tags": "red"}}, {"term": {"tags": "orange"}}]}}`, []string{"1", "4"}, false},
		{"bool should msm", `{"bool": {"should": [{"term": {"tags": "fruit"}}, {"term": {"active": true}}, {"range": {"price": {"gt": 1}}}], "minimum_should_match": 2}}`, []string{"1", "2", "3", "5"}, false},
		{"bool should optional", `{"bool": {"must": [{"term": {"tags": "fruit"}}], "should": [{"term": {"tags": "red"}}]}}`, []string{"1", "2", "3", "5"}, false},
		{"bool must_not only", `{"bool": {"must_not": [{"term": {"tags": "fruit"}}]}}`, []string{"4"}, false},
		{"bool filter only", `{"bool": {"filter": {"range": {"stock": {"lte": 3}}}}}`, []string{"2", "3"}, false},
		{"bool empty", `{"bool": {}}`, []string{"1", "2", "3", "4", "5"}, false},
		{"nested bool", `{"bool": {"must": {"bool": {"should": [{"term": {"tags": "red"}}, {"term": {"tags": "green"}}]}}, "must_not": {"term": {"active": false}}}}`, []string{"1"}, false},
		{"constant_score", `{"constant_score": {"filter": {"term": {"tags": "exotic"}}, "boost": 2}}`, []string{"5"}, false},
		{"dis_max", `{"dis_max": {"queries": [{"term": {"tags": "red"}}, {"term": {"tags": "green"}}]}}`, []string{"1", "2"}, false},
		{"query_string", `{"query_string": {"query": "apple AND NOT green"}}`, []string{"1"}, false},
		{"query_string field", `{"query_string": {"query": "tags:red OR sku:BAN-1"}}`, []string{"1", "3"}, false},
		{"query_string phrase", `{"query_string": {"query": "\"green apple\"", "default_field": "desc"}}`, []string{"2"}, false},
		{"query_string range", `{"query_string": {"query": "price:[1 TO 2]"}}`, []string{"1", "2"}, false},
		{"query_string gt", `{"query_string": {"query": "stock:>5"}}`, []string{"1", "4"}, false},
		{"query_string wildcard", `{"query_string": {"query": "name:ban*"}}`, []string{"3"}, false},
		{"query_string default and", `{"query_string": {"query": "red apple", "default_operator": "AND", "fields": ["desc"]}}`, []string{"1"}, false},
		{"query_string exists", `{"query_string": {"query": "_exists_:location"}}`, []string{"1", "2", "3"}, false},
		{"simple_query_string", `{"simple_query_string": {"query": "apple -green", "fields": ["name"]}}`, []string{"1"}, false},
		{"simple_query_string or", `{"simple_query_string": {"query": "banana | carrot", "fields": ["name"]}}`, []string{"3", "4"}, false},
		{"geo_distance", `{"geo_distance": {"distance": "100km", "location": {"lat": 35.6, "lon": 139.7}}}`, []string{"1", "3"}, false},
		{"geo_bounding_box", `{"geo_bounding_box": {"location": {"top_left": {"lat": 36, "lon": 135}, "bottom_right": {"lat": 34, "lon": 136}}}}`, []string{"2"}, false},
		{"custom analyzer", `{"match": {"name.auto": "dra"}}`, []string{"5"}, false},
		{"match_none", `{"match_none": {}}`, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids, _ := search(t, c, `{"query": `+tc.query+`, "sort": ["_doc"]}`)
			assertSet(t, ids, tc.want...)
		})
	}
}

func TestQueryErrors(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	cases := []struct {
		name string
		body string
		typ  string
	}{
		{"unknown query", `{"query": {"nope": {}}}`, "parsing_exception"},
		{"empty query", `{"query": {}}`, "parsing_exception"},
		{"unknown key", `{"quer": {}}`, "parsing_exception"},
		{"script query", `{"query": {"script": {"script": "true"}}}`, "unsupported_operation_exception"},
		{"sort text", `{"sort": ["name"]}`, "search_phase_execution_exception"},
		{"sort unmapped", `{"sort": ["nope"]}`, "search_phase_execution_exception"},
		{"window", `{"from": 9999, "size": 10}`, "search_phase_execution_exception"},
		{"number format", `{"query": {"term": {"stock": "abc"}}}`, "search_phase_execution_exception"},
		{"agg on text", `{"aggs": {"x": {"terms": {"field": "name"}}}}`, "search_phase_execution_exception"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, body := status(t, c, http.MethodPost, "/products/_search", tc.body)
			if st != 400 || errType(body) != tc.typ {
				t.Fatalf("status=%d body=%v", st, body)
			}
		})
	}
	st, body := status(t, c, http.MethodPost, "/missing/_search", `{}`)
	if st != 404 || errType(body) != "index_not_found_exception" {
		t.Fatalf("missing index: %d %v", st, body)
	}
	st, _ = status(t, c, http.MethodPost, "/missing/_search?ignore_unavailable=true", `{}`)
	if st != 200 {
		t.Fatalf("ignore_unavailable: %d", st)
	}
	st, _ = status(t, c, http.MethodPost, "/miss*/_search", `{}`)
	if st != 200 {
		t.Fatalf("wildcard no match: %d", st)
	}
}

func TestSortingAndPaging(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	ids, res := search(t, c, `{"sort": [{"price": "desc"}], "size": 2}`)
	assertIDs(t, ids, "5", "1")
	hits := res["hits"].(map[string]any)
	if hits["total"].(map[string]any)["value"].(float64) != 5 {
		t.Fatalf("total %v", hits["total"])
	}
	first := hits["hits"].([]any)[0].(map[string]any)
	if first["_score"] != nil {
		t.Fatalf("score should be null when sorting by field: %v", first["_score"])
	}
	if sv := first["sort"].([]any); sv[0].(float64) != 4.0 {
		t.Fatalf("sort values %v", sv)
	}
	ids, _ = search(t, c, `{"sort": [{"price": "desc"}], "size": 2, "from": 2}`)
	assertIDs(t, ids, "2", "3")
	ids, _ = search(t, c, `{"sort": [{"price": "desc"}], "size": 2, "search_after": [1.5]}`)
	assertIDs(t, ids, "2", "3")
	ids, _ = search(t, c, `{"sort": [{"vendor.name": "asc"}, {"price": "asc"}]}`)
	assertIDs(t, ids, "2", "1", "5", "4", "3")
	ids, _ = search(t, c, `{"sort": [{"stock": {"order": "asc", "missing": "_first"}}]}`)
	assertIDs(t, ids, "5", "2", "3", "1", "4")
	ids, _ = search(t, c, `{"sort": [{"stock": {"order": "asc"}}]}`)
	assertIDs(t, ids, "2", "3", "1", "4", "5")
	ids, res = search(t, c, `{"sort": [{"created": "asc"}], "size": 1}`)
	assertIDs(t, ids, "4")
	sv := res["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["sort"].([]any)
	if sv[0].(float64) != float64(time.Date(2023, 12, 31, 23, 59, 59, 0, time.UTC).UnixMilli()) {
		t.Fatalf("date sort value %v", sv)
	}
	ids, _ = search(t, c, `{"sort": [{"created": {"order": "asc", "format": "yyyy-MM-dd"}}], "search_after": ["2024-02-10"]}`)
	assertIDs(t, ids, "3", "5")
	ids, _ = search(t, c, `{"sort": ["_doc"]}`)
	assertIDs(t, ids, "1", "2", "3", "4", "5")
	ids, _ = search(t, c, `{"query": {"match": {"desc": "apple"}}, "sort": ["_score", {"price": "asc"}]}`)
	assertSet(t, ids, "1", "2")
	// tags is multi-valued: mode
	ids, _ = search(t, c, `{"sort": [{"tags": {"order": "asc", "mode": "max"}}]}`)
	assertIDs(t, ids, "5", "2", "1", "4", "3")
	_, res = search(t, c, `{"track_total_hits": false}`)
	if _, ok := res["hits"].(map[string]any)["total"]; ok {
		t.Fatalf("total should be absent")
	}
	_, res = search(t, c, `{"track_total_hits": 2, "size": 0}`)
	tot := res["hits"].(map[string]any)["total"].(map[string]any)
	if tot["value"].(float64) != 2 || tot["relation"] != "gte" {
		t.Fatalf("capped total %v", tot)
	}
	_, res = search(t, c, `{"query": {"match": {"desc": "apple"}}, "min_score": 100}`)
	if len(res["hits"].(map[string]any)["hits"].([]any)) != 0 {
		t.Fatalf("min_score")
	}
	// collapse
	ids, res = search(t, c, `{"collapse": {"field": "vendor.name"}, "sort": [{"price": "desc"}]}`)
	assertIDs(t, ids, "5", "1", "3")
	f := res["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["fields"].(map[string]any)
	if f["vendor.name"].([]any)[0] != "exotic-imports" {
		t.Fatalf("collapse fields %v", f)
	}
	// post_filter keeps aggregations on the full set
	ids, res = search(t, c, `{"query": {"term": {"tags": "fruit"}}, "post_filter": {"term": {"tags": "red"}}, "aggs": {"t": {"terms": {"field": "vendor.name"}}}, "sort": ["_doc"]}`)
	assertIDs(t, ids, "1")
	b := res["aggregations"].(map[string]any)["t"].(map[string]any)["buckets"].([]any)
	if len(b) != 3 {
		t.Fatalf("post_filter aggs %v", b)
	}
	// size param and q param
	_, res = search(t, c, `{"query": {"match_all": {}}, "size": 0}`)
	if len(res["hits"].(map[string]any)["hits"].([]any)) != 0 {
		t.Fatal("size 0")
	}
	r := mustDo(t, c, http.MethodGet, "/products/_search?q=tags:red&size=1", nil)
	if r["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 1 {
		t.Fatalf("q param: %v", r["hits"])
	}
}

func TestSourceAndFields(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	_, res := search(t, c, `{"query": {"ids": {"values": ["1"]}}, "_source": ["name", "vendor.rating"]}`)
	src := res["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["_source"].(map[string]any)
	if len(src) != 2 || src["name"] != "Red Apple" || src["vendor"].(map[string]any)["rating"].(float64) != 4.5 {
		t.Fatalf("source includes: %v", src)
	}
	_, res = search(t, c, `{"query": {"ids": {"values": ["1"]}}, "_source": {"excludes": ["vendor", "t*"]}}`)
	src = res["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["_source"].(map[string]any)
	if _, ok := src["vendor"]; ok || src["tags"] != nil || src["name"] == nil {
		t.Fatalf("source excludes: %v", src)
	}
	_, res = search(t, c, `{"query": {"ids": {"values": ["1"]}}, "_source": false, "fields": ["created", {"field": "price"}], "version": true, "seq_no_primary_term": true}`)
	h := res["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)
	if _, ok := h["_source"]; ok {
		t.Fatalf("source disabled")
	}
	f := h["fields"].(map[string]any)
	if f["created"].([]any)[0] != "2024-01-05T10:00:00.000Z" || f["price"].([]any)[0].(float64) != 1.5 {
		t.Fatalf("fields %v", f)
	}
	if h["_version"].(float64) != 1 || h["_seq_no"] == nil {
		t.Fatalf("version %v", h)
	}
	r := mustDo(t, c, http.MethodGet, "/products/_doc/1?_source_includes=name,price", nil)
	if len(r["_source"].(map[string]any)) != 2 {
		t.Fatalf("get source filter: %v", r)
	}
	r = mustDo(t, c, http.MethodGet, "/products/_source/2", nil)
	if r["name"] != "Green Apple" {
		t.Fatalf("_source: %v", r)
	}
	// original key order of the document is preserved in _source
	raw, _ := c.Do(http.MethodGet, "/products/_doc/1", nil)
	if !strings.Contains(string(raw.Body), `"_source":{"name":"Red Apple","sku":"APL-R"`) {
		t.Fatalf("raw source order: %s", raw.Body)
	}
	// filter_path
	raw, _ = c.Do(http.MethodPost, "/products/_search?filter_path=hits.total.value,hits.hits._id", `{"size": 1}`)
	var fp map[string]any
	_ = json.Unmarshal(raw.Body, &fp)
	if len(fp) != 1 || fp["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 5 {
		t.Fatalf("filter_path: %s", raw.Body)
	}
	if h := fp["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any); len(h) != 1 || h["_id"] == nil {
		t.Fatalf("filter_path hits: %s", raw.Body)
	}
}

func TestHighlight(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	_, res := search(t, c, `{"query": {"match": {"desc": "apple red"}}, "highlight": {"fields": {"desc": {}, "name": {}}, "pre_tags": ["<b>"], "post_tags": ["</b>"]}, "sort": ["_doc"]}`)
	hl := res["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["highlight"].(map[string]any)
	if got := hl["desc"].([]any)[0]; got != "a crisp <b>red</b> <b>apple</b> from the north" {
		t.Fatalf("highlight: %v", got)
	}
	if _, ok := hl["name"]; ok {
		t.Fatalf("name should not be highlighted (require_field_match): %v", hl)
	}
	_, res = search(t, c, `{"query": {"term": {"tags": "green"}}, "highlight": {"fields": {"tags": {}}}}`)
	hl = res["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["highlight"].(map[string]any)
	if got := hl["tags"].([]any); len(got) != 1 || got[0] != "<em>green</em>" {
		t.Fatalf("array highlight: %v", got)
	}
	_, res = search(t, c, `{"query": {"match": {"desc": "yellow"}}, "highlight": {"fields": {"*": {}}}}`)
	hl = res["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["highlight"].(map[string]any)
	if hl["desc"] == nil {
		t.Fatalf("wildcard highlight: %v", hl)
	}
}

func TestAggregations(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	aggs := func(body string) map[string]any {
		t.Helper()
		_, res := search(t, c, `{"size": 0, "aggs": `+body+`}`)
		return res["aggregations"].(map[string]any)
	}
	buckets := func(a map[string]any, name string) []map[string]any {
		var out []map[string]any
		for _, b := range a[name].(map[string]any)["buckets"].([]any) {
			out = append(out, b.(map[string]any))
		}
		return out
	}
	b := buckets(aggs(`{"t": {"terms": {"field": "tags"}}}`), "t")
	if b[0]["key"] != "fruit" || b[0]["doc_count"].(float64) != 4 || len(b) != 7 || b[1]["key"] != "exotic" {
		t.Fatalf("terms: %v", b)
	}
	b = buckets(aggs(`{"t": {"terms": {"field": "tags", "size": 2, "order": {"_key": "desc"}}}}`), "t")
	if len(b) != 2 || b[0]["key"] != "yellow" || b[1]["key"] != "vegetable" {
		t.Fatalf("terms order: %v", b)
	}
	a := aggs(`{"t": {"terms": {"field": "vendor.name", "order": {"avg_price": "desc"}}, "aggs": {"avg_price": {"avg": {"field": "price"}}}}}`)
	b = buckets(a, "t")
	if b[0]["key"] != "exotic-imports" || b[1]["key"] != "acme" || b[1]["avg_price"].(map[string]any)["value"].(float64) != 1.35 {
		t.Fatalf("terms sub order: %v", b)
	}
	if a["t"].(map[string]any)["sum_other_doc_count"].(float64) != 0 {
		t.Fatalf("sum_other: %v", a)
	}
	b = buckets(aggs(`{"t": {"terms": {"field": "active"}}}`), "t")
	if b[0]["key"].(float64) != 1 || b[0]["key_as_string"] != "true" || b[0]["doc_count"].(float64) != 4 {
		t.Fatalf("bool terms: %v", b)
	}
	b = buckets(aggs(`{"t": {"terms": {"field": "stock", "missing": -1, "order": {"_key": "asc"}}}}`), "t")
	if b[0]["key"].(float64) != -1 || b[1]["key"].(float64) != 0 || len(b) != 5 {
		t.Fatalf("numeric terms with missing: %v", b)
	}
	b = buckets(aggs(`{"t": {"terms": {"field": "tags", "include": ["red", "green"]}}}`), "t")
	if len(b) != 2 {
		t.Fatalf("include: %v", b)
	}
	b = buckets(aggs(`{"t": {"terms": {"field": "tags", "exclude": "fr.*"}}}`), "t")
	if len(b) != 6 {
		t.Fatalf("exclude regex: %v", b)
	}
	b = buckets(aggs(`{"t": {"multi_terms": {"terms": [{"field": "vendor.name"}, {"field": "active"}]}}}`), "t")
	if len(b) != 4 || b[0]["key_as_string"] != "acme|true" && b[0]["key_as_string"] != "fruitco|true" {
		t.Fatalf("multi_terms: %v", b)
	}
	// range
	b = buckets(aggs(`{"r": {"range": {"field": "price", "ranges": [{"to": 1}, {"from": 1, "to": 2}, {"from": 2}]}}}`), "r")
	if b[0]["key"] != "*-1.0" || b[0]["doc_count"].(float64) != 2 || b[1]["key"] != "1.0-2.0" || b[2]["doc_count"].(float64) != 1 {
		t.Fatalf("range: %v", b)
	}
	a = aggs(`{"r": {"range": {"field": "price", "keyed": true, "ranges": [{"key": "cheap", "to": 1}]}}}`)
	if a["r"].(map[string]any)["buckets"].(map[string]any)["cheap"].(map[string]any)["doc_count"].(float64) != 2 {
		t.Fatalf("keyed range: %v", a)
	}
	b = buckets(aggs(`{"r": {"date_range": {"field": "created", "ranges": [{"to": "2024-02-01"}, {"from": "2024-02-01", "to": "now"}]}}}`), "r")
	if b[0]["doc_count"].(float64) != 2 || b[1]["doc_count"].(float64) != 3 || b[1]["from_as_string"] != "2024-02-01T00:00:00.000Z" {
		t.Fatalf("date_range: %v", b)
	}
	// histogram
	b = buckets(aggs(`{"h": {"histogram": {"field": "price", "interval": 1}}}`), "h")
	if len(b) != 5 || b[0]["key"].(float64) != 0 || b[0]["doc_count"].(float64) != 2 || b[2]["doc_count"].(float64) != 0 || b[4]["key"].(float64) != 4 {
		t.Fatalf("histogram: %v", b)
	}
	b = buckets(aggs(`{"h": {"histogram": {"field": "price", "interval": 1, "min_doc_count": 1}}}`), "h")
	if len(b) != 3 {
		t.Fatalf("histogram min_doc_count: %v", b)
	}
	b = buckets(aggs(`{"h": {"date_histogram": {"field": "created", "calendar_interval": "month"}}}`), "h")
	if len(b) != 3 || b[0]["key_as_string"] != "2023-12-01T00:00:00.000Z" || b[2]["doc_count"].(float64) != 3 || b[2]["key_as_string"] != "2024-02-01T00:00:00.000Z" {
		t.Fatalf("date_histogram: %v", b)
	}
	b = buckets(aggs(`{"h": {"date_histogram": {"field": "created", "calendar_interval": "1d", "min_doc_count": 1, "format": "yyyy-MM-dd", "time_zone": "+09:00"}}}`), "h")
	if len(b) != 5 || b[0]["key_as_string"] != "2024-01-01" || b[4]["key_as_string"] != "2024-03-01" {
		t.Fatalf("date_histogram tz: %v", b)
	}
	b = buckets(aggs(`{"h": {"date_histogram": {"field": "created", "fixed_interval": "30d", "min_doc_count": 1}}}`), "h")
	if len(b) < 2 {
		t.Fatalf("fixed_interval: %v", b)
	}
	b = buckets(aggs(`{"h": {"date_histogram": {"field": "created", "calendar_interval": "year", "extended_bounds": {"min": "2022-01-01", "max": "2024-12-31"}}}}`), "h")
	if len(b) != 3 || b[0]["doc_count"].(float64) != 0 {
		t.Fatalf("extended_bounds: %v", b)
	}
	// filter / filters / missing / global
	a = aggs(`{"f": {"filter": {"term": {"tags": "fruit"}}, "aggs": {"max_price": {"max": {"field": "price"}}}}}`)
	if a["f"].(map[string]any)["doc_count"].(float64) != 4 || a["f"].(map[string]any)["max_price"].(map[string]any)["value"].(float64) != 4 {
		t.Fatalf("filter: %v", a)
	}
	a = aggs(`{"f": {"filters": {"filters": {"cheap": {"range": {"price": {"lt": 1}}}, "active": {"term": {"active": true}}}, "other_bucket": true}}}`)
	fb := a["f"].(map[string]any)["buckets"].(map[string]any)
	if fb["cheap"].(map[string]any)["doc_count"].(float64) != 2 || fb["active"].(map[string]any)["doc_count"].(float64) != 4 || fb["_other_"].(map[string]any)["doc_count"].(float64) != 1 {
		t.Fatalf("filters: %v", fb)
	}
	a = aggs(`{"m": {"missing": {"field": "stock"}}}`)
	if a["m"].(map[string]any)["doc_count"].(float64) != 1 {
		t.Fatalf("missing: %v", a)
	}
	_, res := search(t, c, `{"size": 0, "query": {"term": {"tags": "red"}}, "aggs": {"g": {"global": {}, "aggs": {"n": {"value_count": {"field": "price"}}}}, "local": {"value_count": {"field": "price"}}}}`)
	a = res["aggregations"].(map[string]any)
	if a["g"].(map[string]any)["doc_count"].(float64) != 5 || a["local"].(map[string]any)["value"].(float64) != 1 {
		t.Fatalf("global: %v", a)
	}
	// metrics
	a = aggs(`{"s": {"stats": {"field": "price"}}, "e": {"extended_stats": {"field": "price"}}, "c": {"cardinality": {"field": "vendor.name"}}, "p": {"percentiles": {"field": "price", "percents": [50, 100]}}, "mn": {"min": {"field": "created"}}, "vc": {"value_count": {"field": "tags"}}, "sum": {"sum": {"field": "stock"}}, "avgmissing": {"avg": {"field": "nope"}}}`)
	s := a["s"].(map[string]any)
	if s["count"].(float64) != 5 || s["min"].(float64) != 0.3 || s["max"].(float64) != 4 || s["sum"].(float64) != 7.5 {
		t.Fatalf("stats: %v", s)
	}
	if a["e"].(map[string]any)["std_deviation"].(float64) <= 0 {
		t.Fatalf("extended_stats: %v", a["e"])
	}
	if a["c"].(map[string]any)["value"].(float64) != 3 {
		t.Fatalf("cardinality: %v", a["c"])
	}
	if a["p"].(map[string]any)["values"].(map[string]any)["100.0"].(float64) != 4 || a["p"].(map[string]any)["values"].(map[string]any)["50.0"].(float64) != 1.2 {
		t.Fatalf("percentiles: %v", a["p"])
	}
	if a["mn"].(map[string]any)["value_as_string"] != "2023-12-31T23:59:59.000Z" {
		t.Fatalf("min date: %v", a["mn"])
	}
	if a["vc"].(map[string]any)["value"].(float64) != 10 || a["sum"].(map[string]any)["value"].(float64) != 113 {
		t.Fatalf("value_count/sum: %v %v", a["vc"], a["sum"])
	}
	if a["avgmissing"].(map[string]any)["value"] != nil {
		t.Fatalf("avg missing: %v", a["avgmissing"])
	}
	// top_hits
	a = aggs(`{"t": {"terms": {"field": "vendor.name"}, "aggs": {"top": {"top_hits": {"size": 1, "sort": [{"price": "desc"}], "_source": ["name"]}}}}}`)
	b = buckets(a, "t")
	top := b[0]["top"].(map[string]any)["hits"].(map[string]any)
	if top["total"].(map[string]any)["value"].(float64) != 2 || top["hits"].([]any)[0].(map[string]any)["_source"].(map[string]any)["name"] != "Red Apple" {
		t.Fatalf("top_hits: %v", top)
	}
	// composite with pagination
	a = aggs(`{"c": {"composite": {"size": 2, "sources": [{"vendor": {"terms": {"field": "vendor.name"}}}, {"active": {"terms": {"field": "active"}}}]}}}`)
	cb := a["c"].(map[string]any)
	b = buckets(a, "c")
	if len(b) != 2 || b[0]["key"].(map[string]any)["vendor"] != "acme" || cb["after_key"].(map[string]any)["vendor"] != "acme" {
		t.Fatalf("composite: %v", cb)
	}
	after, _ := json.Marshal(cb["after_key"])
	a = aggs(`{"c": {"composite": {"size": 2, "after": ` + string(after) + `, "sources": [{"vendor": {"terms": {"field": "vendor.name"}}}, {"active": {"terms": {"field": "active"}}}]}}}`)
	b = buckets(a, "c")
	if len(b) != 2 || b[0]["key"].(map[string]any)["vendor"] != "exotic-imports" || b[1]["key"].(map[string]any)["vendor"] != "fruitco" {
		t.Fatalf("composite page 2: %v", b)
	}
	a = aggs(`{"c": {"composite": {"sources": [{"month": {"date_histogram": {"field": "created", "calendar_interval": "month", "format": "yyyy-MM"}}}]}}}`)
	b = buckets(a, "c")
	if len(b) != 3 || b[0]["key"].(map[string]any)["month"] != "2023-12" {
		t.Fatalf("composite date: %v", b)
	}
	// pipelines
	a = aggs(`{"m": {"date_histogram": {"field": "created", "calendar_interval": "month"}, "aggs": {"s": {"sum": {"field": "price"}}, "cum": {"cumulative_sum": {"buckets_path": "s"}}}}, "avg_m": {"avg_bucket": {"buckets_path": "m>s"}}, "max_m": {"max_bucket": {"buckets_path": "m>s"}}}`)
	b = buckets(a, "m")
	if b[2]["cum"].(map[string]any)["value"].(float64) != 7.5 {
		t.Fatalf("cumulative_sum: %v", b)
	}
	if a["avg_m"].(map[string]any)["value"].(float64) != 2.5 || a["max_m"].(map[string]any)["value"].(float64) != 5.7 {
		t.Fatalf("sibling: %v %v", a["avg_m"], a["max_m"])
	}
	a = aggs(`{"t": {"terms": {"field": "vendor.name"}, "aggs": {"s": {"sum": {"field": "price"}}, "sorted": {"bucket_sort": {"sort": [{"s": {"order": "asc"}}], "size": 2}}}}}`)
	b = buckets(a, "t")
	if len(b) != 2 || b[0]["key"] != "fruitco" {
		t.Fatalf("bucket_sort: %v", b)
	}
}

func TestDocumentAPIs(t *testing.T) {
	c := New()
	defer c.Close()
	// auto create with dynamic mapping, auto id
	r := mustDo(t, c, http.MethodPost, "/logs/_doc", `{"msg": "hello world", "level": 3, "ts": "2024-01-01T00:00:00Z", "meta": {"host": "a"}, "ok": true, "ratio": 0.5}`)
	if r["result"] != "created" || r["_id"] == "" {
		t.Fatalf("create: %v", r)
	}
	m := mustDo(t, c, http.MethodGet, "/logs/_mapping", nil)
	props := m["logs"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)
	check := func(field, typ string) {
		t.Helper()
		if props[field].(map[string]any)["type"] != typ {
			t.Fatalf("%s: %v", field, props[field])
		}
	}
	check("level", "long")
	check("ts", "date")
	check("ok", "boolean")
	check("ratio", "float")
	if props["msg"].(map[string]any)["fields"].(map[string]any)["keyword"].(map[string]any)["ignore_above"].(float64) != 256 {
		t.Fatalf("msg: %v", props["msg"])
	}
	if props["meta"].(map[string]any)["properties"].(map[string]any)["host"].(map[string]any)["type"] != "text" {
		t.Fatalf("meta: %v", props["meta"])
	}
	// versions, create conflicts, seq_no
	r = mustDo(t, c, http.MethodPut, "/logs/_doc/1", `{"msg": "one"}`)
	if r["_version"].(float64) != 1 || r["result"] != "created" {
		t.Fatalf("v1 %v", r)
	}
	r = mustDo(t, c, http.MethodPut, "/logs/_doc/1", `{"msg": "one again"}`)
	if r["_version"].(float64) != 2 || r["result"] != "updated" {
		t.Fatalf("v2 %v", r)
	}
	st, body := status(t, c, http.MethodPut, "/logs/_create/1", `{"msg": "dup"}`)
	if st != 409 || errType(body) != "version_conflict_engine_exception" {
		t.Fatalf("create conflict %d %v", st, body)
	}
	st, _ = status(t, c, http.MethodPut, "/logs/_doc/1?op_type=create", `{"msg": "dup"}`)
	if st != 409 {
		t.Fatalf("op_type create %d", st)
	}
	seq := int(r["_seq_no"].(float64))
	st, _ = status(t, c, http.MethodPut, fmt.Sprintf("/logs/_doc/1?if_seq_no=%d&if_primary_term=1", seq+5), `{"msg": "x"}`)
	if st != 409 {
		t.Fatalf("if_seq_no mismatch %d", st)
	}
	r = mustDo(t, c, http.MethodPut, fmt.Sprintf("/logs/_doc/1?if_seq_no=%d&if_primary_term=1", seq), `{"msg": "three"}`)
	if r["_version"].(float64) != 3 {
		t.Fatalf("if_seq_no ok %v", r)
	}
	// type conflict → 400
	st, body = status(t, c, http.MethodPut, "/logs/_doc/2", `{"level": "not a number"}`)
	if st != 400 || errType(body) != "mapper_parsing_exception" {
		t.Fatalf("type conflict %d %v", st, body)
	}
	// update
	r = mustDo(t, c, http.MethodPost, "/logs/_update/1", `{"doc": {"tags": ["a"], "meta": {"env": "prod"}}}`)
	if r["result"] != "updated" || r["_version"].(float64) != 4 {
		t.Fatalf("update %v", r)
	}
	r = mustDo(t, c, http.MethodGet, "/logs/_doc/1", nil)
	src := r["_source"].(map[string]any)
	if src["msg"] != "three" || src["meta"].(map[string]any)["env"] != "prod" || src["tags"].([]any)[0] != "a" {
		t.Fatalf("merged %v", src)
	}
	r = mustDo(t, c, http.MethodPost, "/logs/_update/1", `{"doc": {"tags": ["a"]}}`)
	if r["result"] != "noop" || r["_version"].(float64) != 4 {
		t.Fatalf("noop %v", r)
	}
	st, body = status(t, c, http.MethodPost, "/logs/_update/404", `{"doc": {"x": 1}}`)
	if st != 404 || errType(body) != "document_missing_exception" {
		t.Fatalf("update missing %d %v", st, body)
	}
	r = mustDo(t, c, http.MethodPost, "/logs/_update/404", `{"doc": {"x": 1}, "doc_as_upsert": true}`)
	if r["result"] != "created" {
		t.Fatalf("doc_as_upsert %v", r)
	}
	r = mustDo(t, c, http.MethodPost, "/logs/_update/405?_source=true", `{"doc": {"x": 1}, "upsert": {"y": 2}}`)
	if r["result"] != "created" || r["get"].(map[string]any)["_source"].(map[string]any)["y"].(float64) != 2 {
		t.Fatalf("upsert %v", r)
	}
	st, body = status(t, c, http.MethodPost, "/logs/_update/1", `{"script": {"source": "ctx._source.x = 1"}}`)
	if st != 400 || errType(body) != "unsupported_operation_exception" {
		t.Fatalf("script %d %v", st, body)
	}
	// exists / delete
	st, _ = status(t, c, http.MethodHead, "/logs/_doc/1", nil)
	if st != 200 {
		t.Fatalf("head %d", st)
	}
	r = mustDo(t, c, http.MethodDelete, "/logs/_doc/1", nil)
	if r["result"] != "deleted" {
		t.Fatalf("delete %v", r)
	}
	st, body = status(t, c, http.MethodDelete, "/logs/_doc/1", nil)
	if st != 404 || body["result"] != "not_found" {
		t.Fatalf("delete again %d %v", st, body)
	}
	st, _ = status(t, c, http.MethodHead, "/logs/_doc/1", nil)
	if st != 404 {
		t.Fatalf("head after delete %d", st)
	}
	// mget
	r = mustDo(t, c, http.MethodPost, "/_mget", `{"docs": [{"_index": "logs", "_id": "404"}, {"_index": "logs", "_id": "nope"}, {"_index": "nope", "_id": "1"}]}`)
	docs := r["docs"].([]any)
	if docs[0].(map[string]any)["found"] != true || docs[1].(map[string]any)["found"] != false || docs[2].(map[string]any)["error"] == nil {
		t.Fatalf("mget %v", docs)
	}
	r = mustDo(t, c, http.MethodPost, "/logs/_mget", `{"ids": ["404", "405"]}`)
	if len(r["docs"].([]any)) != 2 {
		t.Fatalf("mget ids %v", r)
	}
	// strict mapping
	mustDo(t, c, http.MethodPut, "/strict", `{"mappings": {"dynamic": "strict", "properties": {"a": {"type": "keyword"}}}}`)
	st, body = status(t, c, http.MethodPut, "/strict/_doc/1", `{"a": "x", "b": "y"}`)
	if st != 400 || errType(body) != "strict_dynamic_mapping_exception" {
		t.Fatalf("strict %d %v", st, body)
	}
	mustDo(t, c, http.MethodPut, "/nodyn", `{"mappings": {"dynamic": false, "properties": {"a": {"type": "keyword"}}}}`)
	mustDo(t, c, http.MethodPut, "/nodyn/_doc/1", `{"a": "x", "b": "y"}`)
	r = mustDo(t, c, http.MethodPost, "/nodyn/_search", `{"query": {"match": {"b": "y"}}}`)
	if r["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 0 {
		t.Fatalf("dynamic false should not index b")
	}
	r = mustDo(t, c, http.MethodGet, "/nodyn/_doc/1", nil)
	if r["_source"].(map[string]any)["b"] != "y" {
		t.Fatalf("dynamic false keeps source")
	}
	// dotted keys expand into objects
	mustDo(t, c, http.MethodPut, "/dots/_doc/1", `{"a.b": 1, "a": {"c": 2}}`)
	r = mustDo(t, c, http.MethodPost, "/dots/_search", `{"query": {"term": {"a.b": 1}}}`)
	if r["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 1 {
		t.Fatalf("dotted key")
	}
	// null values and null_value
	mustDo(t, c, http.MethodPut, "/nulls", `{"mappings": {"properties": {"s": {"type": "keyword", "null_value": "NULL"}, "n": {"type": "integer"}}}}`)
	mustDo(t, c, http.MethodPut, "/nulls/_doc/1", `{"s": null, "n": null}`)
	r = mustDo(t, c, http.MethodPost, "/nulls/_search", `{"query": {"term": {"s": "NULL"}}}`)
	if r["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 1 {
		t.Fatalf("null_value")
	}
	r = mustDo(t, c, http.MethodPost, "/nulls/_search", `{"query": {"exists": {"field": "n"}}}`)
	if r["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 0 {
		t.Fatalf("null exists")
	}
	// copy_to
	mustDo(t, c, http.MethodPut, "/copy", `{"mappings": {"properties": {"first": {"type": "text", "copy_to": "full"}, "last": {"type": "text", "copy_to": "full"}, "full": {"type": "text"}}}}`)
	mustDo(t, c, http.MethodPut, "/copy/_doc/1", `{"first": "John", "last": "Smith"}`)
	r = mustDo(t, c, http.MethodPost, "/copy/_search", `{"query": {"match": {"full": {"query": "john smith", "operator": "and"}}}}`)
	if r["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 1 {
		t.Fatalf("copy_to")
	}
}

func TestBulk(t *testing.T) {
	c := New()
	defer c.Close()
	res := mustDo(t, c, http.MethodPost, "/_bulk", `
{"index": {"_index": "b", "_id": "1"}}
{"n": 1}
{"create": {"_index": "b", "_id": "1"}}
{"n": 2}
{"update": {"_index": "b", "_id": "1"}}
{"doc": {"m": "x"}}
{"update": {"_index": "b", "_id": "9"}}
{"doc": {"m": "x"}, "doc_as_upsert": true}
{"delete": {"_index": "b", "_id": "9"}}
{"delete": {"_index": "b", "_id": "404"}}
{"index": {"_index": "b"}}
{"n": "bad"}
`)
	if res["errors"] != true {
		t.Fatalf("errors flag %v", res)
	}
	items := res["items"].([]any)
	get := func(i int, action string) map[string]any { return items[i].(map[string]any)[action].(map[string]any) }
	if get(0, "index")["status"].(float64) != 201 || get(0, "index")["result"] != "created" {
		t.Fatalf("item0 %v", items[0])
	}
	if get(1, "create")["status"].(float64) != 409 {
		t.Fatalf("item1 %v", items[1])
	}
	if get(2, "update")["status"].(float64) != 200 || get(2, "update")["_version"].(float64) != 2 {
		t.Fatalf("item2 %v", items[2])
	}
	if get(3, "update")["result"] != "created" {
		t.Fatalf("item3 %v", items[3])
	}
	if get(4, "delete")["result"] != "deleted" || get(5, "delete")["status"].(float64) != 404 {
		t.Fatalf("delete items %v %v", items[4], items[5])
	}
	if get(6, "index")["status"].(float64) != 400 || get(6, "index")["error"].(map[string]any)["type"] != "mapper_parsing_exception" {
		t.Fatalf("item6 %v", items[6])
	}
	// index-scoped bulk and Go helper error
	err := c.BulkString("{\"index\": {\"_index\": \"b\", \"_id\": \"2\"}}\n{\"n\": \"still bad\"}\n")
	be, ok := err.(*BulkError)
	if !ok || len(be.Items) != 1 || be.Items[0].ID != "2" {
		t.Fatalf("BulkError %v", err)
	}
	mustDo(t, c, http.MethodPost, "/b/_bulk", "{\"index\": {\"_id\": \"3\"}}\n{\"n\": 3}\n")
	n, _ := c.Count("b", nil)
	if n != 2 {
		t.Fatalf("count %d", n)
	}
}

func TestByQueryAndReindex(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	r := mustDo(t, c, http.MethodPost, "/products/_delete_by_query", `{"query": {"term": {"vendor.name": "acme"}}}`)
	if r["deleted"].(float64) != 2 || r["total"].(float64) != 2 {
		t.Fatalf("delete_by_query %v", r)
	}
	n, _ := c.Count("products", nil)
	if n != 3 {
		t.Fatalf("count %d", n)
	}
	r = mustDo(t, c, http.MethodPost, "/products/_update_by_query", `{"query": {"match_all": {}}}`)
	if r["updated"].(float64) != 3 {
		t.Fatalf("update_by_query %v", r)
	}
	r = mustDo(t, c, http.MethodGet, "/products/_doc/3", nil)
	if r["_version"].(float64) != 2 {
		t.Fatalf("version after update_by_query %v", r)
	}
	r = mustDo(t, c, http.MethodPost, "/_reindex", `{"source": {"index": "products", "query": {"term": {"active": true}}}, "dest": {"index": "copy"}}`)
	if r["created"].(float64) != 3 {
		t.Fatalf("reindex %v", r)
	}
	m := mustDo(t, c, http.MethodGet, "/copy/_mapping", nil)
	if m["copy"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)["price"].(map[string]any)["type"] != "float" {
		t.Fatalf("reindex dynamic mapping %v", m)
	}
	r = mustDo(t, c, http.MethodPost, "/_reindex", `{"source": {"index": "products"}, "dest": {"index": "copy", "op_type": "create"}, "conflicts": "proceed"}`)
	if r["version_conflicts"].(float64) != 3 {
		t.Fatalf("reindex conflicts %v", r)
	}
}

func TestIndexManagement(t *testing.T) {
	c := New()
	defer c.Close()
	st, body := status(t, c, http.MethodPut, "/BadName", `{}`)
	if st != 400 || errType(body) != "invalid_index_name_exception" {
		t.Fatalf("bad name %d %v", st, body)
	}
	mustDo(t, c, http.MethodPut, "/idx-1", `{"settings": {"number_of_shards": 3, "index.refresh_interval": "5s"}, "aliases": {"idx": {}}}`)
	st, body = status(t, c, http.MethodPut, "/idx-1", `{}`)
	if st != 400 || errType(body) != "resource_already_exists_exception" {
		t.Fatalf("exists %d %v", st, body)
	}
	st, _ = status(t, c, http.MethodHead, "/idx-1", nil)
	if st != 200 {
		t.Fatalf("head %d", st)
	}
	st, _ = status(t, c, http.MethodHead, "/idx", nil)
	if st != 200 {
		t.Fatalf("head alias %d", st)
	}
	st, _ = status(t, c, http.MethodHead, "/idx-2", nil)
	if st != 404 {
		t.Fatalf("head missing %d", st)
	}
	r := mustDo(t, c, http.MethodGet, "/idx-1", nil)
	settings := r["idx-1"].(map[string]any)["settings"].(map[string]any)["index"].(map[string]any)
	if settings["number_of_shards"] != "3" || settings["refresh_interval"] != "5s" || settings["uuid"] == nil || settings["provided_name"] != "idx-1" {
		t.Fatalf("settings %v", settings)
	}
	if r["idx-1"].(map[string]any)["aliases"].(map[string]any)["idx"] == nil {
		t.Fatalf("aliases %v", r)
	}
	r = mustDo(t, c, http.MethodGet, "/idx-1/_settings?flat_settings=true", nil)
	if r["idx-1"].(map[string]any)["settings"].(map[string]any)["index.refresh_interval"] != "5s" {
		t.Fatalf("flat settings %v", r)
	}
	mustDo(t, c, http.MethodPut, "/idx-1/_settings", `{"index": {"refresh_interval": "10s"}}`)
	st, _ = status(t, c, http.MethodPut, "/idx-1/_settings", `{"index": {"number_of_shards": 5}}`)
	if st != 400 {
		t.Fatalf("static setting %d", st)
	}
	// mapping updates
	mustDo(t, c, http.MethodPut, "/idx-1/_mapping", `{"properties": {"a": {"type": "keyword"}}}`)
	mustDo(t, c, http.MethodPut, "/idx-1/_mapping", `{"properties": {"a": {"type": "keyword"}, "b": {"type": "long"}}}`)
	st, body = status(t, c, http.MethodPut, "/idx-1/_mapping", `{"properties": {"a": {"type": "text"}}}`)
	if st != 400 || errType(body) != "illegal_argument_exception" {
		t.Fatalf("mapping conflict %d %v", st, body)
	}
	r = mustDo(t, c, http.MethodGet, "/idx-1/_mapping/field/a,b", nil)
	if r["idx-1"].(map[string]any)["mappings"].(map[string]any)["b"].(map[string]any)["mapping"].(map[string]any)["b"].(map[string]any)["type"] != "long" {
		t.Fatalf("field mapping %v", r)
	}
	// cat and stats
	mustDo(t, c, http.MethodPut, "/idx-1/_doc/1", `{"a": "x"}`)
	raw, _ := c.Do(http.MethodGet, "/_cat/indices?v", nil)
	if !strings.Contains(string(raw.Body), "docs.count") || !strings.Contains(string(raw.Body), "idx-1") {
		t.Fatalf("cat indices: %s", raw.Body)
	}
	raw, _ = c.Do(http.MethodGet, "/_cat/indices?format=json&h=index,docs.count", nil)
	var rows []map[string]any
	_ = json.Unmarshal(raw.Body, &rows)
	if len(rows) != 1 || rows[0]["docs.count"] != "1" {
		t.Fatalf("cat json: %s", raw.Body)
	}
	r = mustDo(t, c, http.MethodGet, "/idx-1/_stats", nil)
	if r["_all"].(map[string]any)["primaries"].(map[string]any)["docs"].(map[string]any)["count"].(float64) != 1 {
		t.Fatalf("stats %v", r)
	}
	mustDo(t, c, http.MethodPost, "/idx-1/_refresh", nil)
	mustDo(t, c, http.MethodPost, "/_refresh", nil)
	mustDo(t, c, http.MethodGet, "/_cluster/health", nil)
	r = mustDo(t, c, http.MethodGet, "/", nil)
	if r["version"].(map[string]any)["distribution"] != "opensearch" {
		t.Fatalf("info %v", r)
	}
	// delete with wildcard and multiple
	mustDo(t, c, http.MethodPut, "/idx-2", nil)
	mustDo(t, c, http.MethodPut, "/other", nil)
	mustDo(t, c, http.MethodDelete, "/idx-*", nil)
	if names := c.Indices(); len(names) != 1 || names[0] != "other" {
		t.Fatalf("indices %v", names)
	}
	st, _ = status(t, c, http.MethodDelete, "/gone", nil)
	if st != 404 {
		t.Fatalf("delete missing %d", st)
	}
	st, _ = status(t, c, http.MethodDelete, "/gone*", nil)
	if st != 200 {
		t.Fatalf("delete wildcard missing %d", st)
	}
	mustDo(t, c, http.MethodDelete, "/_all", nil)
	if len(c.Indices()) != 0 {
		t.Fatal("delete _all")
	}
}

func TestAliases(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/logs-1/_doc/1", `{"level": "info"}`)
	mustDo(t, c, http.MethodPut, "/logs-2/_doc/2", `{"level": "error"}`)
	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions": [
	  {"add": {"index": "logs-1", "alias": "logs"}},
	  {"add": {"index": "logs-2", "alias": "logs", "is_write_index": true}},
	  {"add": {"index": "logs-*", "alias": "errors", "filter": {"term": {"level.keyword": "error"}}}}
	]}`)
	n, _ := c.Count("logs", nil)
	if n != 2 {
		t.Fatalf("alias count %d", n)
	}
	n, _ = c.Count("errors", nil)
	if n != 1 {
		t.Fatalf("filtered alias count %d", n)
	}
	r := mustDo(t, c, http.MethodPost, "/logs/_doc", `{"level": "warn"}`)
	if r["_index"] != "logs-2" {
		t.Fatalf("write index %v", r)
	}
	r = mustDo(t, c, http.MethodGet, "/_alias/logs", nil)
	if len(r) != 2 {
		t.Fatalf("get alias %v", r)
	}
	r = mustDo(t, c, http.MethodGet, "/logs-1/_alias", nil)
	if len(r["logs-1"].(map[string]any)["aliases"].(map[string]any)) != 2 {
		t.Fatalf("index aliases %v", r)
	}
	st, _ := status(t, c, http.MethodHead, "/_alias/logs", nil)
	if st != 200 {
		t.Fatalf("head alias %d", st)
	}
	st, _ = status(t, c, http.MethodHead, "/_alias/nope", nil)
	if st != 404 {
		t.Fatalf("head missing alias %d", st)
	}
	mustDo(t, c, http.MethodDelete, "/logs-1/_alias/logs", nil)
	n, _ = c.Count("logs", nil)
	if n != 2 {
		t.Fatalf("after remove %d", n)
	}
	st, body := status(t, c, http.MethodDelete, "/logs-1/_alias/logs", nil)
	if st != 404 || errType(body) != "aliases_not_found_exception" {
		t.Fatalf("remove missing %d %v", st, body)
	}
	mustDo(t, c, http.MethodPut, "/logs-1/_alias/current", nil)
	raw, _ := c.Do(http.MethodGet, "/_cat/aliases?format=json", nil)
	if !strings.Contains(string(raw.Body), `"alias":"current"`) {
		t.Fatalf("cat aliases %s", raw.Body)
	}
	st, _ = status(t, c, http.MethodPut, "/current", nil)
	if st != 400 {
		t.Fatalf("index named like alias %d", st)
	}
	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions": [{"remove_index": {"index": "logs-1"}}]}`)
	if len(c.Indices()) != 1 {
		t.Fatalf("remove_index %v", c.Indices())
	}
}

func TestTemplates(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/_index_template/logs", `{"index_patterns": ["logs-*"], "priority": 10, "template": {"settings": {"number_of_shards": 2}, "mappings": {"properties": {"ts": {"type": "date"}, "msg": {"type": "keyword"}}}, "aliases": {"logs": {}}}}`)
	mustDo(t, c, http.MethodPut, "/_index_template/logs-high", `{"index_patterns": ["logs-prod-*"], "priority": 20, "template": {"mappings": {"properties": {"msg": {"type": "text"}}}}}`)
	mustDo(t, c, http.MethodPut, "/_template/legacy", `{"index_patterns": ["legacy-*"], "settings": {"index": {"number_of_replicas": 2}}, "mappings": {"properties": {"x": {"type": "integer"}}}}`)
	mustDo(t, c, http.MethodPut, "/logs-2024/_doc/1", `{"ts": "2024-01-01", "msg": "hello"}`)
	r := mustDo(t, c, http.MethodGet, "/logs-2024", nil)
	ix := r["logs-2024"].(map[string]any)
	if ix["settings"].(map[string]any)["index"].(map[string]any)["number_of_shards"] != "2" {
		t.Fatalf("template settings %v", ix)
	}
	if ix["mappings"].(map[string]any)["properties"].(map[string]any)["msg"].(map[string]any)["type"] != "keyword" {
		t.Fatalf("template mapping %v", ix)
	}
	if ix["aliases"].(map[string]any)["logs"] == nil {
		t.Fatalf("template alias %v", ix)
	}
	mustDo(t, c, http.MethodPut, "/logs-prod-1", `{"mappings": {"properties": {"extra": {"type": "boolean"}}}}`)
	r = mustDo(t, c, http.MethodGet, "/logs-prod-1/_mapping", nil)
	props := r["logs-prod-1"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)
	if props["msg"].(map[string]any)["type"] != "text" || props["extra"] == nil || props["ts"] != nil {
		t.Fatalf("priority template %v", props)
	}
	mustDo(t, c, http.MethodPut, "/legacy-1", nil)
	r = mustDo(t, c, http.MethodGet, "/legacy-1", nil)
	if r["legacy-1"].(map[string]any)["settings"].(map[string]any)["index"].(map[string]any)["number_of_replicas"] != "2" {
		t.Fatalf("legacy template %v", r)
	}
	r = mustDo(t, c, http.MethodGet, "/_index_template/logs*", nil)
	if len(r["index_templates"].([]any)) != 2 {
		t.Fatalf("get templates %v", r)
	}
	st, _ := status(t, c, http.MethodHead, "/_index_template/logs", nil)
	if st != 200 {
		t.Fatalf("head template %d", st)
	}
	mustDo(t, c, http.MethodDelete, "/_index_template/logs", nil)
	st, _ = status(t, c, http.MethodGet, "/_index_template/logs", nil)
	if st != 404 {
		t.Fatalf("deleted template %d", st)
	}
	mustDo(t, c, http.MethodDelete, "/_template/legacy", nil)
	st, _ = status(t, c, http.MethodHead, "/_template/legacy", nil)
	if st != 404 {
		t.Fatalf("deleted legacy %d", st)
	}
}

func TestScrollAndPIT(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	r := mustDo(t, c, http.MethodPost, "/products/_search?scroll=1m", `{"size": 2, "sort": ["_doc"]}`)
	id := r["_scroll_id"].(string)
	var ids []string
	collect := func(r map[string]any) {
		for _, h := range r["hits"].(map[string]any)["hits"].([]any) {
			ids = append(ids, h.(map[string]any)["_id"].(string))
		}
	}
	collect(r)
	for i := 0; i < 3; i++ {
		r = mustDo(t, c, http.MethodPost, "/_search/scroll", `{"scroll": "1m", "scroll_id": "`+id+`"}`)
		collect(r)
	}
	assertIDs(t, ids, "1", "2", "3", "4", "5")
	r = mustDo(t, c, http.MethodDelete, "/_search/scroll", `{"scroll_id": ["`+id+`"]}`)
	if r["num_freed"].(float64) != 1 {
		t.Fatalf("clear scroll %v", r)
	}
	st, body := status(t, c, http.MethodPost, "/_search/scroll", `{"scroll_id": "`+id+`"}`)
	if st != 404 || errType(body) != "search_phase_execution_exception" {
		t.Fatalf("expired scroll %d %v", st, body)
	}
	r = mustDo(t, c, http.MethodPost, "/products/_search/point_in_time?keep_alive=1m", nil)
	pit := r["pit_id"].(string)
	r = mustDo(t, c, http.MethodPost, "/_search", `{"pit": {"id": "`+pit+`"}, "size": 10}`)
	if r["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 5 || r["pit_id"] != pit {
		t.Fatalf("pit search %v", r)
	}
	r = mustDo(t, c, http.MethodDelete, "/_search/point_in_time", `{"pit_id": ["`+pit+`"]}`)
	if r["pits"].([]any)[0].(map[string]any)["successful"] != true {
		t.Fatalf("delete pit %v", r)
	}
}

func TestMultiSearchAndCount(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	r := mustDo(t, c, http.MethodPost, "/_msearch", "{\"index\": \"products\"}\n{\"query\": {\"term\": {\"tags\": \"red\"}}}\n{}\n{\"query\": {\"match_all\": {}}, \"size\": 0}\n{\"index\": \"missing\"}\n{}\n")
	resp := r["responses"].([]any)
	if resp[0].(map[string]any)["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 1 {
		t.Fatalf("msearch 0 %v", resp[0])
	}
	if resp[1].(map[string]any)["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 5 {
		t.Fatalf("msearch 1 %v", resp[1])
	}
	if resp[2].(map[string]any)["status"].(float64) != 404 {
		t.Fatalf("msearch 2 %v", resp[2])
	}
	r = mustDo(t, c, http.MethodPost, "/products/_count", `{"query": {"term": {"active": true}}}`)
	if r["count"].(float64) != 4 {
		t.Fatalf("count %v", r)
	}
	r = mustDo(t, c, http.MethodGet, "/_count", nil)
	if r["count"].(float64) != 5 {
		t.Fatalf("count all %v", r)
	}
	r = mustDo(t, c, http.MethodGet, "/products/_analyze", `{"analyzer": "standard", "text": "Hello, World"}`)
	toks := r["tokens"].([]any)
	if len(toks) != 2 || toks[0].(map[string]any)["token"] != "hello" || toks[1].(map[string]any)["start_offset"].(float64) != 7 {
		t.Fatalf("analyze %v", toks)
	}
	r = mustDo(t, c, http.MethodGet, "/products/_analyze", `{"field": "sku", "text": "ABC"}`)
	if r["tokens"].([]any)[0].(map[string]any)["token"] != "ABC" {
		t.Fatalf("analyze field %v", r)
	}
}

func TestCloneSemantics(t *testing.T) {
	base := seedCluster(t)
	defer base.Close()
	a := base.Clone()
	b := base.Clone()
	defer a.Close()
	defer b.Close()
	mustDo(t, a, http.MethodPut, "/products/_doc/100", `{"name": "A only", "newfield": 1}`)
	mustDo(t, b, http.MethodDelete, "/products/_doc/1", nil)
	mustDo(t, b, http.MethodPut, "/extra", nil)
	mustDo(t, base, http.MethodPut, "/products/_doc/200", `{"name": "base only"}`)
	counts := func(c *Cluster) int { n, _ := c.Count("products", nil); return n }
	if counts(base) != 6 || counts(a) != 6 || counts(b) != 4 {
		t.Fatalf("counts base=%d a=%d b=%d", counts(base), counts(a), counts(b))
	}
	if len(base.Indices()) != 1 || len(b.Indices()) != 2 {
		t.Fatalf("indices")
	}
	// mapping changes are isolated too
	m := mustDo(t, base, http.MethodGet, "/products/_mapping", nil)
	if m["products"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)["newfield"] != nil {
		t.Fatalf("mapping leaked into base")
	}
	m = mustDo(t, a, http.MethodGet, "/products/_mapping", nil)
	if m["products"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)["newfield"] == nil {
		t.Fatalf("mapping missing in clone")
	}
	// searches on clones see their own state
	ids, _ := search(t, a, `{"query": {"match": {"name": "only"}}}`)
	assertIDs(t, ids, "100")
	// clone of a clone; closing the middle one keeps the leaf working
	leaf := a.Clone()
	a.Close()
	if counts(leaf) != 6 {
		t.Fatalf("leaf count %d", counts(leaf))
	}
	mustDo(t, leaf, http.MethodPut, "/products/_doc/300", `{"name": "leaf"}`)
	if counts(leaf) != 7 {
		t.Fatalf("leaf write %d", counts(leaf))
	}
	leaf.Close()
	// aliases are per clone
	x := base.Clone()
	defer x.Close()
	mustDo(t, x, http.MethodPut, "/products/_alias/p", nil)
	if st, _ := status(t, base, http.MethodHead, "/p", nil); st != 404 {
		t.Fatalf("alias leaked")
	}
}

func TestServeWithHTTPClient(t *testing.T) {
	c := seedCluster(t)
	defer c.Close()
	srv, err := c.Serve()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/products/_search", "application/json", strings.NewReader(`{"query": {"term": {"tags": "red"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["hits"].(map[string]any)["total"].(map[string]any)["value"].(float64) != 1 {
		t.Fatalf("http search %v", out)
	}
	if resp.Header.Get("Content-Type") != "application/json; charset=UTF-8" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	head, _ := http.Head(srv.URL + "/products")
	if head.StatusCode != 200 {
		t.Fatalf("head %d", head.StatusCode)
	}
}

func TestGoHelpers(t *testing.T) {
	c := New()
	defer c.Close()
	if err := c.CreateIndex("things", map[string]any{"mappings": map[string]any{"properties": map[string]any{"n": map[string]any{"type": "integer"}}}}); err != nil {
		t.Fatal(err)
	}
	type thing struct {
		N    int    `json:"n"`
		Name string `json:"name"`
	}
	if err := c.Index("things", "1", thing{N: 1, Name: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Index("things", "", thing{N: 2, Name: "two"}); err != nil {
		t.Fatal(err)
	}
	var got thing
	found, err := c.Get("things", "1", &got)
	if err != nil || !found || got.Name != "one" {
		t.Fatalf("get %v %v %v", found, err, got)
	}
	found, _ = c.Get("things", "nope", nil)
	if found {
		t.Fatal("found missing")
	}
	var res struct {
		Hits struct {
			Hits []struct {
				ID     string `json:"_id"`
				Source thing  `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := c.Search("things", map[string]any{"query": map[string]any{"range": map[string]any{"n": map[string]any{"gte": 2}}}}, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Hits.Hits) != 1 || res.Hits.Hits[0].Source.Name != "two" {
		t.Fatalf("search %v", res)
	}
	if err := c.CreateIndex("things", nil); err == nil || !strings.Contains(err.Error(), "resource_already_exists_exception") {
		t.Fatalf("expected error, got %v", err)
	}
	if err := c.DeleteIndex("things"); err != nil {
		t.Fatal(err)
	}
}
