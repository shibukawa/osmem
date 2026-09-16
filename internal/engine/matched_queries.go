package engine

import (
	"github.com/blevesearch/bleve/v2"
)

// matched_queries: the names (_name) of the queries of the request (query
// and post_filter) that each hit matches, in the iteration order of the
// Java HashMap OpenSearch collects them in.

// matchedQueries computes the matched_queries of a page of root hits.
func (c *Cluster) matchedQueries(page []*hit, sr *searchRequest) map[*hit][]string {
	if sr == nil || (sr.query == nil && sr.postFilter == nil) {
		return nil
	}
	byIndex := map[*Index][]*hit{}
	var order []*Index
	for _, h := range page {
		if h.doc == nil || h.doc.nested != nil {
			continue
		}
		if _, seen := byIndex[h.ix]; !seen {
			order = append(order, h.ix)
		}
		byIndex[h.ix] = append(byIndex[h.ix], h)
	}
	out := map[*hit][]string{}
	for _, ix := range order {
		hits := byIndex[ix]
		var named []namedQuery
		for _, q := range []any{sr.query, sr.postFilter} {
			if q == nil {
				continue
			}
			qb := &queryBuilder{c: c, ix: ix}
			if _, err := qb.build(q); err != nil {
				return nil
			}
			named = append(named, qb.named...)
		}
		if len(named) == 0 {
			continue
		}
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.doc.ID)
		}
		names := javaHashMapOrderNamed(named)
		matches := map[string]map[string]bool{} // name -> matching ids
		for _, name := range names {
			q := lastNamed(named, name)
			qb := &queryBuilder{c: c, ix: ix}
			res, err := qb.evaluate(bleve.NewConjunctionQuery(q.q, bleve.NewDocIDQuery(ids)))
			if err != nil {
				return nil
			}
			set := map[string]bool{}
			for id := range res {
				set[id] = true
			}
			matches[name] = set
		}
		for _, h := range hits {
			for _, name := range names {
				if matches[name][h.doc.ID] {
					out[h] = append(out[h], name)
				}
			}
		}
	}
	return out
}

// lastNamed returns the query registered last under a name (a later
// registration replaces the value of a HashMap entry).
func lastNamed(named []namedQuery, name string) namedQuery {
	var out namedQuery
	for _, n := range named {
		if n.name == name {
			out = n
		}
	}
	return out
}

// javaHashMapOrder lists the distinct names in the iteration order of a
// java.util.HashMap they were put into in registration order.
func javaHashMapOrderNamed(named []namedQuery) []string {
	capacity := 16
	buckets := make([][]string, capacity)
	size := 0
	seen := map[string]bool{}
	spread := func(s string) int {
		h := uint32(javaStringHash(s))
		return int(h ^ (h >> 16))
	}
	for _, n := range named {
		if seen[n.name] {
			continue
		}
		seen[n.name] = true
		i := spread(n.name) & (capacity - 1)
		buckets[i] = append(buckets[i], n.name)
		size++
		if size > capacity*3/4 {
			capacity *= 2
			resized := make([][]string, capacity)
			for _, b := range buckets {
				for _, name := range b {
					j := spread(name) & (capacity - 1)
					resized[j] = append(resized[j], name)
				}
			}
			buckets = resized
		}
	}
	var out []string
	for _, b := range buckets {
		out = append(out, b...)
	}
	return out
}
