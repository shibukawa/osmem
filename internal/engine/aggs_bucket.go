package engine

import (
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// single-bucket and filter aggregations ---------------------------------------------------

// matchSet caches the documents a query matches per index and nested level.
type matchKey struct {
	query uintptr
	ix    *Index
	level string
}

// filterHits keeps the hits a query matches; nested objects are matched by
// the query built for their nested level.
func (ac *aggContext) filterHits(q any, hits []*hit) ([]*hit, error) {
	if ac.matches == nil {
		ac.matches = map[matchKey]map[string]bool{}
	}
	qp := reflect.ValueOf(q).Pointer()
	out := make([]*hit, 0, len(hits))
	for _, h := range hits {
		level := h.doc.level()
		k := matchKey{qp, h.ix, level}
		set, ok := ac.matches[k]
		if !ok {
			set = map[string]bool{}
			if level == "" {
				matched, err := ac.c.executeTargets([]target{{ix: h.ix}}, q, false, false)
				if err != nil {
					return nil, err
				}
				for _, m := range matched {
					set[m.doc.ID] = true
				}
			} else {
				ids, err := ac.c.nestedFilterMatches(h.ix, level, q)
				if err != nil {
					return nil, shardError(err, h.ix)
				}
				set = ids
			}
			ac.matches[k] = set
		}
		if set[h.doc.bleveID()] {
			out = append(out, h)
		}
	}
	return out, nil
}

func parseFilter(ps *aggParser, d *aggDef) error {
	if len(d.body) == 0 {
		return errIllegalArgument("query malformed, empty clause found at [1:1]").atInReason(endTok(d.body), "query malformed, empty clause found at [%s]")
	}
	return nil
}

func collectFilter(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	matched, err := ac.filterHits(d.body, hits)
	if err != nil {
		return nil, err
	}
	r, err := ac.singleBucket(d, matched)
	if err != nil {
		return nil, err
	}
	r.javaClass = "InternalFilter"
	return r, nil
}

type filtersSpec struct {
	keyed       bool
	keys        []string
	queries     []any
	otherBucket bool
	otherKey    string
}

func parseFilters(ps *aggParser, d *aggDef) error {
	spec := &filtersSpec{otherKey: "_other_"}
	var otherSet *bool
	hasFilters := false
	keys := make([]string, 0, len(d.body))
	for k := range d.body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	unknown := func(k string) error {
		return errParsing("Unknown key for a %s in [%s]: [%s].", jsonTokenName(d.body[k]), d.name, k).at(valueTok(d.body, k))
	}
	for _, k := range keys {
		v := d.body[k]
		switch t := v.(type) {
		case bool:
			if k != "other_bucket" {
				return unknown(k)
			}
			b := t
			otherSet = &b
		case string:
			if k != "other_bucket_key" {
				return unknown(k)
			}
			spec.otherKey = t
			if otherSet == nil {
				b := true
				otherSet = &b
			}
		case M:
			if k != "filters" {
				return unknown(k)
			}
			spec.keyed, hasFilters = true, true
			names := make([]string, 0, len(t))
			for name := range t {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				spec.keys = append(spec.keys, name)
				spec.queries = append(spec.queries, t[name])
			}
		case []any:
			if k != "filters" {
				return unknown(k)
			}
			hasFilters = true
			spec.queries = append(spec.queries, t...)
		default:
			return unknown(k)
		}
	}
	if _, explicit := d.body["other_bucket"]; explicit {
		b := getBool(d.body, "other_bucket", false)
		otherSet = &b
	}
	if otherSet != nil {
		spec.otherBucket = *otherSet
	}
	if !hasFilters || len(spec.queries) == 0 {
		return errIllegalArgument("[filters] cannot be empty.")
	}
	d.spec = spec
	return nil
}

func collectFilters(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*filtersSpec)
	r := &aggResult{kind: resBuckets, keyed: spec.keyed, javaClass: "InternalFilters"}
	matchedAny := map[*hit]bool{}
	for i, q := range spec.queries {
		matched, err := ac.filterHits(q, hits)
		if err != nil {
			return nil, err
		}
		for _, h := range matched {
			matchedAny[h] = true
		}
		b := &bucket{docCount: int64(len(matched)), hits: matched, noKey: true}
		if spec.keyed {
			b.key, b.keyString, b.keyedName = spec.keys[i], spec.keys[i], spec.keys[i]
		} else {
			b.keyString = strconv.Itoa(i)
		}
		r.buckets = append(r.buckets, b)
	}
	if spec.otherBucket {
		var rest []*hit
		for _, h := range hits {
			if !matchedAny[h] {
				rest = append(rest, h)
			}
		}
		b := &bucket{docCount: int64(len(rest)), hits: rest, noKey: true, key: spec.otherKey, keyString: spec.otherKey, keyedName: spec.otherKey}
		r.buckets = append(r.buckets, b)
	}
	if err := ac.collectSubs(d, r.buckets); err != nil {
		return nil, err
	}
	return r, nil
}

