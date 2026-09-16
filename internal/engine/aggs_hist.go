package engine

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// histogram and date_histogram -------------------------------------------------------------

var histogramFields = valuesSourceFields("histogram", true, false, map[string]int{
	"interval": vtNumber, "offset": vtNumber, "order": vtObjectArray, "keyed": vtBool, "min_doc_count": vtNumber,
	"extended_bounds": vtObject, "hard_bounds": vtObject,
})

type doubleBounds struct {
	min, max       float64
	hasMin, hasMax bool
}

func (b *doubleBounds) String() string {
	s := ""
	if b.hasMin {
		s += javaDoubleToString(b.min)
	}
	s += "--"
	if b.hasMax {
		s += javaDoubleToString(b.max)
	}
	return s
}

type histogramSpec struct {
	vs             vsConfig
	interval       float64
	offset         float64
	keyed          bool
	minDoc         int64
	orders         []bucketOrder
	extended, hard *doubleBounds
}

func parseDoubleBounds(of objFields, key string, body M) (*doubleBounds, error) {
	raw, _ := body[key].(M)
	bf := objFields{name: "double_bounds", fields: map[string]int{"min": vtNumber, "max": vtNumber}}
	if err := bf.check(raw); err != nil {
		return nil, of.failed(body, key, err.(*Error))
	}
	b := &doubleBounds{}
	for _, k := range []string{"min", "max"} {
		if _, ok := raw[k]; !ok {
			continue
		}
		v, err := bf.doubleValue(raw, k)
		if err != nil {
			return nil, of.failed(body, key, err.(*Error))
		}
		if k == "min" {
			b.min, b.hasMin = v, true
		} else {
			b.max, b.hasMax = v, true
		}
	}
	if b.hasMin && b.hasMax && b.min > b.max {
		last := "max"
		inner := &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception",
			Reason: "Cannot instantiate an object of org.opensearch.search.aggregations.bucket.histogram.DoubleBounds",
			Cause: &Error{Type: "invocation_target_exception", Extra: map[string]any{"reason": nil}, plain: true,
				Cause: errIllegalArgument("max bound [%s] must be greater than min bound [%s]", javaDoubleToString(b.max), javaDoubleToString(b.min))}}
		return nil, of.failed(body, key, bf.failed(raw, last, errXContent(inner, "Failed to build [double_bounds] after last required field arrived")))
	}
	return b, nil
}

func parseHistogram(ps *aggParser, d *aggDef) error {
	of := histogramFields
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &histogramSpec{}
	var err error
	if spec.vs, err = parseVSConfig(of, body); err != nil {
		return err
	}
	if _, ok := body["interval"]; ok {
		if spec.interval, err = of.doubleValue(body, "interval"); err != nil {
			return err
		}
		if spec.interval <= 0 {
			return of.failed(body, "interval", errIllegalArgument("[interval] must be >0 for histogram aggregation [%s]", d.name))
		}
	}
	if _, ok := body["offset"]; ok {
		if spec.offset, err = of.doubleValue(body, "offset"); err != nil {
			return err
		}
	}
	if _, ok := body["keyed"]; ok {
		if spec.keyed, err = of.boolValue(body, "keyed"); err != nil {
			return err
		}
	}
	if err := parseThresholds(of, d, new(int), nil, &spec.minDoc, nil); err != nil {
		return err
	}
	orders, err := parseBucketOrders(of, body, "order")
	if err != nil {
		return err
	}
	spec.orders = withKeyTiebreak(orders, []bucketOrder{{path: "_key", asc: true}})
	if _, ok := body["extended_bounds"]; ok {
		if spec.extended, err = parseDoubleBounds(of, "extended_bounds", body); err != nil {
			return err
		}
	}
	if _, ok := body["hard_bounds"]; ok {
		if spec.hard, err = parseDoubleBounds(of, "hard_bounds", body); err != nil {
			return err
		}
	}
	if err := requireFieldOrScript(body); err != nil {
		return err
	}
	if spec.vs.script {
		return errScript(d)
	}
	d.spec = spec
	return nil
}

