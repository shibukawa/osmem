package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"
)

// Range field types (RangeFieldMapper and RangeType of OpenSearch): the
// values of documents are parsed with the mapper's rules and messages, and
// term, range and match queries are evaluated against the ranges read from
// the source of each document that has the field.

func isRangeType(t string) bool {
	switch t {
	case TypeIntegerRange, TypeLongRange, TypeFloatRange, TypeDoubleRange, TypeDateRange, TypeIPRange:
		return true
	}
	return false
}

// rangeEnd is one end of a range: integer, long and date ranges use i
// (dates as epoch millis), float and double ranges f, ip ranges the 16 byte
// address.
type rangeEnd struct {
	i  int64
	f  float64
	ip [16]byte
}

// fieldRange is an inclusive range of a document.
type fieldRange struct{ from, to rangeEnd }

func rangeCmp(t string, a, b rangeEnd) int {
	switch t {
	case TypeFloatRange, TypeDoubleRange:
		switch {
		case a.f < b.f:
			return -1
		case a.f > b.f:
			return 1
		}
		return 0
	case TypeIPRange:
		return bytes.Compare(a.ip[:], b.ip[:])
	}
	switch {
	case a.i < b.i:
		return -1
	case a.i > b.i:
		return 1
	}
	return 0
}

// rangeMin and rangeMax are RangeType.minValue and maxValue.
func rangeMin(t string) rangeEnd {
	switch t {
	case TypeIntegerRange:
		return rangeEnd{i: math.MinInt32}
	case TypeFloatRange, TypeDoubleRange:
		return rangeEnd{f: math.Inf(-1)}
	case TypeIPRange:
		return rangeEnd{}
	}
	return rangeEnd{i: math.MinInt64}
}

func rangeMax(t string) rangeEnd {
	switch t {
	case TypeIntegerRange:
		return rangeEnd{i: math.MaxInt32}
	case TypeFloatRange, TypeDoubleRange:
		return rangeEnd{f: math.Inf(1)}
	case TypeIPRange:
		var e rangeEnd
		for i := range e.ip {
			e.ip[i] = 0xff
		}
		return e
	}
	return rangeEnd{i: math.MaxInt64}
}

// rangeNext is RangeType.nextUp (up) and nextDown: integral values wrap
// around like Java arithmetic, addresses fail at the ends of the space.
func rangeNext(t string, e rangeEnd, up bool) (rangeEnd, *Error) {
	switch t {
	case TypeIntegerRange:
		v := int32(e.i)
		if up {
			v++
		} else {
			v--
		}
		return rangeEnd{i: int64(v)}, nil
	case TypeFloatRange:
		f := float32(e.f)
		if up {
			f = math.Nextafter32(f, float32(math.Inf(1)))
		} else {
			f = math.Nextafter32(f, float32(math.Inf(-1)))
		}
		return rangeEnd{f: float64(f)}, nil
	case TypeDoubleRange:
		if up {
			return rangeEnd{f: math.Nextafter(e.f, math.Inf(1))}, nil
		}
		return rangeEnd{f: math.Nextafter(e.f, math.Inf(-1))}, nil
	case TypeIPRange:
		ip := e.ip
		if up {
			if ip == rangeMax(t).ip {
				return e, &Error{Status: 400, Type: "arithmetic_exception", Reason: "Overflow: there is no greater InetAddress than " + formatIP(net.IP(ip[:]))}
			}
			for i := len(ip) - 1; i >= 0; i-- {
				ip[i]++
				if ip[i] != 0 {
					break
				}
			}
		} else {
			if ip == (rangeEnd{}).ip {
				return e, &Error{Status: 400, Type: "arithmetic_exception", Reason: "Underflow: there is no smaller InetAddress than " + formatIP(net.IP(ip[:]))}
			}
			for i := len(ip) - 1; i >= 0; i-- {
				ip[i]--
				if ip[i] != 0xff {
					break
				}
			}
		}
		return rangeEnd{ip: ip}, nil
	}
	v := e.i
	if up {
		v++
	} else {
		v--
	}
	return rangeEnd{i: v}, nil
}

