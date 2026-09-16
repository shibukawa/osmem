package engine

import (
	"encoding/binary"
	"math"
	"math/bits"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// terms and multi_terms aggregations ----------------------------------------------

var termsFields = valuesSourceFields("terms", true, false, map[string]int{
	"show_term_doc_count_error": vtBool, "size": vtNumber, "shard_size": vtNumber, "min_doc_count": vtNumber,
	"shard_min_doc_count": vtNumber, "execution_hint": vtString, "collect_mode": vtString, "order": vtObjectArray,
	"include": vtObjectArrayOrString, "exclude": vtStringArray,
})

type termsAggSpec struct {
	vs          vsConfig
	size        int
	shardSize   int // -1: default
	minDoc      int64
	shardMinDoc int64
	showErr     bool
	hint        string
	orders      []bucketOrder
	ie          *includeExclude
}

// bucketCountThresholds parses size, shard_size, min_doc_count and
// shard_min_doc_count (TermsAggregator.BucketCountThresholds).
func parseThresholds(of objFields, d *aggDef, size, shardSize *int, minDoc, shardMinDoc *int64) error {
	body := d.body
	if _, ok := body["size"]; ok {
		n, err := of.intValue(body, "size")
		if err != nil {
			return err
		}
		if n <= 0 {
			return of.failed(body, "size", errIllegalArgument("[size] must be greater than 0. Found [%d] in [%s]", n, d.name))
		}
		*size = n
	}
	if _, ok := body["shard_size"]; ok && shardSize != nil {
		n, err := of.intValue(body, "shard_size")
		if err != nil {
			return err
		}
		if n <= 0 {
			return of.failed(body, "shard_size", errIllegalArgument("[shardSize] must be greater than 0. Found [%d] in [%s]", n, d.name))
		}
		*shardSize = n
	}
	if _, ok := body["min_doc_count"]; ok && minDoc != nil {
		n, err := of.longValue(body, "min_doc_count")
		if err != nil {
			return err
		}
		if n < 0 {
			return of.failed(body, "min_doc_count", errIllegalArgument("[minDocCount] must be greater than or equal to 0. Found [%d] in [%s]", n, d.name))
		}
		*minDoc = n
	}
	if _, ok := body["shard_min_doc_count"]; ok && shardMinDoc != nil {
		n, err := of.longValue(body, "shard_min_doc_count")
		if err != nil {
			return err
		}
		if n < 0 {
			return of.failed(body, "shard_min_doc_count", errIllegalArgument("[shardMinDocCount] must be greater than or equal to 0. Found [%d] in [%s]", n, d.name))
		}
		*shardMinDoc = n
	}
	return nil
}

func parseCollectMode(of objFields, body M) error {
	if v, ok := body["collect_mode"].(string); ok && v != "depth_first" && v != "breadth_first" {
		return of.failed(body, "collect_mode", &Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "no [collect_mode] found for value [" + v + "]"})
	}
	return nil
}

