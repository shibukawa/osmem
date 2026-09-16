package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Mapping parameters of the field types: what each type accepts, how the
// values are validated and which parameters PUT _mapping may change
// (ParametrizedFieldMapper.Builder.getParameters of OpenSearch).

type paramKind int

const (
	pkBool paramKind = iota
	pkInt
	pkFloat
	pkString
	pkEnum
	pkMeta
	pkAnalyzer
	pkNormalizer
	pkSimilarity
	pkFormat
	pkNullValue
	pkAny
)

type paramSpec struct {
	name      string
	kind      paramKind
	updatable bool
	enum      []string
	def       string // the default as conflict messages print it
}

var (
	textIndexOptions    = []string{"positions", "docs", "freqs", "offsets"}
	keywordIndexOptions = []string{"docs", "freqs"}
	termVectorValues    = []string{"no", "yes", "with_positions", "with_offsets", "with_positions_offsets", "with_positions_payloads", "with_positions_offsets_payloads"}
)

func p(name string, kind paramKind, updatable bool, def string) paramSpec {
	return paramSpec{name: name, kind: kind, updatable: updatable, def: def}
}

func pEnum(name string, values []string, def string) paramSpec {
	return paramSpec{name: name, kind: pkEnum, enum: values, def: def}
}

var textParams = []paramSpec{
	p("index", pkBool, false, "true"), p("store", pkBool, false, "false"), pEnum("index_options", textIndexOptions, "positions"),
	p("norms", pkBool, false, "true"), pEnum("term_vector", termVectorValues, "no"), p("analyzer", pkAnalyzer, false, "default"),
	p("search_analyzer", pkAnalyzer, true, "default"), p("search_quote_analyzer", pkAnalyzer, true, "default"),
	p("similarity", pkSimilarity, false, "null"), p("position_increment_gap", pkInt, false, "-1"), p("fielddata", pkBool, true, "false"),
	p("fielddata_frequency_filter", pkAny, true, "null"), p("eager_global_ordinals", pkBool, true, "false"),
	p("index_phrases", pkBool, false, "false"), p("index_prefixes", pkAny, false, "null"), p("boost", pkFloat, true, "1.0"), p("meta", pkMeta, true, "{}"),
}

var numberParams = []paramSpec{
	p("index", pkBool, false, "true"), p("doc_values", pkBool, false, "true"), p("store", pkBool, false, "false"),
	p("ignore_malformed", pkBool, true, "false"), p("coerce", pkBool, true, "true"), p("null_value", pkNullValue, false, "null"),
	p("boost", pkFloat, true, "1.0"), p("meta", pkMeta, true, "{}"),
}

func withParams(base []paramSpec, extra ...paramSpec) []paramSpec {
	return append(append([]paramSpec(nil), base...), extra...)
}

var rangeParams = []paramSpec{
	p("index", pkBool, false, "true"), p("doc_values", pkBool, false, "true"), p("store", pkBool, false, "false"),
	p("coerce", pkBool, true, "true"), p("ignore_malformed", pkBool, true, "false"), p("boost", pkFloat, true, "1.0"), p("meta", pkMeta, true, "{}"),
}

