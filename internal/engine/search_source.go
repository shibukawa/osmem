package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A search request body is read the way SearchSourceBuilder.parseXContent
// reads it: the keys in document order, each value dispatched on its token
// type (a value, an object or an array; null is never accepted), the first
// failure reported. The JSON text of the body, when known, gives the key
// order and the locations Jackson includes in some messages.

// bodyReader reads the values of a JSON request body.
type bodyReader struct {
	raw []byte
}

// IsParseFailure reports whether err was raised while parsing a request
// body (the search source or a query in it) rather than while executing it.
func IsParseFailure(err error) bool {
	return isQueryParseFailure(err)
}

// jsonLocate scans raw to the value at path (object keys as strings, array
// indices as ints) and returns its first token with the scanner positioned
// after that token.
func jsonLocate(raw []byte, path ...any) (*xScanner, xToken, bool) {
	if len(raw) == 0 {
		return nil, xToken{}, false
	}
	s := newXScanner(raw)
	t, err := s.next()
	if err != nil {
		return nil, xToken{}, false
	}
	for _, p := range path {
		switch key := p.(type) {
		case string:
			if t.kind != xStartObject {
				return nil, xToken{}, false
			}
			for found := false; !found; {
				name, err := s.next()
				if err != nil || name.kind != xFieldName {
					return nil, xToken{}, false
				}
				v, err := s.next()
				if err != nil {
					return nil, xToken{}, false
				}
				if name.text == key {
					t, found = v, true
				} else if s.skipChildren(v) != nil {
					return nil, xToken{}, false
				}
			}
		case int:
			if t.kind != xStartArray {
				return nil, xToken{}, false
			}
			for i := 0; ; i++ {
				v, err := s.next()
				if err != nil || v.kind == xEndArray || v.kind == xEOF {
					return nil, xToken{}, false
				}
				if i == key {
					t = v
					break
				}
				if s.skipChildren(v) != nil {
					return nil, xToken{}, false
				}
			}
		default:
			return nil, xToken{}, false
		}
	}
	return s, t, true
}

// at is the location Jackson appends to its messages about the value at
// path: the byte offset just past the value's token.
func (r bodyReader) at(path ...any) string {
	offset := 0
	if _, t, ok := jsonLocate(r.raw, path...); ok {
		offset = t.end
	}
	return jacksonLocation(offset)
}

// lineCol is the XContentLocation ("line:col") of the value at path.
func (r bodyReader) lineCol(path ...any) string {
	if s, t, ok := jsonLocate(r.raw, path...); ok {
		line, col := s.lineCol(t.start)
		return fmt.Sprintf("%d:%d", line, col)
	}
	return "1:0"
}

// keys lists the keys of the object m found at path in document order
// (sorted when the body text is unknown).
func (r bodyReader) keys(m M, path ...any) []string {
	if s, t, ok := jsonLocate(r.raw, path...); ok && t.kind == xStartObject {
		out := make([]string, 0, len(m))
		for {
			name, err := s.next()
			if err != nil || name.kind != xFieldName {
				break
			}
			if _, in := m[name.text]; in {
				out = append(out, name.text)
			}
			v, err := s.next()
			if err != nil || s.skipChildren(v) != nil {
				break
			}
		}
		if len(out) == len(m) {
			return out
		}
	}
	return keysSorted(m)
}

// tokenAfterMember is the token following the value of key k in the
// object m at path: the next key or the end of the object.
func (r bodyReader) tokenAfterMember(m M, k string, path ...any) string {
	if keys := r.keys(m, path...); len(keys) > 0 && keys[len(keys)-1] != k {
		return "FIELD_NAME"
	}
	return "END_OBJECT"
}

// tokenAfterElement is the token following the first token of the i-th
// element of an array.
func tokenAfterElement(list []any, i int) string {
	switch e := list[i].(type) {
	case []any:
		if len(e) == 0 {
			return "END_ARRAY"
		}
		return jsonTokenName(e[0])
	case M:
		if len(e) == 0 {
			return "END_OBJECT"
		}
		return "FIELD_NAME"
	}
	if i+1 < len(list) {
		return jsonTokenName(list[i+1])
	}
	return "END_ARRAY"
}

// coercion is Jackson's InputCoercionException about the value at path.
func (r bodyReader) coercion(reason string, path ...any) *Error {
	reason += r.at(path...)
	return parseFailure(&Error{Status: http.StatusBadRequest, Type: "input_coercion_exception", Reason: reason,
		Cause: &Error{Type: "input_coercion_exception", Reason: reason}})
}

// bodyNumberFormatError is the NumberFormatException of Double.parseDouble.
func bodyNumberFormatError(text string) *Error {
	if strings.TrimSpace(text) == "" {
		return parseFailure(&Error{Status: http.StatusBadRequest, Type: "number_format_exception", Reason: "empty String"})
	}
	return pNumberFormat(text)
}

