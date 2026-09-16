package engine

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// referenceSortValue is the per-hit sort key computation osmem used before
// sort keys were typed and sort specs resolved once per index.
func (c *Cluster) referenceSortValue(h *hit, s sortSpec) (any, any, error) {
	switch s.field {
	case "_score":
		return h.score, h.score, nil
	case "_doc":
		return float64(h.doc.SeqNo), h.doc.SeqNo, nil
	case "_id":
		return h.doc.ID, h.doc.ID, nil
	case "_index":
		return h.ix.Name, h.ix.Name, nil
	}
	f, base, ok := h.ix.Mapping.resolve(s.field)
	if !ok {
		k, o := missingSortValue(&Field{Type: s.unmappedType}, s)
		return k, o, nil
	}
	fielddata := f.Type == TypeText && getBool(f.Extra, "fielddata", false)
	vals, err := c.sortFieldValues(h, s, base)
	if err != nil {
		return nil, nil, err
	}
	if fielddata {
		analyzer, err := h.ix.analysis.analyzerNamed(f.Analyzer)
		if err != nil {
			return nil, nil, err
		}
		seen := map[string]bool{}
		terms := make([]any, 0, len(vals))
		for _, value := range vals {
			text, err := stringValue(s.field, f, value)
			if err != nil {
				continue
			}
			for _, term := range tokens(analyzer, text) {
				if !seen[term] {
					seen[term] = true
					terms = append(terms, term)
				}
			}
		}
		vals = terms
	}
	var keys []any
	for _, v := range vals {
		switch t := v.(type) {
		case time.Time:
			keys = append(keys, float64(t.UnixMilli()))
		case bool:
			if t {
				keys = append(keys, float64(1))
			} else {
				keys = append(keys, float64(0))
			}
		case float64, string:
			keys = append(keys, t)
		}
	}
	if len(keys) == 0 {
		if s.missing != nil {
			if ms, ok := s.missing.(string); ok && (ms == "_last" || ms == "_first") {
				k, o := missingSortValue(f, s)
				return k, o, nil
			}
			if cv, ok := convertValue(f, s.missing); ok {
				if t, ok := cv.(time.Time); ok {
					ms := float64(t.UnixMilli())
					return ms, sortOutput(f, ms, s), nil
				}
				return cv, sortOutput(f, cv, s), nil
			}
		}
		k, o := missingSortValue(f, s)
		return k, o, nil
	}
	if len(keys) == 1 {
		return keys[0], sortOutput(f, keys[0], s), nil
	}
	mode := s.mode
	if mode == "" {
		if s.desc {
			mode = "max"
		} else {
			mode = "min"
		}
	}
	if _, isStr := keys[0].(string); isStr {
		strs := make([]string, 0, len(keys))
		for _, k := range keys {
			if sv, ok := k.(string); ok {
				strs = append(strs, sv)
			}
		}
		sort.Strings(strs)
		if mode == "max" {
			return strs[len(strs)-1], strs[len(strs)-1], nil
		}
		return strs[0], strs[0], nil
	}
	nums := make([]float64, 0, len(keys))
	for _, k := range keys {
		if n, ok := k.(float64); ok {
			nums = append(nums, n)
		}
	}
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
	return r, sortOutput(f, r, s), nil
}

// referenceSort orders hits the way osmem did before orderHits: every key
// computed, then a stable sort. It returns the keys and reported values too.
func (c *Cluster) referenceSort(t *testing.T, hits []*hit, specs []sortSpec) ([]*hit, map[*hit][]any, map[*hit][]any) {
	t.Helper()
	vals, outs := map[*hit][]any{}, map[*hit][]any{}
	for _, h := range hits {
		for _, s := range specs {
			v, o, err := c.referenceSortValue(h, s)
			if err != nil {
				t.Fatal(err)
			}
			vals[h] = append(vals[h], v)
			outs[h] = append(outs[h], o)
		}
	}
	ordered := append([]*hit(nil), hits...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		for k, s := range specs {
			if cmp := compareSortValues(vals[a][k], vals[b][k], s); cmp != 0 {
				return cmp < 0
			}
		}
		if a.ix != b.ix {
			return a.ix.Name < b.ix.Name
		}
		if a.doc.SeqNo != b.doc.SeqNo {
			return a.doc.SeqNo < b.doc.SeqNo
		}
		for k := 0; k < len(a.doc.nested) && k < len(b.doc.nested); k++ {
			if a.doc.nested[k].offset != b.doc.nested[k].offset {
				return a.doc.nested[k].offset < b.doc.nested[k].offset
			}
		}
		return len(a.doc.nested) < len(b.doc.nested)
	})
	return ordered, vals, outs
}

// referenceCompareTuples is compareTuples before sort keys were typed.
func referenceCompareTuples(vals, after []any, specs []sortSpec) int {
	for i := range specs {
		if after[i] == nil && vals[i] == nil {
			continue
		}
		if cmp := compareSortValues(vals[i], after[i], specs[i]); cmp != 0 {
			return cmp
		}
	}
	return 0
}

