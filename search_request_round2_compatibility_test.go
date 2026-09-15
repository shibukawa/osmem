package osmem

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The expectations of this file were recorded from OpenSearch 3.8.0.

func sr2Books(t *testing.T, c *Cluster, index string) {
	t.Helper()
	mustDo(t, c, http.MethodPut, "/"+index, `{"mappings":{"properties":{
		"title":{"type":"text","fields":{"keyword":{"type":"keyword"}}},"author":{"type":"keyword"},"year":{"type":"integer"},
		"rating":{"type":"double"},"description":{"type":"text"},"location":{"type":"geo_point"}}}}`)
	docs := []struct{ id, src string }{
		{"10", `{"title":"The Quick Brown Fox","author":"alice","year":2001,"rating":4.5,"description":"A quick brown fox jumps over the lazy dog","location":{"lat":35.68,"lon":139.76}}`},
		{"2", `{"title":"Lazy Dogs and Foxes","author":"bob","year":1999,"rating":3.9,"description":"Dogs are lazy, foxes are quick","location":"34.05,-118.24"}`},
		{"1", `{"title":"Search Engines in Action","author":"carol","year":2015,"rating":4.8,"description":"Inverted indices, BM25 and quick searches","location":[2.35,48.85]}`},
		{"abc", `{"title":"Cooking for Programmers","author":"alice","year":2015,"description":"Recipes with a quick twist"}`},
		{"x-5", `{"title":"Untitled"}`},
	}
	var sb strings.Builder
	for _, d := range docs {
		fmt.Fprintf(&sb, "{\"index\":{\"_index\":%q,\"_id\":%q}}\n%s\n", index, d.id, d.src)
	}
	if err := c.BulkString(sb.String()); err != nil {
		t.Fatal(err)
	}
}

// sr2Location is the location Jackson appends to its messages: the byte
// offset just past the value that follows key in body.
func sr2Location(body, key, value string) string {
	offset := strings.Index(body, `"`+key+`":`+value) + len(key) + 3 + len(value)
	return fmt.Sprintf("\n at [Source: REDACTED (`StreamReadFeature.INCLUDE_SOURCE_IN_LOCATION` disabled); byte offset: #%d]", offset)
}

func TestSearchRequestRound2BodyValidation(t *testing.T) {
	c := New()
	defer c.Close()
	sr2Books(t, c, "books")
	cases := []struct {
		body   string
		status int
		typ    string
		reason string
	}{
		{`{"timeout":"abc","size":0}`, 400, "illegal_argument_exception", "failed to parse setting [timeout] with value [abc] as a time value: unit is missing or unrecognized"},
		{`{"timeout":5,"size":0}`, 400, "illegal_argument_exception", "failed to parse setting [timeout] with value [5] as a time value: unit is missing or unrecognized"},
		{`{"timeout":"-2s"}`, 400, "illegal_argument_exception", "failed to parse setting [timeout] with value [-2s] as a time value: negative durations are not supported"},
		{`{"timeout":null}`, 400, "parsing_exception", "Unknown key for a VALUE_NULL in [timeout]."},
		{`{"stats":"g1"}`, 400, "parsing_exception", "Unknown key for a VALUE_STRING in [stats]."},
		{`{"stats":["g",1]}`, 400, "parsing_exception", "Expected [VALUE_STRING] in [stats] but found [VALUE_NUMBER]"},
		{`{"runtime_mappings":{"x":{"type":"keyword"}}}`, 400, "parsing_exception", "Unknown key for a START_OBJECT in [runtime_mappings]."},
		{`{"verbose_pipeline":true}`, 400, "illegal_argument_exception", "The 'verbose pipeline' option requires a search pipeline to be defined."},
		{`{"search_pipeline":"nope"}`, 400, "illegal_argument_exception", "Pipeline nope is not defined"},
		{`{"search_pipeline":{"foo":1}}`, 400, "parse_exception", "pipeline [_ad_hoc_pipeline] doesn't support one or more provided configuration parameters [foo]"},
		{`{"explain":"abc"}`, 400, "illegal_argument_exception", "Failed to parse value [abc] as only [true] or [false] are allowed."},
		{`{"profile":"TRUE"}`, 400, "illegal_argument_exception", "Failed to parse value [TRUE] as only [true] or [false] are allowed."},
		{`{"track_scores":[true]}`, 400, "parsing_exception", "Unknown key for a START_ARRAY in [track_scores]."},
		{`{"from":"abc"}`, 400, "number_format_exception", `For input string: "abc"`},
		{`{"size":""}`, 400, "number_format_exception", "empty String"},
		{`{"size":"3000000000"}`, 400, "illegal_argument_exception", "Value [3000000000] is out of range for an integer"},
		{`{"size":null}`, 400, "parsing_exception", "Unknown key for a VALUE_NULL in [size]."},
		{`{"min_score":"abc"}`, 400, "number_format_exception", `For input string: "abc"`},
		{`{"track_total_hits":"abc"}`, 400, "number_format_exception", `For input string: "abc"`},
		{`{"_source":5}`, 400, "parsing_exception", "Expected one of [VALUE_BOOLEAN, VALUE_STRING, START_ARRAY, START_OBJECT] but found [VALUE_NUMBER]"},
		{`{"_source":{"includes":[5]}}`, 400, "parsing_exception", "Unknown key for a VALUE_NUMBER in [null]."},
		{`{"_source":{"enabled":false}}`, 400, "parsing_exception", "Unknown key for a VALUE_BOOLEAN in [enabled]."},
		{`{"stored_fields":true}`, 400, "parsing_exception", "Expected [VALUE_STRING] or [START_ARRAY] in [stored_fields] but found [VALUE_BOOLEAN]"},
		{`{"stored_fields":["_none_","a"]}`, 400, "illegal_argument_exception", "cannot combine _none_ with other fields"},
		{`{"stored_fields":["a",null]}`, 500, "illegal_state_exception", "Can't get text on a VALUE_NULL at 1:23"},
		{`{"docvalue_fields":"year"}`, 400, "parsing_exception", "Unknown key for a VALUE_STRING in [docvalue_fields]."},
		{`{"docvalue_fields":[{}]}`, 400, "illegal_argument_exception", "Required [field]"},
		{`{"fields":[{"foo":"year"}]}`, 400, "x_content_parse_exception", "[1:13] [docvalues_field] unknown field [foo]"},
		{`{"docvalue_fields":[null,"year"]}`, 400, "x_content_parse_exception", "[1:26] [docvalues_field] Expected START_OBJECT but was: VALUE_STRING"},
		{`{"script_fields":"abc"}`, 400, "parsing_exception", "Unknown key for a VALUE_STRING in [script_fields]."},
		{`{"script_fields":{"s":5}}`, 400, "parsing_exception", "Expected [START_OBJECT] in [s] but found [VALUE_NUMBER]"},
		{`{"script_fields":{"s":{"script":{"source":"1"},"foo":1}}}`, 400, "parsing_exception", "Unknown key for a VALUE_NUMBER in [foo]."},
		{`{"script_fields":{"s":{"script":{}}}}`, 400, "illegal_argument_exception", "must specify either [source] for an inline script or [id] for a stored script"},
		{`{"script_fields":{"s":{"script":5,"ignore_failure":true}}}`, 400, "x_content_parse_exception", "[1:35] [script] Expected START_OBJECT but was: FIELD_NAME"},
		{`{"script_fields":{"s":{"script":{"source":"1","lang":"nope"}}},"size":1}`, 400, "search_phase_execution_exception", "script_lang not supported [nope]"},
		{`{"script_fields":{"s":{"script":{"id":"nope"}}},"size":1}`, 404, "search_phase_execution_exception", "unable to find script [nope] in cluster state"},
		{`{"ext":{"foo":{}}}`, 400, "named_object_not_found_exception", "[1:15] unknown field [foo]"},
		{`{"knn":{}}`, 400, "parsing_exception", "Unknown key for a START_OBJECT in [knn]."},
		{`{"pit":{}}`, 400, "illegal_argument_exception", "point int time id is not provided"},
		{`{"query":null}`, 400, "parsing_exception", "Unknown key for a VALUE_NULL in [query]."},
		{`{"post_filter":"abc"}`, 400, "parsing_exception", "Unknown key for a VALUE_STRING in [post_filter]."},
		{`{"aggregations":5}`, 400, "parsing_exception", "Unknown key for a VALUE_NUMBER in [aggregations]."},
		{`{"suggest":{"foo":5}}`, 400, "illegal_argument_exception", "[suggest] does not support [foo]"},
		{`{"suggest":{"s":{"text":"a"}}}`, 400, "parse_exception", "missing suggestion object"},
		{`{"suggest":{"s":{"foo":{}}}}`, 400, "named_object_not_found_exception", "[1:24] unknown field [foo]"},
		{`{"sort":5}`, 400, "search_phase_execution_exception", "No mapping found for [5] in order to sort on"},
		{`{"stored_fields":"_none_","_source":true}`, 500, "search_phase_execution_exception", "[stored_fields] cannot be disabled if [_source] is requested"},
		// the keys are read in document order
		{`{"from":"x","_source":{"includes":["a"],"excludes":["a"]}}`, 400, "number_format_exception", `For input string: "x"`},
		{`{"_source":{"includes":["a"],"excludes":["a"]},"from":"x"}`, 400, "illegal_argument_exception", "The same entry [a] cannot be both included and excluded in _source."},
		{`{"query":{"foo":{}},"size":"abc"}`, 400, "parsing_exception", "unknown query [foo]"},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			expectRequestError(t, c, http.MethodPost, "/books/_search", tc.body, tc.status, tc.typ, tc.reason)
		})
	}
	// Jackson's coercion failures report where the value ends
	for _, tc := range []struct{ body, key, value, reason string }{
		{`{"version":5}`, "version", "5", "Current token (VALUE_NUMBER_INT) not of boolean type"},
		{`{"explain":1.5}`, "explain", "1.5", "Current token (VALUE_NUMBER_FLOAT) not of boolean type"},
		{`{"size":0,"from":true}`, "from", "true", "Current token (VALUE_TRUE) not numeric, cannot use numeric value accessors"},
		{`{"size":3000000000}`, "size", "3000000000", "Numeric value (3000000000) out of range of `int` (-2147483648 - 2147483647)"},
		{`{"size":2147483647.9}`, "size", "2147483647.9", "Numeric value (2147483647.9) out of range of `int` (-2147483648 - 2147483647)"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			res := expectRequestError(t, c, http.MethodPost, "/books/_search", tc.body, 400, "input_coercion_exception", tc.reason+sr2Location(tc.body, tc.key, tc.value))
			if jsonAt(t, res, "/error/caused_by/type") != "input_coercion_exception" {
				t.Fatalf("caused_by: %v", res)
			}
		})
	}
}