// intValue is XContentParser.intValue() on the value v at path: strings are
// parsed as doubles, numbers must fit an int.
func (r bodyReader) intValue(v any, path ...any) (int, *Error) {
	const intRange = " out of range of `int` (-2147483648 - 2147483647)"
	switch t := v.(type) {
	case string:
		f, ok := javaDoubleOK(t)
		if !ok {
			return 0, bodyNumberFormatError(t)
		}
		if f < math.MinInt32 || f > math.MaxInt32 {
			return 0, pIllegalArgument("Value [%s] is out of range for an integer", t)
		}
		if math.IsNaN(f) {
			return 0, nil
		}
		return int(f), nil
	case json.Number:
		text := t.String()
		if isIntegerLiteral(text) {
			n, err := strconv.ParseInt(text, 10, 64)
			if err != nil || n < math.MinInt32 || n > math.MaxInt32 {
				return 0, r.coercion("Numeric value ("+text+")"+intRange, path...)
			}
			return int(n), nil
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil || f < math.MinInt32 || f > math.MaxInt32 {
			return 0, r.coercion("Numeric value ("+text+")"+intRange, path...)
		}
		return int(f), nil
	case float64:
		if t < math.MinInt32 || t > math.MaxInt32 {
			return 0, r.coercion("Numeric value ("+strconv.FormatFloat(t, 'g', -1, 64)+")"+intRange, path...)
		}
		return int(t), nil
	case int, int64, int32:
		// the Go API passes bodies with native values
		n, _ := toFloat(t)
		if n < math.MinInt32 || n > math.MaxInt32 {
			return 0, r.coercion("Numeric value ("+strconv.FormatFloat(n, 'g', -1, 64)+")"+intRange, path...)
		}
		return int(n), nil
	}
	return 0, r.coercion("Current token ("+jacksonToken(v)+") not numeric, cannot use numeric value accessors", path...)
}

// floatValue is XContentParser.floatValue() on the value v at path.
func (r bodyReader) floatValue(v any, path ...any) (float32, *Error) {
	switch t := v.(type) {
	case string:
		f, ok := javaDoubleOK(t)
		if !ok {
			return 0, bodyNumberFormatError(t)
		}
		return float32(f), nil
	case json.Number:
		f, _ := strconv.ParseFloat(t.String(), 64)
		return float32(f), nil
	case float64:
		return float32(t), nil
	}
	return 0, r.coercion("Current token ("+jacksonToken(v)+") not numeric, cannot use numeric value accessors", path...)
}

// boolValue is XContentParser.booleanValue() on the value v at path.
func (r bodyReader) boolValue(v any, path ...any) (bool, *Error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch t {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return false, pIllegalArgument("Failed to parse value [%s] as only [true] or [false] are allowed.", t)
	}
	return false, r.coercion("Current token ("+jacksonToken(v)+") not of boolean type", path...)
}

func unknownBodyKey(body M, k string) *Error {
	return pParsing("Unknown key for a %s in [%s].", jsonTokenName(body[k]), k).at(valueTok(body, k))
}

// tokAfterElement names the token after the first token of element i of
// list (where ObjectParser looks for the START_OBJECT it expected).
func tokAfterElement(list []any, i int) *tokenRef {
	switch e := list[i].(type) {
	case []any:
		if len(e) > 0 {
			return elemTok(e, 0)
		}
		return endTok(e)
	case M:
		if len(e) > 0 {
			return nthKeyTok(e, 1)
		}
		return endTok(e)
	}
	if i+1 < len(list) {
		return elemTok(list, i+1)
	}
	return endTok(list)
}

// tokAfterMember names the token after the value of member key of m: the
// next member's name or the END_OBJECT of m.
func tokAfterMember(m M, key string) *tokenRef {
	var order []string
	return &tokenRef{in: m, part: tokValue,
		pick: func(o []string) (string, bool) {
			order = o
			return key, true
		},
		via: func(string) *tokenRef {
			for i, name := range order {
				if name == key && i+1 < len(order) {
					return keyTok(m, order[i+1])
				}
			}
			return endTok(m)
		}}
}

// parseSearchSource parses a search request body (raw is its JSON text, or
// nil) followed by the search URL parameters.
func parseSearchSource(body M, raw []byte, p Params) (*searchRequest, error) {
	sr := &searchRequest{size: 10, trackTotal: maxResultWindow}
	if body == nil {
		body = M{}
	}
	if q := p.Get("q"); q != "" {
		qs := M{"query": q}
		if df := p.Get("df"); df != "" {
			qs["default_field"] = df
		}
		if op := p.Get("default_operator"); op != "" {
			qs["default_operator"] = op
		}
		if an := p.Get("analyzer"); an != "" {
			qs["analyzer"] = an
		}
		sr.query = M{"query_string": qs}
	}
	r := bodyReader{raw: raw}
	for _, k := range r.keys(body) {
		if err := sr.parseBodyKey(r, body, k); err != nil {
			return nil, err
		}
	}
	return sr.applySearchParams(p)
}

func (sr *searchRequest) parseBodyKey(r bodyReader, body M, k string) error {
	switch t := body[k].(type) {
	case nil:
		return unknownBodyKey(body, k)
	case M:
		return sr.parseBodyObject(r, body, k, t)
	case []any:
		return sr.parseBodyArray(r, body, k, t)
	}
	return sr.parseBodyValue(r, body, k, body[k])
}

// parseBodyValue reads a key holding a string, number or boolean.
func (sr *searchRequest) parseBodyValue(r bodyReader, body M, k string, v any) error {
	switch k {
	case "from", "size":
		n, err := r.intValue(v, k)
		if err != nil {
			return err
		}
		if n < 0 {
			return pIllegalArgument("[%s] parameter cannot be negative, found [%d]", k, n)
		}
		if k == "from" {
			sr.from = n
		} else {
			sr.size = n
		}
	case "timeout":
		d, err := parseTimeValue(xText(v), "timeout")
		if err != nil {
			if e, ok := err.(*Error); ok {
				return parseFailure(e)
			}
			return err
		}
		sr.timeout = d
	case "terminate_after":
		n, err := r.intValue(v, k)
		if err != nil {
			return err
		}
		sr.terminateAfter = n
		sr.terminateAfterSet = n != 0
	case "min_score":
		f, err := r.floatValue(v, k)
		if err != nil {
			return err
		}
		score := float64(f)
		sr.minScore = &score
	case "version", "seq_no_primary_term", "explain", "track_scores", "profile", "verbose_pipeline", "include_named_queries_score":
		b, err := r.boolValue(v, k)
		if err != nil {
			return err
		}
		switch k {
		case "version":
			sr.version = b
		case "seq_no_primary_term":
			sr.seqNoTerm = b
		case "explain":
			sr.explain = b
		case "track_scores":
			sr.trackScores = b
		case "profile":
			sr.profile = b
		case "verbose_pipeline":
			sr.verbosePipeline = b
		}
		// include_named_queries_score is read from the body but only the
		// URL parameter changes the response
	case "track_total_hits":
		if b, isBool := v.(bool); isBool || v == "true" || v == "false" {
			upTo := -1
			if (isBool && b) || v == "true" {
				sr.trackTotal = -1
				upTo = math.MaxInt32
			} else {
				sr.trackTotal = 0
			}
			sr.trackTotalUpTo = &upTo
			return nil
		}
		n, err := r.intValue(v, k)
		if err != nil {
			return err
		}
		upTo := n
		sr.trackTotalUpTo = &upTo
		return sr.setTrackTotalHits(n)
	case "_source":
		return sr.setFetchSource(r, body, k)
	case "stored_fields":
		return sr.parseStoredFieldsBody(r, body, k)
	case "sort":
		// sort(parser.text())
		text := xText(v)
		sr.setSort([]sortSpec{{field: text, desc: text == "_score", missing: "_last"}})
	case "search_pipeline":
		sr.searchPipeline = xText(v)
	default:
		return unknownBodyKey(body, k)
	}
	return nil
}

// parseBodyObject reads a key holding an object.
func (sr *searchRequest) parseBodyObject(r bodyReader, body M, k string, m M) error {
	switch k {
	case "query", "post_filter":
		if _, err := parseQuery(m); err != nil {
			return err
		}
		if k == "query" {
			sr.query = m
		} else {
			sr.postFilter = m
		}
	case "_source":
		return sr.setFetchSource(r, body, k)
	case "script_fields":
		specs, err := parseScriptFields(r, m)
		if err != nil {
			return err
		}
		sr.scriptFields = specs
	case "indices_boost":
		boosts, err := parseIndicesBoost(body, k)
		if err != nil {
			return err
		}
		sr.indexBoosts = boosts
	case "aggregations", "aggs":
		// [aggs hook] parsed and validated by aggs_parse.go; a later
		// definition replaces an earlier one
		am, err := parseSearchAggregations(body, k)
		if err != nil {
			return err
		}
		sr.aggs = am
	case "highlight":
		if _, err := parseHighlight(body, k); err != nil {
			return err
		}
		sr.highlight = m
	case "suggest":
		spec, err := parseSuggestSpec(r, m)
		if err != nil {
			return err
		}
		sr.suggestSet = true
		sr.suggestions = spec != nil
		sr.suggest = spec
	case "sort":
		specs, err := parseSort(m)
		if err != nil {
			return err
		}
		sr.setSort(specs)
	case "rescore":
		specs, err := parseRescore(body, k)
		if err != nil {
			return err
		}
		sr.rescore = specs
	case "ext":
		// search extensions are named objects of plugins; osmem has none
		for _, name := range r.keys(m, k) {
			return parseFailure((&Error{Status: http.StatusBadRequest, Type: "named_object_not_found_exception", Reason: "unknown field [" + name + "]"}).at(valueTok(m, name)))
		}
	case "slice":
		spec, err := parseSlice(body, k)
		if err != nil {
			return err
		}
		sr.slice = spec
	case "collapse":
		return sr.parseCollapse(r, m)
	case "pit":
		return sr.parsePointInTime(r, m)
	case "search_pipeline":
		sr.parseInlinePipeline(r, m)
	case "derived":
		if len(m) > 0 {
			// derived fields are computed by scripts
			return errUnsupported("[derived] fields")
		}
	default:
		return unknownBodyKey(body, k)
	}
	return nil
}

// parseBodyArray reads a key holding an array.
func (sr *searchRequest) parseBodyArray(r bodyReader, body M, k string, list []any) error {
	switch k {
	case "stored_fields":
		return sr.parseStoredFieldsBody(r, body, k)
	case "docvalue_fields", "fields":
		specs, err := parseFieldAndFormats(r, k, list)
		if err != nil {
			return err
		}
		if k == "fields" {
			sr.fields = specs
		} else {
			sr.docvalueFields = specs
		}
	case "indices_boost":
		boosts, err := parseIndicesBoost(body, k)
		if err != nil {
			return err
		}
		sr.indexBoosts = boosts
	case "sort":
		specs, err := parseSort(list)
		if err != nil {
			return err
		}
		sr.setSort(specs)
	case "rescore":
		specs, err := parseRescore(body, k)
		if err != nil {
			return err
		}
		sr.rescore = specs
	case "stats":
		sr.stats = make([]string, 0, len(list))
		for i, e := range list {
			s, ok := e.(string)
			if !ok {
				return pParsing("Expected [VALUE_STRING] in [stats] but found [%s]", jsonTokenName(e)).at(elemTok(list, i))
			}
			sr.stats = append(sr.stats, s)
		}
	case "_source":
		return sr.setFetchSource(r, body, k)
	case "search_after":
		for i, e := range list {
			switch e.(type) {
			case M, []any:
				return pParsing("Expected [VALUE_STRING] or [VALUE_NUMBER] or [VALUE_BOOLEAN] or [VALUE_NULL] but found [%s] inside search_after.", jsonTokenName(e)).
					at(elemTok(list, i))
			}
		}
		if len(list) == 0 {
			return pIllegalArgument("Values must contains at least one value.")
		}
		sr.searchAfter = list
	default:
		return unknownBodyKey(body, k)
	}
	return nil
}

// setSort records the sort of a request. Sorting on the score alone is no
// sort at all (SortBuilder.buildSort): hits then carry no sort values.
func (sr *searchRequest) setSort(specs []sortSpec) {
	sr.sort = specs
	sr.explicitSort = len(specs) > 0 && !(len(specs) == 1 && specs[0].field == "_score" && specs[0].desc)
}

// setFetchSource reads the _source key (FetchSourceContext.fromXContent).
func (sr *searchRequest) setFetchSource(r bodyReader, body M, key string) error {
	sf, err := parseFetchSource(r, body, key)
	if err != nil {
		return err
	}
	if err := sf.validate(); err != nil {
		return err
	}
	sr.source = sf
	sr.sourceExplicit = true
	return nil
}

func parseFetchSource(r bodyReader, body M, key string) (sourceFilter, error) {
	v := body[key]
	strs := func(list []any) ([]string, error) {
		out := make([]string, 0, len(list))
		for i, e := range list {
			s, ok := e.(string)
			if !ok {
				return nil, pParsing("Unknown key for a %s in [null].", jsonTokenName(e)).at(elemTok(list, i))
			}
			out = append(out, s)
		}
		return out, nil
	}
	switch t := v.(type) {
	case bool:
		return sourceFilter{disabled: !t}, nil
	case string:
		return sourceFilter{includes: []string{t}}, nil
	case []any:
		inc, err := strs(t)
		return sourceFilter{includes: inc}, err
	case M:
		sf := sourceFilter{}
		for _, k := range r.keys(t, key) {
			include := k == "includes" || k == "include"
			exclude := k == "excludes" || k == "exclude"
			switch fv := t[k].(type) {
			case []any:
				if !include && !exclude {
					return sourceFilter{}, pParsing("Unknown key for a START_ARRAY in [%s].", k).at(valueTok(t, k))
				}
				list, err := strs(fv)
				if err != nil {
					return sourceFilter{}, err
				}
				if include {
					sf.includes = list
				} else {
					sf.excludes = list
				}
			case string:
				if !include && !exclude {
					return sourceFilter{}, pParsing("Unknown key for a VALUE_STRING in [%s].", k).at(valueTok(t, k))
				}
				if include {
					sf.includes = []string{fv}
				} else {
					sf.excludes = []string{fv}
				}
			default:
				return sourceFilter{}, pParsing("Unknown key for a %s in [%s].", jsonTokenName(fv), k).at(valueTok(t, k))
			}
		}
		return sf, nil
	}
	return sourceFilter{}, pParsing("Expected one of [VALUE_BOOLEAN, VALUE_STRING, START_ARRAY, START_OBJECT] but found [%s]", jsonTokenName(v)).at(valueTok(body, key))
}

// parseStoredFieldsBody is StoredFieldsContext.fromXContent.
func (sr *searchRequest) parseStoredFieldsBody(r bodyReader, body M, key string) error {
	var names []string
	switch t := body[key].(type) {
	case string:
		names = []string{t}
	case []any:
		for i, e := range t {
			switch e.(type) {
			case M, []any, nil:
				return parseFailure((&Error{Status: http.StatusInternalServerError, Type: "illegal_state_exception",
					Reason: "Can't get text on a " + jacksonTokenForText(e) + " at " + r.lineCol(key, i)}).atInReason(elemTok(t, i), "Can't get text on a "+jacksonTokenForText(e)+" at %s"))
			}
			names = append(names, xText(e))
		}
	default:
		return pParsing("Expected [VALUE_STRING] or [START_ARRAY] in [stored_fields] but found [%s]", jsonTokenName(body[key])).at(valueTok(body, key))
	}
	sr.storedFieldsSet = true
	sr.storedNone = false
	sr.storedFields = nil
	if len(names) == 1 && names[0] == "_none_" {
		sr.storedNone = true
		return nil
	}
	for _, name := range names {
		if name == "_none_" {
			return pIllegalArgument("cannot combine _none_ with other fields")
		}
	}
	sr.storedFields = make([]any, 0, len(names))
	for _, name := range names {
		sr.storedFields = append(sr.storedFields, name)
	}
	return nil
}

// parseFieldAndFormats reads docvalue_fields and fields: field names or
// {"field", "format"} objects (FieldAndFormat.fromXContent).
func parseFieldAndFormats(r bodyReader, key string, list []any) ([]any, error) {
	out := make([]any, 0, len(list))
	for i, e := range list {
		switch t := e.(type) {
		case M:
			spec := M{}
			hasField := false
			for _, k := range r.keys(t, key, i) {
				fv := t[k]
				switch k {
				case "field":
					s, ok := fv.(string)
					if !ok {
						return nil, pXContent("[docvalues_field] field doesn't support values of type: %s", jsonTokenName(fv)).at(valueTok(t, k))
					}
					spec["field"] = s
					hasField = true
				case "format":
					switch s := fv.(type) {
					case string:
						// use_field_mapping only existed to ease the 7.x
						// transition and is now the (unset-format) default.
						if s != "use_field_mapping" {
							spec["format"] = fv
						}
					case nil:
					default:
						return nil, pXContent("[docvalues_field] format doesn't support values of type: %s", jsonTokenName(fv)).at(valueTok(t, k))
					}
				default:
					return nil, pXContent("[docvalues_field] unknown field [%s]", k).at(keyTok(t, k)).atParser(valueTok(t, k))
				}
			}
			if !hasField {
				return nil, pIllegalArgument("Required [field]")
			}
			out = append(out, spec)
		case []any, nil:
			return nil, pXContent("[docvalues_field] Expected START_OBJECT but was: %s", tokenAfterElement(list, i)).at(tokAfterElement(list, i))
		default:
			out = append(out, xText(e))
		}
	}
	return out, nil
}

// collapse, point in time, pipelines, suggest ------------------------------------

// parseCollapse is CollapseBuilder.fromXContent.
func (sr *searchRequest) parseCollapse(r bodyReader, cm M) error {
	for _, key := range r.keys(cm, "collapse") {
		switch key {
		case "field", "inner_hits", "max_concurrent_group_searches":
		default:
			return pXContent("[collapse] unknown field [%s]", key).at(keyTok(cm, key)).atParser(valueTok(cm, key))
		}
	}
	if raw, ok := cm["max_concurrent_group_searches"]; ok {
		n, nerr := xcontentInt(raw)
		if nerr == nil && n <= 0 {
			nerr = &Error{Type: "illegal_argument_exception", Reason: "maxConcurrentGroupRequests` must be positive"}
		}
		if nerr != nil {
			return parseFailure((&Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: "[collapse] failed to parse field [max_concurrent_group_searches]", Cause: nerr}).
				atCause(valueEndTok(cm, "max_concurrent_group_searches")))
		}
	}
	sr.collapseSet = true
	sr.collapse = getString(cm, "field")
	var specs []M
	switch ih := cm["inner_hits"].(type) {
	case M:
		specs = []M{ih}
	case []any:
		for _, e := range ih {
			if em, ok := e.(M); ok {
				specs = append(specs, em)
			}
		}
	}
	for _, ihm := range specs {
		spec, err := parseInnerHits(ihm, "")
		if err != nil {
			if e, ok := err.(*Error); ok {
				return parseFailure((&Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: "[collapse] failed to parse field [inner_hits]", Cause: e}).
					atCause(valueEndTok(cm, "inner_hits")))
			}
			return err
		}
		if spec.name == "" {
			return pIllegalArgument("Field name cannot be null")
		}
		replaced := false
		for i, prev := range sr.collapseInner {
			if prev.name == spec.name {
				sr.collapseInner[i] = spec
				replaced = true
			}
		}
		if !replaced {
			sr.collapseInner = append(sr.collapseInner, spec)
		}
	}
	return nil
}

