package engine

import (
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Lucene query descriptions: Query.toString() of the queries OpenSearch
// 3.8 creates from the query DSL, as _validate/query reports them in its
// explanations. Queries osmem does not model exactly are described by
// their closest form.

const matchNoneUserRequested = `MatchNoDocsQuery("User requested "match_none" query.")`

type luceneDescriber struct {
	qb *queryBuilder
	// rewrite describes the rewritten query (IndexSearcher.rewrite).
	rewrite bool
	// plain describes the queries approximations stand for (the queries
	// explanations and profiles report).
	plain bool
}

// describeQuery describes a parsed query at the top level.
func (d *luceneDescriber) describeQuery(n *qnode) string {
	s, _ := d.describe(n, 0)
	return s
}

// describe returns the description of a query and whether it is a
// BooleanQuery (a boolean clause wraps those in parentheses).
func (d *luceneDescriber) describe(n *qnode, depth int) (string, bool) {
	s, isBool := d.describeSpec(n, depth)
	if n.boost != 1 && !strings.HasPrefix(s, "MatchNoDocsQuery(") {
		if d.rewrite && n.kind == "match_all" {
			s = "*:*"
		}
		return "(" + s + ")^" + javaNumberString(float64(float32(n.boost)), 32), false
	}
	return s, isBool
}

// approximate wraps a query OpenSearch approximates (match_all, point
// ranges); the rewritten query keeps the wrapper at the top level only.
func (d *luceneDescriber) approximate(original, approximation string, depth int) string {
	if d.plain || (d.rewrite && depth > 0) {
		return original
	}
	return "ApproximateScoreQuery(originalQuery=" + original + ", approximationQuery=Approximate(" + approximation + "))"
}

func (d *luceneDescriber) describeSpec(n *qnode, depth int) (string, bool) {
	switch spec := n.spec.(type) {
	case *termSpec:
		return d.term(spec.field, spec.value, depth), false
	case *termsSpec:
		return d.terms(spec), false
	case *rangeSpec:
		return d.rangeQuery(spec, depth), false
	case *matchSpec:
		return d.match(n.kind, spec)
	case *phraseSpec:
		return d.phrase(spec), false
	case *multiTermSpec:
		return d.multiTerm(n.kind, spec), false
	case *fuzzySpec:
		return d.fuzzy(spec), false
	case *idsSpec:
		return idsDescription(spec.values), false
	case *existsSpec:
		if _, _, ok := d.qb.ix.Mapping.resolve(spec.field); !ok {
			return matchNoneUserRequested, false
		}
		return "ConstantScore(FieldExistsQuery [field=" + spec.field + "])", false
	case *boolSpec:
		return d.boolQuery(spec, depth)
	case *constantScoreSpec:
		inner, _ := d.describe(spec.filter, depth+1)
		if d.rewrite && strings.HasPrefix(inner, "ConstantScore(") {
			return inner, false
		}
		return "ConstantScore(" + inner + ")", false
	case *disMaxSpec:
		parts := make([]string, 0, len(spec.queries))
		for _, q := range spec.queries {
			s, _ := d.describe(q, depth+1)
			parts = append(parts, s)
		}
		out := "(" + strings.Join(parts, " | ") + ")"
		if spec.tie != 0 {
			out += "~" + javaNumberString(float64(float32(spec.tie)), 32)
		}
		return out, false
	case *multiMatchSpec:
		return d.multiMatch(spec, depth)
	case *queryStringSpec:
		return d.queryString(spec, depth)
	case *boostingSpec:
		pos, _ := d.describe(spec.positive, depth+1)
		neg, _ := d.describe(spec.negative, depth+1)
		return "FunctionScoreQuery(" + pos + ", scored by boost(queryboost(score(" + neg + "))^" + javaNumberString(float64(float32(spec.negativeBoost)), 32) + "))", false
	case *functionScoreSpec:
		inner := d.approximate("*:*", "*:*", depth+1)
		if spec.query != nil {
			inner, _ = d.describe(spec.query, depth+1)
		}
		return "function score (" + inner + ", functions: [])", false
	case *nestedSpec:
		inner, _ := d.describe(spec.query, depth+1)
		return "ToParentBlockJoinQuery (" + inner + ")", false
	}
	switch n.kind {
	case "match_all":
		return d.approximate("*:*", "*:*", depth), false
	case "match_none":
		return matchNoneUserRequested, false
	}
	return n.kind, false
}

// term queries ---------------------------------------------------------------

func (d *luceneDescriber) term(field string, value any, depth int) string {
	switch field {
	case "_id":
		return idsDescription([]string{xText(value)})
	case "_index":
		return "*:*"
	}
	f, _, ok := d.qb.ix.Mapping.resolve(field)
	if !ok {
		return matchNoneUserRequested
	}
	return d.fieldTerm(field, f, value, depth)
}

// fieldTerm is MappedFieldType.termQuery for a mapped field.
func (d *luceneDescriber) fieldTerm(field string, f *Field, value any, depth int) string {
	text := xText(value)
	switch {
	case f.Type == TypeKeyword || f.Type == TypeConstantKeyword || f.Type == TypeWildcard:
		return "ConstantScore(" + field + ":" + d.qb.normalizeForField(f, text) + ")"
	case f.Type == TypeBoolean:
		if text == "true" {
			return field + ":T"
		}
		return field + ":F"
	case f.isDate():
		lo, hi, ok := d.dateBounds(f, value, value, true, true)
		if !ok {
			return field + ":" + text
		}
		return d.approximate(indexOrDocValues(field, lo, hi, lo, hi), field+":["+lo+" TO "+hi+"]", depth)
	case f.isNumeric():
		n, ok := toFloat(value)
		if !ok {
			return field + ":" + text
		}
		idx, dv := numericPointText(f, n)
		return indexOrDocValues(field, idx, idx, dv, dv)
	case f.Type == TypeIP:
		ip := net.ParseIP(strings.TrimSpace(text))
		if ip == nil {
			return field + ":" + text
		}
		addr, bytes := ipText(ip)
		return "IndexOrDocValuesQuery(indexQuery=" + field + ":[" + addr + " TO " + addr + "], dvQuery=" + field + ":[" + bytes + " TO " + bytes + "])"
	}
	return field + ":" + text
}

func indexOrDocValues(field, lo, hi, dvLo, dvHi string) string {
	return "IndexOrDocValuesQuery(indexQuery=" + field + ":[" + lo + " TO " + hi + "], dvQuery=" + field + ":[" + dvLo + " TO " + dvHi + "])"
}

// numericPointText renders a numeric point value as the points query and
// the doc values query of the field show it.
func numericPointText(f *Field, v float64) (string, string) {
	switch f.Type {
	case TypeFloat:
		fv := float32(v)
		return javaNumberString(float64(fv), 32), strconv.FormatInt(int64(sortableFloatBits(fv)), 10)
	case TypeDouble:
		return javaNumberString(v, 64), strconv.FormatInt(sortableDoubleBits(v), 10)
	case TypeHalfFloat:
		bits := halfFloatToShortBits(float32(v))
		return javaNumberString(float64(shortBitsToHalfFloat(bits)), 32), strconv.Itoa(int(int16(bits)))
	case TypeScaledFloat:
		s := strconv.FormatInt(int64(math.Floor(v*scalingFactor(f)+0.5)), 10)
		return s, s
	}
	s := strconv.FormatInt(int64(v), 10)
	return s, s
}

func sortableFloatBits(f float32) int32 {
	bits := int32(math.Float32bits(f))
	return bits ^ ((bits >> 31) & 0x7fffffff)
}

func sortableDoubleBits(v float64) int64 {
	bits := int64(math.Float64bits(v))
	return bits ^ ((bits >> 63) & 0x7fffffffffffffff)
}

// ipText renders an address as InetAddressPoint and the doc values query
// show it: the address and its 16 bytes.
func ipText(ip net.IP) (string, string) {
	addr := ip.String()
	b := ip.To16()
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = strconv.FormatInt(int64(c), 16)
	}
	return addr, "[" + strings.Join(parts, " ") + "]"
}

