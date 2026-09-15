package engine

import (
	"cmp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2/analysis"
)

// Sorting.
//
// Sort keys are computed once per hit with sort specs resolved once per
// index, and kept typed so comparing them does not go through interfaces.
// When the request needs only the first from+size hits, those are selected
// with a bounded heap instead of ordering every hit. Ties are broken by
// index, sequence number and nested position, then by the position before
// sorting, so the result is the one a stable sort would give.

type sortKeyKind uint8

const (
	keyNone  sortKeyKind = iota // no value
	keyNum                      // num
	keyStr                      // str
	keyOther                    // other: a missing value that is neither
)

// sortKey is the comparable key of a hit for one sort spec.
type sortKey struct {
	kind     sortKeyKind
	sentinel bool // stands for a missing value; reported as missingSortValue does
	num      float64
	str      string
	other    any
}

func sortKeyOf(v any, sentinel bool) sortKey {
	switch t := v.(type) {
	case nil:
		return sortKey{sentinel: sentinel}
	case float64:
		return sortKey{kind: keyNum, num: t, sentinel: sentinel}
	case string:
		return sortKey{kind: keyStr, str: t, sentinel: sentinel}
	}
	return sortKey{kind: keyOther, other: v, sentinel: sentinel}
}

func (k sortKey) value() any {
	switch k.kind {
	case keyNum:
		return k.num
	case keyStr:
		return k.str
	case keyOther:
		return k.other
	}
	return nil
}

