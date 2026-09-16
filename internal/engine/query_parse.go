package engine

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
)

// The parse step of a query: the checks and conversions of OpenSearch's
// QueryBuilder.fromXContent parsers. parseQuery turns a query object into
// a tree of qnodes; queryBuilder.toQuery creates the bleve query of a node
// for one index.

type qnode struct {
	kind  string
	name  string  // _name
	boost float64 // boost applied around the created query
	spec  any
}

// queryKeys lists the keys of a query body in a stable order that keeps
// field names before the common parameters.
func queryKeys(m M) []string {
	keys := objectKeys(m)
	sort.SliceStable(keys, func(i, j int) bool {
		return paramRank(keys[i]) < paramRank(keys[j])
	})
	return keys
}

func paramRank(k string) int {
	switch k {
	case "boost", "_name":
		return 1
	}
	return 0
}

// parseQuery parses a query object {"<kind>": body}.
func parseQuery(v any) (*qnode, *Error) {
	m, ok := v.(M)
	if !ok {
		return nil, pParsing("[_na] query malformed, must start with start_object")
	}
	if len(m) == 0 {
		return nil, pIllegalArgument("query malformed, empty clause found at [1:1]").
			atInReason(endTok(m), "query malformed, empty clause found at [%s]")
	}
	keys := objectKeys(m)
	if len(keys) > 1 {
		return nil, pParsing("[%s] malformed query, expected [END_OBJECT] but found [FIELD_NAME]", keys[0]).at(nthKeyTok(m, 2))
	}
	kind := keys[0]
	body := m[kind]
	parse, known := queryParsers[kind]
	if !known {
		if unsupportedQueries[kind] {
			return &qnode{kind: kind, boost: 1, spec: body}, nil
		}
		return nil, withCause(pParsing("unknown query [%s]", kind).at(valueTok(m, kind)),
			(&Error{Type: "named_object_not_found_exception", Reason: "unknown field [" + kind + "]"}).at(valueTok(m, kind)))
	}
	if _, isObj := body.(M); !isObj {
		return nil, pParsing("[%s] query malformed, no start_object after query name", kind).at(valueTok(m, kind))
	}
	n, err := parse(body)
	if err != nil {
		return nil, err
	}
	if n.kind == "" {
		n.kind = kind
	}
	return n, nil
}

var queryParsers map[string]func(any) (*qnode, *Error)

func init() {
	queryParsers = map[string]func(any) (*qnode, *Error){
		"match_all":           parseMatchAll,
		"match_none":          parseMatchNone,
		"match":               parseMatch,
		"match_phrase":        func(b any) (*qnode, *Error) { return parseMatchPhrase("match_phrase", b) },
		"match_phrase_prefix": func(b any) (*qnode, *Error) { return parseMatchPhrase("match_phrase_prefix", b) },
		"match_bool_prefix":   parseMatchBoolPrefix,
		"term":                parseTerm,
		"prefix":              func(b any) (*qnode, *Error) { return parseMultiTerm("prefix", b) },
		"wildcard":            func(b any) (*qnode, *Error) { return parseMultiTerm("wildcard", b) },
		"regexp":              func(b any) (*qnode, *Error) { return parseMultiTerm("regexp", b) },
		"fuzzy":               parseFuzzy,
		"terms":               parseTerms,
		"terms_set":           parseTermsSet,
		"ids":                 parseIDs,
		"exists":              parseExists,
		"range":               parseRange,
		"bool":                parseBool,
		"constant_score":      parseConstantScore,
		"dis_max":             parseDisMax,
		"boosting":            parseBoosting,
		"function_score":      parseFunctionScore,
		"nested":              parseNested,
		"wrapper":             parseWrapper,
		"multi_match":         parseMultiMatch,
		"combined_fields":     parseCombinedFields,
		"common":              parseCommon,
		"query_string":        parseQueryString,
		"simple_query_string": parseSimpleQueryString,
		"geo_distance":        parseGeoDistance,
		"geo_bounding_box":    parseGeoBoundingBox,
		"geo_polygon":         parseGeoPolygon,
		"rank_feature":        parseRankFeature,
		"has_child":           parseHasChild,
		"has_parent":          parseHasParent,
		"parent_id":           parseParentID,
		"intervals":           parseIntervals,
		"more_like_this":      parseMoreLikeThis,
		"distance_feature":    parseDistanceFeature,
		"span_term":           parseSpanTerm,
		"span_near":           parseSpanNear,
		"span_multi":          parseSpanMulti,
		"geo_shape":           parseGeoShapeQuery,
		"script":              parseScriptQuery,
		"script_score":        parseScriptScoreQuery,
	}
}

