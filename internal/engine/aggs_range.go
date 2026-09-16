package engine

import (
	"bytes"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// range, date_range, ip_range and geo_distance ---------------------------------------------------

type rangeEntry struct {
	key           string
	hasKey        bool
	from, to      any // raw bounds (nil when absent)
	fromN, toN    float64
	fromIP, toIP  []byte // ip_range bounds (nil when open)
	fromS, toS    string
	order         int
	unboundedFrom bool
}

type rangeAggSpec struct {
	vs     vsConfig
	keyed  bool
	ranges []rangeEntry
	// geo_distance
	origin     [2]float64
	unitMeters float64
	plane      bool
	hasOrigin  bool
	originRaw  any
	noRanges   bool
}

func rangeFields(kind string) objFields {
	switch kind {
	case "date_range":
		return valuesSourceFields(kind, true, true, map[string]int{"keyed": vtBool, "ranges": vtObjectArray})
	case "ip_range":
		return valuesSourceFields(kind, false, false, map[string]int{"keyed": vtBool, "ranges": vtObjectArray})
	case "geo_distance":
		f := valuesSourceFields(kind, false, false, map[string]int{"keyed": vtBool, "ranges": vtObjectArray, "origin": vtAny, "unit": vtString, "distance_type": vtString})
		delete(f.fields, "value_type")
		return f
	}
	return valuesSourceFields(kind, true, false, map[string]int{"keyed": vtBool, "ranges": vtObjectArray})
}

var aggDistanceUnits = map[string]float64{
	"in": 0.0254, "inch": 0.0254, "yd": 0.9144, "yards": 0.9144, "ft": 0.3048, "feet": 0.3048, "km": 1000, "kilometers": 1000,
	"NM": 1852, "nmi": 1852, "nauticalmiles": 1852, "mm": 0.001, "millimeters": 0.001, "cm": 0.01, "centimeters": 0.01,
	"mi": 1609.344, "miles": 1609.344, "m": 1, "meters": 1,
}

func parseRangeAgg(ps *aggParser, d *aggDef) error {
	of := rangeFields(d.kind)
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &rangeAggSpec{unitMeters: 1}
	var err error
	if spec.vs, err = parseVSConfig(of, body); err != nil {
		return err
	}
	if _, ok := body["keyed"]; ok {
		if spec.keyed, err = of.boolValue(body, "keyed"); err != nil {
			return err
		}
	}
	if u, ok := body["unit"].(string); ok {
		m, known := aggDistanceUnits[u]
		if !known {
			return of.failed(body, "unit", errIllegalArgument("No distance unit match [%s]", u))
		}
		spec.unitMeters = m
	}
	if dt, ok := body["distance_type"].(string); ok {
		switch strings.ToLower(dt) {
		case "arc":
		case "plane":
			spec.plane = true
		default:
			return of.failed(body, "distance_type", errIllegalArgument("No geo distance for [%s]", dt))
		}
	}
	raw, hasRanges := body["ranges"]
	if !hasRanges {
		spec.noRanges = true
	}
	entryFields := objFields{name: d.kind, fields: map[string]int{"key": vtString, "from": vtValue, "to": vtValue}}
	if d.kind == "ip_range" {
		entryFields.fields["mask"] = vtString
	}
	items := getList(raw)
	for i, item := range items {
		em, ok := item.(M)
		if !ok {
			// ObjectParser reads past the element and names the token after
			// it, the end of the array for the last element
			at := noTok
			if i == len(items)-1 {
				at = valueEndTok(body, "ranges")
			}
			return of.failed(body, "ranges", errXContent(nil, "[%s] Expected START_OBJECT but was: END_ARRAY", d.kind).at(at))
		}
		if err := entryFields.check(em); err != nil {
			return of.failed(body, "ranges", err.(*Error))
		}
		e := rangeEntry{order: i}
		if k, ok := em["key"].(string); ok {
			e.key, e.hasKey = k, true
		}
		e.from, e.to = em["from"], em["to"]
		if d.kind == "ip_range" {
			if err := parseIPRangeEntry(&e, em); err != nil {
				if err.parserTok() == nil {
					err.atParser(endTok(em))
				}
				return of.failed(body, "ranges", err)
			}
		}
		spec.ranges = append(spec.ranges, e)
	}
	if d.kind == "geo_distance" {
		o, ok := body["origin"]
		if !ok {
			return errIllegalArgument("Aggregation [%s] must define an [origin].", d.name)
		}
		lat, lon, valid := geoPointValue(o)
		if !valid {
			return of.failed(body, "origin", &Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "failed to parse [origin]"})
		}
		spec.origin, spec.hasOrigin = [2]float64{lat, lon}, true
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

func parseIPRangeEntry(e *rangeEntry, em M) *Error {
	parse := func(v any) ([]byte, string, *Error) {
		if v == nil {
			return nil, "", nil
		}
		s := missingString(v)
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, "", errIllegalArgument("'%s' is not an IP string literal.", s)
		}
		return ip.To16(), canonicalIP(s), nil
	}
	var err *Error
	if mask, ok := em["mask"].(string); ok {
		parts := strings.Split(mask, "/")
		if len(parts) != 2 {
			return errIllegalArgument("Expected [ip/prefix] but got [%s]", mask)
		}
		ip := net.ParseIP(parts[0])
		if ip == nil {
			return errIllegalArgument("'%s' is not an IP string literal.", parts[0])
		}
		addr := ip.To4()
		if addr == nil {
			addr = ip.To16()
		}
		prefix, perr := strconv.Atoi(parts[1])
		if perr != nil {
			return aggNumberFormatError(parts[1])
		}
		if prefix < 0 || prefix > 8*len(addr) {
			return errIllegalArgument("Illegal prefix length [%d] in [%s]. Must be 0-32 for IPv4 ranges, 0-128 for IPv6 ranges", prefix, mask)
		}
		lower := append([]byte(nil), addr...)
		upper := append([]byte(nil), addr...)
		for i := prefix; i < 8*len(lower); i++ {
			m := byte(1 << (7 - (i & 7)))
			lower[i>>3] &^= m
			upper[i>>3] |= m
		}
		if !e.hasKey {
			e.key, e.hasKey = mask, true
		}
		if !bytes.Equal(net.IP(lower).To16(), make([]byte, 16)) || len(lower) == 4 {
			e.fromIP, e.fromS = net.IP(lower).To16(), net.IP(lower).String()
		}
		allOnes := len(upper) == 16 && bytes.Equal(upper, bytes.Repeat([]byte{0xff}, 16))
		if !allOnes {
			next := append([]byte(nil), upper...)
			for i := len(next) - 1; i >= 0; i-- {
				if next[i] != 0xff {
					next[i]++
					break
				}
				next[i] = 0
			}
			e.toIP, e.toS = net.IP(next).To16(), net.IP(next).String()
		}
		return nil
	}
	if e.fromIP, e.fromS, err = parse(e.from); err != nil {
		return err
	}
	if e.toIP, e.toS, err = parse(e.to); err != nil {
		return err
	}
	return nil
}

func prepareRange(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*rangeAggSpec)
	var vs *valuesSource
	var err error
	switch d.kind {
	case "range":
		vs, err = pc.resolve(d, 0, &spec.vs, "range", vsNumeric, vsNumeric, vsDate, vsBoolean)
	case "date_range":
		vs, err = pc.resolve(d, 0, &spec.vs, "date_range", vsDate, vsNumeric, vsDate, vsBoolean)
	case "ip_range":
		vs, err = pc.resolve(d, 0, &spec.vs, "ip_range", vsIP, vsIP)
	case "geo_distance":
		vs, err = pc.resolve(d, 0, &spec.vs, "geo_distance", vsGeoPoint, vsGeoPoint)
	}
	if err != nil {
		return err
	}
	if len(spec.ranges) == 0 {
		return errIllegalArgument("No [ranges] specified for the [%s] aggregation", d.name)
	}
	if d.kind == "ip_range" || d.kind == "geo_distance" {
		return nil
	}
	resolved := make([]rangeEntry, len(spec.ranges))
	for i, e := range spec.ranges {
		for _, side := range []struct {
			raw  any
			n    *float64
			open float64
		}{{e.from, &e.fromN, math.Inf(-1)}, {e.to, &e.toN, math.Inf(1)}} {
			if side.raw == nil {
				*side.n = side.open
				continue
			}
			v, err := parseRangeBound(vs, side.raw, pc.ac)
			if err != nil {
				return err
			}
			*side.n = v
		}
		resolved[i] = e
	}
	pc.ac.setAux(d, 0, pc.ix, resolved)
	return nil
}