// idsDescription is the TermInSetQuery of an ids query.
func idsDescription(values []string) string {
	if len(values) == 0 {
		return `MatchNoDocsQuery("Missing ids in "ids" query.")`
	}
	seen := map[string]bool{}
	var encoded []string
	for _, v := range values {
		e := string(encodeDocID(v))
		if !seen[e] {
			seen[e] = true
			encoded = append(encoded, e)
		}
	}
	sort.Strings(encoded)
	parts := make([]string, 0, len(encoded))
	for _, e := range encoded {
		parts = append(parts, luceneTermText([]byte(e)))
	}
	return "_id:(" + strings.Join(parts, " ") + ")"
}

// luceneTermText is Term.toString(BytesRef): the text when the bytes are
// UTF-8, the hex bytes otherwise.
func luceneTermText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = strconv.FormatInt(int64(c), 16)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func (d *luceneDescriber) terms(spec *termsSpec) string {
	field := spec.field
	if field == "_id" {
		values := make([]string, 0, len(spec.values))
		for _, v := range spec.values {
			values = append(values, xText(v))
		}
		return idsDescription(values)
	}
	f, _, ok := d.qb.ix.Mapping.resolve(field)
	if !ok {
		return matchNoneUserRequested
	}
	if f.isNumeric() && f.Type != TypeScaledFloat {
		var points []float64
		var dv []string
		for _, v := range spec.values {
			n, ok := toFloat(v)
			if !ok {
				continue
			}
			points = append(points, n)
			_, dvText := numericPointText(f, n)
			dv = append(dv, dvText)
		}
		sort.Float64s(points)
		idx := make([]string, 0, len(points))
		for i, p := range points {
			if i > 0 && p == points[i-1] {
				continue
			}
			text, _ := numericPointText(f, p)
			idx = append(idx, text)
		}
		return "IndexOrDocValuesQuery(indexQuery=" + field + ":{" + strings.Join(idx, " ") + "}, dvQuery=" + field + ": [" + strings.Join(dv, ", ") + "])"
	}
	seen := map[string]bool{}
	var values []string
	for _, v := range spec.values {
		text := xText(v)
		if f.Type == TypeKeyword {
			text = d.qb.normalizeForField(f, text)
		}
		if !seen[text] {
			seen[text] = true
			values = append(values, text)
		}
	}
	sort.Strings(values)
	return field + ":(" + strings.Join(values, " ") + ")"
}