// rangeEndText renders an end the way the Lucene range fields print it in
// their messages.
func rangeEndText(t string, e rangeEnd) string {
	switch t {
	case TypeFloatRange:
		return javaFloatText(e.f, 32)
	case TypeDoubleRange:
		return javaFloatText(e.f, 64)
	case TypeIPRange:
		return formatIP(net.IP(e.ip[:]))
	}
	return strconv.FormatInt(e.i, 10)
}

// javaInetAddressText is InetAddress.toString() of an address without host
// name ("/10.0.0.1", "/0:0:0:0:0:0:0:1").
func javaInetAddressText(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return "/" + v4.String()
	}
	b := ip.To16()
	parts := make([]string, 8)
	for i := 0; i < 8; i++ {
		parts[i] = strconv.FormatUint(uint64(b[2*i])<<8|uint64(b[2*i+1]), 16)
	}
	return "/" + strings.Join(parts, ":")
}

// document values -----------------------------------------------------------------

type rangeFailKind int

const (
	// rangeHandled is an IllegalArgumentException parsing a bound: the
	// mapper records the field in _ignored when malformed values are ignored
	rangeHandled rangeFailKind = iota
	// rangeInside is any other failure inside the object: a swallowed
	// failure leaves the parser inside it
	rangeInside
	// rangeAfter is a failure at a scalar or after the object was read
	rangeAfter
)

type rangeFailure struct {
	cause   *Error
	preview string
	kind    rangeFailKind
	// node is the raw value the failure happened in (for leftover errors)
	node *rawNode
}

// rangeNumberType is the number type parsing the bounds of a numeric range.
func rangeNumberType(t string) string {
	switch t {
	case TypeIntegerRange:
		return TypeInteger
	case TypeLongRange:
		return TypeLong
	case TypeFloatRange:
		return TypeFloat
	}
	return TypeDouble
}

// parseRangeFieldBound parses one bound of a document range (RangeType.parseFrom
// and parseTo without the exclusive adjustment).
func parseRangeFieldBound(f *Field, v any, node *rawNode, coerce bool) (rangeEnd, *rangeFailure) {
	switch f.Type {
	case TypeDateRange, TypeIPRange:
		var text string
		switch t := v.(type) {
		case string:
			text = t
		case json.Number:
			text = t.String()
		case bool:
			text = strconv.FormatBool(t)
		case float64:
			text = javaNumberString(t, 64)
		default:
			line, col := node.lineCol()
			return rangeEnd{}, &rangeFailure{cause: &Error{Type: "illegal_state_exception", Reason: fmt.Sprintf("Can't get text on a %s at %d:%d", valueTokenName(v), line, col)}, kind: rangeInside}
		}
		if f.Type == TypeIPRange {
			ip, ok := parseIPString(text)
			if !ok {
				return rangeEnd{}, &rangeFailure{cause: errNotIP(text), kind: rangeHandled}
			}
			var e rangeEnd
			copy(e.ip[:], ip.To16())
			return e, nil
		}
		millis, err := parseRangeDate(f, text)
		if err != nil {
			return rangeEnd{}, &rangeFailure{cause: err, kind: rangeInside}
		}
		return rangeEnd{i: millis}, nil
	}
	numType := rangeNumberType(f.Type)
	if s, isString := v.(string); isString && coerce && s == "" {
		return rangeEnd{}, &rangeFailure{cause: &Error{Type: "number_format_exception", Reason: "empty String"}, kind: rangeHandled}
	}
	nv, preview, cerr := parseNumericField(&Field{Type: numType, Extra: M{}}, v, coerce)
	if cerr != nil {
		kind := rangeHandled
		if cerr.Type == "input_coercion_exception" {
			kind = rangeInside
			off := 0
			if node != nil {
				off = node.tokEnd
			}
			reason := cerr.Reason + jacksonLocation(off)
			cerr = &Error{Type: cerr.Type, Reason: reason, Cause: &Error{Type: cerr.Type, Reason: reason}}
		}
		return rangeEnd{}, &rangeFailure{cause: cerr, preview: preview, kind: kind}
	}
	switch f.Type {
	case TypeFloatRange, TypeDoubleRange:
		return rangeEnd{f: nv.value}, nil
	}
	if nv.exact != "" {
		if n, err := strconv.ParseInt(nv.exact, 10, 64); err == nil {
			return rangeEnd{i: n}, nil
		}
	}
	return rangeEnd{i: int64(nv.value)}, nil
}

