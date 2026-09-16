package engine

import (
	"strings"
)

// Parsers of the full text queries over several fields.

type fieldWeight struct {
	field string
	boost float64
}

// parseFieldAndWeight is QueryParserHelper.parseFieldsAndWeights for one
// "field^boost" entry.
func parseFieldAndWeight(s string) (fieldWeight, *Error) {
	i := strings.IndexByte(s, '^')
	if i < 0 {
		return fieldWeight{field: s, boost: 1}, nil
	}
	b, ok := javaDoubleOK(s[i+1:])
	if !ok {
		return fieldWeight{}, pNumberFormat(s[i+1:])
	}
	return fieldWeight{field: s[:i], boost: float64(float32(b))}, nil
}

// multi_match ----------------------------------------------------------------

type multiMatchSpec struct {
	query             any
	fields            []fieldWeight
	typ               string
	analyzer          string
	slop              int
	fuzziness         *fuzzinessSpec
	prefixLength      int
	maxExpansions     int
	operator          string
	msm               *string
	fuzzyRewrite      *string
	tieBreaker        *float64
	lenient           *bool
	zeroTermsAll      bool
	autoSynonymPhrase bool
	transpositions    bool
}

var multiMatchTypes = map[string]bool{"best_fields": true, "most_fields": true, "cross_fields": true, "phrase": true, "phrase_prefix": true, "bool_prefix": true}

func parseMultiMatchType(s string) (string, *Error) {
	if multiMatchTypes[s] {
		return s, nil
	}
	return "", pParse("failed to parse [multi_match] query type [%s]. unknown type.", s)
}

func parseMultiMatch(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &multiMatchSpec{typ: "best_fields", operator: "or", maxExpansions: 50, autoSynonymPhrase: true, transpositions: true}
	hasCutoffFrequency := false
	for _, k := range queryKeys(m) {
		v := m[k]
		if k == "fields" {
			switch t := v.(type) {
			case []any:
				for _, e := range t {
					fw, err := parseFieldAndWeight(xText(e))
					if err != nil {
						return nil, err
					}
					spec.fields = append(spec.fields, fw)
				}
				continue
			default:
				if isXValue(t) {
					fw, err := parseFieldAndWeight(xText(t))
					if err != nil {
						return nil, err
					}
					spec.fields = append(spec.fields, fw)
					continue
				}
				return nil, pParsing("[multi_match] query does not support [%s]", k).at(valueTok(m, k))
			}
		}
		if !isXValue(v) {
			return nil, unknownTokenError("multi_match", k, v).at(valueTok(m, k))
		}
		var err *Error
		switch k {
		case "query":
			spec.query = v
		case "type":
			spec.typ, err = parseMultiMatchType(xText(v))
		case "analyzer":
			spec.analyzer = xText(v)
		case "boost":
			n.boost, err = xFloat(v)
		case "slop":
			spec.slop, err = xInt(v)
		case "fuzziness":
			spec.fuzziness, err = parseFuzziness("multi_match", v, valueTok(m, k))
		case "prefix_length":
			spec.prefixLength, err = xInt(v)
		case "max_expansions":
			spec.maxExpansions, err = xInt(v)
		case "operator":
			spec.operator, err = parseOperator(v)
		case "minimum_should_match":
			spec.msm = msmText(v)
		case "fuzzy_rewrite":
			spec.fuzzyRewrite = msmText(v)
		case "tie_breaker":
			var f float64
			f, err = xFloat(v)
			spec.tieBreaker = &f
		case "cutoff_frequency":
			_, err = xFloat(v)
			hasCutoffFrequency = true
		case "lenient":
			var b bool
			b, err = xBool(v)
			spec.lenient = &b
		case "zero_terms_query":
			spec.zeroTermsAll, err = parseZeroTerms(v, valueTok(m, k))
		case "_name":
			n.name = xText(v)
		case "auto_generate_synonyms_phrase_query":
			spec.autoSynonymPhrase, err = xBool(v)
		case "fuzzy_transpositions":
			spec.transpositions, err = xBool(v)
		default:
			return nil, pParsing("[multi_match] query does not support [%s]", k).at(valueTok(m, k))
		}
		if err != nil {
			return nil, err
		}
	}
	if spec.query == nil {
		return nil, pParsing("No text specified for multi_match query").at(endTok(m))
	}
	if spec.fuzziness != nil {
		switch spec.typ {
		case "cross_fields", "phrase", "phrase_prefix":
			return nil, pParsing("Fuzziness not allowed for type [%s]", spec.typ).at(endTok(m))
		}
	}
	if spec.slop != 0 && spec.typ == "bool_prefix" {
		return nil, pParsing("[slop] not allowed for type [bool_prefix]").at(endTok(m))
	}
	if hasCutoffFrequency && spec.typ == "bool_prefix" {
		return nil, pParsing("[cutoff_frequency] not allowed for type [bool_prefix]").at(endTok(m))
	}
	if spec.slop < 0 {
		return nil, pIllegalArgument("No negative slop allowed.")
	}
	if spec.prefixLength < 0 {
		return nil, pIllegalArgument("[multi_match] requires prefix length to be non-negative.")
	}
	if spec.maxExpansions <= 0 {
		return nil, pIllegalArgument("[multi_match] requires maxExpansions to be positive.")
	}
	if n.boost < 0 {
		return nil, negativeBoostError("multi_match", body)
	}
	n.spec = spec
	return n, nil
}