func TestSearchRequestRound2BodyAccepted(t *testing.T) {
	c := New()
	defer c.Close()
	sr2Books(t, c, "books")
	search := func(body string) map[string]any { return mustDo(t, c, http.MethodPost, "/books/_search", body) }

	for _, body := range []string{`{"ext":{},"size":0}`, `{"derived":{},"size":0}`, `{"search_pipeline":{"request_processors":[]},"verbose_pipeline":true,"size":0}`,
		`{"stats":["a","b"],"size":0}`, `{"timeout":"-1","size":0}`, `{"include_named_queries_score":true,"size":0}`, `{"size":"2.7","from":" 1"}`,
		`{"explain":"false","profile":false,"size":0}`, `{"script_fields":{"s":{"script":{"source":"1","lang":"nope"}}},"size":0}`} {
		search(body)
	}
	// suggest without suggestions and without query: no query runs
	assertJSON(t, search(`{"suggest":{}}`)["hits"], `{"total":{"value":0,"relation":"eq"},"max_score":null,"hits":[]}`)
	// a slice without max selects no shard
	res := search(`{"slice":{},"size":0}`)
	assertJSON(t, res["_shards"], `{"total":0,"successful":0,"skipped":0,"failed":0}`)
	assertJSON(t, res["hits"], `{"total":{"value":0,"relation":"eq"},"max_score":0.0,"hits":[]}`)
	// a negative body terminate_after terminates at once
	res = search(`{"terminate_after":-1,"size":0}`)
	if jsonAt(t, res, "/hits/total/value") != 0.0 || res["terminated_early"] != true {
		t.Fatalf("negative terminate_after: %v", res)
	}
	// a string _source is one include pattern
	assertJSON(t, jsonAt(t, search(`{"_source":"false","size":1,"sort":["_id"]}`), "/hits/hits/0/_source"), `{}`)
	// sorting on the score alone is no sort; a primary score sort tracks the maximum score
	res = search(`{"sort":["_score"],"size":1}`)
	if _, has := jsonAt(t, res, "/hits/hits/0").(map[string]any)["sort"]; has || jsonAt(t, res, "/hits/max_score") != 1.0 {
		t.Fatalf("score sort: %v", res)
	}
	if jsonAt(t, search(`{"sort":[{"_score":"asc"}],"size":1}`), "/hits/max_score") != nil {
		t.Fatalf("an ascending score sort does not track the maximum score")
	}
	if jsonAt(t, search(`{"sort":["_score","_id"],"size":1}`), "/hits/max_score") != 1.0 {
		t.Fatalf("a primary score sort tracks the maximum score")
	}
}