// adjacency_matrix -----------------------------------------------------------------------

type adjacencySpec struct {
	names                []string
	queries              []any
	separator            string
	showOnlyIntersecting bool
}

var adjacencyFields = objFields{name: "adjacency_matrix", fields: map[string]int{"filters": vtObject, "separator": vtString, "show_only_intersecting": vtBool}}

func parseAdjacency(ps *aggParser, d *aggDef) error {
	if err := adjacencyFields.check(d.body); err != nil {
		return err
	}
	spec := &adjacencySpec{separator: "&"}
	if s, ok := d.body["separator"].(string); ok {
		spec.separator = s
	}
	spec.showOnlyIntersecting = getBool(d.body, "show_only_intersecting", false)
	filters, _ := d.body["filters"].(M)
	if len(filters) == 0 {
		return errJava(http.StatusInternalServerError, "illegal_state_exception", "["+d.name+"] is missing : filters parameter")
	}
	for name := range filters {
		spec.names = append(spec.names, name)
	}
	sort.Strings(spec.names)
	for _, name := range spec.names {
		spec.queries = append(spec.queries, filters[name])
	}
	d.spec = spec
	return nil
}

func collectAdjacency(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*adjacencySpec)
	sets := make([]map[*hit]bool, len(spec.queries))
	for i, q := range spec.queries {
		matched, err := ac.filterHits(q, hits)
		if err != nil {
			return nil, err
		}
		sets[i] = map[*hit]bool{}
		for _, h := range matched {
			sets[i][h] = true
		}
	}
	var buckets []*bucket
	add := func(key string, in func(h *hit) bool) {
		var list []*hit
		for _, h := range hits {
			if in(h) {
				list = append(list, h)
			}
		}
		if len(list) > 0 {
			buckets = append(buckets, &bucket{key: key, keyString: key, docCount: int64(len(list)), hits: list})
		}
	}
	for i := range spec.names {
		i := i
		add(spec.names[i], func(h *hit) bool { return sets[i][h] })
		for j := i + 1; j < len(spec.names); j++ {
			j := j
			add(spec.names[i]+spec.separator+spec.names[j], func(h *hit) bool { return sets[i][h] && sets[j][h] })
		}
	}
	if spec.showOnlyIntersecting {
		kept := buckets[:0]
		for _, b := range buckets {
			if strings.Contains(b.keyString, spec.separator) {
				kept = append(kept, b)
			}
		}
		buckets = kept
	}
	sort.SliceStable(buckets, func(a, b int) bool { return buckets[a].keyString < buckets[b].keyString })
	if err := ac.collectSubs(d, buckets); err != nil {
		return nil, err
	}
	return &aggResult{kind: resBuckets, buckets: buckets, javaClass: "InternalAdjacencyMatrix"}, nil
}

// missing -------------------------------------------------------------------------------------

type missingSpec struct{ vs vsConfig }