func prepareHistogram(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*histogramSpec)
	if _, err := pc.resolve(d, 0, &spec.vs, "histogram", vsNumeric, vsNumeric, vsDate, vsBoolean, vsRange); err != nil {
		return err
	}
	if h, e := spec.hard, spec.extended; h != nil && e != nil {
		if (h.hasMax && e.hasMax && h.max < e.max) || (h.hasMin && e.hasMin && h.min > e.min) {
			return errIllegalArgument("Extended bounds have to be inside hard bounds, hard bounds: [%s], extended bounds: [%s]", h, e)
		}
	}
	if spec.interval <= 0 {
		return errIllegalArgument("interval must be positive, got: %s", javaDoubleToString(spec.interval))
	}
	return validateOrders(d, spec.orders)
}

func collectHistogram(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*histogramSpec)
	interval, offset := spec.interval, spec.offset
	// bucket index i holds the values with floor((v-offset)/interval) == i;
	// its key is keyOf(i)
	keyOf := func(i float64) float64 { return float64(i*interval) + offset }
	indexOf := func(v float64) float64 { return math.Floor((v - offset) / interval) }
	type histEntry struct {
		idx float64
		b   *bucket
	}
	groups := map[float64]*histEntry{}
	for _, h := range hits {
		vs := ac.source(d, 0, h.ix)
		if vs == nil {
			continue
		}
		prev := math.NaN()
		for _, v := range vs.nums(h) {
			key := indexOf(v)
			if key == prev {
				continue
			}
			prev = key
			if hb := spec.hard; hb != nil && ((hb.hasMax && float64(key*interval) > hb.max) || (hb.hasMin && float64(key*interval) < hb.min)) {
				continue
			}
			e, ok := groups[key]
			if !ok {
				k := keyOf(key)
				e = &histEntry{idx: key, b: &bucket{keyNum: k, numeric: true, sortKey: k}}
				groups[key] = e
				if err := ac.checkBuckets(len(groups)); err != nil {
					return nil, err
				}
			}
			e.b.docCount++
			e.b.hits = append(e.b.hits, h)
		}
	}
	entries := make([]*histEntry, 0, len(groups))
	for _, e := range groups {
		if e.b.docCount >= spec.minDoc {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].idx < entries[j].idx })
	buckets := make([]*bucket, 0, len(entries))
	if spec.minDoc == 0 {
		// InternalHistogram.addEmptyBuckets: empty buckets are added by
		// bucket index, one at a time against search.max_buckets, so a
		// span that is too wide (or a key that no longer advances because
		// the interval is below the precision of the keys) fails with
		// too_many_buckets instead of looping forever
		minBound, maxBound := math.Inf(1), math.Inf(-1)
		if e := spec.extended; e != nil {
			if e.hasMin {
				minBound = e.min
			}
			if e.hasMax {
				maxBound = e.max
			}
		}
		add := func(k float64) error {
			if n := len(buckets); n > 0 && !(k > buckets[n-1].keyNum) {
				return errTooManyBuckets(ac.bucketLimit()+1, ac.bucketLimit())
			}
			if err := ac.checkBuckets(len(buckets) + 1); err != nil {
				return err
			}
			buckets = append(buckets, &bucket{keyNum: k, numeric: true, sortKey: k})
			return nil
		}
		fill := func(from float64, within func(k float64) bool) error {
			for i := from; ; i++ {
				k := keyOf(i)
				if !within(k) {
					return nil
				}
				if err := add(k); err != nil {
					return err
				}
			}
		}
		if len(entries) == 0 {
			if err := fill(indexOf(minBound), func(k float64) bool { return k <= maxBound }); err != nil {
				return nil, err
			}
		} else {
			first := entries[0].b.keyNum
			if err := fill(indexOf(minBound), func(k float64) bool { return k < first }); err != nil {
				return nil, err
			}
			for i, e := range entries {
				if i > 0 {
					upTo := e.b.keyNum
					if err := fill(entries[i-1].idx+1, func(k float64) bool { return k < upTo }); err != nil {
						return nil, err
					}
				}
				if err := ac.checkBuckets(len(buckets) + 1); err != nil {
					return nil, err
				}
				buckets = append(buckets, e.b)
			}
			if err := fill(entries[len(entries)-1].idx+1, func(k float64) bool { return k <= maxBound }); err != nil {
				return nil, err
			}
		}
	} else {
		for _, e := range entries {
			buckets = append(buckets, e.b)
		}
	}
	if err := ac.collectSubs(d, buckets); err != nil {
		return nil, err
	}
	orderHistogram(buckets, spec.orders)
	format := ac.firstFormat(d, 0)
	for _, b := range buckets {
		b.key = b.keyNum
		b.keyString = format.stringDouble(b.keyNum)
		b.asString = !format.raw()
		b.keyedName = b.keyString
	}
	return &aggResult{kind: resBuckets, buckets: buckets, keyed: spec.keyed, javaClass: "InternalHistogram"}, nil
}