func parseTermsAgg(ps *aggParser, d *aggDef) error {
	of := termsFields
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &termsAggSpec{size: 10, shardSize: -1, minDoc: 1}
	var err error
	if spec.vs, err = parseVSConfig(of, body); err != nil {
		return err
	}
	if err := parseThresholds(of, d, &spec.size, &spec.shardSize, &spec.minDoc, &spec.shardMinDoc); err != nil {
		return err
	}
	if _, ok := body["show_term_doc_count_error"]; ok {
		if spec.showErr, err = of.boolValue(body, "show_term_doc_count_error"); err != nil {
			return err
		}
	}
	if err := parseCollectMode(of, body); err != nil {
		return err
	}
	spec.hint, _ = body["execution_hint"].(string)
	orders, err := parseBucketOrders(of, body, "order")
	if err != nil {
		return err
	}
	spec.orders = withKeyTiebreak(orders, []bucketOrder{{path: "_count"}})
	if spec.ie, err = parseIncludeExclude(of, body); err != nil {
		return err
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

var termsKinds = []vsKind{vsBytes, vsIP, vsDate, vsBoolean, vsNumeric}

func prepareTerms(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*termsAggSpec)
	vs, err := pc.resolve(d, 0, &spec.vs, "terms", vsBytes, termsKinds...)
	if err != nil {
		return err
	}
	if spec.hint != "" && (vs.kind == vsBytes || vs.kind == vsIP) && spec.hint != "map" && spec.hint != "global_ordinals" {
		return errIllegalArgument("Unknown `execution_hint`: [%s], expected any of [map, global_ordinals]", spec.hint)
	}
	accept, err := spec.ie.compile(d, vs)
	if err != nil {
		return err
	}
	pc.ac.setAux(d, 0, pc.ix, accept)
	return validateOrders(d, spec.orders)
}

// termKey is a term: a string for bytes and ip sources, a number otherwise.
type termKey struct {
	s   string
	n   float64
	num bool
}

// termValues returns the distinct terms of a hit.
func termValues(vs *valuesSource, h *hit) []termKey {
	switch vs.kind {
	case vsBytes, vsIP:
		strs := vs.strs(h)
		out := make([]termKey, len(strs))
		for i, s := range strs {
			out[i] = termKey{s: s}
		}
		return out
	case vsGeoPoint, vsRange:
		return nil
	}
	nums := vs.nums(h)
	out := make([]termKey, 0, len(nums))
	for i, n := range nums {
		if i > 0 && n == nums[i-1] {
			continue
		}
		out = append(out, termKey{n: n, num: true})
	}
	return out
}

// termsClass determines the reduced terms type over the searched indices.
func (ac *aggContext) termsClass(d *aggDef, slot int) (typed string, numeric bool, err error) {
	class := ""
	for _, ix := range ac.indices {
		vs := ac.source(d, slot, ix)
		if vs == nil || (vs.unmapped && vs.missing == nil) {
			continue
		}
		c := "sterms"
		switch vs.kind {
		case vsNumeric, vsDate, vsBoolean:
			c = "lterms"
			if vs.floating {
				c = "dterms"
			}
			// unsigned_long has its own typed_keys prefix and result type
			// (UnsignedLongTerms), distinct from a plain signed long
			if vs.kind == vsNumeric && vs.f != nil && vs.f.Type == TypeUnsignedLong {
				c = "ulterms"
			}
		}
		switch {
		case class == "":
			class = c
		case class == c:
		case class != "sterms" && c != "sterms":
			class = "dterms"
		default:
			return "", false, reduceFailure(errAggExecution("Merging/Reducing the aggregations failed when computing the aggregation [%s] because the field you gave in the aggregation query existed as two different types in two different indices", d.name))
		}
	}
	if class == "" {
		return "sterms", false, nil
	}
	return class, class != "sterms", nil
}

type termBucket struct {
	key    termKey
	counts map[*Index]int64
	hits   map[*Index][]*hit
}

func collectTerms(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*termsAggSpec)
	typed, _, err := ac.termsClass(d, 0)
	if err != nil {
		return nil, err
	}
	res := &aggResult{kind: resBuckets, typed: typed, javaClass: termsJavaClass(typed)}
	groups := map[termKey]*termBucket{}
	var keys []termKey
	get := func(k termKey) *termBucket {
		tb, ok := groups[k]
		if !ok {
			tb = &termBucket{key: k, counts: map[*Index]int64{}, hits: map[*Index][]*hit{}}
			groups[k] = tb
			keys = append(keys, k)
		}
		return tb
	}
	for _, h := range hits {
		vs := ac.source(d, 0, h.ix)
		if vs == nil || (vs.unmapped && vs.missing == nil) {
			continue
		}
		accept, _ := ac.aux(d, 0, h.ix).(func(termKey) bool)
		for _, k := range termValues(vs, h) {
			if accept != nil && !accept(k) {
				continue
			}
			tb := get(k)
			tb.counts[h.ix]++
			tb.hits[h.ix] = append(tb.hits[h.ix], h)
		}
	}
	shardSize := spec.shardSize
	if shardSize < 0 {
		shardSize = int(math.Min(math.MaxInt32, float64(float64(spec.size)*1.5)+10))
		if isKeyOrder(spec.orders[0]) {
			shardSize = spec.size
		}
	}
	if shardSize < spec.size {
		shardSize = spec.size
	}
	countDesc := spec.orders[0].path == "_count" && !spec.orders[0].asc
	if spec.minDoc == 0 {
		for _, ix := range ac.indices {
			vs := ac.source(d, 0, ix)
			if vs == nil || (vs.unmapped && vs.missing == nil) {
				continue
			}
			inShard := 0
			for _, tb := range groups {
				if tb.counts[ix] > 0 {
					inShard++
				}
			}
			if countDesc && inShard >= shardSize {
				continue
			}
			accept, _ := ac.aux(d, 0, ix).(func(termKey) bool)
			for _, h := range ac.indexDocs(ix) {
				for _, k := range termValues(vs, h) {
					if accept != nil && !accept(k) {
						continue
					}
					tb := get(k)
					if _, ok := tb.counts[ix]; !ok {
						tb.counts[ix] = 0
					}
				}
			}
		}
	}
	keyCmp := termsKeyCompare(ac, d)
	usesAggs := orderUsesAggregations(spec.orders)
	merged := map[termKey]*bucket{}
	var mergedKeys []termKey
	var other int64
	for _, ix := range ac.indices {
		var cands []*bucket
		var total int64
		for _, k := range keys {
			tb := groups[k]
			count, ok := tb.counts[ix]
			if !ok {
				continue
			}
			total += count
			if count < spec.shardMinDoc && len(ac.indices) > 1 {
				continue
			}
			cands = append(cands, &bucket{sortKey: k, docCount: count, hits: tb.hits[ix]})
		}
		if len(cands) == 0 {
			continue
		}
		if usesAggs {
			if err := ac.collectSubs(d, cands); err != nil {
				return nil, err
			}
		}
		sortBucketList(cands, spec.orders, keyCmp)
		if len(cands) > shardSize {
			cands = cands[:shardSize]
		}
		for _, c := range cands {
			total -= c.docCount
			k := c.sortKey.(termKey)
			m, ok := merged[k]
			if !ok {
				m = &bucket{sortKey: k, subs: c.subs}
				merged[k] = m
				mergedKeys = append(mergedKeys, k)
			} else {
				m.subs = nil
			}
			m.docCount += c.docCount
			m.hits = append(m.hits, c.hits...)
		}
		other += total
	}
	final := make([]*bucket, 0, len(mergedKeys))
	for _, k := range mergedKeys {
		b := merged[k]
		if b.docCount < spec.minDoc {
			continue
		}
		if usesAggs && b.subs == nil {
			if err := ac.collectSubs(d, []*bucket{b}); err != nil {
				return nil, err
			}
		}
		final = append(final, b)
	}
	sortBucketList(final, spec.orders, keyCmp)
	if len(final) > spec.size {
		for _, b := range final[spec.size:] {
			other += b.docCount
		}
		final = final[:spec.size]
	}
	if !usesAggs {
		if err := ac.collectSubs(d, final); err != nil {
			return nil, err
		}
	}
	for _, b := range final {
		ac.renderTermKey(d, b, typed)
		if spec.showErr {
			b.fields = M{"doc_count_error_upper_bound": int64(0)}
		}
	}
	res.buckets = final
	res.fields = M{"doc_count_error_upper_bound": int64(0), "sum_other_doc_count": other}
	return res, nil
}