func parseMissingAgg(ps *aggParser, d *aggDef) error {
	of := valuesSourceFields("missing", true, false, nil)
	if err := of.check(d.body); err != nil {
		return err
	}
	cfg, err := parseVSConfig(of, d.body)
	if err != nil {
		return err
	}
	if err := requireFieldOrScript(d.body); err != nil {
		return err
	}
	if cfg.script {
		return errScript(d)
	}
	d.spec = &missingSpec{vs: cfg}
	return nil
}

func prepareMissingAgg(pc *prepareCtx, d *aggDef) error {
	_, err := pc.resolve(d, 0, &d.spec.(*missingSpec).vs, "missing", vsBytes, allCoreKinds...)
	return err
}

func collectMissingAgg(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	var rest []*hit
	for _, h := range hits {
		vs := ac.source(d, 0, h.ix)
		if vs == nil || !vs.hasValue(h) {
			rest = append(rest, h)
		}
	}
	r, err := ac.singleBucket(d, rest)
	if r != nil {
		r.javaClass = "InternalMissing"
	}
	return r, err
}

// global ---------------------------------------------------------------------------------------

func parseGlobal(ps *aggParser, d *aggDef) error {
	for _, k := range aggBodyKeys(d.body) {
		return errParsing("Expected [FIELD_NAME] under a [START_OBJECT], but got a [%s] in [%s]", jsonTokenName(d.body[k]), d.name).at(valueTok(d.body, k))
	}
	return nil
}

func prepareGlobal(pc *prepareCtx, d *aggDef) error {
	if d.parent != nil {
		return errAggExecution("Aggregation [%s] cannot have a global sub-aggregation [%s]. Global aggregations can only be defined as top level aggregations", d.parent.name, d.name)
	}
	return nil
}

func collectGlobal(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	all, err := ac.allHits()
	if err != nil {
		return nil, err
	}
	r, err := ac.singleBucket(d, all)
	if r != nil {
		r.javaClass = "InternalGlobal"
	}
	return r, err
}

// nested and reverse_nested ------------------------------------------------------------------------

func parseNestedAgg(ps *aggParser, d *aggDef) error {
	keys := make([]string, 0, len(d.body))
	for k := range d.body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := d.body[k]
		if _, isStr := v.(string); !isStr {
			return errParsing("Unexpected token %s in [%s].", jsonTokenName(v), d.name).at(valueTok(d.body, k))
		}
		if k != "path" {
			return errParsing("Unknown key for a VALUE_STRING in [%s]: [%s].", d.name, k).at(valueTok(d.body, k))
		}
	}
	if _, ok := d.body["path"]; !ok && d.kind == "nested" {
		return errParsing("Missing [path] field for nested aggregation [%s]", d.name).at(endTok(d.body))
	}
	return nil
}

func prepareNestedAgg(pc *prepareCtx, d *aggDef) error {
	path := getString(d.body, "path")
	if d.kind == "reverse_nested" {
		if len(pc.nested) == 0 {
			return errIllegalArgument("Reverse nested aggregation [%s] can only be used inside a [nested] aggregation", d.name)
		}
		if path == "" {
			return nil
		}
	}
	if f, _, ok := pc.ix.Mapping.resolve(path); ok && f.Type == TypeObject {
		return errAggExecution("[%s] nested path [%s] is not nested", d.kind, path)
	}
	return nil
}

// collectNested runs the sub-aggregations over the nested objects of a path
// below the current documents; doc_count is the number of objects.
func collectNested(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	path := getString(d.body, "path")
	var kids []*hit
	for _, h := range hits {
		f, _, ok := h.ix.Mapping.resolve(path)
		if !ok || f.Type != TypeNested {
			continue
		}
		for _, nd := range h.ix.nestedDescendants(h.doc, path) {
			kids = append(kids, &hit{ix: h.ix, doc: nd, score: h.score, parent: h})
		}
	}
	ac.nested = append(ac.nested, path)
	defer func() { ac.nested = ac.nested[:len(ac.nested)-1] }()
	r, err := ac.singleBucket(d, kids)
	if r != nil {
		r.javaClass = "InternalNested"
	}
	return r, err
}

