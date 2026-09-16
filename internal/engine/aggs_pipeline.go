package engine

import (
	"fmt"
	"math"
	"sort"

	painlessscript "github.com/shibukawa/painlessscript-go"
)

// pipeline aggregations -----------------------------------------------------------------

type pipelineSpec struct {
	paths       []string
	format      *valueFormat
	insertZeros bool
	// percentiles_bucket
	percents []float64
	keyed    bool
	// extended_stats_bucket
	sigma float64
	// derivative
	unit    string
	hasUnit bool
	// serial_diff, moving_avg
	lag, window int
	model       string
	alpha, beta float64
	// bucket_sort
	sorts      []bucketOrder
	from, size int
	hasSize    bool
	// bucket_script, bucket_selector: buckets_path as a name -> path map (a
	// bare string path is its own name); paths holds the same paths, for the
	// pipeline ordering and validation machinery every pipeline type shares.
	pathVars map[string]string
	script   *scriptSpec
}

func pipelinePaths(d *aggDef) []string {
	spec, ok := d.spec.(*pipelineSpec)
	if !ok {
		return nil
	}
	if d.kind == "bucket_sort" {
		var out []string
		for _, s := range spec.sorts {
			if s.path != "_key" {
				out = append(out, s.path)
			}
		}
		return out
	}
	return spec.paths
}

func parseGapPolicy(body M, key string) (bool, error) {
	s, _ := body[key].(string)
	switch s {
	case "skip":
		return false, nil
	case "insert_zeros":
		return true, nil
	}
	return false, errParsing("Invalid gap policy: [%s], accepted values: [insert_zeros, skip]", s).at(valueTok(body, key))
}

func parsePipelineFormat(spec *pipelineSpec, body M) error {
	spec.format = rawFormat
	if f, ok := body["format"].(string); ok {
		vf, err := decimalValueFormat(f)
		if err != nil {
			return err
		}
		spec.format = vf
	}
	return nil
}

// parseBucketMetrics is BucketMetricsParser (sibling pipelines).
func parseBucketMetrics(ps *aggParser, d *aggDef) error {
	spec := &pipelineSpec{sigma: 2, keyed: true, percents: []float64{1, 5, 25, 50, 75, 95, 99}}
	keys := aggBodyKeys(d.body)
	for _, k := range keys {
		v := d.body[k]
		switch t := v.(type) {
		case string:
			switch k {
			case "buckets_path":
				spec.paths = []string{t}
			case "format":
			case "gap_policy":
				iz, err := parseGapPolicy(d.body, k)
				if err != nil {
					return err
				}
				spec.insertZeros = iz
			default:
				return errParsing("Unexpected token VALUE_STRING [%s] in [%s]", k, d.name).at(valueTok(d.body, k))
			}
		case []any:
			switch {
			case k == "buckets_path":
				spec.paths = nil
				for _, p := range t {
					spec.paths = append(spec.paths, missingString(p))
				}
			case k == "percents" && d.kind == "percentiles_bucket":
				spec.percents = nil
				for _, p := range t {
					n, ok := toFloat(p)
					if !ok || n < 0 || n > 100 {
						return errIllegalArgument("percents must only contain non-null doubles from 0.0-100.0 inclusive")
					}
					spec.percents = append(spec.percents, n)
				}
			default:
				return errParsing("Unexpected token START_ARRAY [%s] in [%s]", k, d.name).at(valueTok(d.body, k))
			}
		case bool:
			if k == "keyed" && d.kind == "percentiles_bucket" {
				spec.keyed = t
				continue
			}
			return errParsing("Unexpected token VALUE_BOOLEAN [%s] in [%s]", k, d.name).at(valueTok(d.body, k))
		default:
			if k == "sigma" && d.kind == "extended_stats_bucket" && tokenKind(v) == tkNumber {
				spec.sigma, _ = toFloat(v)
				if spec.sigma < 0 {
					return errIllegalArgument("sigma must be a non-negative double")
				}
				continue
			}
			return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(d.body, k))
		}
	}
	if spec.paths == nil {
		return errParsing("Missing required field [buckets_path] for aggregation [%s]", d.name).at(endTok(d.body))
	}
	if err := parsePipelineFormat(spec, d.body); err != nil {
		return err
	}
	d.spec = spec
	return nil
}

