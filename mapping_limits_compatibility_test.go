package osmem

import (
	"net/http"
	"strings"
	"testing"
)

func TestMappingDepthLimitOnCreateAndUpdate(t *testing.T) {
	c := New()
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/depth-boundary", `{"settings":{"index.mapping.depth.limit":2},"mappings":{"properties":{"obj":{"properties":{"leaf":{"type":"keyword"}}}}}}`)
	st, body := status(t, c, http.MethodPut, "/depth-too-deep", `{"settings":{"index.mapping.depth.limit":2},"mappings":{"properties":{"obj":{"properties":{"sub":{"properties":{"leaf":{"type":"keyword"}}}}}}}}`)
	assertMappingLimitError(t, st, body, "depth")

	mustDo(t, c, http.MethodPut, "/depth-update", `{"settings":{"index.mapping.depth.limit":2},"mappings":{"properties":{"obj":{"properties":{"leaf":{"type":"keyword"}}}}}}`)
	st, body = status(t, c, http.MethodPut, "/depth-update/_mapping", `{"properties":{"obj":{"properties":{"sub":{"properties":{"leaf":{"type":"keyword"}}}}}}}`)
	assertMappingLimitError(t, st, body, "depth")
	assertMappingHasOnly(t, c, "/depth-update/_mapping", "obj", "obj.leaf")
}

func TestTotalFieldsLimitOnStaticCreateAndUpdate(t *testing.T) {
	c := New()
	defer c.Close()

	st, body := status(t, c, http.MethodPut, "/static-fields-too-many", `{"settings":{"index.mapping.total_fields.limit":1},"mappings":{"properties":{"first":{"type":"keyword"},"second":{"type":"long"}}}}`)
	assertMappingLimitError(t, st, body, "total fields")
	if statusCode, _ := status(t, c, http.MethodHead, "/static-fields-too-many", nil); statusCode != http.StatusNotFound {
		t.Fatalf("failed static mapping create left an index behind: HEAD status=%d", statusCode)
	}

	mustDo(t, c, http.MethodPut, "/static-fields-update", `{"settings":{"index.mapping.total_fields.limit":1},"mappings":{"properties":{"first":{"type":"keyword"}}}}`)
	st, body = status(t, c, http.MethodPut, "/static-fields-update/_mapping", `{"properties":{"second":{"type":"long"}}}`)
	assertMappingLimitError(t, st, body, "total fields")
	assertMappingHasOnly(t, c, "/static-fields-update/_mapping", "first")
}

func TestMappingDepthLimitOnDynamicInference(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/dynamic-depth", `{"settings":{"index.mapping.depth.limit":2}}`)

	st, body := status(t, c, http.MethodPut, "/dynamic-depth/_doc/1", `{"obj":{"sub":{"leaf":"x"}}}`)
	// OpenSearch rejects the document while parsing it: "failed to parse"
	// caused by the depth limit
	if st != http.StatusBadRequest || errType(body) != "mapper_parsing_exception" {
		t.Fatalf("status=%d body=%v", st, body)
	}
	cause, _ := body["error"].(map[string]any)["caused_by"].(map[string]any)
	if cause["type"] != "parse_exception" || cause["reason"] != "The depth of the field has exceeded the allowed limit of [2]. This limit can be set by changing the [index.mapping.depth.limit] index level setting." {
		t.Fatalf("depth cause: %v", body)
	}
	assertMappingHasNoProperties(t, c, "/dynamic-depth/_mapping")
}

func TestNestedFieldsLimitOnCreateAndUpdate(t *testing.T) {
	c := New()
	defer c.Close()

	nestedOne := `{"properties":{"first":{"type":"nested","properties":{"v":{"type":"keyword"}}}}}`
	createOne := `{"settings":{"index.mapping.nested_fields.limit":1},"mappings":` + nestedOne + `}`
	mustDo(t, c, http.MethodPut, "/nested-boundary", createOne)

	nestedTwo := `{"properties":{"first":{"type":"nested"},"second":{"type":"nested"}}}`
	createTwo := `{"settings":{"index.mapping.nested_fields.limit":1},"mappings":` + nestedTwo + `}`
	st, body := status(t, c, http.MethodPut, "/nested-too-many", createTwo)
	assertMappingLimitError(t, st, body, "nested fields")

	st, body = status(t, c, http.MethodPut, "/nested-boundary/_mapping", `{"properties":{"second":{"type":"nested"}}}`)
	assertMappingLimitError(t, st, body, "nested fields")
	assertMappingHasOnly(t, c, "/nested-boundary/_mapping", "first", "first.v")
}

func TestNestedFieldsLimitOnDynamicTemplateInference(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/dynamic-nested-limit", `{"settings":{"index.mapping.nested_fields.limit":1},"mappings":{"dynamic_templates":[{"objects_as_nested":{"match_mapping_type":"object","mapping":{"type":"nested"}}}]}}`)

	st, body := status(t, c, http.MethodPut, "/dynamic-nested-limit/_doc/1", `{"first":{"v":"a"},"second":{"v":"b"}}`)
	assertMappingLimitError(t, st, body, "nested fields")
	assertMappingHasNoProperties(t, c, "/dynamic-nested-limit/_mapping")
}

func assertMappingLimitError(t *testing.T, statusCode int, body map[string]any, reason string) {
	t.Helper()
	// OpenSearch reports exceeded mapping limits as illegal_argument_exception
	if statusCode != http.StatusBadRequest || errType(body) != "illegal_argument_exception" {
		t.Fatalf("status=%d body=%v, want illegal_argument_exception", statusCode, body)
	}
	err, _ := body["error"].(map[string]any)
	actual, _ := err["reason"].(string)
	if !strings.Contains(strings.ToLower(actual), strings.ToLower(reason)) {
		t.Fatalf("error reason %q does not mention %q", actual, reason)
	}
}

func assertMappingHasNoProperties(t *testing.T, c *Cluster, path string) {
	t.Helper()
	response := mustDo(t, c, http.MethodGet, path, nil)
	for _, raw := range response {
		index, _ := raw.(map[string]any)
		mapping, _ := index["mappings"].(map[string]any)
		if props, exists := mapping["properties"]; exists && len(props.(map[string]any)) != 0 {
			t.Fatalf("failed inference left mapped properties behind: %v", props)
		}
	}
}

func assertMappingHasOnly(t *testing.T, c *Cluster, path string, wantPaths ...string) {
	t.Helper()
	response := mustDo(t, c, http.MethodGet, path, nil)
	for _, raw := range response {
		index, _ := raw.(map[string]any)
		mapping, _ := index["mappings"].(map[string]any)
		props, _ := mapping["properties"].(map[string]any)
		got := map[string]bool{}
		var walk func(map[string]any, string)
		walk = func(fields map[string]any, prefix string) {
			for name, rawField := range fields {
				path := prefix + name
				got[path] = true
				field, _ := rawField.(map[string]any)
				children, _ := field["properties"].(map[string]any)
				walk(children, path+".")
			}
		}
		walk(props, "")
		if len(got) != len(wantPaths) {
			t.Fatalf("mapping paths = %v, want %v", got, wantPaths)
		}
		for _, path := range wantPaths {
			if !got[path] {
				t.Fatalf("mapping paths = %v, want %v", got, wantPaths)
			}
		}
	}
}
