package osmem

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// Expectations below were taken from OpenSearch 3.8.0 responses.

func analyzeTokens(t *testing.T, c *Cluster, path, body string) []map[string]any {
	t.Helper()
	r := mustDo(t, c, http.MethodPost, path, body)
	var out []map[string]any
	for _, tok := range r["tokens"].([]any) {
		out = append(out, tok.(map[string]any))
	}
	return out
}

func tokenSummary(toks []map[string]any) string {
	var parts []string
	for _, tok := range toks {
		parts = append(parts, tok["token"].(string)+"|"+tok["type"].(string)+"|"+
			itoa(tok["start_offset"])+"-"+itoa(tok["end_offset"])+"|"+itoa(tok["position"]))
	}
	return strings.Join(parts, " ")
}

func itoa(v any) string {
	f, _ := v.(float64)
	return strconv.Itoa(int(f))
}

func TestAnalyzeTokenTypesAndUTF16Offsets(t *testing.T) {
	c := New()
	defer c.Close()
	got := tokenSummary(analyzeTokens(t, c, "/_analyze", `{"analyzer": "standard", "text": "Café 2 日本 😀 3.14"}`))
	want := "café|<ALPHANUM>|0-4|0 2|<NUM>|5-6|1 日|<IDEOGRAPHIC>|7-8|2 本|<IDEOGRAPHIC>|8-9|3 😀|<EMOJI>|10-12|4 3.14|<NUM>|13-17|5"
	if got != want {
		t.Fatalf("standard analyzer:\n got %s\nwant %s", got, want)
	}
	got = tokenSummary(analyzeTokens(t, c, "/_analyze", `{"tokenizer": "standard", "filter": ["uppercase"], "text": "The fox"}`))
	if got != "THE|<ALPHANUM>|0-3|0 FOX|<ALPHANUM>|4-7|1" {
		t.Fatalf("uppercase filter: %s", got)
	}
	got = tokenSummary(analyzeTokens(t, c, "/_analyze", `{"analyzer": "whitespace", "text": ["a  b", "c"]}`))
	if got != "a|word|0-1|0 b|word|3-4|1 c|word|5-6|2" {
		t.Fatalf("whitespace analyzer array: %s", got)
	}
	got = tokenSummary(analyzeTokens(t, c, "/_analyze", `{"tokenizer": "whitespace", "text": ["a b", "c"]}`))
	if got != "a|word|0-1|0 b|word|2-3|1 c|word|4-5|102" {
		t.Fatalf("custom chain position gap: %s", got)
	}
	got = tokenSummary(analyzeTokens(t, c, "/_analyze", `{"analyzer": "stop", "text": "The quick and the dead"}`))
	if got != "quick|word|4-9|1 dead|word|18-22|4" {
		t.Fatalf("stop analyzer: %s", got)
	}
	got = tokenSummary(analyzeTokens(t, c, "/_analyze", `{"tokenizer": "standard", "char_filter": ["html_strip"], "text": "<p>Hello <b>World</b></p>"}`))
	if got != "Hello|<ALPHANUM>|3-8|0 World|<ALPHANUM>|12-21|1" {
		t.Fatalf("html_strip offsets: %s", got)
	}
	got = tokenSummary(analyzeTokens(t, c, "/_analyze", `{"tokenizer": "standard", "filter": [{"type": "synonym_graph", "synonyms": ["ny, new york"]}], "text": "ny city"}`))
	if got != "new|SYNONYM|0-2|0 ny|<ALPHANUM>|0-2|0 york|SYNONYM|0-2|1 city|<ALPHANUM>|3-7|2" {
		t.Fatalf("synonym_graph: %s", got)
	}
}