// orderHistogram applies the order of a histogram after its buckets are in
// key order.
func orderHistogram(buckets []*bucket, orders []bucketOrder) {
	if len(orders) >= 1 && orders[0].path == "_key" {
		if !orders[0].asc {
			for i, j := 0, len(buckets)-1; i < j; i, j = i+1, j-1 {
				buckets[i], buckets[j] = buckets[j], buckets[i]
			}
		}
		return
	}
	sortBucketList(buckets, orders, func(a, b *bucket) int { return javaDoubleCompare(a.keyNum, b.keyNum) })
}

// date_histogram ------------------------------------------------------------------------------------

var dateHistogramFields = valuesSourceFields("date_histogram", true, true, map[string]int{
	"calendar_interval": vtString, "fixed_interval": vtString, "interval": vtNumber, "offset": vtNumber, "order": vtObjectArray,
	"keyed": vtBool, "min_doc_count": vtNumber, "extended_bounds": vtObject, "hard_bounds": vtObject,
})

type longBound struct {
	value    int64
	str      string
	isStr    bool
	present  bool
	resolved int64
}

type longBounds struct{ min, max longBound }

func (b *longBounds) String() string {
	s := ""
	if b.min.present {
		if b.min.isStr {
			s += b.min.str
		} else {
			s += strconv.FormatInt(b.min.value, 10)
		}
	}
	s += "--"
	if b.max.present {
		if b.max.isStr {
			s += b.max.str
		} else {
			s += strconv.FormatInt(b.max.value, 10)
		}
	}
	return s
}

type dateHistogramSpec struct {
	vs             vsConfig
	unit           int
	fixed          int64
	hasInterval    bool
	offset         int64
	keyed          bool
	minDoc         int64
	orders         []bucketOrder
	extended, hard *longBounds
}

func parseLongBounds(of objFields, key string, body M) (*longBounds, error) {
	raw, _ := body[key].(M)
	bf := objFields{name: "bounds", fields: map[string]int{"min": vtNumber, "max": vtNumber}}
	if err := bf.check(raw); err != nil {
		return nil, of.failed(body, key, err.(*Error))
	}
	b := &longBounds{}
	for _, k := range []string{"min", "max"} {
		v, ok := raw[k]
		if !ok {
			continue
		}
		lb := longBound{present: true}
		if s, isStr := v.(string); isStr {
			lb.str, lb.isStr = s, true
		} else {
			n, err := bf.longValue(raw, k)
			if err != nil {
				return nil, of.failed(body, key, err.(*Error))
			}
			lb.value = n
		}
		if k == "min" {
			b.min = lb
		} else {
			b.max = lb
		}
	}
	return b, nil
}

func parseDateHistogram(ps *aggParser, d *aggDef) error {
	return parseDateHistogramInto(dateHistogramFields, d, true)
}

