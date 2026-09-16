package osmem

import (
	"net/http"
	"regexp"
	"testing"
)

// The expectations of this file were recorded from OpenSearch 3.8.0.

func tvTestIndex(t *testing.T, c *Cluster) {
	t.Helper()
	mustDo(t, c, http.MethodPut, "/testidx", `{"mappings":{"properties":{
		"text":{"type":"text","term_vector":"with_positions_offsets"}}}}`)
	mustDo(t, c, http.MethodPut, "/testidx/_doc/testing_document", `{"text":"The quick brown fox is brown."}`)
	mustDo(t, c, http.MethodPost, "/testidx/_refresh", nil)
}

func TestTermVectorsBasicCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	tvTestIndex(t, c)

	res := mustDo(t, c, http.MethodGet, "/testidx/_termvectors/testing_document?term_statistics=true", nil)
	if jsonAt(t, res, "/term_vectors/text/field_statistics/sum_doc_freq") != float64(5) {
		t.Fatalf("sum_doc_freq: %v", res)
	}
	if jsonAt(t, res, "/term_vectors/text/field_statistics/doc_count") != float64(1) {
		t.Fatalf("doc_count: %v", res)
	}
	if jsonAt(t, res, "/term_vectors/text/field_statistics/sum_ttf") != float64(6) {
		t.Fatalf("sum_ttf: %v", res)
	}
	if jsonAt(t, res, "/term_vectors/text/terms/brown/doc_freq") != float64(1) {
		t.Fatalf("brown doc_freq: %v", res)
	}
	if jsonAt(t, res, "/term_vectors/text/terms/brown/ttf") != float64(2) {
		t.Fatalf("brown ttf: %v", res)
	}
	if jsonAt(t, res, "/term_vectors/text/terms/brown/term_freq") != float64(2) {
		t.Fatalf("brown term_freq: %v", res)
	}
	if jsonAt(t, res, "/term_vectors/text/terms/brown/tokens/0/start_offset") != float64(10) {
		t.Fatalf("brown start_offset: %v", res)
	}
	if jsonAt(t, res, "/term_vectors/text/terms/brown/tokens/0/end_offset") != float64(15) {
		t.Fatalf("brown end_offset: %v", res)
	}
	if jsonAt(t, res, "/term_vectors/text/terms/brown/tokens/0/position") != float64(2) {
		t.Fatalf("brown position 0: %v", res)
	}
	if jsonAt(t, res, "/term_vectors/text/terms/brown/tokens/1/position") != float64(5) {
		t.Fatalf("brown position 1: %v", res)
	}
	if jsonAt(t, res, "/_index") != "testidx" || jsonAt(t, res, "/_id") != "testing_document" || jsonAt(t, res, "/found") != true {
		t.Fatalf("envelope: %v", res)
	}
}

func TestTermVectorsDefaultsCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	tvTestIndex(t, c)

	// without term_statistics, doc_freq/ttf are absent but term_freq and
	// tokens (positions+offsets on by default) are present
	res := mustDo(t, c, http.MethodGet, "/testidx/_termvectors/testing_document", nil)
	terms, _ := jsonAt(t, res, "/term_vectors/text/terms").(map[string]any)
	brown, _ := terms["brown"].(map[string]any)
	if _, ok := brown["doc_freq"]; ok {
		t.Fatalf("doc_freq should be absent by default: %v", brown)
	}
	if _, ok := brown["ttf"]; ok {
		t.Fatalf("ttf should be absent by default: %v", brown)
	}
	if brown["term_freq"] != float64(2) {
		t.Fatalf("term_freq: %v", brown)
	}
	if _, ok := brown["tokens"]; !ok {
		t.Fatalf("tokens should be present by default: %v", brown)
	}

	// positions=false&offsets=false drops the tokens array entirely
	res = mustDo(t, c, http.MethodGet, "/testidx/_termvectors/testing_document?positions=false&offsets=false", nil)
	terms, _ = jsonAt(t, res, "/term_vectors/text/terms").(map[string]any)
	brown, _ = terms["brown"].(map[string]any)
	if _, ok := brown["tokens"]; ok {
		t.Fatalf("tokens should be absent: %v", brown)
	}
	if brown["term_freq"] != float64(2) {
		t.Fatalf("term_freq still present: %v", brown)
	}

	// a field named explicitly gets vectors even without a term_vector
	// mapping (OpenSearch falls back to analyzing it on the fly)
	mustDo(t, c, http.MethodPut, "/testidx/_mapping", `{"properties":{"plain":{"type":"text"}}}`)
	mustDo(t, c, http.MethodPost, "/testidx/_update/testing_document", `{"doc":{"plain":"hello world"}}`)
	mustDo(t, c, http.MethodPost, "/testidx/_refresh", nil)
	res = mustDo(t, c, http.MethodGet, "/testidx/_termvectors/testing_document?fields=plain", nil)
	if jsonAt(t, res, "/term_vectors/plain/terms/hello/term_freq") != float64(1) {
		t.Fatalf("explicit plain field: %v", res)
	}
	if _, ok := jsonAt(t, res, "/term_vectors").(map[string]any)["text"]; ok {
		t.Fatalf("only the explicitly requested field should appear: %v", res)
	}

	// nothing requested and no mapped term_vector field: empty term_vectors
	mustDo(t, c, http.MethodPut, "/plainidx", `{"mappings":{"properties":{"a":{"type":"text"}}}}`)
	mustDo(t, c, http.MethodPut, "/plainidx/_doc/1", `{"a":"x"}`)
	mustDo(t, c, http.MethodPost, "/plainidx/_refresh", nil)
	res = mustDo(t, c, http.MethodGet, "/plainidx/_termvectors/1", nil)
	if tv, ok := res["term_vectors"]; ok && len(tv.(map[string]any)) > 0 {
		t.Fatalf("no default fields should yield no term_vectors: %v", res)
	}
}

func TestTermVectorsRealtimeCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/test_1", `{"settings":{"refresh_interval":-1,"number_of_replicas":0}}`)
	mustDo(t, c, http.MethodPut, "/test_1/_doc/1", `{"foo":"bar"}`)

	if r := mustDo(t, c, http.MethodGet, "/test_1/_termvectors/1?realtime=false", nil); r["found"] != false {
		t.Fatalf("realtime=false before refresh should be not found: %v", r)
	}
	if r := mustDo(t, c, http.MethodGet, "/test_1/_termvectors/1?realtime=true", nil); r["found"] != true {
		t.Fatalf("realtime=true should see the live document: %v", r)
	}
	// realtime defaults to true
	if r := mustDo(t, c, http.MethodGet, "/test_1/_termvectors/1", nil); r["found"] != true {
		t.Fatalf("default realtime should see the live document: %v", r)
	}
	mustDo(t, c, http.MethodPost, "/test_1/_refresh", nil)
	if r := mustDo(t, c, http.MethodGet, "/test_1/_termvectors/1?realtime=false", nil); r["found"] != true {
		t.Fatalf("realtime=false after refresh should be found: %v", r)
	}

	// a genuinely missing document is not found regardless of realtime
	if st, body := status(t, c, http.MethodGet, "/test_1/_termvectors/nope", nil); st != 200 {
		t.Fatalf("missing doc status: %d %v", st, body)
	} else if body["found"] != false || body["_version"] != float64(0) {
		t.Fatalf("missing doc body: %v", body)
	}
	// a missing index is a top-level 404, unlike a missing document
	if st, _ := status(t, c, http.MethodGet, "/no_such_index/_termvectors/1", nil); st != 404 {
		t.Fatalf("missing index status: %d", st)
	}
}

func TestMultiTermVectorsBasicCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	tvTestIndex(t, c)

	// docs form at the root, full _index/_id per doc
	res := mustDo(t, c, http.MethodPost, "/_mtermvectors?term_statistics=true", `{"docs":[{"_index":"testidx","_id":"testing_document"}]}`)
	if jsonAt(t, res, "/docs/0/term_vectors/text/terms/brown/term_freq") != float64(2) ||
		jsonAt(t, res, "/docs/0/term_vectors/text/terms/brown/ttf") != float64(2) {
		t.Fatalf("docs form: %v", res)
	}

	// docs form under an index path, _id only
	res = mustDo(t, c, http.MethodPost, "/testidx/_mtermvectors?term_statistics=true", `{"docs":[{"_id":"testing_document"}]}`)
	if jsonAt(t, res, "/docs/0/term_vectors/text/terms/brown/term_freq") != float64(2) ||
		jsonAt(t, res, "/docs/0/term_vectors/text/terms/brown/ttf") != float64(2) {
		t.Fatalf("docs form under index path: %v", res)
	}

	// ids form under an index path
	res = mustDo(t, c, http.MethodPost, "/testidx/_mtermvectors?term_statistics=true", `{"ids":["testing_document"]}`)
	if jsonAt(t, res, "/docs/0/term_vectors/text/terms/brown/term_freq") != float64(2) ||
		jsonAt(t, res, "/docs/0/term_vectors/text/terms/brown/ttf") != float64(2) {
		t.Fatalf("ids form: %v", res)
	}
}

func TestMultiTermVectorsErrorTracesCompatibility(t *testing.T) {
	c := New()
	defer c.Close()

	stackTraceRe := regexp.MustCompile(`\[index_does_not_exist_321\].*IndexNotFoundException`)

	res := mustDo(t, c, http.MethodPost, "/index_does_not_exist_321/_mtermvectors?error_trace=true", `{"ids":["testing_document"]}`)
	if jsonAt(t, res, "/docs/0/error/type") != "index_not_found_exception" {
		t.Fatalf("error type: %v", res)
	}
	if jsonAt(t, res, "/docs/0/error/reason") != "no such index [index_does_not_exist_321]" {
		t.Fatalf("error reason: %v", res)
	}
	trace, _ := jsonAt(t, res, "/docs/0/error/stack_trace").(string)
	if !stackTraceRe.MatchString(trace) {
		t.Fatalf("stack_trace shape: %q", trace)
	}
	if jsonAt(t, res, "/docs/0/error/root_cause/0/type") != "index_not_found_exception" {
		t.Fatalf("root_cause type: %v", res)
	}
	rootTrace, _ := jsonAt(t, res, "/docs/0/error/root_cause/0/stack_trace").(string)
	if !stackTraceRe.MatchString(rootTrace) {
		t.Fatalf("root_cause stack_trace shape: %q", rootTrace)
	}

	// error_trace=false (the default) omits stack_trace
	res = mustDo(t, c, http.MethodPost, "/index_does_not_exist_321/_mtermvectors?error_trace=false", `{"ids":["testing_document"]}`)
	if _, ok := jsonAt(t, res, "/docs/0/error").(map[string]any)["stack_trace"]; ok {
		t.Fatalf("stack_trace should be absent: %v", res)
	}
	if _, ok := jsonAt(t, res, "/docs/0/error/root_cause/0").(map[string]any)["stack_trace"]; ok {
		t.Fatalf("root_cause stack_trace should be absent: %v", res)
	}
}
