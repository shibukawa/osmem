package osmem

import (
	"net/http"
	"testing"
)

// The expectations in this file were captured against a real OpenSearch
// 3.8.0 server (suggest/10_basic.yml, suggest/20_completion.yml and
// suggest/30_context.yml of the OpenSearch REST API spec test suite).
// The phrase suggester's scores are osmem's own stupid-backoff
// approximation (see suggest.go); only structural ordering is checked.

func suggestEntries(t *testing.T, res map[string]any, name string) []any {
	t.Helper()
	sg, ok := res["suggest"].(map[string]any)
	if !ok {
		t.Fatalf("no suggest section: %v", res)
	}
	entries, ok := sg[name].([]any)
	if !ok {
		t.Fatalf("no suggestion %q: %v", name, sg)
	}
	return entries
}

func suggestOptions(t *testing.T, res map[string]any, name string, entry int) []any {
	t.Helper()
	entries := suggestEntries(t, res, name)
	if entry >= len(entries) {
		t.Fatalf("suggestion %q has %d entries, want > %d", name, len(entries), entry)
	}
	options, _ := entries[entry].(map[string]any)["options"].([]any)
	return options
}

// TestSuggestTermBasic mirrors suggest/10_basic.yml.
func TestSuggestTermBasic(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/test", `{"mappings":{"properties":{"body":{"type":"text"}}}}`)
	mustDo(t, c, http.MethodPut, "/test/_doc/testing_document", `{"body":"Amsterdam meetup"}`)

	res := mustDo(t, c, http.MethodPost, "/test/_search", `{
		"suggest": {"test_suggestion": {"text": "The Amsterdma meetpu", "term": {"field": "body"}}}
	}`)
	entries := suggestEntries(t, res, "test_suggestion")
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3: %v", len(entries), entries)
	}
	// captured from OpenSearch 3.8.0
	assertJSON(t, entries[0], `{"text":"the","offset":0,"length":3,"options":[]}`)
	assertJSON(t, entries[1], `{"text":"amsterdma","offset":4,"length":9,"options":[{"text":"amsterdam","score":0.8888889,"freq":1}]}`)
	assertJSON(t, entries[2], `{"text":"meetpu","offset":14,"length":6,"options":[{"text":"meetup","score":0.8333333,"freq":1}]}`)
}

// TestSuggestTermModesAndSort exercises suggest_mode and the term suggester
// candidate ranking against a slightly larger dictionary (also captured
// from OpenSearch 3.8.0).
func TestSuggestTermModesAndSort(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/test", `{"mappings":{"properties":{"body":{"type":"text"}}}}`)
	for id, body := range map[string]string{
		"1": `{"body":"amsterdam"}`, "2": `{"body":"amsterdam amsterdam"}`, "3": `{"body":"amsterdmm"}`,
		"4": `{"body":"amsterdima"}`, "5": `{"body":"amsterdak"}`, "6": `{"body":"amsterdan"}`,
		"7": `{"body":"amsterday"}`, "8": `{"body":"amsterdaz"}`,
	} {
		mustDo(t, c, http.MethodPut, "/test/_doc/"+id, body)
	}
	res := mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"s1":{"text":"amsterdma","term":{"field":"body"}}}}`)
	options := suggestOptions(t, res, "s1", 0)
	// default size 5: the three edit-distance-1 candidates first (by
	// frequency, "amsterdam" appearing twice), then the alphabetically
	// first two of the four tied edit-distance-2 candidates.
	assertJSON(t, options, `[
		{"text":"amsterdam","score":0.8888889,"freq":2},
		{"text":"amsterdima","score":0.8888889,"freq":1},
		{"text":"amsterdmm","score":0.8888889,"freq":1},
		{"text":"amsterdak","score":0.7777778,"freq":1},
		{"text":"amsterdan","score":0.7777778,"freq":1}
	]`)

	// suggest_mode "always" also runs against the input's own frequency
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"s1":{"text":"amsterdammx","term":{"field":"body","suggest_mode":"always"}}}}`)
	assertJSON(t, suggestOptions(t, res, "s1", 0), `[
		{"text":"amsterdam","score":0.7777778,"freq":2},
		{"text":"amsterdmm","score":0.7777778,"freq":1}
	]`)
}