// combined_fields ------------------------------------------------------------

type combinedFieldsSpec struct {
	query        any
	fields       []fieldWeight
	operator     string
	msm          *string
	zeroTermsAll bool
}

func parseCombinedFields(body any) (*qnode, *Error) {
	const kind = "combined_fields"
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &combinedFieldsSpec{operator: "or"}
	for _, k := range queryKeys(m) {
		v := m[k]
		if k == "fields" {
			list, ok := v.([]any)
			if !ok {
				if !isXValue(v) {
					return nil, pParsing("[%s] query does not support [%s]", kind, k).at(valueTok(m, k))
				}
				list = []any{v}
			}
			for _, e := range list {
				fw, err := parseFieldAndWeight(xText(e))
				if err != nil {
					return nil, err
				}
				spec.fields = append(spec.fields, fw)
			}
			continue
		}
		if !isXValue(v) {
			return nil, unknownTokenError(kind, k, v).at(valueTok(m, k))
		}
		var err *Error
		switch k {
		case "query":
			spec.query = v
		case "operator":
			spec.operator, err = parseOperator(v)
		case "minimum_should_match":
			spec.msm = msmText(v)
		case "zero_terms_query":
			spec.zeroTermsAll, err = parseZeroTerms(v, valueTok(m, k))
		case "auto_generate_synonyms_phrase_query":
			_, err = xBool(v)
		case "boost":
			n.boost, err = xFloat(v)
		case "_name":
			n.name = xText(v)
		default:
			return nil, pParsing("[%s] query does not support [%s]", kind, k).at(valueTok(m, k))
		}
		if err != nil {
			return nil, err
		}
	}
	if spec.query == nil {
		return nil, pParsing("No text specified for %s query", kind).at(endTok(m))
	}
	n.spec = spec
	return n, nil
}

// common ---------------------------------------------------------------------

type commonSpec struct {
	field            string
	query            any
	cutoff           float64
	lowFreqOperator  string
	highFreqOperator string
	analyzer         string
	lowFreqMSM       *string
	highFreqMSM      *string
}