func aggBodyKeys(m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateBucketMetrics is BucketMetricsPipelineAggregationBuilder.validate.
func validateBucketMetrics(vc *validationCtx, d *aggDef) {
	spec := d.spec.(*pipelineSpec)
	if len(spec.paths) != 1 {
		vc.add("buckets_path must contain a single entry for aggregation [%s]", d.name)
		return
	}
	first := spec.paths[0]
	for i, c := range first {
		if c == '>' || c == '.' {
			first = first[:i]
			break
		}
	}
	var siblings []*aggDef
	if vc.parent != nil {
		siblings = vc.parent.subs
	} else {
		siblings = vc.root
	}
	for _, s := range siblings {
		if s.name == first {
			if s.typ.card != cardMany {
				vc.add("The first aggregation in buckets_path must be a multi-bucket aggregation for aggregation [%s] found :%s for buckets path: %s", d.name, s.typ.class, spec.paths[0])
			}
			return
		}
	}
	vc.add("buckets_path aggregation does not exist for aggregation [%s]: %s", d.name, spec.paths[0])
}

func siblingBucketMetrics(ac *aggContext, d *aggDef, level []*aggResult) (*aggResult, error) {
	spec := d.spec.(*pipelineSpec)
	els, err := parseAggPath(spec.paths[0])
	if err != nil {
		return nil, reduceFailure(err)
	}
	path := aggPathStrings(els)
	var keys []string
	var values []float64
	for _, r := range level {
		if r.name != path[0] {
			continue
		}
		if r.kind != resBuckets {
			return nil, reduceFailure(errAggExecution("buckets_path must reference either a number value or a single value numeric metric aggregation, got: [%s] at aggregation [%s]", javaSimpleClass(r), r.name))
		}
		for _, b := range r.buckets {
			v, ok, err := resolveBucketValue(r, b, path[1:], spec.insertZeros)
			if err != nil {
				return nil, reduceFailure(err)
			}
			if ok && !math.IsNaN(v) {
				keys = append(keys, b.keyString)
				values = append(values, v)
			}
		}
	}
	format := spec.format
	switch d.kind {
	case "avg_bucket", "sum_bucket":
		var sum float64
		for _, v := range values {
			sum += v
		}
		value := sum
		if d.kind == "avg_bucket" {
			value = math.NaN()
			if len(values) > 0 {
				value = sum / float64(len(values))
			}
		}
		return simpleValue(value, format), nil
	case "min_bucket", "max_bucket":
		best := math.Inf(-1)
		if d.kind == "min_bucket" {
			best = math.Inf(1)
		}
		var bestKeys []any
		for i, v := range values {
			if (d.kind == "max_bucket" && v > best) || (d.kind == "min_bucket" && v < best) {
				best, bestKeys = v, []any{keys[i]}
			} else if v == best {
				bestKeys = append(bestKeys, keys[i])
			}
		}
		if bestKeys == nil {
			bestKeys = []any{}
		}
		r := &aggResult{kind: resValue, value: best, typed: "bucket_metric_value", javaClass: "InternalBucketMetricValue", fields: M{"keys": bestKeys, "value": nil}}
		if !math.IsInf(best, 0) {
			r.fields["value"] = best
			if !format.raw() {
				r.fields["value_as_string"] = format.stringDouble(best)
			}
		}
		return r, nil
	case "stats_bucket", "extended_stats_bucket":
		min, max := math.Inf(1), math.Inf(-1)
		var sum, sq float64
		for _, v := range values {
			min, max = math.Min(min, v), math.Max(max, v)
			sum += v
			sq += float64(v * v)
		}
		r := statsResult(d.kind, int64(len(values)), sum, sq, min, max, spec.sigma, format)
		r.typed = d.kind
		return r, nil
	case "percentiles_bucket":
		sorted := append([]float64(nil), values...)
		sort.Float64s(sorted)
		pct := func(p float64) float64 {
			if len(sorted) == 0 {
				return math.NaN()
			}
			return sorted[int(javaRound(float64(p/100.0*float64(len(sorted)-1))))]
		}
		r := &aggResult{kind: resMultiValue, typed: "percentiles_bucket", javaClass: "InternalPercentilesBucket"}
		r.metric = func(name string) (float64, error) {
			p, err := aggParseDouble(name, func(s string) *Error { return aggNumberFormatError(s) })
			if err != nil {
				return 0, err
			}
			for _, k := range spec.percents {
				if k == p {
					return pct(p), nil
				}
			}
			return 0, errIllegalArgument("Percent requested [%s] was not one of the computed percentiles. Available keys are: %v", javaDoubleToString(p), spec.percents)
		}
		if spec.keyed {
			vals := M{}
			for _, p := range spec.percents {
				name := javaDoubleToString(p)
				v := pct(p)
				vals[name] = nullable(v)
				if finite(v) && !format.raw() {
					vals[name+"_as_string"] = format.stringDouble(v)
				}
			}
			r.fields = M{"values": vals}
		} else {
			list := make([]any, 0, len(spec.percents))
			for _, p := range spec.percents {
				v := pct(p)
				entry := M{"key": p, "value": nullable(v)}
				if finite(v) && !format.raw() {
					entry["value_as_string"] = format.stringDouble(v)
				}
				list = append(list, entry)
			}
			r.fields = M{"values": list}
		}
		return r, nil
	}
	return nil, errUnsupported("[" + d.kind + "] pipeline aggregation")
}

// simpleValue is InternalSimpleValue.
func simpleValue(v float64, format *valueFormat) *aggResult {
	r := &aggResult{kind: resValue, value: v, typed: "simple_value", javaClass: "InternalSimpleValue", fields: M{"value": nullable(v)}}
	if finite(v) && !format.raw() {
		r.fields["value_as_string"] = format.stringDouble(v)
	}
	return r
}

// parent pipelines ----------------------------------------------------------------------------

func parseParentPipeline(ps *aggParser, d *aggDef) error {
	spec := &pipelineSpec{lag: 1, window: 5, model: "simple", alpha: 0.3, beta: 0.1}
	body := d.body
	if d.kind == "cumulative_sum" {
		of := objFields{name: "cumulative_sum", fields: map[string]int{"buckets_path": vtStringArray, "format": vtString}}
		if err := of.check(body); err != nil {
			return err
		}
	}
	for _, k := range aggBodyKeys(body) {
		v := body[k]
		switch k {
		case "buckets_path":
			switch t := v.(type) {
			case string:
				spec.paths = []string{t}
			case []any:
				for _, p := range t {
					spec.paths = append(spec.paths, missingString(p))
				}
			default:
				return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(d.body, k))
			}
		case "format":
			if _, ok := v.(string); !ok {
				return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(d.body, k))
			}
		case "gap_policy":
			iz, err := parseGapPolicy(body, k)
			if err != nil {
				return err
			}
			spec.insertZeros = iz
		case "unit":
			if d.kind != "derivative" {
				return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(d.body, k))
			}
			spec.unit, spec.hasUnit = missingString(v), true
		case "lag":
			if d.kind != "serial_diff" {
				return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(d.body, k))
			}
			n, _ := toFloat(v)
			if n <= 0 {
				return errParsing("Lag must be a positive, non-zero integer.  Value supplied was%s in [%s]: [lag].", missingString(v), d.name).at(valueTok(body, k))
			}
			spec.lag = int(n)
		case "window":
			if d.kind != "moving_avg" {
				return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(d.body, k))
			}
			n, _ := toFloat(v)
			if n <= 0 {
				return errParsing("[window] value must be a positive, non-zero integer.  Value supplied was [null] in [%s].", d.name).at(valueTok(body, k))
			}
			spec.window = int(n)
		case "model":
			if d.kind != "moving_avg" {
				return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(d.body, k))
			}
			s := missingString(v)
			switch s {
			case "simple", "linear", "ewma":
				spec.model = s
			case "holt", "holt_winters":
				return errUnsupported("[" + s + "] model of [moving_avg] aggregation")
			default:
				return errParsing("no [moving_avg_model] registered for [%s]", s)
			}
		case "settings":
			if d.kind != "moving_avg" {
				return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(d.body, k))
			}
			if m, ok := v.(M); ok {
				if a, ok := toFloat(m["alpha"]); ok {
					spec.alpha = a
				}
			}
		case "predict", "minimize":
			if d.kind != "moving_avg" {
				return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(d.body, k))
			}
			if n, _ := toFloat(v); k == "predict" && n > 0 || k == "minimize" && v == true {
				return errUnsupported("[" + k + "] of [moving_avg] aggregation")
			}
		default:
			return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(d.body, k))
		}
	}
	if spec.paths == nil {
		if d.kind == "cumulative_sum" {
			return errXContent(errIllegalArgument("Required [buckets_path]"), "Failed to build [cumulative_sum] after last required field arrived")
		}
		return errParsing("Missing required field [buckets_path] for %s aggregation [%s]", d.kind, d.name).at(endTok(body))
	}
	if err := parsePipelineFormat(spec, body); err != nil {
		return err
	}
	d.spec = spec
	return nil
}

