package osmem

import (
	"net/http"
	"testing"
)

// The expectations in this file were captured from OpenSearch 3.8.0.

func fieldTypesError(t *testing.T, body map[string]any) (reason string, cause map[string]any) {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object: %v", body)
	}
	reason, _ = e["reason"].(string)
	cause, _ = e["caused_by"].(map[string]any)
	return reason, cause
}

func TestFieldTypesUnknownMappingParameter(t *testing.T) {
	c := New()
	defer c.Close()
	code, body := status(t, c, http.MethodPut, "/unknown-param", `{"mappings":{"properties":{"bad":{"type":"keyword","unknown_param":1}}}}`)
	reason, cause := fieldTypesError(t, body)
	if code != http.StatusBadRequest || errType(body) != "mapper_parsing_exception" ||
		reason != "Failed to parse mapping [_doc]: unknown parameter [unknown_param] on mapper [bad] of type [keyword]" ||
		cause["type"] != "mapper_parsing_exception" || cause["reason"] != "unknown parameter [unknown_param] on mapper [bad] of type [keyword]" {
		t.Fatalf("status=%d body=%v", code, body)
	}
}

func TestFieldTypesNumericValueErrors(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/numeric-errors", `{"mappings":{"properties":{"i":{"type":"integer"}}}}`)

	code, body := status(t, c, http.MethodPut, "/numeric-errors/_doc/2", `{"i":2147483648}`)
	reason, cause := fieldTypesError(t, body)
	if code != http.StatusBadRequest || errType(body) != "mapper_parsing_exception" ||
		reason != "failed to parse field [i] of type [integer] in document with id '2'. Preview of field's value: '2147483648'" ||
		cause["type"] != "input_coercion_exception" {
		t.Fatalf("out of range integer: status=%d body=%v", code, body)
	}

	code, body = status(t, c, http.MethodPut, "/numeric-errors/_doc/3", `{"i":"12abc"}`)
	reason, cause = fieldTypesError(t, body)
	if code != http.StatusBadRequest || reason != "failed to parse field [i] of type [integer] in document with id '3'. Preview of field's value: '12abc'" ||
		cause["type"] != "number_format_exception" || cause["reason"] != `For input string: "12abc"` {
		t.Fatalf("malformed integer: status=%d body=%v", code, body)
	}
}

func TestFieldTypesDateNanosAndIPValues(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/typed-values", `{"mappings":{"properties":{"d":{"type":"date"},"n":{"type":"date_nanos"},"ip":{"type":"ip"}}}}`)
	mustDo(t, c, http.MethodPut, "/typed-values/_doc/1?refresh=true", `{"d":"3000-01-01T00:00:00Z","n":"2024-01-01T00:00:00.123456789Z","ip":"::ffff:1.2.3.4"}`)

	// dates after 2262, nanosecond terms and CIDR terms
	res := mustDo(t, c, http.MethodPost, "/typed-values/_search", `{"query":{"bool":{"filter":[
		{"range":{"d":{"gte":"2999-12-31"}}},
		{"term":{"n":"2024-01-01T00:00:00.123456789Z"}},
		{"term":{"ip":"1.2.3.0/24"}}]}},
		"docvalue_fields":["d","n","ip"],"_source":false}`)
	hits, _ := res["hits"].(map[string]any)["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("hits: %v", res["hits"])
	}
	fields, _ := hits[0].(map[string]any)["fields"].(map[string]any)
	want := map[string]string{"d": "3000-01-01T00:00:00.000Z", "n": "2024-01-01T00:00:00.123Z", "ip": "1.2.3.4"}
	for name, value := range want {
		if got, _ := fields[name].([]any); len(got) != 1 || got[0] != value {
			t.Fatalf("docvalue_fields %s = %v, want %s", name, fields[name], value)
		}
	}

	res = mustDo(t, c, http.MethodPost, "/typed-values/_search", `{"query":{"term":{"n":"2024-01-01T00:00:00.123456788Z"}}}`)
	if total := res["hits"].(map[string]any)["total"].(map[string]any)["value"]; total != 0.0 {
		t.Fatalf("date_nanos term must compare nanoseconds: total=%v", total)
	}
}

func TestDynamicMappingDateDetection(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/dynamic-dates/_doc/1", `{"slash":"2024/01/02","iso":"2024-01-02","num":"123"}`)
	mapping := mustDo(t, c, http.MethodGet, "/dynamic-dates/_mapping", nil)
	props, _ := mapping["dynamic-dates"].(map[string]any)["mappings"].(map[string]any)["properties"].(map[string]any)
	slash, _ := props["slash"].(map[string]any)
	iso, _ := props["iso"].(map[string]any)
	num, _ := props["num"].(map[string]any)
	if len(slash) != 3 || slash["type"] != "date" || slash["format"] != "yyyy/MM/dd HH:mm:ss||yyyy/MM/dd||epoch_millis" || slash["print_format"] != "yyyy/MM/dd HH:mm:ss" {
		t.Fatalf("slash date mapping: %v", slash)
	}
	if len(iso) != 1 || iso["type"] != "date" {
		t.Fatalf("iso date mapping: %v", iso)
	}
	if num["type"] != "text" || num["fields"] == nil {
		t.Fatalf("numeric strings stay text without numeric_detection: %v", num)
	}
}
