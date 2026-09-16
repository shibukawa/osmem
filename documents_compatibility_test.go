package osmem

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Expectations in this file were taken from OpenSearch 3.8.0 responses.

func errReason(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		r, _ := e["reason"].(string)
		return r
	}
	return ""
}

func causeType(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		if c, ok := e["caused_by"].(map[string]any); ok {
			t, _ := c["type"].(string)
			return t
		}
	}
	return ""
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func bulkItem(res map[string]any, i int) (string, map[string]any) {
	for k, v := range res["items"].([]any)[i].(map[string]any) {
		return k, v.(map[string]any)
	}
	return "", nil
}

func TestDocumentVersionsContinueAfterDelete(t *testing.T) {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	c := New(WithClock(func() time.Time { return now }))
	defer c.Close()

	mustDo(t, c, http.MethodPut, "/tomb/_doc/1", `{"a":1}`)
	mustDo(t, c, http.MethodPut, "/tomb/_doc/1", `{"a":2}`)
	if r := mustDo(t, c, http.MethodDelete, "/tomb/_doc/1", nil); num(r["_version"]) != 3 || num(r["_seq_no"]) != 2 {
		t.Fatalf("delete: %v", r)
	}
	// deleting a deleted document is not_found, but versions and consumes a sequence number
	st, body := status(t, c, http.MethodDelete, "/tomb/_doc/1", nil)
	if st != http.StatusNotFound || body["result"] != "not_found" || num(body["_version"]) != 4 || num(body["_seq_no"]) != 3 {
		t.Fatalf("delete again: %d %v", st, body)
	}
	st, body = status(t, c, http.MethodPut, "/tomb/_doc/1", `{"a":3}`)
	if st != http.StatusCreated || body["result"] != "created" || num(body["_version"]) != 5 || num(body["_seq_no"]) != 4 {
		t.Fatalf("index after delete: %d %v", st, body)
	}
	st, body = status(t, c, http.MethodDelete, "/tomb/_doc/2", nil)
	if st != http.StatusNotFound || num(body["_version"]) != 1 || num(body["_seq_no"]) != 5 {
		t.Fatalf("delete missing: %d %v", st, body)
	}
	if r := mustDo(t, c, http.MethodPut, "/tomb/_create/2", `{"a":1}`); num(r["_version"]) != 2 {
		t.Fatalf("create after not_found delete: %v", r)
	}

	// external versions are compared with the tombstone
	mustDo(t, c, http.MethodDelete, "/tomb/_doc/1?version=10&version_type=external", nil)
	st, body = status(t, c, http.MethodPut, "/tomb/_doc/1?version=9&version_type=external", `{"a":1}`)
	if st != http.StatusConflict || !strings.HasSuffix(errReason(body), "version conflict, current version [10] is higher or equal to the one provided [9]") {
		t.Fatalf("external version against tombstone: %d %v", st, body)
	}
	st, body = status(t, c, http.MethodDelete, "/tomb/_doc/1?version=10&version_type=external_gte", nil)
	if st != http.StatusNotFound || num(body["_version"]) != 10 {
		t.Fatalf("external_gte delete of a deleted document: %d %v", st, body)
	}
	st, body = status(t, c, http.MethodPut, "/tomb/_doc/1?if_seq_no=1&if_primary_term=1", `{"a":1}`)
	if st != http.StatusConflict || !strings.HasSuffix(errReason(body), "but no document was found") {
		t.Fatalf("compare-and-set against tombstone: %d %v", st, body)
	}

	// a clone inherits the tombstones; its deletes stay its own
	clone := c.Clone()
	defer clone.Close()
	if r := mustDo(t, clone, http.MethodPut, "/tomb/_doc/1", `{"a":1}`); num(r["_version"]) != 11 {
		t.Fatalf("clone index after delete: %v", r)
	}
	mustDo(t, clone, http.MethodDelete, "/tomb/_doc/2", nil)
	if r := mustDo(t, c, http.MethodPut, "/tomb/_doc/1", `{"a":1}`); num(r["_version"]) != 11 {
		t.Fatalf("base index after delete: %v", r)
	}
	if r := mustDo(t, clone, http.MethodPut, "/tomb/_doc/2", `{"a":1}`); num(r["_version"]) != 4 {
		t.Fatalf("clone index after its own delete: %v", r)
	}

	// tombstones expire after index.gc_deletes (60s)
	mustDo(t, c, http.MethodDelete, "/tomb/_doc/1", nil)
	now = now.Add(61 * time.Second)
	if r := mustDo(t, c, http.MethodPut, "/tomb/_doc/1", `{"a":1}`); num(r["_version"]) != 1 {
		t.Fatalf("index after gc_deletes: %v", r)
	}

	// bulk sequence of one id
	res := mustDo(t, c, http.MethodPost, "/_bulk", "{\"index\":{\"_index\":\"tomb-bulk\",\"_id\":\"1\"}}\n{\"a\":1}\n{\"index\":{\"_index\":\"tomb-bulk\",\"_id\":\"1\"}}\n{\"a\":2}\n{\"delete\":{\"_index\":\"tomb-bulk\",\"_id\":\"1\"}}\n{\"create\":{\"_index\":\"tomb-bulk\",\"_id\":\"1\"}}\n{\"a\":3}\n{\"update\":{\"_index\":\"tomb-bulk\",\"_id\":\"1\"}}\n{\"doc\":{\"b\":1}}\n{\"delete\":{\"_index\":\"tomb-bulk\",\"_id\":\"1\"}}\n{\"update\":{\"_index\":\"tomb-bulk\",\"_id\":\"1\"}}\n{\"doc\":{\"b\":1},\"doc_as_upsert\":true}\n")
	for i := 0; i < 7; i++ {
		if _, item := bulkItem(res, i); num(item["_version"]) != i+1 {
			t.Fatalf("bulk item %d: %v", i, item)
		}
	}
}

