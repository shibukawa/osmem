package engine

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// metric aggregations ---------------------------------------------------------------

var (
	numericKinds  = []vsKind{vsNumeric, vsDate, vsBoolean}
	allCoreKinds  = []vsKind{vsBytes, vsNumeric, vsDate, vsBoolean, vsIP, vsGeoPoint, vsRange}
	statsNames    = map[string]bool{"count": true, "sum": true, "min": true, "max": true, "avg": true}
	extStatsNames = map[string]bool{"count": true, "sum": true, "min": true, "max": true, "avg": true, "sum_of_squares": true,
		"variance": true, "variance_population": true, "variance_sampling": true, "std_deviation": true,
		"std_deviation_population": true, "std_deviation_sampling": true, "std_upper": true, "std_lower": true,
		"std_upper_population": true, "std_lower_population": true, "std_upper_sampling": true, "std_lower_sampling": true}
)

type metricSpec struct {
	vs    vsConfig
	sigma float64
}

func parseMetric(ps *aggParser, d *aggDef) error {
	var of objFields
	switch d.kind {
	case "extended_stats":
		of = valuesSourceFields(d.kind, true, false, map[string]int{"sigma": vtNumber})
	case "cardinality":
		of = valuesSourceFields(d.kind, false, false, map[string]int{"precision_threshold": vtNumber, "rehash": vtBool, "execution_hint": vtString})
	default:
		of = valuesSourceFields(d.kind, true, false, nil)
	}
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &metricSpec{sigma: 2}
	var err error
	if spec.vs, err = parseVSConfig(of, body); err != nil {
		return err
	}
	if _, ok := body["sigma"]; ok {
		if spec.sigma, err = of.doubleValue(body, "sigma"); err != nil {
			return err
		}
		if spec.sigma < 0 {
			return of.failed(body, "sigma", errIllegalArgument("[sigma] must be greater than or equal to 0. Found [%s] in [%s]", javaDoubleToString(spec.sigma), d.name))
		}
	}
	if _, ok := body["precision_threshold"]; ok {
		n, err := of.longValue(body, "precision_threshold")
		if err != nil {
			return err
		}
		if n < 0 {
			return of.failed(body, "precision_threshold", errIllegalArgument("[precisionThreshold] must be greater than or equal to 0. Found [%d] in [%s]", n, d.name))
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

func prepareMetric(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*metricSpec)
	switch d.kind {
	case "value_count", "cardinality":
		_, err := pc.resolve(d, 0, &spec.vs, d.kind, vsBytes, allCoreKinds...)
		return err
	}
	_, err := pc.resolve(d, 0, &spec.vs, d.kind, vsNumeric, numericKinds...)
	return err
}

// compensatedSum is CompensatedSum (Kahan summation).
type compensatedSum struct{ value, delta float64 }

func (s *compensatedSum) add(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		s.value = v + s.value
	}
	if !math.IsNaN(s.value) && !math.IsInf(s.value, 0) {
		corrected := v + s.delta
		updated := s.value + corrected
		s.delta = corrected - (updated - s.value)
		s.value = updated
	}
}

// statsAcc accumulates the statistics of one shard.
type statsAcc struct {
	count    int64
	sum, sq  compensatedSum
	min, max float64
}

func newStatsAcc() *statsAcc { return &statsAcc{min: math.Inf(1), max: math.Inf(-1)} }

// firstSource is the values source of the first searched index: reduced
// results keep the format of the first shard result.
func (ac *aggContext) firstSource(d *aggDef, slot int) *valuesSource {
	for _, ix := range ac.indices {
		if vs := ac.source(d, slot, ix); vs != nil {
			return vs
		}
	}
	return nil
}

func (ac *aggContext) firstFormat(d *aggDef, slot int) *valueFormat {
	if vs := ac.firstSource(d, slot); vs != nil && vs.format != nil {
		return vs.format
	}
	return rawFormat
}

// shardStats computes per-index statistics and reduces them.
func (ac *aggContext) shardStats(d *aggDef, hits []*hit) *statsAcc {
	shards := map[*Index]*statsAcc{}
	for _, h := range hits {
		vs := ac.source(d, 0, h.ix)
		if vs == nil {
			continue
		}
		nums := vs.nums(h)
		if len(nums) == 0 {
			continue
		}
		s := shards[h.ix]
		if s == nil {
			s = newStatsAcc()
			shards[h.ix] = s
		}
		for _, n := range nums {
			s.count++
			s.sum.add(n)
			s.sq.add(float64(n * n))
			s.min = math.Min(s.min, n)
			s.max = math.Max(s.max, n)
		}
	}
	total := newStatsAcc()
	for _, ix := range ac.indices {
		s := shards[ix]
		if s == nil {
			continue
		}
		total.count += s.count
		total.min = math.Min(total.min, s.min)
		total.max = math.Max(total.max, s.max)
		total.sum.add(s.sum.value)
		total.sq.add(s.sq.value)
	}
	return total
}

func collectMetric(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	format := ac.firstFormat(d, 0)
	switch d.kind {
	case "value_count":
		var n int64
		for _, h := range hits {
			if vs := ac.source(d, 0, h.ix); vs != nil {
				n += int64(valueCount(vs, h))
			}
		}
		return &aggResult{kind: resValue, value: float64(n), fields: M{"value": n}, javaClass: "InternalValueCount"}, nil
	case "cardinality":
		seen := map[any]bool{}
		for _, h := range hits {
			vs := ac.source(d, 0, h.ix)
			if vs == nil {
				continue
			}
			switch vs.kind {
			case vsBytes, vsIP:
				for _, s := range vs.strs(h) {
					seen[s] = true
				}
			case vsGeoPoint:
				for _, p := range vs.points(h) {
					seen[p] = true
				}
			default:
				for _, n := range vs.nums(h) {
					seen[math.Float64bits(n)] = true
				}
			}
		}
		n := int64(len(seen))
		return &aggResult{kind: resValue, value: float64(n), fields: M{"value": n}, javaClass: "InternalCardinality"}, nil
	}
	st := ac.shardStats(d, hits)
	r := &aggResult{kind: resValue, fields: M{}}
	switch d.kind {
	case "avg":
		r.javaClass = "InternalAvg"
		r.value = st.sum.value / float64(st.count)
		if st.count == 0 {
			r.fields["value"] = nil
		} else {
			r.fields["value"] = r.value
			if !format.raw() {
				r.fields["value_as_string"] = format.stringDouble(r.value)
			}
		}
	case "sum":
		r.javaClass = "InternalSum"
		r.value = st.sum.value
		r.fields["value"] = r.value
		if !format.raw() {
			r.fields["value_as_string"] = format.stringDouble(r.value)
		}
	case "min", "max":
		r.javaClass = "InternalMin"
		r.value = st.min
		if d.kind == "max" {
			r.javaClass = "InternalMax"
			r.value = st.max
		}
		if math.IsInf(r.value, 0) {
			r.fields["value"] = nil
		} else {
			r.fields["value"] = r.value
			if !format.raw() {
				r.fields["value_as_string"] = format.stringDouble(r.value)
			}
		}
	case "stats", "extended_stats":
		sigma := d.spec.(*metricSpec).sigma
		return statsResult(d.kind, st.count, st.sum.value, st.sq.value, st.min, st.max, sigma, format), nil
	}
	return r, nil
}

// valueCount counts the doc values of a hit: every numeric value, distinct
// terms.
func valueCount(vs *valuesSource, h *hit) int {
	switch vs.kind {
	case vsBytes, vsIP:
		return len(vs.strs(h))
	case vsGeoPoint:
		return len(vs.points(h))
	}
	return len(vs.nums(h))
}

// extendedStats holds the values of InternalExtendedStats.
type extendedStats struct {
	count                    int64
	sum, sumOfSqrs, min, max float64
	sigma                    float64
}

func (s extendedStats) avg() float64 { return s.sum / float64(s.count) }

func (s extendedStats) variancePopulation() float64 {
	v := (s.sumOfSqrs - float64(s.sum*s.sum)/float64(s.count)) / float64(s.count)
	if v < 0 {
		return 0
	}
	return v
}

func (s extendedStats) varianceSampling() float64 {
	v := (s.sumOfSqrs - float64(s.sum*s.sum)/float64(s.count)) / float64(s.count-1)
	if v < 0 {
		return 0
	}
	return v
}

func (s extendedStats) value(name string) (float64, error) {
	switch name {
	case "count":
		return float64(s.count), nil
	case "sum":
		return s.sum, nil
	case "min":
		return s.min, nil
	case "max":
		return s.max, nil
	case "avg":
		return s.avg(), nil
	case "sum_of_squares":
		return s.sumOfSqrs, nil
	case "variance", "variance_population":
		return s.variancePopulation(), nil
	case "variance_sampling":
		return s.varianceSampling(), nil
	case "std_deviation", "std_deviation_population":
		return math.Sqrt(s.variancePopulation()), nil
	case "std_deviation_sampling":
		return math.Sqrt(s.varianceSampling()), nil
	case "std_upper", "std_upper_population":
		return s.avg() + float64(math.Sqrt(s.variancePopulation())*s.sigma), nil
	case "std_lower", "std_lower_population":
		return s.avg() - float64(math.Sqrt(s.variancePopulation())*s.sigma), nil
	case "std_upper_sampling":
		return s.avg() + float64(math.Sqrt(s.varianceSampling())*s.sigma), nil
	case "std_lower_sampling":
		return s.avg() - float64(math.Sqrt(s.varianceSampling())*s.sigma), nil
	}
	return 0, errIllegalArgument("No enum constant org.opensearch.search.aggregations.metrics.InternalStats.Metrics.%s", name)
}

// statsResult renders stats and extended_stats (also the bucket pipelines).
func statsResult(kind string, count int64, sum, sumOfSqrs, min, max, sigma float64, format *valueFormat) *aggResult {
	s := extendedStats{count: count, sum: sum, sumOfSqrs: sumOfSqrs, min: min, max: max, sigma: sigma}
	extended := kind == "extended_stats" || kind == "extended_stats_bucket"
	r := &aggResult{kind: resMultiValue, fields: M{"count": count}, javaClass: "InternalStats"}
	names := statsNames
	if extended {
		names = extStatsNames
		r.javaClass = "InternalExtendedStats"
	}
	r.metric = func(name string) (float64, error) {
		if !names[name] {
			return 0, errIllegalArgument("No enum constant org.opensearch.search.aggregations.metrics.InternalStats.Metrics.%s", name)
		}
		return s.value(name)
	}
	f := r.fields
	if count != 0 {
		f["min"], f["max"], f["avg"], f["sum"] = min, max, s.avg(), sum
		if !format.raw() {
			f["min_as_string"] = format.formatDouble(min)
			f["max_as_string"] = format.formatDouble(max)
			f["avg_as_string"] = format.formatDouble(s.avg())
			f["sum_as_string"] = format.formatDouble(sum)
		}
	} else {
		f["min"], f["max"], f["avg"], f["sum"] = nil, nil, nil, 0.0
	}
	if !extended {
		return r
	}
	boundNames := []string{"upper", "lower", "upper_population", "lower_population", "upper_sampling", "lower_sampling"}
	boundMetrics := []string{"std_upper", "std_lower", "std_upper_population", "std_lower_population", "std_upper_sampling", "std_lower_sampling"}
	if count == 0 {
		for _, k := range []string{"sum_of_squares", "variance", "variance_population", "variance_sampling", "std_deviation", "std_deviation_population", "std_deviation_sampling"} {
			f[k] = nil
		}
		bounds := M{}
		for _, b := range boundNames {
			bounds[b] = nil
		}
		f["std_deviation_bounds"] = bounds
		return r
	}
	val := func(name string) float64 { v, _ := s.value(name); return v }
	for _, k := range []string{"sum_of_squares", "variance", "variance_population", "variance_sampling", "std_deviation", "std_deviation_population", "std_deviation_sampling"} {
		f[k] = val(k)
	}
	bounds := M{}
	for i, b := range boundNames {
		bounds[b] = val(boundMetrics[i])
	}
	f["std_deviation_bounds"] = bounds
	if !format.raw() {
		for _, k := range []string{"sum_of_squares", "variance", "variance_population", "variance_sampling"} {
			f[k+"_as_string"] = format.formatDouble(val(k))
		}
		for _, k := range []string{"std_deviation", "std_deviation_population", "std_deviation_sampling"} {
			f[k+"_as_string"] = format.stringDouble(val(k))
		}
		bs := M{}
		for i, b := range boundNames {
			bs[b] = format.stringDouble(val(boundMetrics[i]))
		}
		f["std_deviation_bounds_as_string"] = bs
	}
	return r
}

// weighted_avg ---------------------------------------------------------------------------

var (
	weightedAvgFields = objFields{name: "weighted_avg", fields: map[string]int{"value": vtObject, "weight": vtObject, "format": vtString, "value_type": vtString}}
	fieldConfigFields = objFields{name: "field_config", fields: map[string]int{"field": vtString, "missing": vtValue, "script": vtObjectOrString, "time_zone": vtNumber, "filter": vtObject}}
)

type weightedAvgSpec struct {
	value, weight       vsConfig
	hasValue, hasWeight bool
	format              *valueFormat
}

func parseWeightedAvg(ps *aggParser, d *aggDef) error {
	of := weightedAvgFields
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &weightedAvgSpec{format: rawFormat}
	for _, key := range []string{"value", "weight"} {
		raw, ok := body[key].(M)
		if !ok {
			continue
		}
		if err := fieldConfigFields.check(raw); err != nil {
			return of.failed(body, key, err.(*Error))
		}
		cfg, err := parseVSConfig(fieldConfigFields, raw)
		if err != nil {
			return of.failed(body, key, err.(*Error))
		}
		if !cfg.hasField && !cfg.script {
			return of.failed(body, key, errIllegalArgument("[field] and [script] cannot both be null.  Please specify one or the other.").atParser(endTok(raw)))
		}
		if cfg.script {
			return errScript(d)
		}
		cfg.valueType = "numeric"
		if key == "value" {
			spec.value, spec.hasValue = cfg, true
		} else {
			spec.weight, spec.hasWeight = cfg, true
		}
	}
	vt, _ := body["value_type"].(string)
	if _, ok := body["value_type"]; ok {
		if _, known := valueTypeKind(vt); !known {
			return of.failed(body, "value_type", errIllegalArgument("Unknown value type [%s]", vt))
		}
	}
	format, hasFormat := body["format"].(string)
	switch {
	case vt == "date":
		df := ParseDateFormat(DefaultDateFormat)
		if hasFormat {
			df = ParseDateFormat(format)
		}
		spec.format = &valueFormat{kind: fmtDate, date: df, loc: time.UTC}
	case vt == "boolean":
		spec.format = boolFormat
	case vt == "ip":
		spec.format = ipFormat
	case vt == "" && hasFormat:
		f, err := decimalValueFormat(format)
		if err != nil {
			return err
		}
		spec.format = f
	}
	d.spec = spec
	return nil
}

func prepareWeightedAvg(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*weightedAvgSpec)
	if spec.hasValue {
		if _, err := pc.resolve(d, 0, &spec.value, "weighted_avg", vsNumeric, numericKinds...); err != nil {
			return err
		}
	}
	if spec.hasWeight {
		if _, err := pc.resolve(d, 1, &spec.weight, "weighted_avg", vsNumeric, numericKinds...); err != nil {
			return err
		}
	}
	if !spec.hasValue {
		return errIllegalArgument("Could not find field name [value] in multiValuesSource")
	}
	if !spec.hasWeight {
		return errIllegalArgument("Could not find field name [weight] in multiValuesSource")
	}
	return nil
}