func TestAnalyzeRequestErrors(t *testing.T) {
	c := New()
	defer c.Close()
	cases := []struct {
		path, body string
		status     int
		typ        string
		reason     string
	}{
		{"/_analyze", `{"tokenizer": "nope", "text": "x"}`, 400, "illegal_argument_exception", "failed to find global tokenizer under [nope]"},
		{"/_analyze", `{"tokenizer": "standard", "filter": ["nope"], "text": "x"}`, 400, "illegal_argument_exception", "failed to find global filter under [nope]"},
		{"/_analyze", `{"analyzer": "nope", "text": "x"}`, 400, "illegal_argument_exception", "failed to find global analyzer [nope]"},
		{"/_analyze", `{"analyzer": "standard"}`, 400, "action_request_validation_exception", "Validation Failed: 1: text is missing;"},
		{"/_analyze", `{"analyzer": "standard", "tokenizer": "standard", "text": "x"}`, 400, "action_request_validation_exception", "Validation Failed: 1: cannot define extra components on a named analyzer;"},
		{"/_analyze", `{"normalizer": "lowercase", "tokenizer": "standard", "text": "x"}`, 400, "action_request_validation_exception", "Validation Failed: 1: index is required if normalizer is specified;2: tokenizer/analyze should be null if normalizer is specified;3: cannot define extra components on a named normalizer;"},
		{"/_analyze", `{"field": "title", "text": "x"}`, 400, "illegal_argument_exception", "analysis based on a specific field requires an index"},
		{"/_analyze", `{"analyzer": "standard", "text": "x", "foo": 1}`, 400, "x_content_parse_exception", "[1:39] [analyze_request] unknown field [foo]"},
		{"/_analyze", `{"analyzer": "standard", "text": 5}`, 400, "x_content_parse_exception", "[1:34] [analyze_request] text doesn't support values of type: VALUE_NUMBER"},
		{"/_analyze", `{"filter": ["porter_stem"], "text": "running"}`, 400, "illegal_argument_exception", "Custom normalizer may not use filter [porter_stem]"},
		{"/_analyze", `{"tokenizer": {"type": "ngram", "min_gram": 1, "max_gram": 5}, "text": "abc"}`, 400, "illegal_argument_exception", "The difference between max_gram and min_gram in NGram Tokenizer must be less than or equal to: [1] but was [4]. This limit can be set by changing the [index.max_ngram_diff] index level setting."},
		{"/_analyze", `{"tokenizer": {"type": "pattern", "pattern": "("}, "text": "x"}`, 400, "pattern_syntax_exception", "Unclosed group near index 1\n("},
		{"/missing-index/_analyze", `{"analyzer": "standard", "text": "x"}`, 404, "index_not_found_exception", "no such index [missing-index]"},
		{"/_analyze", ``, 400, "parse_exception", "request body or source parameter is required"},
	}
	for _, tc := range cases {
		var body any
		if tc.body != "" {
			body = tc.body
		}
		code, r := status(t, c, http.MethodPost, tc.path, body)
		e, _ := r["error"].(map[string]any)
		if code != tc.status || errType(r) != tc.typ || e["reason"] != tc.reason {
			t.Errorf("%s %s: got %d %s %v", tc.path, tc.body, code, errType(r), e["reason"])
		}
	}
	_, r := status(t, c, http.MethodPost, "/missing-index/_analyze", `{"analyzer": "standard", "text": "x"}`)
	if e := r["error"].(map[string]any); e["resource.type"] != "index_expression" {
		t.Errorf("missing index resource.type: %v", e)
	}
}