// collectReverseNested climbs back from nested objects to the documents of an
// enclosing level (the root by default), keeping each once.
func collectReverseNested(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	path := getString(d.body, "path")
	seen := map[*Doc]bool{}
	var parents []*hit
	for _, h := range hits {
		p := h
		for p.parent != nil && p.doc.level() != path {
			p = p.parent
		}
		if p.doc.level() != path {
			if path != "" {
				continue
			}
			for p.parent != nil {
				p = p.parent
			}
		}
		if seen[p.doc] {
			continue
		}
		seen[p.doc] = true
		parents = append(parents, p)
	}
	sort.SliceStable(parents, func(i, j int) bool { return hitBefore(parents[i], parents[j]) })
	saved := ac.nested
	ac.nested = nil
	for _, p := range saved {
		if path != "" && (p == path || hasPrefixDot(path, p)) {
			ac.nested = append(ac.nested, p)
		}
	}
	defer func() { ac.nested = saved }()
	r, err := ac.singleBucket(d, parents)
	if r != nil {
		r.javaClass = "InternalReverseNested"
	}
	return r, err
}

// sampler and diversified_sampler ------------------------------------------------------------------

type samplerSpec struct {
	shardSize   int
	maxPerValue int
	hint        string
	vs          vsConfig
}

func parseSampler(ps *aggParser, d *aggDef) error {
	spec := &samplerSpec{shardSize: 100, maxPerValue: 1}
	keys := make([]string, 0, len(d.body))
	for k := range d.body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n, isNum := d.body[k].(float64)
		if !isNum {
			if jn, ok := toFloat(d.body[k]); ok && tokenKind(d.body[k]) == tkNumber {
				n, isNum = jn, true
			}
		}
		if k != "shard_size" || !isNum {
			return errParsing("Unsupported property \"%s\" for aggregation \"%s", k, d.name).at(valueTok(d.body, k))
		}
		spec.shardSize = int(n)
	}
	d.spec = spec
	return nil
}

var diversifiedFields = valuesSourceFields("diversified_sampler", false, false, map[string]int{
	"shard_size": vtNumber, "max_docs_per_value": vtNumber, "execution_hint": vtString})

func parseDiversified(ps *aggParser, d *aggDef) error {
	of := diversifiedFields
	if err := of.check(d.body); err != nil {
		return err
	}
	spec := &samplerSpec{shardSize: 100, maxPerValue: 1}
	var err error
	if spec.vs, err = parseVSConfig(of, d.body); err != nil {
		return err
	}
	if _, ok := d.body["shard_size"]; ok {
		if spec.shardSize, err = of.intValue(d.body, "shard_size"); err != nil {
			return err
		}
	}
	if _, ok := d.body["max_docs_per_value"]; ok {
		if spec.maxPerValue, err = of.intValue(d.body, "max_docs_per_value"); err != nil {
			return err
		}
	}
	spec.hint, _ = d.body["execution_hint"].(string)
	if err := requireFieldOrScript(d.body); err != nil {
		return err
	}
	if spec.vs.script {
		return errScript(d)
	}
	d.spec = spec
	return nil
}

func prepareSampler(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*samplerSpec)
	if d.kind == "diversified_sampler" {
		vs, err := pc.resolve(d, 0, &spec.vs, "diversified_sampler", vsBytes, vsBytes, vsNumeric, vsDate, vsBoolean, vsIP)
		if err != nil {
			return err
		}
		if spec.hint != "" && (vs.kind == vsBytes || vs.kind == vsIP) {
			switch spec.hint {
			case "map", "bytes_hash", "global_ordinals":
			default:
				return errIllegalArgument("Unknown `execution_hint`: [%s], expected any of [map, bytes_hash, global_ordinals]", spec.hint)
			}
		}
	}
	if len(d.subs) == 0 {
		return errAggExecution("Sampler aggregation must be used with child aggregations.")
	}
	return nil
}

