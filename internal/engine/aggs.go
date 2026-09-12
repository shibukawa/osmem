package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type aggContext struct {
	c   *Cluster
	all func() []*hit
}

type bucket struct {
	key         any
	keyAsString string
	docCount    int
	hits        []*hit
	sub         M
	sortKey     any // internal comparable key
}

func (c *Cluster) runAggregations(aggs M, hits []*hit, all func() []*hit) (M, error) {
	ac := &aggContext{c: c, all: all}
	return ac.run(aggs, hits)
}

var pipelineAggs = map[string]bool{
	"bucket_sort": true, "cumulative_sum": true, "derivative": true, "avg_bucket": true, "sum_bucket": true, "min_bucket": true,
	"max_bucket": true, "stats_bucket": true, "extended_stats_bucket": true, "percentiles_bucket": true, "bucket_script": true,
	"bucket_selector": true, "moving_avg": true, "moving_fn": true, "serial_diff": true, "cumulative_cardinality": true,
}

func (ac *aggContext) run(aggs M, hits []*hit) (M, error) {
	out := M{}
	// regular aggregations first, pipelines afterwards (they read results)
	names := make([]string, 0, len(aggs))
	for name := range aggs {
		names = append(names, name)
	}
	sort.Strings(names)
	var pipelines []string
	for _, name := range names {
		spec, ok := aggs[name].(M)
		if !ok {
			return nil, errParsing("Aggregation definition for [%s] must be an object", name)
		}
		kind, _, _, err := splitAggSpec(spec)
		if err != nil {
			return nil, err
		}
		if pipelineAggs[kind] {
			if parentPipelineAggs[kind] {
				continue // applied by the enclosing bucket aggregation
			}
			pipelines = append(pipelines, name)
			continue
		}
		res, err := ac.runOne(name, spec, hits)
		if err != nil {
			return nil, err
		}
		out[name] = res
	}
	for _, name := range pipelines {
		spec := aggs[name].(M)
		if err := ac.runPipeline(name, spec, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// splitAggSpec separates the aggregation type from sub aggregations.
func splitAggSpec(spec M) (kind string, body M, sub M, err error) {
	for k, v := range spec {
		switch k {
		case "aggs", "aggregations":
			sm, ok := v.(M)
			if !ok {
				return "", nil, nil, errParsing("[aggregations] must be an object")
			}
			if sub == nil {
				sub = M{}
			}
			for sk, sv := range sm {
				sub[sk] = sv
			}
		case "meta":
		default:
			if kind != "" {
				return "", nil, nil, errParsing("Found two aggregation type definitions in [%s]: [%s] and [%s]", "aggs", kind, k)
			}
			kind = k
			body, _ = v.(M)
			if body == nil {
				body = M{}
			}
		}
	}
	if kind == "" {
		return "", nil, nil, errParsing("Missing definition for aggregation")
	}
	return kind, body, sub, nil
}

func errAggField(h *hit, field string) error {
	return errSearchPhase(&Error{Status: 400, Type: "illegal_argument_exception", Reason: "Text fields are not optimised for operations that require per-document field data like aggregations and sorting, so these operations are disabled by default. Please use a keyword field instead. Alternatively, set fielddata=true on [" + field + "] in order to load field data by uninverting the inverted index. Note that this can use significant memory.", Index: h.ix.Name})
}

// values returns the typed values of a field for a hit, with the missing
// substitution applied.
func (ac *aggContext) values(h *hit, field string, missing any) ([]any, *Field, error) {
	f, _, ok := h.ix.Mapping.resolve(field)
	if field == "_id" || field == "_index" {
		return h.ix.fieldValues(h.doc, field), &Field{Type: TypeKeyword}, nil
	}
	if !ok {
		if missing != nil {
			if n, isNum := missing.(json.Number); isNum {
				if f, err := n.Float64(); err == nil {
					return []any{f}, nil, nil
				}
			}
			return []any{missing}, nil, nil
		}
		return nil, nil, nil
	}
	if f.Type == TypeText {
		return nil, f, errAggField(h, field)
	}
	vals := h.ix.fieldValues(h.doc, field)
	if len(vals) == 0 && missing != nil {
		if cv, ok := convertValue(f, missing); ok {
			vals = []any{cv}
		}
	}
	return vals, f, nil
}

func (ac *aggContext) numbers(h *hit, field string, missing any) ([]float64, *Field, error) {
	vals, f, err := ac.values(h, field, missing)
	if err != nil {
		return nil, f, err
	}
	out := make([]float64, 0, len(vals))
	for _, v := range vals {
		switch t := v.(type) {
		case float64:
			out = append(out, t)
		case time.Time:
			out = append(out, float64(t.UnixMilli()))
		case bool:
			if t {
				out = append(out, 1)
			} else {
				out = append(out, 0)
			}
		case string:
			if n, err := strconv.ParseFloat(t, 64); err == nil {
				out = append(out, n)
			}
		}
	}
	return out, f, nil
}

var parentPipelineAggs = map[string]bool{"bucket_sort": true, "cumulative_sum": true, "derivative": true}

func (ac *aggContext) runOne(name string, spec M, hits []*hit) (any, error) {
	res, err := ac.runOneInner(name, spec, hits)
	if err != nil {
		return nil, err
	}
	_, _, sub, _ := splitAggSpec(spec)
	if err := ac.applyParentPipelines(sub, res); err != nil {
		return nil, err
	}
	return res, nil
}

func (ac *aggContext) runOneInner(name string, spec M, hits []*hit) (any, error) {
	kind, body, sub, err := splitAggSpec(spec)
	if err != nil {
		return nil, err
	}
	if _, ok := body["script"]; ok {
		return nil, errUnsupported("aggregation [" + name + "] with script")
	}
	switch kind {
	case "terms":
		return ac.terms(body, sub, hits)
	case "multi_terms":
		return ac.multiTerms(body, sub, hits)
	case "range":
		return ac.rangeAgg(body, sub, hits, false)
	case "date_range":
		return ac.rangeAgg(body, sub, hits, true)
	case "histogram":
		return ac.histogram(body, sub, hits)
	case "date_histogram":
		return ac.dateHistogram(body, sub, hits)
	case "filter":
		return ac.filter(body, sub, hits)
	case "filters":
		return ac.filters(body, sub, hits)
	case "missing":
		return ac.missingAgg(body, sub, hits)
	case "global":
		return ac.single(sub, ac.all())
	case "nested", "reverse_nested", "sampler", "diversified_sampler", "children", "parent":
		return ac.single(sub, hits)
	case "composite":
		return ac.composite(body, sub, hits)
	case "avg", "sum", "min", "max", "value_count":
		return ac.simpleMetric(kind, body, hits)
	case "stats", "extended_stats":
		return ac.stats(kind, body, hits)
	case "cardinality":
		return ac.cardinality(body, hits)
	case "percentiles":
		return ac.percentiles(body, hits)
	case "percentile_ranks":
		return ac.percentileRanks(body, hits)
	case "top_hits":
		return ac.topHits(body, hits)
	case "weighted_avg":
		return ac.weightedAvg(body, hits)
	case "median_absolute_deviation":
		return ac.medianAbsoluteDeviation(body, hits)
	case "significant_terms", "significant_text", "geo_distance", "geohash_grid", "geotile_grid", "geohex_grid", "geo_bounds", "geo_centroid",
		"matrix_stats", "scripted_metric", "string_stats", "rare_terms", "auto_date_histogram", "variable_width_histogram", "ip_range", "adjacency_matrix", "date_range_nanos", "top_metrics":
		return nil, errUnsupported("[" + kind + "] aggregation")
	}
	return nil, errParsing("Unknown aggregation type [%s]", kind)
}

func (ac *aggContext) single(sub M, hits []*hit) (any, error) {
	out := M{"doc_count": len(hits)}
	if len(sub) > 0 {
		res, err := ac.run(sub, hits)
		if err != nil {
			return nil, err
		}
		for k, v := range res {
			out[k] = v
		}
	}
	return out, nil
}

func (ac *aggContext) fillSub(buckets []*bucket, sub M) error {
	if len(sub) == 0 {
		return nil
	}
	for _, b := range buckets {
		res, err := ac.run(sub, b.hits)
		if err != nil {
			return err
		}
		b.sub = res
	}
	return nil
}

func bucketJSON(b *bucket, keyed bool) M {
	out := M{"doc_count": b.docCount}
	if !keyed || b.key != nil {
		if !keyed {
			out["key"] = b.key
		}
		if b.keyAsString != "" {
			out["key_as_string"] = b.keyAsString
		}
	}
	for k, v := range b.sub {
		out[k] = v
	}
	return out
}

// keyOf converts a value into a bucket key and comparable sort key.
func keyOf(f *Field, v any) (key any, sortKey any, asString string) {
	switch t := v.(type) {
	case string:
		return t, t, ""
	case float64:
		if f != nil && f.isIntegral() {
			return int64(t), t, ""
		}
		return numberValue(t), t, ""
	case bool:
		if t {
			return int64(1), 1.0, "true"
		}
		return int64(0), 0.0, "false"
	case time.Time:
		df := ParseDateFormat(DefaultDateFormat)
		if f != nil && f.Format != nil {
			df = f.Format
		}
		return t.UnixMilli(), float64(t.UnixMilli()), df.Format(t.UTC())
	case [2]float64:
		s := fmt.Sprintf("%v,%v", t[0], t[1])
		return s, s, ""
	}
	s := fmt.Sprint(v)
	return s, s, ""
}

// terms aggregation --------------------------------------------------

type termsOrder struct {
	path string // "_count", "_key" or sub aggregation path
	desc bool
}

func parseOrder(v any) []termsOrder {
	var out []termsOrder
	for _, item := range getList(v) {
		m, ok := item.(M)
		if !ok {
			continue
		}
		for k, dir := range m {
			out = append(out, termsOrder{path: k, desc: strings.EqualFold(fmt.Sprint(dir), "desc")})
		}
	}
	return out
}

func subMetricValue(sub M, path string) (float64, bool) {
	parts := strings.SplitN(path, ".", 2)
	agg, ok := sub[parts[0]].(M)
	if !ok {
		return 0, false
	}
	if len(parts) == 2 {
		v, ok := toFloat(agg[parts[1]])
		return v, ok
	}
	if v, ok := toFloat(agg["value"]); ok {
		return v, ok
	}
	if v, ok := toFloat(agg["doc_count"]); ok {
		return v, ok
	}
	return 0, false
}

func sortBuckets(buckets []*bucket, orders []termsOrder) {
	if len(orders) == 0 {
		orders = []termsOrder{{path: "_count", desc: true}, {path: "_key"}}
	} else if orders[len(orders)-1].path != "_key" {
		orders = append(orders, termsOrder{path: "_key"})
	}
	sort.SliceStable(buckets, func(i, j int) bool {
		a, b := buckets[i], buckets[j]
		for _, o := range orders {
			var cmp int
			switch o.path {
			case "_count":
				cmp = compareValues(float64(a.docCount), float64(b.docCount))
			case "_key", "_term":
				cmp = compareValues(a.sortKey, b.sortKey)
			default:
				av, _ := subMetricValue(a.sub, o.path)
				bv, _ := subMetricValue(b.sub, o.path)
				cmp = compareValues(av, bv)
			}
			if cmp != 0 {
				if o.desc {
					return cmp > 0
				}
				return cmp < 0
			}
		}
		return false
	})
}

func (ac *aggContext) terms(body M, sub M, hits []*hit) (any, error) {
	field := getString(body, "field")
	if field == "" {
		return nil, errParsing("[terms] aggregation requires a field")
	}
	size := getInt(body, "size", 10)
	minDoc := getInt(body, "min_doc_count", 1)
	missing := body["missing"]
	includes, excludes, incRe, excRe := parseIncludeExclude(body)
	groups := map[string]*bucket{}
	var order []string
	for _, h := range hits {
		vals, f, err := ac.values(h, field, missing)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, v := range vals {
			key, sk, ks := keyOf(f, v)
			id := fmt.Sprintf("%T:%v", key, key)
			if seen[id] {
				continue
			}
			seen[id] = true
			strKey := fmt.Sprint(key)
			if ks != "" && f != nil && f.Type == TypeBoolean {
				strKey = ks
			}
			if !includeKey(strKey, includes, excludes, incRe, excRe) {
				continue
			}
			b, ok := groups[id]
			if !ok {
				b = &bucket{key: key, sortKey: sk, keyAsString: ks}
				groups[id] = b
				order = append(order, id)
			}
			b.docCount++
			b.hits = append(b.hits, h)
		}
	}
	buckets := make([]*bucket, 0, len(groups))
	for _, id := range order {
		b := groups[id]
		if b.docCount >= minDoc {
			buckets = append(buckets, b)
		}
	}
	if err := ac.fillSub(buckets, sub); err != nil {
		return nil, err
	}
	sortBuckets(buckets, parseOrder(body["order"]))
	other := 0
	if len(buckets) > size {
		for _, b := range buckets[size:] {
			other += b.docCount
		}
		buckets = buckets[:size]
	}
	list := make([]any, 0, len(buckets))
	for _, b := range buckets {
		list = append(list, bucketJSON(b, false))
	}
	return M{"doc_count_error_upper_bound": 0, "sum_other_doc_count": other, "buckets": list}, nil
}

func parseIncludeExclude(body M) (includes, excludes map[string]bool, incRe, excRe *regexp.Regexp) {
	parse := func(v any) (map[string]bool, *regexp.Regexp) {
		switch t := v.(type) {
		case string:
			re, err := regexp.Compile("^(?:" + t + ")$")
			if err != nil {
				return nil, nil
			}
			return nil, re
		case []any:
			set := map[string]bool{}
			for _, e := range t {
				set[fmt.Sprint(numberValue(e))] = true
			}
			return set, nil
		}
		return nil, nil
	}
	includes, incRe = parse(body["include"])
	excludes, excRe = parse(body["exclude"])
	return
}

func includeKey(key string, includes, excludes map[string]bool, incRe, excRe *regexp.Regexp) bool {
	if includes != nil && !includes[key] {
		return false
	}
	if incRe != nil && !incRe.MatchString(key) {
		return false
	}
	if excludes != nil && excludes[key] {
		return false
	}
	if excRe != nil && excRe.MatchString(key) {
		return false
	}
	return true
}

func (ac *aggContext) multiTerms(body M, sub M, hits []*hit) (any, error) {
	termSpecs := getList(body["terms"])
	if len(termSpecs) == 0 {
		return nil, errParsing("[multi_terms] aggregation requires terms")
	}
	size := getInt(body, "size", 10)
	minDoc := getInt(body, "min_doc_count", 1)
	groups := map[string]*bucket{}
	var order []string
	for _, h := range hits {
		combos := [][]any{{}}
		strs := [][]string{{}}
		skip := false
		for _, ts := range termSpecs {
			tm, _ := ts.(M)
			vals, f, err := ac.values(h, getString(tm, "field"), tm["missing"])
			if err != nil {
				return nil, err
			}
			if len(vals) == 0 {
				skip = true
				break
			}
			var nc [][]any
			var ns [][]string
			for i, combo := range combos {
				for _, v := range vals {
					key, _, ks := keyOf(f, v)
					nc = append(nc, append(append([]any{}, combo...), key))
					s := fmt.Sprint(key)
					if ks != "" {
						s = ks
					}
					ns = append(ns, append(append([]string{}, strs[i]...), s))
				}
			}
			combos, strs = nc, ns
		}
		if skip {
			continue
		}
		for i, combo := range combos {
			id := strings.Join(strs[i], "|")
			b, ok := groups[id]
			if !ok {
				b = &bucket{key: combo, sortKey: id, keyAsString: id}
				groups[id] = b
				order = append(order, id)
			}
			b.docCount++
			b.hits = append(b.hits, h)
		}
	}
	buckets := make([]*bucket, 0, len(groups))
	for _, id := range order {
		if groups[id].docCount >= minDoc {
			buckets = append(buckets, groups[id])
		}
	}
	if err := ac.fillSub(buckets, sub); err != nil {
		return nil, err
	}
	sortBuckets(buckets, parseOrder(body["order"]))
	other := 0
	if len(buckets) > size {
		for _, b := range buckets[size:] {
			other += b.docCount
		}
		buckets = buckets[:size]
	}
	list := make([]any, 0, len(buckets))
	for _, b := range buckets {
		list = append(list, bucketJSON(b, false))
	}
	return M{"doc_count_error_upper_bound": 0, "sum_other_doc_count": other, "buckets": list}, nil
}

// range aggregations ---------------------------------------------------

func formatRangeNumber(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func (ac *aggContext) rangeAgg(body M, sub M, hits []*hit, isDate bool) (any, error) {
	field := getString(body, "field")
	if field == "" {
		return nil, errParsing("[range] aggregation requires a field")
	}
	keyed := getBool(body, "keyed", false)
	missing := body["missing"]
	var df *DateFormat
	loc := time.UTC
	if isDate {
		if f := getString(body, "format"); f != "" {
			df = ParseDateFormat(f)
		}
		if tz := getString(body, "time_zone"); tz != "" {
			var err error
			if loc, err = parseTimeZone(tz); err != nil {
				return nil, errParsing("%v", err)
			}
		}
	}
	type rng struct {
		from, to    *float64
		key         string
		fromS, toS  string
		explicitKey bool
	}
	var ranges []rng
	for _, raw := range getList(body["ranges"]) {
		rm, _ := raw.(M)
		r := rng{key: getString(rm, "key"), explicitKey: getString(rm, "key") != ""}
		parseBound := func(v any, roundUp bool) (*float64, string, error) {
			if v == nil {
				return nil, "", nil
			}
			if isDate {
				fdf := df
				if fdf == nil {
					fdf = ParseDateFormat(DefaultDateFormat)
				}
				var t time.Time
				var err error
				if s, ok := v.(string); ok {
					t, err = ParseDateMath(s, fdf, ac.c.now(), loc, roundUp)
				} else {
					t, err = fdf.Parse(v)
				}
				if err != nil {
					return nil, "", errParsing("%v", err)
				}
				ms := float64(t.UnixMilli())
				return &ms, fdf.Format(t.In(loc)), nil
			}
			n, ok := toFloat(v)
			if !ok {
				return nil, "", errParsing("invalid range bound [%v]", v)
			}
			return &n, formatRangeNumber(n), nil
		}
		var err error
		if r.from, r.fromS, err = parseBound(rm["from"], false); err != nil {
			return nil, err
		}
		if r.to, r.toS, err = parseBound(rm["to"], false); err != nil {
			return nil, err
		}
		if r.key == "" {
			fs, ts := "*", "*"
			if r.from != nil {
				fs = r.fromS
			}
			if r.to != nil {
				ts = r.toS
			}
			r.key = fs + "-" + ts
		}
		ranges = append(ranges, r)
	}
	buckets := make([]*bucket, len(ranges))
	for i, r := range ranges {
		buckets[i] = &bucket{key: r.key}
	}
	for _, h := range hits {
		nums, _, err := ac.numbers(h, field, missing)
		if err != nil {
			return nil, err
		}
		for i, r := range ranges {
			matched := false
			for _, n := range nums {
				if (r.from == nil || n >= *r.from) && (r.to == nil || n < *r.to) {
					matched = true
					break
				}
			}
			if matched {
				buckets[i].docCount++
				buckets[i].hits = append(buckets[i].hits, h)
			}
		}
	}
	if err := ac.fillSub(buckets, sub); err != nil {
		return nil, err
	}
	render := func(i int) M {
		b := buckets[i]
		r := ranges[i]
		out := M{"doc_count": b.docCount}
		if !keyed {
			out["key"] = r.key
		}
		if r.from != nil {
			out["from"] = *r.from
			if isDate {
				out["from_as_string"] = r.fromS
			}
		}
		if r.to != nil {
			out["to"] = *r.to
			if isDate {
				out["to_as_string"] = r.toS
			}
		}
		for k, v := range b.sub {
			out[k] = v
		}
		return out
	}
	if keyed {
		out := M{}
		for i := range ranges {
			out[ranges[i].key] = render(i)
		}
		return M{"buckets": out}, nil
	}
	list := make([]any, 0, len(ranges))
	for i := range ranges {
		list = append(list, render(i))
	}
	return M{"buckets": list}, nil
}

// histograms ------------------------------------------------------------

func (ac *aggContext) histogram(body M, sub M, hits []*hit) (any, error) {
	field := getString(body, "field")
	interval := getFloat(body, "interval", 0)
	if field == "" || interval <= 0 {
		return nil, errParsing("[histogram] aggregation requires a field and a positive interval")
	}
	offset := getFloat(body, "offset", 0)
	minDoc := getInt(body, "min_doc_count", 0)
	keyed := getBool(body, "keyed", false)
	missing := body["missing"]
	groups := map[float64]*bucket{}
	var minKey, maxKey float64
	hasAny := false
	for _, h := range hits {
		nums, _, err := ac.numbers(h, field, missing)
		if err != nil {
			return nil, err
		}
		seen := map[float64]bool{}
		for _, n := range nums {
			key := math.Floor((n-offset)/interval)*interval + offset
			if seen[key] {
				continue
			}
			seen[key] = true
			b, ok := groups[key]
			if !ok {
				b = &bucket{key: key, sortKey: key}
				groups[key] = b
			}
			b.docCount++
			b.hits = append(b.hits, h)
			if !hasAny || key < minKey {
				minKey = key
			}
			if !hasAny || key > maxKey {
				maxKey = key
			}
			hasAny = true
		}
	}
	if eb := getMap(body, "extended_bounds"); eb != nil {
		if v, ok := toFloat(eb["min"]); ok {
			k := math.Floor((v-offset)/interval)*interval + offset
			if !hasAny || k < minKey {
				minKey = k
			}
			hasAny = true
		}
		if v, ok := toFloat(eb["max"]); ok {
			k := math.Floor((v-offset)/interval)*interval + offset
			if !hasAny || k > maxKey {
				maxKey = k
			}
			hasAny = true
		}
	}
	var buckets []*bucket
	if hasAny {
		if minDoc == 0 {
			for k := minKey; k <= maxKey+interval/1e9; k += interval {
				b, ok := groups[k]
				if !ok {
					b = &bucket{key: k, sortKey: k}
				}
				buckets = append(buckets, b)
			}
		} else {
			for _, b := range groups {
				if b.docCount >= minDoc {
					buckets = append(buckets, b)
				}
			}
			sort.Slice(buckets, func(i, j int) bool { return buckets[i].key.(float64) < buckets[j].key.(float64) })
		}
	}
	if hb := getMap(body, "hard_bounds"); hb != nil {
		filtered := buckets[:0]
		for _, b := range buckets {
			k := b.key.(float64)
			if v, ok := toFloat(hb["min"]); ok && k < v {
				continue
			}
			if v, ok := toFloat(hb["max"]); ok && k > v {
				continue
			}
			filtered = append(filtered, b)
		}
		buckets = filtered
	}
	if err := ac.fillSub(buckets, sub); err != nil {
		return nil, err
	}
	if keyed {
		out := M{}
		for _, b := range buckets {
			out[formatRangeNumber(b.key.(float64))] = bucketJSON(b, true)
		}
		return M{"buckets": out}, nil
	}
	list := make([]any, 0, len(buckets))
	for _, b := range buckets {
		list = append(list, bucketJSON(b, false))
	}
	return M{"buckets": list}, nil
}

type dateInterval struct {
	calendar byte // y M q w d h m s or 0 for fixed
	n        int
	fixed    time.Duration
}

func parseDateInterval(body M) (dateInterval, error) {
	if s := getString(body, "calendar_interval"); s != "" {
		di, ok := calendarInterval(s)
		if !ok {
			return di, errParsing("The supplied interval [%s] could not be parsed as a calendar interval.", s)
		}
		return di, nil
	}
	if s := getString(body, "fixed_interval"); s != "" {
		d, ok := parseDuration(s)
		if !ok || d <= 0 {
			return dateInterval{}, errParsing("failed to parse setting [fixed_interval] with value [%s]", s)
		}
		return dateInterval{fixed: d}, nil
	}
	if s := getString(body, "interval"); s != "" {
		if di, ok := calendarInterval(s); ok {
			return di, nil
		}
		d, ok := parseDuration(s)
		if !ok || d <= 0 {
			return dateInterval{}, errParsing("failed to parse setting [interval] with value [%s]", s)
		}
		return dateInterval{fixed: d}, nil
	}
	return dateInterval{}, errParsing("[date_histogram] requires calendar_interval or fixed_interval")
}

func calendarInterval(s string) (dateInterval, bool) {
	switch s {
	case "year", "1y":
		return dateInterval{calendar: 'y', n: 1}, true
	case "quarter", "1q":
		return dateInterval{calendar: 'q', n: 1}, true
	case "month", "1M":
		return dateInterval{calendar: 'M', n: 1}, true
	case "week", "1w":
		return dateInterval{calendar: 'w', n: 1}, true
	case "day", "1d":
		return dateInterval{calendar: 'd', n: 1}, true
	case "hour", "1h":
		return dateInterval{calendar: 'h', n: 1}, true
	case "minute", "1m":
		return dateInterval{calendar: 'm', n: 1}, true
	case "second", "1s":
		return dateInterval{calendar: 's', n: 1}, true
	}
	return dateInterval{}, false
}

func (di dateInterval) floor(t time.Time, loc *time.Location) time.Time {
	if di.calendar == 0 {
		// fixed intervals are aligned to the epoch in the given zone
		lt := t.In(loc)
		_, off := lt.Zone()
		ms := lt.UnixMilli() + int64(off)*1000
		step := di.fixed.Milliseconds()
		start := ms - ((ms%step)+step)%step
		return time.UnixMilli(start - int64(off)*1000).In(loc)
	}
	lt := t.In(loc)
	switch di.calendar {
	case 'q':
		m := ((int(lt.Month())-1)/3)*3 + 1
		return time.Date(lt.Year(), time.Month(m), 1, 0, 0, 0, 0, loc)
	default:
		return roundDate(lt, di.calendar, false)
	}
}

func (di dateInterval) next(t time.Time) time.Time {
	if di.calendar == 0 {
		return t.Add(di.fixed)
	}
	if di.calendar == 'q' {
		return t.AddDate(0, 3, 0)
	}
	return addUnit(t, di.calendar, di.n)
}

func (ac *aggContext) dateHistogram(body M, sub M, hits []*hit) (any, error) {
	field := getString(body, "field")
	if field == "" {
		return nil, errParsing("[date_histogram] aggregation requires a field")
	}
	di, err := parseDateInterval(body)
	if err != nil {
		return nil, err
	}
	loc := time.UTC
	if tz := getString(body, "time_zone"); tz != "" {
		if loc, err = parseTimeZone(tz); err != nil {
			return nil, errParsing("%v", err)
		}
	}
	var offset time.Duration
	if s := getString(body, "offset"); s != "" {
		neg := strings.HasPrefix(s, "-")
		d, ok := parseDuration(strings.TrimLeft(s, "+-"))
		if !ok {
			return nil, errParsing("failed to parse setting [offset] with value [%s]", s)
		}
		if neg {
			d = -d
		}
		offset = d
	}
	minDoc := getInt(body, "min_doc_count", 0)
	keyed := getBool(body, "keyed", false)
	missing := body["missing"]
	var df *DateFormat
	if f := getString(body, "format"); f != "" {
		df = ParseDateFormat(f)
	}
	bucketStart := func(t time.Time) time.Time {
		return di.floor(t.Add(-offset), loc).Add(offset)
	}
	groups := map[int64]*bucket{}
	var minT, maxT time.Time
	hasAny := false
	var fieldDef *Field
	for _, h := range hits {
		vals, f, err := ac.values(h, field, missing)
		if err != nil {
			return nil, err
		}
		if f != nil {
			fieldDef = f
		}
		seen := map[int64]bool{}
		for _, v := range vals {
			t, ok := v.(time.Time)
			if !ok {
				if n, ok := toFloat(v); ok {
					t = time.UnixMilli(int64(n)).UTC()
				} else {
					continue
				}
			}
			start := bucketStart(t)
			key := start.UnixMilli()
			if seen[key] {
				continue
			}
			seen[key] = true
			b, ok := groups[key]
			if !ok {
				b = &bucket{key: key, sortKey: float64(key)}
				groups[key] = b
			}
			b.docCount++
			b.hits = append(b.hits, h)
			if !hasAny || start.Before(minT) {
				minT = start
			}
			if !hasAny || start.After(maxT) {
				maxT = start
			}
			hasAny = true
		}
	}
	if df == nil {
		if fieldDef != nil && fieldDef.Format != nil {
			df = fieldDef.Format
		} else {
			df = ParseDateFormat(DefaultDateFormat)
		}
	}
	if eb := getMap(body, "extended_bounds"); eb != nil {
		parse := func(v any) (time.Time, bool) {
			if s, ok := v.(string); ok {
				t, err := ParseDateMath(s, df, ac.c.now(), loc, false)
				return t, err == nil
			}
			if n, ok := toFloat(v); ok {
				return time.UnixMilli(int64(n)).UTC(), true
			}
			return time.Time{}, false
		}
		if t, ok := parse(eb["min"]); ok {
			s := bucketStart(t)
			if !hasAny || s.Before(minT) {
				minT = s
			}
			hasAny = true
		}
		if t, ok := parse(eb["max"]); ok {
			s := bucketStart(t)
			if !hasAny || s.After(maxT) {
				maxT = s
			}
			hasAny = true
		}
	}
	var buckets []*bucket
	if hasAny {
		if minDoc == 0 {
			for t := minT; !t.After(maxT); t = di.next(t) {
				key := t.UnixMilli()
				b, ok := groups[key]
				if !ok {
					b = &bucket{key: key, sortKey: float64(key)}
				}
				buckets = append(buckets, b)
				if len(buckets) > 100000 {
					break
				}
			}
		} else {
			for _, b := range groups {
				if b.docCount >= minDoc {
					buckets = append(buckets, b)
				}
			}
			sort.Slice(buckets, func(i, j int) bool { return buckets[i].key.(int64) < buckets[j].key.(int64) })
		}
	}
	for _, b := range buckets {
		b.keyAsString = df.Format(time.UnixMilli(b.key.(int64)).In(loc))
	}
	if err := ac.fillSub(buckets, sub); err != nil {
		return nil, err
	}
	if keyed {
		out := M{}
		for _, b := range buckets {
			bj := bucketJSON(b, true)
			bj["key"] = b.key
			out[b.keyAsString] = bj
		}
		return M{"buckets": out}, nil
	}
	list := make([]any, 0, len(buckets))
	for _, b := range buckets {
		list = append(list, bucketJSON(b, false))
	}
	return M{"buckets": list}, nil
}

// filter aggregations -----------------------------------------------------

func (ac *aggContext) filterHits(q any, hits []*hit) ([]*hit, error) {
	byIndex := map[*Index][]*hit{}
	var order []*Index
	for _, h := range hits {
		if _, ok := byIndex[h.ix]; !ok {
			order = append(order, h.ix)
		}
		byIndex[h.ix] = append(byIndex[h.ix], h)
	}
	keep := map[*Doc]bool{}
	for _, ix := range order {
		matched, err := ac.c.executeTargets([]target{{ix: ix}}, q, false)
		if err != nil {
			return nil, err
		}
		for _, m := range matched {
			keep[m.doc] = true
		}
	}
	var out []*hit
	for _, h := range hits {
		if keep[h.doc] {
			out = append(out, h)
		}
	}
	return out, nil
}

func (ac *aggContext) filter(body M, sub M, hits []*hit) (any, error) {
	if len(body) == 0 {
		return nil, errParsing("[filter] aggregation requires a query")
	}
	matched, err := ac.filterHits(body, hits)
	if err != nil {
		return nil, err
	}
	return ac.single(sub, matched)
}

func (ac *aggContext) filters(body M, sub M, hits []*hit) (any, error) {
	otherBucket := getBool(body, "other_bucket", false)
	otherKey := getString(body, "other_bucket_key")
	if otherKey != "" {
		otherBucket = true
	} else {
		otherKey = "_other_"
	}
	seen := map[*Doc]bool{}
	switch fs := body["filters"].(type) {
	case M:
		out := M{}
		keys := make([]string, 0, len(fs))
		for k := range fs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			matched, err := ac.filterHits(fs[k], hits)
			if err != nil {
				return nil, err
			}
			for _, h := range matched {
				seen[h.doc] = true
			}
			res, err := ac.single(sub, matched)
			if err != nil {
				return nil, err
			}
			out[k] = res
		}
		if otherBucket {
			var rest []*hit
			for _, h := range hits {
				if !seen[h.doc] {
					rest = append(rest, h)
				}
			}
			res, err := ac.single(sub, rest)
			if err != nil {
				return nil, err
			}
			out[otherKey] = res
		}
		return M{"buckets": out}, nil
	case []any:
		var list []any
		for _, q := range fs {
			matched, err := ac.filterHits(q, hits)
			if err != nil {
				return nil, err
			}
			for _, h := range matched {
				seen[h.doc] = true
			}
			res, err := ac.single(sub, matched)
			if err != nil {
				return nil, err
			}
			list = append(list, res)
		}
		if otherBucket {
			var rest []*hit
			for _, h := range hits {
				if !seen[h.doc] {
					rest = append(rest, h)
				}
			}
			res, err := ac.single(sub, rest)
			if err != nil {
				return nil, err
			}
			list = append(list, res)
		}
		if list == nil {
			list = []any{}
		}
		return M{"buckets": list}, nil
	}
	return nil, errParsing("[filters] aggregation requires filters")
}

func (ac *aggContext) missingAgg(body M, sub M, hits []*hit) (any, error) {
	field := getString(body, "field")
	var rest []*hit
	for _, h := range hits {
		vals, _, err := ac.values(h, field, nil)
		if err != nil {
			return nil, err
		}
		if len(vals) == 0 {
			rest = append(rest, h)
		}
	}
	return ac.single(sub, rest)
}

// composite ------------------------------------------------------------

func (ac *aggContext) composite(body M, sub M, hits []*hit) (any, error) {
	sources := getList(body["sources"])
	if len(sources) == 0 {
		return nil, errParsing("[composite] aggregation requires sources")
	}
	size := getInt(body, "size", 10)
	type source struct {
		name    string
		kind    string
		body    M
		desc    bool
		missing bool
		di      dateInterval
		loc     *time.Location
		df      *DateFormat
	}
	var srcs []source
	for _, raw := range sources {
		sm, ok := raw.(M)
		if !ok || len(sm) != 1 {
			return nil, errParsing("[composite] each source must have exactly one name")
		}
		for name, def := range sm {
			dm, _ := def.(M)
			if len(dm) != 1 {
				return nil, errParsing("[composite] source [%s] must define exactly one type", name)
			}
			for kind, kb := range dm {
				kbm, _ := kb.(M)
				s := source{name: name, kind: kind, body: kbm, desc: getString(kbm, "order") == "desc", missing: getBool(kbm, "missing_bucket", false), loc: time.UTC}
				if kind == "date_histogram" {
					di, err := parseDateInterval(kbm)
					if err != nil {
						return nil, err
					}
					s.di = di
					if tz := getString(kbm, "time_zone"); tz != "" {
						loc, err := parseTimeZone(tz)
						if err != nil {
							return nil, errParsing("%v", err)
						}
						s.loc = loc
					}
					if f := getString(kbm, "format"); f != "" {
						s.df = ParseDateFormat(f)
					}
				}
				srcs = append(srcs, s)
			}
		}
	}
	type combo struct {
		keys []any
		sort []any
		b    *bucket
	}
	groups := map[string]*combo{}
	for _, h := range hits {
		partial := [][]any{{}}
		sorts := [][]any{{}}
		skip := false
		for _, s := range srcs {
			field := getString(s.body, "field")
			vals, f, err := ac.values(h, field, nil)
			if err != nil {
				return nil, err
			}
			var keys []any
			var sks []any
			switch s.kind {
			case "terms":
				for _, v := range vals {
					k, sk, _ := keyOf(f, v)
					keys = append(keys, k)
					sks = append(sks, sk)
				}
			case "histogram":
				interval := getFloat(s.body, "interval", 1)
				seen := map[float64]bool{}
				for _, v := range vals {
					n, ok := toFloat(v)
					if !ok {
						continue
					}
					k := math.Floor(n/interval) * interval
					if seen[k] {
						continue
					}
					seen[k] = true
					keys = append(keys, k)
					sks = append(sks, k)
				}
			case "date_histogram":
				seen := map[int64]bool{}
				for _, v := range vals {
					t, ok := v.(time.Time)
					if !ok {
						continue
					}
					start := s.di.floor(t, s.loc)
					k := start.UnixMilli()
					if seen[k] {
						continue
					}
					seen[k] = true
					if s.df != nil {
						keys = append(keys, s.df.Format(start.In(s.loc)))
					} else {
						keys = append(keys, k)
					}
					sks = append(sks, float64(k))
				}
			default:
				return nil, errUnsupported("[composite] source type [" + s.kind + "]")
			}
			if len(keys) == 0 {
				if !s.missing {
					skip = true
					break
				}
				keys = []any{nil}
				sks = []any{nil}
			}
			var np, ns [][]any
			for i, p := range partial {
				for j, k := range keys {
					np = append(np, append(append([]any{}, p...), k))
					ns = append(ns, append(append([]any{}, sorts[i]...), sks[j]))
				}
			}
			partial, sorts = np, ns
		}
		if skip {
			continue
		}
		for i, keys := range partial {
			id := fmt.Sprint(keys...)
			cb, ok := groups[id]
			if !ok {
				cb = &combo{keys: keys, sort: sorts[i], b: &bucket{}}
				groups[id] = cb
			}
			cb.b.docCount++
			cb.b.hits = append(cb.b.hits, h)
		}
	}
	combos := make([]*combo, 0, len(groups))
	for _, cb := range groups {
		combos = append(combos, cb)
	}
	cmpCombo := func(a, b []any) int {
		for i, s := range srcs {
			var c int
			switch {
			case a[i] == nil && b[i] == nil:
				c = 0
			case a[i] == nil:
				c = -1
			case b[i] == nil:
				c = 1
			default:
				c = compareValues(a[i], b[i])
			}
			if s.desc {
				c = -c
			}
			if c != 0 {
				return c
			}
		}
		return 0
	}
	sort.Slice(combos, func(i, j int) bool { return cmpCombo(combos[i].sort, combos[j].sort) < 0 })
	if after := getMap(body, "after"); after != nil {
		afterSort := make([]any, len(srcs))
		for i, s := range srcs {
			v := after[s.name]
			if n, ok := toFloat(v); ok {
				if _, isStr := v.(string); !isStr {
					v = n
				}
			}
			if s.kind == "date_histogram" && s.df != nil {
				if str, ok := v.(string); ok {
					if t, err := s.df.ParseString(str); err == nil {
						v = float64(t.UnixMilli())
					}
				}
			}
			afterSort[i] = v
		}
		filtered := combos[:0]
		for _, cb := range combos {
			if cmpCombo(cb.sort, afterSort) > 0 {
				filtered = append(filtered, cb)
			}
		}
		combos = filtered
	}
	if len(combos) > size {
		combos = combos[:size]
	}
	buckets := make([]*bucket, 0, len(combos))
	for _, cb := range combos {
		buckets = append(buckets, cb.b)
	}
	if err := ac.fillSub(buckets, sub); err != nil {
		return nil, err
	}
	list := make([]any, 0, len(combos))
	var afterKey M
	for _, cb := range combos {
		key := M{}
		for i, s := range srcs {
			key[s.name] = cb.keys[i]
		}
		bj := M{"key": key, "doc_count": cb.b.docCount}
		for k, v := range cb.b.sub {
			bj[k] = v
		}
		list = append(list, bj)
		afterKey = key
	}
	out := M{"buckets": list}
	if afterKey != nil {
		out["after_key"] = afterKey
	}
	return out, nil
}

// metrics ----------------------------------------------------------------

func (ac *aggContext) collectNumbers(body M, hits []*hit) ([]float64, *Field, error) {
	field := getString(body, "field")
	if field == "" {
		return nil, nil, errParsing("metric aggregation requires a field")
	}
	var all []float64
	var fd *Field
	for _, h := range hits {
		nums, f, err := ac.numbers(h, field, body["missing"])
		if err != nil {
			return nil, nil, err
		}
		if f != nil {
			fd = f
		}
		all = append(all, nums...)
	}
	return all, fd, nil
}

func metricValue(f *Field, v *float64, format string) M {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return M{"value": nil}
	}
	out := M{"value": *v}
	if f != nil && f.isDate() {
		df := f.Format
		if format != "" {
			df = ParseDateFormat(format)
		}
		out["value_as_string"] = df.Format(time.UnixMilli(int64(*v)).UTC())
	} else if format != "" {
		out["value_as_string"] = formatRangeNumber(*v)
	}
	return out
}

func (ac *aggContext) simpleMetric(kind string, body M, hits []*hit) (any, error) {
	if kind == "value_count" {
		field := getString(body, "field")
		if field == "" {
			return nil, errParsing("[value_count] aggregation requires a field")
		}
		n := 0
		for _, h := range hits {
			vals, _, err := ac.values(h, field, body["missing"])
			if err != nil {
				return nil, err
			}
			n += len(vals)
		}
		return M{"value": n}, nil
	}
	nums, f, err := ac.collectNumbers(body, hits)
	if err != nil {
		return nil, err
	}
	format := getString(body, "format")
	switch kind {
	case "sum":
		s := 0.0
		for _, n := range nums {
			s += n
		}
		return metricValue(f, &s, format), nil
	}
	if len(nums) == 0 {
		return M{"value": nil}, nil
	}
	var r float64
	switch kind {
	case "avg":
		for _, n := range nums {
			r += n
		}
		r /= float64(len(nums))
	case "min":
		r = nums[0]
		for _, n := range nums {
			if n < r {
				r = n
			}
		}
	case "max":
		r = nums[0]
		for _, n := range nums {
			if n > r {
				r = n
			}
		}
	}
	return metricValue(f, &r, format), nil
}

func (ac *aggContext) stats(kind string, body M, hits []*hit) (any, error) {
	nums, f, err := ac.collectNumbers(body, hits)
	if err != nil {
		return nil, err
	}
	count := len(nums)
	if count == 0 {
		out := M{"count": 0, "min": nil, "max": nil, "avg": nil, "sum": 0.0}
		if kind == "extended_stats" {
			out["sum_of_squares"] = nil
			out["variance"] = nil
			out["variance_population"] = nil
			out["variance_sampling"] = nil
			out["std_deviation"] = nil
			out["std_deviation_population"] = nil
			out["std_deviation_sampling"] = nil
			out["std_deviation_bounds"] = M{"upper": nil, "lower": nil, "upper_population": nil, "lower_population": nil, "upper_sampling": nil, "lower_sampling": nil}
		}
		return out, nil
	}
	min, max, sum, sq := nums[0], nums[0], 0.0, 0.0
	for _, n := range nums {
		if n < min {
			min = n
		}
		if n > max {
			max = n
		}
		sum += n
		sq += n * n
	}
	avg := sum / float64(count)
	out := M{"count": count, "min": min, "max": max, "avg": avg, "sum": sum}
	if f != nil && f.isDate() {
		out["min_as_string"] = f.Format.Format(time.UnixMilli(int64(min)).UTC())
		out["max_as_string"] = f.Format.Format(time.UnixMilli(int64(max)).UTC())
		out["avg_as_string"] = f.Format.Format(time.UnixMilli(int64(avg)).UTC())
		out["sum_as_string"] = f.Format.Format(time.UnixMilli(int64(sum)).UTC())
	}
	if kind == "extended_stats" {
		sigma := getFloat(body, "sigma", 2)
		varPop := sq/float64(count) - avg*avg
		if varPop < 0 {
			varPop = 0
		}
		varSamp := 0.0
		if count > 1 {
			varSamp = (sq - float64(count)*avg*avg) / float64(count-1)
			if varSamp < 0 {
				varSamp = 0
			}
		}
		stdPop := math.Sqrt(varPop)
		stdSamp := math.Sqrt(varSamp)
		out["sum_of_squares"] = sq
		out["variance"] = varPop
		out["variance_population"] = varPop
		out["variance_sampling"] = varSamp
		out["std_deviation"] = stdPop
		out["std_deviation_population"] = stdPop
		out["std_deviation_sampling"] = stdSamp
		out["std_deviation_bounds"] = M{
			"upper": avg + sigma*stdPop, "lower": avg - sigma*stdPop,
			"upper_population": avg + sigma*stdPop, "lower_population": avg - sigma*stdPop,
			"upper_sampling": avg + sigma*stdSamp, "lower_sampling": avg - sigma*stdSamp,
		}
	}
	return out, nil
}

func (ac *aggContext) cardinality(body M, hits []*hit) (any, error) {
	field := getString(body, "field")
	if field == "" {
		return nil, errParsing("[cardinality] aggregation requires a field")
	}
	set := map[string]bool{}
	for _, h := range hits {
		vals, f, err := ac.values(h, field, body["missing"])
		if err != nil {
			return nil, err
		}
		for _, v := range vals {
			k, _, _ := keyOf(f, v)
			set[fmt.Sprintf("%T:%v", k, k)] = true
		}
	}
	return M{"value": len(set)}, nil
}

func percentileKey(p float64) string {
	s := strconv.FormatFloat(p, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	rank := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(rank-float64(lo))
}

func (ac *aggContext) percentiles(body M, hits []*hit) (any, error) {
	nums, f, err := ac.collectNumbers(body, hits)
	if err != nil {
		return nil, err
	}
	sort.Float64s(nums)
	percents := []float64{1, 5, 25, 50, 75, 95, 99}
	if ps := getList(body["percents"]); len(ps) > 0 {
		percents = nil
		for _, p := range ps {
			if n, ok := toFloat(p); ok {
				percents = append(percents, n)
			}
		}
	}
	keyed := getBool(body, "keyed", true)
	if keyed {
		vals := M{}
		for _, p := range percents {
			v := percentile(nums, p)
			if math.IsNaN(v) {
				vals[percentileKey(p)] = nil
			} else {
				vals[percentileKey(p)] = v
				if f != nil && f.isDate() {
					vals[percentileKey(p)+"_as_string"] = f.Format.Format(time.UnixMilli(int64(v)).UTC())
				}
			}
		}
		return M{"values": vals}, nil
	}
	var list []any
	for _, p := range percents {
		v := percentile(nums, p)
		entry := M{"key": p}
		if math.IsNaN(v) {
			entry["value"] = nil
		} else {
			entry["value"] = v
		}
		list = append(list, entry)
	}
	return M{"values": list}, nil
}

func (ac *aggContext) percentileRanks(body M, hits []*hit) (any, error) {
	nums, _, err := ac.collectNumbers(body, hits)
	if err != nil {
		return nil, err
	}
	sort.Float64s(nums)
	keyed := getBool(body, "keyed", true)
	values := getList(body["values"])
	rank := func(v float64) float64 {
		if len(nums) == 0 {
			return math.NaN()
		}
		count := 0
		for _, n := range nums {
			if n <= v {
				count++
			}
		}
		return float64(count) / float64(len(nums)) * 100
	}
	if keyed {
		out := M{}
		for _, raw := range values {
			v, ok := toFloat(raw)
			if !ok {
				continue
			}
			r := rank(v)
			if math.IsNaN(r) {
				out[percentileKey(v)] = nil
			} else {
				out[percentileKey(v)] = r
			}
		}
		return M{"values": out}, nil
	}
	var list []any
	for _, raw := range values {
		v, ok := toFloat(raw)
		if !ok {
			continue
		}
		list = append(list, M{"key": v, "value": rank(v)})
	}
	return M{"values": list}, nil
}

func (ac *aggContext) topHits(body M, hits []*hit) (any, error) {
	sr := &searchRequest{size: getInt(body, "size", 3), from: getInt(body, "from", 0), trackTotal: -1}
	if v, ok := body["sort"]; ok {
		specs, err := parseSort(v)
		if err != nil {
			return nil, err
		}
		sr.sort = specs
		sr.explicitSort = len(specs) > 0
	}
	if len(sr.sort) == 0 {
		sr.sort = []sortSpec{{field: "_score", desc: true}}
	}
	if v, ok := body["_source"]; ok {
		sr.source = parseSourceParam(v)
	}
	if v, ok := body["fields"]; ok {
		sr.fields = getList(v)
	}
	if v, ok := body["docvalue_fields"]; ok {
		sr.docvalueFields = getList(v)
	}
	sr.version = getBool(body, "version", false)
	sr.seqNoTerm = getBool(body, "seq_no_primary_term", false)
	sr.trackScores = getBool(body, "track_scores", false)
	if hl, ok := body["highlight"].(M); ok {
		sr.highlight = hl
	}
	copyHits := make([]*hit, len(hits))
	for i, h := range hits {
		c := *h
		copyHits[i] = &c
	}
	if err := ac.c.sortHits(copyHits, sr); err != nil {
		return nil, err
	}
	page := copyHits
	if sr.from < len(page) {
		page = page[sr.from:]
	} else {
		page = nil
	}
	if len(page) > sr.size {
		page = page[:sr.size]
	}
	return M{"hits": ac.c.hitsJSON(page, sr, len(hits))}, nil
}

func (ac *aggContext) weightedAvg(body M, hits []*hit) (any, error) {
	vs := getMap(body, "value")
	ws := getMap(body, "weight")
	if vs == nil || ws == nil {
		return nil, errParsing("[weighted_avg] requires value and weight")
	}
	num, den := 0.0, 0.0
	for _, h := range hits {
		vals, _, err := ac.numbers(h, getString(vs, "field"), vs["missing"])
		if err != nil {
			return nil, err
		}
		weights, _, err := ac.numbers(h, getString(ws, "field"), ws["missing"])
		if err != nil {
			return nil, err
		}
		if len(weights) == 0 {
			continue
		}
		w := weights[0]
		for _, v := range vals {
			num += v * w
			den += w
		}
	}
	if den == 0 {
		return M{"value": nil}, nil
	}
	return M{"value": num / den}, nil
}

func (ac *aggContext) medianAbsoluteDeviation(body M, hits []*hit) (any, error) {
	nums, _, err := ac.collectNumbers(body, hits)
	if err != nil {
		return nil, err
	}
	if len(nums) == 0 {
		return M{"value": nil}, nil
	}
	sort.Float64s(nums)
	med := percentile(nums, 50)
	devs := make([]float64, len(nums))
	for i, n := range nums {
		devs[i] = math.Abs(n - med)
	}
	sort.Float64s(devs)
	return M{"value": percentile(devs, 50)}, nil
}

// pipeline aggregations ---------------------------------------------------

func bucketsOf(res any) ([]M, bool) {
	rm, ok := res.(M)
	if !ok {
		return nil, false
	}
	list, ok := rm["buckets"].([]any)
	if !ok {
		return nil, false
	}
	out := make([]M, 0, len(list))
	for _, b := range list {
		if bm, ok := b.(M); ok {
			out = append(out, bm)
		}
	}
	return out, true
}

func setBuckets(res any, buckets []M) {
	list := make([]any, len(buckets))
	for i, b := range buckets {
		list[i] = b
	}
	res.(M)["buckets"] = list
}

// bucketPathValue resolves a buckets_path inside one bucket.
func bucketPathValue(b M, path string) (float64, bool) {
	if path == "_count" {
		v, ok := toFloat(b["doc_count"])
		return v, ok
	}
	parts := strings.Split(path, ">")
	var cur any = b
	for i, p := range parts {
		name, metric := p, ""
		if idx := strings.Index(p, "."); idx >= 0 {
			name, metric = p[:idx], p[idx+1:]
		}
		cm, ok := cur.(M)
		if !ok {
			return 0, false
		}
		cur, ok = cm[name]
		if !ok {
			return 0, false
		}
		if metric != "" {
			m, ok := cur.(M)
			if !ok {
				return 0, false
			}
			cur = m[metric]
		} else if i == len(parts)-1 {
			if m, ok := cur.(M); ok {
				if v, ok := m["value"]; ok {
					cur = v
				}
			}
		}
	}
	v, ok := toFloat(cur)
	return v, ok
}

func (ac *aggContext) runPipeline(name string, spec M, out M) error {
	kind, body, _, err := splitAggSpec(spec)
	if err != nil {
		return err
	}
	path := getString(body, "buckets_path")
	switch kind {
	case "bucket_script", "bucket_selector", "moving_fn", "moving_avg", "serial_diff", "cumulative_cardinality", "extended_stats_bucket", "percentiles_bucket":
		return errUnsupported("[" + kind + "] pipeline aggregation")
	}
	// sibling pipelines: path "agg>metric"
	if kind == "avg_bucket" || kind == "sum_bucket" || kind == "min_bucket" || kind == "max_bucket" || kind == "stats_bucket" {
		parts := strings.SplitN(path, ">", 2)
		if len(parts) != 2 {
			return errParsing("[%s] buckets_path must reference a sibling aggregation", kind)
		}
		buckets, ok := bucketsOf(out[parts[0]])
		if !ok {
			return errParsing("No aggregation found for path [%s]", path)
		}
		var vals []float64
		var keys []any
		for _, b := range buckets {
			if v, ok := bucketPathValue(b, parts[1]); ok {
				vals = append(vals, v)
				keys = append(keys, b["key"])
			}
		}
		switch kind {
		case "stats_bucket":
			if len(vals) == 0 {
				out[name] = M{"count": 0, "min": nil, "max": nil, "avg": nil, "sum": 0.0}
				return nil
			}
			min, max, sum := vals[0], vals[0], 0.0
			for _, v := range vals {
				min, max, sum = math.Min(min, v), math.Max(max, v), sum+v
			}
			out[name] = M{"count": len(vals), "min": min, "max": max, "avg": sum / float64(len(vals)), "sum": sum}
		case "sum_bucket":
			sum := 0.0
			for _, v := range vals {
				sum += v
			}
			out[name] = M{"value": sum}
		case "avg_bucket":
			if len(vals) == 0 {
				out[name] = M{"value": nil}
				return nil
			}
			sum := 0.0
			for _, v := range vals {
				sum += v
			}
			out[name] = M{"value": sum / float64(len(vals))}
		case "min_bucket", "max_bucket":
			if len(vals) == 0 {
				out[name] = M{"value": nil, "keys": []any{}}
				return nil
			}
			best := vals[0]
			var bestKeys []any
			for i, v := range vals {
				if (kind == "min_bucket" && v < best) || (kind == "max_bucket" && v > best) {
					best = v
					bestKeys = nil
				}
				if v == best {
					bestKeys = append(bestKeys, keyString(keys[i]))
				}
			}
			out[name] = M{"value": best, "keys": bestKeys}
		}
		return nil
	}
	return errUnsupported("[" + kind + "] pipeline aggregation at this level")
}

func keyString(k any) any {
	switch t := k.(type) {
	case string:
		return t
	case nil:
		return nil
	}
	return fmt.Sprint(numberValue(k))
}

// applyParentPipelines handles pipelines declared inside a bucket
// aggregation (bucket_sort, cumulative_sum, derivative). It is called by
// the parent after its buckets are built.
func (ac *aggContext) applyParentPipelines(sub M, res any) error {
	if len(sub) == 0 {
		return nil
	}
	buckets, ok := bucketsOf(res)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(sub))
	for n := range sub {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		spec, _ := sub[name].(M)
		kind, body, _, err := splitAggSpec(spec)
		if err != nil {
			return err
		}
		if !parentPipelineAggs[kind] {
			continue
		}
		path := getString(body, "buckets_path")
		switch kind {
		case "cumulative_sum":
			sum := 0.0
			for _, b := range buckets {
				if v, ok := bucketPathValue(b, path); ok {
					sum += v
				}
				b[name] = M{"value": sum}
			}
		case "derivative":
			var prev *float64
			for _, b := range buckets {
				v, ok := bucketPathValue(b, path)
				if ok && prev != nil {
					b[name] = M{"value": v - *prev}
				}
				if ok {
					pv := v
					prev = &pv
				}
			}
		case "bucket_sort":
			orders := parseOrder(body["sort"])
			if len(orders) > 0 {
				sort.SliceStable(buckets, func(i, j int) bool {
					for _, o := range orders {
						var av, bv float64
						if o.path == "_key" {
							c := compareValues(buckets[i]["key"], buckets[j]["key"])
							if c != 0 {
								if o.desc {
									return c > 0
								}
								return c < 0
							}
							continue
						}
						av, _ = bucketPathValue(buckets[i], o.path)
						bv, _ = bucketPathValue(buckets[j], o.path)
						if av != bv {
							if o.desc {
								return av > bv
							}
							return av < bv
						}
					}
					return false
				})
			}
			from := getInt(body, "from", 0)
			size := getInt(body, "size", len(buckets))
			if from > len(buckets) {
				from = len(buckets)
			}
			buckets = buckets[from:]
			if size < len(buckets) {
				buckets = buckets[:size]
			}
		}
	}
	setBuckets(res, buckets)
	return nil
}
