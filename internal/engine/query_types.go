package engine

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"
)

// Typed query values: the term, range and exists semantics of OpenSearch's
// field types (NumberFieldMapper, DateFieldMapper, IpFieldMapper, ...).

// searchPath resolves field aliases: the path of the field a query on
// field searches (a multi-field of an alias target keeps its suffix).
// Query builders set this as the bleve field name.
func (m *Mapping) searchPath(field string) string {
	parts := strings.Split(field, ".")
	fields := m.Properties
	for i, p := range parts {
		f, ok := fields[p]
		if !ok {
			return field
		}
		if f.Type == TypeAlias && f.Path != "" {
			rest := strings.Join(parts[i+1:], ".")
			target := f.Path
			if rest != "" {
				target += "." + rest
			}
			if target == field {
				return field
			}
			return m.searchPath(target)
		}
		fields = f.Properties
	}
	return field
}

// objectPaths lists the paths of the object and nested fields.
func (m *Mapping) objectPaths() []string {
	var out []string
	var walk func(prefix string, fields map[string]*Field)
	walk = func(prefix string, fields map[string]*Field) {
		for name, f := range fields {
			if f.Type == TypeObject || f.Type == TypeNested {
				out = append(out, prefix+name)
				walk(prefix+name+".", f.Properties)
			}
		}
	}
	walk("", m.Properties)
	return out
}

// errCreateQuery is a query_shard_exception around the failure to build a
// field's query.
func errCreateQueryCause(cause *Error) *Error {
	return &Error{Status: 400, Type: "query_shard_exception", Reason: "failed to create query: " + cause.Reason, Cause: cause}
}

// fieldHasDocValues reports whether a field keeps doc values.
func fieldHasDocValues(f *Field) bool {
	switch f.Type {
	case TypeText, TypeMatchOnlyText, TypeObject, TypeNested, TypeCompletion, TypeGeoShape, TypeXYShape, TypeRankFeature, TypeRankFeatures, TypePercolator, TypeFlatObject:
		return getBool(f.Extra, "doc_values", false) && f.Type != TypeText
	case TypeBinary, TypeSearchAsYouType:
		return getBool(f.Extra, "doc_values", false)
	}
	return getBool(f.Extra, "doc_values", true)
}

// searchableError is the failIfNotIndexed / failIfNotIndexedAndNoDocValues
// check of a query on field.
func searchableError(field string, f *Field) *Error {
	if f == nil || f.Index {
		return nil
	}
	switch f.Type {
	case TypeText, TypeMatchOnlyText, TypeSearchAsYouType:
		return errCreateQueryCause(errIllegalArgument("Cannot search on field [%s] since it is not indexed.", field))
	}
	if fieldHasDocValues(f) {
		return nil
	}
	return errCreateQueryCause(errIllegalArgument("Cannot search on field [%s] since it is both not indexed, and does not have doc_values enabled.", field))
}

// fieldIndexed reports whether documents index a field so queries can
// match it (doc values stand in for the index on OpenSearch).
func fieldIndexed(f *Field) bool {
	if f.Index {
		return true
	}
	switch f.Type {
	case TypeText, TypeMatchOnlyText, TypeSearchAsYouType:
		return false
	}
	return fieldHasDocValues(f)
}

// Java objectToDouble ----------------------------------------------------------

// queryValueText is value.toString() of a query value.
func queryValueText(v any) string {
	switch t := v.(type) {
	case json.Number:
		return javaJSONNumberText(t)
	case float64:
		return javaNumberString(t, 64)
	}
	return javaValueString(v)
}

func objectToDouble(v any) (float64, *Error) {
	switch t := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(t.String(), 64)
		if err != nil && !math.IsInf(f, 0) {
			return 0, numberFormatError(t.String())
		}
		return f, nil
	case float64:
		return t, nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	}
	return javaParseDouble(javaValueString(v), 64)
}

func hasDecimalPart(v any) (bool, *Error) {
	switch v := v.(type) {
	case json.Number, float64, int, int64:
		d, err := objectToDouble(v)
		if err != nil {
			return false, err
		}
		return math.Mod(d, 1) != 0 || math.IsNaN(d), nil
	case string:
		d, err := javaParseDouble(v, 64)
		if err != nil {
			return false, err
		}
		return math.Mod(d, 1) != 0 || math.IsNaN(d), nil
	}
	return false, nil
}

func signum(v any) float64 {
	d, err := objectToDouble(v)
	if err != nil || d == 0 {
		return 0
	}
	if d < 0 {
		return -1
	}
	return 1
}

// parseIntegralQueryValue is the parse(value, coerce=true) of the integral
// number types (byte and short parse as integers).
func parseIntegralQueryValue(f *Field, v any) (*big.Int, *Error) {
	return parseIntegralValue(f, v, queryValueText(v))
}

