package osmem

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/shibukawa/osmem/internal/engine"
)

// OpenSearch REST handlers read ("consume") the URL parameters they know
// while preparing a request, validating each value as they go; any
// parameter left unconsumed is rejected with "contains unrecognized
// parameter", with "did you mean" suggestions drawn from the consumed ones.
// This file reproduces that per endpoint: the rules of each API list its
// parameters in OpenSearch's consumption order together with the value
// checks OpenSearch applies. The lists were taken from the OpenSearch 3.8
// REST handlers and verified against a running server.

// ---------------------------------------------------------------------------
// query string and headers

// parseQueryString is RestUtils.decodeQueryString: '&' and ';' separate
// parameters, a later value replaces an earlier one and '+' is a space.
func parseQueryString(raw string) (engine.Params, *engine.Error) {
	p := engine.Params{}
	var failure error
	decode := func(s string) string {
		out, err := decodeURLComponent(s, true)
		if err != nil && failure == nil {
			failure = err
		}
		return out
	}
	name, haveName, pos := "", false, 0
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c == '=' && !haveName:
			if pos != i {
				name, haveName = decode(raw[pos:i]), true
			}
			pos = i + 1
		case c == '&' || c == ';':
			if !haveName && pos != i {
				p[decode(raw[pos:i])] = ""
			} else if haveName {
				p[name] = decode(raw[pos:i])
				haveName = false
			}
			pos = i + 1
		}
	}
	if pos != len(raw) {
		if !haveName {
			p[decode(raw[pos:])] = ""
		} else {
			p[name] = decode(raw[pos:])
		}
	} else if haveName {
		p[name] = ""
	}
	if failure != nil {
		return nil, &engine.Error{Status: http.StatusBadRequest, Type: "bad_parameter_exception",
			Reason: "java.lang.IllegalArgumentException: " + failure.Error(),
			Cause:  &engine.Error{Type: "illegal_argument_exception", Reason: failure.Error()}}
	}
	return p, nil
}

// decodeURLComponent is RestUtils.decodeComponent.
func decodeURLComponent(s string, plusAsSpace bool) (string, error) {
	if !strings.ContainsAny(s, "%+") {
		return s, nil
	}
	buf := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '+':
			if plusAsSpace {
				buf = append(buf, ' ')
			} else {
				buf = append(buf, '+')
			}
		case '%':
			if i == len(s)-1 {
				return "", fmt.Errorf("unterminated escape sequence at end of string: %s", s)
			}
			i++
			if s[i] == '%' {
				buf = append(buf, '%')
				continue
			}
			if i == len(s)-1 {
				return "", fmt.Errorf("partial escape sequence at end of string: %s", s)
			}
			hi, lo := unhexNibble(s[i]), unhexNibble(s[i+1])
			i++
			if hi < 0 || lo < 0 {
				return "", fmt.Errorf("invalid escape sequence `%%%c%c' at index %d of: %s", s[i-1], s[i], i-2, s)
			}
			buf = append(buf, byte(hi*16+lo))
		default:
			buf = append(buf, c)
		}
	}
	return string(buf), nil
}

func unhexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

type mediaKind int

const (
	mediaNone mediaKind = iota
	mediaJSON
	mediaOther   // smile, yaml, cbor: known to OpenSearch, not parsed by osmem
	mediaUnknown // not an xcontent type (text/plain, form data, ...)
)

// parseContentType is RestRequest.parseContentType plus the media type lookup.
func parseContentType(values []string) (mediaKind, *engine.Error) {
	if len(values) == 0 {
		return mediaNone, nil
	}
	if len(values) > 1 {
		return 0, contentTypeHeaderError("only one Content-Type header should be provided")
	}
	raw := strings.TrimSpace(values[0])
	first := raw
	if i := strings.IndexByte(raw, ';'); i >= 0 {
		first = strings.TrimRight(raw[:i], " \t")
	}
	parts := strings.Split(first, "/")
	if len(parts) != 2 || !isTokenChars(parts[0]) || !isTokenChars(strings.TrimSpace(parts[1])) {
		return 0, contentTypeHeaderError("invalid Content-Type header [" + raw + "]")
	}
	switch strings.ToLower(parts[0]) + "/" + strings.ToLower(strings.TrimSpace(parts[1])) {
	case "application/json", "application/x-ndjson", "application/vnd.opensearch+json", "application/vnd.opensearch+x-ndjson":
		return mediaJSON, nil
	case "application/smile", "application/yaml", "application/cbor",
		"application/vnd.opensearch+smile", "application/vnd.opensearch+yaml", "application/vnd.opensearch+cbor":
		return mediaOther, nil
	}
	return mediaUnknown, nil
}

func contentTypeHeaderError(msg string) *engine.Error {
	return &engine.Error{Status: http.StatusBadRequest, Type: "content_type_header_exception",
		Reason: "java.lang.IllegalArgumentException: " + msg,
		Cause:  &engine.Error{Type: "illegal_argument_exception", Reason: msg}}
}

// isTokenChars matches RestRequest's TCHAR_PATTERN [a-zA-z0-9!#$%&'*+\-.\^_`|~]+.
func isTokenChars(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0 {
			continue
		}
		return false
	}
	return true
}