func validateParentPipeline(vc *validationCtx, d *aggDef) {
	spec := d.spec.(*pipelineSpec)
	if len(spec.paths) != 1 {
		vc.add("buckets_path must contain a single entry for aggregation [%s]", d.name)
	}
	vc.validateParentHistogram(d.kind, d.name)
}

func pipelineBucketPath(spec *pipelineSpec) ([]string, error) {
	els, err := parseAggPath(spec.paths[0])
	if err != nil {
		return nil, err
	}
	return aggPathStrings(els), nil
}

func applyParentPipeline(ac *aggContext, d *aggDef, r *aggResult) error {
	spec := d.spec.(*pipelineSpec)
	if r.kind != resBuckets {
		return nil
	}
	switch d.kind {
	case "bucket_sort":
		return applyBucketSort(ac, d, spec, r)
	case "bucket_script":
		return applyBucketScript(ac, d, spec, r)
	case "bucket_selector":
		return applyBucketSelector(d, spec, r)
	}
	path, err := pipelineBucketPath(spec)
	if err != nil {
		return reduceFailure(err)
	}
	add := func(b *bucket, res *aggResult) {
		ac.finish(d, res)
		b.subs = append(b.subs, res)
	}
	switch d.kind {
	case "cumulative_sum":
		sum := 0.0
		for _, b := range r.buckets {
			v, ok, err := resolveBucketValue(r, b, path, true)
			if err != nil {
				return reduceFailure(err)
			}
			if ok && finite(v) {
				sum += v
			}
			add(b, simpleValue(sum, spec.format))
		}
	case "derivative":
		var units float64
		if spec.hasUnit {
			if u, ok := calendarUnits[spec.unit]; ok {
				units = float64(unitMillis[u])
			} else {
				ms, perr := parseTimeValueMillis(spec.unit, "DerivativePipelineAggregationBuilder.unit")
				if perr != nil {
					return reduceFailure(perr)
				}
				units = float64(ms)
			}
		}
		var lastKey float64
		var last float64
		hasLast := false
		for _, b := range r.buckets {
			v, ok, err := resolveBucketValue(r, b, path, spec.insertZeros)
			if err != nil {
				return reduceFailure(err)
			}
			if hasLast && ok {
				gradient := v - last
				res := &aggResult{kind: resValue, value: gradient, typed: "derivative", javaClass: "InternalDerivative", fields: M{"value": nullable(gradient)}}
				if finite(gradient) && !spec.format.raw() {
					res.fields["value_as_string"] = spec.format.stringDouble(gradient)
				}
				if spec.hasUnit {
					xDiff := (b.keyNum - lastKey) / units
					norm := gradient
					if xDiff > 0 {
						norm = gradient / xDiff
						res.fields["normalized_value"] = nullable(norm)
						if finite(norm) && !spec.format.raw() {
							res.fields["normalized_value_as_string"] = spec.format.stringDouble(norm)
						}
					}
				}
				add(b, res)
			}
			lastKey, last, hasLast = b.keyNum, v, ok
		}
	case "serial_diff":
		var window []float64
		for i, b := range r.buckets {
			v, ok, err := resolveBucketValue(r, b, path, spec.insertZeros)
			if err != nil {
				return reduceFailure(err)
			}
			if !ok {
				v = math.NaN()
			}
			lagValue := math.NaN()
			if i+1 > spec.lag && len(window) > 0 {
				lagValue = window[0]
			}
			if !math.IsNaN(v) && !math.IsNaN(lagValue) {
				add(b, simpleValue(v-lagValue, spec.format))
			}
			window = append(window, v)
			if len(window) > spec.lag {
				window = window[1:]
			}
		}
	case "moving_avg":
		var values []float64
		for _, b := range r.buckets {
			v, ok, err := resolveBucketValue(r, b, path, spec.insertZeros)
			if err != nil {
				return reduceFailure(err)
			}
			if !ok || math.IsNaN(v) {
				continue
			}
			if len(values) > 0 {
				add(b, simpleValue(movingAverage(spec, values), spec.format))
			}
			values = append(values, v)
			if len(values) > spec.window {
				values = values[1:]
			}
		}
	}
	return nil
}

