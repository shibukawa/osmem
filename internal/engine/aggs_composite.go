package engine

import (
	"bytes"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// composite aggregation ------------------------------------------------------------------------

var compositeFields = objFields{name: "composite", fields: map[string]int{"sources": vtObjectArray, "size": vtNumber, "after": vtObject}}

type compositeSource struct {
	name         string
	kind         string // terms, histogram, date_histogram, geotile_grid
	vs           vsConfig
	desc         bool
	missingBkt   bool
	missingOrder string // "", "first", "last"
	hasOrderMiss bool
	interval     float64
	date         *dateHistogramSpec
	precision    int
}

type compositeSpec struct {
	sources  []*compositeSource
	size     int
	after    M
	hasAfter bool
}

func compositeSourceFields(kind string) objFields {
	f := map[string]int{"field": vtString, "script": vtObjectOrString, "value_type": vtString, "missing_bucket": vtBool,
		"missing_order": vtString, "order": vtString}
	switch kind {
	case "histogram":
		f["interval"] = vtNumber
		f["format"] = vtString
	case "date_histogram":
		for _, k := range []string{"calendar_interval", "fixed_interval", "format", "time_zone"} {
			f[k] = vtString
		}
		f["interval"] = vtNumber
		f["offset"] = vtNumber
		f["time_zone"] = vtNumber
	case "geotile_grid":
		f["precision"] = vtNumber
		f["bounds"] = vtObject
		f["format"] = vtString
	}
	return objFields{name: kind, fields: f}
}

func parseComposite(ps *aggParser, d *aggDef) error {
	of := compositeFields
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &compositeSpec{size: 10}
	var err error
	if _, ok := body["size"]; ok {
		if spec.size, err = of.intValue(body, "size"); err != nil {
			return err
		}
	}
	if a, ok := body["after"].(M); ok {
		spec.after, spec.hasAfter = a, true
	}
	raw, ok := body["sources"]
	buildErr := func(cause *Error) error {
		return of.failed(body, "sources", errXContent(cause, "Failed to build [composite] after last required field arrived"))
	}
	if !ok {
		return pIllegalArgument("Required [sources]")
	}
	seen := map[string]bool{}
	var dups []string
	for i, item := range getList(raw) {
		sm, ok := item.(M)
		if !ok {
			return of.failed(body, "sources", errParsing("Failed to parse object: expecting token of type [START_OBJECT] but found [%s]", jsonTokenName(item)).
				at(memberElemTok(body, "sources", i)))
		}
		for name, def := range sm {
			dm, ok := def.(M)
			if !ok {
				return of.failed(body, "sources", errParsing("Failed to parse object: expecting token of type [START_OBJECT] but found [%s]", jsonTokenName(def)).
					at(valueTok(sm, name)))
			}
			if len(dm) != 1 {
				// after the first source type the list parser reads the first
				// field of the second one
				at := viaNthTok(dm, 2, func(name string) *tokenRef {
					if inner, ok := dm[name].(M); ok && len(inner) > 0 {
						return nthKeyTok(inner, 1)
					}
					return nil
				})
				return of.failed(body, "sources", errJava(http.StatusInternalServerError, "illegal_state_exception", "expected value but got [FIELD_NAME]").atParser(at))
			}
			for kind, kb := range dm {
				switch kind {
				case "terms", "histogram", "date_histogram", "geotile_grid":
				default:
					return of.failed(body, "sources", errParsing("invalid source type: %s", kind).at(valueTok(dm, kind)))
				}
				kbm, ok := kb.(M)
				if !ok {
					return of.failed(body, "sources", errParsing("Failed to parse object: expecting token of type [START_OBJECT] but found [%s]", jsonTokenName(kb)).
						at(valueTok(dm, kind)))
				}
				src, err := parseCompositeSource(name, kind, kbm, d)
				if err != nil {
					return of.failed(body, "sources", err.(*Error))
				}
				if seen[name] {
					dups = append(dups, name)
				}
				seen[name] = true
				spec.sources = append(spec.sources, src)
			}
		}
	}
	if len(spec.sources) == 0 {
		return buildErr(errIllegalArgument("Composite [sources] cannot be null or empty"))
	}
	if len(dups) > 0 {
		return buildErr(errIllegalArgument("Composite source names must be unique, found duplicates: %s", javaList(dups)))
	}
	d.spec = spec
	return nil
}

func parseCompositeSource(name, kind string, body M, d *aggDef) (*compositeSource, error) {
	of := compositeSourceFields(kind)
	if err := of.check(body); err != nil {
		return nil, err
	}
	src := &compositeSource{name: name, kind: kind}
	var err error
	if src.vs, err = parseVSConfig(of, body); err != nil {
		return nil, err
	}
	if _, ok := body["missing_bucket"]; ok {
		if src.missingBkt, err = of.boolValue(body, "missing_bucket"); err != nil {
			return nil, err
		}
	}
	if mo, ok := body["missing_order"].(string); ok {
		switch strings.ToLower(mo) {
		case "first", "last", "default":
			src.missingOrder, src.hasOrderMiss = strings.ToLower(mo), true
		default:
			return nil, of.failed(body, "missing_order", errIllegalArgument("No enum constant org.opensearch.search.aggregations.bucket.missing.MissingOrder.%s", strings.ToUpper(mo)))
		}
	}
	if o, ok := body["order"].(string); ok {
		desc, err := parseSortOrder(o)
		if err != nil {
			return nil, of.failed(body, "order", err.(*Error))
		}
		src.desc = desc
	}
	switch kind {
	case "histogram":
		if _, ok := body["interval"]; ok {
			if src.interval, err = of.doubleValue(body, "interval"); err != nil {
				return nil, err
			}
			if src.interval <= 0 {
				return nil, of.failed(body, "interval", errIllegalArgument("[interval] must be greater than 0 for [histogram] source"))
			}
		}
	case "date_histogram":
		sub := &aggDef{name: d.name, kind: "date_histogram", body: body}
		if err := parseDateHistogramInto(of, sub, false); err != nil {
			return nil, err
		}
		src.date = sub.spec.(*dateHistogramSpec)
	case "geotile_grid":
		src.precision = 7
		if _, ok := body["precision"]; ok {
			if src.precision, err = of.intValue(body, "precision"); err != nil {
				return nil, err
			}
			if src.precision < 0 || src.precision > 29 {
				return nil, of.failed(body, "precision", errIllegalArgument("Invalid geotile_grid precision of %d. Must be between 0 and 29.", src.precision))
			}
		}
	}
	if src.vs.script {
		return nil, errScript(d)
	}
	return src, nil
}

// parentFactoryNames are the Java aggregator factories named by the
// composite parent check.
var parentFactoryNames = map[string]string{
	"terms": "TermsAggregatorFactory", "global": "GlobalAggregatorFactory", "filter": "FilterAggregatorFactory",
	"filters": "FiltersAggregatorFactory", "histogram": "HistogramAggregatorFactory", "date_histogram": "DateHistogramAggregatorFactory",
	"range": "RangeAggregatorFactory", "date_range": "DateRangeAggregatorFactory", "missing": "MissingAggregatorFactory",
	"reverse_nested": "ReverseNestedAggregatorFactory", "sampler": "SamplerAggregatorFactory", "diversified_sampler": "DiversifiedAggregatorFactory",
	"multi_terms": "MultiTermsAggregationFactory", "adjacency_matrix": "AdjacencyMatrixAggregatorFactory", "ip_range": "BinaryRangeAggregatorFactory",
	"geo_distance": "GeoDistanceRangeAggregatorFactory", "rare_terms": "RareTermsAggregatorFactory", "significant_terms": "SignificantTermsAggregatorFactory",
	"auto_date_histogram": "AutoDateHistogramAggregatorFactory", "variable_width_histogram": "VariableWidthHistogramAggregatorFactory",
	"geohash_grid": "GeoHashGridAggregatorFactory", "geotile_grid": "GeoTileGridAggregatorFactory", "children": "ChildrenAggregatorFactory",
	"parent": "ParentAggregatorFactory", "significant_text": "SignificantTextAggregatorFactory",
}

type compositeShard struct {
	sources []*valuesSource
	after   []any // parsed after values (nil entries for missing buckets)
}

func prepareComposite(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*compositeSpec)
	for p := d.parent; p != nil; p = p.parent {
		if p.kind != "nested" && p.kind != "filter" && p.kind != "reverse_nested" {
			name := parentFactoryNames[p.kind]
			if name == "" {
				name = "AggregatorFactory"
			}
			return errIllegalArgument("[composite] aggregation cannot be used with a parent aggregation of type: [%s]", name)
		}
	}
	shard := &compositeShard{}
	for i, src := range spec.sources {
		var allowed []vsKind
		def := vsBytes
		switch src.kind {
		case "terms":
			allowed = []vsKind{vsBytes, vsIP, vsDate, vsNumeric, vsBoolean}
		case "histogram":
			allowed, def = []vsKind{vsNumeric, vsDate, vsBoolean}, vsNumeric
		case "date_histogram":
			allowed, def = []vsKind{vsDate, vsNumeric}, vsDate
		case "geotile_grid":
			allowed, def = []vsKind{vsGeoPoint}, vsGeoPoint
		}
		cfg := &src.vs
		if src.kind == "date_histogram" {
			cfg = &src.date.vs
		}
		if !cfg.hasField && !cfg.script {
			return errJava(http.StatusInternalServerError, "illegal_state_exception", "value source config is invalid; must have either a field or a script")
		}
		vs, err := pc.resolve(d, i, cfg, src.kind, def, allowed...)
		if err != nil {
			return err
		}
		if src.kind == "date_histogram" {
			if !src.date.hasInterval {
				return errIllegalArgument("Invalid interval specified, must be non-null and non-empty")
			}
			if src.date.unit == 0 && src.date.fixed <= 0 {
				return errIllegalArgument("Zero or negative time interval not supported")
			}
		}
		if src.hasOrderMiss && src.missingOrder != "default" && !src.missingBkt {
			return errIllegalArgument("missing_order require missing_bucket is true")
		}
		shard.sources = append(shard.sources, vs)
	}
	if spec.size == 0 {
		return errJava(http.StatusInternalServerError, "null_pointer_exception", "Cannot invoke \"java.lang.Integer.intValue()\" because the return value of \"org.opensearch.search.aggregations.bucket.composite.CompositeValuesCollectorQueue.top()\" is null")
	}
	if spec.size < 0 {
		return errJava(http.StatusInternalServerError, "negative_array_size_exception", strconv.Itoa(spec.size))
	}
	if spec.hasAfter {
		if len(spec.after) != len(spec.sources) {
			return errIllegalArgument("[after] has %d value(s) but [sources] has %d", len(spec.after), len(spec.sources))
		}
		for i, src := range spec.sources {
			v, ok := spec.after[src.name]
			if !ok {
				return errIllegalArgument("Missing value for [after.%s]", src.name)
			}
			parsed, err := parseCompositeAfter(src, shard.sources[i], v, pc.ac.now)
			if err != nil {
				return err
			}
			shard.after = append(shard.after, parsed)
		}
	}
	pc.ac.setAux(d, 0, pc.ix, shard)
	return nil
}