// validateChannelParams parses the parameters OpenSearch reads when it
// creates the response channel, before routing.
func validateChannelParams(p engine.Params) *engine.Error {
	for _, name := range []string{"pretty", "human", "error_trace"} {
		if v, ok := p[name]; ok {
			if _, err := engine.ParseBoolValue(v, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// parameter rules

// requestContext is what parameter checks may look at.
type requestContext struct {
	params     engine.Params
	body       []byte
	path       string // decoded request path, as quoted in messages
	vars       map[string]string
	sourceUsed bool // the body came from the source parameter
}

type paramCheck func(c *requestContext, name, value string) *engine.Error

type paramRule struct {
	name     string
	check    paramCheck
	present  bool   // consumed only when present ("?")
	noBody   bool   // consumed only without a body ("!")
	withBody bool   // consumed only with a body ("+")
	requires string // consumed only when this other parameter is present ("q>df")
	// optionalBool: paramAsBoolean(name, null), a blank value is no value
	optionalBool bool
	// zeroIsUnset: 0 leaves the setting unset (terminate_after)
	zeroIsUnset bool
}

type bodyKind int

const (
	bodyAny          bodyKind = iota
	bodySearch                // SearchSourceBuilder: one object, nothing after it
	bodyQuery                 // count, field_caps: blank means no body
	bodyObjectParser          // ObjectParser: x_content_parse_exception
	bodyMget                  // MultiGetRequest parser
	bodyMap                   // XContentHelper.convertToMap
	bodyDocument              // IndexRequest source
	bodySettings              // settings updates
	bodyScroll                // scroll ids, PIT ids
)

// restAPI describes the URL parameters of one OpenSearch REST handler.
type restAPI struct {
	rules []paramRule
	// response params are read while rendering the response: accepted and
	// suggested, not validated before the handler runs.
	response []string
	// source: the body may be passed as source/source_content_type.
	source bool
	// bodyRequired is the parse_exception for a missing body.
	bodyRequired string
	body         bodyKind
	objectName   string // ObjectParser name for bodyObjectParser
	// bodyFirst: OpenSearch parses the body before reading parameters.
	bodyFirst bool
	// prepare runs after the rules, before the unrecognized parameter check.
	prepare func(c *requestContext, consumed map[string]bool) *engine.Error
	// post runs after the unrecognized parameter check (checks OpenSearch
	// makes while executing).
	post func(c *requestContext) *engine.Error
	// render runs after a successful execution (parameters OpenSearch reads
	// while rendering the response).
	render func(c *requestContext) *engine.Error
	// sortParam: the sort parameter drops entries with an unknown order.
	sortParam bool
	// helpShortCircuit: ?help renders the _cat help without reading any
	// other parameter.
	helpShortCircuit bool
}

// parseRules reads rule specs "[?!+][dep>]name[:kind[=arg]]".
func parseRules(specs ...[]string) []paramRule {
	var out []paramRule
	for _, group := range specs {
		for _, spec := range group {
			var r paramRule
			for len(spec) > 0 && strings.IndexByte("?!+", spec[0]) >= 0 {
				switch spec[0] {
				case '?':
					r.present = true
				case '!':
					r.noBody = true
				case '+':
					r.withBody = true
				}
				spec = spec[1:]
			}
			name, kind := spec, ""
			if i := strings.IndexByte(spec, ':'); i >= 0 {
				name, kind = spec[:i], spec[i+1:]
			}
			if i := strings.IndexByte(name, '>'); i >= 0 {
				r.requires, name = name[:i], name[i+1:]
			}
			r.name = name
			switch kind {
			case "optbool":
				r.optionalBool, kind = true, "bool"
			case "ta":
				r.zeroIsUnset = true
			}
			if kind != "" {
				r.check = paramKind(kind)
			}
			out = append(out, r)
		}
	}
	return out
}

func illegalArgument(reason string) *engine.Error {
	return &engine.Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: reason}
}

func noEnumConstant(class, value string, allowed ...string) *engine.Error {
	upper := strings.ToUpper(value)
	for _, a := range allowed {
		if a == upper {
			return nil
		}
	}
	return illegalArgument("No enum constant " + class + "." + upper)
}

// paramKind returns the value check for a kind name.
func paramKind(kind string) paramCheck {
	arg := ""
	if i := strings.IndexByte(kind, '='); i >= 0 {
		kind, arg = kind[:i], kind[i+1:]
	}
	intValue := func(name, value string) (int, *engine.Error) { return engine.ParseIntValue(name, value) }
	switch kind {
	case "bool":
		return func(_ *requestContext, _, v string) *engine.Error {
			_, err := engine.ParseBoolValue(v, false)
			return err
		}
	case "ibool":
		return func(_ *requestContext, name, v string) *engine.Error { return engine.ParseIndicesOptionBool(name, v) }
	case "int":
		return func(_ *requestContext, name, v string) *engine.Error {
			_, err := intValue(name, v)
			return err
		}
	case "long":
		return func(_ *requestContext, name, v string) *engine.Error {
			_, err := engine.ParseLongValue(name, v)
			return err
		}
	case "float":
		return func(_ *requestContext, name, v string) *engine.Error {
			_, err := engine.ParseFloatValue(name, v)
			return err
		}
	case "time":
		return func(_ *requestContext, name, v string) *engine.Error {
			setting := name
			if arg != "" {
				setting = arg
			}
			_, err := engine.ParseTimeValue(setting, v)
			return err
		}
	case "master_timeout":
		return func(c *requestContext, name, v string) *engine.Error {
			if c.params.Has("cluster_manager_timeout") {
				return &engine.Error{Status: http.StatusBadRequest, Type: "parse_exception",
					Reason: "Please only use one of the request parameters [cluster_manager_timeout, master_timeout]."}
			}
			_, err := engine.ParseTimeValue(name, v)
			return err
		}
	case "shards":
		return func(_ *requestContext, _, v string) *engine.Error { return engine.ParseActiveShardCount(v) }
	case "wild":
		return func(_ *requestContext, _, v string) *engine.Error { return engine.ParseExpandWildcards(v) }
	case "refresh":
		return func(_ *requestContext, _, v string) *engine.Error {
			switch v {
			case "", "true", "false", "wait_for":
				return nil
			}
			return illegalArgument("Unknown value for refresh: [" + v + "].")
		}
	case "search_type":
		return func(_ *requestContext, _, v string) *engine.Error {
			switch v {
			case "query_and_fetch", "dfs_query_and_fetch":
				return illegalArgument("Unsupported search type [" + v + "]")
			case "query_then_fetch", "dfs_query_then_fetch":
				return nil
			}
			return illegalArgument("No search type for [" + v + "]")
		}
	case "msearch_type":
		return func(_ *requestContext, _, v string) *engine.Error {
			if v == "query_then_fetch" || v == "dfs_query_then_fetch" {
				return nil
			}
			return illegalArgument("No search type for [" + v + "]")
		}
	case "enum":
		class, values, _ := strings.Cut(arg, ":")
		allowed := strings.Split(values, ",")
		return func(_ *requestContext, _, v string) *engine.Error { return noEnumConstant(class, v, allowed...) }
	case "nonneg":
		return func(_ *requestContext, name, v string) *engine.Error {
			n, err := intValue(name, v)
			if err != nil {
				return err
			}
			label := name
			if arg != "" {
				label = arg
			}
			if n < 0 {
				return illegalArgument("[" + label + "] parameter cannot be negative, found [" + strconv.Itoa(n) + "]")
			}
			return nil
		}
	case "min":
		minText, message, _ := strings.Cut(arg, ":")
		minimum, _ := strconv.Atoi(minText)
		return func(_ *requestContext, name, v string) *engine.Error {
			n, err := intValue(name, v)
			if err != nil {
				return err
			}
			if n < minimum {
				return illegalArgument(message)
			}
			return nil
		}
	case "ta":
		return func(_ *requestContext, name, v string) *engine.Error {
			n, err := intValue(name, v)
			if err != nil {
				return err
			}
			if n < 0 {
				return illegalArgument("terminateAfter must be > 0")
			}
			return nil
		}
	case "tth":
		return func(_ *requestContext, name, v string) *engine.Error {
			if v == "true" || v == "false" {
				return nil
			}
			_, err := intValue(name, v)
			return err
		}
	case "apsr":
		return func(_ *requestContext, _, v string) *engine.Error {
			if strings.TrimSpace(v) == "" {
				return &engine.Error{Status: http.StatusInternalServerError, Type: "null_pointer_exception",
					Reason: `Cannot invoke "java.lang.Boolean.booleanValue()" because the return value of "org.opensearch.rest.RestRequest.paramAsBoolean(String, java.lang.Boolean)" is null`}
			}
			_, err := engine.ParseBoolValue(v, false)
			return err
		}
	case "rthai":
		return checkRestTotalHitsAsInt
	case "optype":
		return func(_ *requestContext, _, v string) *engine.Error {
			switch strings.ToLower(v) {
			case "create", "index":
				return nil
			}
			return illegalArgument("opType must be 'create' or 'index', found: [" + v + "]")
		}
	case "optype_create":
		return func(_ *requestContext, _, v string) *engine.Error {
			if strings.ToLower(v) != "create" {
				return illegalArgument("opType must be 'create', found: [" + v + "]")
			}
			return nil
		}
	case "version_type":
		return func(_ *requestContext, _, v string) *engine.Error {
			switch v {
			case "internal", "external", "external_gt", "external_gte":
				return nil
			}
			return illegalArgument("No version type match [" + v + "]")
		}
	case "conflicts":
		return func(_ *requestContext, _, v string) *engine.Error {
			if v == "proceed" || v == "abort" {
				return nil
			}
			return illegalArgument(`conflicts may only be "proceed" or "abort" but was [` + v + `]`)
		}
	case "slices":
		return func(_ *requestContext, _, v string) *engine.Error {
			if v == "auto" {
				return nil
			}
			msg := `[slices] must be a positive integer or the string "auto", but was [` + v + `]`
			n, err := intValue("slices", v)
			if err != nil {
				e := illegalArgument(msg)
				e.Cause = err.Cause
				return e
			}
			if n < 1 {
				return illegalArgument(msg)
			}
			return nil
		}
	case "rps":
		return func(_ *requestContext, name, v string) *engine.Error {
			msg := "[requests_per_second] must be a float greater than 0. Use -1 to disable throttling."
			f, err := engine.ParseFloatValue(name, v)
			if err != nil {
				e := illegalArgument(msg)
				e.Cause = err.Cause
				return e
			}
			if f != -1 && f <= 0 {
				return illegalArgument(msg)
			}
			return nil
		}
	case "reject":
		return func(_ *requestContext, _, _ string) *engine.Error { return illegalArgument(arg) }
	case "validation":
		return func(_ *requestContext, _, _ string) *engine.Error {
			return &engine.Error{Status: http.StatusBadRequest, Type: "action_request_validation_exception", Reason: "Validation Failed: 1: " + arg + ";"}
		}
	case "suggest_field":
		return func(_ *requestContext, _, v string) *engine.Error {
			if v == "" {
				return illegalArgument("suggestion field name is empty")
			}
			return nil
		}
	}
	panic("osmem: unknown parameter kind " + kind)
}

// checkRestTotalHitsAsInt is RestSearchAction.checkRestTotalHits.
func checkRestTotalHitsAsInt(c *requestContext, name, v string) *engine.Error {
	on, err := engine.ParseBoolValue(v, false)
	if err != nil || !on {
		return err
	}
	const accurate, disabled = 2147483647, -1
	var upTo *int64
	set := func(n int64) { upTo = &n }
	if m, decodeErr := engine.DecodeObject(c.body); decodeErr == nil {
		switch t := m["track_total_hits"].(type) {
		case bool:
			if t {
				set(accurate)
			} else {
				set(disabled)
			}
		case json.Number:
			if n, e := t.Int64(); e == nil {
				set(n)
			}
		}
	}
	if tth, ok := c.params["track_total_hits"]; ok {
		switch tth {
		case "true":
			set(accurate)
		case "false":
			set(disabled)
		default:
			if n, e := strconv.ParseInt(tth, 10, 32); e == nil {
				set(n)
			}
		}
	}
	if upTo != nil && *upTo != accurate && *upTo != disabled {
		return illegalArgument(fmt.Sprintf("[rest_total_hits_as_int] cannot be used if the tracking of total hits is not accurate, got %d", *upTo))
	}
	return nil
}

// validate applies the rules of the API to a request.
func (api *restAPI) validate(c *requestContext) *engine.Error {
	consumed := map[string]bool{"format": true, "filter_path": true, "pretty": true, "human": true, "error_trace": true}
	hasBody := len(c.body) > 0
	for _, r := range api.rules {
		if (r.noBody && hasBody) || (r.withBody && !hasBody) {
			continue
		}
		if r.requires != "" && !c.params.Has(r.requires) {
			continue
		}
		value, present := c.params[r.name]
		if r.present && !present {
			continue
		}
		consumed[r.name] = true
		if present && r.check != nil {
			if err := r.check(c, r.name, value); err != nil {
				return err
			}
		}
	}
	if c.sourceUsed {
		consumed["source"], consumed["source_content_type"] = true, true
	}
	if api.prepare != nil {
		if err := api.prepare(c, consumed); err != nil {
			return err
		}
	}
	if api.bodyRequired != "" && !hasBody {
		return &engine.Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: api.bodyRequired}
	}
	for _, name := range api.response {
		consumed[name] = true
	}
	var unknown []string
	for name := range c.params {
		if !consumed[name] {
			unknown = append(unknown, name)
		}
	}
	if api.helpShortCircuit {
		// _cat help answers before the handler reads its parameters, path
		// parameters included
		if help, ok := c.params["help"]; ok {
			if on, _ := engine.ParseBoolValue(help, false); on {
				consumed = map[string]bool{"format": true, "filter_path": true, "pretty": true, "human": true, "error_trace": true, "help": true}
				for _, name := range api.response {
					consumed[name] = true
				}
				unknown = unknown[:0]
				for name := range c.params {
					if !consumed[name] {
						unknown = append(unknown, name)
					}
				}
				for name := range c.vars {
					if !consumed[name] {
						unknown = append(unknown, name)
					}
				}
			}
		}
	}
	if len(unknown) > 0 {
		candidates := make([]string, 0, len(consumed))
		for name := range consumed {
			candidates = append(candidates, name)
		}
		sort.Strings(candidates)
		return engine.UnrecognizedError(c.path, unknown, candidates, "parameter")
	}
	if api.post != nil {
		return api.post(c)
	}
	return nil
}

// bodyShapeError reports a body that is not a JSON object the way the
// handler's parser does.
func (api *restAPI) bodyShapeError(body []byte) *engine.Error {
	if len(body) == 0 || api.body == bodyAny || api.body == bodySearch {
		return nil
	}
	tok := engine.FirstJSONToken(body, 0)
	if tok.Name == "START_OBJECT" {
		return nil
	}
	notXContent := &engine.Error{Status: http.StatusBadRequest, Type: "not_x_content_exception",
		Reason: "Compressor detection can only be called on some xcontent bytes or compressed xcontent bytes"}
	switch api.body {
	case bodyMap:
		return notXContent
	case bodyDocument:
		return &engine.Error{Status: http.StatusBadRequest, Type: "mapper_parsing_exception", Reason: "failed to parse", Cause: notXContent}
	case bodySettings:
		return &engine.Error{Status: http.StatusBadRequest, Type: "action_request_validation_exception", Reason: "Validation Failed: 1: no settings to update;"}
	case bodyScroll:
		return illegalArgument("Malformed content, must start with an object")
	}
	if tok.Name == "" {
		return nil // a malformed token: the JSON decoder reports it
	}
	switch api.body {
	case bodyQuery:
		if tok.Name == "null" {
			return nil
		}
		return &engine.Error{Status: http.StatusBadRequest, Type: "parsing_exception",
			Reason: "Expected [START_OBJECT] but found [" + tok.Name + "]", Extra: map[string]any{"line": tok.Line, "col": tok.Col}}
	case bodyMget:
		return &engine.Error{Status: http.StatusBadRequest, Type: "parsing_exception",
			Reason: "unexpected token [" + tok.Name + "], expected [START_OBJECT]", Extra: map[string]any{"line": tok.Line, "col": tok.Col}}
	case bodyObjectParser:
		return &engine.Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception",
			Reason: fmt.Sprintf("[%d:%d] [%s] Expected START_OBJECT but was: %s", tok.Line, tok.Col, api.objectName, tok.Name)}
	}
	return nil
}

// ---------------------------------------------------------------------------
// APIs

var (
	indicesOptionRules  = []string{"expand_wildcards:wild", "ignore_unavailable:ibool", "allow_no_indices:ibool", "ignore_throttled:ibool"}
	clusterManagerRules = []string{"cluster_manager_timeout:time", "?master_timeout:master_timeout"}
	queryStringRules    = []string{"q>df", "q>analyzer", "q>analyze_wildcard:bool", "q>lenient:optbool", "q>default_operator:enum=org.opensearch.index.query.Operator:OR,AND"}
	catResponseParams   = []string{"format", "h", "v", "ts", "pri", "bytes", "size", "time", "s", "timeout"}
	settingsFormat      = []string{"settings_filter", "flat_settings"}
)

func searchRules(sizeKind string) []string {
	return []string{
		"index",
		"batched_reduce_size:min=2:batchedReduceSize must be >= 2",
		"?pre_filter_shard_size:min=1:preFilterShardSize must be >= 1",
		"?max_concurrent_shard_requests:min=1:maxConcurrentShardRequests must be >= 1",
		"?allow_partial_search_results:apsr",
		"?phase_took:bool",
		"search_type:search_type",
		"q", "q>df", "q>analyzer", "q>analyze_wildcard:bool", "q>lenient:optbool", "q>default_operator:enum=org.opensearch.index.query.Operator:OR,AND",
		"?from:nonneg", "?size:" + sizeKind, "?explain:optbool", "?version:optbool", "?seq_no_primary_term:optbool",
		"?timeout:time", "?verbose_pipeline:bool", "?terminate_after:ta",
		"stored_fields", "docvalue_fields", "_source", "_source_includes", "_source_excludes",
		"?track_scores:bool", "?include_named_queries_score:bool", "?track_total_hits:tth",
		"sort", "stats",
		"suggest_field:suggest_field", "suggest_field>suggest_text", "suggest_field>suggest_size:int",
		"suggest_field>suggest_mode:enum=org.opensearch.search.suggest.term.TermSuggestionBuilder.SuggestMode:MISSING,POPULAR,ALWAYS",
		"request_cache:bool", "scroll:time", "routing", "preference",
		"expand_wildcards:wild", "ignore_unavailable:ibool", "allow_no_indices:ibool", "ignore_throttled:ibool",
		"search_pipeline", "rest_total_hits_as_int:rthai", "include_named_queries_score:bool",
		"ccs_minimize_roundtrips:bool", "cancel_after_time_interval:time",
	}
}

// prepareSearch covers checks RestSearchAction makes while reading
// parameters that do not belong to a single parameter.
func prepareSearch(c *requestContext, consumed map[string]bool) *engine.Error {
	if c.params.Get("suggest_field") != "" && !c.params.Has("suggest_mode") {
		// SuggestMode.resolve(null)
		return &engine.Error{Status: http.StatusInternalServerError, Type: "null_pointer_exception", Reason: "Input string is null"}
	}
	return nil
}

// searchPost rejects what osmem cannot execute the way OpenSearch fails it.
func searchPost(c *requestContext) *engine.Error {
	if v := c.params.Get("search_pipeline"); v != "" && v != "_none" {
		return illegalArgument("Pipeline " + v + " is not defined")
	}
	if v, ok := c.params["verbose_pipeline"]; ok && c.params.Get("search_pipeline") == "" {
		if on, _ := engine.ParseBoolValue(v, false); on {
			return illegalArgument("The 'verbose pipeline' option requires a search pipeline to be defined.")
		}
	}
	if c.params.Get("suggest_field") != "" {
		return &engine.Error{Status: http.StatusBadRequest, Type: "unsupported_operation_exception", Reason: "suggest is not supported by osmem"}
	}
	return nil
}

func ingestPipelinePost(c *requestContext) *engine.Error {
	if v := c.params.Get("pipeline"); v != "" && v != "_none" {
		return illegalArgument("pipeline with id [" + v + "] does not exist")
	}
	return nil
}

var statsMetrics = []string{"store", "indexing", "get", "search", "merge", "flush", "refresh", "query_cache", "fielddata",
	"docs", "warmer", "completion", "segments", "translog", "request_cache", "recovery"}

// prepareStats is the metric handling of RestIndicesStatsAction.
func prepareStats(c *requestContext, consumed map[string]bool) *engine.Error {
	metric, ok := c.vars["metric"]
	if !ok {
		metric = "_all"
		if v, present := c.params["metric"]; present {
			metric = v
		}
	}
	consumed["metric"] = true
	set := map[string]bool{}
	for _, m := range strings.Split(metric, ",") {
		if m = strings.TrimSpace(m); m != "" {
			set[m] = true
		}
	}
	all := len(set) == 1 && set["_all"]
	if !all {
		if set["_all"] {
			return illegalArgument("request [" + c.path + "] contains _all and individual metrics [" + metric + "]")
		}
		var invalid []string
		for m := range set {
			if !contains(statsMetrics, m) {
				invalid = append(invalid, m)
			}
		}
		if len(invalid) > 0 {
			return engine.UnrecognizedError(c.path, invalid, statsMetrics, "metric")
		}
	}
	enabled := func(m string) bool { return all || set[m] }
	if c.params.Has("groups") {
		consumed["groups"] = true
	}
	if enabled("completion") && (c.params.Has("fields") || c.params.Has("completion_fields")) {
		consumed["completion_fields"], consumed["fields"] = true, true
	}
	if enabled("fielddata") && (c.params.Has("fields") || c.params.Has("fielddata_fields")) {
		consumed["fielddata_fields"], consumed["fields"] = true, true
	}
	if enabled("segments") {
		for _, name := range []string{"include_segment_file_sizes", "include_unloaded_segments"} {
			consumed[name] = true
			if v, present := c.params[name]; present {
				if _, err := engine.ParseBoolValue(v, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func flatSettingsRender(c *requestContext) *engine.Error {
	if v, ok := c.params["flat_settings"]; ok {
		if _, err := engine.ParseBoolValue(v, false); err != nil {
			return err
		}
	}
	return nil
}

func statsPost(c *requestContext) *engine.Error {
	if level, ok := c.params["level"]; ok {
		switch level {
		case "cluster", "indices", "shards":
		default:
			return illegalArgument("level parameter must be one of [cluster] or [indices] or [shards] but was [" + level + "]")
		}
	}
	return nil
}

func catPost(c *requestContext) *engine.Error {
	if v, ok := c.params["v"]; ok {
		if _, err := engine.ParseBoolValue(v, false); err != nil {
			return err
		}
	}
	return nil
}

func catIndicesPost(c *requestContext) *engine.Error {
	if v, ok := c.params["health"]; ok {
		switch strings.ToLower(v) {
		case "green", "yellow", "red":
		default:
			return illegalArgument("unknown cluster health status [" + v + "]")
		}
	}
	return catPost(c)
}

func prepareFieldCaps(c *requestContext, consumed map[string]bool) *engine.Error {
	consumed["fields"] = true
	if len(engine.JavaSplitComma(c.params.Get("fields"))) == 0 {
		return illegalArgument("specified fields can't be null or empty")
	}
	// the body only takes index_filter (malformed bodies fail in checkBody)
	if engine.FirstJSONToken(c.body, 0).Name == "START_OBJECT" {
		if _, err := engine.DecodeObject(c.body); err != nil {
			return nil
		}
		for _, key := range engine.TopLevelKeys(c.body) {
			if key.Name != "index_filter" {
				return &engine.Error{Status: http.StatusBadRequest, Type: "parsing_exception", Reason: "request does not support [" + key.Name + "]",
					Extra: map[string]any{"line": key.Line, "col": key.Col}}
			}
		}
	}
	return nil
}

func catAPI(rules ...string) *restAPI {
	return &restAPI{rules: parseRules([]string{"help:bool"}, rules), response: catResponseParams, render: catPost, helpShortCircuit: true}
}

func join(groups ...[]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

const (
	bodyRequired         = "request body is required"
	bodyOrSourceRequired = "request body or source parameter is required"
)

var restAPIs = map[string]*restAPI{
	"info": {},
	"cluster.health": {rules: parseRules([]string{"index"}, indicesOptionRules, []string{"local:bool"}, clusterManagerRules, []string{
		"timeout:time", "wait_for_status:enum=org.opensearch.cluster.health.ClusterHealthStatus:GREEN,YELLOW,RED",
		"wait_for_no_relocating_shards:bool", "wait_for_no_initializing_shards:bool",
		"?wait_for_relocating_shards:reject=wait_for_relocating_shards has been removed, use wait_for_no_relocating_shards [true/false] instead",
		"wait_for_active_shards:shards", "wait_for_nodes",
		"wait_for_events:enum=org.opensearch.common.Priority:IMMEDIATE,URGENT,HIGH,NORMAL,LOW,LANGUID",
		"level", "awareness_attribute", "ensure_node_weighed_in:bool",
	})},
	"cluster.get_settings": {rules: parseRules([]string{"local:bool"}, clusterManagerRules, []string{"include_defaults:bool"}), response: settingsFormat, render: flatSettingsRender},
	"cluster.put_settings": {rules: parseRules([]string{"flat_settings"}, clusterManagerRules, []string{"timeout:time"}), response: settingsFormat,
		bodyRequired: bodyRequired, body: bodySettings},
	"cluster.state": {rules: parseRules([]string{"local:bool"}, clusterManagerRules, []string{"?wait_for_metadata_version:long", "wait_for_timeout:time", "indices", "metric"}, indicesOptionRules),
		response: []string{"metric", "settings_filter", "flat_settings"}},
	"cluster.stats": {rules: parseRules([]string{"nodeId", "metric", "index_metric", "timeout:time=ClusterStatsRequest.timeout"})},
	"nodes.info":    {rules: parseRules([]string{"nodeId", "metrics", "timeout:time=NodesInfoRequest.timeout"}), response: settingsFormat},
	"cat.help":      {},
	"cat.indices": {rules: parseRules([]string{"help:bool", "index"}, indicesOptionRules, []string{"local:bool"}, clusterManagerRules,
		[]string{"health", "include_unloaded_segments:bool"}), response: catResponseParams, render: catIndicesPost, helpShortCircuit: true},
	"cat.aliases":         catAPI(join([]string{"alias", "local:bool"}, indicesOptionRules)...),
	"cat.health":          catAPI(),
	"cat.count":           {rules: parseRules([]string{"help:bool", "index", "!q"}, prefix("!", queryStringRules)), response: catResponseParams, source: true, render: catPost, helpShortCircuit: true},
	"cat.nodes":           catAPI(join([]string{"full_id:bool", "local:bool"}, clusterManagerRules)...),
	"cat.cluster_manager": catAPI(join([]string{"local:bool"}, clusterManagerRules)...),
	"cat.plugins":         catAPI(join([]string{"local:bool"}, clusterManagerRules)...),
	"cat.templates":       catAPI(join([]string{"name", "local:bool"}, clusterManagerRules)...),
	// cat APIs added with the admin work (parameters from the 3.8 REST specs;
	// bytes, size and time are cat response parameters)
	"cat.shards":     catAPI(join([]string{"index", "local:bool"}, clusterManagerRules)...),
	"cat.segments":   catAPI(join([]string{"index"}, clusterManagerRules)...),
	"cat.recovery":   catAPI("index", "active_only:bool", "detailed:bool"),
	"cat.allocation": catAPI(join([]string{"nodes", "local:bool"}, clusterManagerRules)...),
	// thread_pool reads its path parameter before answering ?help
	"cat.thread_pool": {rules: parseRules([]string{"help:bool", "thread_pool_patterns", "local:bool"}, clusterManagerRules),
		response: join(catResponseParams, []string{"thread_pool_patterns"}), render: catPost, helpShortCircuit: true},
	"cat.pending_tasks":       catAPI(join([]string{"local:bool"}, clusterManagerRules)...),
	"cat.fielddata":           catAPI("fields"),
	"cat.nodeattrs":           catAPI(join([]string{"local:bool"}, clusterManagerRules)...),
	"cat.tasks":               catAPI("nodes", "actions", "detailed:bool", "parent_task_id"),
	"cat.repositories":        catAPI(join([]string{"local:bool"}, clusterManagerRules)...),
	"cat.snapshots":           catAPI(join([]string{"repository", "ignore_unavailable:bool"}, clusterManagerRules)...),
	"cat.segment_replication": catAPI(join([]string{"index", "active_only:bool", "detailed:bool", "shards"}, indicesOptionRules)...),

	"indices.update_aliases": {rules: parseRules(clusterManagerRules, []string{"timeout:time"}), bodyRequired: bodyRequired, body: bodyObjectParser, objectName: "aliases"},
	"indices.get_alias":      {rules: parseRules([]string{"name", "index", "local:bool"}, indicesOptionRules, clusterManagerRules)},
	"indices.put_alias":      {rules: parseRules([]string{"index", "name", "timeout:time"}, clusterManagerRules)},
	"indices.delete_alias":   {rules: parseRules([]string{"index", "name", "timeout:time"}, clusterManagerRules)},

	"indices.simulate_template":       {rules: parseRules([]string{"name", "+create:bool", "+cause"}, clusterManagerRules), body: bodyObjectParser, objectName: "index_template"},
	"indices.simulate_index_template": {rules: parseRules([]string{"name"}, clusterManagerRules, []string{"+create:bool", "+cause"}), body: bodyObjectParser, objectName: "index_template"},
	"indices.put_index_template": {rules: parseRules([]string{"name"}, clusterManagerRules, []string{"create:bool", "cause"}),
		bodyRequired: bodyRequired, body: bodyObjectParser, objectName: "index_template"},
	"indices.get_index_template":    {rules: parseRules([]string{"name", "local:bool"}, clusterManagerRules), response: settingsFormat, render: flatSettingsRender},
	"indices.delete_index_template": {rules: parseRules([]string{"name"}, clusterManagerRules)},
	"indices.put_template": {rules: parseRules([]string{"name", "?template", "!index_patterns", "order:int"}, clusterManagerRules, []string{"create:bool", "cause"}),
		bodyRequired: bodyRequired, body: bodyMap},
	"indices.get_template":              {rules: parseRules([]string{"name", "local:bool"}, clusterManagerRules), response: settingsFormat, render: flatSettingsRender},
	"indices.delete_template":           {rules: parseRules([]string{"name"}, clusterManagerRules)},
	"cluster.put_component_template":    {rules: parseRules([]string{"name"}, clusterManagerRules, []string{"create:bool", "cause"}), bodyRequired: bodyRequired},
	"cluster.get_component_template":    {rules: parseRules([]string{"name", "local:bool"}, clusterManagerRules), response: settingsFormat, render: flatSettingsRender},
	"cluster.delete_component_template": {rules: parseRules([]string{"name"}, clusterManagerRules)},

	"bulk": {rules: parseRules([]string{"index", "routing", "pipeline", "wait_for_active_shards:shards", "require_alias:bool",
		"timeout:time", "refresh:refresh", "_source", "_source_includes", "_source_excludes"}), bodyRequired: bodyRequired},
	"search": {rules: parseRules(searchRules("nonneg")), response: []string{"typed_keys", "rest_total_hits_as_int", "include_named_queries_score"},
		source: true, body: bodySearch, bodyFirst: true, prepare: prepareSearch, post: searchPost, sortParam: true},
	"count": {rules: parseRules([]string{"index"}, indicesOptionRules, []string{"!q"}, prefix("!", queryStringRules),
		[]string{"routing", "min_score:float", "preference", "terminate_after:ta"}), source: true, body: bodyQuery, bodyFirst: true},
	"field_caps": {rules: parseRules([]string{"index"}, indicesOptionRules, []string{"include_unmapped:bool"}), source: true, body: bodyQuery, prepare: prepareFieldCaps},
	"msearch": {rules: parseRules(indicesOptionRules, []string{
		"?max_concurrent_searches:min=1:maxConcurrentSearchRequests must be positive",
		"?pre_filter_shard_size:min=1:preFilterShardSize must be >= 1",
		"?max_concurrent_shard_requests:min=1:maxConcurrentShardRequests must be >= 1",
		"index", "search_type:msearch_type", "ccs_minimize_roundtrips:bool", "routing",
		"rest_total_hits_as_int:rthai", "cancel_after_time_interval:time",
	}), response: []string{"typed_keys", "rest_total_hits_as_int"}, source: true, bodyRequired: bodyOrSourceRequired},
	"mget": {rules: parseRules([]string{"refresh:bool", "preference", "realtime:bool",
		"?fields:reject=The parameter [fields] is no longer supported, please use [stored_fields] to retrieve stored fields or _source filtering if the field is not stored",
		"stored_fields", "_source", "_source_includes", "_source_excludes", "index", "routing"}),
		source: true, bodyRequired: bodyOrSourceRequired, body: bodyMget},
	"scroll":       {rules: parseRules([]string{"scroll_id", "scroll:time"}), response: []string{"rest_total_hits_as_int"}, source: true, body: bodyScroll},
	"clear_scroll": {rules: parseRules([]string{"scroll_id"}), source: true, body: bodyScroll},
	"delete_pit":   {body: bodyScroll},
	"get_all_pits": {},
	"create_pit":   {rules: parseRules([]string{"allow_partial_pit_creation:bool", "index", "keep_alive:time"}, indicesOptionRules, []string{"preference", "routing"})},

	"indices.refresh":     {rules: parseRules([]string{"index"}, indicesOptionRules)},
	"indices.flush":       {rules: parseRules([]string{"index"}, indicesOptionRules, []string{"force:bool", "wait_if_ongoing:bool"})},
	"indices.forcemerge":  {rules: parseRules([]string{"index"}, indicesOptionRules, []string{"max_num_segments:int", "only_expunge_deletes:bool", "flush:bool", "primary_only:bool", "wait_for_completion:bool"})},
	"indices.clear_cache": {rules: parseRules([]string{"index"}, indicesOptionRules, []string{"query:bool", "request:bool", "fielddata:bool", "file:bool", "fields"})},
	"reindex": {rules: parseRules([]string{
		"?pipeline:reject=_reindex doesn't support [pipeline] as a query parameter. Specify it in the [dest] object instead.",
		"?scroll:time", "?require_alias:bool", "refresh:bool", "timeout:time", "slices:slices", "wait_for_active_shards:shards",
		"requests_per_second:rps", "?max_docs:int", "wait_for_completion:bool",
	}), bodyRequired: bodyRequired, body: bodyObjectParser, objectName: "reindex"},
	"delete_by_query": {rules: parseRules(searchRules("int"), []string{"scroll_size:nonneg=size", "conflicts:conflicts", "?search_timeout:time",
		"refresh:bool", "timeout:time", "slices:slices", "wait_for_active_shards:shards", "requests_per_second:rps", "?max_docs:int", "wait_for_completion:bool"}),
		response: []string{"typed_keys", "rest_total_hits_as_int", "include_named_queries_score"}},
	"update_by_query": {rules: parseRules(searchRules("int"), []string{"scroll_size:nonneg=size", "conflicts:conflicts", "?search_timeout:time", "pipeline",
		"refresh:bool", "timeout:time", "slices:slices", "wait_for_active_shards:shards", "requests_per_second:rps", "?max_docs:int", "wait_for_completion:bool"}),
		response: []string{"typed_keys", "rest_total_hits_as_int", "include_named_queries_score"}},
	"indices.analyze": {rules: parseRules([]string{"index"}), source: true, bodyRequired: bodyOrSourceRequired, body: bodyObjectParser, objectName: "analyze_request"},

	"indices.get_mapping":       {rules: parseRules([]string{"index"}, indicesOptionRules, []string{"local:bool"}, clusterManagerRules)},
	"indices.put_mapping":       {rules: parseRules([]string{"index"}, indicesOptionRules, []string{"timeout:time"}, clusterManagerRules, []string{"write_index_only:bool"}), bodyRequired: bodyRequired, body: bodyMap},
	"indices.get_field_mapping": {rules: parseRules([]string{"index", "fields"}, indicesOptionRules, []string{"include_defaults:bool", "local:bool"})},
	"indices.get_settings":      {rules: parseRules([]string{"index", "name"}, indicesOptionRules, []string{"flat_settings:bool", "include_defaults:bool", "local:bool"}, clusterManagerRules)},
	"indices.put_settings": {rules: parseRules([]string{"index"}, indicesOptionRules, []string{"timeout:time"}, clusterManagerRules, []string{"preserve_existing:bool"}),
		response: settingsFormat, bodyRequired: bodyRequired, body: bodySettings},
	"indices.stats": {rules: parseRules([]string{"timeout:time=IndicesStatsRequest.timeout", "forbid_closed_indices:bool"}, indicesOptionRules, []string{"index"}),
		response: []string{"level"}, prepare: prepareStats, render: statsPost},
	"indices.resolve_index": {rules: parseRules([]string{"name"}, indicesOptionRules)},
	"indices.validate_query": {rules: parseRules([]string{"index"}, indicesOptionRules, []string{"explain:bool", "rewrite:bool", "all_shards:bool", "!?q"}, prefix("!", queryStringRules)),
		source: true},
	"indices.get":    {rules: parseRules([]string{"index", "local:bool"}, indicesOptionRules, []string{"include_defaults:bool"}, clusterManagerRules), response: settingsFormat, render: flatSettingsRender},
	"indices.create": {rules: parseRules([]string{"index", "wait_for_active_shards:shards", "timeout:time"}, clusterManagerRules), body: bodyMap},
	"indices.delete": {rules: parseRules([]string{"index", "timeout:time"}, clusterManagerRules, indicesOptionRules)},
	"indices.open": {rules: parseRules([]string{"index", "timeout:time"}, clusterManagerRules, indicesOptionRules,
		[]string{"wait_for_active_shards:shards", "wait_for_completion:bool", "?task_execution_timeout:time"})},
	"indices.close": {rules: parseRules([]string{"index", "timeout:time"}, clusterManagerRules, indicesOptionRules, []string{"wait_for_active_shards:shards"})},

	"index": {rules: parseRules([]string{"index", "id", "routing", "pipeline", "timeout:time", "refresh:refresh", "?version:long",
		"version_type:version_type", "if_seq_no:long", "if_primary_term:long", "require_alias:bool", "op_type:optype", "wait_for_active_shards:shards"}),
		bodyRequired: bodyRequired, body: bodyDocument, post: ingestPipelinePost},
	"create": {rules: parseRules([]string{"?op_type:optype_create", "index", "id", "routing", "pipeline", "timeout:time", "refresh:refresh", "?version:long",
		"version_type:version_type", "if_seq_no:long", "if_primary_term:long", "require_alias:bool", "wait_for_active_shards:shards"}),
		bodyRequired: bodyRequired, body: bodyDocument, post: ingestPipelinePost},
	"get": {rules: parseRules([]string{"index", "id", "refresh:bool", "routing", "preference", "realtime:bool",
		"?fields:reject=the parameter [fields] is no longer supported, please use [stored_fields] to retrieve stored fields or [_source] to load the field from _source",
		"stored_fields", "?version:long", "version_type:version_type", "_source", "_source_includes", "_source_excludes"})},
	"delete": {rules: parseRules([]string{"index", "id", "routing", "timeout:time", "refresh:refresh", "?version:long", "version_type:version_type",
		"if_seq_no:long", "if_primary_term:long", "wait_for_active_shards:shards"})},
	"update": {rules: parseRules([]string{"index", "id", "routing", "timeout:time", "refresh:refresh", "wait_for_active_shards:shards", "doc_as_upsert:bool",
		"_source", "_source_includes", "_source_excludes", "retry_on_conflict:int",
		"?version:validation=internal versioning can not be used for optimistic concurrency control. Please use `if_seq_no` and `if_primary_term` instead",
		"?version_type:validation=internal versioning can not be used for optimistic concurrency control. Please use `if_seq_no` and `if_primary_term` instead",
		"if_seq_no:long", "if_primary_term:long", "require_alias:bool"}), body: bodyObjectParser, objectName: "UpdateRequest"},
	"get_source": {rules: parseRules([]string{"index", "id", "refresh:bool", "routing", "preference", "realtime:bool", "_source", "_source_includes", "_source_excludes"})},
	"explain": {rules: parseRules([]string{"index", "id", "routing", "preference", "stored_fields", "_source", "_source_includes", "_source_excludes", "!?q"}, prefix("!", queryStringRules)),
		source: true},
}

func prefix(p string, specs []string) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = p + s
	}
	return out
}

// routeAPIs maps "METHOD pattern" of osmem routes to the OpenSearch handler
// whose parameters they take. Routes without an entry accept any parameter.
var routeAPIs = map[string]string{
	"GET /": "info", "HEAD /": "info",
	"GET /_cluster/health": "cluster.health", "GET /_cluster/health/{index}": "cluster.health",
	"GET /_cluster/settings": "cluster.get_settings", "PUT /_cluster/settings": "cluster.put_settings",
	"GET /_cluster/state": "cluster.state", "GET /_cluster/state/{metric}": "cluster.state", "GET /_cluster/state/{metric}/{index}": "cluster.state", "GET /_cluster/stats": "cluster.stats",
	"GET /_nodes": "nodes.info", "GET /_nodes/{a}": "nodes.info", "GET /_nodes/{a}/{b}": "nodes.info",
	"GET /_cat":         "cat.help",
	"GET /_cat/indices": "cat.indices", "GET /_cat/indices/{index}": "cat.indices",
	"GET /_cat/aliases": "cat.aliases", "GET /_cat/aliases/{name}": "cat.aliases",
	"GET /_cat/aliases/{alias}": "cat.aliases",
	"GET /_cat/shards":          "cat.shards", "GET /_cat/shards/{index}": "cat.shards",
	"GET /_cat/segments": "cat.segments", "GET /_cat/segments/{index}": "cat.segments",
	"GET /_cat/recovery": "cat.recovery", "GET /_cat/recovery/{index}": "cat.recovery",
	"GET /_cat/allocation": "cat.allocation", "GET /_cat/allocation/{nodes}": "cat.allocation",
	"GET /_cat/thread_pool": "cat.thread_pool", "GET /_cat/thread_pool/{thread_pool_patterns}": "cat.thread_pool",
	"GET /_cat/pending_tasks": "cat.pending_tasks",
	"GET /_cat/fielddata":     "cat.fielddata", "GET /_cat/fielddata/{fields}": "cat.fielddata",
	"GET /_cat/nodeattrs": "cat.nodeattrs", "GET /_cat/tasks": "cat.tasks", "GET /_cat/repositories": "cat.repositories",
	"GET /_cat/snapshots": "cat.snapshots", "GET /_cat/snapshots/{repository}": "cat.snapshots",
	"GET /_cat/segment_replication": "cat.segment_replication", "GET /_cat/segment_replication/{index}": "cat.segment_replication",
	"GET /_nodes/{a}/{b}/{c}": "nodes.info",
	"POST /_flush/synced":     "indices.flush", "GET /_flush/synced": "indices.flush", "POST /{index}/_flush/synced": "indices.flush", "GET /{index}/_flush/synced": "indices.flush",
	"PUT /{index}/": "indices.create", "POST /{index}/": "indices.create", "DELETE /{index}/": "indices.delete",
	"HEAD /{index}/_alias": "indices.get_alias",
	"GET /_cat/health":     "cat.health",
	"GET /_cat/count":      "cat.count", "GET /_cat/count/{index}": "cat.count",
	"GET /_cat/nodes":  "cat.nodes",
	"GET /_cat/master": "cat.cluster_manager", "GET /_cat/cluster_manager": "cat.cluster_manager",
	"GET /_cat/plugins":   "cat.plugins",
	"GET /_cat/templates": "cat.templates", "GET /_cat/templates/{name}": "cat.templates",

	"POST /_aliases": "indices.update_aliases",
	"GET /_alias":    "indices.get_alias", "GET /_aliases": "indices.get_alias", "GET /_alias/{name}": "indices.get_alias", "HEAD /_alias/{name}": "indices.get_alias",
	"GET /{index}/_alias/{name}": "indices.get_alias", "HEAD /{index}/_alias/{name}": "indices.get_alias", "GET /{index}/_alias": "indices.get_alias",
	"GET /{index}/_aliases/{name}": "indices.get_alias", "HEAD /{index}/_aliases/{name}": "indices.get_alias", "GET /{index}/_aliases": "indices.get_alias",
	"PUT /{index}/_alias/{name}": "indices.put_alias", "POST /{index}/_alias/{name}": "indices.put_alias",
	"PUT /{index}/_aliases/{name}": "indices.put_alias", "POST /{index}/_aliases/{name}": "indices.put_alias",
	"PUT /{index}/_alias": "indices.put_alias", "PUT /{index}/_aliases": "indices.put_alias",
	"PUT /_alias/{name}": "indices.put_alias", "POST /_alias/{name}": "indices.put_alias", "PUT /_aliases/{name}": "indices.put_alias", "POST /_aliases/{name}": "indices.put_alias", "PUT /_alias": "indices.put_alias",
	"DELETE /{index}/_alias/{name}": "indices.delete_alias", "DELETE /{index}/_aliases/{name}": "indices.delete_alias",

	"POST /_index_template/_simulate": "indices.simulate_template", "POST /_index_template/_simulate/{name}": "indices.simulate_template",
	"POST /_index_template/_simulate_index/{index}": "indices.simulate_index_template",
	"PUT /_index_template/{name}":                   "indices.put_index_template", "POST /_index_template/{name}": "indices.put_index_template",
	"GET /_index_template": "indices.get_index_template", "GET /_index_template/{name}": "indices.get_index_template", "HEAD /_index_template/{name}": "indices.get_index_template",
	"DELETE /_index_template/{name}": "indices.delete_index_template",
	"PUT /_template/{name}":          "indices.put_template", "POST /_template/{name}": "indices.put_template",
	"GET /_template": "indices.get_template", "GET /_template/{name}": "indices.get_template", "HEAD /_template/{name}": "indices.get_template",
	"DELETE /_template/{name}":        "indices.delete_template",
	"PUT /_component_template/{name}": "cluster.put_component_template", "POST /_component_template/{name}": "cluster.put_component_template",
	"GET /_component_template": "cluster.get_component_template", "GET /_component_template/{name}": "cluster.get_component_template",
	"HEAD /_component_template/{name}": "cluster.get_component_template", "DELETE /_component_template/{name}": "cluster.delete_component_template",

	"POST /_bulk": "bulk", "PUT /_bulk": "bulk", "POST /{index}/_bulk": "bulk", "PUT /{index}/_bulk": "bulk",
	"GET /_search": "search", "POST /_search": "search", "GET /{index}/_search": "search", "POST /{index}/_search": "search",
	"GET /_count": "count", "POST /_count": "count", "GET /{index}/_count": "count", "POST /{index}/_count": "count",
	"GET /_field_caps": "field_caps", "POST /_field_caps": "field_caps", "GET /{index}/_field_caps": "field_caps", "POST /{index}/_field_caps": "field_caps",
	"GET /_msearch": "msearch", "POST /_msearch": "msearch", "GET /{index}/_msearch": "msearch", "POST /{index}/_msearch": "msearch",
	"GET /_mget": "mget", "POST /_mget": "mget", "GET /{index}/_mget": "mget", "POST /{index}/_mget": "mget",
	"GET /_search/scroll": "scroll", "POST /_search/scroll": "scroll", "GET /_search/scroll/{id}": "scroll", "POST /_search/scroll/{id}": "scroll",
	"DELETE /_search/scroll": "clear_scroll", "DELETE /_search/scroll/{id}": "clear_scroll",
	"DELETE /_search/point_in_time": "delete_pit", "DELETE /_search/point_in_time/_all": "delete_pit",
	"GET /_search/point_in_time/_all":     "get_all_pits",
	"POST /{index}/_search/point_in_time": "create_pit",

	"POST /_refresh": "indices.refresh", "GET /_refresh": "indices.refresh", "POST /{index}/_refresh": "indices.refresh", "GET /{index}/_refresh": "indices.refresh",
	"POST /_flush": "indices.flush", "GET /_flush": "indices.flush", "POST /{index}/_flush": "indices.flush", "GET /{index}/_flush": "indices.flush",
	"POST /_forcemerge": "indices.forcemerge", "POST /{index}/_forcemerge": "indices.forcemerge",
	"POST /_cache/clear": "indices.clear_cache", "POST /{index}/_cache/clear": "indices.clear_cache",
	"POST /_reindex":                 "reindex",
	"POST /{index}/_delete_by_query": "delete_by_query", "POST /{index}/_update_by_query": "update_by_query",
	"GET /_analyze": "indices.analyze", "POST /_analyze": "indices.analyze", "GET /{index}/_analyze": "indices.analyze", "POST /{index}/_analyze": "indices.analyze",
	"GET /_mapping": "indices.get_mapping", "GET /{index}/_mapping": "indices.get_mapping", "GET /_mappings": "indices.get_mapping", "GET /{index}/_mappings": "indices.get_mapping",
	"PUT /{index}/_mapping": "indices.put_mapping", "POST /{index}/_mapping": "indices.put_mapping",
	"GET /{index}/_mapping/field/{fields}": "indices.get_field_mapping", "GET /_mapping/field/{fields}": "indices.get_field_mapping",
	"GET /_settings": "indices.get_settings", "GET /_settings/{name}": "indices.get_settings", "GET /{index}/_settings": "indices.get_settings", "GET /{index}/_settings/{name}": "indices.get_settings",
	"PUT /_settings": "indices.put_settings", "PUT /{index}/_settings": "indices.put_settings",
	"GET /_stats": "indices.stats", "GET /_stats/{metric}": "indices.stats", "GET /{index}/_stats": "indices.stats", "GET /{index}/_stats/{metric}": "indices.stats",
	"GET /_resolve/index/{name}": "indices.resolve_index",
	"GET /_validate/query":       "indices.validate_query", "POST /_validate/query": "indices.validate_query",
	"GET /{index}/_validate/query": "indices.validate_query", "POST /{index}/_validate/query": "indices.validate_query",
	"PUT /{index}": "indices.create", "GET /{index}": "indices.get", "HEAD /{index}": "indices.get", "DELETE /{index}": "indices.delete",
	"POST /{index}/_open": "indices.open", "POST /{index}/_close": "indices.close",

	"PUT /{index}/_doc/{id}": "index", "POST /{index}/_doc/{id}": "index", "POST /{index}/_doc": "index",
	"PUT /{index}/_create/{id}": "create", "POST /{index}/_create/{id}": "create",
	"GET /{index}/_doc/{id}": "get", "HEAD /{index}/_doc/{id}": "get",
	"DELETE /{index}/_doc/{id}":  "delete",
	"POST /{index}/_update/{id}": "update",
	"GET /{index}/_source/{id}":  "get_source", "HEAD /{index}/_source/{id}": "get_source",
	"GET /{index}/_explain/{id}": "explain", "POST /{index}/_explain/{id}": "explain",
}

// apiFor returns the parameter description of a matched route.
func apiFor(method string, r *route) *restAPI {
	name, ok := routeAPIs[method+" "+routePattern(r.pattern)]
	if !ok {
		return nil
	}
	return restAPIs[name]
}