const sortTestMapping = `{"mappings": {"properties": {
	"l": {"type": "long"},
	"d": {"type": "double"},
	"t": {"type": "date"},
	"tf": {"type": "date", "format": "yyyy/MM/dd"},
	"k": {"type": "keyword", "ignore_above": 5},
	"b": {"type": "boolean"},
	"n": {"type": "integer", "null_value": 7},
	"txt": {"type": "text", "fielddata": true},
	"obj": {"properties": {"x": {"type": "long"}}},
	"nest": {"type": "nested", "properties": {"v": {"type": "long"}, "tag": {"type": "keyword"}}}}}}`

// randomSortDoc makes a document whose fields are absent, null, single or
// multi-valued (with null elements), with few distinct values so sorts tie.
func randomSortDoc(rng *rand.Rand) M {
	doc := M{}
	field := func(name string, gen func() any) {
		switch rng.Intn(6) {
		case 0:
		case 1:
			doc[name] = nil
		case 2:
			doc[name] = []any{gen(), gen()}
		case 3:
			doc[name] = []any{gen(), nil, gen()}
		default:
			doc[name] = gen()
		}
	}
	pick := func(vs ...any) func() any { return func() any { return vs[rng.Intn(len(vs))] } }
	field("l", func() any { return rng.Intn(5) - 2 })
	field("d", func() any { return float64(rng.Intn(7)) / 2 })
	field("t", pick("2024-01-01T00:00:00Z", "2024-01-02T03:00:00Z", 1704067200000, 1704070800000))
	field("tf", pick("2024/01/01", "2024/01/03", "2024/01/02"))
	field("k", pick("a", "b", "ab", "zzzzzz"))
	field("b", pick(true, false, "true"))
	field("n", func() any { return rng.Intn(3) })
	field("txt", pick("red fox", "blue", "red blue fox"))
	switch rng.Intn(3) {
	case 1:
		doc["obj"] = M{"x": rng.Intn(3)}
	case 2:
		doc["obj"] = []any{M{"x": rng.Intn(3)}, M{"x": rng.Intn(3)}}
	}
	if rng.Intn(3) > 0 {
		doc["nest"] = []any{M{"v": rng.Intn(4), "tag": "x"}, M{"v": rng.Intn(4), "tag": "y"}}
	}
	return doc
}

func randomSortSpecs(rng *rand.Rand) []sortSpec {
	fields := []string{"l", "d", "t", "tf", "k", "b", "n", "txt", "obj.x", "nest.v", "nest.tag", "_score", "_doc", "_id", "_index", "unmapped"}
	var specs []sortSpec
	for i := rng.Intn(3); i >= 0; i-- {
		s := sortSpec{field: fields[rng.Intn(len(fields))], desc: rng.Intn(2) == 0, missing: "_last"}
		switch rng.Intn(4) {
		case 0:
			s.missing = "_first"
		case 1:
			s.missing = []any{json.Number("1"), "zz", "2024-01-02", "true"}[rng.Intn(4)]
		}
		if rng.Intn(3) == 0 {
			s.mode = []string{"min", "max", "sum", "avg", "median"}[rng.Intn(5)]
		}
		if s.field == "unmapped" {
			s.unmappedType = []string{"long", "keyword", "date"}[rng.Intn(3)]
		}
		if strings.HasPrefix(s.field, "nest.") && rng.Intn(3) > 0 {
			s.nested = &nestedSort{path: "nest", matched: map[*Index]map[string]bool{}}
			if rng.Intn(2) == 0 {
				s.nested.filter = M{"term": M{"nest.tag": "x"}}
			}
		}
		if (s.field == "t" || s.field == "tf") && rng.Intn(3) == 0 {
			s.format = ParseDateFormat("yyyy-MM-dd")
		}
		specs = append(specs, s)
	}
	return specs
}

func randomSortQuery(rng *rand.Rand) any {
	switch rng.Intn(3) {
	case 0:
		return nil
	case 1:
		return M{"match": M{"txt": "red fox"}}
	}
	return M{"bool": M{"should": []any{M{"term": M{"k": "a"}}, M{"range": M{"n": M{"gte": 1}}}}}}
}