// parseIntegralTermValue parses the value of a term or range query: Lucene
// holds string values as a BytesRef, which out of range errors print as bytes.
func parseIntegralTermValue(f *Field, v any) (*big.Int, *Error) {
	shown := queryValueText(v)
	if s, isString := v.(string); isString {
		shown = bytesRefText(s)
	}
	return parseIntegralValue(f, v, shown)
}

func parseIntegralValue(f *Field, v any, shown string) (*big.Int, *Error) {
	text := queryValueText(v)
	d, err := objectToDouble(v)
	if err != nil {
		return nil, err
	}
	switch f.Type {
	case TypeLong:
		if d < -9.223372036854775808e18 || d > 9.223372036854775807e18 {
			return nil, errIllegalArgument("Value [%s] is out of range for a long", shown)
		}
		if _, isNum := v.(json.Number); !isNum {
			if _, isStr := v.(string); !isStr {
				return big.NewInt(int64(d)), nil
			}
		}
		lv, lerr := numbersToLong(text, true)
		if lerr != nil {
			return nil, lerr
		}
		return big.NewInt(lv), nil
	case TypeUnsignedLong:
		r, ok := bigDecimalOf(text)
		if !ok {
			if math.IsNaN(d) {
				return nil, errIllegalArgument("For input string: \"%s\"", text)
			}
			r = new(big.Rat)
			r.SetFloat64(d)
		}
		bi := ratTrunc(r)
		if bi.Sign() < 0 || bi.Cmp(bigMaxULong) > 0 {
			return nil, errIllegalArgument("Value [%s] is out of range for an unsigned long", shown)
		}
		return bi, nil
	}
	if d < math.MinInt32 || d > math.MaxInt32 {
		return nil, errIllegalArgument("Value [%s] is out of range for an integer", shown)
	}
	if math.IsNaN(d) {
		return big.NewInt(0), nil
	}
	return big.NewInt(int64(d)), nil
}

// parseFloatQueryValue is FLOAT/HALF_FLOAT/DOUBLE.parse(value, false).
func parseFloatQueryValue(f *Field, v any) (float64, *Error) {
	bits := 64
	if f.Type == TypeFloat || f.Type == TypeHalfFloat {
		bits = 32
	}
	var d float64
	switch t := v.(type) {
	case json.Number:
		d, _ = strconv.ParseFloat(t.String(), 64)
	case float64:
		d = t
	default:
		var err *Error
		if d, err = javaParseDouble(javaValueString(v), bits); err != nil {
			return 0, err
		}
	}
	if bits == 32 {
		d = float64(float32(d))
	}
	switch f.Type {
	case TypeHalfFloat:
		if h := halfFloat(float32(d)); math.IsInf(float64(h), 0) || math.IsNaN(float64(h)) {
			return 0, errIllegalArgument("[half_float] supports only finite values, but got [%s]", javaFloatText(d, 32))
		}
	case TypeFloat:
		if math.IsInf(d, 0) || math.IsNaN(d) {
			return 0, errIllegalArgument("[float] supports only finite values, but got [%s]", javaFloatText(d, 32))
		}
	case TypeDouble:
		if math.IsInf(d, 0) || math.IsNaN(d) {
			return 0, errIllegalArgument("[double] supports only finite values, but got [%s]", javaFloatText(d, 64))
		}
	}
	return d, nil
}

func numericRange(field string, lo, hi *float64) query.Query {
	inc := true
	q := bleve.NewNumericRangeInclusiveQuery(lo, hi, &inc, &inc)
	q.SetField(field)
	return q
}

func bigToFloat(b *big.Int) float64 {
	f, _ := new(big.Float).SetInt(b).Float64()
	return f
}

// numericTermQuery is NumberFieldMapper's termQuery.
func numericTermQuery(field string, f *Field, value any) (query.Query, *Error) {
	switch f.Type {
	case TypeLong, TypeInteger, TypeShort, TypeByte, TypeUnsignedLong, TypeTokenCount:
		dec, err := hasDecimalPart(value)
		if err != nil {
			return nil, errCreateQueryCause(err)
		}
		if dec {
			return bleve.NewMatchNoneQuery(), nil
		}
		n, err := parseIntegralTermValue(f, value)
		if err != nil {
			return nil, errCreateQueryCause(err)
		}
		tq := bleve.NewTermQuery(n.String())
		tq.SetField(exactNumericField(field))
		return tq, nil
	case TypeScaledFloat:
		d, err := objectToDouble(value)
		if err != nil {
			return nil, errCreateQueryCause(err)
		}
		sf := scalingFactor(f)
		v := float64(javaMathRound(d*sf)) * (1 / sf)
		return numericRange(field, &v, &v), nil
	}
	d, err := parseFloatQueryValue(f, value)
	if err != nil {
		return nil, errCreateQueryCause(err)
	}
	if f.Type == TypeHalfFloat {
		d = float64(halfFloat(float32(d)))
	}
	return numericRange(field, &d, &d), nil
}