// parseDateHistogramInto parses a date_histogram body (also the
// date_histogram source of composite, which has no bounds or ordering).
func parseDateHistogramInto(of objFields, d *aggDef, full bool) error {
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &dateHistogramSpec{}
	var err error
	if spec.vs, err = parseVSConfig(of, body); err != nil {
		return err
	}
	_, hasCal := body["calendar_interval"]
	_, hasFixed := body["fixed_interval"]
	_, hasLegacy := body["interval"]
	if hasCal {
		s := body["calendar_interval"].(string)
		unit, ok := calendarUnits[s]
		if !ok {
			return of.failed(body, "calendar_interval", errIllegalArgument("The supplied interval [%s] could not be parsed as a calendar interval.", s))
		}
		spec.unit, spec.hasInterval = unit, true
	}
	if hasFixed {
		if hasCal {
			return of.failed(body, "fixed_interval", errIllegalArgument("Cannot use [fixed_interval] with [calendar_interval] configuration option."))
		}
		ms, perr := parseTimeValueMillis(body["fixed_interval"].(string), "date_histogram.fixedInterval")
		if perr != nil {
			return of.failed(body, "fixed_interval", perr)
		}
		spec.fixed, spec.hasInterval = ms, true
	}
	if hasLegacy {
		if hasCal || hasFixed {
			return of.failed(body, "interval", errIllegalArgument("Cannot use [interval] with [fixed_interval] or [calendar_interval] configuration options."))
		}
		switch t := body["interval"].(type) {
		case string:
			if unit, ok := calendarUnits[t]; ok {
				spec.unit = unit
			} else {
				ms, perr := parseTimeValueMillis(t, "date_histogram.interval")
				if perr != nil {
					return of.failed(body, "interval", perr)
				}
				spec.fixed = ms
			}
		default:
			n, err := of.longValue(body, "interval")
			if err != nil {
				return err
			}
			spec.fixed = n
		}
		spec.hasInterval = true
	}
	switch t := body["offset"].(type) {
	case string:
		s := t
		neg := false
		if len(s) > 0 && s[0] == '-' {
			neg, s = true, s[1:]
		} else if len(s) > 0 && s[0] == '+' {
			s = s[1:]
		}
		ms, perr := parseTimeValueMillis(s, "DateHistogramAggregationBuilder.parseOffset")
		if perr != nil {
			return of.failed(body, "offset", perr)
		}
		if neg {
			ms = -ms
		}
		spec.offset = ms
	case nil:
	default:
		if spec.offset, err = of.longValue(body, "offset"); err != nil {
			return err
		}
	}
	if _, ok := body["keyed"]; ok {
		if spec.keyed, err = of.boolValue(body, "keyed"); err != nil {
			return err
		}
	}
	if err := parseThresholds(of, d, new(int), nil, &spec.minDoc, nil); err != nil {
		return err
	}
	orders, err := parseBucketOrders(of, body, "order")
	if err != nil {
		return err
	}
	spec.orders = withKeyTiebreak(orders, []bucketOrder{{path: "_key", asc: true}})
	if _, ok := body["extended_bounds"]; ok {
		if spec.extended, err = parseLongBounds(of, "extended_bounds", body); err != nil {
			return err
		}
	}
	if _, ok := body["hard_bounds"]; ok {
		if spec.hard, err = parseLongBounds(of, "hard_bounds", body); err != nil {
			return err
		}
	}
	if full {
		if err := requireFieldOrScript(body); err != nil {
			return err
		}
	}
	if spec.vs.script {
		return errScript(d)
	}
	d.spec = spec
	return nil
}

func (spec *dateHistogramSpec) rounding() *dateRounding {
	loc := time.UTC
	if spec.vs.hasZone {
		loc = spec.vs.zone
	}
	return &dateRounding{unit: spec.unit, interval: spec.fixed, loc: loc, offset: spec.offset}
}