func collectWeightedAvg(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*weightedAvgSpec)
	type acc struct{ values, weights compensatedSum }
	shards := map[*Index]*acc{}
	for _, h := range hits {
		vvs, wvs := ac.source(d, 0, h.ix), ac.source(d, 1, h.ix)
		if vvs == nil || wvs == nil {
			continue
		}
		vals, weights := vvs.nums(h), wvs.nums(h)
		if len(vals) == 0 || len(weights) == 0 {
			continue
		}
		if len(weights) > 1 {
			return nil, shardError(errAggExecution("Encountered more than one weight for a single document. Use a script to combine multiple weights-per-doc into a single value."), h.ix)
		}
		a := shards[h.ix]
		if a == nil {
			a = &acc{}
			shards[h.ix] = a
		}
		for _, v := range vals {
			a.values.add(float64(v * weights[0]))
			a.weights.add(weights[0])
		}
	}
	var sum, weight compensatedSum
	for _, ix := range ac.indices {
		if a := shards[ix]; a != nil {
			sum.add(a.values.value)
			weight.add(a.weights.value)
		}
	}
	r := &aggResult{kind: resValue, value: sum.value / weight.value, fields: M{"value": nil}, javaClass: "InternalWeightedAvg"}
	if weight.value != 0 {
		r.fields["value"] = r.value
		if !spec.format.raw() {
			r.fields["value_as_string"] = spec.format.stringDouble(r.value)
		}
	}
	return r, nil
}