// numericRangeQuery is NumberFieldMapper's rangeQuery (bounds of nil are open).
func numericRangeQuery(field string, f *Field, lower, upper any, includeLower, includeUpper bool) (query.Query, *Error) {
	switch f.Type {
	case TypeLong, TypeInteger, TypeShort, TypeByte, TypeUnsignedLong, TypeTokenCount:
		minV, maxV := big.NewInt(math.MinInt32), big.NewInt(math.MaxInt32)
		switch f.Type {
		case TypeLong:
			minV, maxV = big.NewInt(math.MinInt64), big.NewInt(math.MaxInt64)
		case TypeUnsignedLong:
			minV, maxV = big.NewInt(0), new(big.Int).Set(bigMaxULong)
		}
		lo, hi := new(big.Int).Set(minV), new(big.Int).Set(maxV)
		if lower != nil {
			n, err := parseIntegralTermValue(f, lower)
			if err != nil {
				return nil, errCreateQueryCause(err)
			}
			dec, derr := hasDecimalPart(lower)
			if derr != nil {
				return nil, errCreateQueryCause(derr)
			}
			lo = n
			if (!dec && !includeLower) || (dec && signum(lower) > 0) {
				if lo.Cmp(maxV) == 0 {
					return bleve.NewMatchNoneQuery(), nil
				}
				lo = new(big.Int).Add(lo, big.NewInt(1))
			}
		}
		if upper != nil {
			n, err := parseIntegralTermValue(f, upper)
			if err != nil {
				return nil, errCreateQueryCause(err)
			}
			dec, derr := hasDecimalPart(upper)
			if derr != nil {
				return nil, errCreateQueryCause(derr)
			}
			hi = n
			if (!dec && !includeUpper) || (dec && signum(upper) < 0) {
				if hi.Cmp(minV) == 0 {
					return bleve.NewMatchNoneQuery(), nil
				}
				hi = new(big.Int).Sub(hi, big.NewInt(1))
			}
		}
		if lo.Cmp(hi) > 0 {
			return bleve.NewMatchNoneQuery(), nil
		}
		var lp, hp *float64
		if lower != nil {
			v := bigToFloat(lo)
			lp = &v
		}
		if upper != nil {
			v := bigToFloat(hi)
			hp = &v
		}
		if lp == nil && hp == nil {
			return fieldPresenceQuery(field), nil
		}
		return numericRange(field, lp, hp), nil
	case TypeScaledFloat:
		sf := scalingFactor(f)
		var lp, hp *float64
		if lower != nil {
			d, err := objectToDouble(lower)
			if err != nil {
				return nil, errCreateQueryCause(err)
			}
			dv := d * sf
			if !includeLower {
				dv = math.Nextafter(dv, math.Inf(1))
			}
			v := float64(javaMathRound(math.Ceil(dv))) * (1 / sf)
			lp = &v
		}
		if upper != nil {
			d, err := objectToDouble(upper)
			if err != nil {
				return nil, errCreateQueryCause(err)
			}
			dv := d * sf
			if !includeUpper {
				dv = math.Nextafter(dv, math.Inf(-1))
			}
			v := float64(javaMathRound(math.Floor(dv))) * (1 / sf)
			hp = &v
		}
		if lp == nil && hp == nil {
			return fieldPresenceQuery(field), nil
		}
		return rangeWithFlags(field, lp, hp, true, true), nil
	}
	var lp, hp *float64
	if lower != nil {
		d, err := parseFloatQueryValue(f, lower)
		if err != nil {
			return nil, errCreateQueryCause(err)
		}
		lp = &d
	}
	if upper != nil {
		d, err := parseFloatQueryValue(f, upper)
		if err != nil {
			return nil, errCreateQueryCause(err)
		}
		hp = &d
	}
	if lp == nil && hp == nil {
		return fieldPresenceQuery(field), nil
	}
	return rangeWithFlags(field, lp, hp, includeLower, includeUpper), nil
}

func rangeWithFlags(field string, lo, hi *float64, incLo, incHi bool) query.Query {
	q := bleve.NewNumericRangeInclusiveQuery(lo, hi, &incLo, &incHi)
	q.SetField(field)
	return q
}

// fieldPresenceQuery matches the documents with a value in field (an
// unbounded range).
func fieldPresenceQuery(field string) query.Query {
	tq := bleve.NewTermQuery(field)
	tq.SetField("_exists_")
	return tq
}

// dates ----------------------------------------------------------------------------------