// TestSuggestCompletionBasic mirrors suggest/20_completion.yml.
func TestSuggestCompletionBasic(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/test", `{"mappings":{"properties":{
		"suggest_1": {"type":"completion"}, "suggest_2": {"type":"completion"}, "suggest_3": {"type":"completion"},
		"suggest_4": {"type":"completion"}, "suggest_5a": {"type":"completion"}, "suggest_5b": {"type":"completion"},
		"suggest_6": {"type":"completion"}, "title": {"type":"keyword"}
	}}}`)

	optText := func(o any) string { return o.(map[string]any)["text"].(string) }

	// "Simple suggestion should work"
	mustDo(t, c, http.MethodPut, "/test/_doc/1", `{"suggest_1":"bar"}`)
	mustDo(t, c, http.MethodPut, "/test/_doc/2", `{"suggest_1":"baz"}`)
	res := mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"b","completion":{"field":"suggest_1"}}}}`)
	if entries := suggestEntries(t, res, "result"); len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if opts := suggestOptions(t, res, "result", 0); len(opts) != 2 {
		t.Fatalf("options = %d, want 2: %v", len(opts), opts)
	}

	// "Simple suggestion array should work"
	mustDo(t, c, http.MethodPut, "/test/_doc/3", `{"suggest_2":["bar","foo"]}`)
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"f","completion":{"field":"suggest_2"}}}}`)
	opts := suggestOptions(t, res, "result", 0)
	if len(opts) != 1 || optText(opts[0]) != "foo" {
		t.Fatalf("options = %v, want [foo]", opts)
	}
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"b","completion":{"field":"suggest_2"}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 1 || optText(opts[0]) != "bar" {
		t.Fatalf("options = %v, want [bar]", opts)
	}

	// "Suggestion entry should work": weight-ordered, not insertion-ordered
	mustDo(t, c, http.MethodPut, "/test/_doc/4", `{"suggest_3":{"input":"bar","weight":2}}`)
	mustDo(t, c, http.MethodPut, "/test/_doc/5", `{"suggest_3":{"input":"baz","weight":3}}`)
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"b","completion":{"field":"suggest_3"}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 2 || optText(opts[0]) != "baz" || optText(opts[1]) != "bar" {
		t.Fatalf("options = %v, want [baz bar]", opts)
	}

	// "Suggestion entry array should work"
	mustDo(t, c, http.MethodPut, "/test/_doc/6", `{"suggest_4":[{"input":"bar","weight":3},{"input":"fo","weight":3}]}`)
	mustDo(t, c, http.MethodPut, "/test/_doc/7", `{"suggest_4":[{"input":"baz","weight":2},{"input":"foo","weight":1}]}`)
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"b","completion":{"field":"suggest_4"}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 2 || optText(opts[0]) != "bar" || optText(opts[1]) != "baz" {
		t.Fatalf("options = %v, want [bar baz]", opts)
	}
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"f","completion":{"field":"suggest_4"}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 2 || optText(opts[0]) != "fo" || optText(opts[1]) != "foo" {
		t.Fatalf("options = %v, want [fo foo]", opts)
	}

	// "Multiple Completion fields should work"
	mustDo(t, c, http.MethodPut, "/test/_doc/8", `{"suggest_5a":"bar","suggest_5b":"baz"}`)
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"b","completion":{"field":"suggest_5a"}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 1 || optText(opts[0]) != "bar" {
		t.Fatalf("suggest_5a options = %v, want [bar]", opts)
	}
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"b","completion":{"field":"suggest_5b"}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 1 || optText(opts[0]) != "baz" {
		t.Fatalf("suggest_5b options = %v, want [baz]", opts)
	}

	// "Suggestions with source should work"
	mustDo(t, c, http.MethodPut, "/test/_doc/9", `{"suggest_6":{"input":"bar","weight":2},"title":"title_bar","count":4}`)
	mustDo(t, c, http.MethodPut, "/test/_doc/10", `{"suggest_6":{"input":"baz","weight":3},"title":"title_baz","count":3}`)
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"b","completion":{"field":"suggest_6"}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 2 {
		t.Fatalf("options = %v, want 2 entries", opts)
	}
	first, second := opts[0].(map[string]any), opts[1].(map[string]any)
	if first["text"] != "baz" || first["_index"] != "test" {
		t.Fatalf("first option = %v", first)
	}
	src, _ := first["_source"].(map[string]any)
	if src["title"] != "title_baz" {
		t.Fatalf("first option source = %v", src)
	}
	if n, ok := src["count"].(float64); !ok || n != 3 {
		t.Fatalf("first option source count = %v", src["count"])
	}
	if second["text"] != "bar" {
		t.Fatalf("second option = %v", second)
	}
	src2, _ := second["_source"].(map[string]any)
	if src2["title"] != "title_bar" {
		t.Fatalf("second option source = %v", src2)
	}

	// "Skip duplicates should work" (overwrites docs 1 and 2, as the source
	// YAML test does, so only "bar" remains in suggest_1)
	mustDo(t, c, http.MethodPut, "/test/_doc/1", `{"suggest_1":"bar"}`)
	mustDo(t, c, http.MethodPut, "/test/_doc/2", `{"suggest_1":"bar"}`)
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"b","completion":{"field":"suggest_1","skip_duplicates":true}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 1 || optText(opts[0]) != "bar" {
		t.Fatalf("skip_duplicates options = %v, want [bar]", opts)
	}
}

// TestSuggestCompletionContext mirrors suggest/30_context.yml.
func TestSuggestCompletionContext(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/test", `{"mappings":{"properties":{
		"location": {"type":"geo_point"},
		"suggest_context": {"type":"completion","contexts":[{"name":"color","type":"category"}]},
		"suggest_context_with_path": {"type":"completion","contexts":[{"name":"color","type":"category","path":"color"}]},
		"suggest_multi_contexts": {"type":"completion","contexts":[
			{"name":"location","type":"geo","precision":"5km","path":"location"},
			{"name":"color","type":"category","path":"color"}
		]}
	}}}`)

	optText := func(o any) string { return o.(map[string]any)["text"].(string) }

	// "Simple context suggestion should work"
	mustDo(t, c, http.MethodPut, "/test/_doc/1", `{"suggest_context":{"input":"foo red","contexts":{"color":"red"}}}`)
	mustDo(t, c, http.MethodPut, "/test/_doc/2", `{"suggest_context":{"input":"foo blue","contexts":{"color":"blue"}}}`)
	res := mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"foo","completion":{"field":"suggest_context","contexts":{"color":"red"}}}}}`)
	opts := suggestOptions(t, res, "result", 0)
	if len(opts) != 1 || optText(opts[0]) != "foo red" {
		t.Fatalf("options = %v, want [\"foo red\"]", opts)
	}

	// "Category suggest context from path should work"
	mustDo(t, c, http.MethodPut, "/test/_doc/3", `{"suggest_context_with_path":{"input":"Foo red","contexts":{"color":"red"}}}`)
	mustDo(t, c, http.MethodPut, "/test/_doc/4", `{"suggest_context_with_path":"Foo blue","color":"blue"}`)
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"foo","completion":{"field":"suggest_context_with_path","contexts":{"color":"red"}}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 1 || optText(opts[0]) != "Foo red" {
		t.Fatalf("options = %v, want [\"Foo red\"]", opts)
	}
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"foo","completion":{"field":"suggest_context_with_path","contexts":{"color":"blue"}}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 1 || optText(opts[0]) != "Foo blue" {
		t.Fatalf("options = %v, want [\"Foo blue\"]", opts)
	}
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"foo","completion":{"field":"suggest_context_with_path","contexts":{"color":["blue","red"]}}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 2 {
		t.Fatalf("options = %v, want 2 entries", opts)
	}

	// "Multi contexts should work"
	mustDo(t, c, http.MethodPut, "/test/_doc/5", `{"suggest_multi_contexts":"Marriot in Amsterdam","location":{"lat":52.22,"lon":4.53},"color":"red"}`)
	mustDo(t, c, http.MethodPut, "/test/_doc/6", `{"suggest_multi_contexts":"Marriot in Berlin","location":{"lat":53.31,"lon":13.24},"color":"blue"}`)
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"mar","completion":{"field":"suggest_multi_contexts","contexts":{"location":{"lat":52.22,"lon":4.53}}}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 1 || optText(opts[0]) != "Marriot in Amsterdam" {
		t.Fatalf("geo-context options = %v, want [\"Marriot in Amsterdam\"]", opts)
	}
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"mar","completion":{"field":"suggest_multi_contexts","contexts":{"color":"blue"}}}}}`)
	opts = suggestOptions(t, res, "result", 0)
	if len(opts) != 1 || optText(opts[0]) != "Marriot in Berlin" {
		t.Fatalf("color-only-context options = %v, want [\"Marriot in Berlin\"]", opts)
	}

	// "Indexing and Querying without contexts is forbidden"
	code, body := status(t, c, http.MethodPut, "/test/_doc/7", `{"suggest_context":{"input":"foo"}}`)
	if code != http.StatusBadRequest || errType(body) != "illegal_argument_exception" {
		t.Fatalf("indexing without context: status=%d body=%v", code, body)
	}
	if reason, _ := body["error"].(map[string]any)["reason"].(string); reason != "Contexts are mandatory in context enabled completion field [suggest_context]" {
		t.Fatalf("indexing without context reason = %q", reason)
	}
	for _, body := range []string{
		`{"suggest":{"result":{"text":"foo","completion":{"field":"suggest_context"}}}}`,
		`{"suggest":{"result":{"text":"foo","completion":{"field":"suggest_context","contexts":{}}}}}`,
		`{"suggest":{"result":{"text":"foo","completion":{"field":"suggest_multi_contexts","contexts":{"location":[]}}}}}`,
	} {
		code, res := status(t, c, http.MethodPost, "/test/_search", body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", body, code)
		}
		typ, reason := rootCause(res)
		if typ != "illegal_argument_exception" || reason != "Missing mandatory contexts in context query" {
			t.Fatalf("%s: root_cause = %s %q", body, typ, reason)
		}
	}
}

// TestSuggestPhraseApproximate checks the phrase suggester's structure
// (an entry per suggestion, an option per candidate, "highlighted" marking
// the corrected word) rather than exact scores, which osmem approximates
// with a stupid-backoff n-gram model instead of Lucene's smoothed language
// model (see suggest.go).
func TestSuggestPhraseApproximate(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/test", `{"settings":{"index":{"number_of_shards":1}},"mappings":{"properties":{"title":{"type":"text"}}}}`)
	for id, body := range map[string]string{
		"1": `{"title":"noble prize"}`, "2": `{"title":"nobel prize winner"}`,
		"3": `{"title":"nobel gas"}`, "4": `{"title":"nobel prize"}`,
	} {
		mustDo(t, c, http.MethodPut, "/test/_doc/"+id, body)
	}

	// without a direct_generator, OpenSearch (and osmem) only scores the
	// unmodified phrase against the language model
	res := mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"simple_phrase":{"text":"noble prize","phrase":{
		"field":"title","size":3,"gram_size":2,"confidence":0,"max_errors":2
	}}}}`)
	opts := suggestOptions(t, res, "simple_phrase", 0)
	if len(opts) != 1 || opts[0].(map[string]any)["text"] != "noble prize" {
		t.Fatalf("options without direct_generator = %v, want just the input phrase", opts)
	}

	// with one, "nobel prize" (the bigram that actually occurs) outranks
	// the input phrase and is highlighted
	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"simple_phrase":{"text":"noble prize","phrase":{
		"field":"title","size":3,"gram_size":2,"confidence":0,"max_errors":2,
		"direct_generator":[{"field":"title","suggest_mode":"always","min_word_length":1}],
		"highlight":{"pre_tag":"<em>","post_tag":"</em>"}
	}}}}`)
	opts = suggestOptions(t, res, "simple_phrase", 0)
	if len(opts) < 2 {
		t.Fatalf("options with direct_generator = %v, want at least 2", opts)
	}
	top := opts[0].(map[string]any)
	if top["text"] != "nobel prize" || top["highlighted"] != "<em>nobel</em> prize" {
		t.Fatalf("top option = %v, want nobel prize highlighted", top)
	}
	foundIdentity := false
	for _, o := range opts {
		if o.(map[string]any)["text"] == "noble prize" {
			foundIdentity = true
		}
	}
	if !foundIdentity {
		t.Fatalf("options = %v, want the input phrase still present", opts)
	}
}

// TestSuggestCompletionFuzzy checks fuzzy-prefix completion matching
// (not exercised by suggest/20_completion.yml, but part of the task's
// explicit scope). Real OpenSearch 3.8.0, for "s": ["bar","box"] and the
// query text "boa" with fuzziness 1 and prefix_length 0, returns both "box"
// (edit distance 1 against the full candidate) and "bar" (edit distance 1
// against its "ba" prefix) but boosts exact-length matches over prefix
// ones in _score, a Lucene-internal detail osmem does not reproduce; only
// which candidates match is checked here.
func TestSuggestCompletionFuzzy(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/test", `{"mappings":{"properties":{"s":{"type":"completion"}}}}`)
	mustDo(t, c, http.MethodPut, "/test/_doc/1", `{"s":"bar"}`)
	mustDo(t, c, http.MethodPut, "/test/_doc/2", `{"s":"box"}`)

	// no fuzziness: neither candidate shares the "boa" prefix
	res := mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"boa","completion":{"field":"s"}}}}`)
	if opts := suggestOptions(t, res, "result", 0); len(opts) != 0 {
		t.Fatalf("exact-prefix options = %v, want none", opts)
	}

	res = mustDo(t, c, http.MethodPost, "/test/_search", `{"suggest":{"result":{"text":"boa","completion":{"field":"s","fuzzy":{"fuzziness":1,"prefix_length":0}}}}}`)
	opts := suggestOptions(t, res, "result", 0)
	got := map[string]bool{}
	for _, o := range opts {
		got[o.(map[string]any)["text"].(string)] = true
	}
	if !got["bar"] || !got["box"] || len(got) != 2 {
		t.Fatalf("fuzzy options = %v, want both bar and box", opts)
	}
}

// TestSuggestUnsupportedErrors checks the parse-time validation shared by
// every suggester (a required "field" option).
func TestSuggestUnsupportedErrors(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/test", `{"mappings":{"properties":{"body":{"type":"text"}}}}`)
	code, body := status(t, c, http.MethodPost, "/test/_search", `{"suggest":{"s1":{"text":"foo","term":{}}}}`)
	if code != http.StatusBadRequest || errType(body) != "parse_exception" {
		t.Fatalf("missing field: status=%d body=%v", code, body)
	}
	if reason, _ := body["error"].(map[string]any)["reason"].(string); reason != "the required field option [field] is missing" {
		t.Fatalf("missing field reason = %q", reason)
	}

	code, body = status(t, c, http.MethodPost, "/test/_search", `{"suggest":{"s1":{"text":"foo","completion":{"field":"body"}}}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("wrong field type: status=%d body=%v", code, body)
	}
	typ, reason := rootCause(body)
	if typ != "illegal_argument_exception" || reason != "Field [body] is not a completion suggest field" {
		t.Fatalf("wrong field type: root_cause = %s %q", typ, reason)
	}
}