func collectSampler(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*samplerSpec)
	var sample []*hit
	byIndex := map[*Index][]*hit{}
	for _, h := range hits {
		byIndex[h.ix] = append(byIndex[h.ix], h)
	}
	unmapped := d.kind == "diversified_sampler"
	for _, ix := range ac.indices {
		list := append([]*hit(nil), byIndex[ix]...)
		sort.SliceStable(list, func(i, j int) bool { return list[i].score > list[j].score })
		if d.kind == "sampler" {
			if spec.shardSize >= 0 && len(list) > spec.shardSize {
				list = list[:spec.shardSize]
			}
			sample = append(sample, list...)
			continue
		}
		vs := ac.source(d, 0, ix)
		if vs == nil || (vs.unmapped && vs.missing == nil) {
			continue
		}
		unmapped = false
		counts := map[string]int{}
		var picked []*hit
		for _, h := range list {
			if len(picked) >= spec.shardSize {
				break
			}
			var keys []string
			switch vs.kind {
			case vsBytes, vsIP:
				keys = vs.strs(h)
			default:
				for _, n := range vs.nums(h) {
					keys = append(keys, javaDoubleToString(n))
				}
			}
			if len(keys) > 1 {
				return nil, shardError(errIllegalArgument("Sample diversifying key must be a single valued-field"), ix)
			}
			key := "\x00missing"
			if len(keys) == 1 {
				key = keys[0]
			}
			if counts[key] >= spec.maxPerValue {
				continue
			}
			counts[key]++
			picked = append(picked, h)
		}
		sort.SliceStable(picked, func(i, j int) bool { return hitBefore(picked[i], picked[j]) })
		sample = append(sample, picked...)
	}
	if unmapped {
		return &aggResult{kind: resSingleBucket, typed: "sampler", javaClass: "UnmappedSampler"}, nil
	}
	sort.SliceStable(sample, func(i, j int) bool { return hitBefore(sample[i], sample[j]) })
	r, err := ac.singleBucket(d, sample)
	if r != nil {
		r.typed, r.javaClass = "sampler", "InternalSampler"
	}
	return r, err
}

// children and parent --------------------------------------------------------------------------------

func parseJoinAgg(ps *aggParser, d *aggDef) error {
	keys := make([]string, 0, len(d.body))
	for k := range d.body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := d.body[k]
		if _, isStr := v.(string); !isStr {
			return errParsing("Unexpected token %s in [%s].", jsonTokenName(v), d.name).at(valueTok(d.body, k))
		}
		if k != "type" {
			return errParsing("Unknown key for a VALUE_STRING in [%s]: [%s].", d.name, k).at(valueTok(d.body, k))
		}
	}
	if _, ok := d.body["type"]; !ok {
		if d.kind == "children" {
			return errParsing("Missing [child_type] field for children aggregation [%s]", d.name).at(endTok(d.body))
		}
		return errParsing("Missing [parent_type] field for parent aggregation [%s]", d.name).at(endTok(d.body))
	}
	return nil
}

func hasJoinField(fields map[string]*Field) bool {
	for _, f := range fields {
		if f.Type == TypeJoin || hasJoinField(f.Properties) {
			return true
		}
	}
	return false
}

func prepareJoinAgg(pc *prepareCtx, d *aggDef) error {
	if hasJoinField(pc.ix.Mapping.Properties) {
		return errUnsupported("[" + d.kind + "] aggregation over join fields")
	}
	if d.kind == "parent" {
		return errJava(http.StatusInternalServerError, "null_pointer_exception", "Cannot invoke \"org.opensearch.join.mapper.ParentJoinFieldMapper.getParentIdFieldMapper(String, boolean)\" because \"parentJoinFieldMapper\" is null")
	}
	return nil
}

func collectJoinAgg(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	r, err := ac.singleBucket(d, nil)
	if r != nil {
		r.javaClass = "InternalChildren"
	}
	return r, err
}