// dateQueryBound parses a date bound to its epoch value (millis or nanos).
func (qb *queryBuilder) dateQueryBound(f *Field, df *DateFormat, v any, roundUp bool, loc *time.Location) (time.Time, *Error) {
	text, ok := dateText(v)
	if !ok {
		text = javaValueString(v)
	}
	t, err := ParseDateMath(text, df, qb.c.now(), loc, roundUp)
	if err != nil {
		if de, isDate := err.(*dateError); isDate {
			return time.Time{}, de.queryError()
		}
		return time.Time{}, &Error{Status: 400, Type: "parse_exception", Reason: err.Error()}
	}
	if f.Type == TypeDateNanos {
		if msg := nanosRangeError(t); msg != "" {
			return time.Time{}, errIllegalArgument("%s", msg)
		}
	}
	return t, nil
}

// dateRangeBleveQuery builds the query for inclusive epoch bounds.
func dateRangeBleveQuery(field string, f *Field, lo, hi *time.Time) query.Query {
	if f.Type == TypeDateNanos {
		inc := true
		var start, end time.Time
		if lo != nil {
			start = *lo
		} else {
			start = minNanosTime
		}
		if hi != nil {
			end = *hi
		} else {
			end = maxNanosTime
		}
		if start.After(end) {
			return bleve.NewMatchNoneQuery()
		}
		// bleve rejects the int64 nanosecond limits; a zero time is an open
		// bound
		if !start.After(minNanosTime) {
			start = time.Time{}
		}
		if !end.Before(maxNanosTime) {
			end = time.Time{}
		}
		if start.IsZero() && end.IsZero() {
			return fieldPresenceQuery(field)
		}
		q := bleve.NewDateRangeInclusiveQuery(start, end, &inc, &inc)
		q.SetField(field)
		return q
	}
	var lp, hp *float64
	if lo != nil {
		v := float64(epochMillis(*lo))
		lp = &v
	}
	if hi != nil {
		v := float64(epochMillis(*hi))
		hp = &v
	}
	if lp != nil && hp != nil && *lp > *hp {
		return bleve.NewMatchNoneQuery()
	}
	return numericRange(field, lp, hp)
}

func adjustDateBound(f *Field, t time.Time, up bool) time.Time {
	unit := time.Millisecond
	if f.Type == TypeDateNanos {
		unit = time.Nanosecond
	}
	if f.Type != TypeDateNanos {
		t = time.UnixMilli(epochMillis(t)).UTC()
	}
	if up {
		return t.Add(unit)
	}
	return t.Add(-unit)
}

// ips --------------------------------------------------------------------------------------

func ipQueryText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return queryValueText(v)
}

func ipTermQuery(field string, value any) (query.Query, *Error) {
	text := ipQueryText(value)
	if _, isString := value.(string); isString && strings.Contains(text, "/") {
		lo, hi, err := parseCIDR(text)
		if err != nil {
			return nil, errCreateQueryCause(err)
		}
		inc := true
		q := bleve.NewTermRangeInclusiveQuery(ipTerm(lo), ipTerm(hi), &inc, &inc)
		q.SetField(field)
		return q, nil
	}
	ip, ok := parseIPString(text)
	if !ok {
		return nil, errCreateQueryCause(errNotIP(text))
	}
	tq := bleve.NewTermQuery(ipTerm(ip))
	tq.SetField(field)
	return tq, nil
}

func ipRangeQuery(field string, lower, upper any, includeLower, includeUpper bool) (query.Query, *Error) {
	min, max := strings.Repeat("0", 32), strings.Repeat("f", 32)
	loInc, hiInc := includeLower, includeUpper
	if lower != nil {
		text := ipQueryText(lower)
		ip, ok := parseIPString(text)
		if !ok {
			return nil, errCreateQueryCause(errNotIP(text))
		}
		min = ipTerm(ip)
	} else {
		loInc = true
	}
	if upper != nil {
		text := ipQueryText(upper)
		ip, ok := parseIPString(text)
		if !ok {
			return nil, errCreateQueryCause(errNotIP(text))
		}
		max = ipTerm(ip)
	} else {
		hiInc = true
	}
	q := bleve.NewTermRangeInclusiveQuery(min, max, &loInc, &hiInc)
	q.SetField(field)
	return q, nil
}

// booleans ------------------------------------------------------------------------------------

// booleanQueryTerm is BooleanFieldType.indexedValueForSearch.
func booleanQueryTerm(v any) (string, *Error) {
	switch t := v.(type) {
	case bool:
		if t {
			return "T", nil
		}
		return "F", nil
	case nil:
		return "F", nil
	}
	s := queryValueText(v)
	switch s {
	case "true":
		return "T", nil
	case "false":
		return "F", nil
	}
	return "", errCreateQueryCause(errIllegalArgument("Can't parse boolean value [%s], expected [true] or [false]", s))
}