func compareFloats(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareKeys is compareSortValues for typed keys.
func compareKeys(a, b sortKey, s sortSpec) int {
	if a.kind == keyNone || b.kind == keyNone {
		if a.kind == b.kind {
			return 0
		}
		first := s.missing == "_first"
		if (a.kind == keyNone) == first {
			return -1
		}
		return 1
	}
	var c int
	switch {
	case a.kind == keyNum && b.kind == keyNum:
		c = compareFloats(a.num, b.num)
	case a.kind == keyStr && b.kind == keyStr:
		c = strings.Compare(a.str, b.str)
	default:
		c = compareValues(a.value(), b.value())
	}
	if s.desc {
		return -c
	}
	return c
}

// compareKeyValue is compareSortValues for a typed key and a search_after
// value.
func compareKeyValue(k sortKey, v any, s sortSpec) int {
	c, typed := 0, false
	if n, ok := v.(float64); ok && k.kind == keyNum {
		c, typed = compareFloats(k.num, n), true
	} else if str, ok := v.(string); ok && k.kind == keyStr {
		c, typed = strings.Compare(k.str, str), true
	}
	if !typed {
		return compareSortValues(k.value(), v, s)
	}
	if s.desc {
		return -c
	}
	return c
}

// compareHitOrder breaks sort ties: index name, sequence number, then the
// position of nested objects within their document.
func compareHitOrder(a, b *hit) int {
	if a.ix != b.ix {
		return strings.Compare(a.ix.Name, b.ix.Name)
	}
	if a.doc.SeqNo != b.doc.SeqNo {
		return cmp.Compare(a.doc.SeqNo, b.doc.SeqNo)
	}
	for k := 0; k < len(a.doc.nested) && k < len(b.doc.nested); k++ {
		if a.doc.nested[k].offset != b.doc.nested[k].offset {
			return cmp.Compare(a.doc.nested[k].offset, b.doc.nested[k].offset)
		}
	}
	return cmp.Compare(len(a.doc.nested), len(b.doc.nested))
}

func compareHits(a, b *hit, specs []sortSpec) int {
	for k, s := range specs {
		if c := compareKeys(a.keys[k], b.keys[k], s); c != 0 {
			return c
		}
	}
	return compareHitOrder(a, b)
}

// sortField is a sort spec resolved against one index.
type sortField struct {
	spec     sortSpec
	f        *Field // nil for _score, _doc, _id, _index and unmapped fields
	base     string
	parts    []string // path of the field's value in the source
	anc      string   // nested path the field belongs to
	slow     bool     // values come from sortFieldValues (fielddata, metadata names)
	analyzer analysis.Analyzer
	dates    *DateFormat
	miss     sortKey // key of a hit without a value
	missOut  any     // value reported for miss when it is a sentinel
	first    sortKeyKind
	nums     []float64
	strs     []string
}

// prepareSortField resolves a sort spec for the index of h, failing the way
// sorting the first hit of that index does.
func (c *Cluster) prepareSortField(h *hit, s sortSpec) (*sortField, error) {
	sf := &sortField{spec: s}
	switch s.field {
	case "_score", "_doc", "_id", "_index":
		return sf, nil
	}
	f, base, ok := h.ix.Mapping.resolve(s.field)
	if !ok {
		if s.unmappedType == "" {
			return nil, errSearchPhase(&Error{Status: 400, Type: "query_shard_exception", Reason: "No mapping found for [" + s.field + "] in order to sort on", Index: h.ix.Name})
		}
		k, o := missingSortValue(&Field{Type: s.unmappedType}, s)
		sf.miss, sf.missOut = sortKeyOf(k, true), o
		return sf, nil
	}
	fielddata := f.Type == TypeText && getBool(f.Extra, "fielddata", false)
	if f.Type == TypeText && !fielddata {
		return nil, errSearchPhase(&Error{Status: 400, Type: "illegal_argument_exception", Reason: "Text fields are not optimised for operations that require per-document field data like aggregations and sorting, so these operations are disabled by default. Please use a keyword field instead. Alternatively, set fielddata=true on [" + s.field + "] in order to load field data by uninverting the inverted index. Note that this can use significant memory.", Index: h.ix.Name})
	}
	if !fielddata && !getBool(f.Extra, "doc_values", true) {
		return nil, errDocValuesDisabled(h, s.field, f)
	}
	if fielddata {
		an, err := h.ix.analysis.analyzerNamed(f.Analyzer)
		if err != nil {
			return nil, err
		}
		sf.analyzer = an
	}
	sf.f, sf.base = f, base
	sf.anc = h.ix.Mapping.nestedAncestor(base)
	sf.slow = fielddata || s.field == "_seq_no" || s.field == "_version"
	if base != "" {
		sf.parts = strings.Split(base, ".")
	}
	sf.dates = f.Format
	if sf.dates == nil {
		sf.dates = ParseDateFormat(DefaultDateFormat)
	}
	sf.miss, sf.missOut = sf.missingKey()
	return sf, nil
}

// missingKey returns the key of a hit without a value and, when that key is
// a sentinel, the value reported for it.
func (sf *sortField) missingKey() (sortKey, any) {
	s, f := sf.spec, sf.f
	if ms, isString := s.missing.(string); s.missing != nil && (!isString || (ms != "_last" && ms != "_first")) {
		if cv, ok := convertValue(f, s.missing); ok {
			if t, isTime := cv.(time.Time); isTime {
				return sortKey{kind: keyNum, num: float64(t.UnixMilli())}, nil
			}
			return sortKeyOf(cv, false), nil
		}
	}
	k, o := missingSortValue(f, s)
	return sortKeyOf(k, true), o
}

// key computes the sort key of a hit.
func (sf *sortField) key(c *Cluster, h *hit) (sortKey, error) {
	switch sf.spec.field {
	case "_score":
		return sortKey{kind: keyNum, num: h.score}, nil
	case "_doc":
		return sortKey{kind: keyNum, num: float64(h.doc.SeqNo)}, nil
	case "_id":
		return sortKey{kind: keyStr, str: h.doc.ID}, nil
	case "_index":
		return sortKey{kind: keyStr, str: h.ix.Name}, nil
	}
	if sf.f == nil {
		return sf.miss, nil
	}
	sf.first, sf.nums, sf.strs = keyNone, sf.nums[:0], sf.strs[:0]
	switch {
	case !sf.slow && sf.anc == h.doc.level():
		sf.addSource(lookupParts(h.doc.Src, sf.parts))
	case !sf.slow && sf.spec.nested == nil:
		// a field of another nested level has no values here
	default:
		vals, err := c.sortFieldValues(h, sf.spec, sf.base)
		if err != nil {
			return sortKey{}, err
		}
		for _, v := range vals {
			if sf.analyzer == nil {
				sf.addConverted(v)
				continue
			}
			text, err := stringValue(sf.spec.field, sf.f, v)
			if err != nil {
				continue
			}
			for _, term := range tokens(sf.analyzer, text) {
				sf.addStr(term)
			}
		}
	}
	return sf.reduce(), nil
}

// addSource adds the values of a source value the way fieldValues reads
// them: arrays are flattened and null elements dropped, so null_value never
// applies.
func (sf *sortField) addSource(v any) {
	switch t := v.(type) {
	case nil:
	case []any:
		for _, e := range t {
			sf.addSource(e)
		}
	default:
		sf.addValue(t)
	}
}

// addValue adds one source value converted like convertValue.
func (sf *sortField) addValue(v any) {
	f := sf.f
	switch {
	case f.isNumeric():
		if n, ok := toFloat(v); ok {
			sf.addNum(n)
		}
	case f.isDate():
		if t, err := sf.dates.Parse(v); err == nil {
			sf.addNum(float64(t.UnixMilli()))
		}
	case f.Type == TypeBoolean:
		if b, ok := boolValue(v); ok {
			if b {
				sf.addNum(1)
			} else {
				sf.addNum(0)
			}
		}
	case f.Type == TypeGeoPoint, f.Type == TypeObject, f.Type == TypeNested:
		// not sortable values
	default:
		s, err := stringValue("", f, v)
		if err == nil && (f.IgnoreAbove <= 0 || utf8.RuneCountInString(s) <= f.IgnoreAbove) {
			sf.addStr(s)
		}
	}
}

// addConverted adds a value already converted by convertValue.
func (sf *sortField) addConverted(v any) {
	switch t := v.(type) {
	case time.Time:
		sf.addNum(float64(t.UnixMilli()))
	case bool:
		if t {
			sf.addNum(1)
		} else {
			sf.addNum(0)
		}
	case float64:
		sf.addNum(t)
	case string:
		sf.addStr(t)
	}
}

func (sf *sortField) addNum(n float64) {
	if sf.first == keyNone {
		sf.first = keyNum
	}
	sf.nums = append(sf.nums, n)
}

func (sf *sortField) addStr(s string) {
	if sf.first == keyNone {
		sf.first = keyStr
	}
	sf.strs = append(sf.strs, s)
}

// reduce turns the collected values into one key, applying the sort mode to
// multi-valued fields: strings when the first value is one, numbers
// otherwise.
func (sf *sortField) reduce() sortKey {
	switch len(sf.nums) + len(sf.strs) {
	case 0:
		return sf.miss
	case 1:
		if sf.first == keyNum {
			return sortKey{kind: keyNum, num: sf.nums[0]}
		}
		return sortKey{kind: keyStr, str: sf.strs[0]}
	}
	mode := sf.spec.mode
	if mode == "" {
		mode = "min"
		if sf.spec.desc {
			mode = "max"
		}
	}
	if sf.first == keyStr {
		sort.Strings(sf.strs)
		if mode == "max" {
			return sortKey{kind: keyStr, str: sf.strs[len(sf.strs)-1]}
		}
		return sortKey{kind: keyStr, str: sf.strs[0]}
	}
	nums := sf.nums
	sort.Float64s(nums)
	var r float64
	switch mode {
	case "max":
		r = nums[len(nums)-1]
	case "sum":
		for _, n := range nums {
			r += n
		}
	case "avg":
		for _, n := range nums {
			r += n
		}
		r /= float64(len(nums))
	case "median":
		r = nums[len(nums)/2]
		if len(nums)%2 == 0 {
			r = (nums[len(nums)/2-1] + nums[len(nums)/2]) / 2
		}
	default:
		r = nums[0]
	}
	return sortKey{kind: keyNum, num: r}
}

// output returns the sort value reported for a hit's key.
func (sf *sortField) output(h *hit, k sortKey) any {
	switch sf.spec.field {
	case "_score":
		return h.score
	case "_doc":
		return h.doc.SeqNo
	case "_id":
		return h.doc.ID
	case "_index":
		return h.ix.Name
	}
	if k.sentinel {
		return sf.missOut
	}
	return sortOutput(sf.f, k.value(), sf.spec)
}

// orderHits sorts hits by the request's sort and returns the first limit of
// them (every one when limit < 0) in order, with their reported sort values.
// keep, when set, drops hits before ordering (search_after). hits keeps its
// order.
func (c *Cluster) orderHits(hits []*hit, sr *searchRequest, limit int, keep func(*hit) bool) ([]*hit, error) {
	specs := sr.sort
	fields := map[*Index][]*sortField{}
	var lastIx *Index
	var last []*sortField
	resolve := func(h *hit) ([]*sortField, error) {
		if h.ix == lastIx {
			return last, nil
		}
		sfs, ok := fields[h.ix]
		if !ok {
			sfs = make([]*sortField, len(specs))
			for k, s := range specs {
				sf, err := c.prepareSortField(h, s)
				if err != nil {
					return nil, err
				}
				sfs[k] = sf
			}
			fields[h.ix] = sfs
		}
		lastIx, last = h.ix, sfs
		return sfs, nil
	}
	if limit == 0 && !hasNestedSort(specs) {
		// no hit is returned: only check that the sort applies
		for _, h := range hits {
			if _, err := resolve(h); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	n := len(specs)
	keys := make([]sortKey, len(hits)*n)
	kinds := make([]sortKeyKind, n) // the kind of every key of a spec; keyOther when they mix
	for i, h := range hits {
		sfs, err := resolve(h)
		if err != nil {
			return nil, err
		}
		h.keys, h.order = keys[i*n:(i+1)*n:(i+1)*n], i
		for k, sf := range sfs {
			key, err := sf.key(c, h)
			if err != nil {
				return nil, err
			}
			h.keys[k] = key
			switch {
			case key.kind == keyNone || kinds[k] == keyOther:
			case key.kind == keyOther || key.num != key.num, kinds[k] != keyNone && kinds[k] != key.kind:
				kinds[k] = keyOther
			default:
				kinds[k] = key.kind
			}
		}
	}

	// mixed or NaN keys may not order totally: they keep the stable sort of
	// every hit, filtered afterwards, which a subset would not reproduce
	mixed := slices.Contains(kinds, keyOther)
	list := hits
	if keep != nil && !mixed {
		list = make([]*hit, 0, len(hits))
		for _, h := range hits {
			if keep(h) {
				list = append(list, h)
			}
		}
	}
	switch {
	case limit == 0:
		list = nil
	case mixed:
		list = slices.Clone(list)
		sort.SliceStable(list, func(i, j int) bool { return compareHits(list[i], list[j], specs) < 0 })
		if keep != nil {
			list = slices.DeleteFunc(list, func(h *hit) bool { return !keep(h) })
		}
	case limit >= 0 && limit < len(list):
		list = topHits(list, limit, func(a, b *hit) int {
			if c := compareHits(a, b, specs); c != 0 {
				return c
			}
			return cmp.Compare(a.order, b.order)
		})
	default:
		list = slices.Clone(list)
		slices.SortFunc(list, func(a, b *hit) int {
			if c := compareHits(a, b, specs); c != 0 {
				return c
			}
			return cmp.Compare(a.order, b.order)
		})
	}
	if limit >= 0 && len(list) > limit {
		list = list[:limit]
	}

	outs := make([]any, len(list)*n)
	for i, h := range list {
		h.sortOut = outs[i*n : (i+1)*n : (i+1)*n]
		for k, sf := range fields[h.ix] {
			h.sortOut[k] = sf.output(h, h.keys[k])
		}
	}
	return list, nil
}

func hasNestedSort(specs []sortSpec) bool {
	for _, s := range specs {
		if s.nested != nil {
			return true
		}
	}
	return false
}

// topHits returns the k smallest hits by compare, in order. It keeps the k
// best hits seen so far in a heap whose root is the worst of them.
func topHits(hits []*hit, k int, compare func(a, b *hit) int) []*hit {
	heap := make([]*hit, 0, k)
	for _, h := range hits {
		if len(heap) < k {
			heap = append(heap, h)
			for i := len(heap) - 1; i > 0; {
				p := (i - 1) / 2
				if compare(heap[i], heap[p]) <= 0 {
					break
				}
				heap[i], heap[p] = heap[p], heap[i]
				i = p
			}
			continue
		}
		if compare(h, heap[0]) >= 0 {
			continue
		}
		heap[0] = h
		for i := 0; ; {
			m, l := i, 2*i+1
			if l < len(heap) && compare(heap[l], heap[m]) > 0 {
				m = l
			}
			if r := l + 1; r < len(heap) && compare(heap[r], heap[m]) > 0 {
				m = r
			}
			if m == i {
				break
			}
			heap[i], heap[m] = heap[m], heap[i]
			i = m
		}
	}
	slices.SortFunc(heap, compare)
	return heap
}
