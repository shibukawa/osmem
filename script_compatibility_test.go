package osmem

import (
	"net/http"
	"strings"
	"testing"
)

// Painless script execution via github.com/shibukawa/painlessscript-go: the
// script and script_score queries, script_fields, the script_score function
// of function_score, and sort by _script. The embedded runtime is read-only
// (no ctx._source mutation), so update scripts, update_by_query/reindex
// scripts and scripted_metric stay unsupported; see api_test.go.

func scriptIndex(t *testing.T, c *Cluster) {
	t.Helper()
	dslIndex(t, c, "scripts", `{"mappings":{"properties":{
		"price":{"type":"double"},"qty":{"type":"integer"},"name":{"type":"keyword"},"active":{"type":"boolean"}
	}}}`,
		`{"price":10.5,"qty":3,"name":"a","active":true}`,
		`{"price":2.25,"qty":7,"name":"b","active":false}`,
		`{"price":100,"qty":1,"name":"c","active":true}`,
	)
}

func TestScriptFields(t *testing.T) {
	c := New()
	defer c.Close()
	scriptIndex(t, c)

	t.Run("computes a value per hit from doc and params", func(t *testing.T) {
		res := mustDo(t, c, http.MethodPost, "/scripts/_search", `{
			"query": {"match_all": {}}, "sort": ["_id"],
			"script_fields": {"total": {"script": {"source": "doc['price'].value * params.factor", "params": {"factor": 2}}}}
		}`)
		hits := res["hits"].(map[string]any)["hits"].([]any)
		got := hits[0].(map[string]any)["fields"].(map[string]any)["total"]
		assertJSON(t, got, `[21.0]`)
	})

	t.Run("ignore_failure skips the field on a runtime error", func(t *testing.T) {
		res := mustDo(t, c, http.MethodPost, "/scripts/_search", `{
			"query": {"match_all": {}}, "sort": ["_id"],
			"script_fields": {"x": {"script": {"source": "doc['missing'].value"}, "ignore_failure": true}}
		}`)
		hits := res["hits"].(map[string]any)["hits"].([]any)
		fields, _ := hits[0].(map[string]any)["fields"].(map[string]any)
		if _, ok := fields["x"]; ok {
			t.Fatalf("field x should have been skipped: %v", fields)
		}
	})

	t.Run("a runtime error fails the request without ignore_failure", func(t *testing.T) {
		st, res := status(t, c, http.MethodPost, "/scripts/_search", `{
			"query": {"match_all": {}},
			"script_fields": {"x": {"script": {"source": "doc['missing'].value"}}}
		}`)
		if st != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%v", st, res)
		}
		if typ, _ := rootCause(res); typ != "script_exception" {
			t.Fatalf("root cause=%s body=%v", typ, res)
		}
	})

	t.Run("a compile error is reported, not the raw unsupported stub", func(t *testing.T) {
		st, res := status(t, c, http.MethodPost, "/scripts/_search", `{
			"query": {"match_all": {}},
			"script_fields": {"x": {"script": {"source": "doc['price'"}}}
		}`)
		if st != http.StatusBadRequest {
			t.Fatalf("status=%d body=%v", st, res)
		}
		if typ, _ := rootCause(res); typ != "script_exception" {
			t.Fatalf("root cause=%s body=%v", typ, res)
		}
	})

	t.Run("object and array params are rejected", func(t *testing.T) {
		st, res := status(t, c, http.MethodPost, "/scripts/_search", `{
			"query": {"match_all": {}},
			"script_fields": {"x": {"script": {"source": "1", "params": {"nested": {"a": 1}}}}}
		}`)
		if st != http.StatusBadRequest {
			t.Fatalf("status=%d body=%v", st, res)
		}
	})
}

func TestScriptQuery(t *testing.T) {
	c := New()
	defer c.Close()
	scriptIndex(t, c)
	runDSLCases(t, c, "scripts", []dslCase{
		{name: "filters by a numeric comparison", query: `{"script": {"script": {"source": "doc['price'].value > params.min", "params": {"min": 5}}}}`, ids: []string{"1", "3"}},
		{name: "filters by a boolean field", query: `{"script": {"script": {"source": "doc['active'].value"}}}`, ids: []string{"1", "3"}},
	})
}

func TestScriptScoreQuery(t *testing.T) {
	c := New()
	defer c.Close()
	scriptIndex(t, c)
	st, res := status(t, c, http.MethodPost, "/scripts/_search",
		`{"query": {"script_score": {"query": {"match_all": {}}, "script": {"source": "doc['price'].value"}}}, "sort": ["_id"], "track_scores": true}`)
	if st != http.StatusOK {
		t.Fatalf("status=%d body=%v", st, res)
	}
	hits := res["hits"].(map[string]any)["hits"].([]any)
	for i, want := range []float64{10.5, 2.25, 100} {
		got, ok := hits[i].(map[string]any)["_score"].(float64)
		if !ok || got != want {
			t.Fatalf("hit %d score = %v, want %v", i, hits[i].(map[string]any)["_score"], want)
		}
	}
	st, res = status(t, c, http.MethodPost, "/scripts/_search",
		`{"query": {"script_score": {"query": {"match_all": {}}, "script": {"source": "doc['price'].value"}, "min_score": 5}}}`)
	if st != http.StatusOK {
		t.Fatalf("status=%d body=%v", st, res)
	}
	if hits := res["hits"].(map[string]any)["hits"].([]any); len(hits) != 2 {
		t.Fatalf("min_score: got %d hits, want 2: %v", len(hits), res)
	}
}