// compositeValueKind is the kind of key a source produces.
func compositeValueKind(src *compositeSource, vs *valuesSource) string {
	switch src.kind {
	case "histogram":
		return "double"
	case "date_histogram":
		return "long"
	case "geotile_grid":
		return "tile"
	}
	switch vs.kind {
	case vsBytes, vsIP:
		return "bytes"
	}
	if vs.floating {
		return "double"
	}
	return "long"
}

// compositeFormat is the format of a source's keys.
func compositeFormat(src *compositeSource, vs *valuesSource) *valueFormat {
	switch src.kind {
	case "terms":
		if vs.kind == vsDate {
			return rawFormat
		}
	case "date_histogram":
		if !src.date.vs.hasFormat {
			return rawFormat
		}
	case "geotile_grid":
		return rawFormat
	}
	return vs.format
}

func parseCompositeAfter(src *compositeSource, vs *valuesSource, v any, now time.Time) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch compositeValueKind(src, vs) {
	case "bytes", "tile":
		s, ok := v.(string)
		if !ok {
			return nil, errIllegalArgument("invalid value, expected string, got %s", javaBoxedClass(v))
		}
		if vs.kind == vsIP {
			return string(ipBytes(s)), nil
		}
		return s, nil
	case "long":
		if s, ok := v.(string); ok {
			f := compositeFormat(src, vs)
			if f.kind == fmtDate {
				t, err := ParseDateMath(s, f.date, now, f.loc, false)
				if err != nil {
					return nil, errDateParse(s, f.date)
				}
				return float64(t.UnixMilli()), nil
			}
			n, err := aggParseDouble(s, func(s string) *Error { return aggNumberFormatError(s) })
			if err != nil {
				return nil, err
			}
			return math.Floor(n), nil
		}
		if b, ok := v.(bool); ok {
			if b {
				return 1.0, nil
			}
			return 0.0, nil
		}
		n, _ := toFloat(v)
		return n, nil
	}
	if s, ok := v.(string); ok {
		return aggParseDouble(s, func(s string) *Error { return aggNumberFormatError(s) })
	}
	n, _ := toFloat(v)
	return n, nil
}