func TestAnalyzeIndexAndFields(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/az", `{"settings": {"analysis": {
	  "analyzer": {"my": {"type": "custom", "tokenizer": "whitespace", "filter": ["lowercase", "my_stop"]}},
	  "filter": {"my_stop": {"type": "stop", "stopwords": ["the"]}},
	  "normalizer": {"lc": {"type": "custom", "filter": ["lowercase", "asciifolding"]}}}},
	  "mappings": {"properties": {"title": {"type": "text", "analyzer": "my"}, "k": {"type": "keyword", "normalizer": "lc"}, "n": {"type": "long"}, "std": {"type": "text"}}}}`)
	if got := tokenSummary(analyzeTokens(t, c, "/az/_analyze", `{"field": "title", "text": "The QUICK Fox"}`)); got != "quick|word|4-9|1 fox|word|10-13|2" {
		t.Fatalf("field analyzer: %s", got)
	}
	if got := tokenSummary(analyzeTokens(t, c, "/_analyze?index=az", `{"field": "title", "text": "The QUICK Fox"}`)); got != "quick|word|4-9|1 fox|word|10-13|2" {
		t.Fatalf("index URL parameter: %s", got)
	}
	if got := tokenSummary(analyzeTokens(t, c, "/az/_analyze", `{"field": "k", "text": "Café ÉCOLE"}`)); got != "cafe ecole|word|0-10|0" {
		t.Fatalf("keyword normalizer: %s", got)
	}
	if got := tokenSummary(analyzeTokens(t, c, "/az/_analyze", `{"field": "std", "text": ["a b", "c"]}`)); got != "a|<ALPHANUM>|0-1|0 b|<ALPHANUM>|2-3|1 c|<ALPHANUM>|4-5|102" {
		t.Fatalf("text field position gap: %s", got)
	}
	code, r := status(t, c, http.MethodPost, "/az/_analyze", `{"field": "n", "text": "123"}`)
	if code != 400 || r["error"].(map[string]any)["reason"] != "Can't process field [n], Analysis requests are only supported on tokenized fields" {
		t.Fatalf("long field: %d %v", code, r)
	}
	code, r = status(t, c, http.MethodPost, "/az/_analyze", `{"normalizer": "nope", "text": "x"}`)
	if code != 400 || r["error"].(map[string]any)["reason"] != "failed to find normalizer under [nope]" {
		t.Fatalf("missing normalizer: %d %v", code, r)
	}
	code, r = status(t, c, http.MethodPost, "/az/_analyze", `{"analyzer": "nope", "text": "x"}`)
	if code != 400 || r["error"].(map[string]any)["reason"] != "failed to find analyzer [nope]" {
		t.Fatalf("missing analyzer: %d %v", code, r)
	}
	r = mustDo(t, c, http.MethodPost, "/az/_analyze", `{"field": "k", "text": "Café", "explain": true}`)
	detail := r["detail"].(map[string]any)
	if detail["custom_analyzer"] != true || detail["tokenizer"].(map[string]any)["name"] != "keyword" {
		t.Fatalf("explain detail: %v", detail)
	}
	filters := detail["tokenfilters"].([]any)
	last := filters[len(filters)-1].(map[string]any)["tokens"].([]any)[0].(map[string]any)
	if last["token"] != "cafe" || last["bytes"] != "[63 61 66 65]" || last["termFrequency"] != float64(1) {
		t.Fatalf("explain tokens: %v", last)
	}
}

func TestIndexCreationAnalysisErrors(t *testing.T) {
	c := New()
	defer c.Close()
	cases := []struct{ body, reason string }{
		{`{"settings": {"analysis": {"analyzer": {"bad": {"type": "custom", "tokenizer": "nosuchtokenizer"}}}}}`, "Failed to build analyzers: [bad]"},
		{`{"settings": {"analysis": {"analyzer": {"bad": {"type": "custom", "tokenizer": "standard", "filter": ["nosuchfilter"]}}}}}`, "Failed to build analyzers: [bad]"},
		{`{"settings": {"analysis": {"filter": {"f": {"type": "nosuchtype"}}}}}`, "Unknown filter type [nosuchtype] for [f]"},
		{`{"settings": {"analysis": {"tokenizer": {"t": {"type": "nosuchtype"}}}}}`, "Unknown tokenizer type [nosuchtype] for [t]"},
		{`{"settings": {"analysis": {"char_filter": {"c": {"type": "nosuchtype"}}}}}`, "Unknown char_filter type [nosuchtype] for [c]"},
		{`{"settings": {"analysis": {"analyzer": {"a": {"type": "nosuchtype"}}}}}`, "Unknown analyzer type [nosuchtype] for [a]"},
		{`{"settings": {"analysis": {"normalizer": {"n": {"type": "custom", "filter": ["nosuchfilter"]}}}}}`, "Custom Analyzer [n] failed to find filter under name [nosuchfilter]"},
		{`{"settings": {"analysis": {"normalizer": {"n": {"type": "custom", "filter": ["porter_stem"]}}}}}`, "Custom normalizer [n] may not use filter [porter_stem]"},
	}
	for i, tc := range cases {
		code, r := status(t, c, http.MethodPut, "/bad-analysis-"+string(rune('a'+i)), tc.body)
		if code != 400 || errType(r) != "illegal_argument_exception" || r["error"].(map[string]any)["reason"] != tc.reason {
			t.Errorf("%s: got %d %v", tc.body, code, r)
		}
	}
}