// ranges -------------------------------------------------------------------------

func describeRangeBounds(params M) (lower, upper any, includeLower, includeUpper bool) {
	includeLower, includeUpper = true, true
	for _, k := range []string{"from", "to", "include_lower", "include_upper", "gte", "gt", "lte", "lt"} {
		v, ok := params[k]
		if !ok {
			continue
		}
		switch k {
		case "from":
			lower = v
		case "to":
			upper = v
		case "include_lower":
			includeLower = getBool(params, k, true)
		case "include_upper":
			includeUpper = getBool(params, k, true)
		case "gte":
			lower, includeLower = v, true
		case "gt":
			lower, includeLower = v, false
		case "lte":
			upper, includeUpper = v, true
		case "lt":
			upper, includeUpper = v, false
		}
	}
	return lower, upper, includeLower, includeUpper
}

func (d *luceneDescriber) rangeQuery(spec *rangeSpec, depth int) string {
	field := spec.field
	f, _, ok := d.qb.ix.Mapping.resolve(field)
	if !ok {
		return matchNoneUserRequested
	}
	lower, upper, incLower, incUpper := describeRangeBounds(spec.params)
	switch {
	case f.isDate():
		lo, hi, ok := d.dateBounds(f, lower, upper, incLower, incUpper)
		if !ok {
			break
		}
		return d.approximate(indexOrDocValues(field, lo, hi, lo, hi), field+":["+lo+" TO "+hi+"]", depth)
	case f.Type == TypeFloat || f.Type == TypeHalfFloat:
		lo, hi := float32(math.Inf(-1)), float32(math.Inf(1))
		if n, ok := toFloat(lower); ok {
			lo = float32(n)
			if !incLower {
				lo = math.Nextafter32(lo, float32(math.Inf(1)))
			}
		}
		if n, ok := toFloat(upper); ok {
			hi = float32(n)
			if !incUpper {
				hi = math.Nextafter32(hi, float32(math.Inf(-1)))
			}
		}
		idxLo, dvLo := numericPointText(f, float64(lo))
		idxHi, dvHi := numericPointText(f, float64(hi))
		return d.approximate(indexOrDocValues(field, idxLo, idxHi, dvLo, dvHi), field+":["+idxLo+" TO "+idxHi+"]", depth)
	case f.Type == TypeDouble:
		lo, hi := math.Inf(-1), math.Inf(1)
		if n, ok := toFloat(lower); ok {
			lo = n
			if !incLower {
				lo = math.Nextafter(lo, math.Inf(1))
			}
		}
		if n, ok := toFloat(upper); ok {
			hi = n
			if !incUpper {
				hi = math.Nextafter(hi, math.Inf(-1))
			}
		}
		idxLo, dvLo := numericPointText(f, lo)
		idxHi, dvHi := numericPointText(f, hi)
		return d.approximate(indexOrDocValues(field, idxLo, idxHi, dvLo, dvHi), field+":["+idxLo+" TO "+idxHi+"]", depth)
	case f.isNumeric():
		minV, maxV := int64(math.MinInt64), int64(math.MaxInt64)
		switch f.Type {
		case TypeInteger, TypeShort, TypeByte, TypeTokenCount:
			minV, maxV = math.MinInt32, math.MaxInt32
		}
		lo, hi := minV, maxV
		if n, ok := toFloat(lower); ok {
			if f.Type == TypeScaledFloat {
				n *= scalingFactor(f)
				if !incLower {
					n = math.Nextafter(n, math.Inf(1))
				}
				lo = int64(math.Ceil(n))
			} else {
				lo = int64(n)
				hasDecimal := n != math.Trunc(n)
				if (!hasDecimal && !incLower) || (hasDecimal && n > 0) {
					lo++
				}
			}
		}
		if n, ok := toFloat(upper); ok {
			if f.Type == TypeScaledFloat {
				n *= scalingFactor(f)
				if !incUpper {
					n = math.Nextafter(n, math.Inf(-1))
				}
				hi = int64(math.Floor(n))
			} else {
				hi = int64(n)
				hasDecimal := n != math.Trunc(n)
				if (!hasDecimal && !incUpper) || (hasDecimal && n < 0) {
					hi--
				}
			}
		}
		l, h := strconv.FormatInt(lo, 10), strconv.FormatInt(hi, 10)
		return d.approximate(indexOrDocValues(field, l, h, l, h), field+":["+l+" TO "+h+"]", depth)
	}
	// terms: TermRangeQuery
	bound := func(v any) string {
		if v == nil {
			return "*"
		}
		return xText(v)
	}
	text := field + ":"
	if incLower {
		text += "["
	} else {
		text += "{"
	}
	text += bound(lower) + " TO " + bound(upper)
	if incUpper {
		text += "]"
	} else {
		text += "}"
	}
	if f.Type == TypeText || f.Type == TypeMatchOnlyText {
		return text
	}
	dv := text
	if d.rewrite {
		dv = "ConstantScore(" + text + ")"
	}
	return "IndexOrDocValuesQuery(indexQuery=" + text + ", dvQuery=" + dv + ")"
}