func TestDocumentWriteParameterValidation(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/params/_doc/1", `{"a":1}`)

	cases := []struct {
		method, path, body string
		status             int
		errType, reason    string
	}{
		{http.MethodPut, "/params/_doc/1?version=1", `{"a":1}`, 400, "action_request_validation_exception",
			"Validation Failed: 1: internal versioning can not be used for optimistic concurrency control. Please use `if_seq_no` and `if_primary_term` instead;"},
		{http.MethodDelete, "/params/_doc/1?version=1", "", 400, "action_request_validation_exception",
			"Validation Failed: 1: internal versioning can not be used for optimistic concurrency control. Please use `if_seq_no` and `if_primary_term` instead;"},
		{http.MethodPut, "/params/_doc/1?version_type=force&version=5", `{"a":1}`, 400, "illegal_argument_exception", "No version type match [force]"},
		{http.MethodPut, "/params/_doc/1?version=abc&version_type=external", `{"a":1}`, 400, "illegal_argument_exception", "Failed to parse long parameter [version] with value [abc]"},
		{http.MethodPut, "/params/_doc/1?version=-5&version_type=external", `{"a":1}`, 400, "action_request_validation_exception", "Validation Failed: 1: illegal version value [-5] for version type [EXTERNAL];"},
		{http.MethodPut, "/params/_doc/1?if_seq_no=-1&if_primary_term=1", `{"a":1}`, 400, "illegal_argument_exception", "sequence numbers must be non negative. got [-1]."},
		{http.MethodPut, "/params/_doc/1?if_seq_no=0", `{"a":1}`, 400, "action_request_validation_exception", "Validation Failed: 1: ifSeqNo is set, but primary term is [0];"},
		{http.MethodPut, "/params/_doc/3?op_type=invalid", `{"a":1}`, 400, "illegal_argument_exception", "opType must be 'create' or 'index', found: [invalid]"},
		{http.MethodPut, "/params/_create/3?op_type=index", `{"a":1}`, 400, "illegal_argument_exception", "opType must be 'create', found: [index]"},
		{http.MethodPost, "/params/_doc?version=3", `{"a":1}`, 400, "action_request_validation_exception", "Validation Failed: 1: create operations do not support explicit versions. use index instead;"},
		{http.MethodPut, "/params/_doc/1?refresh=invalid", `{"a":1}`, 400, "illegal_argument_exception", "Unknown value for refresh: [invalid]."},
		{http.MethodPut, "/params/_doc/1?timeout=abc", `{"a":1}`, 400, "illegal_argument_exception", "failed to parse setting [timeout] with value [abc] as a time value: unit is missing or unrecognized"},
		{http.MethodPut, "/params/_doc/1?wait_for_active_shards=abc", `{"a":1}`, 400, "illegal_argument_exception", "cannot parse ActiveShardCount[abc]"},
		{http.MethodPut, "/params/_doc/1?wait_for_active_shards=all&timeout=100ms", `{"a":1}`, 503, "unavailable_shards_exception",
			"[params][0] Not enough active copies to meet shard count of [ALL] (have 1, needed 2). Timeout: [100ms]"},
		{http.MethodPut, "/params/_doc/1", "", 400, "parse_exception", "request body is required"},
		{http.MethodPut, "/params/_doc/1", `[1,2]`, 400, "mapper_parsing_exception", "failed to parse"},
		{http.MethodPut, "/params/_doc/1", `{"a":1}{"b":2}`, 400, "mapper_parsing_exception", "failed to parse"},
		{http.MethodPut, "/params/_doc/1", `{"_id":"2"}`, 400, "mapper_parsing_exception",
			"failed to parse field [_id] of type [_id] in document with id '1'. Preview of field's value: '2'"},
		{http.MethodPut, "/params/_doc/1?pipeline=missing", `{"a":1}`, 400, "illegal_argument_exception", "pipeline with id [missing] does not exist"},
		{http.MethodPut, "/params/_doc/1?pipeline=", `{"a":1}`, 400, "action_request_validation_exception", "Validation Failed: 1: pipeline cannot be an empty string;"},
	}
	for _, tc := range cases {
		st, body := status(t, c, tc.method, tc.path, tc.body)
		if st != tc.status || errType(body) != tc.errType || errReason(body) != tc.reason {
			t.Errorf("%s %s: %d %v", tc.method, tc.path, st, body)
		}
	}
	if st, body := status(t, c, http.MethodPut, "/params/_doc/1", `{"_source":{"a":1}}`); st != 400 ||
		body["error"].(map[string]any)["root_cause"].([]any)[0].(map[string]any)["reason"] != "Field [_source] is a metadata field and cannot be added inside a document. Use the index API request parameters." {
		t.Errorf("metadata field root cause: %d %v", st, body)
	}
	if st, body := status(t, c, http.MethodPut, "/params/_doc/1", `{"a": }`); st != 400 || causeType(body) != "json_parse_exception" {
		t.Errorf("malformed source: %d %v", st, body)
	}
	if r := mustDo(t, c, http.MethodPut, "/params/_doc/2?op_type=CREATE", `{"a":1}`); r["result"] != "created" {
		t.Errorf("upper-case op_type: %v", r)
	}
	if st, _ := status(t, c, http.MethodHead, "/missing-pipeline-target", nil); st != 404 {
		t.Errorf("failed pipeline created index")
	}
	mustDo(t, c, http.MethodPut, "/default-pipeline", `{"settings":{"index.default_pipeline":"missing"}}`)
	if st, body := status(t, c, http.MethodPut, "/default-pipeline/_doc/1", `{"a":1}`); st != 400 || errReason(body) != "pipeline with id [missing] does not exist" {
		t.Errorf("default_pipeline: %d %v", st, body)
	}
	if r := mustDo(t, c, http.MethodPut, "/default-pipeline/_doc/1?pipeline=_none", `{"a":1}`); r["result"] != "created" {
		t.Errorf("pipeline _none: %v", r)
	}
}

func TestUpdateCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/upd/_doc/1", `{"z":1,"a":2,"o":{"y":1,"x":2}}`)
	mustDo(t, c, http.MethodPost, "/upd/_update/1", `{"doc":{"a":5,"m":3,"o":{"y":9,"b":1},"o.k":1}}`)
	res, _ := c.Do(http.MethodGet, "/upd/_source/1", nil)
	if got := string(res.Body); got != `{"z":1,"a":5,"o":{"y":9,"x":2,"b":1},"m":3,"o.k":1}` {
		t.Fatalf("merged source keeps key order and dotted keys: %s", got)
	}
	if r := mustDo(t, c, http.MethodPost, "/upd/_update/1", `{"doc":{"o":{"x":2}}}`); r["result"] != "noop" {
		t.Fatalf("noop: %v", r)
	}
	mustDo(t, c, http.MethodPost, "/upd-dots/_update/2", `{"doc":{"a.b":1},"doc_as_upsert":true}`)
	if res, _ := c.Do(http.MethodGet, "/upd-dots/_source/2", nil); string(res.Body) != `{"a.b":1}` {
		t.Fatalf("upserted dotted key: %s", res.Body)
	}

	cases := []struct {
		path, body      string
		status          int
		errType, reason string
	}{
		{"/upd/_update/1", `{"upsert":{"a":9}}`, 400, "action_request_validation_exception", "Validation Failed: 1: script or doc is missing;"},
		{"/upd/_update/1", `{"doc":{"a":1},"script":{"source":"x"}}`, 400, "action_request_validation_exception", "Validation Failed: 1: can't provide both script and doc;"},
		{"/upd/_update/1", `{"doc":{"a":1},"unknown":1}`, 400, "x_content_parse_exception", "[1:16] [UpdateRequest] unknown field [unknown]"},
		{"/upd/_update/1", `{"doc":1}`, 400, "x_content_parse_exception", "[1:8] [UpdateRequest] doc doesn't support values of type: VALUE_NUMBER"},
		{"/upd/_update/1", `{"doc":{"a":1},"doc_as_upsert":"yes"}`, 400, "x_content_parse_exception", "[1:32] [UpdateRequest] failed to parse field [doc_as_upsert]"},
		{"/upd/_update/1", `{"doc":{"b":2},"if_seq_no":99,"if_primary_term":1}`, 409, "version_conflict_engine_exception",
			"[1]: version conflict, required seqNo [99], primary term [1]. current document has seqNo [1] and primary term [1]"},
		{"/upd/_update/1?version=1", `{"doc":{"b":2}}`, 400, "action_request_validation_exception",
			"Validation Failed: 1: internal versioning can not be used for optimistic concurrency control. Please use `if_seq_no` and `if_primary_term` instead;"},
		{"/upd/_update/1?retry_on_conflict=abc", `{"doc":{"b":2}}`, 400, "illegal_argument_exception", "Failed to parse int parameter [retry_on_conflict] with value [abc]"},
		{"/upd/_update/1?retry_on_conflict=2&if_seq_no=1&if_primary_term=1", `{"doc":{"b":2}}`, 400, "action_request_validation_exception",
			"Validation Failed: 1: compare and write operations can not be retried;"},
		{"/upd/_update/9?if_seq_no=0&if_primary_term=1", `{"doc":{"a":1},"doc_as_upsert":true}`, 400, "action_request_validation_exception",
			"Validation Failed: 1: compare and write operations can not be used with upsert;"},
		{"/upd/_update/9?if_seq_no=0&if_primary_term=1", `{"doc":{"a":1},"upsert":{"a":0}}`, 400, "action_request_validation_exception",
			"Validation Failed: 1: upsert requests don't support `if_seq_no` and `if_primary_term`;"},
		{"/upd/_update/1", `{"doc":{"_id":"x"}}`, 400, "mapper_parsing_exception",
			"failed to parse field [_id] of type [_id] in document with id '1'. Preview of field's value: 'x'"},
	}
	for _, tc := range cases {
		st, body := status(t, c, http.MethodPost, tc.path, tc.body)
		if st != tc.status || errType(body) != tc.errType || errReason(body) != tc.reason {
			t.Errorf("POST %s %s: %d %v", tc.path, tc.body, st, body)
		}
	}

	// updates auto-create the index, also when the document is missing
	if st, body := status(t, c, http.MethodPost, "/upd-new/_update/1", `{"doc":{"a":1},"doc_as_upsert":true}`); st != http.StatusCreated {
		t.Fatalf("upsert into missing index: %d %v", st, body)
	}
	if st, body := status(t, c, http.MethodPost, "/upd-new2/_update/1", `{"doc":{"a":1}}`); st != http.StatusNotFound || errType(body) != "document_missing_exception" {
		t.Fatalf("update in missing index: %d %v", st, body)
	}
	if st, _ := status(t, c, http.MethodHead, "/upd-new2", nil); st != http.StatusOK {
		t.Fatalf("update did not create the index")
	}

	mustDo(t, c, http.MethodPut, "/upd-nosource", `{"mappings":{"_source":{"enabled":false}}}`)
	mustDo(t, c, http.MethodPut, "/upd-nosource/_doc/1", `{"a":1}`)
	if st, body := status(t, c, http.MethodPost, "/upd-nosource/_update/1", `{"doc":{"b":2}}`); st != 400 || errType(body) != "document_source_missing_exception" || errReason(body) != "[1]: document source missing" {
		t.Fatalf("update without source: %d %v", st, body)
	}
	if st, body := status(t, c, http.MethodGet, "/upd-nosource/_source/1", nil); st != 404 || errReason(body) != "Source not found [upd-nosource]/[1]" {
		t.Fatalf("_source without source: %d %v", st, body)
	}
}

func TestBulkItemCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/bk", `{"settings":{"number_of_replicas":2}}`)
	mustDo(t, c, http.MethodPut, "/bk/_doc/1", `{"a":1}`)

	res := mustDo(t, c, http.MethodPost, "/_bulk", "{\"index\":{\"_index\":\"bk\",\"_id\":\"1\",\"op_type\":\"create\"}}\n{\"a\":9}\n"+
		"{\"index\":{\"_index\":\"bk\",\"_id\":\"2\",\"require_alias\":true}}\n{\"a\":1}\n"+
		"{\"index\":{\"_index\":\"bk\",\"_id\":\"\"}}\n{\"a\":1}\n"+
		"{\"index\":{\"_index\":\"bk\",\"_id\":\"3\",\"pipeline\":\"missing\"}}\n{\"a\":1}\n"+
		"{\"update\":{\"_index\":\"bk\",\"_id\":\"1\"}}\n{\"doc\":{\"a\":1}}\n"+
		"{\"update\":{\"_index\":\"bk\",\"_id\":\"1\",\"_source\":true}}\n{\"doc\":{\"b\":1}}\n")
	if key, item := bulkItem(res, 0); key != "create" || num(item["status"]) != 409 {
		t.Errorf("op_type create in index metadata: %s %v", key, item)
	}
	if _, item := bulkItem(res, 1); num(item["status"]) != 404 || item["error"].(map[string]any)["reason"] != "no such index [bk] and [require_alias] request flag is [true] and [bk] is not an alias" {
		t.Errorf("require_alias item: %v", item)
	}
	if _, item := bulkItem(res, 2); num(item["status"]) != 400 || item["error"].(map[string]any)["reason"] != "if _id is specified it must not be empty" {
		t.Errorf("empty _id item: %v", item)
	}
	if _, item := bulkItem(res, 3); num(item["status"]) != 400 || item["error"].(map[string]any)["reason"] != "pipeline with id [missing] does not exist" {
		t.Errorf("pipeline item: %v", item)
	}
	if res["ingest_took"] == nil {
		t.Errorf("ingest_took missing: %v", res)
	}
	if _, item := bulkItem(res, 4); item["result"] != "noop" || num(item["_shards"].(map[string]any)["total"]) != 3 || num(item["_shards"].(map[string]any)["successful"]) != 1 {
		t.Errorf("noop update item: %v", item)
	}
	if _, item := bulkItem(res, 5); item["get"] == nil {
		t.Errorf("update item with _source: %v", item)
	}

	// ?pipeline on _bulk fails the index items, not the request.
	piped := mustDo(t, c, http.MethodPost, "/_bulk?pipeline=missing", "{\"index\":{\"_index\":\"bk\",\"_id\":\"7\"}}\n{\"a\":1}\n"+
		"{\"delete\":{\"_index\":\"bk\",\"_id\":\"7\"}}\n")
	if _, item := bulkItem(piped, 0); num(item["status"]) != 400 || item["error"].(map[string]any)["reason"] != "pipeline with id [missing] does not exist" {
		t.Errorf("?pipeline index item: %v", piped)
	}
	if _, item := bulkItem(piped, 1); num(item["status"]) != 404 || piped["ingest_took"] == nil {
		t.Errorf("?pipeline delete item: %v", piped)
	}

	requestErrors := []struct{ body, errType, reason string }{
		{"{\"foo\":{\"_index\":\"bk\"}}\n{}\n", "illegal_argument_exception", "Malformed action/metadata line [1], expected one of [create, delete, index, update] but found [foo]"},
		{"{\"index\":{\"_index\":\"bk\",\"_require_alias\":true}}\n{}\n", "illegal_argument_exception", "Action/metadata line [1] contains an unknown parameter [_require_alias]"},
		{"[1]\n{}\n", "illegal_argument_exception", "Malformed action/metadata line [1], expected START_OBJECT but found [START_ARRAY]"},
		{"{}\n{}\n", "illegal_argument_exception", "Malformed action/metadata line [1], expected FIELD_NAME but found [END_OBJECT]"},
		{"{\"index\":{\"_index\":\"bk\",\"_id\":[\"x\"]}}\n{}\n", "illegal_argument_exception", "Malformed action/metadata line [1], expected a simple value for field [_id] but found [START_ARRAY]"},
		{"{\"delete\":{\"_index\":\"bk\",\"_id\":\"\"}}\n{\"update\":{\"_index\":\"bk\"}}\n{\"doc\":{}}\n", "action_request_validation_exception", "Validation Failed: 1: id is missing;2: id is missing;"},
		{"{\"index\":{\"_index\":\"bk\"}}\n", "action_request_validation_exception", "Validation Failed: 1: no requests added;"},
		{"{\"delete\":{\"_index\":\"bk\",\"_id\":\"1\",\"version\":4}}\n", "action_request_validation_exception",
			"Validation Failed: 1: internal versioning can not be used for optimistic concurrency control. Please use `if_seq_no` and `if_primary_term` instead;"},
		{"{\"update\":{\"_index\":\"bk\",\"_id\":\"1\",\"retry_on_conflict\":\"abc\"}}\n{\"doc\":{}}\n", "number_format_exception", "For input string: \"abc\""},
		{"{\"update\":{\"_index\":\"bk\",\"_id\":\"1\"}}\n{\"doc\": {\"a\": 5}, \"foo\": 1}\n", "x_content_parse_exception", "[1:19] [UpdateRequest] unknown field [foo]"},
	}
	for _, tc := range requestErrors {
		st, body := status(t, c, http.MethodPost, "/_bulk", tc.body)
		if st != 400 || errType(body) != tc.errType || errReason(body) != tc.reason {
			t.Errorf("bulk %q: %d %v", tc.body, st, body)
		}
	}
	// OpenSearch reads only the first action of a line and ignores a
	// trailing action without a source
	res = mustDo(t, c, http.MethodPost, "/_bulk", "{\"index\": {\"_index\": \"bk\", \"_id\": \"7\"}\n{}\n{\"index\":{\"_index\":\"bk\",\"_id\":\"8\"}}\n")
	if items := res["items"].([]any); len(items) != 1 || res["errors"] != false {
		t.Errorf("lenient action lines: %v", res)
	}
	if res := mustDo(t, c, http.MethodPost, "/_bulk?require_alias=true", "{\"delete\":{\"_index\":\"bk\",\"_id\":\"7\"}}\n"); res["errors"] != false {
		t.Errorf("require_alias does not apply to deletes: %v", res)
	}
}