// resolveLongBounds is LongBounds.parseAndValidate followed by round.
func resolveLongBounds(b *longBounds, name, agg string, vs *valuesSource, r *dateRounding, now time.Time) (*longBounds, error) {
	if b == nil {
		return nil, nil
	}
	out := *b
	for _, lb := range []*longBound{&out.min, &out.max} {
		if !lb.present {
			continue
		}
		v := lb.value
		if lb.isStr {
			n, err := parseLongWithFormat(vs, lb.str, now)
			if err != nil {
				return nil, err
			}
			v = n
		}
		lb.resolved = v
	}
	if out.min.present && out.max.present && out.min.resolved > out.max.resolved {
		return nil, errIllegalArgument("[%s.min][%d] cannot be greater than [%s.max][%d] for histogram aggregation [%s]", name, out.min.resolved, name, out.max.resolved, agg)
	}
	noOffset := *r
	noOffset.offset = 0
	for _, lb := range []*longBound{&out.min, &out.max} {
		if lb.present {
			lb.resolved = noOffset.round(lb.resolved)
		}
	}
	return &out, nil
}

// parseLongWithFormat is DocValueFormat.parseLong of a values source.
func parseLongWithFormat(vs *valuesSource, s string, now time.Time) (int64, error) {
	switch vs.format.kind {
	case fmtDate:
		t, err := ParseDateMath(s, vs.format.date, now, vs.format.loc, false)
		if err != nil {
			return 0, errDateParse(s, vs.format.date)
		}
		return t.UnixMilli(), nil
	case fmtBool:
		switch s {
		case "true":
			return 1, nil
		case "false":
			return 0, nil
		}
		return 0, errIllegalArgument("Cannot parse boolean [%s], expected either [true] or [false]", s)
	}
	f, err := aggParseDouble(s, func(s string) *Error { return aggNumberFormatError(s) })
	if err != nil {
		return 0, err
	}
	return int64(javaRound(math.Floor(f))), nil
}

type dateHistogramShard struct {
	extended, hard *longBounds
}

func prepareDateHistogram(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*dateHistogramSpec)
	vs, err := pc.resolve(d, 0, &spec.vs, "date_histogram", vsDate, vsDate, vsNumeric, vsBoolean, vsRange)
	if err != nil {
		return err
	}
	if !spec.hasInterval {
		return errIllegalArgument("Invalid interval specified, must be non-null and non-empty")
	}
	if spec.unit == 0 && spec.fixed <= 0 {
		return errIllegalArgument("Zero or negative time interval not supported")
	}
	r := spec.rounding()
	shard := &dateHistogramShard{}
	if shard.extended, err = resolveLongBounds(spec.extended, "extended_bounds", d.name, vs, r, pc.ac.now); err != nil {
		return err
	}
	if shard.hard, err = resolveLongBounds(spec.hard, "hard_bounds", d.name, vs, r, pc.ac.now); err != nil {
		return err
	}
	if e, h := shard.extended, shard.hard; e != nil && h != nil {
		if (e.max.present && h.max.present && e.max.resolved > h.max.resolved) || (e.min.present && h.min.present && e.min.resolved < h.min.resolved) {
			return errIllegalArgument("Extended bounds have to be inside hard bounds, hard bounds: [%s], extended bounds: [%s]", spec.hard, spec.extended)
		}
	}
	if h := shard.hard; h != nil && (!h.min.present || !h.max.present) {
		missing := "getMax"
		if !h.min.present {
			missing = "getMin"
		}
		return errJava(http.StatusInternalServerError, "null_pointer_exception", "Cannot invoke \"java.lang.Long.longValue()\" because the return value of \"org.opensearch.search.aggregations.bucket.histogram.LongBounds."+missing+"()\" is null")
	}
	pc.ac.setAux(d, 0, pc.ix, shard)
	return validateOrders(d, spec.orders)
}