// unsupportedQueries are queries OpenSearch knows that osmem does not
// implement; they fail when created.
var unsupportedQueries = map[string]bool{
	"knn": true, "neural": true, "neural_sparse": true, "percolate": true,
	"xy_shape": true, "hybrid": true,
	"template": true,
	// not implemented yet
	"span_or": true, "span_first": true, "span_not": true,
	"span_containing": true, "span_within": true, "field_masking_span": true, "span_field_masking": true,
}

// common parameter conversions -----------------------------------------------

// parseBoostValue reads a boost and rejects negative boosts the way
// AbstractQueryBuilder.boost does.
func parseBoostValue(kind string, body any, v any) (float64, *Error) {
	f, err := xFloat(v)
	if err != nil {
		return 0, err
	}
	if f < 0 {
		return 0, negativeBoostError(kind, body)
	}
	return f, nil
}

func negativeBoostError(kind string, body any) *Error {
	return pIllegalArgument("negative [boost] are not allowed in [%s], use a value between 0 and 1 to deboost", renderBuilder(kind, body))
}

// renderBuilder renders a query the way QueryBuilder.toString does (with
// the default boost it has when the negative boost is rejected).
func renderBuilder(kind string, body any) string {
	copy := resetBoosts(cloneDeep(body))
	data, err := EncodeJSON(M{kind: copy}, true)
	if err != nil {
		return kind
	}
	return strings.TrimRight(string(data), "\n")
}

