package engine

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
)

// bucket orders (InternalOrder) ------------------------------------------------

type bucketOrder struct {
	path string // "_count", "_key" or an aggregation path
	asc  bool
	els  []aggPathElement
}

func isKeyOrder(o bucketOrder) bool { return o.path == "_key" }

// parseBucketOrders parses the order parameter of a bucket aggregation
// (InternalOrder.Parser.parseOrderParam for an object or each array item).
func parseBucketOrders(of objFields, body M, key string) ([]bucketOrder, error) {
	v, ok := body[key]
	if !ok {
		return nil, nil
	}
	var items []any
	switch t := v.(type) {
	case M:
		items = []any{t}
	case []any:
		items = t
	}
	var out []bucketOrder
	for _, item := range items {
		m, ok := item.(M)
		if !ok {
			return nil, of.failed(body, key, errParsing("Unexpected token [%s] for [order]", jsonTokenName(item)).atParser(noTok))
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		// the last key of the object wins; JSON order is lost, so the keys
		// are taken in lexical order
		sort.Strings(keys)
		if len(keys) == 0 {
			return nil, of.failed(body, key, errParsing("Must specify at least one field for [order]").at(endTok(m)))
		}
		var o bucketOrder
		for _, k := range keys {
			dir, isStr := m[k].(string)
			if !isStr {
				return nil, of.failed(body, key, errParsing("Unexpected token [%s] for [order]", jsonTokenName(m[k])).at(valueTok(m, k)))
			}
			switch strings.ToLower(dir) {
			case "asc":
				o.asc = true
			case "desc":
				o.asc = false
			default:
				return nil, of.failed(body, key, errParsing("Unknown order direction [%s]", dir).at(valueTok(m, k)))
			}
			o.path = k
		}
		switch o.path {
		case "_term", "_time", "_key":
			o.path = "_key"
		case "_count":
		default:
			els, err := parseAggPath(o.path)
			if err != nil {
				return nil, err
			}
			o.els = els
		}
		out = append(out, o)
	}
	return out, nil
}

// withKeyTiebreak is the compound order of terms and histograms: a key
// ascending tie-breaker unless the order ends with a key order.
func withKeyTiebreak(orders []bucketOrder, def []bucketOrder) []bucketOrder {
	if len(orders) == 0 {
		orders = def
	}
	out := append([]bucketOrder(nil), orders...)
	if !isKeyOrder(out[len(out)-1]) {
		out = append(out, bucketOrder{path: "_key", asc: true})
	}
	return out
}

func errInvalidOrderPath(path, msg string) *Error {
	e := errAggExecution("Invalid aggregation order path [%s]. %s", path, msg)
	e.Cause = &Error{Type: "illegal_argument_exception", Reason: msg}
	return e
}

// validateOrders is AggregationPath.validate for the aggregation orders of a
// bucket aggregation.
func validateOrders(d *aggDef, orders []bucketOrder) error {
	for _, o := range orders {
		if o.els == nil {
			continue
		}
		cur := d
		for i, el := range o.els {
			var sub *aggDef
			for _, s := range cur.subs {
				if s.name == el.name {
					sub = s
				}
			}
			if sub == nil {
				return errInvalidOrderPath(o.path, fmt.Sprintf("The provided aggregation [%s] either does not exist, or is a pipeline aggregation and cannot be used to sort the buckets.", el.name))
			}
			cur = sub
			if i == len(o.els)-1 {
				break
			}
			subPath := make([]string, i+1)
			for j := 0; j <= i; j++ {
				subPath[j] = o.els[j].fullName()
			}
			if sub.typ.card != cardOne {
				return errInvalidOrderPath(o.path, "Buckets can only be sorted on a sub-aggregator path that is built out of zero or more single-bucket aggregations within the path and a final single-bucket or a metrics aggregation at the path end. Sub-path ["+strings.Join(subPath, ">")+"] points to non single-bucket aggregation")
			}
			if el.hasKey {
				return errInvalidOrderPath(o.path, "Buckets can only be sorted on a sub-aggregator path that is built out of zero or more single-bucket aggregations within the path and a final single-bucket or a metrics aggregation at the path end. Keyed paths are only allowed at the path end. Sub-path ["+strings.Join(subPath, ">")+"] points to non single-bucket aggregation")
			}
		}
		last := o.els[len(o.els)-1]
		switch {
		case cur.typ.card == cardOne:
			if last.hasKey && last.key != "doc_count" {
				return errInvalidOrderPath(o.path, fmt.Sprintf("Ordering on a single-bucket aggregation can only be done on its doc_count. Either drop the key (a la \"%s\") or change it to \"doc_count\" (a la \"%s.doc_count\")", cur.name, cur.name))
			}
		case cur.typ.metric == metricSingle:
			if last.hasKey && last.key != "value" {
				return errInvalidOrderPath(o.path, fmt.Sprintf("Ordering on a single-value metrics aggregation can only be done on its value. Either drop the key (a la \"%s\") or change it to \"value\" (a la \"%s.value\")", cur.name, cur.name))
			}
		case cur.typ.metric == metricMulti:
			if !last.hasKey {
				return errInvalidOrderPath(o.path, "When ordering on a multi-value metrics aggregation a metric name must be specified.")
			}
			if cur.typ.hasMetric != nil && !cur.typ.hasMetric(cur, last.key) {
				return errInvalidOrderPath(o.path, fmt.Sprintf("Unknown metric name [%s] on multi-value metrics aggregation [%s]", last.key, cur.name))
			}
		default:
			return errInvalidOrderPath(o.path, "Buckets can only be sorted on a sub-aggregator path that is built out of zero or more single-bucket aggregations within the path and a final single-bucket or a metrics aggregation at the path end.")
		}
	}
	return nil
}

func (e aggPathElement) fullName() string {
	if e.hasKey {
		return e.name + "." + e.key
	}
	return e.name
}

// orderValue resolves an aggregation order path in a bucket.
func orderValue(b *bucket, els []aggPathElement) float64 {
	subs := b.subs
	var r *aggResult
	for i, el := range els {
		if r = findResult(subs, el.name); r == nil {
			return math.NaN()
		}
		if i < len(els)-1 {
			subs = r.subs
		}
	}
	switch r.kind {
	case resSingleBucket:
		return float64(r.docCount)
	case resValue:
		return r.value
	case resMultiValue:
		v, err := r.metric(els[len(els)-1].key)
		if err != nil {
			return math.NaN()
		}
		return v
	}
	return math.NaN()
}

func findResult(list []*aggResult, name string) *aggResult {
	for _, r := range list {
		if r.name == name {
			return r
		}
	}
	return nil
}

// compareDiscardNaN is Comparators.compareDiscardNaN: NaN sorts last in both
// directions.
func compareDiscardNaN(l, r float64, asc bool) int {
	cmp := javaDoubleCompare(l, r)
	if !asc {
		cmp = -cmp
	}
	if math.IsNaN(l) {
		if math.IsNaN(r) {
			return 0
		}
		return 1
	}
	if math.IsNaN(r) {
		return -1
	}
	return cmp
}

// javaDoubleCompare is Double.compare.
func javaDoubleCompare(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	ab, bb := int64(math.Float64bits(a)), int64(math.Float64bits(b))
	if math.IsNaN(a) {
		ab = 0x7ff8000000000000
	}
	if math.IsNaN(b) {
		bb = 0x7ff8000000000000
	}
	switch {
	case ab < bb:
		return -1
	case ab > bb:
		return 1
	}
	return 0
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareBuckets compares two buckets with a compound order.
func compareBuckets(a, b *bucket, orders []bucketOrder, keyCmp func(x, y *bucket) int) int {
	for _, o := range orders {
		var c int
		switch o.path {
		case "_count":
			c = cmpInt64(a.docCount, b.docCount)
			if !o.asc {
				c = -c
			}
		case "_key":
			c = keyCmp(a, b)
			if !o.asc {
				c = -c
			}
		default:
			c = compareDiscardNaN(orderValue(a, o.els), orderValue(b, o.els), o.asc)
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

func sortBucketList(buckets []*bucket, orders []bucketOrder, keyCmp func(x, y *bucket) int) {
	sort.SliceStable(buckets, func(i, j int) bool { return compareBuckets(buckets[i], buckets[j], orders, keyCmp) < 0 })
}

// orderUsesAggregations reports whether an order reads sub-aggregations.
func orderUsesAggregations(orders []bucketOrder) bool {
	for _, o := range orders {
		if o.els != nil {
			return true
		}
	}
	return false
}

// property resolution (InternalAggregation.getProperty) --------------------------

// invalidPathError is InvalidAggregationPathException.
type invalidPathError struct{ msg string }

func (e *invalidPathError) Error() string { return e.msg }

// javaList renders a list like List.toString.
func javaList(items []string) string { return "[" + strings.Join(items, ", ") + "]" }

func (r *aggResult) property(path []string) (any, error) {
	if len(path) == 0 {
		return r, nil
	}
	switch r.kind {
	case resBuckets:
		if path[0] == "_bucket_count" {
			return int64(len(r.buckets)), nil
		}
		out := make([]any, len(r.buckets))
		for i, b := range r.buckets {
			v, err := bucketProperty(r.name, b, path)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case resSingleBucket:
		if path[0] == "_count" {
			if len(path) > 1 {
				return nil, errIllegalArgument("_count must be the last element in the path")
			}
			return r.docCount, nil
		}
		sub := findResult(r.subs, path[0])
		if sub == nil {
			return nil, errIllegalArgument("Cannot find an aggregation named [%s] in [%s]", path[0], r.name)
		}
		return sub.property(path[1:])
	case resValue:
		if len(path) == 1 && path[0] == "value" {
			return r.value, nil
		}
	case resMultiValue:
		if len(path) == 1 {
			v, err := r.metric(path[0])
			if err != nil {
				return nil, err
			}
			return v, nil
		}
	}
	return nil, errIllegalArgument("path not supported for [%s]: %s", r.name, javaList(path))
}

func bucketProperty(containing string, b *bucket, path []string) (any, error) {
	if len(path) == 0 {
		return b, nil
	}
	switch path[0] {
	case "_count":
		if len(path) > 1 {
			return nil, &invalidPathError{"_count must be the last element in the path"}
		}
		return b.docCount, nil
	case "_key":
		if len(path) > 1 {
			return nil, &invalidPathError{"_key must be the last element in the path"}
		}
		if b.numeric {
			return b.keyNum, nil
		}
		return b.key, nil
	}
	sub := findResult(b.subs, path[0])
	if sub == nil {
		return nil, &invalidPathError{fmt.Sprintf("Cannot find an aggregation named [%s] in [%s]", path[0], containing)}
	}
	return sub.property(path[1:])
}

// resolveBucketValue is BucketHelpers.resolveBucketValue: null (ok=false)
// for invalid paths, NaN or zero for gaps.
func resolveBucketValue(agg *aggResult, b *bucket, path []string, insertZeros bool) (float64, bool, error) {
	v, err := bucketProperty(agg.name, b, path)
	if err != nil {
		if _, invalid := err.(*invalidPathError); invalid {
			return 0, false, nil
		}
		return 0, false, err
	}
	var value float64
	switch t := v.(type) {
	case nil:
		return 0, false, errAggExecution("buckets_path must reference either a number value or a single value numeric metric aggregation")
	case int64:
		value = float64(t)
	case float64:
		value = t
	case *aggResult:
		if t.kind != resValue {
			return 0, false, formatResolutionError(agg, path, v)
		}
		value = t.value
	default:
		return 0, false, formatResolutionError(agg, path, v)
	}
	isDocCount := len(path) == 1 && path[0] == "_count"
	if !finite(value) || (b.docCount == 0 && !isDocCount) {
		if insertZeros {
			return 0, true, nil
		}
		return math.NaN(), true, nil
	}
	return value, true, nil
}

func formatResolutionError(agg *aggResult, path []string, v any) *Error {
	name := agg.name
	var cur any = agg
	if len(path) > 0 {
		name, cur = path[0], v
	}
	if r, ok := cur.(*aggResult); ok && r.kind == resMultiValue {
		return errAggExecution("buckets_path must reference either a number value or a single value numeric metric aggregation, but [%s] contains multiple values. Please specify which to use.", name)
	}
	return errAggExecution("buckets_path must reference either a number value or a single value numeric metric aggregation, got: [%s] at aggregation [%s]", javaSimpleClass(v), name)
}

func javaSimpleClass(v any) string {
	switch t := v.(type) {
	case string:
		return "String"
	case []any:
		return "Object[]"
	case bool:
		return "Boolean"
	case *aggResult:
		if t.javaClass != "" {
			return t.javaClass
		}
		return "InternalAggregation"
	case *bucket:
		return "Bucket"
	case M:
		return "HashMap"
	}
	return "Object"
}

// reduceFailure reports an error raised while reducing aggregations.
func reduceFailure(err error) error {
	if e, ok := err.(*Error); ok && e.Type != "search_phase_execution_exception" {
		if e.Status == 0 {
			e.Status = http.StatusInternalServerError
		}
		return errReduce(e)
	}
	return err
}
