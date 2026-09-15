package osmem

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// The expectations below were copied from OpenSearch 3.8.0 responses (2.19.1
// returns the same) for the same mapping, documents and requests.

const highlightLong = "Alpha beta gamma. The fox ran into the forest. Delta epsilon zeta eta theta iota kappa lambda mu nu xi " +
	"omicron pi rho sigma tau upsilon phi chi psi omega. Another fox appeared near the river bank at dawn. " +
	"Numbers one two three four five six seven eight nine ten. Finally the last fox slept."

func highlightCluster(t *testing.T) *Cluster {
	t.Helper()
	c := New()
	mustDo(t, c, http.MethodPut, "/hl", `{"mappings":{"properties":{
		"title":{"type":"text","fields":{"keyword":{"type":"keyword"}}},
		"body":{"type":"text"},"long":{"type":"text"},"multi":{"type":"text"},"html":{"type":"text"},
		"tags":{"type":"keyword"},"tv":{"type":"text","term_vector":"with_positions_offsets"},
		"group":{"type":"keyword"},"comments":{"type":"nested","properties":{"text":{"type":"text"}}}}}}`)
	body1 := "The quick brown fox jumps over the lazy dog. The dog sleeps."
	docs := map[string]map[string]any{
		"1": {"title": "The Quick Brown Fox", "body": body1, "long": highlightLong, "multi": []string{"red fox", "blue whale", "green fox"},
			"html": "<b>fox</b> & friends", "tags": []string{"fox", "animal"}, "tv": body1, "group": "g1",
			"comments": []map[string]string{{"text": "nice fox"}, {"text": "bad dog"}, {"text": "fox and dog"}}},
		"2": {"title": "Lazy Dogs", "body": "Dogs are lazy. Foxes are quick.", "tags": []string{"dog"}, "group": "g1"},
		"3": {"title": "Fox fox FOX", "body": "fox", "multi": "fox", "group": "g2"},
	}
	for id, doc := range docs {
		raw, _ := json.Marshal(doc)
		mustDo(t, c, http.MethodPut, "/hl/_doc/"+id+"?refresh=true", string(raw))
	}
	return c
}

// summarizeHighlights reduces a search response to the ids, highlights and
// inner hit highlights of its hits.
func summarizeHighlights(res map[string]any) []any {
	out := []any{}
	hits, _ := res["hits"].(map[string]any)
	list, _ := hits["hits"].([]any)
	for _, h := range list {
		hm := h.(map[string]any)
		e := map[string]any{"_id": hm["_id"]}
		if hl, ok := hm["highlight"]; ok {
			e["hl"] = hl
		}
		if ih, ok := hm["inner_hits"].(map[string]any); ok {
			inner := map[string]any{}
			for name, v := range ih {
				var sub []any
				for _, x := range v.(map[string]any)["hits"].(map[string]any)["hits"].([]any) {
					xm := x.(map[string]any)
					se := map[string]any{"_id": xm["_id"]}
					if hl, ok := xm["highlight"]; ok {
						se["hl"] = hl
					}
					sub = append(sub, se)
				}
				inner[name] = sub
			}
			e["inner"] = inner
		}
		out = append(out, e)
	}
	return out
}