func TestGetAndMultiGetCompatibility(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/g1", `{"aliases":{"both":{}},"mappings":{"properties":{"d":{"type":"date","store":true},"g":{"type":"geo_point","store":true},"ip":{"type":"ip","store":true},"i":{"type":"integer","store":true}}}}`)
	mustDo(t, c, http.MethodPut, "/g2", `{"aliases":{"both":{}}}`)
	mustDo(t, c, http.MethodPut, "/g1/_doc/1?routing=a", `{"d":"2024-01-15","g":{"lat":1.5,"lon":2.25},"ip":"::ffff:1.2.3.4","i":7.9}`)

	if r := mustDo(t, c, http.MethodGet, "/g1/_doc/1", nil); r["_routing"] != "a" {
		t.Errorf("_routing in get: %v", r)
	}
	if r := mustDo(t, c, http.MethodGet, "/g1/_doc/1?_source=false", nil); r["_routing"] != nil {
		t.Errorf("_routing without loaded fields: %v", r)
	}
	hits := mustDo(t, c, http.MethodPost, "/g1/_search", `{}`)["hits"].(map[string]any)["hits"].([]any)
	if hits[0].(map[string]any)["_routing"] != "a" {
		t.Errorf("_routing in search hit: %v", hits)
	}
	docs := mustDo(t, c, http.MethodPost, "/_mget", `{"docs":[{"_index":"g1","_id":"1"},{"_index":"both","_id":"1"}]}`)["docs"].([]any)
	if docs[0].(map[string]any)["_routing"] != "a" || errReason(docs[1].(map[string]any)) != "alias [both] has more than one index associated with it [g1, g2], can't execute a single index op" {
		t.Errorf("mget: %v", docs)
	}
	if st, body := status(t, c, http.MethodGet, "/both/_doc/1", nil); st != 400 || errReason(body) != "alias [both] has more than one index associated with it [g1, g2], can't execute a single index op" {
		t.Errorf("get through alias with two indices: %d %v", st, body)
	}
	if st, _ := status(t, c, http.MethodHead, "/both/_doc/1", nil); st != 400 {
		t.Errorf("head through alias with two indices: %d", st)
	}

	fields := mustDo(t, c, http.MethodGet, "/g1/_doc/1?stored_fields=d,g,ip,i", nil)["fields"].(map[string]any)
	if fields["d"].([]any)[0] != "2024-01-15T00:00:00.000Z" || fields["g"].([]any)[0] != "1.5, 2.25" || fields["ip"].([]any)[0] != "1.2.3.4" || fields["i"].([]any)[0] != 7.0 {
		t.Errorf("stored field formats: %v", fields)
	}
	if r := mustDo(t, c, http.MethodGet, "/g1/_doc/1?stored_fields=*", nil); r["fields"] != nil || r["_source"] != nil {
		t.Errorf("stored_fields=* on get: %v", r)
	}
	hit := mustDo(t, c, http.MethodPost, "/g1/_search", `{"stored_fields":["g","ip"]}`)["hits"].(map[string]any)["hits"].([]any)[0].(map[string]any)
	if f := hit["fields"].(map[string]any); f["g"].([]any)[0] != "1.5, 2.25" || f["ip"].([]any)[0] != "1.2.3.4" {
		t.Errorf("stored fields in search: %v", hit)
	}

	cases := []struct {
		method, path, body string
		status             int
		errType, reason    string
	}{
		{http.MethodGet, "/g1/_doc/1?routing=a&version=2", "", 409, "version_conflict_engine_exception", "[1]: version conflict, current version [1] is different than the one provided [2]"},
		{http.MethodGet, "/g1/_doc/1?version=abc", "", 400, "illegal_argument_exception", "Failed to parse long parameter [version] with value [abc]"},
		{http.MethodGet, "/g1/_doc/1?version=-5", "", 400, "action_request_validation_exception", "Validation Failed: 1: illegal version value [-5] for version type [INTERNAL];"},
		{http.MethodGet, "/g1/_doc/1?realtime=abc", "", 400, "illegal_argument_exception", "Failed to parse value [abc] as only [true] or [false] are allowed."},
		{http.MethodGet, "/g1/_source/1?_source=false", "", 400, "action_request_validation_exception", "Validation Failed: 1: fetching source can not be disabled;"},
		{http.MethodGet, "/_all/_doc/1", "", 404, "index_not_found_exception", "no such index [_all] and no indices exist"},
		{http.MethodPost, "/g1/_mget", `{"ids":[null]}`, 400, "illegal_argument_exception", "ids array element should only contain ids"},
		{http.MethodPost, "/g1/_mget", `{"docs":[{"_id":null}]}`, 400, "action_request_validation_exception", "Validation Failed: 1: id is missing for doc 0;"},
		{http.MethodPost, "/g1/_mget", `{"docs":["1"]}`, 400, "illegal_argument_exception", "docs array element should include an object"},
		{http.MethodPost, "/g1/_mget", `{"docs":[{"_id":"1","_type":"_doc"}]}`, 400, "parse_exception", "failed to parse multi get request. unknown field [_type]"},
		{http.MethodPost, "/g1/_count", `{"query":{"match_all":{}},"size":1}`, 400, "parsing_exception", "request does not support [size]"},
	}
	for _, tc := range cases {
		st, body := status(t, c, tc.method, tc.path, tc.body)
		if st != tc.status || errType(body) != tc.errType || errReason(body) != tc.reason {
			t.Errorf("%s %s: %d %v", tc.method, tc.path, st, body)
		}
	}
	docs = mustDo(t, c, http.MethodPost, "/g1/_mget", `{"docs":[{"_id":"1","version":2},{"_id":1}]}`)["docs"].([]any)
	if errType(docs[0].(map[string]any)) != "version_conflict_engine_exception" || docs[1].(map[string]any)["found"] != true {
		t.Errorf("mget versions and numeric ids: %v", docs)
	}
}