// TestOrderHitsMatchesReferenceSort checks that orderHits returns the hits and
// sort values of the stable sort it replaced, for whole orders, first pages
// and search_after, over two indices where one field changes type.
func TestOrderHitsMatchesReferenceSort(t *testing.T) {
	c := New()
	defer c.Close()
	rng := rand.New(rand.NewSource(7))
	for _, name := range []string{"a", "b"} {
		mapping := sortTestMapping
		if name == "b" {
			mapping = strings.Replace(mapping, `"l": {"type": "long"}`, `"l": {"type": "keyword"}`, 1)
		}
		if _, err := c.CreateIndex(name, parseM(t, mapping)); err != nil {
			t.Fatal(err)
		}
		var bulk strings.Builder
		for i := 0; i < 120; i++ {
			src, _ := json.Marshal(randomSortDoc(rng))
			fmt.Fprintf(&bulk, "{\"index\":{\"_id\":\"%d\"}}\n%s\n", i, src)
		}
		res, err := c.Bulk(name, []byte(bulk.String()), Params{})
		if err != nil || res.Body.(M)["errors"] == true {
			t.Fatalf("bulk: %v %v", err, res.Body)
		}
		// rewrite some documents so sequence numbers do not follow ids
		for i := 0; i < 20; i++ {
			src, _ := json.Marshal(randomSortDoc(rng))
			if _, err := c.IndexDoc(name, fmt.Sprint(rng.Intn(120)), src, DocParams{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	ts, err := c.resolve("a,b", resolveOpts(Params{}))
	if err != nil {
		t.Fatal(err)
	}
	for q := 0; q < 300; q++ {
		query, specs := randomSortQuery(rng), randomSortSpecs(rng)
		hits, err := c.executeTargets(ts, query, false, false)
		if err != nil {
			t.Fatal(err)
		}
		want, vals, outs := c.referenceSort(t, hits, specs)
		sr := &searchRequest{sort: specs}
		check := func(what string, got, want []*hit) {
			t.Helper()
			if len(got) != len(want) {
				t.Fatalf("query %d, %s: %d hits, want %d (sort %+v)", q, what, len(got), len(want), specs)
			}
			for i := range want {
				if got[i] != want[i] || !reflect.DeepEqual(got[i].sortOut, outs[want[i]]) {
					var unordered []int
					for j := 0; j+1 < len(got); j++ {
						if compareHits(got[j], got[j+1], specs) > 0 {
							unordered = append(unordered, j)
						}
					}
					t.Fatalf("query %d, %s: hit %d is %s/%s %v, want %s/%s %v (sort %+v)\ngot keys %+v reference %v\nwant keys %+v reference %v\nout of order after: %v", q, what, i,
						got[i].ix.Name, got[i].doc.ID, got[i].sortOut, want[i].ix.Name, want[i].doc.ID, outs[want[i]], specs,
						got[i].keys, vals[got[i]], want[i].keys, vals[want[i]], unordered)
				}
			}
		}
		for _, limit := range []int{-1, 0, 1, 3, rng.Intn(len(hits) + 2), len(hits)} {
			got, err := c.orderHits(hits, sr, limit, nil)
			if err != nil {
				t.Fatal(err)
			}
			w := want
			if limit >= 0 && limit < len(w) {
				w = w[:limit]
			}
			check(fmt.Sprintf("limit %d", limit), got, w)
		}
		if len(want) == 0 {
			continue
		}
		after, err := c.normalizeSearchAfter(outs[want[rng.Intn(len(want))]], specs, ts)
		if err != nil {
			continue
		}
		var rest []*hit
		for _, h := range want {
			if referenceCompareTuples(vals[h], after, specs) > 0 {
				rest = append(rest, h)
			}
		}
		limit := rng.Intn(5) + 1
		if len(rest) > limit {
			rest = rest[:limit]
		}
		got, err := c.orderHits(hits, sr, limit, func(h *hit) bool { return compareTuples(h.keys, after, specs) > 0 })
		if err != nil {
			t.Fatal(err)
		}
		check("search_after", got, rest)
	}
}

// TestSearchAfterLeavesAggregationsWhole checks that aggregations count every
// matching document when search_after skips hits (filtering the hits used to
// overwrite the list the aggregations read).
func TestSearchAfterLeavesAggregationsWhole(t *testing.T) {
	c := segmentsCluster(t, `{"mappings": {"properties": {"n": {"type": "integer"}, "tag": {"type": "keyword"}}}}`)
	for i := 0; i < 6; i++ {
		indexSource(t, c, fmt.Sprint(i), fmt.Sprintf(`{"n": %d, "tag": "t%d"}`, i, i))
	}
	body := parseM(t, `{"size": 2, "sort": [{"n": "asc"}], "search_after": [3], "aggs": {"tags": {"terms": {"field": "tag"}}}}`)
	res, err := c.Search("docs", body, Params{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, h := range res.Body.(M)["hits"].(M)["hits"].([]any) {
		ids = append(ids, h.(M)["_id"].(string))
	}
	if strings.Join(ids, ",") != "4,5" {
		t.Errorf("hits %v, want 4,5", ids)
	}
	buckets := res.Body.(M)["aggregations"].(M)["tags"].(M)["buckets"].([]any)
	if len(buckets) != 6 {
		t.Fatalf("%d buckets, want 6: %v", len(buckets), buckets)
	}
	for _, b := range buckets {
		if count := fmt.Sprint(b.(M)["doc_count"]); count != "1" {
			t.Errorf("bucket %v counted %s documents, want 1", b.(M)["key"], count)
		}
	}
}