// parsePointInTime is PointInTimeBuilder.fromXContent.
func (sr *searchRequest) parsePointInTime(r bodyReader, pm M) error {
	for _, key := range r.keys(pm, "pit") {
		switch key {
		case "id":
			if _, isString := pm[key].(string); !isString {
				return pXContent("[pit] id doesn't support values of type: %s", jsonTokenName(pm[key])).at(valueTok(pm, key))
			}
		case "keep_alive":
			keepAlive, isString := pm[key].(string)
			if !isString {
				return pXContent("[pit] keep_alive doesn't support values of type: %s", jsonTokenName(pm[key])).at(valueTok(pm, key))
			}
			d, err := parseTimeValue(keepAlive, "keep_alive")
			if err != nil {
				cause, _ := err.(*Error)
				return parseFailure((&Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: "[pit] failed to parse field [keep_alive]", Cause: cause}).
					atCause(valueEndTok(pm, key)))
			}
			sr.pitKeepAlive = d
			sr.pitKeepAliveSet = true
		default:
			return pXContent("[pit] unknown field [%s]", key).at(keyTok(pm, key)).atParser(valueTok(pm, key))
		}
	}
	id, ok := pm["id"].(string)
	if !ok {
		return pIllegalArgument("point int time id is not provided")
	}
	sr.pitSet = true
	sr.pitID = id
	return nil
}