func resetBoosts(v any) any {
	switch t := v.(type) {
	case M:
		for k, e := range t {
			if k == "boost" {
				t[k] = Float(1)
				continue
			}
			t[k] = resetBoosts(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = resetBoosts(e)
		}
	}
	return v
}

// unknownTokenError is "[kind] unknown token [TOKEN] after [param]".
func unknownTokenError(kind, param string, v any) *Error {
	return pParsing("[%s] unknown token [%s] after [%s]", kind, xTokenName(v), param)
}

// parseOperator is Operator.fromString.
func parseOperator(v any) (string, *Error) {
	s := strings.ToUpper(xText(v))
	switch s {
	case "OR", "AND":
		return strings.ToLower(s), nil
	}
	return "", pIllegalArgument("No enum constant org.opensearch.index.query.Operator.%s", s)
}

func parseZeroTerms(v any, at *tokenRef) (bool, *Error) {
	s := xText(v)
	switch strings.ToLower(s) {
	case "none":
		return false, nil
	case "all":
		return true, nil
	}
	return false, pParsing("Unsupported zero_terms_query value [%s]", s).at(at)
}

// fuzzinessSpec is a parsed Fuzziness.
type fuzzinessSpec struct {
	auto      bool
	low, high int
	value     string // the numeric text when not AUTO
}

// parseFuzziness is Fuzziness.parse for a request value.
func parseFuzziness(kind string, v any, at *tokenRef) (*fuzzinessSpec, *Error) {
	switch v.(type) {
	case string, json.Number, float64, int, int64:
	case bool:
		return nil, pIllegalArgument("Can't parse fuzziness on token: [VALUE_BOOLEAN]")
	default:
		return nil, unknownTokenError(kind, "fuzziness", v).at(at)
	}
	return buildFuzziness(xText(v))
}

// buildFuzziness is Fuzziness.build with OpenSearch's validation.
func buildFuzziness(s string) (*fuzzinessSpec, *Error) {
	up := strings.ToUpper(s)
	if up == "AUTO" {
		return &fuzzinessSpec{auto: true, low: 3, high: 6}, nil
	}
	if strings.HasPrefix(up, "AUTO:") {
		parts := strings.Split(s[5:], ",")
		if len(parts) != 2 {
			return nil, pParse("failed to find low and high distance values")
		}
		low, err1 := strconv.Atoi(parts[0])
		if err1 != nil {
			return nil, withCause(pParse("failed to parse [%s] as a \"auto:int,int\"", s), &Error{Type: "number_format_exception", Reason: javaNumberFormatReason(parts[0])})
		}
		high, err2 := strconv.Atoi(parts[1])
		if err2 != nil {
			return nil, withCause(pParse("failed to parse [%s] as a \"auto:int,int\"", s), &Error{Type: "number_format_exception", Reason: javaNumberFormatReason(parts[1])})
		}
		if low < 0 || high < 0 || low > high {
			return nil, pIllegalArgument("fuzziness wrongly configured, must be: lowDistance > 0, highDistance > 0 and lowDistance <= highDistance")
		}
		return &fuzzinessSpec{auto: true, low: low, high: high}, nil
	}
	f, ok := javaDoubleOK(s)
	if !ok || f < 0 || math.IsNaN(f) {
		return nil, pIllegalArgument("Invalid fuzziness value: %s", s)
	}
	return &fuzzinessSpec{value: strings.TrimSpace(s)}, nil
}

// distance is Fuzziness.asDistance for a term.
func (fz *fuzzinessSpec) distance(term string) int {
	if fz == nil {
		return 0
	}
	if fz.auto {
		n := len([]rune(term))
		switch {
		case n < fz.low:
			return 0
		case n < fz.high:
			return 1
		}
		return 2
	}
	f, _ := javaDoubleOK(fz.value)
	if f > 2 {
		return 2
	}
	return int(f)
}

// parseRewrite validates a rewrite method name (QueryParsers.parseRewriteMethod).
func validRewrite(s string) bool {
	switch s {
	case "constant_score", "scoring_boolean", "constant_score_boolean":
		return true
	}
	i := strings.IndexAny(s, "0123456789")
	if i < 0 {
		return false
	}
	if _, err := strconv.Atoi(s[i:]); err != nil {
		return false
	}
	switch s[:i] {
	case "top_terms_", "top_terms_boost_", "top_terms_blended_freqs_":
		return true
	}
	return false
}

// scoringRewrite reports whether a rewrite method scores terms (rather than
// giving every match the boost).
func scoringRewrite(s string) bool {
	return s != "" && s != "constant_score" && s != "constant_score_boolean"
}

// msmText reads a minimum_should_match value (textOrNull).
func msmText(v any) *string {
	if v == nil {
		return nil
	}
	s := xText(v)
	return &s
}

// calcMinShouldMatch is Queries.calculateMinShouldMatch. The error is the
// message of the Java exception (with its type).
func calcMinShouldMatch(optional int, spec string) (int, *Error) {
	result := optional
	spec = strings.TrimSpace(spec)
	if strings.Contains(spec, "<") {
		spec = msmSpaceAroundLess(spec)
		for _, s := range strings.Fields(spec) {
			parts := strings.Split(s, "<")
			// Java's String.split drops trailing empty strings
			for len(parts) > 0 && parts[len(parts)-1] == "" {
				parts = parts[:len(parts)-1]
			}
			if len(parts) == 0 {
				return 0, errCreateQuery("array_index_out_of_bounds_exception", "Index 0 out of bounds for length 0")
			}
			upper, err := strconv.Atoi(parts[0])
			if err != nil {
				return 0, errCreateNumberFormat(parts[0])
			}
			if optional <= upper {
				return result, nil
			}
			if len(parts) < 2 {
				return 0, errCreateQuery("array_index_out_of_bounds_exception", "Index 1 out of bounds for length 1")
			}
			r, e := calcMinShouldMatch(optional, parts[1])
			if e != nil {
				return 0, e
			}
			result = r
		}
		return result, nil
	}
	if strings.Contains(spec, "%") {
		num := spec[:len(spec)-1]
		percent, err := strconv.Atoi(num)
		if err != nil {
			return 0, errCreateNumberFormat(num)
		}
		calc := float32(result*percent) * (1 / float32(100))
		if calc < 0 {
			result = result + int(calc)
		} else {
			result = int(calc)
		}
	} else {
		calc, err := strconv.Atoi(spec)
		if err != nil {
			return 0, errCreateNumberFormat(spec)
		}
		if calc < 0 {
			result = result + calc
		} else {
			result = calc
		}
	}
	if result < 0 {
		return 0, nil
	}
	return result, nil
}

func msmSpaceAroundLess(s string) string {
	for strings.Contains(s, " <") || strings.Contains(s, "< ") {
		s = strings.ReplaceAll(strings.ReplaceAll(s, " <", "<"), "< ", "<")
	}
	return s
}

// match family ---------------------------------------------------------------

type matchSpec struct {
	field             string
	query             any
	analyzer          string
	operator          string
	msm               *string
	fuzziness         *fuzzinessSpec
	prefixLength      int
	maxExpansions     int
	transpositions    bool
	fuzzyRewrite      *string
	lenient           bool
	zeroTermsAll      bool
	autoSynonymPhrase bool
}

func parseMatchAll(body any) (*qnode, *Error) {
	return parseBoostOnly("match_all", body, true)
}

func parseMatchNone(body any) (*qnode, *Error) {
	return parseBoostOnly("match_none", body, false)
}

// parseBoostOnly parses queries whose only parameters are boost and _name.
// objectParser selects the ObjectParser flavour of errors (match_all).
func parseBoostOnly(kind string, body any, objectParser bool) (*qnode, *Error) {
	m, ok := body.(M)
	if !ok {
		return nil, pParsing("[%s] query malformed, no start_object after query name", kind)
	}
	n := &qnode{boost: 1}
	for _, k := range queryKeys(m) {
		v := m[k]
		switch k {
		case "boost":
			if objectParser {
				if _, isBool := v.(bool); isBool || !isXValue(v) {
					return nil, objectParserTypeError(kind, m, k)
				}
				f, err := xFloat(v)
				if err != nil {
					return nil, objectParserFieldError(kind, m, k, err)
				}
				if f < 0 {
					return nil, objectParserFieldError(kind, m, k, negativeBoostError(kind, body))
				}
				n.boost = f
				continue
			}
			b, err := parseBoostValue(kind, body, v)
			if err != nil {
				return nil, err
			}
			n.boost = b
		case "_name":
			n.name = xText(v)
		default:
			if objectParser {
				e := pXContent("[%s] unknown field [%s]%s", kind, k, didYouMean(k, []string{"boost", "_name"})).
					at(keyTok(m, k)).atParser(valueTok(m, k))
				return nil, withCause(pParsing("%s", e.Reason), e).at(valueTok(m, k)).withCauseLocation()
			}
			return nil, pParsing("[%s] query does not support [%s]", kind, k).at(valueTok(m, k))
		}
	}
	return n, nil
}

// objectParserFieldError is ObjectParser's "failed to parse field" as
// AbstractQueryBuilder reports it for match_all and ids.
func objectParserFieldError(kind string, m M, field string, cause *Error) *Error {
	x := (&Error{Status: 400, Type: "x_content_parse_exception", Reason: "[" + kind + "] failed to parse field [" + field + "]", Cause: cause}).
		atCause(valueEndTok(m, field))
	return withCause(pParsing("[%s] failed to parse field [%s]", kind, field), x).at(x.parserTok()).withCauseLocation()
}

func objectParserTypeError(kind string, m M, field string) *Error {
	reason := "[" + kind + "] " + field + " doesn't support values of type: " + xTokenName(m[field])
	x := (&Error{Status: 400, Type: "x_content_parse_exception", Reason: reason}).at(valueTok(m, field))
	return withCause(pParsing("%s", reason), x).at(valueTok(m, field)).withCauseLocation()
}

// didYouMean is ObjectParser's suggestion for an unknown field.
func didYouMean(field string, candidates []string) string {
	type scored struct {
		d float32
		s string
	}
	var list []scored
	for _, c := range candidates {
		if d := luceneLevenshtein(field, c); d > 0.5 {
			list = append(list, scored{d, c})
		}
	}
	if len(list) == 0 {
		return ""
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].d != list[j].d {
			return list[i].d > list[j].d
		}
		return list[i].s < list[j].s
	})
	if len(list) == 1 {
		return " did you mean [" + list[0].s + "]?"
	}
	names := make([]string, len(list))
	for i, e := range list {
		names[i] = e.s
	}
	return " did you mean any of [" + strings.Join(names, ", ") + "]?"
}