// top_hits ------------------------------------------------------------------------------------

type topHitsSpec struct {
	from, size int
	body       M
}

func parseTopHits(ps *aggParser, d *aggDef) error {
	body := d.body
	spec := &topHitsSpec{size: 3, body: body}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := body[k]
		tok := jsonTokenName(v)
		switch v.(type) {
		case M:
			switch k {
			case "_source", "highlight", "sort":
			case "script_fields":
				return errUnsupported("[script_fields] in top_hits aggregation [" + d.name + "]")
			default:
				return errParsing("Unknown key for a START_OBJECT in [%s].", k).at(valueTok(body, k))
			}
		case []any:
			switch k {
			case "stored_fields", "docvalue_fields", "fields", "sort", "_source":
			default:
				return errParsing("Unknown key for a START_ARRAY in [%s].", k).at(valueTok(body, k))
			}
		default:
			switch k {
			case "from", "size":
				n, err := javaIntValue(v)
				if err != nil {
					return err
				}
				if n < 0 {
					return errIllegalArgument("[%s] must be greater than or equal to 0. Found [%d] in [%s]", k, n, d.name)
				}
				if k == "from" {
					spec.from = n
				} else {
					spec.size = n
				}
			case "explain", "version", "seq_no_primary_term", "track_scores", "stored_fields", "sort":
			case "_source":
				switch v.(type) {
				case bool, string:
				default:
					return errParsing("Expected one of [VALUE_BOOLEAN, VALUE_STRING, START_ARRAY, START_OBJECT] but found [%s]", tok).at(valueTok(body, k))
				}
			default:
				return errParsing("Unknown key for a %s in [%s].", tok, k).at(valueTok(body, k))
			}
		}
	}
	d.spec = spec
	return nil
}