func TestByQueryAndReindexValidation(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/src", `{"mappings":{"properties":{"n":{"type":"long"}}}}`)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		mustDo(t, c, http.MethodPut, "/src/_doc/"+id, `{"n":`+id+`}`)
	}

	cases := []struct {
		path, body      string
		status          int
		errType, reason string
	}{
		{"/src/_delete_by_query", ``, 400, "action_request_validation_exception", "Validation Failed: 1: query is missing;"},
		{"/src/_delete_by_query", `{"max_docs":1}`, 400, "action_request_validation_exception", "Validation Failed: 1: query is missing;"},
		{"/src/_delete_by_query?conflicts=foo", `{"query":{"match_all":{}}}`, 400, "illegal_argument_exception", "conflicts may only be \"proceed\" or \"abort\" but was [foo]"},
		{"/src/_update_by_query", `{"conflicts":"foo"}`, 400, "illegal_argument_exception", "conflicts may only be \"proceed\" or \"abort\" but was [foo]"},
		{"/src/_delete_by_query", `{"query":{"match_all":{}},"from":1}`, 400, "action_request_validation_exception",
			"Validation Failed: 1: using [from] is not allowed in a scroll context;2: from is not supported in this context;"},
		{"/src/_delete_by_query?slices=0", `{"query":{"match_all":{}}}`, 400, "illegal_argument_exception", "[slices] must be a positive integer or the string \"auto\", but was [0]"},
		{"/_reindex", `{"source":{},"dest":{"index":"dx"}}`, 400, "action_request_validation_exception", "Validation Failed: 1: use _all if you really want to copy from all existing indexes;"},
		{"/_reindex", `{"source":{"index":"src","remote":{"host":"http://127.0.0.1:9200"}},"dest":{"index":"dr"}}`, 400, "illegal_argument_exception", "[127.0.0.1:9200] not allowlisted in reindex.remote.allowlist"},
		{"/_reindex", `{"source":{"index":"src"},"dest":{"index":"src"}}`, 400, "action_request_validation_exception", "Validation Failed: 1: reindex cannot write into an index its reading from [src];"},
		{"/_reindex", `{"source":{"index":"src"},"dest":{}}`, 400, "action_request_validation_exception", "Validation Failed: 1: index must be specified;"},
		{"/_reindex", `{"source":{"index":"src"},"dest":{"index":"dx"},"foo":1}`, 400, "x_content_parse_exception", "[1:49] [reindex] unknown field [foo]"},
		{"/_reindex", `{"source":{"index":"src","_source":false},"dest":{"index":"dx"}}`, 400, "action_request_validation_exception", "Validation Failed: 1: _source:false is not supported in this context;"},
	}
	for _, tc := range cases {
		st, body := status(t, c, http.MethodPost, tc.path, tc.body)
		if st != tc.status || errType(body) != tc.errType || errReason(body) != tc.reason {
			t.Errorf("POST %s %s: %d %v", tc.path, tc.body, st, body)
		}
	}
	if n, _ := c.Count("src", nil); n != 5 {
		t.Fatalf("rejected by-query request changed documents: %d", n)
	}
	for _, name := range []string{"dx", "dr"} {
		if st, _ := status(t, c, http.MethodHead, "/"+name, nil); st != 404 {
			t.Errorf("rejected reindex created %s", name)
		}
	}

	r := mustDo(t, c, http.MethodPost, "/src/_update_by_query", `{"query":{"match_all":{}},"size":2}`)
	if num(r["updated"]) != 5 || num(r["batches"]) != 3 || r["created"] != nil {
		t.Errorf("update_by_query batches: %v", r)
	}
	r = mustDo(t, c, http.MethodPost, "/src/_delete_by_query", `{"query":{"match_none":{}}}`)
	if num(r["batches"]) != 0 || r["updated"] != nil {
		t.Errorf("delete_by_query without matches: %v", r)
	}
	r = mustDo(t, c, http.MethodPost, "/_reindex", `{"source":{"index":"src","query":{"match_none":{}}},"dest":{"index":"never"}}`)
	if num(r["batches"]) != 0 || r["deleted"] != 0.0 {
		t.Errorf("reindex without matches: %v", r)
	}
	if st, _ := status(t, c, http.MethodHead, "/never", nil); st != 404 {
		t.Errorf("reindex without matches created the destination")
	}
	r = mustDo(t, c, http.MethodPost, "/_reindex", `{"size":2,"source":{"index":"src","size":1,"sort":[{"n":"desc"}]},"dest":{"index":"top2"}}`)
	if num(r["created"]) != 2 || num(r["batches"]) != 2 {
		t.Errorf("reindex size and batches: %v", r)
	}
	if st, _ := status(t, c, http.MethodHead, "/top2/_doc/5", nil); st != 200 {
		t.Errorf("reindex sort before max_docs")
	}
	r = mustDo(t, c, http.MethodPost, "/_reindex", `{"source":{"index":"src","slice":{"id":0,"max":2}},"dest":{"index":"sliced"}}`)
	if num(r["created"]) != 3 || num(r["slice_id"]) != 0 {
		t.Errorf("manual slice: %v", r)
	}
	st, body := status(t, c, http.MethodPost, "/_reindex", `{"source":{"index":"src"},"dest":{"index":"piped","pipeline":"missing"}}`)
	if st != 400 || len(body["failures"].([]any)) != 5 || num(body["created"]) != 0 {
		t.Errorf("reindex with a missing pipeline: %d %v", st, body)
	}
	r = mustDo(t, c, http.MethodPost, "/src/_delete_by_query?wait_for_completion=false", `{"query":{"term":{"n":1}}}`)
	if task, _ := r["task"].(string); !strings.HasPrefix(task, "osmem-node:") {
		t.Errorf("wait_for_completion=false: %v", r)
	}
	r = mustDo(t, c, http.MethodPost, "/src/_delete_by_query", `{"query":{"match_all":{}},"max_docs":2}`)
	if num(r["deleted"]) != 2 {
		t.Errorf("delete_by_query max_docs in body: %v", r)
	}
}