// dateBounds resolves the bounds of a date range to epoch milliseconds
// (DateFieldType.rangeQuery).
func (d *luceneDescriber) dateBounds(f *Field, lower, upper any, incLower, incUpper bool) (string, string, bool) {
	lo, hi := int64(math.MinInt64), int64(math.MaxInt64)
	parse := func(v any, roundUp bool) (int64, bool) {
		if n, ok := v.(float64); ok {
			return int64(n), true
		}
		t, err := ParseDateMath(xText(v), f.Format, d.qb.c.now(), time.UTC, roundUp)
		if err != nil {
			return 0, false
		}
		return t.UnixMilli(), true
	}
	if lower != nil {
		n, ok := parse(lower, !incLower)
		if !ok {
			return "", "", false
		}
		lo = n
		if !incLower {
			lo++
		}
	}
	if upper != nil {
		n, ok := parse(upper, incUpper)
		if !ok {
			return "", "", false
		}
		hi = n
		if !incUpper {
			hi--
		}
	}
	return strconv.FormatInt(lo, 10), strconv.FormatInt(hi, 10), true
}

// full text queries --------------------------------------------------------------

// analyzedTerms returns the search terms of text for a text field.
func (d *luceneDescriber) analyzedTerms(kind string, f *Field, text, analyzer string, quoted bool) []string {
	an, _, err := d.qb.searchAnalyzer(kind, f, analyzer, quoted)
	if err != nil || an == nil {
		return []string{text}
	}
	return tokens(an, text)
}

func isTextType(f *Field) bool {
	return f.Type == TypeText || f.Type == TypeMatchOnlyText || f.Type == TypeSearchAsYouType
}