func TestSearchRequestRound2ValidateQuery(t *testing.T) {
	c := New()
	defer c.Close()
	sr2Books(t, c, "books")
	validate := func(qs, body string) map[string]any {
		return mustDo(t, c, http.MethodPost, "/books/_validate/query"+qs, body)
	}
	explanation := func(qs, query string) any {
		return jsonAt(t, validate(qs, `{"query":`+query+`}`), "/explanations/0/explanation")
	}
	for _, tc := range []struct{ qs, query, want string }{
		{"?explain=true", `{"term":{"author":"alice"}}`, "ConstantScore(author:alice)"},
		{"?explain=true", `{"term":{"author":{"value":"alice","boost":2}}}`, "(ConstantScore(author:alice))^2.0"},
		{"?explain=true", `{"match":{"description":"Quick Fox"}}`, "description:quick description:fox"},
		{"?explain=true", `{"match":{"description":{"query":"quick fox","operator":"and"}}}`, "+description:quick +description:fox"},
		{"?rewrite=true", `{"prefix":{"author":"al"}}`, "author:al*"},
		{"?explain=true", `{"match_all":{}}`, "ApproximateScoreQuery(originalQuery=*:*, approximationQuery=Approximate(*:*))"},
		{"?explain=true", `{"ids":{"values":["1","abc","x-5","10"]}}`, "_id:([69 b7] [fe 10] [fe 1f] [ff 78 2d 35])"},
		{"?explain=true", `{"terms":{"author":["bob","alice"]}}`, "author:(alice bob)"},
		{"?explain=true", `{"term":{"year":2001}}`, "IndexOrDocValuesQuery(indexQuery=year:[2001 TO 2001], dvQuery=year:[2001 TO 2001])"},
		{"?explain=true", `{"term":{"rating":4.5}}`, "IndexOrDocValuesQuery(indexQuery=rating:[4.5 TO 4.5], dvQuery=rating:[4616752568008179712 TO 4616752568008179712])"},
		{"?explain=true", `{"bool":{"must":[{"term":{"author":"alice"}}],"filter":[{"range":{"year":{"gte":2000}}}]}}`,
			"+ConstantScore(author:alice) #ApproximateScoreQuery(originalQuery=IndexOrDocValuesQuery(indexQuery=year:[2000 TO 2147483647], dvQuery=year:[2000 TO 2147483647]), approximationQuery=Approximate(year:[2000 TO 2147483647]))"},
		{"?explain=true", `{"bool":{"must_not":[{"term":{"author":"alice"}}]}}`, "-ConstantScore(author:alice) #*:*"},
		{"?rewrite=true", `{"bool":{"must_not":[{"term":{"author":"alice"}}]}}`, "-author:alice #*:*"},
		{"?rewrite=true", `{"bool":{"filter":[{"term":{"author":"alice"}}]}}`, "(ConstantScore(author:alice))^0.0"},
		{"?explain=true", `{"dis_max":{"queries":[{"term":{"author":"alice"}},{"match":{"description":"fox"}}],"tie_breaker":0.5}}`, "(ConstantScore(author:alice) | description:fox)~0.5"},
		{"?explain=true", `{"exists":{"field":"year"}}`, "ConstantScore(FieldExistsQuery [field=year])"},
		{"?explain=true", `{"match":{"nope":"x"}}`, `MatchNoDocsQuery("unmapped fields [nope]")`},
		{"?explain=true", `{"term":{"nope":"x"}}`, `MatchNoDocsQuery("User requested "match_none" query.")`},
		{"?explain=true", `{"match_phrase":{"description":"quick fox"}}`, `description:"quick fox"`},
	} {
		if got := explanation(tc.qs, tc.query); got != tc.want {
			t.Errorf("%s %s: explanation = %v, want %v", tc.qs, tc.query, got, tc.want)
		}
	}
	res := validate("?all_shards=true", `{"query":{"match_all":{}}}`)
	assertJSON(t, res["explanations"], `[{"index":"books","shard":0,"valid":true,"explanation":"ApproximateScoreQuery(originalQuery=*:*, approximationQuery=Approximate(*:*))"}]`)
	// a body that fails to parse is invalid without reaching a shard
	assertJSON(t, validate("", `{"query":{"foo":{}}}`), `{"valid":false}`)
	assertJSON(t, validate("?explain=true", `{"query":{"match_all":{}},"size":1}`), `{"valid":false,"error":"ParsingException[request does not support [size]]"}`)
	assertJSON(t, validate("?explain=true", `{"query":null}`), `{"valid":false,"error":"ParsingException[[_na] query malformed, must start with start_object]"}`)
	res = validate("?explain=true", `not json`)
	if msg, _ := res["error"].(string); res["valid"] != false || !strings.HasPrefix(msg, "ParsingException[Failed to parse]; nested: JsonParseException[Unrecognized token 'not'") ||
		!strings.Contains(msg, ";; org.opensearch.tools.jackson.core.JsonParseException: Unrecognized token 'not'") {
		t.Fatalf("malformed body: %v", res)
	}
	expectRequestError(t, c, http.MethodPost, "/books/_validate/query", `{}`, 400, "action_request_validation_exception", "Validation Failed: 1: query cannot be null;")
	if mustDo(t, c, http.MethodGet, "/books/_validate/query", nil)["valid"] != true {
		t.Fatalf("a request without body validates match_all")
	}
	// a query a shard cannot create reports the detailed message
	res = validate("?explain=true", `{"query":{"range":{"year":{"gte":"abc"}}}}`)
	msg, _ := jsonAt(t, res, "/explanations/0/error").(string)
	if res["valid"] != false || !strings.HasPrefix(msg, "[books/") ||
		!strings.HasSuffix(msg, `] QueryShardException[failed to create query: For input string: "abc"]; nested: NumberFormatException[For input string: "abc"];; java.lang.NumberFormatException: For input string: "abc"`) {
		t.Fatalf("shard error: %v", res)
	}
	// alias filters restrict the query
	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"books","alias":"alice","filter":{"term":{"author":"alice"}}}}]}`)
	res = mustDo(t, c, http.MethodPost, "/alice/_validate/query?explain=true", `{"query":{"match_all":{}}}`)
	if got := jsonAt(t, res, "/explanations/0/explanation"); got != "+ApproximateScoreQuery(originalQuery=*:*, approximationQuery=Approximate(*:*)) #ConstantScore(author:alice)" {
		t.Fatalf("alias filter explanation = %v", got)
	}
}

func TestSearchRequestRound2FieldCaps(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/fa", `{"mappings":{"properties":{"title":{"type":"text"},"year":{"type":"integer"},"meta_f":{"type":"long","meta":{"unit":"ms"}},
		"obj":{"properties":{"b":{"properties":{"c":{"type":"long"}}}}},"nest":{"type":"nested","properties":{"n":{"type":"keyword"}}},"bin":{"type":"binary"},"noidx":{"type":"keyword","index":false}}}}`)
	mustDo(t, c, http.MethodPut, "/fb", `{"mappings":{"properties":{"title":{"type":"keyword"},"year":{"type":"long"},"extra":{"type":"keyword"},"meta_f":{"type":"long","meta":{"unit":"s"}}}}}`)
	caps := func(path string) map[string]any {
		return mustDo(t, c, http.MethodGet, path, nil)["fields"].(map[string]any)
	}
	fields := caps("/fa,fb/_field_caps?fields=title,year,extra,meta_f")
	assertJSON(t, fields["extra"], `{"keyword":{"type":"keyword","searchable":true,"aggregatable":true}}`)
	assertJSON(t, fields["year"], `{"integer":{"type":"integer","searchable":true,"aggregatable":true,"indices":["fa"]},"long":{"type":"long","searchable":true,"aggregatable":true,"indices":["fb"]}}`)
	assertJSON(t, fields["title"], `{"text":{"type":"text","searchable":true,"aggregatable":false,"indices":["fa"]},"keyword":{"type":"keyword","searchable":true,"aggregatable":true,"indices":["fb"]}}`)
	assertJSON(t, fields["meta_f"], `{"long":{"type":"long","searchable":true,"aggregatable":true,"meta":{"unit":["ms","s"]}}}`)
	fields = caps("/fa/_field_caps?fields=obj*,nest,bin,noidx,_id,_source,_doc_count")
	assertJSON(t, fields["obj"], `{"object":{"type":"object","searchable":false,"aggregatable":false}}`)
	assertJSON(t, fields["obj.b"], `{"object":{"type":"object","searchable":false,"aggregatable":false}}`)
	assertJSON(t, fields["nest"], `{"nested":{"type":"nested","searchable":false,"aggregatable":false}}`)
	assertJSON(t, fields["bin"], `{"binary":{"type":"binary","searchable":false,"aggregatable":false}}`)
	assertJSON(t, fields["noidx"], `{"keyword":{"type":"keyword","searchable":false,"aggregatable":true}}`)
	assertJSON(t, fields["_id"], `{"_id":{"type":"_id","searchable":true,"aggregatable":true}}`)
	assertJSON(t, fields["_source"], `{"_source":{"type":"_source","searchable":false,"aggregatable":false}}`)
	assertJSON(t, fields["_doc_count"], `{"long":{"type":"long","searchable":false,"aggregatable":false}}`)
	// unmapped groups are only reported for fields some index maps
	if len(caps("/fa/_field_caps?fields=nope&include_unmapped=true")) != 0 {
		t.Fatalf("a field no index maps is not reported")
	}
	fields = caps("/fa,fb/_field_caps?fields=extra&include_unmapped=true")
	assertJSON(t, fields["extra"], `{"keyword":{"type":"keyword","searchable":true,"aggregatable":true,"indices":["fb"]},"unmapped":{"type":"unmapped","searchable":false,"aggregatable":false,"indices":["fa"]}}`)
	// index_filter skips indices where the filter cannot match
	res := mustDo(t, c, http.MethodPost, "/fa,fb/_field_caps?fields=year", `{"index_filter":{"term":{"extra":"x"}}}`)
	assertJSON(t, res, `{"indices":["fb"],"fields":{"year":{"long":{"type":"long","searchable":true,"aggregatable":true}}}}`)
}

func TestSearchRequestRound2AggregationsAcrossMappings(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/m1", `{"mappings":{"properties":{"f":{"type":"keyword"},"l":{"type":"long"},"u":{"type":"keyword"}}}}`)
	mustDo(t, c, http.MethodPut, "/m2", `{"mappings":{"properties":{"f":{"type":"long"},"l":{"type":"double"}}}}`)
	if err := c.BulkString("{\"index\":{\"_index\":\"m1\",\"_id\":\"1\"}}\n{\"f\":\"10\",\"l\":1,\"u\":\"x\"}\n{\"index\":{\"_index\":\"m1\",\"_id\":\"2\"}}\n{\"f\":\"b\",\"l\":2,\"u\":\"y\"}\n" +
		"{\"index\":{\"_index\":\"m2\",\"_id\":\"1\"}}\n{\"f\":10,\"l\":1.5}\n{\"index\":{\"_index\":\"m2\",\"_id\":\"2\"}}\n{\"f\":20,\"l\":2}\n"); err != nil {
		t.Fatal(err)
	}
	// the index the aggregation does not apply to fails its shard
	res := mustDo(t, c, http.MethodPost, "/m1,m2/_search", `{"size":0,"aggs":{"a":{"avg":{"field":"f"}}}}`)
	assertJSON(t, res["_shards"], `{"total":2,"successful":1,"skipped":0,"failed":1,"failures":[{"shard":0,"index":"m1","node":"osmem","reason":{"type":"illegal_argument_exception","reason":"Field [f] of type [keyword] is not supported for aggregation [avg]"}}]}`)
	assertJSON(t, res["hits"], `{"total":{"value":2,"relation":"eq"},"max_score":null,"hits":[]}`)
	assertJSON(t, res["aggregations"], `{"a":{"value":15.0}}`)
	// an index where the field is unmapped still answers
	res = mustDo(t, c, http.MethodPost, "/m1,m2/_search", `{"size":0,"aggs":{"a":{"avg":{"field":"u"}}}}`)
	if jsonAt(t, res, "/_shards/failed") != 1.0 || jsonAt(t, res, "/aggregations/a/value") != nil {
		t.Fatalf("unmapped index answers: %v", res)
	}
	// keys read as bytes in one index and numbers in another cannot be reduced
	res = expectRequestError(t, c, http.MethodPost, "/m1,m2/_search", `{"size":0,"aggs":{"mt":{"multi_terms":{"terms":[{"field":"f"},{"field":"l"}]}}}}`, 500, "search_phase_execution_exception", "")
	if !strings.HasPrefix(fmt.Sprint(jsonAt(t, res, "/error/caused_by/reason")), "class org.apache.lucene.util.BytesRef cannot be cast to class java.lang.Long") {
		t.Fatalf("multi_terms reduce failure: %v", res)
	}
}

func TestSearchRequestRound2ScoresOnlyWhenNeeded(t *testing.T) {
	c := New()
	defer c.Close()
	sr2Books(t, c, "books")
	fs := `{"function_score":{"query":{"match_all":{}},"field_value_factor":{"field":"rating"}}}`
	for _, body := range []string{
		`{"query":` + fs + `,"sort":["_id"]}`,
		`{"query":` + fs + `,"size":0}`,
		`{"query":` + fs + `,"size":0,"aggs":{"t":{"terms":{"field":"author"}}}}`,
		`{"query":{"bool":{"filter":[` + fs + `]}}}`,
		`{"query":{"function_score":{"query":{"match_all":{}},"script_score":{"script":"doc['year'].value"}}},"sort":["_id"]}`,
	} {
		if n, res := status(t, c, http.MethodPost, "/books/_search", body); n != 200 {
			t.Fatalf("%s: status %d %v", body, n, res)
		}
	}
	if n, _ := status(t, c, http.MethodPost, "/books/_count", `{"query":`+fs+`}`); n != 200 {
		t.Fatalf("_count does not score")
	}
	for _, body := range []string{
		`{"query":` + fs + `}`,
		`{"query":` + fs + `,"sort":["_id"],"track_scores":true}`,
		`{"query":` + fs + `,"sort":["_id"],"min_score":0}`,
		`{"query":` + fs + `,"size":0,"aggs":{"th":{"top_hits":{"size":1}}}}`,
	} {
		res := expectRequestError(t, c, http.MethodPost, "/books/_search", body, 500, "search_phase_execution_exception", "Missing value for field [rating]")
		assertJSON(t, res["error"].(map[string]any)["caused_by"], `{"type":"exception","reason":"Missing value for field [rating]"}`)
	}
	res := expectRequestError(t, c, http.MethodPost, "/books/_search", `{"query":{"function_score":{"field_value_factor":{"field":"nope"}}},"sort":["_id"]}`, 400, "search_phase_execution_exception",
		"Unable to find a field mapper for field [nope]. No 'missing' value defined.")
	if jsonAt(t, res, "/error/failed_shards/0/reason/type") != "query_shard_exception" ||
		jsonAt(t, res, "/error/failed_shards/0/reason/reason") != "failed to create query: Unable to find a field mapper for field [nope]. No 'missing' value defined." {
		t.Fatalf("field mapper failure: %v", res)
	}
}

func TestSearchRequestRound2Explain(t *testing.T) {
	c := New()
	defer c.Close()
	sr2Books(t, c, "books")
	res := mustDo(t, c, http.MethodGet, "/books/_explain/1", `{"query":{"match_all":{}}}`)
	assertJSON(t, res, `{"_index":"books","_id":"1","matched":true,"explanation":{"value":1.0,"description":"*:*","details":[]}}`)
	res = mustDo(t, c, http.MethodPost, "/books/_explain/1", `{"query":{"term":{"author":"alice"}}}`)
	assertJSON(t, res, `{"_index":"books","_id":"1","matched":false,"explanation":{"value":0.0,"description":"ConstantScore(author:alice) doesn't match id 2","details":[]}}`)
	res = mustDo(t, c, http.MethodPost, "/books/_explain/10", `{"query":{"constant_score":{"filter":{"term":{"author":"alice"}},"boost":3}}}`)
	assertJSON(t, res["explanation"], `{"value":3.0,"description":"ConstantScore(author:alice)^3.0","details":[]}`)
	res = mustDo(t, c, http.MethodPost, "/books/_explain/10", `{"query":{"bool":{"must":[{"term":{"author":"alice"}}],"filter":[{"range":{"year":{"gte":2000}}}]}}}`)
	assertJSON(t, res["explanation"], `{"value":1.0,"description":"sum of:","details":[{"value":1.0,"description":"ConstantScore(author:alice)","details":[]},
		{"value":0.0,"description":"match on required clause, product of:","details":[{"value":0.0,"description":"# clause","details":[]},
		{"value":1.0,"description":"IndexOrDocValuesQuery(indexQuery=year:[2000 TO 2147483647], dvQuery=year:[2000 TO 2147483647])","details":[]}]}]}`)
	res = mustDo(t, c, http.MethodPost, "/books/_explain/10", `{"query":{"match_none":{}}}`)
	assertJSON(t, res["explanation"], `{"value":0.0,"description":"User requested \"match_none\" query.","details":[]}`)
	if n, body := status(t, c, http.MethodPost, "/books/_explain/nope", `{"query":{"match_all":{}}}`); n != 404 {
		t.Fatalf("missing document: %d %v", n, body)
	} else {
		assertJSON(t, body, `{"_index":"books","_id":"nope","matched":false}`)
	}
	expectRequestError(t, c, http.MethodPost, "/books/_explain/1", `{}`, 400, "action_request_validation_exception", "Validation Failed: 1: query is missing;")
	expectRequestError(t, c, http.MethodPost, "/books/_explain/1", `{"query":{"match_all":{}},"size":1}`, 400, "parsing_exception", "request does not support [size]")
	if got := mustDo(t, c, http.MethodPost, "/books/_explain/1?q=author:carol", nil)["matched"]; got != true {
		t.Fatalf("q parameter: %v", got)
	}
	// search hits carry their shard, node and explanation
	res = mustDo(t, c, http.MethodPost, "/books/_search", `{"explain":true,"query":{"ids":{"values":["1"]}},"_source":false}`)
	hit := jsonAt(t, res, "/hits/hits/0").(map[string]any)
	if hit["_shard"] != "[books][0]" || hit["_node"] == nil {
		t.Fatalf("explained hit: %v", hit)
	}
	assertJSON(t, hit["_explanation"], `{"value":1.0,"description":"_id:([fe 1f])","details":[]}`)
	// profile lists the searched shards
	res = mustDo(t, c, http.MethodPost, "/books/_search", `{"profile":true,"size":0}`)
	if shards, _ := jsonAt(t, res, "/profile/shards").([]any); len(shards) != 1 {
		t.Fatalf("profile: %v", res["profile"])
	}
}

func TestSearchRequestRound2GeoDistanceSort(t *testing.T) {
	c := New()
	defer c.Close()
	sr2Books(t, c, "books")
	// distances are compared to 9 significant digits: OpenSearch computes
	// them with Lucene's SloppyMath, whose last digits osmem does not reproduce
	sortValues := func(sort string) string {
		res := mustDo(t, c, http.MethodPost, "/books/_search", `{"_source":false,"sort":`+sort+`}`)
		var out []string
		for _, h := range res["hits"].(map[string]any)["hits"].([]any) {
			hm := h.(map[string]any)
			var values []string
			for _, v := range hm["sort"].([]any) {
				if f, ok := v.(float64); ok {
					values = append(values, strconv.FormatFloat(f, 'g', 9, 64))
				} else {
					data, _ := json.Marshal(v)
					values = append(values, string(data))
				}
			}
			out = append(out, hm["_id"].(string)+"=["+strings.Join(values, ",")+"]")
		}
		return strings.Join(out, " ")
	}
	if got := sortValues(`[{"_geo_distance":{"location":{"lat":35,"lon":139},"order":"asc","unit":"km"}}]`); got != `10=[102.319683] 2=[8911.11689] 1=[9753.09955] abc=["Infinity"] x-5=["Infinity"]` {
		t.Errorf("km sort = %s", got)
	}
	if got := sortValues(`[{"_geo_distance":{"location":["35,139",{"lat":0,"lon":0}],"unit":"km"}}]`); got != `10=[102.319683] 1=[5436.56052] 2=[8911.11689] abc=["Infinity"] x-5=["Infinity"]` {
		t.Errorf("several origins = %s", got)
	}
	if got := sortValues(`[{"_geo_distance":{"location":{"lat":35,"lon":139},"unit":"mi","order":"desc"}}]`); !strings.HasPrefix(got, `abc=["Infinity"] x-5=["Infinity"] 1=[`) {
		t.Errorf("descending sort = %s", got)
	}
	expectRequestError(t, c, http.MethodPost, "/books/_search", `{"sort":[{"_geo_distance":{"location":{"lat":35,"lon":139},"unit":"parsecs"}}]}`, 400, "illegal_argument_exception", "No distance unit match [parsecs]")
	expectRequestError(t, c, http.MethodPost, "/books/_search", `{"sort":[{"_geo_distance":{"location":{"lat":35,"lon":139},"mode":"sum"}}]}`, 400, "illegal_argument_exception", "sort_mode [sum] isn't supported for sorting by geo distance")
	expectRequestError(t, c, http.MethodPost, "/books/_search", `{"sort":[{"_geo_distance":{"nope":{"lat":35,"lon":139}}}]}`, 400, "search_phase_execution_exception", "failed to find mapper for [nope] for geo distance based sort")
	expectRequestError(t, c, http.MethodPost, "/books/_search", `{"sort":[{"_geo_distance":{"location":{"lat":100,"lon":139}}}]}`, 400, "search_phase_execution_exception", "illegal latitude value [100.0] for [GeoDistanceSort] for field [location].")
}

func TestSearchRequestRound2TerminateAfterAndShards(t *testing.T) {
	c := New()
	defer c.Close()
	sr2Books(t, c, "books")
	// aggregations a shard precomputes for a query matching every document
	// are not terminated; the others are
	res := mustDo(t, c, http.MethodPost, "/books/_search", `{"terminate_after":1,"size":0,"aggs":{"t":{"terms":{"field":"author"}}}}`)
	assertJSON(t, jsonAt(t, res, "/aggregations/t/buckets"), `[{"key":"alice","doc_count":2},{"key":"bob","doc_count":1},{"key":"carol","doc_count":1}]`)
	res = mustDo(t, c, http.MethodPost, "/books/_search", `{"terminate_after":1,"size":0,"aggs":{"c":{"value_count":{"field":"year"}}}}`)
	assertJSON(t, jsonAt(t, res, "/aggregations/c"), `{"value":1}`)
	// exists on a field every document has counts every document
	res = mustDo(t, c, http.MethodPost, "/books/_search", `{"terminate_after":1,"size":0,"query":{"exists":{"field":"title"}}}`)
	if jsonAt(t, res, "/hits/total/value") != 5.0 {
		t.Fatalf("count shortcut: %v", res)
	}
	// with a primary field sort, shards whose query cannot match are skipped
	mustDo(t, c, http.MethodPut, "/other", `{"mappings":{"properties":{"author":{"type":"keyword"}}}}`)
	mustDo(t, c, http.MethodPut, "/other/_doc/o1?refresh=true", `{"author":"alice"}`)
	res = mustDo(t, c, http.MethodPost, "/books,other/_search", `{"sort":["_id"],"_source":false,"query":{"term":{"_index":"other"}}}`)
	assertJSON(t, res["_shards"], `{"total":2,"successful":2,"skipped":1,"failed":0}`)
	// _index terms match the aliases of the index
	mustDo(t, c, http.MethodPost, "/_aliases", `{"actions":[{"add":{"index":"other","alias":"al"}}]}`)
	if ids := hitIDsOf(mustDo(t, c, http.MethodPost, "/books,other/_search", `{"query":{"term":{"_index":"al"}}}`)); len(ids) != 1 || ids[0] != "o1" {
		t.Fatalf("_index alias term: %v", ids)
	}
}

// sr2Numbered indexes documents {"n":1} ... {"n":docs} into an index with
// the given number of shards.
func sr2Numbered(t *testing.T, c *Cluster, index string, shards, docs int) {
	t.Helper()
	mustDo(t, c, http.MethodPut, "/"+index, fmt.Sprintf(`{"settings":{"number_of_shards":%d},"mappings":{"properties":{"n":{"type":"integer"}}}}`, shards))
	var sb strings.Builder
	for i := 1; i <= docs; i++ {
		fmt.Fprintf(&sb, "{\"index\":{\"_index\":%q,\"_id\":\"%d\"}}\n{\"n\":%d}\n", index, i, i)
	}
	if err := c.BulkString(sb.String()); err != nil {
		t.Fatal(err)
	}
}

func TestSearchRequestRound2Preference(t *testing.T) {
	c := New()
	defer c.Close()
	sr2Numbered(t, c, "two", 2, 6)
	sr2Numbered(t, c, "one", 1, 3)
	for pref, want := range map[string]string{
		"_shards:0": `["1","2","3","5","6"]`, "_shards:1": `["4"]`, "_shards:0,1": `["1","2","3","4","5","6"]`,
		"_shards:0%7C_local": `["1","2","3","5","6"]`, "_shards:0%7C": `["1","2","3","5","6"]`, "_shards:%2B1,": `["4"]`,
		"_local": `["1","2","3","4","5","6"]`, "_prefer_nodes:x": `["1","2","3","4","5","6"]`, "custom": `["1","2","3","4","5","6"]`,
	} {
		res := mustDo(t, c, http.MethodPost, "/two/_search?preference="+pref, `{"sort":["n"],"_source":false}`)
		assertJSON(t, hitIDsOf(res), want)
	}
	res := mustDo(t, c, http.MethodPost, "/two/_search?preference=_shards:1", `{"size":0,"aggs":{"m":{"max":{"field":"n"}}}}`)
	assertJSON(t, res["_shards"], `{"total":1,"successful":1,"skipped":0,"failed":0}`)
	assertJSON(t, jsonAt(t, res, "/aggregations/m"), `{"value":4.0}`)
	// a preference selecting no shard searches nothing
	res = mustDo(t, c, http.MethodPost, "/one,two/_search?preference=_shards:9", `{"size":0,"aggs":{"m":{"max":{"field":"n"}}}}`)
	assertJSON(t, res["_shards"], `{"total":0,"successful":0,"skipped":0,"failed":0}`)
	assertJSON(t, res["hits"], `{"total":{"value":0,"relation":"eq"},"max_score":0.0,"hits":[]}`)
	if _, has := res["aggregations"]; has {
		t.Fatalf("aggregations of an empty search: %v", res)
	}
	res = mustDo(t, c, http.MethodPost, "/two/_search?preference=_shards:", `{"track_total_hits":false}`)
	assertJSON(t, res["hits"], `{"max_score":0.0,"hits":[]}`)
	// indices without a selected shard are not searched
	res = mustDo(t, c, http.MethodPost, "/one,two/_search?preference=_shards:1", `{"sort":["n"],"_source":false}`)
	assertJSON(t, res["_shards"], `{"total":1,"successful":1,"skipped":0,"failed":0}`)
	// the shard ids are parsed until the shard is found
	mustDo(t, c, http.MethodPost, "/one/_search?preference=_shards:0,abc", nil)
	for _, tc := range []struct {
		pref   string
		status int
		typ    string
		reason string
	}{
		{"_shards:0,abc", 400, "number_format_exception", `For input string: "abc"`},
		{"_shards:1,,0", 400, "number_format_exception", `For input string: ""`},
		{"_shards:2147483648", 400, "number_format_exception", `For input string: "2147483648"`},
		{"_foo", 400, "illegal_argument_exception", "no Preference for [_foo]"},
		{"_SHARDS:0", 400, "illegal_argument_exception", "no Preference for [_SHARDS]"},
		{"_shards:0%7Ccustom", 400, "illegal_argument_exception", "no Preference for [custom]"},
		{"_shards:0%7C_shards:1", 400, "illegal_argument_exception", "unknown preference [SHARDS]"},
		{"_shards", 500, "string_index_out_of_bounds_exception", "Range [8, 7) out of bounds for length 7"},
		{"_prefer_nodes", 500, "string_index_out_of_bounds_exception", "Range [14, 13) out of bounds for length 13"},
	} {
		expectRequestError(t, c, http.MethodPost, "/two/_search?preference="+tc.pref, nil, tc.status, tc.typ, tc.reason)
	}
	res = mustDo(t, c, http.MethodGet, "/two/_count?preference=_shards:1", nil)
	assertJSON(t, res, `{"count":1,"_shards":{"total":1,"successful":1,"skipped":0,"failed":0}}`)
	res = mustDo(t, c, http.MethodGet, "/two/_count?preference=_shards:9", nil)
	assertJSON(t, res, `{"count":0,"_shards":{"total":0,"successful":0,"skipped":0,"failed":0}}`)
	expectRequestError(t, c, http.MethodGet, "/two/_count?preference=_foo", nil, 400, "illegal_argument_exception", "no Preference for [_foo]")
	res = mustDo(t, c, http.MethodPost, "/_msearch", "{\"index\":\"two\",\"preference\":\"_shards:9\"}\n{}\n{\"index\":\"two\",\"preference\":\"_foo\"}\n{}\n")
	assertJSON(t, jsonAt(t, res, "/responses/0/_shards"), `{"total":0,"successful":0,"skipped":0,"failed":0}`)
	assertJSON(t, jsonAt(t, res, "/responses/1/error/type"), `"illegal_argument_exception"`)
	assertJSON(t, jsonAt(t, res, "/responses/1/status"), `400`)
}

func TestSearchRequestRound2PhaseTook(t *testing.T) {
	c := New()
	defer c.Close()
	sr2Numbered(t, c, "two", 2, 6)
	phases := `{"dfs_pre_query":0,"query":0,"fetch":0,"dfs_query":0,"expand":0,"can_match":0}`
	for _, uri := range []string{"/two/_search?phase_took=true&size=0", "/two/_search?phase_took&size=0", "/two/_search?phase_took=true&preference=_shards:9"} {
		assertJSON(t, mustDo(t, c, http.MethodPost, uri, nil)["phase_took"], phases)
	}
	for _, uri := range []string{"/two/_search?size=0", "/two/_search?phase_took=false&size=0"} {
		if res := mustDo(t, c, http.MethodPost, uri, nil); res["phase_took"] != nil {
			t.Fatalf("%s: phase_took %v", uri, res["phase_took"])
		}
	}
	res := mustDo(t, c, http.MethodPost, "/_msearch", "{\"index\":\"two\",\"phase_took\":true}\n{\"size\":0}\n{\"index\":\"two\"}\n{\"size\":0}\n")
	assertJSON(t, jsonAt(t, res, "/responses/0/phase_took"), phases)
	if second := res["responses"].([]any)[1].(map[string]any); second["phase_took"] != nil {
		t.Fatalf("msearch phase_took without the header: %v", second)
	}
	// the request parameter overrides search.phase_took_enabled
	// (TransportSearchAction)
	mustDo(t, c, http.MethodPut, "/_cluster/settings", `{"transient":{"search.phase_took_enabled":true}}`)
	assertJSON(t, mustDo(t, c, http.MethodPost, "/two/_search?size=0", nil)["phase_took"], phases)
	if res := mustDo(t, c, http.MethodPost, "/two/_search?size=0&phase_took=false", nil); res["phase_took"] != nil {
		t.Fatalf("phase_took=false with the setting: %v", res)
	}
}

func TestSearchRequestRound2DocValueFieldsParameter(t *testing.T) {
	c := New()
	defer c.Close()
	sr2Books(t, c, "books")
	res := mustDo(t, c, http.MethodGet, "/books/_search?docvalue_fields=year&size=1&sort=_id&_source=false", nil)
	assertJSON(t, jsonAt(t, res, "/hits/hits/0/fields"), `{"year":[2015]}`)
	// the parameter adds to the fields of the body; a field requested again
	// adds its values again
	res = mustDo(t, c, http.MethodPost, "/books/_search?docvalue_fields=year&size=1&sort=_id&_source=false", `{"docvalue_fields":[{"field":"year","format":"#.0"}]}`)
	assertJSON(t, jsonAt(t, res, "/hits/hits/0/fields"), `{"year":["2015.0",2015]}`)
	res = mustDo(t, c, http.MethodPost, "/books/_search?size=1&sort=_id&_source=false", `{"docvalue_fields":["year","y*"]}`)
	assertJSON(t, jsonAt(t, res, "/hits/hits/0/fields"), `{"year":[2015,2015]}`)
	res = mustDo(t, c, http.MethodPost, "/books/_search?size=1&sort=_id&_source=false", `{"fields":["year"],"docvalue_fields":["year"]}`)
	assertJSON(t, jsonAt(t, res, "/hits/hits/0/fields"), `{"year":[2015]}`)
	mustDo(t, c, http.MethodPut, "/stored", `{"mappings":{"properties":{"s":{"type":"keyword","store":true}}}}`)
	mustDo(t, c, http.MethodPut, "/stored/_doc/1?refresh=true", `{"s":"a"}`)
	res = mustDo(t, c, http.MethodPost, "/stored/_search", `{"stored_fields":["s","s"],"docvalue_fields":["s"]}`)
	assertJSON(t, jsonAt(t, res, "/hits/hits/0/fields"), `{"s":["a","a"]}`)
}

func TestSearchRequestRound2DecayFunctionErrors(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/decay", `{"mappings":{"properties":{"n":{"type":"integer"},"k":{"type":"keyword"},"d":{"type":"date"},"g":{"type":"geo_point"}}}}`)
	mustDo(t, c, http.MethodPut, "/decay/_doc/1?refresh=true", `{"n":1,"k":"a","d":"2020-01-01","g":"1,1"}`)
	query := func(fn string) string { return `{"query":{"function_score":{"gauss":` + fn + `}}}` }
	// DecayFunctionBuilder parses its function on the shard: its parsing
	// exceptions are shard failures located in the function's own parser
	keyword := `{"type":"parsing_exception","reason":"field [k] is of type [org.opensearch.index.mapper.KeywordFieldMapper$KeywordFieldType@1], but only numeric types are supported.","line":1,"col":1}`
	for _, path := range []string{"/decay/_search", "/decay/_count"} {
		res := expectRequestError(t, c, http.MethodPost, path, query(`{"k":{"origin":1,"scale":1}}`), 400, "search_phase_execution_exception", "")
		assertJSON(t, jsonAt(t, res, "/error/failed_shards/0/reason"), keyword)
		assertJSON(t, jsonAt(t, res, "/error/root_cause/0"), keyword)
	}
	res := expectRequestError(t, c, http.MethodPost, "/decay/_search", query(`{"nope":{"origin":1,"scale":1}}`), 400, "search_phase_execution_exception", "unknown field [nope]")
	assertJSON(t, jsonAt(t, res, "/error/caused_by"), `{"type":"parsing_exception","reason":"unknown field [nope]","line":1,"col":0}`)
	res = expectRequestError(t, c, http.MethodPost, "/decay/_explain/1", query(`{"nope":{"origin":1,"scale":1}}`), 400, "parsing_exception", "unknown field [nope]")
	assertJSON(t, jsonAt(t, res, "/error/col"), `0`)
	res = mustDo(t, c, http.MethodPost, "/decay/_validate/query?explain=true", query(`{"k":{"origin":1,"scale":1}}`))
	assertJSON(t, jsonAt(t, res, "/explanations/0/error"), `"ParsingException[field [k] is of type [org.opensearch.index.mapper.KeywordFieldMapper$KeywordFieldType@1], but only numeric types are supported.]"`)
	res = mustDo(t, c, http.MethodPost, "/_msearch", "{\"index\":\"decay\"}\n"+query(`{"k":{"origin":1,"scale":1}}`)+"\n")
	assertJSON(t, jsonAt(t, res, "/responses/0/status"), `400`)
	// other exceptions are wrapped in query_shard_exception
	for fn, cause := range map[string]string{
		`{"n":{"origin":1}}`:                        `{"type":"parse_exception","reason":"both [scale] and [origin] must be set for numeric fields."}`,
		`{"g":{"origin":"1,1"}}`:                    `{"type":"parse_exception","reason":"[origin] and [scale] must be set for geo fields."}`,
		`{"d":{"origin":"2020-01-01"}}`:             `{"type":"parse_exception","reason":"[scale] must be set for date fields."}`,
		`{"d":{"origin":"2020-01-01","scale":"x"}}`: `{"type":"illegal_argument_exception","reason":"failed to parse setting [DecayFunctionParser.scale] with value [x] as a time value: unit is missing or unrecognized"}`,
		`{"g":{"origin":"1,1","scale":"x"}}`:        `{"type":"number_format_exception","reason":"For input string: \"x\""}`,
	} {
		var c0 map[string]any
		if err := json.Unmarshal([]byte(cause), &c0); err != nil {
			t.Fatal(err)
		}
		res := expectRequestError(t, c, http.MethodPost, "/decay/_search", query(fn), 400, "search_phase_execution_exception", "")
		assertJSON(t, jsonAt(t, res, "/error/failed_shards/0/reason/type"), `"query_shard_exception"`)
		assertJSON(t, jsonAt(t, res, "/error/failed_shards/0/reason/reason"), strconv.Quote("failed to create query: "+c0["reason"].(string)))
		assertJSON(t, jsonAt(t, res, "/error/failed_shards/0/reason/caused_by"), cause)
		res = expectRequestError(t, c, http.MethodPost, "/decay/_explain/1", query(fn), 400, "query_shard_exception", "")
		assertJSON(t, jsonAt(t, res, "/error/index"), `"decay"`)
	}
	// the multi value mode is read while parsing
	mode := `{"query":{"function_score":{"gauss":{"n":{"origin":1,"scale":1},"multi_value_mode":"foo"}}}}`
	expectRequestError(t, c, http.MethodPost, "/decay/_search", mode, 400, "illegal_argument_exception", "Illegal sort mode: foo")
	for _, path := range []string{"/decay/_count", "/decay/_explain/1"} {
		res := expectRequestError(t, c, http.MethodPost, path, mode, 400, "parsing_exception", "Failed to parse")
		assertJSON(t, jsonAt(t, res, "/error/col"), `84`)
		assertJSON(t, jsonAt(t, res, "/error/caused_by"), `{"type":"illegal_argument_exception","reason":"Illegal sort mode: foo"}`)
	}
	res = mustDo(t, c, http.MethodPost, "/decay/_validate/query?explain=true", mode)
	assertJSON(t, res, `{"valid":false,"error":"ParsingException[Failed to parse]; nested: IllegalArgumentException[Illegal sort mode: foo];; java.lang.IllegalArgumentException: Illegal sort mode: foo"}`)
	res = mustDo(t, c, http.MethodPost, "/decay/_search", `{"query":{"function_score":{"gauss":{"n":{"origin":1,"scale":1},"multi_value_mode":"Median"}}}}`)
	assertJSON(t, jsonAt(t, res, "/hits/max_score"), `1.0`)
}

func TestSearchRequestRound2QueryContentErrors(t *testing.T) {
	c := New()
	defer c.Close()
	sr2Books(t, c, "books")
	// count, explain and validate query wrap exceptions other than parsing
	// exceptions (RestActions.getQueryContent); search reports them as they are
	body := `{"query":{"match":{"author":{"query":"a","operator":"xyz"}}}}`
	reason := "No enum constant org.opensearch.index.query.Operator.XYZ"
	expectRequestError(t, c, http.MethodPost, "/books/_search", body, 400, "illegal_argument_exception", reason)
	for _, path := range []string{"/books/_count", "/books/_explain/1"} {
		res := expectRequestError(t, c, http.MethodPost, path, body, 400, "parsing_exception", "Failed to parse")
		assertJSON(t, jsonAt(t, res, "/error/caused_by"), `{"type":"illegal_argument_exception","reason":"`+reason+`"}`)
	}
	res := mustDo(t, c, http.MethodPost, "/books/_validate/query?explain=true", body)
	assertJSON(t, res, `{"valid":false,"error":"ParsingException[Failed to parse]; nested: IllegalArgumentException[`+reason+`];; java.lang.IllegalArgumentException: `+reason+`"}`)
}