func termsJavaClass(typed string) string {
	switch typed {
	case "lterms":
		return "LongTerms"
	case "ulterms":
		return "UnsignedLongTerms"
	case "dterms":
		return "DoubleTerms"
	}
	return "StringTerms"
}

// termsFormat returns the format of the first mapped values source.
func (ac *aggContext) termsSource(d *aggDef, slot int) *valuesSource {
	var fallback *valuesSource
	for _, ix := range ac.indices {
		vs := ac.source(d, slot, ix)
		if vs == nil {
			continue
		}
		if fallback == nil {
			fallback = vs
		}
		if !vs.unmapped || vs.missing != nil {
			return vs
		}
	}
	return fallback
}

func (ac *aggContext) renderTermKey(d *aggDef, b *bucket, typed string) {
	k := b.sortKey.(termKey)
	vs := ac.termsSource(d, 0)
	format := rawFormat
	if vs != nil {
		format = vs.format
	}
	switch typed {
	case "lterms", "ulterms":
		b.key = int64(k.n)
		b.keyString = format.stringLong(int64(k.n))
		b.asString = !format.raw()
	case "dterms":
		b.key = k.n
		b.keyString = format.stringDouble(k.n)
		b.asString = !format.raw()
	default:
		b.key = k.s
		b.keyString = k.s
	}
}