func TestHighlightCompatibility(t *testing.T) {
	c := highlightCluster(t)
	defer c.Close()
	cases := []struct{ name, body, want string }{
		{"passage is the matching sentence",
			`{"query":{"match":{"body":"foxes"}},"highlight":{"fields":{"body":{}}}}`,
			`[{"_id":"2","hl":{"body":["<em>Foxes</em> are quick."]}}]`},
		{"fields given as an array",
			`{"query":{"match":{"body":"foxes"}},"highlight":{"fields":[{"body":{}}]}}`,
			`[{"_id":"2","hl":{"body":["<em>Foxes</em> are quick."]}}]`},
		{"sentence passages",
			`{"query":{"match":{"long":"fox"}},"highlight":{"fields":{"long":{}}}}`,
			`[{"_id":"1","hl":{"long":["The <em>fox</em> ran into the forest.","Another <em>fox</em> appeared near the river bank at dawn.","Finally the last <em>fox</em> slept."]}}]`},
		{"fragment_size bounds long sentences at words",
			`{"query":{"match":{"long":"fox"}},"highlight":{"fields":{"long":{"fragment_size":30,"number_of_fragments":20}}}}`,
			`[{"_id":"1","hl":{"long":["The <em>fox</em> ran into the forest.","Another <em>fox</em> appeared near the river","Finally the last <em>fox</em> slept."]}}]`},
		{"best passages in document order",
			`{"query":{"match":{"long":"fox"}},"highlight":{"fields":{"long":{"number_of_fragments":2}}}}`,
			`[{"_id":"1","hl":{"long":["The <em>fox</em> ran into the forest.","Finally the last <em>fox</em> slept."]}}]`},
		{"order score",
			`{"query":{"match":{"long":"fox"}},"highlight":{"fields":{"long":{"number_of_fragments":3,"order":"score"}}}}`,
			`[{"_id":"1","hl":{"long":["The <em>fox</em> ran into the forest.","Finally the last <em>fox</em> slept.","Another <em>fox</em> appeared near the river bank at dawn."]}}]`},
		{"number_of_fragments 0 highlights the whole value",
			`{"query":{"match":{"long":"fox"}},"highlight":{"fields":{"long":{"number_of_fragments":0}}}}`,
			`[{"_id":"1","hl":{"long":["` + `Alpha beta gamma. The <em>fox</em> ran into the forest. Delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau upsilon phi chi psi omega. Another <em>fox</em> appeared near the river bank at dawn. Numbers one two three four five six seven eight nine ten. Finally the last <em>fox</em> slept."]}}]`},
		{"word boundary scanner",
			`{"query":{"match":{"body":"fox"}},"highlight":{"fields":{"body":{"boundary_scanner":"word","fragment_size":20}}},"sort":["_id"]}`,
			`[{"_id":"1","hl":{"body":["<em>fox</em>"]}},{"_id":"3","hl":{"body":["<em>fox</em>"]}}]`},
		{"one snippet per matching value",
			`{"query":{"match":{"multi":"fox"}},"highlight":{"fields":{"multi":{}}},"sort":["_id"]}`,
			`[{"_id":"1","hl":{"multi":["red <em>fox</em>","green <em>fox</em>"]}},{"_id":"3","hl":{"multi":["<em>fox</em>"]}}]`},
		{"phrase terms",
			`{"query":{"match_phrase":{"body":"brown fox"}},"highlight":{"fields":{"body":{}}}}`,
			`[{"_id":"1","hl":{"body":["The quick <em>brown</em> <em>fox</em> jumps over the lazy dog. The dog sleeps."]}}]`},
		{"require_field_match false",
			`{"query":{"match":{"title":"fox"}},"highlight":{"require_field_match":false,"fields":{"body":{},"multi":{}}},"sort":["_id"]}`,
			`[{"_id":"1","hl":{"body":["The quick brown <em>fox</em> jumps over the lazy dog. The dog sleeps."],"multi":["red <em>fox</em>","green <em>fox</em>"]}},{"_id":"3","hl":{"body":["<em>fox</em>"],"multi":["<em>fox</em>"]}}]`},
		{"field highlight_query",
			`{"query":{"match_all":{}},"highlight":{"fields":{"body":{"highlight_query":{"match":{"body":"dog"}}}}},"sort":["_id"]}`,
			`[{"_id":"1","hl":{"body":["The quick brown fox jumps over the lazy <em>dog</em>. The <em>dog</em> sleeps."]}},{"_id":"2"},{"_id":"3"}]`},
		{"global highlight_query",
			`{"query":{"match":{"body":"fox"}},"highlight":{"highlight_query":{"match":{"body":"lazy"}},"fields":{"body":{}}},"sort":["_id"]}`,
			`[{"_id":"1","hl":{"body":["The quick brown fox jumps over the <em>lazy</em> dog. The dog sleeps."]}},{"_id":"3"}]`},
		{"no_match_size",
			`{"query":{"match":{"body":"fox"}},"highlight":{"fields":{"body":{},"title":{"no_match_size":10}}},"sort":["_id"]}`,
			`[{"_id":"1","hl":{"body":["The quick brown <em>fox</em> jumps over the lazy dog. The dog sleeps."],"title":["The Quick Brown"]}},{"_id":"3","hl":{"body":["<em>fox</em>"],"title":["Fox fox FOX"]}}]`},
		{"styled tags",
			`{"query":{"match":{"body":"fox dog"}},"highlight":{"tags_schema":"styled","fields":{"body":{}}},"sort":["_id"]}`,
			`[{"_id":"1","hl":{"body":["The quick brown <em class=\"hlt1\">fox</em> jumps over the lazy <em class=\"hlt1\">dog</em>. The <em class=\"hlt1\">dog</em> sleeps."]}},{"_id":"3","hl":{"body":["<em class=\"hlt1\">fox</em>"]}}]`},
		{"html encoder",
			`{"query":{"match":{"html":"fox"}},"highlight":{"encoder":"html","fields":{"html":{}}}}`,
			`[{"_id":"1","hl":{"html":["&lt;b&gt;<em>fox</em>&lt;&#x2F;b&gt; &amp; friends"]}}]`},
		{"plain highlighter fragments",
			`{"query":{"match":{"long":"fox"}},"highlight":{"fields":{"long":{"type":"plain","fragment_size":30,"number_of_fragments":20}}}}`,
			`[{"_id":"1","hl":{"long":["Alpha beta gamma. The <em>fox</em> ran"," omega. Another <em>fox</em> appeared",". Finally the last <em>fox</em> slept."]}}]`},
		{"fvh tags per query term",
			`{"query":{"match":{"tv":"fox dog"}},"highlight":{"pre_tags":["<x>","<y>"],"post_tags":["</x>","</y>"],"fields":{"tv":{"type":"fvh"}}}}`,
			`[{"_id":"1","hl":{"tv":["The quick brown <x>fox</x> jumps over the lazy <y>dog</y>. The <y>dog</y> sleeps."]}}]`},
		{"fvh phrase",
			`{"query":{"match_phrase":{"tv":"brown fox"}},"highlight":{"fields":{"tv":{"type":"fvh"}}}}`,
			`[{"_id":"1","hl":{"tv":["The quick <em>brown fox</em> jumps over the lazy dog. The dog sleeps."]}}]`},
		{"keyword values",
			`{"query":{"terms":{"tags":["fox","animal"]}},"highlight":{"fields":{"tags":{}}}}`,
			`[{"_id":"1","hl":{"tags":["<em>fox</em>","<em>animal</em>"]}}]`},
		{"collapse inner_hits without a top-level highlight",
			`{"query":{"match":{"body":"fox"}},"collapse":{"field":"group","inner_hits":{"name":"g","highlight":{"fields":{"body":{},"group":{}}}}},"sort":["_id"]}`,
			`[{"_id":"1","inner":{"g":[{"_id":"1","hl":{"body":["The quick brown <em>fox</em> jumps over the lazy dog. The dog sleeps."],"group":["<em>g1</em>"]}}]}},{"_id":"3","inner":{"g":[{"_id":"3","hl":{"body":["<em>fox</em>"],"group":["<em>g2</em>"]}}]}}]`},
		{"nested inner_hits",
			`{"query":{"nested":{"path":"comments","query":{"match":{"comments.text":"fox"}},"inner_hits":{"highlight":{"fields":{"comments.text":{}}}}}}}`,
			`[{"_id":"1","inner":{"comments":[{"_id":"1","hl":{"comments.text":["nice <em>fox</em>"]}},{"_id":"1","hl":{"comments.text":["<em>fox</em> and dog"]}}]}}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := mustDo(t, c, http.MethodPost, "/hl/_search", tc.body)
			var want any
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			got := summarizeHighlights(res)
			if !reflect.DeepEqual(any(got), want) {
				g, _ := json.Marshal(got)
				t.Fatalf("highlights\n got %s\nwant %s", g, tc.want)
			}
		})
	}
}

func TestHighlightValidationCompatibility(t *testing.T) {
	c := highlightCluster(t)
	defer c.Close()
	fox := `"query":{"match":{"body":"fox"}}`
	cases := []struct {
		name, body, typ, reason, rootType, rootReason string
	}{
		{"unknown highlighter type", `{` + fox + `,"highlight":{"fields":{"body":{"type":"foo"}}}}`,
			"search_phase_execution_exception", "all shards failed", "illegal_argument_exception", "unknown highlighter type [foo] for the field [body]"},
		{"unknown field with suggestion", `{` + fox + `,"highlight":{"max_analyzed_offset":10,"fields":{"body":{}}}}`,
			"x_content_parse_exception", "[1:48] [highlight] unknown field [max_analyzed_offset] did you mean [max_analyzer_offset]?",
			"x_content_parse_exception", "[1:48] [highlight] unknown field [max_analyzed_offset] did you mean [max_analyzer_offset]?"},
		{"fields array entry with two fields", `{` + fox + `,"highlight":{"fields":[{"body":{},"title":{}}]}}`,
			"x_content_parse_exception", "[1:69] [highlight] failed to parse field [fields]",
			"x_content_parse_exception", "[1:69] [fields] can be a single object with any number of fields or an array where each entry is an object with a single field"},
		{"unknown tags_schema", `{` + fox + `,"highlight":{"tags_schema":"unknown","fields":{"body":{}}}}`,
			"x_content_parse_exception", "[1:62] [highlight] failed to parse field [tags_schema]",
			"x_content_parse_exception", "[1:62] [highlight] failed to parse field [tags_schema]"},
		{"fragment_size is not a number", `{` + fox + `,"highlight":{"fields":{"body":{"fragment_size":"abc"}}}}`,
			"x_content_parse_exception", "[1:82] [highlight] failed to parse field [fields]",
			"x_content_parse_exception", "[1:82] [highlight_field] failed to parse field [fragment_size]"},
		{"unknown boundary_scanner", `{` + fox + `,"highlight":{"fields":{"body":{"boundary_scanner":"foo"}}}}`,
			"x_content_parse_exception", "[1:85] [highlight] failed to parse field [fields]",
			"x_content_parse_exception", "[1:85] [highlight_field] failed to parse field [boundary_scanner]"},
		{"pre_tags without post_tags", `{` + fox + `,"highlight":{"pre_tags":["<x>"],"fields":{"body":{}}}}`,
			"parsing_exception", "pre_tags are set but post_tags are not set", "parsing_exception", "pre_tags are set but post_tags are not set"},
		{"highlight is a string", `{` + fox + `,"highlight":"x"}`,
			"parsing_exception", "Unknown key for a VALUE_STRING in [highlight].", "parsing_exception", "Unknown key for a VALUE_STRING in [highlight]."},
		{"field options are a string", `{` + fox + `,"highlight":{"fields":{"body":"x"}}}`,
			"x_content_parse_exception", "[1:65] [highlight] failed to parse field [fields]",
			"x_content_parse_exception", "[1:65] [highlight_field] Expected START_OBJECT but was: VALUE_STRING"},
		{"unknown query in highlight_query", `{` + fox + `,"highlight":{"fields":{"body":{"highlight_query":{"nope":{}}}}}}`,
			"x_content_parse_exception", "[1:92] [highlight] failed to parse field [fields]", "parsing_exception", "unknown query [nope]"},
		{"fvh without term vectors", `{` + fox + `,"highlight":{"fields":{"body":{"type":"fvh"}}}}`,
			"search_phase_execution_exception", "all shards failed", "illegal_argument_exception",
			"the field [body] should be indexed with term vector with position offsets to be used with fast vector highlighter"},
		{"fvh fragment_size too small", `{"query":{"match":{"tv":"fox"}},"highlight":{"fields":{"tv":{"type":"fvh","fragment_size":10}}}}`,
			"search_phase_execution_exception", "all shards failed", "illegal_argument_exception", "fragCharSize(10) is too small. It must be 18 or higher."},
		{"chars boundary scanner with unified", `{` + fox + `,"highlight":{"fields":{"body":{"boundary_scanner":"chars"}}}}`,
			"search_phase_execution_exception", "all shards failed", "illegal_argument_exception", "Invalid boundary scanner type: chars"},
		{"max_analyzer_offset must be positive", `{` + fox + `,"highlight":{"max_analyzer_offset":0,"fields":{"body":{}}}}`,
			"search_phase_execution_exception", "all shards failed", "illegal_argument_exception", "the value [0] of max_analyzer_offset is invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := status(t, c, http.MethodPost, "/hl/_search", tc.body)
			e, _ := body["error"].(map[string]any)
			roots, _ := e["root_cause"].([]any)
			var root map[string]any
			if len(roots) > 0 {
				root, _ = roots[0].(map[string]any)
			}
			if code != http.StatusBadRequest || e["type"] != tc.typ || e["reason"] != tc.reason || root["type"] != tc.rootType || root["reason"] != tc.rootReason {
				t.Fatalf("got %d %v", code, body)
			}
		})
	}
}