func collectDateHistogram(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*dateHistogramSpec)
	r := spec.rounding()
	var shard *dateHistogramShard
	for _, ix := range ac.indices {
		if s, ok := ac.aux(d, 0, ix).(*dateHistogramShard); ok {
			shard = s
			break
		}
	}
	groups := map[int64]*bucket{}
	for _, h := range hits {
		vs := ac.source(d, 0, h.ix)
		if vs == nil {
			continue
		}
		var hard *longBounds
		if s, ok := ac.aux(d, 0, h.ix).(*dateHistogramShard); ok {
			hard = s.hard
		}
		prev := int64(math.MinInt64)
		first := true
		for _, v := range vs.nums(h) {
			key := r.round(int64(v))
			if !first && key == prev {
				continue
			}
			first, prev = false, key
			if hard != nil && (key >= hard.max.resolved || key < hard.min.resolved) {
				continue
			}
			b, ok := groups[key]
			if !ok {
				b = &bucket{keyNum: float64(key), numeric: true, sortKey: float64(key)}
				groups[key] = b
				if err := ac.checkBuckets(len(groups)); err != nil {
					return nil, err
				}
			}
			b.docCount++
			b.hits = append(b.hits, h)
		}
	}
	buckets := make([]*bucket, 0, len(groups))
	for _, b := range groups {
		if b.docCount >= spec.minDoc {
			buckets = append(buckets, b)
		}
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].keyNum < buckets[j].keyNum })
	if spec.minDoc == 0 {
		var bounds *longBounds
		if shard != nil {
			bounds = shard.extended
		}
		filled, err := ac.fillDateHistogram(buckets, bounds, r)
		if err != nil {
			return nil, err
		}
		buckets = filled
	}
	if err := ac.collectSubs(d, buckets); err != nil {
		return nil, err
	}
	orderHistogram(buckets, spec.orders)
	format := ac.firstFormat(d, 0)
	for _, b := range buckets {
		k := int64(b.keyNum)
		b.key = k
		b.keyString = format.stringLong(k)
		b.asString = !format.raw()
		b.keyedName = b.keyString
	}
	return &aggResult{kind: resBuckets, buckets: buckets, keyed: spec.keyed, javaClass: "InternalDateHistogram"}, nil
}

// fillDateHistogram is InternalDateHistogram.addEmptyBuckets.
func (ac *aggContext) fillDateHistogram(buckets []*bucket, bounds *longBounds, r *dateRounding) ([]*bucket, error) {
	offset := r.offset
	noOffset := *r
	noOffset.offset = 0
	next := func(k int64) int64 { return noOffset.next(k-offset) + offset }
	out := make([]*bucket, 0, len(buckets))
	// every bucket, empty or not, counts against search.max_buckets as it
	// is added (MultiBucketConsumer), so a span too wide for the limit
	// fails before it is materialized
	add := func(k int64) error {
		if err := ac.checkBuckets(len(out) + 1); err != nil {
			return err
		}
		out = append(out, &bucket{keyNum: float64(k), numeric: true, sortKey: float64(k)})
		return nil
	}
	if bounds != nil {
		if len(buckets) == 0 {
			if bounds.min.present && bounds.max.present {
				max := bounds.max.resolved + offset
				for k := bounds.min.resolved + offset; k <= max; k = next(k) {
					if err := add(k); err != nil {
						return nil, err
					}
				}
			}
		} else if bounds.min.present {
			first := int64(buckets[0].keyNum)
			for k := bounds.min.resolved + offset; k < first; k = next(k) {
				if err := add(k); err != nil {
					return nil, err
				}
			}
		}
	}
	for i, b := range buckets {
		if i > 0 {
			for k := next(int64(buckets[i-1].keyNum)); k < int64(b.keyNum); k = next(k) {
				if err := add(k); err != nil {
					return nil, err
				}
			}
		}
		if err := ac.checkBuckets(len(out) + 1); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if bounds != nil && len(buckets) > 0 && bounds.max.present {
		last := int64(buckets[len(buckets)-1].keyNum)
		if max := bounds.max.resolved + offset; max > last {
			for k := next(last); k <= max; k = next(k) {
				if err := add(k); err != nil {
					return nil, err
				}
			}
		}
	}
	return out, nil
}