func (d *luceneDescriber) match(kind string, spec *matchSpec) (string, bool) {
	f, _, ok := d.qb.ix.Mapping.resolve(spec.field)
	if !ok {
		return `MatchNoDocsQuery("unmapped fields [` + spec.field + `]")`, false
	}
	if !isTextType(f) {
		return d.fieldTerm(spec.field, f, spec.query, 1), false
	}
	terms := d.analyzedTerms(kind, f, xText(spec.query), spec.analyzer, false)
	if len(terms) == 0 {
		return `MatchNoDocsQuery("Matching no documents because no terms present")`, false
	}
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = spec.field + ":" + t
		if kind == "match_bool_prefix" && i == len(terms)-1 {
			parts[i] += "*"
		}
	}
	if len(parts) == 1 {
		return parts[0], false
	}
	if spec.operator == "and" {
		for i := range parts {
			parts[i] = "+" + parts[i]
		}
	}
	out := strings.Join(parts, " ")
	if spec.msm != nil && spec.operator != "and" {
		if n, err := calcMinShouldMatch(len(parts), *spec.msm); err == nil && n > 0 {
			out = "(" + out + ")~" + strconv.Itoa(n)
		}
	}
	return out, true
}

func (d *luceneDescriber) phrase(spec *phraseSpec) string {
	f, _, ok := d.qb.ix.Mapping.resolve(spec.field)
	if !ok {
		return `MatchNoDocsQuery("unmapped fields [` + spec.field + `]")`
	}
	if !isTextType(f) {
		return d.fieldTerm(spec.field, f, spec.query, 1)
	}
	terms := d.analyzedTerms("match_phrase", f, xText(spec.query), spec.analyzer, true)
	if len(terms) == 0 {
		return `MatchNoDocsQuery("Matching no documents because no terms present")`
	}
	if spec.prefix {
		terms[len(terms)-1] += "*"
	} else if len(terms) == 1 {
		return spec.field + ":" + terms[0]
	}
	out := spec.field + ":\"" + strings.Join(terms, " ") + "\""
	if spec.slop != 0 {
		out += "~" + strconv.Itoa(spec.slop)
	}
	return out
}

func (d *luceneDescriber) multiTerm(kind string, spec *multiTermSpec) string {
	f, _, ok := d.qb.ix.Mapping.resolve(spec.field)
	if !ok {
		return matchNoneUserRequested
	}
	value := spec.value
	if f.Type == TypeKeyword {
		value = d.qb.normalizeForField(f, value)
	}
	switch kind {
	case "prefix":
		return spec.field + ":" + value + "*"
	case "regexp":
		return spec.field + ":/" + value + "/"
	}
	return spec.field + ":" + value
}

func (d *luceneDescriber) fuzzy(spec *fuzzySpec) string {
	if _, _, ok := d.qb.ix.Mapping.resolve(spec.field); !ok {
		return matchNoneUserRequested
	}
	text := xText(spec.value)
	edits := 1
	if spec.fuzziness != nil {
		edits = spec.fuzziness.distance(text)
	} else {
		edits = (&fuzzinessSpec{auto: true, low: 3, high: 6}).distance(text)
	}
	return spec.field + ":" + text + "~" + strconv.Itoa(edits)
}

func (d *luceneDescriber) multiMatch(spec *multiMatchSpec, depth int) (string, bool) {
	fields := make([]string, 0, len(spec.fields))
	boosts := map[string]float64{}
	for _, fw := range spec.fields {
		if _, _, ok := d.qb.ix.Mapping.resolve(fw.field); !ok {
			continue
		}
		if _, dup := boosts[fw.field]; !dup {
			fields = append(fields, fw.field)
		}
		boosts[fw.field] = fw.boost
	}
	if len(fields) == 0 {
		return `MatchNoDocsQuery("unmapped fields")`, false
	}
	ordered := javaHashMapOrder(fields)
	parts := make([]string, 0, len(ordered))
	for _, field := range ordered {
		n := &qnode{kind: "match", boost: 1, spec: &matchSpec{field: field, query: spec.query, analyzer: spec.analyzer, operator: spec.operator, msm: spec.msm}}
		if b := boosts[field]; b != 0 && b != 1 {
			n.boost = b
		}
		s, isBool := d.describe(n, depth+1)
		if isBool {
			s = "(" + s + ")"
		}
		parts = append(parts, s)
	}
	if len(parts) == 1 {
		return parts[0], false
	}
	switch spec.typ {
	case "most_fields":
		if d.rewrite {
			return strings.Join(parts, " "), true
		}
		return "(" + strings.Join(parts, " | ") + ")~1.0", false
	}
	out := "(" + strings.Join(parts, " | ") + ")"
	if spec.tieBreaker != nil && *spec.tieBreaker != 0 {
		out += "~" + javaNumberString(float64(float32(*spec.tieBreaker)), 32)
	}
	return out, false
}