func TestAliasWithoutWriteIndexRejectsDocumentWrites(t *testing.T) {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	c := New(WithClock(func() time.Time { return now }))
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/am1", `{"aliases":{"al":{}}}`)
	mustDo(t, c, http.MethodPut, "/am2", `{"aliases":{"al":{}}}`)
	mustDo(t, c, http.MethodPut, "/am1/_doc/1", `{"a":1}`)
	noWrite := "no write index is defined for alias [al]. The write index may be explicitly disabled using is_write_index=false or the alias points to multiple indices without one being designated as a write index"
	for _, w := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/al/_doc/2", `{"a":1}`},
		{http.MethodPost, "/al/_update/1", `{"doc":{"a":2}}`},
		{http.MethodPost, "/al/_update/3", `{"doc":{"a":2},"doc_as_upsert":true}`},
		{http.MethodDelete, "/al/_doc/1", nil},
	} {
		if st, body := status(t, c, w.method, w.path, w.body); st != 400 || errReason(body) != noWrite {
			t.Errorf("%s %s: %d %v", w.method, w.path, st, body)
		}
	}
	res := mustDo(t, c, http.MethodPost, "/_bulk", "{\"index\":{\"_index\":\"al\",\"_id\":\"4\"}}\n{\"a\":1}\n")
	if _, item := bulkItem(res, 0); num(item["status"]) != 400 || item["error"].(map[string]any)["reason"] != noWrite {
		t.Errorf("bulk item through alias: %v", res)
	}
	if st, body := status(t, c, http.MethodGet, "/al/_doc/1", nil); st != 400 || errReason(body) != "alias [al] has more than one index associated with it [am1, am2], can't execute a single index op" {
		t.Errorf("alias shadowed by an auto-created index: %d %v", st, body)
	}
}