func javaBoxedClass(v any) string {
	switch t := v.(type) {
	case bool:
		return "Boolean"
	case json.Number:
		if _, err := t.Int64(); err == nil {
			if n, _ := t.Int64(); n >= math.MinInt32 && n <= math.MaxInt32 {
				return "Integer"
			}
			return "Long"
		}
		return "Double"
	case float64:
		if t == math.Trunc(t) && t >= math.MinInt32 && t <= math.MaxInt32 {
			return "Integer"
		}
		return "Double"
	case M:
		return "HashMap"
	case []any:
		return "ArrayList"
	}
	return "Object"
}

// sourceValues returns the composite keys of a source for a hit (strings for
// bytes and tiles, float64 otherwise).
func (ac *aggContext) sourceValues(src *compositeSource, vs *valuesSource, h *hit) []any {
	var out []any
	switch src.kind {
	case "terms":
		if vs.kind == vsBytes || vs.kind == vsIP {
			for _, s := range vs.strs(h) {
				if vs.kind == vsIP {
					s = string(ipBytes(s))
				}
				out = append(out, s)
			}
			return out
		}
		for _, n := range vs.nums(h) {
			out = append(out, n)
		}
	case "histogram":
		prev := math.NaN()
		for _, n := range vs.nums(h) {
			k := float64(math.Floor(n/src.interval) * src.interval)
			if k != prev {
				out = append(out, k)
			}
			prev = k
		}
	case "date_histogram":
		r := src.date.rounding()
		first, prev := true, int64(0)
		for _, n := range vs.nums(h) {
			k := r.round(int64(n))
			if first || k != prev {
				out = append(out, float64(k))
			}
			first, prev = false, k
		}
	case "geotile_grid":
		seen := map[string]bool{}
		for _, p := range vs.points(h) {
			k := geotileKey(p[0], p[1], src.precision)
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

func compareCompositeValue(a, b any, ip bool) int {
	switch at := a.(type) {
	case string:
		bt, ok := b.(string)
		if !ok {
			return 1
		}
		if ip {
			return bytes.Compare([]byte(at), []byte(bt))
		}
		return strings.Compare(at, bt)
	case float64:
		bt, ok := b.(float64)
		if !ok {
			return -1
		}
		return javaDoubleCompare(at, bt)
	}
	return 0
}

func collectComposite(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*compositeSpec)
	var shard *compositeShard
	for _, ix := range ac.indices {
		if s, ok := ac.aux(d, 0, ix).(*compositeShard); ok {
			shard = s
			break
		}
	}
	type combo struct {
		keys []any
		b    *bucket
	}
	groups := map[string]*combo{}
	var order []string
	for _, h := range hits {
		parts := [][]any{{}}
		skip := false
		for i, src := range spec.sources {
			vs := ac.source(d, i, h.ix)
			var vals []any
			if vs != nil {
				vals = ac.sourceValues(src, vs, h)
			}
			if len(vals) == 0 {
				if !src.missingBkt {
					skip = true
					break
				}
				vals = []any{nil}
			}
			var next [][]any
			for _, p := range parts {
				for _, v := range vals {
					next = append(next, append(append([]any(nil), p...), v))
				}
			}
			parts = next
		}
		if skip {
			continue
		}
		for _, keys := range parts {
			enc, _ := json.Marshal(keys)
			id := string(enc)
			c, ok := groups[id]
			if !ok {
				c = &combo{keys: keys, b: &bucket{}}
				groups[id] = c
				order = append(order, id)
			}
			c.b.docCount++
			if len(c.b.hits) == 0 || c.b.hits[len(c.b.hits)-1] != h {
				c.b.hits = append(c.b.hits, h)
			}
		}
	}
	// a source reading bytes in one index and numbers in another cannot be
	// reduced (OpenSearch fails casting the keys)
	for i, src := range spec.sources {
		kinds := map[string]bool{}
		for _, ix := range ac.indices {
			if vs := ac.source(d, i, ix); vs != nil && !(vs.unmapped && vs.missing == nil) {
				kinds[compositeValueKind(src, vs)] = true
			}
		}
		if kinds["bytes"] && (kinds["long"] || kinds["double"]) {
			return nil, reduceFailure(errJava(http.StatusInternalServerError, "class_cast_exception", "class org.apache.lucene.util.BytesRef cannot be cast to class java.lang.Long (org.apache.lucene.util.BytesRef is in unnamed module of loader 'app'; java.lang.Long is in module java.base of loader 'bootstrap')"))
		}
	}
	ips := make([]bool, len(spec.sources))
	var sources []*valuesSource
	if shard != nil {
		sources = shard.sources
		for i, vs := range sources {
			ips[i] = vs.kind == vsIP && spec.sources[i].kind == "terms"
		}
	}
	cmp := func(a, b []any) int {
		for i, src := range spec.sources {
			var c int
			missingFirst := !src.desc
			switch src.missingOrder {
			case "first":
				missingFirst = true
			case "last":
				missingFirst = false
			}
			switch {
			case a[i] == nil && b[i] == nil:
				c = 0
			case a[i] == nil:
				if missingFirst {
					return -1
				}
				return 1
			case b[i] == nil:
				if missingFirst {
					return 1
				}
				return -1
			default:
				c = compareCompositeValue(a[i], b[i], ips[i])
				if src.desc {
					c = -c
				}
			}
			if c != 0 {
				return c
			}
		}
		return 0
	}
	combos := make([]*combo, 0, len(order))
	for _, id := range order {
		combos = append(combos, groups[id])
	}
	sort.SliceStable(combos, func(i, j int) bool { return cmp(combos[i].keys, combos[j].keys) < 0 })
	if shard != nil && shard.after != nil {
		filtered := combos[:0]
		for _, c := range combos {
			if cmp(c.keys, shard.after) > 0 {
				filtered = append(filtered, c)
			}
		}
		combos = filtered
	}
	if spec.size > 0 && len(combos) > spec.size {
		combos = combos[:spec.size]
	}
	buckets := make([]*bucket, len(combos))
	for i, c := range combos {
		buckets[i] = c.b
	}
	if err := ac.collectSubs(d, buckets); err != nil {
		return nil, err
	}
	var afterKey M
	for i, c := range combos {
		key := M{}
		var strs []string
		for j, src := range spec.sources {
			var v any
			if c.keys[j] != nil && sources != nil {
				v = compositeKeyJSON(src, sources[j], c.keys[j])
			}
			key[src.name] = v
			strs = append(strs, src.name+"="+objectString(v))
		}
		buckets[i].key = key
		buckets[i].keyString = "{" + strings.Join(strs, ", ") + "}"
		afterKey = key
	}
	r := &aggResult{kind: resBuckets, buckets: buckets, javaClass: "InternalComposite"}
	if afterKey != nil {
		r.fields = M{"after_key": afterKey}
	}
	return r, nil
}

func compositeKeyJSON(src *compositeSource, vs *valuesSource, v any) any {
	format := compositeFormat(src, vs)
	switch t := v.(type) {
	case string:
		if vs.kind == vsIP && src.kind == "terms" {
			return netIPString([]byte(t))
		}
		return t
	case float64:
		if compositeValueKind(src, vs) == "long" {
			if vs.f != nil && vs.f.Type == TypeUnsignedLong {
				return numericOutput(vs.f, t)
			}
			if format.raw() {
				return int64(t)
			}
			return format.formatLong(int64(t))
		}
		if format.raw() {
			return t
		}
		return format.formatDouble(t)
	}
	return v
}

func netIPString(b []byte) string {
	return canonicalIP(net.IP(b).String())
}
