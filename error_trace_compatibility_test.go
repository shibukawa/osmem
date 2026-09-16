package osmem

import (
	"net/http"
	"strings"
	"testing"
)

// TestErrorTraceCompatibility checks that ?error_trace=true fabricates a
// stack_trace naming the failing exception on every error object osmem
// renders, exactly where a real OpenSearch 3.8.0 server adds one: the
// top-level error, its root_cause entries, and the per-item errors of
// multi-document APIs (mget, bulk) — and that omitting error_trace, or
// setting it to false, never adds one. See internal/engine/errors.go's
// Error.header/content/Body and fakeStackTrace, threaded in here from the
// error_trace URL parameter (http.go's responseOptions.errorTrace for
// top-level errors, or a local read of the parameter for errors embedded in
// a larger response body).
func TestErrorTraceCompatibility(t *testing.T) {
	c := New()
	defer c.Close()

	// Top-level error (a 404 from a missing index, the shape a plain GET
	// returns): stack_trace on the error itself and on its root_cause entry,
	// both naming the exception; OpenSearch's own trace additionally quotes
	// the index in brackets, which osmem's fabricated one also does.
	res := expectStatus(t, c, http.MethodGet, "/no-such-index/_doc/1?error_trace=true", nil, http.StatusNotFound)
	if trace, _ := jsonAt(t, res, "/error/stack_trace").(string); !strings.Contains(trace, "IndexNotFoundException") {
		t.Errorf("top-level stack_trace = %q, want it to mention IndexNotFoundException", trace)
	}
	if trace, _ := jsonAt(t, res, "/error/root_cause/0/stack_trace").(string); !strings.Contains(trace, "IndexNotFoundException") {
		t.Errorf("root_cause stack_trace = %q, want it to mention IndexNotFoundException", trace)
	}

	for _, path := range []string{"/no-such-index/_doc/1?error_trace=false", "/no-such-index/_doc/1"} {
		res = expectStatus(t, c, http.MethodGet, path, nil, http.StatusNotFound)
		if _, ok := compatibilityPointer(res, "/error/stack_trace"); ok {
			t.Errorf("GET %s: unexpected stack_trace: %v", path, res)
		}
		if _, ok := compatibilityPointer(res, "/error/root_cause/0/stack_trace"); ok {
			t.Errorf("GET %s: unexpected root_cause stack_trace: %v", path, res)
		}
	}

	// mget's per-item error keeps the same root_cause-wrapped shape as a
	// top-level error (verified against a live OpenSearch 3.8.0 server).
	mgetBody := `{"docs":[{"_index":"no-such-mget-index","_id":"1"}]}`
	mgetTrue := mustDo(t, c, http.MethodPost, "/_mget?error_trace=true", mgetBody)
	if trace, _ := jsonAt(t, mgetTrue, "/docs/0/error/stack_trace").(string); !strings.Contains(trace, "IndexNotFoundException") {
		t.Errorf("mget stack_trace = %q, want it to mention IndexNotFoundException", trace)
	}
	if trace, _ := jsonAt(t, mgetTrue, "/docs/0/error/root_cause/0/stack_trace").(string); !strings.Contains(trace, "IndexNotFoundException") {
		t.Errorf("mget root_cause stack_trace = %q, want it to mention IndexNotFoundException", trace)
	}
	mgetFalse := mustDo(t, c, http.MethodPost, "/_mget", mgetBody)
	if _, ok := compatibilityPointer(mgetFalse, "/docs/0/error/stack_trace"); ok {
		t.Errorf("mget without error_trace: unexpected stack_trace: %v", mgetFalse)
	}

	// bulk's per-item error has no root_cause wrapper at all (verified
	// against a live OpenSearch 3.8.0 server: it renders the bare exception
	// content, matching content() rather than Body()).
	mustDo(t, c, http.MethodPut, "/et-bulk/_doc/1", `{"foo":"bar"}`)
	createConflict := "{\"create\":{\"_index\":\"et-bulk\",\"_id\":\"1\"}}\n{\"foo\":\"baz\"}\n"
	action, item := bulkItem(mustDo(t, c, http.MethodPost, "/_bulk?error_trace=true", createConflict), 0)
	if action != "create" {
		t.Fatalf("bulk action = %q, want create", action)
	}
	if trace, _ := jsonAt(t, item, "/error/stack_trace").(string); !strings.Contains(trace, "VersionConflictEngineException") {
		t.Errorf("bulk stack_trace = %q, want it to mention VersionConflictEngineException", trace)
	}
	if _, ok := compatibilityPointer(item, "/error/root_cause"); ok {
		t.Errorf("bulk item error has a root_cause, real OpenSearch's per-item error doesn't: %v", item)
	}
	_, item = bulkItem(mustDo(t, c, http.MethodPost, "/_bulk", createConflict), 0)
	if _, ok := compatibilityPointer(item, "/error/stack_trace"); ok {
		t.Errorf("bulk without error_trace: unexpected stack_trace: %v", item)
	}
}