func TestFunctionScoreScriptScore(t *testing.T) {
	c := New()
	defer c.Close()
	scriptIndex(t, c)
	st, res := status(t, c, http.MethodPost, "/scripts/_search", `{
		"query": {"function_score": {"query": {"match_all": {}}, "script_score": {"script": {"source": "_score * params.factor", "params": {"factor": 2}}}}},
		"sort": ["_id"], "track_scores": true
	}`)
	if st != http.StatusOK {
		t.Fatalf("status=%d body=%v", st, res)
	}
	for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
		if score := h.(map[string]any)["_score"].(float64); score != 2 {
			t.Fatalf("hit score = %v, want 2", score)
		}
	}
}

func TestSortByScript(t *testing.T) {
	c := New()
	defer c.Close()
	scriptIndex(t, c)
	st, res := status(t, c, http.MethodPost, "/scripts/_search", `{
		"query": {"match_all": {}},
		"sort": [{"_script": {"type": "number", "script": {"source": "doc['price'].value"}, "order": "asc"}}]
	}`)
	if st != http.StatusOK {
		t.Fatalf("status=%d body=%v", st, res)
	}
	var ids []string
	for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
		ids = append(ids, h.(map[string]any)["_id"].(string))
	}
	if got := strings.Join(ids, ","); got != "2,1,3" {
		t.Fatalf("ids=%s, want 2,1,3 (sorted by price ascending)", got)
	}

	t.Run("search_after continues a numeric script sort", func(t *testing.T) {
		st, res := status(t, c, http.MethodPost, "/scripts/_search", `{
			"query": {"match_all": {}},
			"sort": [{"_script": {"type": "number", "script": {"source": "doc['price'].value"}, "order": "asc"}}],
			"search_after": [2.25]
		}`)
		if st != http.StatusOK {
			t.Fatalf("status=%d body=%v", st, res)
		}
		var ids []string
		for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
			ids = append(ids, h.(map[string]any)["_id"].(string))
		}
		if got := strings.Join(ids, ","); got != "1,3" {
			t.Fatalf("ids=%s, want 1,3 (after price 2.25)", got)
		}
	})

	t.Run("a null search_after on a numeric script sort is rejected", func(t *testing.T) {
		st, res := status(t, c, http.MethodPost, "/scripts/_search", `{
			"query": {"match_all": {}},
			"sort": [{"_script": {"type": "number", "script": {"source": "doc['price'].value"}}}],
			"search_after": [null]
		}`)
		if st != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%v", st, res)
		}
		if typ, _ := rootCause(res); typ != "null_pointer_exception" {
			t.Fatalf("root cause=%s body=%v", typ, res)
		}
	})

	t.Run("mode is not a script sort option", func(t *testing.T) {
		st, res := status(t, c, http.MethodPost, "/scripts/_search", `{
			"query": {"match_all": {}},
			"sort": [{"_script": {"type": "number", "script": {"source": "doc['price'].value"}, "mode": "avg"}}]
		}`)
		if st != http.StatusBadRequest {
			t.Fatalf("status=%d body=%v", st, res)
		}
	})

	t.Run("a nested script sort is rejected rather than silently scoped to the root", func(t *testing.T) {
		st, res := status(t, c, http.MethodPost, "/scripts/_search", `{
			"query": {"match_all": {}},
			"sort": [{"_script": {"type": "number", "script": {"source": "doc['price'].value"}, "nested": {"path": "x"}}}]
		}`)
		if st != http.StatusBadRequest {
			t.Fatalf("status=%d body=%v", st, res)
		}
		if typ, _ := rootCause(res); typ != "unsupported_operation_exception" {
			t.Fatalf("root cause=%s body=%v", typ, res)
		}
	})
}

func TestScriptUnsupportedLangAndStored(t *testing.T) {
	c := New()
	defer c.Close()
	scriptIndex(t, c)
	st, res := status(t, c, http.MethodPost, "/scripts/_search", `{"query": {"script": {"script": {"source": "true", "lang": "expression"}}}}`)
	if st != http.StatusBadRequest {
		t.Fatalf("expression lang: status=%d body=%v", st, res)
	}
	if typ, _ := rootCause(res); typ != "unsupported_operation_exception" {
		t.Fatalf("expression lang: root cause=%s body=%v", typ, res)
	}
	st, res = status(t, c, http.MethodPost, "/scripts/_search", `{"query": {"script": {"script": {"id": "nope"}}}}`)
	if st != http.StatusNotFound {
		t.Fatalf("stored script: status=%d body=%v", st, res)
	}
}