// termsKeyCompare orders terms: bytes by UTF-8, ip addresses by their binary
// form, numbers numerically.
func termsKeyCompare(ac *aggContext, d *aggDef) func(a, b *bucket) int {
	ip := false
	if vs := ac.termsSource(d, 0); vs != nil && vs.kind == vsIP {
		ip = true
	}
	return func(a, b *bucket) int {
		ka, kb := a.sortKey.(termKey), b.sortKey.(termKey)
		return compareTermKeys(ka, kb, ip)
	}
}

func compareTermKeys(ka, kb termKey, ip bool) int {
	if ka.num || kb.num {
		return javaDoubleCompare(ka.n, kb.n)
	}
	if ip {
		return strings.Compare(string(ipBytes(ka.s)), string(ipBytes(kb.s)))
	}
	return strings.Compare(ka.s, kb.s)
}

// indexDocs returns every document of an index as hits, nested objects
// included (the documents a shard iterates to fill in zero-count terms).
func (ac *aggContext) indexDocs(ix *Index) []*hit {
	if ac.docsCache == nil {
		ac.docsCache = map[*Index][]*hit{}
	}
	if docs, ok := ac.docsCache[ix]; ok {
		return docs
	}
	roots := make([]*Doc, 0, len(ix.docs))
	for _, doc := range ix.docs {
		roots = append(roots, doc)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].SeqNo < roots[j].SeqNo })
	paths := ix.Mapping.nestedPaths()
	var out []*hit
	for _, root := range roots {
		for _, p := range paths {
			for _, nd := range ix.nestedDescendants(root, p) {
				out = append(out, &hit{ix: ix, doc: nd})
			}
		}
		out = append(out, &hit{ix: ix, doc: root})
	}
	ac.docsCache[ix] = out
	return out
}

func (ac *aggContext) setAux(d *aggDef, slot int, ix *Index, v any) {
	if ac.auxs == nil {
		ac.auxs = map[vsKey]any{}
	}
	ac.auxs[vsKey{d, slot, ix}] = v
}

func (ac *aggContext) aux(d *aggDef, slot int, ix *Index) any {
	return ac.auxs[vsKey{d, slot, ix}]
}

// include/exclude (IncludeExclude) ----------------------------------------------------

type includeExclude struct {
	incRegex, excRegex   *string
	incSet, excSet       []string
	partition, numParts  int
	partitioned, hasSets bool
}

func (ie *includeExclude) regexBased() bool { return ie.incRegex != nil || ie.excRegex != nil }

func setValues(v []any) []string {
	out := make([]string, 0, len(v))
	for _, e := range v {
		out = append(out, missingString(e))
	}
	return out
}

