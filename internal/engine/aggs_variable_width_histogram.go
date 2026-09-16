package engine

import "sort"

// variable_width_histogram ------------------------------------------------------------
//
// Real OpenSearch builds variable-width buckets with a single streaming
// pass of agglomerative clustering, seeded from a shard_size-large sample
// (VariableWidthHistogramAggregator) — inherently approximate and
// algorithm-dependent even there. osmem instead sorts every (in-memory)
// value and splits at the `buckets-1` largest gaps between consecutive
// distinct values, which is a simpler single-pass clustering that targets
// the right *number* of buckets and sane per-bucket key/min/max/doc_count;
// it reproduces the worked example in OpenSearch's own docs exactly, but is
// not meant to bit-match arbitrary real clustering results.

var variableWidthHistogramFields = valuesSourceFields("variable_width_histogram", false, false, map[string]int{
	"buckets": vtNumber, "shard_size": vtNumber, "initial_buffer": vtNumber,
})

type variableWidthHistogramSpec struct {
	vs      vsConfig
	buckets int
}

func parseVariableWidthHistogram(ps *aggParser, d *aggDef) error {
	of := variableWidthHistogramFields
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &variableWidthHistogramSpec{buckets: 10}
	var err error
	if spec.vs, err = parseVSConfig(of, body); err != nil {
		return err
	}
	if _, ok := body["buckets"]; ok {
		n, err := of.intValue(body, "buckets")
		if err != nil {
			return err
		}
		if n <= 0 {
			return of.failed(body, "buckets", errIllegalArgument("[buckets] must be greater than 0 for [%s]", d.name))
		}
		spec.buckets = n
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

func prepareVariableWidthHistogram(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*variableWidthHistogramSpec)
	_, err := pc.resolve(d, 0, &spec.vs, "variable_width_histogram", vsNumeric, vsNumeric)
	return err
}

func collectVariableWidthHistogram(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*variableWidthHistogramSpec)
	type distinct struct {
		v    float64
		hits []*hit
	}
	var vals []distinct
	add := func(v float64, h *hit) {
		if n := len(vals); n > 0 && vals[n-1].v == v {
			vals[n-1].hits = append(vals[n-1].hits, h)
			return
		}
		vals = append(vals, distinct{v: v, hits: []*hit{h}})
	}
	var raw []struct {
		v float64
		h *hit
	}
	for _, h := range hits {
		vs := ac.source(d, 0, h.ix)
		if vs == nil {
			continue
		}
		for _, v := range vs.nums(h) {
			raw = append(raw, struct {
				v float64
				h *hit
			}{v, h})
		}
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].v < raw[j].v })
	for _, r := range raw {
		add(r.v, r.h)
	}

	target := spec.buckets
	if target > len(vals) {
		target = len(vals)
	}
	var buckets []*bucket
	if target > 0 {
		type gap struct {
			at   int // split point: between vals[at-1] and vals[at]
			size float64
		}
		gaps := make([]gap, 0, len(vals)-1)
		for i := 1; i < len(vals); i++ {
			gaps = append(gaps, gap{i, vals[i].v - vals[i-1].v})
		}
		sort.SliceStable(gaps, func(i, j int) bool { return gaps[i].size > gaps[j].size })
		nSplits := target - 1
		if nSplits > len(gaps) {
			nSplits = len(gaps)
		}
		splits := make([]int, nSplits)
		for i := range splits {
			splits[i] = gaps[i].at
		}
		sort.Ints(splits)
		splits = append(splits, len(vals))

		start := 0
		for _, end := range splits {
			var sum float64
			var count int64
			var hs []*hit
			for _, dv := range vals[start:end] {
				sum += dv.v * float64(len(dv.hits))
				count += int64(len(dv.hits))
				hs = append(hs, dv.hits...)
			}
			key := sum / float64(count)
			buckets = append(buckets, &bucket{keyNum: key, numeric: true, sortKey: key, docCount: count, hits: hs,
				fields: M{"min": vals[start].v, "max": vals[end-1].v}})
			start = end
		}
	}

	if err := ac.collectSubs(d, buckets); err != nil {
		return nil, err
	}
	format := ac.firstFormat(d, 0)
	for _, b := range buckets {
		b.key = b.keyNum
		b.keyString = format.stringDouble(b.keyNum)
		b.asString = !format.raw()
	}
	return &aggResult{kind: resBuckets, buckets: buckets, javaClass: "InternalVariableWidthHistogram"}, nil
}