func movingAverage(spec *pipelineSpec, values []float64) float64 {
	switch spec.model {
	case "linear":
		avg, total, current := 0.0, int64(1), int64(1)
		for _, v := range values {
			avg += float64(v * float64(current))
			total += current
			current++
		}
		return avg / float64(total)
	case "ewma":
		var avg float64
		for i, v := range values {
			if i == 0 {
				avg = v
			} else {
				avg = float64(v*spec.alpha) + float64(avg*(1-spec.alpha))
			}
		}
		return avg
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// bucket_sort ---------------------------------------------------------------------------------

func parseBucketSort(ps *aggParser, d *aggDef) error {
	of := objFields{name: "bucket_sort", fields: map[string]int{"sort": vtObjectArrayOrString, "from": vtNumber, "size": vtNumber, "gap_policy": vtString}}
	body := d.body
	wrap := func(err *Error) error {
		// ConstructingObjectParser reports the location of the parse exception
		e := errXContent(err, "failed to build [bucket_sort] after last required field arrived")
		if err.pos != nil {
			e.at(err.pos.at).atParser(err.pos.parser)
		}
		return e
	}
	if err := of.check(body); err != nil {
		return err
	}
	spec := &pipelineSpec{}
	if _, ok := body["from"]; ok {
		n, err := of.intValue(body, "from")
		if err != nil {
			return wrap(err.(*Error))
		}
		if n < 0 {
			return wrap(of.failed(body, "from", errIllegalArgument("[from] must be a non-negative integer: [%d]", n)))
		}
		spec.from = n
	}
	if _, ok := body["size"]; ok {
		n, err := of.intValue(body, "size")
		if err != nil {
			return wrap(err.(*Error))
		}
		if n <= 0 {
			return wrap(of.failed(body, "size", errIllegalArgument("[size] must be a positive integer: [%d]", n)))
		}
		spec.size, spec.hasSize = n, true
	}
	if _, ok := body["gap_policy"]; ok {
		iz, err := parseGapPolicy(body, "gap_policy")
		if err != nil {
			return err
		}
		spec.insertZeros = iz
	}
	for _, item := range getList(body["sort"]) {
		switch t := item.(type) {
		case string:
			spec.sorts = append(spec.sorts, bucketOrder{path: t, asc: t != "_score"})
		case M:
			for _, field := range aggBodyKeys(t) {
				o := bucketOrder{path: field, asc: true}
				switch sv := t[field].(type) {
				case string:
					desc, err := parseSortOrder(sv)
					if err != nil {
						return err
					}
					o.asc = !desc
				case M:
					if order, ok := sv["order"].(string); ok {
						desc, err := parseSortOrder(order)
						if err != nil {
							return err
						}
						o.asc = !desc
					}
				}
				spec.sorts = append(spec.sorts, o)
			}
		}
	}
	for i := range spec.sorts {
		if spec.sorts[i].path != "_key" {
			spec.paths = append(spec.paths, spec.sorts[i].path)
		}
	}
	d.spec = spec
	return nil
}

func validateBucketSort(vc *validationCtx, d *aggDef) {
	vc.validateHasParent(d.kind, d.name)
}

type sortableBucket struct {
	b      *bucket
	values []any // float64 or key values; nil when skipped
}

func applyBucketSort(ac *aggContext, d *aggDef, spec *pipelineSpec, r *aggResult) error {
	buckets := r.buckets
	size := len(buckets)
	if spec.hasSize {
		size = spec.size
	}
	if spec.from >= len(buckets) {
		r.buckets = nil
		return nil
	}
	if len(spec.sorts) == 0 {
		end := spec.from + size
		if end > len(buckets) {
			end = len(buckets)
		}
		r.buckets = append([]*bucket(nil), buckets[spec.from:end]...)
		return nil
	}
	var items []*sortableBucket
	for _, b := range buckets {
		sb := &sortableBucket{b: b, values: make([]any, len(spec.sorts))}
		resolved := 0
		for i, s := range spec.sorts {
			if s.path == "_key" {
				if b.numeric {
					sb.values[i] = b.keyNum
				} else {
					sb.values[i] = b.key
				}
				resolved++
				continue
			}
			els, err := parseAggPath(s.path)
			if err != nil {
				return reduceFailure(err)
			}
			v, ok, err := resolveBucketValue(r, b, aggPathStrings(els), spec.insertZeros)
			if err != nil {
				return reduceFailure(err)
			}
			if !spec.insertZeros && (!ok || math.IsNaN(v)) {
				continue
			}
			if ok {
				sb.values[i] = v
			}
			resolved++
		}
		if resolved > 0 {
			items = append(items, sb)
		}
	}
	compare := func(a, b *sortableBucket) int {
		for i, s := range spec.sorts {
			av, bv := a.values[i], b.values[i]
			switch {
			case av == nil && bv == nil:
				continue
			case av == nil:
				return 1
			case bv == nil:
				return -1
			}
			var c int
			switch at := av.(type) {
			case float64:
				c = javaDoubleCompare(at, bv.(float64))
			case string:
				c = compareStrings(at, bv.(string))
			case int64:
				c = cmpInt64(at, bv.(int64))
			}
			if !s.asc {
				c = -c
			}
			if c != 0 {
				return c
			}
		}
		return 0
	}
	// buckets with equal sort values keep their order
	sort.SliceStable(items, func(i, j int) bool { return compare(items[i], items[j]) < 0 })
	end := spec.from + size
	if end > len(items) {
		end = len(items)
	}
	var out []*bucket
	for i := spec.from; i < end; i++ {
		out = append(out, items[i].b)
	}
	r.buckets = out
	return nil
}

// bucket_script, bucket_selector -----------------------------------------------------------

// parseBucketScriptLike is BucketScriptPipelineAggregationBuilder.PARSER and
// BucketSelectorPipelineAggregationBuilder.PARSER (MultiBucketsPathParser):
// buckets_path is a bare path (bound to the single variable "_value", not
// the path text — the path can contain '>', which is not a legal Painless
// identifier), an array (bound to "_value0", "_value1", ...), or an object
// of name -> path.
func parseBucketScriptLike(ps *aggParser, d *aggDef) error {
	spec := &pipelineSpec{}
	body := d.body
	for _, k := range aggBodyKeys(body) {
		v := body[k]
		switch k {
		case "buckets_path":
			switch t := v.(type) {
			case string:
				spec.pathVars = map[string]string{"_value": t}
			case []any:
				spec.pathVars = make(map[string]string, len(t))
				for i, p := range t {
					spec.pathVars[fmt.Sprintf("_value%d", i)] = missingString(p)
				}
			case M:
				spec.pathVars = make(map[string]string, len(t))
				for _, name := range aggBodyKeys(t) {
					spec.pathVars[name] = missingString(t[name])
				}
			default:
				return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(body, k))
			}
		case "script":
			sc, err := parseScript(bodyReader{}, v, body, k, k)
			if err != nil {
				return err
			}
			spec.script = sc
		case "format":
			if d.kind != "bucket_script" {
				return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(body, k))
			}
			if _, ok := v.(string); !ok {
				return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(body, k))
			}
		case "gap_policy":
			iz, err := parseGapPolicy(body, k)
			if err != nil {
				return err
			}
			spec.insertZeros = iz
		default:
			return errParsing("Unexpected token %s [%s] in [%s]", jsonTokenName(v), k, d.name).at(valueTok(body, k))
		}
	}
	if len(spec.pathVars) == 0 {
		return errParsing("Missing required field [buckets_path] for %s aggregation [%s]", d.kind, d.name).at(endTok(body))
	}
	if spec.script == nil {
		return errParsing("Missing required field [script] for %s aggregation [%s]", d.kind, d.name).at(endTok(body))
	}
	for _, path := range spec.pathVars {
		spec.paths = append(spec.paths, path)
	}
	sort.Strings(spec.paths)
	if d.kind == "bucket_script" {
		if err := parsePipelineFormat(spec, body); err != nil {
			return err
		}
	}
	d.spec = spec
	return nil
}