var fieldParams = map[string][]paramSpec{
	TypeText:          textParams,
	TypeMatchOnlyText: textParams,
	TypeKeyword: {
		p("index", pkBool, false, "true"), p("doc_values", pkBool, false, "true"), p("store", pkBool, false, "false"),
		p("null_value", pkNullValue, false, "null"), p("eager_global_ordinals", pkBool, true, "false"), p("ignore_above", pkInt, true, "2147483647"),
		pEnum("index_options", keywordIndexOptions, "docs"), p("norms", pkBool, false, "false"), p("similarity", pkSimilarity, false, "null"),
		p("normalizer", pkNormalizer, false, "null"), p("split_queries_on_whitespace", pkBool, true, "false"), p("boost", pkFloat, true, "1.0"),
		p("meta", pkMeta, true, "{}"),
	},
	TypeWildcard: {
		p("ignore_above", pkInt, true, "2147483647"), p("normalizer", pkNormalizer, false, "null"), p("null_value", pkNullValue, false, "null"),
		p("doc_values", pkBool, false, "true"), p("meta", pkMeta, true, "{}"),
	},
	TypeLong:         numberParams,
	TypeInteger:      numberParams,
	TypeShort:        numberParams,
	TypeByte:         numberParams,
	TypeDouble:       numberParams,
	TypeFloat:        numberParams,
	TypeHalfFloat:    numberParams,
	TypeUnsignedLong: numberParams,
	TypeScaledFloat:  withParams(numberParams, p("scaling_factor", pkFloat, false, "null")),
	TypeTokenCount: {
		p("index", pkBool, false, "true"), p("doc_values", pkBool, false, "true"), p("store", pkBool, false, "false"),
		p("analyzer", pkAnalyzer, false, "null"), p("null_value", pkNullValue, false, "null"), p("enable_position_increments", pkBool, false, "true"),
		p("boost", pkFloat, true, "1.0"), p("meta", pkMeta, true, "{}"),
	},
	TypeBoolean: {
		p("meta", pkMeta, true, "{}"), p("boost", pkFloat, true, "1.0"), p("doc_values", pkBool, false, "true"),
		p("index", pkBool, false, "true"), p("null_value", pkNullValue, false, "null"), p("store", pkBool, false, "false"),
	},
	TypeDate:      dateParams,
	TypeDateNanos: dateParams,
	TypeIP: {
		p("index", pkBool, false, "true"), p("doc_values", pkBool, false, "true"), p("store", pkBool, false, "false"),
		p("ignore_malformed", pkBool, true, "false"), p("null_value", pkNullValue, false, "null"), p("boost", pkFloat, true, "1.0"),
		p("meta", pkMeta, true, "{}"),
	},
	TypeBinary: {p("store", pkBool, false, "false"), p("doc_values", pkBool, false, "false"), p("meta", pkMeta, true, "{}")},
	TypeSearchAsYouType: {
		p("index", pkBool, false, "true"), p("store", pkBool, false, "false"), p("doc_values", pkBool, false, "false"),
		p("max_shingle_size", pkInt, false, "3"), p("analyzer", pkAnalyzer, false, "default"), p("search_analyzer", pkAnalyzer, true, "default"),
		p("search_quote_analyzer", pkAnalyzer, true, "default"), pEnum("index_options", textIndexOptions, "positions"),
		p("norms", pkBool, false, "true"), pEnum("term_vector", termVectorValues, "no"), p("similarity", pkSimilarity, false, "null"),
		p("meta", pkMeta, true, "{}"),
	},
	TypeCompletion: {
		p("analyzer", pkAnalyzer, false, "simple"), p("search_analyzer", pkAnalyzer, true, "simple"), p("preserve_separators", pkBool, false, "true"),
		p("preserve_position_increments", pkBool, false, "true"), p("max_input_length", pkInt, true, "50"), p("contexts", pkAny, false, "null"),
		p("meta", pkMeta, true, "{}"),
	},
	TypeRankFeature:  {p("positive_score_impact", pkBool, false, "true"), p("meta", pkMeta, true, "{}")},
	TypeRankFeatures: {p("positive_score_impact", pkBool, false, "true"), p("meta", pkMeta, true, "{}")},
	TypeKNNVector: {
		p("dimension", pkInt, false, "null"), p("method", pkAny, false, "null"), p("model_id", pkString, false, "null"),
		p("data_type", pkString, false, "float"), p("mode", pkString, false, "null"), p("compression_level", pkString, false, "null"),
		p("space_type", pkString, false, "null"), p("doc_values", pkBool, false, "true"), p("meta", pkMeta, true, "{}"),
	},
	TypeVersion:      {p("index", pkBool, false, "true"), p("doc_values", pkBool, false, "true"), p("store", pkBool, false, "false"), p("meta", pkMeta, true, "{}")},
	TypeIntegerRange: rangeParams,
	TypeLongRange:    rangeParams,
	TypeFloatRange:   rangeParams,
	TypeDoubleRange:  rangeParams,
	TypeIPRange:      rangeParams,
	TypeDateRange:    withParams(rangeParams, p("format", pkFormat, false, DefaultDateFormat), p("locale", pkString, false, "")),
}

var dateParams = []paramSpec{
	p("index", pkBool, false, "true"), p("doc_values", pkBool, false, "true"), p("store", pkBool, false, "false"),
	p("format", pkFormat, false, DefaultDateFormat), p("print_format", pkFormat, false, "null"), p("locale", pkString, false, ""),
	p("null_value", pkNullValue, false, "null"), p("ignore_malformed", pkBool, true, "false"), p("boost", pkFloat, true, "1.0"),
	p("meta", pkMeta, true, "{}"),
}

func init() {
	fieldParams[TypeDate] = dateParams
	fieldParams[TypeDateNanos] = dateParams
}

