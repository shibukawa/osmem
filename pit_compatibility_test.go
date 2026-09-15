package osmem

import (
	"net/http"
	"testing"
	"time"
)

func TestPITSearchExpiresAndExtendsKeepAlive(t *testing.T) {
	start := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	now := start
	c := New(WithClock(func() time.Time { return now }))
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/pit-test", nil)
	mustDo(t, c, http.MethodPut, "/pit-test/_doc/1", `{"value":"snapshot"}`)

	createPIT := func() string {
		t.Helper()
		r := mustDo(t, c, http.MethodPost, "/pit-test/_search/point_in_time?keep_alive=1m", nil)
		return r["pit_id"].(string)
	}
	searchPIT := func(id, keepAlive string) (int, map[string]any) {
		t.Helper()
		body := `{"pit":{"id":"` + id + `"`
		if keepAlive != "" {
			body += `,"keep_alive":"` + keepAlive + `"`
		}
		body += `},"query":{"match_all":{}}}`
		return status(t, c, http.MethodPost, "/_search", body)
	}

	t.Run("expired PIT cannot be searched", func(t *testing.T) {
		id := createPIT()
		now = start.Add(61 * time.Second)
		code, body := searchPIT(id, "")
		if code != http.StatusNotFound || !pitContextMissing(body) {
			t.Fatalf("expired PIT search = %d %v, want 404 search_phase_execution_exception caused by search_context_missing_exception", code, body)
		}
	})

	t.Run("search keep_alive extends expiry from the search time", func(t *testing.T) {
		now = start
		id := createPIT()
		now = start.Add(40 * time.Second)
		code, body := searchPIT(id, "2m")
		if code != http.StatusOK {
			t.Fatalf("PIT search with keep_alive = %d %v, want 200", code, body)
		}

		// This is beyond the original creation-time expiry but before the
		// keep_alive extension measured from the search at t=40s.
		now = start.Add(130 * time.Second)
		code, body = searchPIT(id, "")
		if code != http.StatusOK {
			t.Fatalf("PIT search after original expiry = %d %v, want 200", code, body)
		}

		now = start.Add(161 * time.Second)
		code, body = searchPIT(id, "")
		if code != http.StatusNotFound || !pitContextMissing(body) {
			t.Fatalf("PIT search after extended expiry = %d %v, want 404 search_phase_execution_exception caused by search_context_missing_exception", code, body)
		}
	})
}

// pitContextMissing reports the error OpenSearch returns for a point in time
// whose reader context is gone: a search phase failure of its shards caused
// by search_context_missing_exception.
func pitContextMissing(body map[string]any) bool {
	if errType(body) != "search_phase_execution_exception" {
		return false
	}
	e, _ := body["error"].(map[string]any)
	roots, _ := e["root_cause"].([]any)
	if len(roots) == 0 {
		return false
	}
	root, _ := roots[0].(map[string]any)
	return root["type"] == "search_context_missing_exception"
}