// parseInlinePipeline reads an ad hoc search pipeline. Its configuration is
// checked when the pipeline is resolved, after the whole body was parsed.
func (sr *searchRequest) parseInlinePipeline(r bodyReader, m M) {
	sr.inlinePipeline = true
	var unknown []string
	for _, k := range r.keys(m, "search_pipeline") {
		switch k {
		case "description", "version":
		case "request_processors", "response_processors", "phase_results_processors":
			if list, ok := m[k].([]any); ok && len(list) > 0 {
				sr.pipelineProcessors = true
			}
		default:
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sr.pipelineError = &Error{Status: http.StatusBadRequest, Type: "parse_exception",
			Reason: "pipeline [_ad_hoc_pipeline] doesn't support one or more provided configuration parameters [" + strings.Join(unknown, ", ") + "]"}
	}
}

// searchPipelineError resolves the search pipeline of a request
// (SearchPipelineService.resolvePipeline): osmem defines no pipelines.
func searchPipelineError(sr *searchRequest, p Params) error {
	if sr.pipelineError != nil {
		return sr.pipelineError
	}
	name := sr.searchPipeline
	if v := p.Get("search_pipeline"); v != "" {
		name = v
	}
	named := name != "" && name != "_none"
	if named && !sr.inlinePipeline {
		return errIllegalArgument("Pipeline %s is not defined", name)
	}
	verbose := sr.verbosePipeline
	if v, ok := p["verbose_pipeline"]; ok {
		verbose, _ = ParseBoolValue(v, false)
	}
	if verbose && !named && !sr.inlinePipeline {
		return errIllegalArgument("The 'verbose pipeline' option requires a search pipeline to be defined.")
	}
	if sr.pipelineProcessors {
		return errUnsupported("search pipeline processors")
	}
	return nil
}

// parseSuggestSpec (suggest.go) is SuggestBuilder.fromXContent, extended to
// build the structures runSuggest executes.

// script fields -----------------------------------------------------------------

// scriptSpec is a parsed Script.
type scriptSpec struct {
	stored      bool
	idOrCode    string
	lang        string
	contentType bool // the source was given as an object
}

type scriptFieldSpec struct {
	name          string
	script        *scriptSpec // nil when the field has no script
	ignoreFailure bool
}

// parseScriptFields reads script_fields (SearchSourceBuilder.ScriptField).
func parseScriptFields(r bodyReader, m M) ([]scriptFieldSpec, error) {
	var out []scriptFieldSpec
	for _, name := range r.keys(m, "script_fields") {
		fm, ok := m[name].(M)
		if !ok {
			return nil, pParsing("Expected [START_OBJECT] in [%s] but found [%s]", name, jsonTokenName(m[name])).at(valueTok(m, name))
		}
		spec := scriptFieldSpec{name: name}
		for _, k := range r.keys(fm, "script_fields", name) {
			fv := fm[k]
			switch fv.(type) {
			case M:
				if k != "script" {
					return nil, pParsing("Unknown key for a START_OBJECT in [%s].", k).at(valueTok(fm, k))
				}
			case []any, nil:
				return nil, pParsing("Unknown key for a %s in [%s].", jsonTokenName(fv), k).at(valueTok(fm, k))
			default:
				if k != "script" && k != "ignore_failure" {
					return nil, pParsing("Unknown key for a %s in [%s].", jsonTokenName(fv), k).at(valueTok(fm, k))
				}
			}
			if k == "ignore_failure" {
				b, err := r.boolValue(fv, "script_fields", name, k)
				if err != nil {
					return nil, err
				}
				spec.ignoreFailure = b
				continue
			}
			sc, err := parseScript(r, fv, fm, k, "script_fields", name, k)
			if err != nil {
				return nil, err
			}
			spec.script = sc
		}
		out = append(out, spec)
	}
	return out, nil
}

// parseScript is Script.parse with the painless default language; v is the
// value of key in the object parent at path.
func parseScript(r bodyReader, v any, parent M, key string, path ...any) (*scriptSpec, error) {
	switch t := v.(type) {
	case string:
		return &scriptSpec{idOrCode: t, lang: "painless"}, nil
	case M:
		sc := &scriptSpec{}
		typed := false
		options := map[string]string{}
		conflict := func(k string) error {
			return parseFailure((&Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: "[script] failed to parse field [" + k + "]",
				Cause: errIllegalArgument("must only use one of [source, id] when specifying a script")}).atCause(valueEndTok(t, k)))
		}
		for _, k := range r.keys(t, path...) {
			fv := t[k]
			switch k {
			case "source", "inline":
				switch fv.(type) {
				case string, M:
				default:
					return nil, pXContent("[script] %s doesn't support values of type: %s", k, jsonTokenName(fv)).at(valueTok(t, k))
				}
				if typed {
					return nil, conflict(k)
				}
				typed = true
				if s, ok := fv.(string); ok {
					sc.idOrCode = s
				} else {
					data, _ := json.Marshal(fv)
					sc.idOrCode = string(data)
					sc.contentType = true
				}
			case "id":
				s, ok := fv.(string)
				if !ok {
					return nil, pXContent("[script] id doesn't support values of type: %s", jsonTokenName(fv)).at(valueTok(t, k))
				}
				if typed {
					return nil, conflict(k)
				}
				typed = true
				sc.stored = true
				sc.idOrCode = s
			case "lang":
				s, ok := fv.(string)
				if !ok {
					return nil, pXContent("[script] lang doesn't support values of type: %s", jsonTokenName(fv)).at(valueTok(t, k))
				}
				sc.lang = s
			case "options":
				om, ok := fv.(M)
				if !ok {
					return nil, pXContent("[script] options doesn't support values of type: %s", jsonTokenName(fv)).at(valueTok(t, k))
				}
				for ok, ov := range om {
					options[ok] = xText(ov)
				}
			case "params":
				if _, ok := fv.(M); !ok {
					return nil, pXContent("[script] params doesn't support values of type: %s", jsonTokenName(fv)).at(valueTok(t, k))
				}
			default:
				return nil, pXContent("[script] unknown field [%s]", k).at(keyTok(t, k)).atParser(valueTok(t, k))
			}
		}
		if !typed {
			return nil, pIllegalArgument("must specify either [source] for an inline script or [id] for a stored script")
		}
		if sc.stored {
			if sc.lang != "" {
				return nil, pIllegalArgument("illegally specified <lang> for a stored script")
			}
			if len(options) > 0 {
				return nil, pIllegalArgument("field [options] cannot be specified using a stored script")
			}
			return sc, nil
		}
		if sc.lang == "" {
			sc.lang = "painless"
		}
		delete(options, "content_type")
		if len(options) > 0 {
			keys := make([]string, 0, len(options))
			for k := range options {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, k+"="+options[k])
			}
			return nil, pIllegalArgument("illegal compiler options [{%s}] specified", strings.Join(parts, ", "))
		}
		return sc, nil
	}
	// a number or boolean: the script parser wants an object
	return nil, pXContent("[script] Expected START_OBJECT but was: %s", r.tokenAfterMember(parent, key, path[:len(path)-1]...)).at(tokAfterMember(parent, key))
}