// luceneLevenshtein is Lucene's LevenshteinDistance.getDistance.
func luceneLevenshtein(target, other string) float32 {
	sa, ta := []rune(other), []rune(target)
	n := len(ta)
	if n == 0 || len(sa) == 0 {
		if n == len(sa) {
			return 1
		}
		return 0
	}
	d := editDistance(ta, sa, false, len(ta)+len(sa))
	max := len(sa)
	if n > max {
		max = n
	}
	return 1 - float32(d)/float32(max)
}

// fieldEntry is the single field of a field-level query body.
type fieldEntry struct {
	field string
	value any
}

// singleField finds the field of a field-level query, rejecting a second
// field the way throwParsingExceptionOnMultipleFields does. Keys whose
// value is null are skipped when skipNull is set.
func singleField(kind string, m M, skipNull bool) (*fieldEntry, *Error) {
	var found *fieldEntry
	for _, k := range queryKeys(m) {
		v := m[k]
		if skipNull && v == nil {
			continue
		}
		if found != nil {
			return nil, pParsing("[%s] query doesn't support multiple fields, found [%s] and [%s]", kind, found.field, k).at(valueTok(m, k))
		}
		found = &fieldEntry{field: k, value: v}
	}
	return found, nil
}

func parseMatch(body any) (*qnode, *Error) {
	m := body.(M)
	fe, err := singleField("match", m, false)
	if err != nil {
		return nil, err
	}
	n := &qnode{boost: 1}
	spec := &matchSpec{operator: "or", maxExpansions: 50, transpositions: true, autoSynonymPhrase: true}
	if fe != nil {
		spec.field = fe.field
		switch t := fe.value.(type) {
		case M:
			for _, k := range queryKeys(t) {
				v := t[k]
				if !isXValue(v) {
					return nil, unknownTokenError("match", k, v).at(valueTok(t, k))
				}
				switch k {
				case "query":
					spec.query = v
				case "analyzer":
					spec.analyzer = xText(v)
				case "boost":
					f, e := xFloat(v)
					if e != nil {
						return nil, e
					}
					n.boost = f
				case "fuzziness":
					if spec.fuzziness, err = parseFuzziness("match", v, valueTok(t, k)); err != nil {
						return nil, err
					}
				case "prefix_length":
					if spec.prefixLength, err = xInt(v); err != nil {
						return nil, err
					}
				case "max_expansions":
					if spec.maxExpansions, err = xInt(v); err != nil {
						return nil, err
					}
				case "operator":
					if spec.operator, err = parseOperator(v); err != nil {
						return nil, err
					}
				case "minimum_should_match":
					spec.msm = msmText(v)
				case "fuzzy_rewrite":
					spec.fuzzyRewrite = msmText(v)
				case "fuzzy_transpositions":
					if spec.transpositions, err = xBool(v); err != nil {
						return nil, err
					}
				case "lenient":
					if spec.lenient, err = xBool(v); err != nil {
						return nil, err
					}
				case "zero_terms_query":
					if spec.zeroTermsAll, err = parseZeroTerms(v, valueTok(t, k)); err != nil {
						return nil, err
					}
				case "_name":
					n.name = xText(v)
				case "auto_generate_synonyms_phrase_query":
					if spec.autoSynonymPhrase, err = xBool(v); err != nil {
						return nil, err
					}
				case "cutoff_frequency":
					if _, err = xFloat(v); err != nil {
						return nil, err
					}
				default:
					return nil, pParsing("[match] query does not support [%s]", k).at(valueTok(t, k))
				}
			}
		case []any:
			return nil, noTextAt(t, valueTok(m, fe.field))
		default:
			spec.query = t
		}
	}
	if spec.query == nil {
		return nil, pParsing("No text specified for text query").at(endTok(m))
	}
	if spec.prefixLength < 0 {
		return nil, pIllegalArgument("[match] requires prefix length to be non-negative.")
	}
	if spec.maxExpansions <= 0 {
		return nil, pIllegalArgument("[match] requires maxExpansions to be positive.")
	}
	if n.boost < 0 {
		return nil, negativeBoostError("match", body)
	}
	n.spec = spec
	return n, nil
}