func parseIncludeExclude(of objFields, body M) (*includeExclude, error) {
	var inc, exc *includeExclude
	if v, ok := body["include"]; ok {
		inc = &includeExclude{}
		switch t := v.(type) {
		case string:
			inc.incRegex = &t
		case []any:
			inc.incSet, inc.hasSets = setValues(t), true
		case M:
			var partition, num *int
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				n, err := objFields{name: of.name}.intValue(t, k)
				if err != nil {
					return nil, of.failed(body, "include", err.(*Error).Cause.atParser(valueTok(t, k)))
				}
				switch k {
				case "partition":
					partition = &n
				case "num_partitions":
					num = &n
				default:
					return nil, of.failed(body, "include", (&Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "Unknown parameter in Include/Exclude clause: " + k}).
						atParser(valueTok(t, k)))
				}
			}
			if partition == nil {
				return nil, of.failed(body, "include", errIllegalArgument("Missing [partition] parameter for partition-based include"))
			}
			if num == nil {
				return nil, of.failed(body, "include", errIllegalArgument("Missing [num_partitions] parameter for partition-based include"))
			}
			if *partition < 0 || *partition >= *num {
				return nil, of.failed(body, "include", errIllegalArgument("Partition must be >=0 and < numPartition which is %d", *num))
			}
			inc.partitioned, inc.partition, inc.numParts = true, *partition, *num
		}
	}
	if v, ok := body["exclude"]; ok {
		exc = &includeExclude{}
		switch t := v.(type) {
		case string:
			exc.excRegex = &t
		case []any:
			exc.excSet, exc.hasSets = setValues(t), true
		}
	}
	if inc == nil {
		return exc, nil
	}
	if exc == nil {
		return inc, nil
	}
	if inc.partitioned {
		return nil, of.failed(body, "exclude", errIllegalArgument("Cannot specify any excludes when using a partition-based include"))
	}
	incMethod, excMethod := "set", "set"
	if inc.incRegex != nil {
		incMethod = "regex"
	}
	if exc.excRegex != nil {
		excMethod = "regex"
	}
	if incMethod != excMethod {
		return nil, of.failed(body, "exclude", errIllegalArgument("Cannot mix a %s-based include with a %s-based method", incMethod, excMethod))
	}
	inc.excRegex, inc.excSet = exc.excRegex, exc.excSet
	inc.hasSets = inc.hasSets || exc.hasSets
	return inc, nil
}