// parseRangeBound is DocValueFormat.parseDouble of a range bound. A date
// field's bound goes through its DocValueFormat (e.g. epoch_second) even
// when the request wrote it as a bare JSON number, not just a string: 1000
// on a field mapped `format: epoch_second` means 1000 seconds (1_000_000
// internal millis), not the literal 1000.
func parseRangeBound(vs *valuesSource, raw any, ac *aggContext) (float64, error) {
	if vs.format.kind == fmtDate {
		s, ok := dateText(raw)
		if !ok {
			n, _ := toFloat(raw)
			return n, nil
		}
		t, err := ParseDateMath(s, vs.format.date, ac.now, vs.format.loc, false)
		if err != nil {
			return 0, errDateParse(s, vs.format.date)
		}
		return float64(t.UnixMilli()), nil
	}
	if _, isStr := raw.(string); !isStr {
		n, _ := toFloat(raw)
		return n, nil
	}
	s := raw.(string)
	switch vs.format.kind {
	case fmtBool:
		switch s {
		case "true":
			return 1, nil
		case "false":
			return 0, nil
		}
		return 0, errIllegalArgument("Cannot parse boolean [%s], expected either [true] or [false]", s)
	}
	return aggParseDouble(s, func(s string) *Error { return aggNumberFormatError(s) })
}