// scriptFieldsError is the shard's compilation of script_fields
// (SearchService.parseSource); scripts are not compiled for size 0.
func scriptFieldsError(ix *Index, sr *searchRequest) *Error {
	if len(sr.scriptFields) == 0 {
		return nil
	}
	for _, f := range sr.scriptFields {
		if f.script == nil && sr.size == 0 {
			return &Error{Status: http.StatusInternalServerError, Type: "null_pointer_exception",
				Reason: "Cannot invoke \"org.opensearch.script.Script.writeTo(org.opensearch.core.common.io.stream.StreamOutput)\" because \"this.script\" is null"}
		}
	}
	if sr.size == 0 {
		return nil
	}
	settings := getMap(ix.Settings, "index")
	if max := getInt(settings, "max_script_fields", 32); len(sr.scriptFields) > max {
		return errIllegalArgument("Trying to retrieve too many script_fields. Must be less than or equal to: [%d] but was [%d]. This limit can be set by changing the [index.max_script_fields] index level setting.", max, len(sr.scriptFields))
	}
	for _, f := range sr.scriptFields {
		sc := f.script
		switch {
		case sc == nil:
			return &Error{Status: http.StatusInternalServerError, Type: "null_pointer_exception", nullReason: true}
		case sc.stored:
			return &Error{Status: http.StatusNotFound, Type: "resource_not_found_exception", Reason: "unable to find script [" + sc.idOrCode + "] in cluster state"}
		case sc.lang != "painless" && sc.lang != "expression" && sc.lang != "mustache":
			return errIllegalArgument("script_lang not supported [%s]", sc.lang)
		case sc.lang == "mustache":
			return errIllegalArgument("mustache engine does not know how to handle context [field]")
		case sc.contentType:
			return errIllegalArgument("Unrecognized compile-time parameter(s): {content_type=application/json; charset=UTF-8}")
		}
	}
	return nil
}