// compile builds the term filter of an include/exclude for a values source.
func (ie *includeExclude) compile(d *aggDef, vs *valuesSource) (func(termKey) bool, error) {
	if ie == nil {
		return nil, nil
	}
	numeric := vs.kind == vsNumeric || vs.kind == vsDate || vs.kind == vsBoolean
	if ie.partitioned {
		n, p := ie.numParts, ie.partition
		if numeric {
			floating := vs.floating
			return func(k termKey) bool {
				v := int64(k.n)
				if floating {
					v = int64(math.Float64bits(k.n) ^ 0x8000000000000000)
				}
				return floorModInt64(mix64(v), int64(n)) == int64(p)
			}, nil
		}
		return func(k termKey) bool {
			return floorModInt32(murmur3x86(k.s, 31), int32(n)) == int32(p)
		}, nil
	}
	if ie.regexBased() {
		if numeric {
			return nil, errAggExecution("Aggregation [%s] cannot support regular expression style include/exclude settings as they can only be applied to string fields. Use an array of numeric values for include/exclude clauses used to filter numeric fields", d.name)
		}
		var inc, exc *aggLuceneRegexp
		var err error
		if ie.incRegex != nil {
			if inc, err = compileAggLuceneRegexp(*ie.incRegex); err != nil {
				return nil, err
			}
		}
		if ie.excRegex != nil {
			if exc, err = compileAggLuceneRegexp(*ie.excRegex); err != nil {
				return nil, err
			}
		}
		return func(k termKey) bool {
			return (inc == nil || inc.match(k.s)) && (exc == nil || !exc.match(k.s))
		}, nil
	}
	if numeric {
		parse := func(s string) (float64, error) {
			switch vs.kind {
			case vsDate:
				t, err := ParseDateMath(s, vs.format.date, timeNow(), vs.format.loc, false)
				if err != nil {
					return 0, errDateParse(s, vs.format.date)
				}
				return float64(t.UnixMilli()), nil
			case vsBoolean:
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
			if !vs.floating {
				f = float64(javaRound(math.Floor(f)))
			}
			return f, nil
		}
		incs, excs := map[float64]bool{}, map[float64]bool{}
		for _, s := range ie.incSet {
			f, err := parse(s)
			if err != nil {
				return nil, err
			}
			incs[f] = true
		}
		for _, s := range ie.excSet {
			f, err := parse(s)
			if err != nil {
				return nil, err
			}
			excs[f] = true
		}
		hasInc := ie.incSet != nil
		return func(k termKey) bool { return (!hasInc || incs[k.n]) && !excs[k.n] }, nil
	}
	incs, excs := map[string]bool{}, map[string]bool{}
	for _, s := range ie.incSet {
		if vs.kind == vsIP {
			s = canonicalIP(s)
		}
		incs[s] = true
	}
	for _, s := range ie.excSet {
		if vs.kind == vsIP {
			s = canonicalIP(s)
		}
		excs[s] = true
	}
	hasInc := ie.incSet != nil
	return func(k termKey) bool { return (!hasInc || incs[k.s]) && !excs[k.s] }, nil
}

// mix64 is BitMixer.mix64.
func mix64(z int64) int64 {
	u := uint64(z)
	u = (u ^ (u >> 32)) * 0x4cd6944c5cc20b6d
	u = (u ^ (u >> 29)) * 0xfc12c5b19d3259e9
	return int64(u ^ (u >> 32))
}

func floorModInt64(x, n int64) int64 {
	m := x % n
	if m != 0 && (m < 0) != (n < 0) {
		m += n
	}
	return m
}

func floorModInt32(x, n int32) int32 {
	m := x % n
	if m != 0 && (m < 0) != (n < 0) {
		m += n
	}
	return m
}

// murmur3x86 is StringHelper.murmurhash3_x86_32 over the UTF-8 bytes.
func murmur3x86(s string, seed uint32) int32 {
	data := []byte(s)
	const c1, c2 = 0xcc9e2d51, 0x1b873593
	h1 := seed
	n := len(data)
	rounded := n &^ 3
	for i := 0; i < rounded; i += 4 {
		k1 := binary.LittleEndian.Uint32(data[i:])
		k1 *= c1
		k1 = bits.RotateLeft32(k1, 15)
		k1 *= c2
		h1 ^= k1
		h1 = bits.RotateLeft32(h1, 13)
		h1 = h1*5 + 0xe6546b64
	}
	var k1 uint32
	switch n & 3 {
	case 3:
		k1 = uint32(data[rounded+2]) << 16
		fallthrough
	case 2:
		k1 |= uint32(data[rounded+1]) << 8
		fallthrough
	case 1:
		k1 |= uint32(data[rounded])
		k1 *= c1
		k1 = bits.RotateLeft32(k1, 15)
		k1 *= c2
		h1 ^= k1
	}
	h1 ^= uint32(n)
	h1 ^= h1 >> 16
	h1 *= 0x85ebca6b
	h1 ^= h1 >> 13
	h1 *= 0xc2b2ae35
	h1 ^= h1 >> 16
	return int32(h1)
}

// multi_terms ---------------------------------------------------------------------------

var multiTermsFields = objFields{name: "multi_terms", fields: map[string]int{
	"terms": vtObjectArray, "size": vtNumber, "shard_size": vtNumber, "min_doc_count": vtNumber, "shard_min_doc_count": vtNumber,
	"show_term_doc_count_error": vtBool, "collect_mode": vtString, "order": vtObjectArray,
}}

var multiTermsTermFields = valuesSourceFields("multi_terms", true, true, nil)

type multiTermsSpec struct {
	terms   []vsConfig
	size    int
	minDoc  int64
	showErr bool
	orders  []bucketOrder
	noTerms bool
}

func parseMultiTerms(ps *aggParser, d *aggDef) error {
	of := multiTermsFields
	body := d.body
	if err := of.check(body); err != nil {
		return err
	}
	spec := &multiTermsSpec{size: 10, minDoc: 1}
	shard := -1
	var shardMin int64
	if err := parseThresholds(of, d, &spec.size, &shard, &spec.minDoc, &shardMin); err != nil {
		return err
	}
	var err error
	if _, ok := body["show_term_doc_count_error"]; ok {
		if spec.showErr, err = of.boolValue(body, "show_term_doc_count_error"); err != nil {
			return err
		}
	}
	if err := parseCollectMode(of, body); err != nil {
		return err
	}
	orders, err := parseBucketOrders(of, body, "order")
	if err != nil {
		return err
	}
	spec.orders = withKeyTiebreak(orders, []bucketOrder{{path: "_count"}})
	raw, ok := body["terms"]
	if !ok {
		spec.noTerms = true
		d.spec = spec
		return nil
	}
	items := getList(raw)
	for _, item := range items {
		tm, ok := item.(M)
		if !ok {
			return of.failed(body, "terms", errParsing("Failed to parse object: expecting token of type [START_OBJECT] but found [%s]", jsonTokenName(item)).atParser(noTok))
		}
		if err := multiTermsTermFields.check(tm); err != nil {
			return of.failed(body, "terms", err.(*Error))
		}
		cfg, err := parseVSConfig(multiTermsTermFields, tm)
		if err != nil {
			return of.failed(body, "terms", err.(*Error))
		}
		if !cfg.hasField && !cfg.script {
			return of.failed(body, "terms", errIllegalArgument("[field] and [script] cannot both be null.  Please specify one or the other.").atParser(endTok(tm)))
		}
		if cfg.script {
			return errScript(d)
		}
		spec.terms = append(spec.terms, cfg)
	}
	if len(spec.terms) < 2 {
		return of.failed(body, "terms", errIllegalArgument("multi term aggregation must has at least 2 terms. Found [%d] in [%s] Use terms aggregation for single term aggregation", len(spec.terms), d.name))
	}
	d.spec = spec
	return nil
}

func prepareMultiTerms(pc *prepareCtx, d *aggDef) error {
	spec := d.spec.(*multiTermsSpec)
	if spec.noTerms {
		return errJava(http.StatusInternalServerError, "null_pointer_exception", "Cannot invoke \"java.util.List.stream()\" because \"multiTermConfigs\" is null")
	}
	for i := range spec.terms {
		if _, err := pc.resolve(d, i, &spec.terms[i], "multi_terms", vsBytes, termsKinds...); err != nil {
			return err
		}
	}
	return validateOrders(d, spec.orders)
}

func collectMultiTerms(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error) {
	spec := d.spec.(*multiTermsSpec)
	// a term reading bytes in one index and numbers in another cannot be
	// reduced (OpenSearch fails casting the keys)
	for i := range spec.terms {
		kinds := map[string]bool{}
		for _, ix := range ac.indices {
			vs := ac.source(d, i, ix)
			if vs == nil || (vs.unmapped && vs.missing == nil) {
				continue
			}
			switch {
			case vs.kind == vsBytes || vs.kind == vsIP:
				kinds["bytes"] = true
			default:
				kinds["number"] = true
			}
		}
		if kinds["bytes"] && kinds["number"] {
			return nil, reduceFailure(errJava(http.StatusInternalServerError, "class_cast_exception", "class org.apache.lucene.util.BytesRef cannot be cast to class java.lang.Long (org.apache.lucene.util.BytesRef is in unnamed module of loader 'app'; java.lang.Long is in module java.base of loader 'bootstrap')"))
		}
	}
	type group struct {
		keys []termKey
		b    *bucket
	}
	groups := map[string]*group{}
	var order []string
	encode := func(keys []termKey) string {
		var sb strings.Builder
		for _, k := range keys {
			if k.num {
				sb.WriteString("n" + strconv.FormatFloat(k.n, 'g', -1, 64))
			} else {
				sb.WriteString("s" + strconv.Itoa(len(k.s)) + ":" + k.s)
			}
		}
		return sb.String()
	}
	combos := func(h *hit) [][]termKey {
		out := [][]termKey{{}}
		for i := range spec.terms {
			vs := ac.source(d, i, h.ix)
			if vs == nil || (vs.unmapped && vs.missing == nil) {
				return nil
			}
			vals := termValues(vs, h)
			if len(vals) == 0 {
				return nil
			}
			var next [][]termKey
			for _, c := range out {
				for _, v := range vals {
					next = append(next, append(append([]termKey(nil), c...), v))
				}
			}
			out = next
		}
		return out
	}
	add := func(h *hit, fill bool) {
		seen := map[string]bool{}
		for _, keys := range combos(h) {
			id := encode(keys)
			if seen[id] {
				continue
			}
			seen[id] = true
			g, ok := groups[id]
			if !ok {
				g = &group{keys: keys, b: &bucket{sortKey: keys}}
				groups[id] = g
				order = append(order, id)
			}
			if !fill {
				g.b.docCount++
				g.b.hits = append(g.b.hits, h)
			}
		}
	}
	for _, h := range hits {
		add(h, false)
	}
	if spec.minDoc == 0 {
		for _, ix := range ac.indices {
			for _, h := range ac.indexDocs(ix) {
				add(h, true)
			}
		}
	}
	var buckets []*bucket
	for _, id := range order {
		if b := groups[id].b; b.docCount >= spec.minDoc {
			buckets = append(buckets, b)
		}
	}
	ips := make([]bool, len(spec.terms))
	for i := range spec.terms {
		if vs := ac.termsSource(d, i); vs != nil && vs.kind == vsIP {
			ips[i] = true
		}
	}
	keyCmp := func(a, b *bucket) int {
		ka, kb := a.sortKey.([]termKey), b.sortKey.([]termKey)
		for i := range ka {
			if c := compareTermKeys(ka[i], kb[i], ips[i]); c != 0 {
				return c
			}
		}
		return 0
	}
	usesAggs := orderUsesAggregations(spec.orders)
	if usesAggs {
		if err := ac.collectSubs(d, buckets); err != nil {
			return nil, err
		}
	}
	sortBucketList(buckets, spec.orders, keyCmp)
	var other int64
	if len(buckets) > spec.size {
		for _, b := range buckets[spec.size:] {
			other += b.docCount
		}
		buckets = buckets[:spec.size]
	}
	if !usesAggs {
		if err := ac.collectSubs(d, buckets); err != nil {
			return nil, err
		}
	}
	formats := make([]*valueFormat, len(spec.terms))
	floating := make([]bool, len(spec.terms))
	for i := range spec.terms {
		formats[i] = rawFormat
		if vs := ac.termsSource(d, i); vs != nil {
			formats[i] = vs.format
			floating[i] = vs.floating
		}
	}
	for _, b := range buckets {
		keys := b.sortKey.([]termKey)
		out := make([]any, len(keys))
		strs := make([]string, len(keys))
		for i, k := range keys {
			var v any
			switch {
			case !k.num:
				v = k.s
			case floating[i]:
				v = formats[i].formatDouble(k.n)
			default:
				v = formats[i].formatLong(int64(k.n))
			}
			out[i] = v
			strs[i] = objectString(v)
		}
		b.key = out
		b.keyString = strings.Join(strs, "|")
		b.asString = true
		if spec.showErr {
			b.fields = M{"doc_count_error_upper_bound": int64(0)}
		}
	}
	return &aggResult{kind: resBuckets, buckets: buckets, javaClass: "InternalMultiTerms",
		fields: M{"doc_count_error_upper_bound": int64(0), "sum_other_doc_count": other}}, nil
}
