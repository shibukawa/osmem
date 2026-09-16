package ja_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/shibukawa/osmem"
	_ "github.com/shibukawa/osmem/ja"
)

func do(t *testing.T, c *osmem.Cluster, method, path string, body any) map[string]any {
	t.Helper()
	res, err := c.Do(method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError() {
		t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, res.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(res.Body, &out)
	return out
}

func tokens(t *testing.T, c *osmem.Cluster, index, body string) []string {
	t.Helper()
	res := do(t, c, http.MethodGet, "/"+index+"/_analyze", body)
	var out []string
	for _, tok := range res["tokens"].([]any) {
		out = append(out, tok.(map[string]any)["token"].(string))
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestKuromojiAnalyzer(t *testing.T) {
	c := osmem.New()
	defer c.Close()
	do(t, c, http.MethodPut, "/docs", `{
	  "settings": {"analysis": {
	    "tokenizer": {"ja_user": {"type": "kuromoji_tokenizer", "mode": "normal", "user_dictionary_rules": ["東京スカイツリー,東京スカイツリー,トウキョウスカイツリー,カスタム名詞"]}},
	    "filter": {"pos_all": {"type": "kuromoji_part_of_speech", "stoptags": ["助詞", "助動詞", "記号"]}, "romaji": {"type": "kuromoji_readingform", "use_romaji": true}, "edge_ngram_2": {"type": "edge_ngram", "min_gram": 2, "max_gram": 2}},
	    "analyzer": {
	      "ja_custom": {"type": "custom", "tokenizer": "ja_user", "filter": ["kuromoji_baseform", "pos_all", "lowercase"]},
	      "ja_reading": {"type": "custom", "tokenizer": "kuromoji_tokenizer", "filter": ["romaji"]},
	      "ja_ngram": {"type": "custom", "tokenizer": "kuromoji_tokenizer", "filter": ["kuromoji_baseform", "edge_ngram_2"]}
	    }
	  }},
	  "mappings": {"properties": {
	    "body": {"type": "text", "analyzer": "kuromoji"},
	    "custom": {"type": "text", "analyzer": "ja_custom"},
	    "reading": {"type": "text", "analyzer": "ja_reading"}
	  }}
	}`)
	if w := c.Warnings("docs"); len(w) != 0 {
		t.Fatalf("expected no warnings, got %v", w)
	}
	// an analyzer referring to an undefined filter fails index creation, as in OpenSearch
	badRes, err := c.Do(http.MethodPut, "/bad", `{"settings": {"analysis": {"analyzer": {"ja_bad": {"type": "custom", "tokenizer": "kuromoji_tokenizer", "filter": ["undefined_filter"]}}}}}`)
	if err != nil || badRes.StatusCode != http.StatusBadRequest || !strings.Contains(string(badRes.Body), "Failed to build analyzers: [ja_bad]") {
		t.Fatalf("undefined filter: %v %d %s", err, badRes.StatusCode, badRes.Body)
	}
	got := tokens(t, c, "docs", `{"analyzer": "kuromoji", "text": "東京スカイツリーに行きました。"}`)
	want := []string{"東京", "スカイ", "ツリー", "行く"}
	if !equal(got, want) {
		t.Fatalf("kuromoji: %v", got)
	}
	got = tokens(t, c, "docs", `{"analyzer": "ja_custom", "text": "東京スカイツリーに行きました。"}`)
	if !equal(got, []string{"東京スカイツリー", "行く"}) {
		t.Fatalf("user dictionary + normal mode: %v", got)
	}
	got = tokens(t, c, "docs", `{"analyzer": "ja_reading", "text": "寿司を食べる"}`)
	if !equal(got, []string{"sushi", "o", "taberu"}) {
		t.Fatalf("romaji reading: %v", got)
	}
	got = tokens(t, c, "docs", `{"analyzer": "kuromoji", "text": "サーバー コンピューター"}`)
	if !equal(got, []string{"サーバ", "コンピュータ"}) {
		t.Fatalf("stemmer: %v", got)
	}
	got = tokens(t, c, "docs", `{"analyzer": "kuromoji", "text": "ＡＢＣ ﾃｽﾄ"}`)
	if !equal(got, []string{"abc", "テスト"}) {
		t.Fatalf("cjk_width + lowercase: %v", got)
	}
	got = tokens(t, c, "docs", `{"tokenizer": "kuromoji_tokenizer", "text": "関西国際空港"}`)
	if !equal(got, []string{"関西", "国際", "空港"}) {
		t.Fatalf("search mode: %v", got)
	}
	// offsets are UTF-16 offsets into the original text, as in OpenSearch
	res := do(t, c, http.MethodGet, "/docs/_analyze", `{"analyzer": "kuromoji", "text": "私は東京に住む"}`)
	first := res["tokens"].([]any)[0].(map[string]any)
	if first["token"] != "私" || first["start_offset"].(float64) != 0 || first["end_offset"].(float64) != 1 {
		t.Fatalf("offsets: %v", first)
	}

	// indexing and searching
	if err := c.BulkString(`{"index": {"_index": "docs", "_id": "1"}}
{"body": "東京スカイツリーに行きました。", "custom": "東京スカイツリーに行きました。"}
{"index": {"_index": "docs", "_id": "2"}}
{"body": "大阪で美味しいたこ焼きを食べた", "custom": "大阪で美味しいたこ焼きを食べた"}
{"index": {"_index": "docs", "_id": "3"}}
{"body": "サーバーの設定について", "custom": "サーバーの設定について"}
`); err != nil {
		t.Fatal(err)
	}
	cases := map[string]int{
		`{"match": {"body": "スカイツリー"}}`:                              1,
		`{"match": {"body": "行く"}}`:                                  1,
		`{"match": {"body": "食べる"}}`:                                 1,
		`{"match": {"body": "サーバ"}}`:                                 1,
		`{"match": {"body": "東京 大阪"}}`:                               2,
		`{"match": {"body": {"query": "東京 大阪", "operator": "and"}}}`: 0,
		`{"match_phrase": {"body": "東京スカイツリー"}}`:                     1,
		`{"match": {"custom": "東京スカイツリー"}}`:                          1,
		`{"match": {"custom": "スカイツリー"}}`:                            0,
		`{"match": {"body": "の"}}`:                                   0,
	}
	for q, want := range cases {
		n, err := c.Count("docs", `{"query": `+q+`}`)
		if err != nil || n != want {
			t.Errorf("%s: got %d want %d (%v)", q, n, want, err)
		}
	}
	hl := do(t, c, http.MethodPost, "/docs/_search", `{"query": {"match": {"body": "たこ焼き"}}, "highlight": {"fields": {"body": {}}}}`)
	frag := hl["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)["highlight"].(map[string]any)["body"].([]any)[0]
	if frag != "大阪で美味しい<em>たこ焼き</em>を食べた" {
		t.Fatalf("highlight: %v", frag)
	}
}

// user_dictionary names a file on the server; osmem never opens files named
// by a request, so the setting is rejected (user_dictionary_rules works).
func TestUserDictionaryPathRejected(t *testing.T) {
	c := osmem.New()
	defer c.Close()
	err := c.CreateIndex("t", `{"settings": {"analysis": {
	    "tokenizer": {"ja_user": {"type": "kuromoji_tokenizer", "user_dictionary": "/etc/passwd"}},
	    "analyzer": {"ja": {"type": "custom", "tokenizer": "ja_user"}}}},
	  "mappings": {"properties": {"body": {"type": "text", "analyzer": "ja"}}}}`)
	if err == nil {
		t.Fatal("expected user_dictionary to be rejected")
	}
	if !strings.Contains(err.Error(), "user_dictionary_rules") {
		t.Fatalf("unexpected error: %v", err)
	}
}