func collectRange(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*rangeAggSpec)
	switch d.kind {
	case "ip_range":
		return collectIPRange(ac, d, spec, hits)
	case "geo_distance":
		return collectGeoDistance(ac, d, spec, hits)
	}
	var entries []rangeEntry
	for _, ix := range ac.indices {
		if e, ok := ac.aux(d, 0, ix).([]rangeEntry); ok {
			entries = append([]rangeEntry(nil), e...)
			break
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if c := javaDoubleCompare(entries[i].fromN, entries[j].fromN); c != 0 {
			return c < 0
		}
		return javaDoubleCompare(entries[i].toN, entries[j].toN) < 0
	})
	format := ac.firstFormat(d, 0)
	r := &aggResult{kind: resBuckets, keyed: spec.keyed, javaClass: "InternalRange"}
	if d.kind == "date_range" {
		r.javaClass = "InternalDateRange"
	}
	for _, e := range entries {
		b := &bucket{noKey: spec.keyed, fields: M{}}
		key := e.key
		if !e.hasKey {
			key = rangeKeyPart(e.fromN, format) + "-" + rangeKeyPart(e.toN, format)
		}
		b.key, b.keyString, b.keyedName = key, key, key
		if !math.IsInf(e.fromN, 0) {
			b.fields["from"] = e.fromN
			if !format.raw() {
				b.fields["from_as_string"] = format.formatDouble(e.fromN)
			}
		}
		if !math.IsInf(e.toN, 0) {
			b.fields["to"] = e.toN
			if !format.raw() {
				b.fields["to_as_string"] = format.formatDouble(e.toN)
			}
		}
		for _, h := range hits {
			vs := ac.source(d, 0, h.ix)
			if vs == nil {
				continue
			}
			for _, v := range vs.nums(h) {
				if v >= e.fromN && v < e.toN {
					b.docCount++
					b.hits = append(b.hits, h)
					break
				}
			}
		}
		r.buckets = append(r.buckets, b)
	}
	if err := ac.collectSubs(d, r.buckets); err != nil {
		return nil, err
	}
	return r, nil
}

func rangeKeyPart(v float64, format *valueFormat) string {
	if math.IsInf(v, 0) {
		return "*"
	}
	return format.stringDouble(v)
}