// scoresNeeded reports whether a search computes the scores of its query
// (the score mode of the collectors): hits sorted by score or with tracked
// scores, min_score, rescoring, explanations, and aggregations reading
// scores. A request without hits (size 0) only counts them.
func (sr *searchRequest) scoresNeeded() bool {
	if sr.minScore != nil || len(sr.rescore) > 0 || aggsNeedScores(sr.aggs) {
		return true
	}
	if sr.size == 0 {
		return false
	}
	return sr.trackScores || sr.explain || sortsByScore(sr.sort)
}

// aggsNeedScores reports aggregations that read scores: top_hits sorted by
// score and samplers.
func aggsNeedScores(aggs M) bool {
	for _, v := range aggs {
		def, ok := v.(M)
		if !ok {
			continue
		}
		for k, body := range def {
			switch k {
			case "sampler", "diversified_sampler":
				return true
			case "top_hits":
				bm, _ := body.(M)
				sortSpec, has := bm["sort"]
				if !has {
					return true
				}
				specs, err := parseSort(sortSpec)
				if err != nil || sortsByScore(specs) {
					return true
				}
			case "aggs", "aggregations":
				if sub, ok := body.(M); ok && aggsNeedScores(sub) {
					return true
				}
			}
		}
	}
	return false
}

// indexMatchesName is QueryShardContext.indexMatches for the _index field:
// the index name, one of its aliases, or a wildcard pattern over them.
func indexMatchesName(ix *Index, pattern string) bool {
	if pattern == ix.Name || matchAny([]string{pattern}, ix.Name) {
		return true
	}
	for alias := range ix.Aliases {
		if pattern == alias || matchAny([]string{pattern}, alias) {
			return true
		}
	}
	return false
}

// queryMatchesAll reports whether a query rewrites to match_all on an index
// (Lucene's rewrite: match_all, an empty bool, exists on a field every
// document has, filters made only of those).
func queryMatchesAll(q any, ix *Index) bool {
	if q == nil {
		return true
	}
	n, err := parseQuery(q)
	if err != nil {
		return false
	}
	return rewritesToMatchAll(n, ix)
}

func rewritesToMatchAll(n *qnode, ix *Index) bool {
	if n.boost != 1 {
		return false
	}
	switch spec := n.spec.(type) {
	case *existsSpec:
		f, _, ok := ix.Mapping.resolve(spec.field)
		if !ok || f.Type == TypeObject || f.Type == TypeNested || ix.Mapping.nestedAncestor(spec.field) != "" {
			return false
		}
		roots := 0
		for _, d := range ix.docs {
			if d.nested != nil {
				continue
			}
			roots++
			if len(ix.fieldValues(d, spec.field)) == 0 {
				return false
			}
		}
		return roots > 0
	case *constantScoreSpec:
		return rewritesToMatchAll(spec.filter, ix)
	case *boolSpec:
		if len(spec.should)+len(spec.mustNot) > 0 {
			return false
		}
		for _, c := range append(append([]*qnode(nil), spec.must...), spec.filter...) {
			if !rewritesToMatchAll(c, ix) {
				return false
			}
		}
		return true
	}
	return n.kind == "match_all"
}

// countableQueryOn reports queries whose hit count a shard reads without
// collecting (Weight#count): queries rewritten to match_all, and single
// term, range, exists and match queries on indices without deletions.
func countableQueryOn(q any, ix *Index, deletions bool) bool {
	if q == nil {
		return true
	}
	n, err := parseQuery(q)
	if err != nil {
		return false
	}
	return countableNode(n, ix, deletions)
}

func countableNode(n *qnode, ix *Index, deletions bool) bool {
	if rewritesToMatchAll(n, ix) {
		return true
	}
	switch spec := n.spec.(type) {
	case *termSpec, *rangeSpec, *existsSpec:
		return !deletions
	case *matchSpec:
		return !deletions && n.kind == "match"
	case *constantScoreSpec:
		return countableNode(spec.filter, ix, deletions)
	case *boolSpec:
		if len(spec.should)+len(spec.mustNot) > 0 {
			return false
		}
		var rest []*qnode
		for _, c := range append(append([]*qnode(nil), spec.must...), spec.filter...) {
			if !rewritesToMatchAll(c, ix) {
				rest = append(rest, c)
			}
		}
		return len(rest) == 1 && countableNode(rest[0], ix, deletions)
	}
	return false
}

// preFilterSkipped counts the shards the can_match phase skips
// (CanMatchPreFilterSearchPhase): with a primary sort on a field, or more
// than 128 shards, a shard whose query rewrites to match_none is not
// searched; one shard is always searched.
func preFilterSkipped(sr *searchRequest, ts []target, live map[*Index]bool) int {
	shards := 0
	for _, t := range ts {
		if live[t.ix] {
			shards += len(sr.includedShards(t.ix))
		}
	}
	primaryFieldSort := sr.explicitSort && len(sr.sort) > 0 && sr.sort[0].field != "_score"
	if shards < 2 || !(primaryFieldSort || shards > 128) || sr.query == nil || sr.suggestSet {
		return 0
	}
	n, err := parseQuery(sr.query)
	if err != nil || n.kind == "match_all" {
		return 0
	}
	for _, v := range sr.aggs {
		// aggregations that must visit every document (global, terms with
		// min_doc_count 0)
		def, _ := v.(M)
		if _, global := def["global"]; global {
			return 0
		}
		if terms, ok := def["terms"].(M); ok {
			if n, ok := toFloat(terms["min_doc_count"]); ok && n == 0 {
				return 0
			}
		}
	}
	var filters map[*Index]*qnode
	skipped := 0
	for _, t := range ts {
		if !live[t.ix] {
			continue
		}
		none := rewritesToMatchNone(n, t.ix)
		if !none && t.filter != nil {
			if filters == nil {
				filters = map[*Index]*qnode{}
			}
			if fn, ferr := parseQuery(t.filter); ferr == nil {
				none = rewritesToMatchNone(fn, t.ix)
			}
		}
		if none {
			skipped += len(sr.includedShards(t.ix))
		}
	}
	if skipped == shards {
		skipped--
	}
	return skipped
}

