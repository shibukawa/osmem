package engine

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// auto_date_histogram ----------------------------------------------------------------
//
// Picks the finest rounding from a fixed rotation table such that bucketing
// the data at that rounding does not exceed the requested `buckets` target,
// then buckets like date_histogram at that rounding (AutoDateHistogramAggregationBuilder
// / RoundingInfo). Real OpenSearch approximates this adaptively while
// streaming per-segment; osmem holds all data in memory, so it computes the
// exact min/max of the values and picks the rounding directly. The rotation
// table (1s,5s,10s,30s,1m,5m,10m,30m,1h,3h,12h,1d,7d,1M,3M,1y,5y,10y,20y,50y,100y)
// and its behavior (finest rounding whose count is <= target; ties/edge
// cases) were confirmed against a live OpenSearch 3.8.0 instance.

type autoRounder interface {
	round(utc int64) int64
	next(utc int64) int64
}

// yearMultipleRounding rounds down to the nearest multiple-of-n year; used
// for the tiers coarser than a single calendar year, which dateRounding
// does not model. Unlike the tiers below, real OpenSearch likely anchors
// this to the data's own starting year too (as confirmed for the other
// multi-unit tiers), but that is unverified here — none of the confirmed
// differing requests reach a range this wide, so this keeps the simpler
// epoch/proleptic-anchored form.
type yearMultipleRounding struct {
	loc *time.Location
	n   int
}

func (r *yearMultipleRounding) round(utc int64) int64 {
	y := time.UnixMilli(utc).In(r.loc).Year()
	y -= ((y % r.n) + r.n) % r.n
	return time.Date(y, 1, 1, 0, 0, 0, 0, r.loc).UnixMilli()
}

func (r *yearMultipleRounding) next(utc int64) int64 {
	y := time.UnixMilli(r.round(utc)).In(r.loc).Year() + r.n
	return time.Date(y, 1, 1, 0, 0, 0, 0, r.loc).UnixMilli()
}

// originRounding tiles fixed-size buckets from a given origin instead of
// from the epoch. Confirmed against a live server: every multi-unit tier
// (5s/10s/30s, 5m/10m/30m, 3h/12h, 7d) anchors to the data's own minimum
// value rounded down to its base unit, not to a fixed reference since
// epoch — e.g. a 12h tier over data starting at 01:17 buckets from 01:00,
// not from midnight/noon (InternalAutoDateHistogram merges consecutive
// base-unit buckets starting from whichever one is first collected). Single
// -unit tiers (1s,1m,1h,1d,1M,1y) have no such ambiguity and use calendarTier
// instead, which stays correct across DST transitions.
type originRounding struct {
	origin int64
	stepMs int64
}

func (r *originRounding) round(utc int64) int64 {
	return r.origin + roundKey(utc-r.origin, r.stepMs)*r.stepMs
}
func (r *originRounding) next(utc int64) int64 { return r.round(utc) + r.stepMs }

type autoDateTier struct {
	label    string
	unitName string // for minimum_interval: second/minute/hour/day/month/year
	fixedMs  int64  // 0 for calendar-based tiers
	make     func(loc *time.Location, min int64) autoRounder
}

func calendarTier(label, unitName string, unit int) autoDateTier {
	return autoDateTier{label: label, unitName: unitName,
		make: func(loc *time.Location, min int64) autoRounder { return &dateRounding{unit: unit, loc: loc} }}
}

// multipleTier groups `multiple` consecutive base-unit (second/minute/hour/
// day) spans into one bucket, anchored to the data's own minimum value.
func multipleTier(label, unitName string, unit, multiple int) autoDateTier {
	stepMs := unitMillis[unit] * int64(multiple)
	return autoDateTier{label: label, unitName: unitName, fixedMs: stepMs,
		make: func(loc *time.Location, min int64) autoRounder {
			origin := (&dateRounding{unit: unit, loc: loc}).round(min)
			return &originRounding{origin: origin, stepMs: stepMs}
		}}
}