func parseCommon(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &commonSpec{cutoff: 0.01, lowFreqOperator: "or", highFreqOperator: "or"}
	fe, err := singleField("common", m, false)
	if err != nil {
		return nil, err
	}
	if fe != nil {
		spec.field = fe.field
		switch t := fe.value.(type) {
		case M:
			for _, k := range queryKeys(t) {
				v := t[k]
				if k == "minimum_should_match" {
					if obj, ok := v.(M); ok {
						spec.lowFreqMSM = msmText(obj["low_freq"])
						spec.highFreqMSM = msmText(obj["high_freq"])
						continue
					}
				}
				if !isXValue(v) {
					return nil, unknownTokenError("common", k, v).at(valueTok(t, k))
				}
				var e *Error
				switch k {
				case "query":
					spec.query = v
				case "analyzer":
					spec.analyzer = xText(v)
				case "disable_coord":
					_, e = xBool(v)
				case "boost":
					n.boost, e = xFloat(v)
				case "high_freq_operator":
					spec.highFreqOperator, e = parseOperator(v)
				case "low_freq_operator":
					spec.lowFreqOperator, e = parseOperator(v)
				case "minimum_should_match":
					spec.lowFreqMSM = msmText(v)
				case "cutoff_frequency":
					spec.cutoff, e = xFloat(v)
				case "_name":
					n.name = xText(v)
				default:
					return nil, pParsing("[common] query does not support [%s]", k).at(valueTok(t, k))
				}
				if e != nil {
					return nil, e
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
	n.spec = spec
	return n, nil
}

// query_string ---------------------------------------------------------------

type queryStringSpec struct {
	query                string
	defaultField         string
	hasDefaultField      bool
	fields               []fieldWeight
	hasFields            bool
	defaultOperator      string
	analyzer             string
	quoteAnalyzer        string
	allowLeadingWildcard bool
	fuzzyPrefixLength    int
	fuzzyMaxExpansions   int
	fuzzyRewrite         *string
	phraseSlop           int
	fuzziness            *fuzzinessSpec
	tieBreaker           *float64
	analyzeWildcard      bool
	rewrite              *string
	msm                  *string
	quoteFieldSuffix     string
	lenient              *bool
	timeZone             string
	transpositions       bool
	typ                  string
	escape               bool
}

func parseQueryString(body any) (*qnode, *Error) {
	const kind = "query_string"
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &queryStringSpec{defaultOperator: "or", allowLeadingWildcard: true, fuzzyMaxExpansions: 50,
		fuzziness: &fuzzinessSpec{auto: true, low: 3, high: 6}, transpositions: true, typ: "best_fields"}
	hasQuery := false
	for _, k := range queryKeys(m) {
		v := m[k]
		if list, ok := v.([]any); ok {
			if k != "fields" {
				return nil, pParsing("[%s] query does not support [%s]", kind, k).at(valueTok(m, k))
			}
			spec.hasFields = true
			for i, e := range list {
				if !isXValue(e) {
					return nil, noTextAt(e, elemTok(list, i))
				}
				fw, err := parseFieldAndWeight(xText(e))
				if err != nil {
					return nil, err
				}
				spec.fields = append(spec.fields, fw)
			}
			continue
		}
		if !isXValue(v) {
			return nil, unknownTokenError(kind, k, v).at(valueTok(m, k))
		}
		var err *Error
		switch k {
		case "query":
			spec.query, hasQuery = xText(v), true
		case "default_field":
			spec.defaultField, spec.hasDefaultField = xText(v), true
		case "default_operator":
			spec.defaultOperator, err = parseOperator(v)
		case "analyzer":
			spec.analyzer = xText(v)
		case "quote_analyzer":
			spec.quoteAnalyzer = xText(v)
		case "allow_leading_wildcard":
			spec.allowLeadingWildcard, err = xBool(v)
		case "max_determinized_states":
			_, err = xInt(v)
		case "enable_position_increments":
			_, err = xBool(v)
		case "escape":
			spec.escape, err = xBool(v)
		case "fuzzy_prefix_length":
			spec.fuzzyPrefixLength, err = xInt(v)
		case "fuzzy_max_expansions":
			spec.fuzzyMaxExpansions, err = xInt(v)
		case "fuzzy_rewrite":
			spec.fuzzyRewrite = msmText(v)
		case "phrase_slop":
			spec.phraseSlop, err = xInt(v)
		case "fuzziness":
			spec.fuzziness, err = parseFuzziness(kind, v, valueTok(m, k))
		case "boost":
			n.boost, err = xFloat(v)
		case "tie_breaker":
			var f float64
			f, err = xFloat(v)
			spec.tieBreaker = &f
		case "analyze_wildcard":
			spec.analyzeWildcard, err = xBool(v)
		case "rewrite":
			spec.rewrite = msmText(v)
		case "minimum_should_match":
			spec.msm = msmText(v)
		case "quote_field_suffix":
			spec.quoteFieldSuffix = xText(v)
		case "lenient":
			var b bool
			b, err = xBool(v)
			spec.lenient = &b
		case "time_zone":
			spec.timeZone = xText(v)
		case "_name":
			n.name = xText(v)
		case "auto_generate_synonyms_phrase_query":
			_, err = xBool(v)
		case "fuzzy_transpositions":
			spec.transpositions, err = xBool(v)
		case "type":
			spec.typ, err = parseMultiMatchType(xText(v))
		default:
			return nil, pParsing("[%s] query does not support [%s]", kind, k).at(valueTok(m, k))
		}
		if err != nil {
			return nil, err
		}
	}
	if !hasQuery {
		return nil, pParsing("[%s] must be provided with a [query]", kind).at(endTok(m))
	}
	if spec.timeZone != "" || m["time_zone"] != nil {
		if err := checkJavaZoneID(spec.timeZone); err != nil {
			// thrown by the builder outside of the parser: the Java
			// exception itself, reported with status 500
			cause := err.Cause
			return nil, parseFailure(&Error{Status: 500, Type: cause.Type, Reason: cause.Reason})
		}
	}
	if n.boost < 0 {
		return nil, negativeBoostError(kind, body)
	}
	n.spec = spec
	return n, nil
}

// simple_query_string --------------------------------------------------------

// simple_query_string flags (Lucene SimpleQueryParser).
const (
	sqsAnd        = 1 << 0
	sqsNot        = 1 << 1
	sqsOr         = 1 << 2
	sqsPrefix     = 1 << 3
	sqsPhrase     = 1 << 4
	sqsPrecedence = 1 << 5
	sqsEscape     = 1 << 6
	sqsWhitespace = 1 << 7
	sqsFuzzy      = 1 << 8
	sqsNear       = 1 << 9
	sqsAll        = -1
)

type simpleQueryStringSpec struct {
	query              string
	fields             []fieldWeight
	hasFields          bool
	analyzer           string
	defaultOperator    string
	flags              int
	lenient            *bool
	analyzeWildcard    bool
	msm                *string
	quoteFieldSuffix   string
	fuzzyPrefixLength  int
	fuzzyMaxExpansions int
	transpositions     bool
}

func resolveSQSFlags(s string) (int, *Error) {
	if s == "" {
		return sqsAll, nil
	}
	magic := 0
	for _, part := range strings.Split(s, "|") {
		if part == "" {
			continue
		}
		switch strings.ToUpper(part) {
		case "NONE":
			return 0, nil
		case "ALL":
			return sqsAll, nil
		case "AND":
			magic |= sqsAnd
		case "NOT":
			magic |= sqsNot
		case "OR":
			magic |= sqsOr
		case "PREFIX":
			magic |= sqsPrefix
		case "PHRASE":
			magic |= sqsPhrase
		case "PRECEDENCE":
			magic |= sqsPrecedence
		case "ESCAPE":
			magic |= sqsEscape
		case "WHITESPACE":
			magic |= sqsWhitespace
		case "FUZZY":
			magic |= sqsFuzzy
		case "NEAR", "SLOP":
			magic |= sqsNear
		default:
			return 0, pIllegalArgument("Unknown simple_query_string flag [%s]", part)
		}
	}
	return magic, nil
}

func parseSimpleQueryString(body any) (*qnode, *Error) {
	const kind = "simple_query_string"
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &simpleQueryStringSpec{defaultOperator: "or", flags: sqsAll, fuzzyMaxExpansions: 50, transpositions: true}
	hasQuery := false
	for _, k := range queryKeys(m) {
		v := m[k]
		if list, ok := v.([]any); ok {
			if k != "fields" {
				return nil, pParsing("[%s] query does not support [%s]", kind, k).at(valueTok(m, k))
			}
			spec.hasFields = true
			for i, e := range list {
				if !isXValue(e) {
					return nil, noTextAt(e, elemTok(list, i))
				}
				fw, err := parseFieldAndWeight(xText(e))
				if err != nil {
					return nil, err
				}
				spec.fields = append(spec.fields, fw)
			}
			continue
		}
		if !isXValue(v) {
			return nil, unknownTokenError(kind, k, v).at(valueTok(m, k))
		}
		var err *Error
		switch k {
		case "query":
			spec.query, hasQuery = xText(v), true
		case "boost":
			n.boost, err = xFloat(v)
		case "analyzer":
			spec.analyzer = xText(v)
		case "default_operator":
			spec.defaultOperator, err = parseOperator(v)
		case "flags":
			if _, isString := v.(string); isString {
				spec.flags, err = resolveSQSFlags(xText(v))
			} else {
				var f int
				f, err = xInt(v)
				if f < 0 {
					f = sqsAll
				}
				spec.flags = f
			}
		case "locale", "lowercase_expanded_terms", "all_fields":
		case "lenient":
			var b bool
			b, err = xBool(v)
			spec.lenient = &b
		case "analyze_wildcard":
			spec.analyzeWildcard, err = xBool(v)
		case "_name":
			n.name = xText(v)
		case "minimum_should_match":
			spec.msm = msmText(v)
		case "quote_field_suffix":
			spec.quoteFieldSuffix = xText(v)
		case "auto_generate_synonyms_phrase_query":
			_, err = xBool(v)
		case "fuzzy_prefix_length":
			spec.fuzzyPrefixLength, err = xInt(v)
		case "fuzzy_max_expansions":
			spec.fuzzyMaxExpansions, err = xInt(v)
		case "fuzzy_transpositions":
			spec.transpositions, err = xBool(v)
		default:
			return nil, pParsing("[%s] unsupported field [%s]", kind, k).at(valueTok(m, k))
		}
		if err != nil {
			return nil, err
		}
	}
	if !hasQuery {
		return nil, pParsing("[%s] query text missing", kind).at(endTok(m))
	}
	if n.boost < 0 {
		return nil, negativeBoostError(kind, body)
	}
	n.spec = spec
	return n, nil
}