func collectIPRange(ac *aggContext, d *aggDef, spec *rangeAggSpec, hits []*hit) (*aggResult, error) {
	entries := append([]rangeEntry(nil), spec.ranges...)
	cmp := func(a, b []byte, nilLow bool) int {
		switch {
		case a == nil && b == nil:
			return 0
		case a == nil:
			if nilLow {
				return -1
			}
			return 1
		case b == nil:
			if nilLow {
				return 1
			}
			return -1
		}
		return bytes.Compare(a, b)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if c := cmp(entries[i].fromIP, entries[j].fromIP, true); c != 0 {
			return c < 0
		}
		return cmp(entries[i].toIP, entries[j].toIP, false) < 0
	})
	r := &aggResult{kind: resBuckets, keyed: spec.keyed, javaClass: "InternalBinaryRange"}
	for _, e := range entries {
		key := e.key
		if !e.hasKey {
			f, t := "*", "*"
			if e.fromIP != nil {
				f = e.fromS
			}
			if e.toIP != nil {
				t = e.toS
			}
			key = f + "-" + t
		}
		b := &bucket{key: key, keyString: key, keyedName: key, noKey: spec.keyed, fields: M{}}
		if e.fromIP != nil {
			b.fields["from"] = e.fromS
		}
		if e.toIP != nil {
			b.fields["to"] = e.toS
		}
		for _, h := range hits {
			vs := ac.source(d, 0, h.ix)
			if vs == nil {
				continue
			}
			for _, s := range vs.strs(h) {
				v := ipBytes(s)
				if (e.fromIP == nil || bytes.Compare(v, e.fromIP) >= 0) && (e.toIP == nil || bytes.Compare(v, e.toIP) < 0) {
					b.docCount++
					b.hits = append(b.hits, h)
					break
				}
			}
		}
		r.buckets = append(r.buckets, b)
	}
	if err := ac.collectSubs(d, r.buckets); err != nil {
		return nil, err
	}
	return r, nil
}

const earthMeanRadius = 6371008.7714

// arcDistance is the haversine distance in meters.
func arcDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const toRad = math.Pi / 180
	x1, x2 := lat1*toRad, lat2*toRad
	h1 := 1 - math.Cos(x1-x2)
	h2 := 1 - math.Cos((lon1-lon2)*toRad)
	h := h1 + float64(math.Cos(x1)*math.Cos(x2)*h2)
	return float64(earthMeanRadius*2) * math.Asin(math.Min(1, math.Sqrt(float64(h*0.5))))
}

func planeDistance(lat1, lon1, lat2, lon2 float64) float64 {
	x := (lon2 - lon1) * math.Pi / 180 * math.Cos((lat2+lat1)/2.0*math.Pi/180)
	y := (lat2 - lat1) * math.Pi / 180
	return math.Sqrt(float64(x*x)+float64(y*y)) * earthMeanRadius
}

func collectGeoDistance(ac *aggContext, d *aggDef, spec *rangeAggSpec, hits []*hit) (*aggResult, error) {
	entries := append([]rangeEntry(nil), spec.ranges...)
	for i := range entries {
		e := &entries[i]
		e.fromN, e.toN = math.Inf(-1), math.Inf(1)
		if e.from != nil {
			e.fromN, _ = toFloat(e.from)
		}
		if e.to != nil {
			e.toN, _ = toFloat(e.to)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		fi, fj := entries[i].fromN, entries[j].fromN
		if math.IsInf(fi, -1) {
			fi = 0
		}
		if math.IsInf(fj, -1) {
			fj = 0
		}
		if c := javaDoubleCompare(fi, fj); c != 0 {
			return c < 0
		}
		return javaDoubleCompare(entries[i].toN, entries[j].toN) < 0
	})
	r := &aggResult{kind: resBuckets, keyed: spec.keyed, javaClass: "InternalGeoDistance"}
	for _, e := range entries {
		key := e.key
		if !e.hasKey {
			key = rangeKeyPart(e.fromN, rawFormat) + "-" + rangeKeyPart(e.toN, rawFormat)
		}
		from := e.fromN
		if math.IsInf(from, -1) {
			from = 0
		}
		b := &bucket{key: key, keyString: key, keyedName: key, noKey: spec.keyed, fields: M{"from": from}}
		if !math.IsInf(e.toN, 0) {
			b.fields["to"] = e.toN
		}
		for _, h := range hits {
			vs := ac.source(d, 0, h.ix)
			if vs == nil {
				continue
			}
			for _, p := range vs.points(h) {
				dist := arcDistance(spec.origin[0], spec.origin[1], p[0], p[1])
				if spec.plane {
					dist = planeDistance(spec.origin[0], spec.origin[1], p[0], p[1])
				}
				dist /= spec.unitMeters
				if dist >= from && dist < e.toN {
					b.docCount++
					b.hits = append(b.hits, h)
					break
				}
			}
		}
		r.buckets = append(r.buckets, b)
	}
	if err := ac.collectSubs(d, r.buckets); err != nil {
		return nil, err
	}
	return r, nil
}