// parseRangeDate parses a date bound at indexing time (date math is
// allowed, now is not).
func parseRangeDate(f *Field, text string) (int64, *Error) {
	if strings.HasPrefix(text, "now") {
		return 0, &Error{Status: 400, Type: "parse_exception", Reason: "could not read the current timestamp",
			Cause: errIllegalArgument("now is not used at indexing time")}
	}
	df := f.Format
	if df == nil {
		df = ParseDateFormat(DefaultDateFormat)
	}
	t, err := ParseDateMath(text, df, time.Time{}, time.UTC, false)
	if err != nil {
		if de, ok := err.(*dateError); ok {
			return 0, de.queryError()
		}
		return 0, &Error{Status: 400, Type: "parse_exception", Reason: err.Error()}
	}
	return epochMillis(t), nil
}

// parseRangeValue parses one value of a range field: an object of bounds or,
// for ip ranges, a CIDR string. name is the full field name, simple the name
// reported for scalars ("null" inside arrays).
func parseRangeValue(f *Field, name, simple string, v any, node *rawNode, coerce bool) (fieldRange, *rangeFailure) {
	t := f.Type
	switch val := v.(type) {
	case M:
		from, to := rangeMin(t), rangeMax(t)
		fromSet, toSet := false, false
		keys := sortedMapKeys(val)
		var nodes []*rawNode
		if node != nil && node.kind == '{' {
			keys, nodes = node.keys, node.vals
		}
		for i, k := range keys {
			var bv any
			var bn *rawNode
			if nodes != nil {
				bn = nodes[i]
				bv = bn.value()
			} else {
				bv = val[k]
			}
			lower, include := false, false
			switch k {
			case "gt":
				lower = true
			case "gte":
				lower, include = true, true
			case "lt":
			case "lte":
				include = true
			default:
				return fieldRange{}, &rangeFailure{cause: errMapperParsing("error parsing field [%s], with unknown parameter [%s]", name, k), preview: javaValueString(bv), kind: rangeInside, node: node}
			}
			if lower && fromSet {
				return fieldRange{}, &rangeFailure{cause: errMapperParsing("error parsing field [%s], invalid lower bound (gt/gte)", name), preview: javaValueString(bv), kind: rangeInside, node: node}
			}
			if !lower && toSet {
				return fieldRange{}, &rangeFailure{cause: errMapperParsing("error parsing field [%s], invalid upper bound (lt/lte)", name), preview: javaValueString(bv), kind: rangeInside, node: node}
			}
			if lower {
				fromSet = true
			} else {
				toSet = true
			}
			if bv == nil {
				continue
			}
			end, failure := parseRangeFieldBound(f, bv, bn, coerce)
			if failure != nil {
				if failure.preview == "" {
					failure.preview = javaValueString(bv)
				}
				failure.node = node
				return fieldRange{}, failure
			}
			if !include {
				var err *Error
				if end, err = rangeNext(t, end, lower); err != nil {
					return fieldRange{}, &rangeFailure{cause: err, preview: javaValueString(bv), kind: rangeInside, node: node}
				}
			}
			if lower {
				from = end
			} else {
				to = end
			}
		}
		if rangeCmp(t, from, to) > 0 {
			var cause *Error
			if t == TypeIPRange {
				cause = errIllegalArgument("min value cannot be greater than max value for InetAddressRange field")
			} else {
				cause = errIllegalArgument("min value (%s) is greater than max value (%s)", rangeEndText(t, from), rangeEndText(t, to))
			}
			return fieldRange{}, &rangeFailure{cause: cause, preview: "null", kind: rangeAfter}
		}
		return fieldRange{from: from, to: to}, nil
	case string:
		if t == TypeIPRange {
			lo, hi, err := parseCIDR(val)
			if err != nil {
				return fieldRange{}, &rangeFailure{cause: err, preview: val, kind: rangeHandled}
			}
			var r fieldRange
			copy(r.from.ip[:], lo.To16())
			copy(r.to.ip[:], hi.To16())
			return r, nil
		}
	}
	return fieldRange{}, &rangeFailure{cause: errMapperParsing("error parsing field [%s], expected an object but got %s", name, simple), preview: javaValueString(v), kind: rangeAfter}
}