// queryString describes simple query strings: field:value clauses joined
// by the default operator or AND/OR.
func (d *luceneDescriber) queryString(spec *queryStringSpec, depth int) (string, bool) {
	words := strings.Fields(spec.query)
	type clause struct {
		occur string
		text  string
	}
	var clauses []clause
	and := spec.defaultOperator == "and"
	pendingAnd := false
	for _, w := range words {
		switch w {
		case "AND", "&&":
			pendingAnd = true
			if len(clauses) > 0 {
				clauses[len(clauses)-1].occur = "+"
			}
			continue
		case "OR", "||":
			continue
		}
		if strings.ContainsAny(w, "()\"[]{}*?~^/\\") {
			return spec.query, false
		}
		field, value := spec.defaultField, w
		if i := strings.Index(w, ":"); i > 0 {
			field, value = w[:i], w[i+1:]
		}
		if field == "" || field == "*" {
			return spec.query, false
		}
		f, _, ok := d.qb.ix.Mapping.resolve(field)
		if !ok {
			return spec.query, false
		}
		var desc string
		switch {
		case isTextType(f):
			terms := d.analyzedTerms("query_string", f, value, spec.analyzer, false)
			if len(terms) != 1 {
				return spec.query, false
			}
			desc = field + ":" + terms[0]
		case f.isNumeric():
			if _, ok := javaDoubleOK(value); !ok {
				desc = `MatchNoDocsQuery("failed [` + field + `] query, caused by number_format_exception:[For input string: "` + value + `"]")`
				break
			}
			desc = d.fieldTerm(field, f, value, depth+1)
		default:
			desc = d.fieldTerm(field, f, value, depth+1)
		}
		occur := ""
		if and || pendingAnd {
			occur = "+"
		}
		pendingAnd = false
		clauses = append(clauses, clause{occur: occur, text: desc})
	}
	if len(clauses) == 0 {
		return `MatchNoDocsQuery("Matching no documents because no terms present")`, false
	}
	if len(clauses) == 1 {
		return clauses[0].text, false
	}
	parts := make([]string, len(clauses))
	for i, c := range clauses {
		parts[i] = c.occur + c.text
	}
	return strings.Join(parts, " "), true
}

// boolean queries ----------------------------------------------------------------

func (d *luceneDescriber) boolQuery(spec *boolSpec, depth int) (string, bool) {
	if len(spec.must)+len(spec.filter)+len(spec.should)+len(spec.mustNot) == 0 {
		return d.approximate("*:*", "*:*", depth), false
	}
	type clause struct {
		occur string
		text  string
		bool  bool
	}
	var clauses []clause
	add := func(occur string, list []*qnode) {
		for _, q := range list {
			s, isBool := d.describe(q, depth+1)
			if d.rewrite && (occur == "#" || occur == "-") {
				// scores of filter and prohibited clauses are not needed
				for strings.HasPrefix(s, "ConstantScore(") && strings.HasSuffix(s, ")") && balancedInner(s[len("ConstantScore("):len(s)-1]) {
					s = s[len("ConstantScore(") : len(s)-1]
				}
			}
			clauses = append(clauses, clause{occur: occur, text: s, bool: isBool})
		}
	}
	add("+", spec.must)
	add("-", spec.mustNot)
	add("", spec.should)
	add("#", spec.filter)
	if len(spec.must)+len(spec.should)+len(spec.filter) == 0 && spec.adjustPureNegative {
		clauses = append(clauses, clause{occur: "#", text: "*:*"})
	}
	msm := 0
	if spec.msm != nil && len(spec.should) > 0 {
		if n, err := calcMinShouldMatch(len(spec.should), *spec.msm); err == nil {
			msm = n
		}
	}
	if d.rewrite && len(clauses) == 1 {
		c := clauses[0]
		switch c.occur {
		case "#":
			return "(ConstantScore(" + c.text + "))^0.0", false
		case "+":
			return c.text, c.bool
		case "":
			if msm <= 1 {
				return c.text, c.bool
			}
		}
	}
	parts := make([]string, len(clauses))
	for i, c := range clauses {
		text := c.text
		if c.bool {
			text = "(" + text + ")"
		}
		parts[i] = c.occur + text
	}
	out := strings.Join(parts, " ")
	if msm > 0 {
		out = "(" + out + ")~" + strconv.Itoa(msm)
	}
	return out, true
}