// oldStyleParams are the parameters of the mappers that report unknown
// parameters as "Mapping definition for [f] has unsupported parameters".
var oldStyleParams = map[string]map[string]bool{
	TypeObject:          set("properties", "dynamic", "enabled"),
	TypeNested:          set("properties", "dynamic", "enabled", "include_in_parent", "include_in_root"),
	TypeGeoPoint:        set("ignore_malformed", "ignore_z_value", "null_value", "index", "doc_values", "store", "copy_to", "fields", "meta", "boost"),
	TypeGeoShape:        set("orientation", "strategy", "tree", "precision", "tree_levels", "distance_error_pct", "points_only", "ignore_malformed", "ignore_z_value", "coerce", "doc_values", "index", "store", "meta"),
	TypeXYPoint:         set("ignore_malformed", "ignore_z_value", "null_value", "index", "doc_values", "store", "meta"),
	TypeXYShape:         set("orientation", "ignore_malformed", "ignore_z_value", "coerce", "doc_values", "index", "store", "meta"),
	TypeConstantKeyword: set("value", "meta"),
	TypeFlatObject:      set("index", "doc_values", "store", "depth_limit", "ignore_above", "null_value", "similarity", "normalizer", "meta"),
	TypeAlias:           set("path"),
	TypePercolator:      set(),
	TypeJoin:            set("relations", "eager_global_ordinals"),
}

func set(keys ...string) map[string]bool {
	out := map[string]bool{}
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// value conversions ---------------------------------------------------------

// javaClassName is the class of a parsed JSON value in messages.
func javaClassName(v any) string {
	switch t := v.(type) {
	case string:
		return "java.lang.String"
	case bool:
		return "java.lang.Boolean"
	case json.Number:
		if isIntToken(t.String()) {
			if n, err := strconv.ParseInt(t.String(), 10, 64); err == nil && n >= math.MinInt32 && n <= math.MaxInt32 {
				return "java.lang.Integer"
			}
			return "java.lang.Long"
		}
		return "java.lang.Double"
	case []any:
		return "java.util.ArrayList"
	case M:
		return "java.util.HashMap"
	}
	return "java.lang.Object"
}

func javaSimpleClassName(v any) string {
	name := javaClassName(v)
	return name[strings.LastIndexByte(name, '.')+1:]
}

// strictBool is Booleans.parseBoolean of a mapping parameter.
func strictBool(v any) (bool, *Error) {
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
	}
	return false, errIllegalArgument("Failed to parse value [%s] as only [true] or [false] are allowed.", javaValueString(v))
}

// convertBool is XContentMapValues.nodeBooleanValue(node, name).
func convertBool(v any, name string) (bool, *Error) {
	b, err := strictBool(v)
	if err != nil {
		return false, &Error{Status: 400, Type: "illegal_argument_exception", Reason: "Could not convert [" + name + "] to boolean", Cause: err}
	}
	return b, nil
}

// nodeInt is XContentMapValues.nodeIntegerValue.
func nodeInt(v any) (int64, *Error) {
	switch t := v.(type) {
	case json.Number:
		if n, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return int64(int32(n)), nil
		}
		f, err := strconv.ParseFloat(t.String(), 64)
		if err != nil {
			return 0, numberFormatError(t.String())
		}
		return int64(int32(f)), nil
	case float64:
		return int64(int32(t)), nil
	}
	s := javaValueString(v)
	if !javaLongTokenRe.MatchString(s) {
		return 0, numberFormatError(s)
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, numberFormatError(s)
	}
	return n, nil
}

// nodeDouble is XContentMapValues.nodeDoubleValue.
func nodeDouble(v any) (float64, *Error) {
	switch t := v.(type) {
	case json.Number:
		f, _ := strconv.ParseFloat(t.String(), 64)
		return f, nil
	case float64:
		return t, nil
	}
	return javaParseDouble(javaValueString(v), 64)
}

func paramDisplay(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case float64:
		return javaNumberString(t, 64)
	case Double:
		return javaNumberString(float64(t), 64)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return javaValueString(v)
}

var knownSimilarities = set("BM25", "boolean", "classic", "LMDirichlet", "LMJelinekMercer", "DFR", "IB", "DFI")

// validateMeta checks the meta parameter.
func validateMeta(name string, v any) *Error {
	m, ok := v.(M)
	if !ok {
		return errMapperParsing("[meta] must be an object, got %s[%s] for field [%s]", javaSimpleClassName(v), javaValueString(v), name)
	}
	// OpenSearch 3.8 only requires string values (no key or value length
	// limits, unlike Elasticsearch)
	for _, k := range javaHashMapOrder(sortedMapKeys(m)) {
		if _, isString := m[k].(string); !isString {
			return errMapperParsing("[meta] values can only be strings, but got %s[%s] for field [%s]", javaSimpleClassName(m[k]), javaValueString(m[k]), name)
		}
	}
	return nil
}

func remainingFields(spec M, keys []string) string {
	var b strings.Builder
	for _, k := range javaHashMapOrder(keys) {
		fmt.Fprintf(&b, " [%s : %s]", k, javaValueString(spec[k]))
	}
	return b.String()
}
