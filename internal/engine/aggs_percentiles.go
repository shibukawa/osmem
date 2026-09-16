package engine

import (
	"math"
	"sort"
	"strconv"
)

// percentiles, percentile_ranks and median_absolute_deviation ------------------------------

type percentilesSpec struct {
	vs          vsConfig
	keys        []float64
	keyed       bool
	compression float64
	hdr         bool
	digits      int
}

var defaultPercents = []float64{1, 5, 25, 50, 75, 95, 99}

func parsePercentiles(ps *aggParser, d *aggDef) error {
	var of objFields
	switch d.kind {
	case "percentiles":
		of = valuesSourceFields(d.kind, true, false, map[string]int{"percents": vtNumberArray, "keyed": vtBool, "tdigest": vtObject, "hdr": vtObject})
	case "percentile_ranks":
		of = valuesSourceFields(d.kind, true, false, map[string]int{"values": vtNumberArray, "keyed": vtBool, "tdigest": vtObject, "hdr": vtObject})
	default:
		of = valuesSourceFields(d.kind, true, false, map[string]int{"compression": vtNumber})
	}
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &percentilesSpec{keyed: true, compression: 100}
	if d.kind == "median_absolute_deviation" {
		spec.compression = 1000
	}
	var err error
	if spec.vs, err = parseVSConfig(of, body); err != nil {
		return err
	}
	if _, ok := body["keyed"]; ok {
		if spec.keyed, err = of.boolValue(body, "keyed"); err != nil {
			return err
		}
	}
	if _, ok := body["compression"]; ok {
		if spec.compression, err = of.doubleValue(body, "compression"); err != nil {
			return err
		}
		if spec.compression <= 0 {
			return of.failed(body, "compression", errIllegalArgument("[compression] must be greater than 0. Found [%s] in [%s]", javaDoubleToString(spec.compression), d.name))
		}
	}
	_, hasTD := body["tdigest"]
	_, hasHDR := body["hdr"]
	if td, ok := body["tdigest"].(M); ok {
		tf := objFields{name: "tdigest", fields: map[string]int{"compression": vtNumber}}
		if err := tf.check(td); err != nil {
			return of.failed(body, "tdigest", err.(*Error))
		}
		if _, ok := td["compression"]; ok {
			c, err := tf.doubleValue(td, "compression")
			if err != nil {
				return of.failed(body, "tdigest", err.(*Error))
			}
			if c < 0 {
				return of.failed(body, "tdigest", tf.failed(td, "compression", errIllegalArgument("[compression] must be greater than or equal to 0. Found [%s]", javaDoubleToString(c))))
			}
			spec.compression = c
		}
	}
	if hdr, ok := body["hdr"].(M); ok {
		hf := objFields{name: "hdr", fields: map[string]int{"number_of_significant_value_digits": vtNumber}}
		if err := hf.check(hdr); err != nil {
			return of.failed(body, "hdr", err.(*Error))
		}
		spec.hdr, spec.digits = true, 3
		if _, ok := hdr["number_of_significant_value_digits"]; ok {
			n, err := hf.intValue(hdr, "number_of_significant_value_digits")
			if err != nil {
				return of.failed(body, "hdr", err.(*Error))
			}
			if n < 0 || n > 5 {
				return of.failed(body, "hdr", hf.failed(hdr, "number_of_significant_value_digits", errIllegalArgument("[numberOfSignificantValueDigits] must be between 0 and 5")))
			}
			spec.digits = n
		}
	}
	build := func(msg string, args ...any) error {
		return errXContent(errIllegalArgument(msg, args...), "Failed to build [%s] after last required field arrived", d.kind)
	}
	if hasTD && hasHDR {
		return build("Only one percentiles method should be declared.")
	}
	switch d.kind {
	case "percentiles":
		spec.keys = append([]float64(nil), defaultPercents...)
		if raw, ok := body["percents"]; ok {
			spec.keys = nil
			for i, v := range getList(raw) {
				n, err := aggParseDouble(v, func(s string) *Error {
					return of.failed(body, "percents", aggNumberFormatError(s).atParser(memberElemTok(body, "percents", i)))
				})
				if err != nil {
					return err
				}
				spec.keys = append(spec.keys, n)
			}
			if len(spec.keys) == 0 {
				return build("[percents] must not be empty: [%s]", d.name)
			}
		}
		for _, p := range spec.keys {
			if p < 0 || p > 100 {
				return build("percent must be in [0,100], got [%s]: [%s]", javaDoubleToString(p), d.name)
			}
		}
	case "percentile_ranks":
		raw, ok := body["values"]
		if !ok {
			return build("[values] must not be null: [%s]", d.name)
		}
		for i, v := range getList(raw) {
			n, err := aggParseDouble(v, func(s string) *Error {
				return of.failed(body, "values", aggNumberFormatError(s).atParser(memberElemTok(body, "values", i)))
			})
			if err != nil {
				return err
			}
			spec.keys = append(spec.keys, n)
		}
		if len(spec.keys) == 0 {
			return build("[values] must not be an empty array: [%s]", d.name)
		}
	}
	sort.Float64s(spec.keys)
	if err := requireFieldOrScript(body); err != nil {
		return err
	}
	if spec.hdr {
		return errUnsupported("[hdr] method of [" + d.kind + "] aggregation")
	}
	if spec.vs.script {
		return errScript(d)
	}
	d.spec = spec
	return nil
}