// balancedInner reports whether s has balanced parentheses (so that an
// enclosing wrapper can be removed).
func balancedInner(s string) bool {
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// Java exception messages ----------------------------------------------------------

// javaExceptionClasses are the classes behind the exception types.
var javaExceptionClasses = map[string]string{
	"parsing_exception":                   "org.opensearch.core.common.ParsingException",
	"query_shard_exception":               "org.opensearch.index.query.QueryShardException",
	"parse_exception":                     "org.opensearch.OpenSearchParseException",
	"illegal_argument_exception":          "java.lang.IllegalArgumentException",
	"illegal_state_exception":             "java.lang.IllegalStateException",
	"number_format_exception":             "java.lang.NumberFormatException",
	"date_time_parse_exception":           "java.time.format.DateTimeParseException",
	"named_object_not_found_exception":    "org.opensearch.core.xcontent.NamedObjectNotFoundException",
	"x_content_parse_exception":           "org.opensearch.core.xcontent.XContentParseException",
	"json_parse_exception":                "org.opensearch.tools.jackson.core.JsonParseException",
	"stream_read_exception":               "org.opensearch.tools.jackson.core.exc.StreamReadException",
	"input_coercion_exception":            "org.opensearch.tools.jackson.core.exc.InputCoercionException",
	"null_pointer_exception":              "java.lang.NullPointerException",
	"unsupported_operation_exception":     "java.lang.UnsupportedOperationException",
	"class_cast_exception":                "java.lang.ClassCastException",
	"array_index_out_of_bounds_exception": "java.lang.ArrayIndexOutOfBoundsException",
	"arithmetic_exception":                "java.lang.ArithmeticException",
	"search_exception":                    "org.opensearch.search.SearchException",
	"exception":                           "org.opensearch.OpenSearchException",
}

// exceptionClassName is the fully qualified class of an error.
func exceptionClassName(e *Error) string {
	if e.Type == "parse_exception" && e.plain {
		return "org.apache.lucene.queryparser.classic.ParseException"
	}
	if name, ok := javaExceptionClasses[e.Type]; ok {
		return name
	}
	var b strings.Builder
	for _, part := range strings.Split(e.Type, "_") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return "org.opensearch." + b.String()
}

func exceptionSimpleName(e *Error) string {
	name := exceptionClassName(e)
	return name[strings.LastIndex(name, ".")+1:]
}

// exceptionsDetailedMessage is ExceptionsHelper.detailedMessage.
func exceptionsDetailedMessage(e *Error) string {
	if e.Cause == nil {
		return exceptionSimpleName(e) + "[" + e.Reason + "]"
	}
	var b strings.Builder
	for t := e; t != nil; t = t.Cause {
		b.WriteString(exceptionSimpleName(t))
		if !t.nullReason {
			b.WriteString("[" + t.Reason + "]")
		}
		b.WriteString("; ")
		if t.Cause != nil {
			b.WriteString("nested: ")
		}
	}
	return b.String()
}

// openSearchDetailedMessage is OpenSearchException.getDetailedMessage; uuid
// gives the uuid of the index an error names.
func openSearchDetailedMessage(e *Error, uuid func(string) string) string {
	text := strings.TrimSpace(exceptionsDetailedMessage(e))
	if e.Index != "" {
		id := "_na_"
		if uuid != nil {
			id = uuid(e.Index)
		}
		text = "[" + e.Index + "/" + id + "] " + text
	}
	if e.Cause == nil {
		return text
	}
	if !e.Cause.isPlain() {
		return text + "; " + openSearchDetailedMessage(e.Cause, uuid)
	}
	cause := exceptionClassName(e.Cause)
	if !e.Cause.nullReason {
		cause += ": " + e.Cause.Reason
	}
	return text + "; " + cause
}
