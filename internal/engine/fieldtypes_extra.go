package engine

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/blevesearch/bleve/v2/search/query"
)

// field name length limit --------------------------------------------------------

// metadataMapperNames are the metadata field mappers of an OpenSearch 3.8
// mapping in registration order.
var metadataMapperNames = []string{"_ignored", "_id", "_routing", "_index", "_data_stream_timestamp", "_source",
	"_nested_path", "_version", "_seq_no", "_doc_count", "_feature", "_field_names"}

// fieldNameOverLimit returns the first mapper name longer than the limit in
// the order of MappingLookup.checkFieldNameLengthLimit: the object mappers,
// then the field mappers (metadata fields, fields, multi-fields and field
// aliases), each in java.util.HashMap order of their full paths.
func fieldNameOverLimit(fields map[string]*Field, limit int) string {
	simple := map[string]string{}
	var objects []string
	leaves := append([]string(nil), metadataMapperNames...)
	for _, name := range leaves {
		simple[name] = name
	}
	var walk func(prefix string, fields map[string]*Field)
	walk = func(prefix string, fields map[string]*Field) {
		for _, name := range sortedFieldNames(fields) {
			f := fields[name]
			full := prefix + name
			simple[full] = name
			if f.Type == TypeObject || f.Type == TypeNested {
				objects = append(objects, full)
				walk(full+".", f.Properties)
				continue
			}
			leaves = append(leaves, full)
			for _, sub := range sortedFieldNames(f.Fields) {
				simple[full+"."+sub] = sub
				leaves = append(leaves, full+"."+sub)
			}
		}
	}
	walk("", fields)
	for _, group := range [][]string{objects, leaves} {
		for _, path := range javaHashMapOrder(group) {
			if utf16Length(simple[path]) > limit {
				return simple[path]
			}
		}
	}
	return ""
}

// similarity ----------------------------------------------------------------------

// booleanSimilarity reports whether a field scores with the boolean
// similarity (named directly, through a custom similarity of that type or as
// the index default): every matching term scores its boost.
func (ix *Index) booleanSimilarity(f *Field) bool {
	if f == nil {
		return false
	}
	name := getString(f.Extra, "similarity")
	if name == "" {
		name = "default"
	}
	if name == "boolean" {
		return true
	}
	return getString(getMap(getMap(getMap(ix.Settings, "index"), "similarity"), name), "type") == "boolean"
}

// similarityLeaf scores a term query of a field: 1 per match with the
// boolean similarity and on the norm-less prefix field of search_as_you_type
// fields (as keyword fields).
func (qb *queryBuilder) similarityLeaf(f *Field, q query.Query) query.Query {
	if _, none := q.(*query.MatchNoneQuery); !none && (qb.ix.booleanSimilarity(f) || f != nil && f.saytPrefix) {
		return &constantScoreQuery{inner: q, score: 1}
	}
	return scoreLeaf(f, q)
}

// similarityTerms makes the terms of a fuzzy query score their boosts with
// the boolean similarity.
func (qb *queryBuilder) similarityTerms(f *Field, q query.Query) query.Query {
	if tu, ok := q.(*termsUnionQuery); ok && qb.ix.booleanSimilarity(f) {
		tu.booleanSim = true
	}
	return q
}

// routing -------------------------------------------------------------------------

// routingRequired reports whether the mapping of an index requires routing.
func routingRequired(ix *Index) bool {
	return getBool(getMap(ix.Mapping.Extra, "_routing"), "required", false)
}

// errIDMustNotBeNull fails an index request without id and routing on an
// index that requires routing: RoutingMissingException rejects the null id
// before an id is generated, and the whole request fails.
func errIDMustNotBeNull() *Error {
	return &Error{Status: 500, Type: "null_pointer_exception", Reason: "id must not be null"}
}

// exact integral values -------------------------------------------------------------

// exactInt is a value of an integral field beyond the integers a double
// holds exactly, with the digits the field keeps.
type exactInt struct {
	n     float64
	exact string
}

// exactIntegralValue converts a value of an integral field like convertValue
// and keeps the exact digits of values beyond 2^53.
func exactIntegralValue(f *Field, v any) (any, bool) {
	cv, ok := convertValue(f, v)
	n, isNumber := cv.(float64)
	if !ok || !isNumber || math.Abs(n) < 1<<53 {
		return cv, ok
	}
	dv, _, err := parseNumericField(f, v, true)
	if err != nil || dv.null || dv.exact == "" {
		return cv, ok
	}
	return exactInt{n: n, exact: dv.exact}, true
}

// output renders the value as a long (an unsigned long for unsigned_long).
func (e exactInt) output(f *Field) any {
	if e.exact != "" {
		if f != nil && f.Type == TypeUnsignedLong {
			if u, err := strconv.ParseUint(e.exact, 10, 64); err == nil {
				return u
			}
		} else if i, err := strconv.ParseInt(e.exact, 10, 64); err == nil {
			return i
		}
	}
	return numericOutput(f, e.n)
}

// formatted renders the exact digits with an integer DecimalFormat pattern
// (only '#' and '0'), which formats a long without rounding.
func (e exactInt) formatted(pattern string) (string, bool) {
	if e.exact == "" || pattern == "" || strings.Trim(pattern, "#0") != "" {
		return "", false
	}
	digits, sign := e.exact, ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	if zeros := strings.Count(pattern, "0"); len(digits) < zeros {
		digits = strings.Repeat("0", zeros-len(digits)) + digits
	}
	return sign + digits, true
}

// bytesRefText is BytesRef.toString: the hexadecimal bytes of a term.
func bytesRefText(s string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < len(s); i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.FormatUint(uint64(s[i]), 16))
	}
	b.WriteByte(']')
	return b.String()
}

// less orders values like sort.Float64s, the exact digits breaking ties.
func (e exactInt) less(o exactInt) bool {
	if e.n == o.n && e.exact != "" && o.exact != "" {
		a, okA := new(big.Int).SetString(e.exact, 10)
		b, okB := new(big.Int).SetString(o.exact, 10)
		if okA && okB {
			return a.Cmp(b) < 0
		}
	}
	return e.n < o.n || (math.IsNaN(e.n) && !math.IsNaN(o.n))
}