type phraseSpec struct {
	field         string
	query         any
	analyzer      string
	slop          int
	maxExpansions int
	zeroTermsAll  bool
	prefix        bool
}

func parseMatchPhrase(kind string, body any) (*qnode, *Error) {
	m := body.(M)
	fe, err := singleField(kind, m, false)
	if err != nil {
		return nil, err
	}
	n := &qnode{boost: 1}
	spec := &phraseSpec{maxExpansions: 50, prefix: kind == "match_phrase_prefix"}
	if fe != nil {
		spec.field = fe.field
		switch t := fe.value.(type) {
		case M:
			for _, k := range queryKeys(t) {
				v := t[k]
				if !isXValue(v) {
					return nil, unknownTokenError(kind, k, v).at(valueTok(t, k))
				}
				switch {
				case k == "query":
					spec.query = v
				case k == "analyzer":
					spec.analyzer = xText(v)
				case k == "boost":
					f, e := xFloat(v)
					if e != nil {
						return nil, e
					}
					n.boost = f
				case k == "slop":
					if spec.slop, err = xInt(v); err != nil {
						return nil, err
					}
				case k == "max_expansions" && spec.prefix:
					if spec.maxExpansions, err = xInt(v); err != nil {
						return nil, err
					}
				case k == "zero_terms_query":
					if spec.zeroTermsAll, err = parseZeroTerms(v, valueTok(t, k)); err != nil {
						return nil, err
					}
				case k == "_name":
					n.name = xText(v)
				default:
					return nil, pParsing("[%s] query does not support [%s]", kind, k).at(valueTok(t, k))
				}
			}
		case []any:
			return nil, noTextAt(t, valueTok(m, fe.field))
		default:
			spec.query = t
		}
	}
	if fe == nil {
		return nil, pIllegalArgument("[%s] requires fieldName", kind)
	}
	if spec.query == nil {
		return nil, pIllegalArgument("[%s] requires query value", kind)
	}
	if spec.slop < 0 {
		return nil, pIllegalArgument("No negative slop allowed.")
	}
	if spec.maxExpansions < 0 {
		return nil, pIllegalArgument("No negative maxExpansions allowed.")
	}
	if n.boost < 0 {
		return nil, negativeBoostError(kind, body)
	}
	n.spec = spec
	return n, nil
}