// addRange parses a value of a range field while indexing a document. The
// ranges are read back from the source by the queries; the document only
// records that the field has a value.
func (b *docBuilder) addRange(name, rawPath string, f *Field, v any, occ int) (bool, error) {
	node, inArray := b.rawLeaf(rawPath, occ, false)
	simple := name[strings.LastIndexByte(name, '.')+1:]
	if inArray {
		simple = "null"
	}
	_, failure := parseRangeValue(f, name, simple, v, node, b.ix.coerceEnabled(f))
	if failure == nil {
		return true, nil
	}
	if b.ix.ignoreMalformed(f) {
		switch failure.kind {
		case rangeHandled:
			b.rootBuilder().ignored[name] = true
			return false, nil
		case rangeInside:
			return false, b.errLeftoverContent(node)
		}
		return false, nil
	}
	return false, b.valueError(name, f, v, failure.preview, failure.cause)
}

// docByBleveID returns the stored document (or nested object) behind a
// bleve document id.
func (ix *Index) docByBleveID(id string) *Doc {
	root, chain := parseNestedID(id)
	d := ix.docs[root]
	if d == nil || chain == nil {
		return d
	}
	return ix.nestedDocByChain(d, chain)
}

// sourceLeafValues returns the source values of a field in a document
// (arrays flattened, nulls dropped).
func (ix *Index) sourceLeafValues(d *Doc, path string) []any {
	f, base, ok := ix.Mapping.resolve(path)
	if !ok {
		return nil
	}
	raw, found := lookupPathFound(d.Src, base)
	if !found {
		return nil
	}
	vals := leafValues(f, raw)
	if f.Type == TypeCompletion {
		// the source values of completion fields are listed one by one
		vals = flattenKeepNull(raw)
	}
	var out []any
	for _, v := range vals {
		if v != nil {
			out = append(out, v)
		}
	}
	return out
}

// docRanges parses the ranges of a document for a range field.
func (ix *Index) docRanges(d *Doc, path string, f *Field) []fieldRange {
	coerce := ix.coerceEnabled(f)
	var out []fieldRange
	for _, v := range ix.sourceLeafValues(d, path) {
		if r, failure := parseRangeValue(f, path, "", v, nil, coerce); failure == nil {
			out = append(out, r)
		}
	}
	return out
}

// queries ------------------------------------------------------------------------

// rangeQueryEnd parses a query value for a range field (RangeType.parse
// with coerce disabled, dates with the date math parser).
func (qb *queryBuilder) rangeQueryEnd(f *Field, v any, roundUp bool, df *DateFormat, loc *time.Location) (rangeEnd, *Error) {
	switch f.Type {
	case TypeIntegerRange, TypeLongRange:
		n, err := parseIntegralTermValue(&Field{Type: rangeNumberType(f.Type)}, v)
		if err != nil {
			return rangeEnd{}, err
		}
		dec, derr := hasDecimalPart(v)
		if derr != nil {
			return rangeEnd{}, derr
		}
		if dec {
			return rangeEnd{}, errIllegalArgument("Value [%s] has a decimal part", queryValueText(v))
		}
		return rangeEnd{i: n.Int64()}, nil
	case TypeFloatRange, TypeDoubleRange:
		d, err := parseFloatQueryValue(&Field{Type: rangeNumberType(f.Type)}, v)
		if err != nil {
			return rangeEnd{}, err
		}
		return rangeEnd{f: d}, nil
	case TypeDateRange:
		if df == nil {
			df = f.Format
		}
		t, err := qb.dateQueryBound(&Field{Type: TypeDate}, df, v, roundUp, loc)
		if err != nil {
			return rangeEnd{}, err
		}
		return rangeEnd{i: epochMillis(t)}, nil
	}
	text := ipQueryText(v)
	ip, ok := parseIPString(text)
	if !ok {
		return rangeEnd{}, errNotIP(text)
	}
	var e rangeEnd
	copy(e.ip[:], ip.To16())
	return e, nil
}