// aggsPrecomputable reports whether every top-level aggregation of a
// request can be computed from the index alone for a query matching all
// documents: terms on a keyword field and min/max on an indexed numeric or
// date field, without sub-aggregations, scripts or missing values.
func aggsPrecomputable(aggs M, ix *Index) bool {
	if len(aggs) == 0 {
		return false
	}
	for _, v := range aggs {
		def, ok := v.(M)
		if !ok {
			return false
		}
		kind := ""
		var body M
		for k, b := range def {
			switch k {
			case "meta":
				continue
			case "terms", "min", "max":
				if kind != "" {
					return false
				}
				kind = k
				body, _ = b.(M)
			default:
				return false
			}
		}
		if kind == "" || body == nil {
			return false
		}
		for k := range body {
			switch k {
			case "field", "size", "order", "shard_size", "min_doc_count", "shard_min_doc_count", "show_term_doc_count_error", "format", "execution_hint", "collect_mode", "value_type":
			default:
				return false
			}
		}
		f, _, mapped := ix.Mapping.resolve(getString(body, "field"))
		if !mapped {
			return false
		}
		switch kind {
		case "terms":
			if f.Type != TypeKeyword {
				return false
			}
		default:
			if !(f.isNumeric() || f.isDate()) || !f.Index {
				return false
			}
		}
	}
	return true
}

// geo distance sort -----------------------------------------------------------

type geoSortSpec struct {
	points         []geoPointSpec
	unitMeters     float64
	ignoreUnmapped bool
	validation     string
}

// parseGeoDistanceSort is GeoDistanceSortBuilder.fromXContent.
func parseGeoDistanceSort(v any) (sortSpec, error) {
	m, ok := v.(M)
	if !ok {
		return sortSpec{}, pParsing("[_geo_distance] malformed sort")
	}
	ss := sortSpec{missing: "_last", geo: &geoSortSpec{unitMeters: 1, validation: "STRICT"}}
	for _, k := range keysSorted(m) {
		val := m[k]
		switch k {
		case "order":
			desc, err := parseSortOrder(xText(val))
			if err != nil {
				return sortSpec{}, err
			}
			ss.desc = desc
		case "unit":
			meters, ok := distanceUnitMeters(xText(val))
			if !ok {
				return sortSpec{}, pIllegalArgument("No distance unit match [%s]", xText(val))
			}
			ss.geo.unitMeters = meters
		case "distance_type":
			switch strings.ToLower(xText(val)) {
			case "arc", "plane":
			default:
				return sortSpec{}, pIllegalArgument("No geo distance for [%s]", xText(val))
			}
		case "mode":
			switch mode := strings.ToLower(xText(val)); mode {
			case "min", "max", "avg", "median":
				ss.mode = mode
			case "sum":
				return sortSpec{}, pIllegalArgument("sort_mode [sum] isn't supported for sorting by geo distance")
			default:
				return sortSpec{}, pIllegalArgument("No enum constant org.opensearch.search.MultiValueMode.%s", strings.ToUpper(mode))
			}
		case "ignore_unmapped":
			b, err := xBool(val)
			if err != nil {
				return sortSpec{}, err
			}
			ss.geo.ignoreUnmapped = b
		case "validation_method":
			method, err := parseValidationMethod(val)
			if err != nil {
				return sortSpec{}, err
			}
			ss.geo.validation = method
		case "nested", "nested_path", "nested_filter":
			return sortSpec{}, errUnsupported("nested geo distance sort")
		default:
			var raw []any
			switch t := val.(type) {
			case []any:
				raw = t
				if len(t) > 0 {
					if _, isNumber := toFloat(t[0]); isNumber && !isStringValue(t[0]) {
						// [lon, lat]
						raw = []any{t}
					}
				}
			case M, string:
				raw = []any{t}
			default:
				return sortSpec{}, pParsing("[_geo_distance] does not support [%s]", k).at(valueTok(m, k))
			}
			if ss.field != "" {
				return sortSpec{}, pParsing("[_geo_distance] sort doesn't support multiple field names")
			}
			ss.field = k
			for _, p := range raw {
				point, err := parseGeoPointAny(p, k)
				if err != nil {
					return sortSpec{}, err
				}
				ss.geo.points = append(ss.geo.points, point)
			}
		}
	}
	if ss.mode == "" {
		ss.mode = "min"
		if ss.desc {
			ss.mode = "max"
		}
	}
	return ss, nil
}

func isStringValue(v any) bool {
	_, ok := v.(string)
	return ok
}

// geoSortValue is the distance a document sorts on: the distances of its
// points to every origin reduced by the sort mode; documents without a
// point sort as infinitely far.
func geoSortValue(h *hit, s sortSpec) (any, any, error) {
	f, _, mapped := h.ix.Mapping.resolve(s.field)
	if !mapped {
		if s.geo.ignoreUnmapped {
			return math.Inf(1), "Infinity", nil
		}
		return nil, nil, errSearchPhase(&Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "failed to find mapper for [" + s.field + "] for geo distance based sort", Index: h.ix.Name})
	}
	if f.Type != TypeGeoPoint {
		fielddata := "org.opensearch.index.fielddata.plain.SortedSetOrdinalsIndexFieldData"
		if f.isNumeric() || f.isDate() || f.Type == TypeBoolean {
			fielddata = "org.opensearch.index.fielddata.plain.SortedNumericIndexFieldData"
		}
		return nil, nil, errSearchPhase(&Error{Status: http.StatusInternalServerError, Type: "class_cast_exception", plain: true, Index: h.ix.Name,
			Reason: "class " + fielddata + " cannot be cast to class org.opensearch.index.fielddata.IndexGeoPointFieldData (" + fielddata + " and org.opensearch.index.fielddata.IndexGeoPointFieldData are in unnamed module of loader 'app')"})
	}
	origins := make([]geoPointSpec, 0, len(s.geo.points))
	for _, p := range s.geo.points {
		switch s.geo.validation {
		case "STRICT":
			if !validLat(p.lat) {
				return nil, nil, errSearchPhase(&Error{Status: http.StatusBadRequest, Type: "parse_exception", Index: h.ix.Name, Reason: "illegal latitude value [" + javaDouble(p.lat) + "] for [GeoDistanceSort] for field [" + s.field + "]."})
			}
			if !validLon(p.lon) {
				return nil, nil, errSearchPhase(&Error{Status: http.StatusBadRequest, Type: "parse_exception", Index: h.ix.Name, Reason: "illegal longitude value [" + javaDouble(p.lon) + "] for [GeoDistanceSort] for field [" + s.field + "]."})
			}
		case "COERCE":
			p = normalizeGeoPoint(p)
		}
		origins = append(origins, p)
	}
	var dists []float64
	for _, v := range h.ix.fieldValues(h.doc, s.field) {
		p, ok := v.([2]float64)
		if !ok {
			continue
		}
		lat, lon := encodedLatLon(p[0], p[1])
		for _, o := range origins {
			dists = append(dists, arcDistance(o.lat, o.lon, lat, lon)/s.geo.unitMeters)
		}
	}
	if len(dists) == 0 {
		return math.Inf(1), "Infinity", nil
	}
	sort.Float64s(dists)
	var d float64
	switch s.mode {
	case "max":
		d = dists[len(dists)-1]
	case "avg":
		for _, x := range dists {
			d += x
		}
		d /= float64(len(dists))
	case "median":
		d = dists[len(dists)/2]
		if len(dists)%2 == 0 {
			d = (dists[len(dists)/2-1] + dists[len(dists)/2]) / 2
		}
	default:
		d = dists[0]
	}
	return d, Double(d), nil
}