func parseMatchBoolPrefix(body any) (*qnode, *Error) {
	const kind = "match_bool_prefix"
	m := body.(M)
	fe, err := singleField(kind, m, false)
	if err != nil {
		return nil, err
	}
	n := &qnode{boost: 1}
	spec := &matchSpec{operator: "or", maxExpansions: 50, transpositions: true, autoSynonymPhrase: true}
	if fe != nil {
		spec.field = fe.field
		switch t := fe.value.(type) {
		case M:
			for _, k := range queryKeys(t) {
				v := t[k]
				if !isXValue(v) {
					return nil, unknownTokenError(kind, k, v).at(valueTok(t, k))
				}
				switch k {
				case "query":
					spec.query = v
				case "analyzer":
					spec.analyzer = xText(v)
				case "operator":
					if spec.operator, err = parseOperator(v); err != nil {
						return nil, err
					}
				case "minimum_should_match":
					spec.msm = msmText(v)
				case "fuzziness":
					if spec.fuzziness, err = parseFuzziness(kind, v, valueTok(t, k)); err != nil {
						return nil, err
					}
				case "prefix_length":
					if spec.prefixLength, err = xInt(v); err != nil {
						return nil, err
					}
				case "max_expansions":
					if spec.maxExpansions, err = xInt(v); err != nil {
						return nil, err
					}
				case "fuzzy_transpositions":
					if spec.transpositions, err = xBool(v); err != nil {
						return nil, err
					}
				case "fuzzy_rewrite":
					spec.fuzzyRewrite = msmText(v)
				case "boost":
					f, e := xFloat(v)
					if e != nil {
						return nil, e
					}
					n.boost = f
				case "_name":
					n.name = xText(v)
				default:
					return nil, pParsing("[%s] query does not support [%s]", kind, k).at(valueTok(t, k))
				}
			}
		case []any:
			return nil, noTextAt(t, valueTok(m, fe.field))
		default:
			spec.query = t
		}
	}
	if fe == nil {
		return nil, pIllegalArgument("[%s] requires fieldName", kind)
	}
	if spec.query == nil {
		return nil, pIllegalArgument("[%s] requires query value", kind)
	}
	if spec.prefixLength < 0 {
		return nil, pIllegalArgument("[%s] requires prefix length to be non-negative.", kind)
	}
	if spec.maxExpansions <= 0 {
		return nil, pIllegalArgument("[%s] requires maxExpansions to be positive.", kind)
	}
	if n.boost < 0 {
		return nil, negativeBoostError(kind, body)
	}
	n.spec = spec
	return n, nil
}