func validateBucketScriptLike(vc *validationCtx, d *aggDef) {
	vc.validateHasParent(d.kind, d.name)
}

// parsePathVars pre-parses every buckets_path once per pipeline run, rather
// than once per bucket, and lists the variable names in a fixed (sorted)
// order so a multi-path resolution failure always names the same one.
func parsePathVars(pathVars map[string]string) ([]string, map[string][]string, error) {
	names := make([]string, 0, len(pathVars))
	for name := range pathVars {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string][]string, len(pathVars))
	for _, name := range names {
		els, err := parseAggPath(pathVars[name])
		if err != nil {
			return nil, nil, err
		}
		out[name] = aggPathStrings(els)
	}
	return names, out, nil
}

// resolvePathVarValues resolves every buckets_path of spec against b, in
// the fixed order names lists.
//
// bucket_script (skipGaps) matches BucketScriptPipelineAggregator: under
// gap_policy skip (the default), any unresolvable path or a value that
// fails the gap policy (NaN) makes the whole bucket ineligible (ok=false)
// — the caller leaves the bucket unchanged and never runs the script.
//
// bucket_selector (!skipGaps) matches BucketSelectorPipelineAggregator,
// which has no such skip: an unresolvable path becomes an explicit null
// and a gap becomes NaN, and the script itself decides via its return
// value, the same way OpenSearch always runs the selector script.
func resolvePathVarValues(r *aggResult, b *bucket, spec *pipelineSpec, names []string, paths map[string][]string, skipGaps bool) (map[string]painlessscript.Value, bool, error) {
	values := make(map[string]painlessscript.Value, len(names))
	for _, name := range names {
		v, ok, err := resolveBucketValue(r, b, paths[name], spec.insertZeros)
		if err != nil {
			return nil, false, err
		}
		switch {
		case ok && !math.IsNaN(v):
			values[name] = painlessscript.Float64Value(v)
		case skipGaps:
			return nil, false, nil
		case !ok:
			values[name] = painlessscript.NullValue()
		default: // ok && NaN: resolved, but failed the gap policy
			values[name] = painlessscript.Float64Value(v)
		}
	}
	return values, true, nil
}