// javaIntValue is XContentParser.intValue for a value token.
func javaIntValue(v any) (int, *Error) {
	if s, ok := v.(string); ok {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || !javaDoubleString(s) {
			return 0, aggNumberFormatError(s)
		}
		return javaInt(f), nil
	}
	f, _ := toFloat(v)
	return javaInt(f), nil
}

func prepareTopHits(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*topHitsSpec)
	limit := getInt(getMap(pc.ix.Settings, "index"), "max_inner_result_window", 100)
	if s, ok := pc.ix.Settings["index.max_inner_result_window"]; ok {
		if n, ok := toFloat(s); ok {
			limit = int(n)
		}
	}
	if spec.from+spec.size > limit {
		return errIllegalArgument("Top hits result window is too large, the top hits aggregator [%s]'s from + size must be less than or equal to: [%d] but was [%d]. This limit can be set by changing the [index.max_inner_result_window] index level setting.", d.name, limit, spec.from+spec.size)
	}
	return nil
}

func collectTopHits(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*topHitsSpec)
	body := spec.body
	if spec.size == 0 && len(hits) > 0 {
		return nil, shardError(errIllegalArgument("numHits must be > 0; please use TotalHitCountCollectorManager if you just need the total hit count"), hits[0].ix)
	}
	sr := &searchRequest{size: spec.size, from: spec.from, trackTotal: -1, totalAsInt: ac.totalAsInt}
	if v, ok := body["sort"]; ok {
		if n, isNum := v.(float64); isNum {
			v = javaNumberString(n, 64)
		} else if n, isNum := toFloat(v); isNum {
			if _, isStr := v.(string); !isStr {
				v = strconv.FormatFloat(n, 'f', -1, 64)
			}
		}
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
		sr.sourceExplicit = true
	}
	if v, ok := body["fields"]; ok {
		sr.fields = getList(v)
	}
	if v, ok := body["docvalue_fields"]; ok {
		sr.docvalueFields = getList(v)
	}
	if _, ok := body["stored_fields"]; ok {
		sr.storedFieldsSet = true
		for _, field := range getStrings(body, "stored_fields") {
			if field == "_none_" {
				sr.storedNone = true
				continue
			}
			sr.storedFields = append(sr.storedFields, field)
		}
	}
	sr.version = getBool(body, "version", false)
	sr.seqNoTerm = getBool(body, "seq_no_primary_term", false)
	sr.trackScores = getBool(body, "track_scores", false)
	if hl, ok := body["highlight"].(M); ok {
		sr.highlight = hl
	}
	copies := make([]*hit, len(hits))
	for i, h := range hits {
		c := *h
		copies[i] = &c
	}
	if err := ac.c.sortHits(copies, sr); err != nil {
		if len(hits) > 0 {
			return nil, shardError(err, hits[0].ix)
		}
		return nil, err
	}
	page := copies
	if sr.from < len(page) {
		page = page[sr.from:]
	} else {
		page = nil
	}
	if len(page) > sr.size {
		page = page[:sr.size]
	}
	hj, err := ac.c.hitsJSON(page, sr, len(hits))
	if err != nil {
		return nil, err
	}
	return &aggResult{kind: resOther, fields: M{"hits": hj}, javaClass: "InternalTopHits"}, nil
}