// term-level queries -----------------------------------------------------------

type termSpec struct {
	field           string
	value           any
	caseInsensitive bool
}

func parseTerm(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &termSpec{}
	var field string
	for _, k := range queryKeys(m) {
		v := m[k]
		switch t := v.(type) {
		case nil:
			continue
		case []any:
			return nil, pParsing("[term] query does not support array of values").at(valueTok(m, k))
		case M:
			if field != "" {
				return nil, pParsing("[term] query doesn't support multiple fields, found [%s] and [%s]", field, k).at(valueTok(m, k))
			}
			field = k
			for _, pk := range queryKeys(t) {
				pv := t[pk]
				switch pk {
				case "value", "term":
					if obj, isObj := pv.(M); isObj {
						keys := objectKeys(obj)
						if len(keys) > 0 {
							return nil, pParsing("[term] query does not support [%s]", keys[0]).at(valueTok(obj, keys[0]))
						}
						continue
					}
					spec.value = pv
				case "_name":
					n.name = xText(pv)
				case "boost":
					f, err := xFloat(pv)
					if err != nil {
						return nil, err
					}
					n.boost = f
				case "case_insensitive":
					b, err := xBool(pv)
					if err != nil {
						return nil, err
					}
					spec.caseInsensitive = b
				default:
					return nil, pParsing("[term] query does not support [%s]", pk).at(valueTok(t, pk))
				}
			}
		default:
			if field != "" {
				return nil, pParsing("[term] query doesn't support multiple fields, found [%s] and [%s]", field, k).at(valueTok(m, k))
			}
			field = k
			spec.value = t
		}
	}
	if field == "" {
		return nil, pIllegalArgument("field name is null or empty")
	}
	if spec.value == nil {
		return nil, pIllegalArgument("value cannot be null")
	}
	spec.field = field
	if n.boost < 0 {
		return nil, negativeBoostError("term", body)
	}
	n.spec = spec
	return n, nil
}

// multiTermSpec is prefix, wildcard or regexp.
type multiTermSpec struct {
	field           string
	value           string
	rewrite         *string
	caseInsensitive bool
	flags           int
	maxStates       int
}