func percentilesHasMetric(d *aggDef, name string) bool {
	spec, ok := d.spec.(*percentilesSpec)
	if !ok {
		return false
	}
	v, err := strconv.ParseFloat(name, 64)
	if err != nil {
		return false
	}
	for _, k := range spec.keys {
		if k == v {
			return true
		}
	}
	return false
}

func preparePercentiles(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*percentilesSpec)
	_, err := pc.resolve(d, 0, &spec.vs, d.kind, vsNumeric, numericKinds...)
	return err
}

// percentileState is the reduced sketch of a percentiles aggregation.
type percentileState interface {
	quantile(q float64) float64 // q in [0, 1]
	rank(v float64) float64     // percent
	count() int64
}

type tdigestState struct{ td *mergingDigest }

func (s tdigestState) quantile(q float64) float64 { return s.td.quantile(q) }
func (s tdigestState) rank(v float64) float64     { return float64(s.td.cdf(v) * 100) }
func (s tdigestState) count() int64               { return s.td.size() }

func (ac *aggContext) percentileState(d *aggDef, hits []*hit) (percentileState, error) {
	spec := d.spec.(*percentilesSpec)
	if spec.hdr {
		return ac.hdrState(d, spec, hits)
	}
	shards := map[*Index]*mergingDigest{}
	for _, h := range hits {
		vs := ac.source(d, 0, h.ix)
		if vs == nil {
			continue
		}
		td := shards[h.ix]
		if td == nil {
			td = newMergingDigest(spec.compression)
			shards[h.ix] = td
		}
		for _, v := range vs.nums(h) {
			td.add(v, 1)
		}
	}
	merged := newMergingDigest(spec.compression)
	for _, ix := range ac.indices {
		if td := shards[ix]; td != nil {
			merged.addDigest(td)
		}
	}
	return tdigestState{merged}, nil
}

func collectPercentiles(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*percentilesSpec)
	state, err := ac.percentileState(d, hits)
	if err != nil {
		return nil, err
	}
	format := ac.firstFormat(d, 0)
	if d.kind == "median_absolute_deviation" {
		mad := math.NaN()
		if td, ok := state.(tdigestState); ok && td.td.size() > 0 {
			mad = medianAbsoluteDeviation(td.td, spec.compression)
		}
		r := &aggResult{kind: resValue, value: mad, fields: M{"value": nil}, javaClass: "InternalMedianAbsoluteDeviation"}
		if state.count() > 0 {
			r.fields["value"] = mad
			if !format.raw() {
				r.fields["value_as_string"] = format.stringDouble(mad)
			}
		}
		return r, nil
	}
	ranks := d.kind == "percentile_ranks"
	value := func(key float64) float64 {
		if ranks {
			return state.rank(key)
		}
		return state.quantile(key / 100)
	}
	r := &aggResult{kind: resMultiValue, javaClass: "InternalTDigestPercentiles", typed: "tdigest_percentiles"}
	switch {
	case ranks && spec.hdr:
		r.javaClass, r.typed = "InternalHDRPercentileRanks", "hdr_percentile_ranks"
	case ranks:
		r.javaClass, r.typed = "InternalTDigestPercentileRanks", "tdigest_percentile_ranks"
	case spec.hdr:
		r.javaClass, r.typed = "InternalHDRPercentiles", "hdr_percentiles"
	}
	r.metric = func(name string) (float64, error) {
		k, err := aggParseDouble(name, func(s string) *Error { return aggNumberFormatError(s) })
		if err != nil {
			return 0, err
		}
		return value(k), nil
	}
	empty := state.count() == 0
	if spec.keyed {
		values := M{}
		for _, k := range spec.keys {
			name := javaDoubleToString(k)
			v := value(k)
			if empty {
				values[name] = nil
				continue
			}
			values[name] = v
			if !format.raw() {
				values[name+"_as_string"] = format.stringDouble(v)
			}
		}
		r.fields = M{"values": values}
		return r, nil
	}
	list := make([]any, 0, len(spec.keys))
	for _, k := range spec.keys {
		entry := M{"key": k, "value": nil}
		if !empty {
			v := value(k)
			entry["value"] = v
			if !format.raw() {
				entry["value_as_string"] = format.stringDouble(v)
			}
		}
		list = append(list, entry)
	}
	r.fields = M{"values": list}
	return r, nil
}

// medianAbsoluteDeviation is InternalMedianAbsoluteDeviation.computeMedianAbsoluteDeviation.
func medianAbsoluteDeviation(sketch *mergingDigest, compression float64) float64 {
	median := sketch.quantile(0.5)
	deviations := newMergingDigest(compression)
	for _, c := range sketch.centroids() {
		deviations.add(math.Abs(median-c.mean), c.count)
	}
	return deviations.quantile(0.5)
}

func (ac *aggContext) hdrState(d *aggDef, spec *percentilesSpec, hits []*hit) (percentileState, error) {
	return nil, errUnsupported("[hdr] method of [" + d.kind + "] aggregation")
}