var autoDateTiers = []autoDateTier{
	calendarTier("1s", "second", unitSecond),
	multipleTier("5s", "second", unitSecond, 5),
	multipleTier("10s", "second", unitSecond, 10),
	multipleTier("30s", "second", unitSecond, 30),
	calendarTier("1m", "minute", unitMinute),
	multipleTier("5m", "minute", unitMinute, 5),
	multipleTier("10m", "minute", unitMinute, 10),
	multipleTier("30m", "minute", unitMinute, 30),
	calendarTier("1h", "hour", unitHour),
	multipleTier("3h", "hour", unitHour, 3),
	multipleTier("12h", "hour", unitHour, 12),
	calendarTier("1d", "day", unitDay),
	multipleTier("7d", "day", unitDay, 7),
	calendarTier("1M", "month", unitMonth),
	calendarTier("3M", "month", unitQuarter),
	calendarTier("1y", "year", unitYear),
	{label: "5y", unitName: "year", make: func(loc *time.Location, min int64) autoRounder { return &yearMultipleRounding{loc: loc, n: 5} }},
	{label: "10y", unitName: "year", make: func(loc *time.Location, min int64) autoRounder { return &yearMultipleRounding{loc: loc, n: 10} }},
	{label: "20y", unitName: "year", make: func(loc *time.Location, min int64) autoRounder { return &yearMultipleRounding{loc: loc, n: 20} }},
	{label: "50y", unitName: "year", make: func(loc *time.Location, min int64) autoRounder { return &yearMultipleRounding{loc: loc, n: 50} }},
	{label: "100y", unitName: "year", make: func(loc *time.Location, min int64) autoRounder { return &yearMultipleRounding{loc: loc, n: 100} }},
}

// autoMinIntervalIndex resolves minimum_interval to the first (finest) tier
// of that calendar unit.
func autoMinIntervalIndex(name string) (int, bool) {
	for i, t := range autoDateTiers {
		if t.unitName == name {
			return i, true
		}
	}
	return 0, false
}

// autoTierCount estimates the bucket count of a tier over [min,max]: exact
// arithmetic for fixed-interval tiers (which can span too many buckets to
// enumerate), a bounded exact loop for calendar tiers (whose count stays
// small even over centuries, since coarser tiers are only reached once
// finer ones already overflow the target).
func autoTierCount(tier autoDateTier, loc *time.Location, min, max int64) int64 {
	r := tier.make(loc, min)
	rMin, rMax := r.round(min), r.round(max)
	if tier.fixedMs > 0 {
		return (rMax-rMin)/tier.fixedMs + 1
	}
	count := int64(1)
	for k := rMin; k < rMax; {
		k = r.next(k)
		count++
		if count > 1_000_000 {
			break
		}
	}
	return count
}

var autoDateHistogramFields = valuesSourceFields("auto_date_histogram", true, true, map[string]int{
	"buckets": vtNumber, "minimum_interval": vtString,
})

type autoDateHistogramSpec struct {
	vs      vsConfig
	buckets int
	minTier int
}

func parseAutoDateHistogram(ps *aggParser, d *aggDef) error {
	of := autoDateHistogramFields
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &autoDateHistogramSpec{buckets: 10}
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
			return of.failed(body, "buckets", errIllegalArgument("buckets must be greater than 0 for [%s]", d.name))
		}
		spec.buckets = n
	}
	if v, ok := body["minimum_interval"]; ok {
		s, _ := v.(string)
		idx, known := autoMinIntervalIndex(s)
		if !known {
			return of.failed(body, "minimum_interval", errIllegalArgument("minimum_interval must be one of [[day, second, minute, hour, year, month]]"))
		}
		spec.minTier = idx
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

// autoMaxRoundingInterval is the largest inner interval of the roundings
// the minimum_interval leaves (AutoDateHistogramAggregationBuilder.innerBuild
// takes it over every rounding but the last, the year one); 0 when none is
// left.
func autoMaxRoundingInterval(minTier int) int {
	max := 0
	for _, t := range autoDateTiers[minTier:] {
		if t.unitName == "year" {
			continue
		}
		// the multiplier is the number the label starts with (5s, 3M)
		n, _ := strconv.Atoi(strings.TrimRight(t.label, "smhdMy"))
		if n > max {
			max = n
		}
	}
	return max
}

func prepareAutoDateHistogram(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*autoDateHistogramSpec)
	// the target must leave room for the finest rounding to overshoot it
	// by the largest interval multiplier before search.max_buckets
	if maxInterval := autoMaxRoundingInterval(spec.minTier); maxInterval > 0 {
		if ceiling := pc.ac.bucketLimit() / maxInterval; spec.buckets > ceiling {
			return errIllegalArgument("buckets must be less than %d", ceiling)
		}
	}
	_, err := pc.resolve(d, 0, &spec.vs, "auto_date_histogram", vsDate, vsDate, vsNumeric, vsBoolean)
	return err
}