// preference ------------------------------------------------------------------

// preferenceShards applies a search preference to the shards of the targets
// (OperationRouting.preferenceActiveShardIterator runs for every shard):
// _shards:<ids> searches only the listed shards, and a preference type
// OpenSearch does not know fails. It returns nil when every shard is
// searched.
func preferenceShards(preference string, ts []target) (map[string]map[int]bool, error) {
	if preference == "" || preference[0] != '_' {
		return nil, nil
	}
	out := map[string]map[int]bool{}
	excluded := false
	for _, t := range ts {
		n := indexShardCount(t.ix)
		if out[t.ix.Name] == nil {
			out[t.ix.Name] = map[int]bool{}
		}
		for s := 0; s < n; s++ {
			searched, err := preferenceSelectsShard(preference, s)
			if err != nil {
				return nil, err
			}
			if searched {
				out[t.ix.Name][s] = true
			} else {
				excluded = true
			}
		}
	}
	if !excluded {
		return nil, nil
	}
	return out, nil
}

// preferenceSelectsShard evaluates a preference starting with "_" for one
// shard.
func preferenceSelectsShard(preference string, shard int) (bool, error) {
	typ, err := preferenceType(preference)
	if err != nil {
		return false, err
	}
	if typ == "_shards" {
		bar := strings.IndexByte(preference, '|')
		begin, end := len("_shards:"), len(preference)
		if bar >= 0 {
			end = bar
		}
		if begin > end {
			return false, errStringIndexOutOfBounds(begin, end, len(preference))
		}
		found := false
		for _, id := range javaSplitComma(preference[begin:end]) {
			n, ok := javaParseInt(id, 32)
			if !ok {
				return false, &Error{Status: http.StatusBadRequest, Type: "number_format_exception", Reason: javaNumberFormatReason(id)}
			}
			if int(n) == shard {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
		if bar < 0 || bar == len(preference)-1 {
			return true, nil
		}
		// the rest of the preference applies to the shard
		preference = preference[bar+1:]
		if typ, err = preferenceType(preference); err != nil {
			return false, err
		}
		if typ == "_shards" {
			return false, errIllegalArgument("unknown preference [SHARDS]")
		}
	}
	switch typ {
	case "_prefer_nodes", "_only_nodes":
		// the node criteria follow the colon
		if begin := len(typ) + 1; begin > len(preference) {
			return false, errStringIndexOutOfBounds(begin, len(preference), len(preference))
		}
	}
	return true, nil
}

// preferenceType is Preference.parse.
func preferenceType(preference string) (string, error) {
	typ := preference
	if i := strings.IndexByte(preference, ':'); i >= 0 {
		typ = preference[:i]
	}
	switch typ {
	case "_shards", "_prefer_nodes", "_local", "_only_local", "_onlyLocal", "_only_nodes":
		return typ, nil
	}
	return "", errIllegalArgument("no Preference for [%s]", typ)
}

// errStringIndexOutOfBounds is String.substring failing.
func errStringIndexOutOfBounds(begin, end, length int) *Error {
	return &Error{Status: http.StatusInternalServerError, Type: "string_index_out_of_bounds_exception",
		Reason: fmt.Sprintf("Range [%d, %d) out of bounds for length %d", begin, end, length)}
}

// javaSplitComma is Strings.splitStringByCommaToArray (String.split drops
// trailing empty strings).
func javaSplitComma(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// emptySearchResponse is the response of a search that has no shard to
// search (AbstractSearchAsyncAction.start): no hits, a zero max_score and
// no aggregations.
func (c *Cluster) emptySearchResponse(sr *searchRequest, p Params, start time.Time) M {
	hits := M{"max_score": Float(0), "hits": []any{}}
	if sr.trackTotalUpTo == nil || *sr.trackTotalUpTo != -1 {
		if sr.totalAsInt {
			hits["total"] = 0
		} else {
			hits["total"] = M{"value": 0, "relation": "eq"}
		}
	}
	res := M{"took": int(time.Since(start).Milliseconds()), "timed_out": false,
		"_shards": M{"total": 0, "successful": 0, "skipped": 0, "failed": 0}, "hits": hits}
	if c.phaseTookEnabled(p) {
		res["phase_took"] = phaseTookSection()
	}
	return res
}

// phase_took ------------------------------------------------------------------

// phaseTookEnabled reports whether a search response lists the time its
// phases took: the phase_took parameter, else the search.phase_took_enabled
// cluster setting (TransportSearchAction).
func (c *Cluster) phaseTookEnabled(p Params) bool {
	if p.Has("phase_took") {
		v := p.Get("phase_took")
		return v == "" || v == "true"
	}
	for _, scope := range []string{"transient", "persistent"} {
		if m, ok := c.clusterSettings[scope].(M); ok {
			if v, ok := m["search.phase_took_enabled"]; ok {
				return settingString(v) == "true"
			}
		}
	}
	return false
}

// phaseTookSection is the phase_took of a search response, by
// SearchPhaseName. osmem runs a search in a single step.
func phaseTookSection() M {
	return M{"dfs_pre_query": 0, "query": 0, "fetch": 0, "dfs_query": 0, "expand": 0, "can_match": 0}
}

// query errors ----------------------------------------------------------------

// shardParsingFailures are ParsingExceptions thrown while a query is created
// on a shard (DecayFunctionBuilder parses its function there): unlike the
// parsing exceptions of the request, they are shard failures.
var shardParsingFailures = struct {
	sync.Mutex
	m map[*Error]struct{}
}{m: map[*Error]struct{}{}}

// errShardParsing is a ParsingException raised while creating a query on a
// shard, at a location of the parser it creates there.
func errShardParsing(line, col int, format string, args ...any) *Error {
	e := &Error{Status: http.StatusBadRequest, Type: "parsing_exception", Reason: fmt.Sprintf(format, args...),
		Extra: map[string]any{"line": line, "col": col}}
	shardParsingFailures.Lock()
	if len(shardParsingFailures.m) > 4096 {
		shardParsingFailures.m = map[*Error]struct{}{}
	}
	shardParsingFailures.m[e] = struct{}{}
	shardParsingFailures.Unlock()
	return e
}

func isShardParsingFailure(e *Error) bool {
	shardParsingFailures.Lock()
	_, found := shardParsingFailures.m[e]
	shardParsingFailures.Unlock()
	return found
}

// queryContentError is RestActions.getQueryContent (count, explain and
// validate query) failing: an exception other than a ParsingException is
// wrapped in "Failed to parse" at the token the parser is on.
func queryContentError(perr *Error) *Error {
	if perr.Type == "parsing_exception" {
		return perr
	}
	w := &Error{Status: http.StatusBadRequest, Type: "parsing_exception", Reason: "Failed to parse", Cause: perr}
	if t := perr.parserTok(); t != nil {
		w.at(t)
	}
	return w
}
