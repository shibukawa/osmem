package osmem

import (
	"net/http"
	"testing"
)

func TestIgnoreAboveZeroSkipsNonemptyKeywordValues(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/ignore-above-zero", `{"mappings":{"properties":{"k":{"type":"keyword","ignore_above":0}}}}`)
	mustDo(t, c, http.MethodPut, "/ignore-above-zero/_doc/1", `{"k":"a"}`)
	res := mustDo(t, c, http.MethodPost, "/ignore-above-zero/_search", `{"query":{"term":{"k":"a"}}}`)
	hits := res["hits"].(map[string]any)["hits"].([]any)
	if len(hits) != 0 {
		t.Fatalf("ignore_above:0 indexed a nonempty value: %v", hits)
	}

	mapping := mustDo(t, c, http.MethodGet, "/ignore-above-zero/_mapping", nil)
	props := mapping["ignore-above-zero"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)
	if value := props["k"].(map[string]any)["ignore_above"]; value != float64(0) {
		t.Fatalf("mapping ignore_above = %v, want 0", value)
	}
}

func TestIgnoreAboveCanBeUpdatedToZero(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/ignore-above-update", `{"mappings":{"properties":{"k":{"type":"keyword","ignore_above":3}}}}`)
	mustDo(t, c, http.MethodPut, "/ignore-above-update/_doc/1", `{"k":"old"}`)
	mustDo(t, c, http.MethodPut, "/ignore-above-update/_mapping", `{"properties":{"k":{"type":"keyword","ignore_above":0}}}`)
	mustDo(t, c, http.MethodPut, "/ignore-above-update/_doc/2", `{"k":"new"}`)

	res := mustDo(t, c, http.MethodPost, "/ignore-above-update/_search", `{"query":{"term":{"k":"new"}}}`)
	hits := res["hits"].(map[string]any)["hits"].([]any)
	if len(hits) != 0 {
		t.Fatalf("updated ignore_above:0 indexed a nonempty value: %v", hits)
	}
	res = mustDo(t, c, http.MethodPost, "/ignore-above-update/_search", `{"query":{"term":{"k":"old"}}}`)
	hits = res["hits"].(map[string]any)["hits"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["_id"] != "1" {
		t.Fatalf("mapping update changed terms already indexed: %v", hits)
	}

	mapping := mustDo(t, c, http.MethodGet, "/ignore-above-update/_mapping", nil)
	props := mapping["ignore-above-update"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)
	if value := props["k"].(map[string]any)["ignore_above"]; value != float64(0) {
		t.Fatalf("mapping ignore_above after update = %v, want 0", value)
	}
}