func collectAutoDateHistogram(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*autoDateHistogramSpec)
	loc := time.UTC
	if spec.vs.hasZone {
		loc = spec.vs.zone
	}

	// values in doc order, one entry per (hit, value) so sub-aggregations see
	// every match of a multi-valued field, as date_histogram does
	type valAt struct {
		v float64
		h *hit
	}
	var vals []valAt
	min, max := int64(0), int64(0)
	first := true
	for _, h := range hits {
		vs := ac.source(d, 0, h.ix)
		if vs == nil {
			continue
		}
		for _, v := range vs.nums(h) {
			vals = append(vals, valAt{v, h})
			iv := int64(v)
			if first || iv < min {
				min = iv
			}
			if first || iv > max {
				max = iv
			}
			first = false
		}
	}

	tierIdx := len(autoDateTiers) - 1
	if !first { // there is at least one value; the coarsest tier is otherwise used vacuously
		for i := spec.minTier; i < len(autoDateTiers); i++ {
			if autoTierCount(autoDateTiers[i], loc, min, max) <= int64(spec.buckets) {
				tierIdx = i
				break
			}
		}
	} else if spec.minTier > 0 {
		tierIdx = spec.minTier
	} else {
		tierIdx = 0
	}
	tier := autoDateTiers[tierIdx]
	r := tier.make(loc, min)

	groups := map[int64]*bucket{}
	for _, va := range vals {
		key := r.round(int64(va.v))
		b, ok := groups[key]
		if !ok {
			b = &bucket{keyNum: float64(key), numeric: true, sortKey: float64(key)}
			groups[key] = b
			if err := ac.checkBuckets(len(groups)); err != nil {
				return nil, err
			}
		}
		b.docCount++
		b.hits = append(b.hits, va.h)
	}
	buckets := make([]*bucket, 0, len(groups))
	for _, b := range groups {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].keyNum < buckets[j].keyNum })

	// fill empty buckets across the full observed span, like date_histogram
	// with min_doc_count 0 (InternalAutoDateHistogram.addEmptyBuckets)
	// (the fill counts against search.max_buckets bucket by bucket, so a
	// span too wide for the limit fails before it is materialized)
	if len(buckets) > 0 {
		filled := make([]*bucket, 0, len(buckets))
		add := func(b *bucket) error {
			if err := ac.checkBuckets(len(filled) + 1); err != nil {
				return err
			}
			filled = append(filled, b)
			return nil
		}
		for i, b := range buckets {
			if i > 0 {
				for k := r.next(int64(buckets[i-1].keyNum)); k < int64(b.keyNum); k = r.next(k) {
					if err := add(&bucket{keyNum: float64(k), numeric: true, sortKey: float64(k)}); err != nil {
						return nil, err
					}
				}
			}
			if err := add(b); err != nil {
				return nil, err
			}
		}
		buckets = filled
	}

	if err := ac.collectSubs(d, buckets); err != nil {
		return nil, err
	}
	format := ac.firstFormat(d, 0)
	for _, b := range buckets {
		k := int64(b.keyNum)
		b.key = k
		b.keyString = format.stringLong(k)
		b.asString = !format.raw()
	}
	return &aggResult{kind: resBuckets, buckets: buckets, javaClass: "InternalAutoDateHistogram",
		fields: M{"interval": tier.label}}, nil
}