// rangeFieldQuery is RangeFieldType.rangeQuery: documents with a range in
// the given relation to the query range (bounds of nil are open).
func (qb *queryBuilder) rangeFieldQuery(field, path string, f *Field, lower, upper any, includeLower, includeUpper bool, relation string, df *DateFormat, loc *time.Location) (query.Query, error) {
	t := f.Type
	lo, hi := rangeMin(t), rangeMax(t)
	if lower != nil {
		e, err := qb.rangeQueryEnd(f, lower, !includeLower, df, loc)
		if err != nil {
			return nil, errCreateQueryCause(err)
		}
		lo = e
	}
	if upper != nil {
		e, err := qb.rangeQueryEnd(f, upper, includeUpper, df, loc)
		if err != nil {
			return nil, errCreateQueryCause(err)
		}
		hi = e
	}
	if rangeCmp(t, lo, hi) > 0 {
		text := func(e rangeEnd) string {
			if t == TypeIPRange {
				return javaInetAddressText(net.IP(e.ip[:]))
			}
			if t == TypeFloatRange || t == TypeDoubleRange {
				return rangeEndText(t, e)
			}
			return strconv.FormatInt(e.i, 10)
		}
		return nil, errCreateQueryCause(errIllegalArgument("Range query `from` value (%s) is greater than `to` value (%s)", text(lo), text(hi)))
	}
	if !includeLower {
		lo, _ = rangeNext(t, lo, true)
	}
	if !includeUpper {
		hi, _ = rangeNext(t, hi, false)
	}
	if rangeCmp(t, lo, hi) > 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	relation = strings.ToLower(relation)
	ix := qb.ix
	return &docFuncQuery{inner: fieldPresenceQuery(path), fn: func(id string, score float64) (float64, bool) {
		d := ix.docByBleveID(id)
		if d == nil {
			return 0, false
		}
		for _, r := range ix.docRanges(d, path, f) {
			if rangeRelationMatches(t, relation, r, lo, hi) {
				return 1, true
			}
		}
		return 0, false
	}}, nil
}

func rangeRelationMatches(t, relation string, r fieldRange, lo, hi rangeEnd) bool {
	switch relation {
	case "within":
		return rangeCmp(t, r.from, lo) >= 0 && rangeCmp(t, r.to, hi) <= 0
	case "contains":
		return rangeCmp(t, r.from, lo) <= 0 && rangeCmp(t, r.to, hi) >= 0
	}
	return rangeCmp(t, r.from, hi) <= 0 && rangeCmp(t, r.to, lo) >= 0
}

// rangeFieldTermQuery is RangeFieldType.termQuery: the ranges containing the
// value (a date covers the whole rounded interval).
func (qb *queryBuilder) rangeFieldTermQuery(field, path string, f *Field, value any) (query.Query, error) {
	return qb.rangeFieldQuery(field, path, f, value, value, true, true, "intersects", nil, time.UTC)
}

// fields option ------------------------------------------------------------------

// rangeFieldsOutput renders the source ranges of the fields option: every
// bound parsed and formatted by the range type, CIDR strings normalized.
func rangeFieldsOutput(f *Field, vals []any, format string) []any {
	out := make([]any, 0, len(vals))
	coerce := true
	if raw, ok := f.Extra["coerce"]; ok {
		coerce = getBool(M{"v": raw}, "v", true)
	}
	for _, v := range vals {
		switch t := v.(type) {
		case string:
			if f.Type != TypeIPRange {
				continue
			}
			if slash := strings.IndexByte(t, '/'); slash > 0 {
				if ip, ok := parseIPString(t[:slash]); ok {
					out = append(out, formatIP(ip)+t[slash:])
				}
			}
		case M:
			m := M{}
			for k, bv := range t {
				if bv == nil {
					continue
				}
				if f.Type == TypeDateRange {
					text := javaValueString(bv)
					if s, ok := bv.(string); ok {
						text = s
					}
					millis, err := parseRangeDate(f, text)
					if err != nil {
						continue
					}
					m[k] = dateFormatFor(f, format).Format(time.UnixMilli(millis).UTC())
					continue
				}
				end, failure := parseRangeFieldBound(f, bv, nil, coerce)
				if failure != nil {
					continue
				}
				switch f.Type {
				case TypeIntegerRange, TypeLongRange:
					m[k] = end.i
				case TypeFloatRange:
					m[k] = Float(float32(end.f))
				case TypeDoubleRange:
					m[k] = Double(end.f)
				case TypeIPRange:
					m[k] = formatIP(net.IP(end.ip[:]))
				}
			}
			out = append(out, m)
		}
	}
	return out
}