func parseMultiTerm(kind string, body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &multiTermSpec{flags: reFlagAll, maxStates: 10000}
	var field string
	hasValue := false
	valueKeys := map[string]bool{"value": true}
	if kind == "wildcard" {
		valueKeys["wildcard"] = true
	}
	readText := func(v any, nullable bool, at *tokenRef) (string, bool, *Error) {
		switch v.(type) {
		case nil:
			if nullable {
				return "", false, nil
			}
			return "", false, noTextAt(v, at)
		case M, []any:
			return "", false, noTextAt(v, at)
		}
		return xText(v), true, nil
	}
	for _, k := range queryKeys(m) {
		v := m[k]
		obj, isObj := v.(M)
		if field != "" {
			return nil, pParsing("[%s] query doesn't support multiple fields, found [%s] and [%s]", kind, field, k).at(valueTok(m, k))
		}
		field = k
		if !isObj {
			s, ok, err := readText(v, kind != "wildcard", valueTok(m, k))
			if err != nil {
				return nil, err
			}
			spec.value, hasValue = s, ok
			continue
		}
		for _, pk := range queryKeys(obj) {
			pv := obj[pk]
			switch {
			case valueKeys[pk]:
				s, ok, err := readText(pv, kind != "wildcard", valueTok(obj, pk))
				if err != nil {
					return nil, err
				}
				spec.value, hasValue = s, ok
			case pk == "boost":
				f, err := xFloat(pv)
				if err != nil {
					return nil, err
				}
				n.boost = f
			case pk == "_name":
				n.name = xText(pv)
			case pk == "rewrite":
				spec.rewrite = msmText(pv)
			case pk == "case_insensitive":
				b, err := xBool(pv)
				if err != nil {
					return nil, err
				}
				spec.caseInsensitive = b
			case kind == "regexp" && pk == "flags":
				flags, err := parseRegexpFlags(xText(pv))
				if err != nil {
					return nil, err
				}
				spec.flags = flags
			case kind == "regexp" && pk == "flags_value":
				f, err := xInt(pv)
				if err != nil {
					return nil, err
				}
				spec.flags = f
			case kind == "regexp" && pk == "max_determinized_states":
				f, err := xInt(pv)
				if err != nil {
					return nil, err
				}
				spec.maxStates = f
			default:
				return nil, pParsing("[%s] query does not support [%s]", kind, pk).at(valueTok(obj, pk))
			}
		}
	}
	if field == "" {
		return nil, pIllegalArgument("field name is null or empty")
	}
	if !hasValue {
		return nil, pIllegalArgument("value cannot be null")
	}
	if kind == "regexp" && spec.maxStates < 0 {
		return nil, pIllegalArgument("[regexp] max_determinized_states cannot be negative but was [%d]", spec.maxStates)
	}
	spec.field = field
	if n.boost < 0 {
		return nil, negativeBoostError(kind, body)
	}
	n.spec = spec
	return n, nil
}

type fuzzySpec struct {
	field          string
	value          any
	fuzziness      *fuzzinessSpec
	prefixLength   int
	maxExpansions  int
	transpositions bool
	rewrite        *string
}

func parseFuzzy(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &fuzzySpec{maxExpansions: 50, transpositions: true}
	var field string
	for _, k := range queryKeys(m) {
		v := m[k]
		if field != "" {
			return nil, pParsing("[fuzzy] query doesn't support multiple fields, found [%s] and [%s]", field, k).at(valueTok(m, k))
		}
		switch t := v.(type) {
		case M:
			field = k
			for _, pk := range queryKeys(t) {
				pv := t[pk]
				if _, isObj := pv.(M); isObj {
					return nil, pParsing("[fuzzy] unexpected token [START_OBJECT] after [%s]", pk).at(valueTok(t, pk))
				}
				var err *Error
				switch pk {
				case "term", "value":
					if pv != nil {
						spec.value = pv
					}
				case "boost":
					n.boost, err = xFloat(pv)
				case "fuzziness":
					spec.fuzziness, err = parseFuzziness("fuzzy", pv, valueTok(t, pk))
				case "prefix_length":
					spec.prefixLength, err = xInt(pv)
				case "max_expansions":
					spec.maxExpansions, err = xInt(pv)
				case "transpositions":
					spec.transpositions, err = xBool(pv)
				case "rewrite":
					spec.rewrite = msmText(pv)
				case "_name":
					n.name = xText(pv)
				default:
					return nil, pParsing("[fuzzy] query does not support [%s]", pk).at(valueTok(t, pk))
				}
				if err != nil {
					return nil, err
				}
			}
		case []any:
			return nil, pParsing("[fuzzy] query doesn't support multiple fields, found [%s] and [null]", k).at(firstInsideTok(m, k))
		default:
			field = k
			spec.value = t
		}
	}
	if field == "" {
		return nil, pIllegalArgument("field name cannot be null or empty")
	}
	if spec.value == nil {
		return nil, pIllegalArgument("query value cannot be null")
	}
	spec.field = field
	if n.boost < 0 {
		return nil, negativeBoostError("fuzzy", body)
	}
	n.spec = spec
	return n, nil
}