// evalBucketScript runs a bucket_script/bucket_selector script: it sees only
// params (its own static params merged with the buckets_path values), never
// a document — doc[...] reports "document fields are unavailable".
func evalBucketScript(sc *scriptSpec, pctx painlessscript.Context, extra map[string]painlessscript.Value) (painlessscript.Value, *Error) {
	prog, params, cerr := sc.compile(pctx)
	if cerr != nil {
		return painlessscript.Value{}, cerr
	}
	merged := make(map[string]painlessscript.Value, len(params)+len(extra))
	for k, v := range params {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	v, err := prog.Eval(painlessscript.EvalContext{Params: merged})
	if err != nil {
		return painlessscript.Value{}, errScriptException("runtime error", sc, asPainlessError(painlessscript.ErrorRuntime, err))
	}
	return v, nil
}

func applyBucketScript(ac *aggContext, d *aggDef, spec *pipelineSpec, r *aggResult) error {
	if _, _, cerr := spec.script.compile(painlessscript.ContextField); cerr != nil {
		return reduceFailure(cerr)
	}
	names, paths, err := parsePathVars(spec.pathVars)
	if err != nil {
		return reduceFailure(err)
	}
	for _, b := range r.buckets {
		values, ok, err := resolvePathVarValues(r, b, spec, names, paths, true)
		if err != nil {
			return reduceFailure(err)
		}
		if !ok {
			continue
		}
		result, serr := evalBucketScript(spec.script, painlessscript.ContextField, values)
		if serr != nil {
			return reduceFailure(serr)
		}
		if result.IsNull() {
			// the script itself decided this bucket gets no value
			// (BucketScriptPipelineAggregator: returned == null)
			continue
		}
		f, isNum := result.Float64()
		if !isNum {
			return reduceFailure(errAggExecution("bucket_script script for aggregation [%s] must return a number", d.name))
		}
		res := simpleValue(f, spec.format)
		ac.finish(d, res)
		b.subs = append(b.subs, res)
	}
	return nil
}

func applyBucketSelector(d *aggDef, spec *pipelineSpec, r *aggResult) error {
	if _, _, cerr := spec.script.compile(painlessscript.ContextFilter); cerr != nil {
		return reduceFailure(cerr)
	}
	names, paths, err := parsePathVars(spec.pathVars)
	if err != nil {
		return reduceFailure(err)
	}
	var out []*bucket
	for _, b := range r.buckets {
		values, _, err := resolvePathVarValues(r, b, spec, names, paths, false)
		if err != nil {
			return reduceFailure(err)
		}
		result, serr := evalBucketScript(spec.script, painlessscript.ContextFilter, values)
		if serr != nil {
			return reduceFailure(serr)
		}
		keep, isBool := result.Bool()
		if !isBool {
			return reduceFailure(errAggExecution("bucket_selector script for aggregation [%s] did not return a boolean", d.name))
		}
		if keep {
			out = append(out, b)
		}
	}
	r.buckets = out
	return nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
