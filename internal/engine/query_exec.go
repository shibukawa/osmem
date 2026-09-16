package engine

import (
	"bytes"
	"context"
	"sort"

	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/blevesearch/bleve/v2/search/searcher"
	index "github.com/blevesearch/bleve_index_api"
)

// Compound queries with Lucene semantics. Bleve's own compound searchers
// scale scores by query norms and coordination factors and treat
// minimum_should_match differently; these searchers combine the matches of
// their clauses the way Lucene's BooleanQuery, DisjunctionMaxQuery and
// BoostQuery do: scores of scoring clauses add up and boosts multiply.
//
// The searchers collect the matches of their clauses when first used and
// serve the combined, id-ordered result; indexes are in memory and small,
// which keeps the combination logic simple.

// docEntry is one matching document of a clause.
type docEntry struct {
	id    index.IndexInternalID
	score float64
	ftls  []search.FieldTermLocation
}

func copyLocations(src []search.FieldTermLocation) []search.FieldTermLocation {
	if len(src) == 0 {
		return nil
	}
	out := make([]search.FieldTermLocation, len(src))
	for i, l := range src {
		out[i] = l
		if len(l.Location.ArrayPositions) > 0 {
			out[i].Location.ArrayPositions = append(search.ArrayPositions(nil), l.Location.ArrayPositions...)
		}
	}
	return out
}

// drainSearcher reads every match of a searcher.
func drainSearcher(ctx *search.SearchContext, s search.Searcher) ([]docEntry, error) {
	var out []docEntry
	for {
		dm, err := s.Next(ctx)
		if err != nil {
			return nil, err
		}
		if dm == nil {
			return out, nil
		}
		out = append(out, docEntry{
			id:    append(index.IndexInternalID(nil), dm.IndexInternalID...),
			score: dm.Score,
			ftls:  copyLocations(dm.FieldTermLocations),
		})
		ctx.DocumentMatchPool.Put(dm)
	}
}

// evalSearcher serves matches computed from the complete match lists of
// its children.
type evalSearcher struct {
	children []search.Searcher
	compute  func(ctx *search.SearchContext, results [][]docEntry) ([]docEntry, error)
	entries  []docEntry
	pos      int
	ready    bool
}

func newEvalSearcher(children []search.Searcher, compute func(*search.SearchContext, [][]docEntry) ([]docEntry, error)) *evalSearcher {
	return &evalSearcher{children: children, compute: compute}
}

func (s *evalSearcher) prepare(ctx *search.SearchContext) error {
	if s.ready {
		return nil
	}
	s.ready = true
	results := make([][]docEntry, len(s.children))
	for i, c := range s.children {
		r, err := drainSearcher(ctx, c)
		if err != nil {
			return err
		}
		results[i] = r
	}
	entries, err := s.compute(ctx, results)
	if err != nil {
		return err
	}
	s.entries = entries
	return nil
}

func (s *evalSearcher) emit(ctx *search.SearchContext) *search.DocumentMatch {
	e := s.entries[s.pos]
	s.pos++
	dm := ctx.DocumentMatchPool.Get()
	dm.IndexInternalID = append(dm.IndexInternalID[:0], e.id...)
	dm.Score = e.score
	if len(e.ftls) > 0 {
		dm.FieldTermLocations = append(dm.FieldTermLocations[:0], copyLocations(e.ftls)...)
	}
	return dm
}

func (s *evalSearcher) Next(ctx *search.SearchContext) (*search.DocumentMatch, error) {
	if err := s.prepare(ctx); err != nil {
		return nil, err
	}
	if s.pos >= len(s.entries) {
		return nil, nil
	}
	return s.emit(ctx), nil
}

func (s *evalSearcher) Advance(ctx *search.SearchContext, id index.IndexInternalID) (*search.DocumentMatch, error) {
	if err := s.prepare(ctx); err != nil {
		return nil, err
	}
	rest := s.entries[s.pos:]
	i := sort.Search(len(rest), func(i int) bool { return bytes.Compare(rest[i].id, id) >= 0 })
	s.pos += i
	if s.pos >= len(s.entries) {
		return nil, nil
	}
	return s.emit(ctx), nil
}

func (s *evalSearcher) Close() error {
	var first error
	for _, c := range s.children {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Weight and SetQueryNorm keep bleve's query normalization away from the
// scores computed here.
func (s *evalSearcher) Weight() float64      { return 0 }
func (s *evalSearcher) SetQueryNorm(float64) {}
func (s *evalSearcher) Min() int             { return 0 }
func (s *evalSearcher) Size() int            { return 64 + 48*len(s.entries) }

func (s *evalSearcher) Count() uint64 {
	if s.ready {
		return uint64(len(s.entries))
	}
	var n uint64
	for _, c := range s.children {
		n += c.Count()
	}
	return n
}

func (s *evalSearcher) DocumentMatchPoolSize() int {
	n := 2
	for _, c := range s.children {
		n += c.DocumentMatchPoolSize()
	}
	return n
}

func childSearchers(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions, qs []query.Query) ([]search.Searcher, error) {
	out := make([]search.Searcher, 0, len(qs))
	for _, q := range qs {
		s, err := q.Searcher(ctx, i, m, options)
		if err != nil {
			for _, c := range out {
				_ = c.Close()
			}
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// bool -------------------------------------------------------------------

// luceneBoolQuery is Lucene's BooleanQuery: every must and filter clause
// has to match, no must_not clause may match and at least minShould should
// clauses have to match (at least one when there is no required clause).
// A query with only must_not clauses matches nothing.
type luceneBoolQuery struct {
	must, filter, should, mustNot []query.Query
	minShould                     int
}

func (q *luceneBoolQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	all := make([]query.Query, 0, len(q.must)+len(q.filter)+len(q.should)+len(q.mustNot))
	all = append(all, q.must...)
	all = append(all, q.filter...)
	all = append(all, q.should...)
	all = append(all, q.mustNot...)
	children, err := childSearchers(ctx, i, m, options, all)
	if err != nil {
		return nil, err
	}
	nm, nf, ns := len(q.must), len(q.filter), len(q.should)
	return newEvalSearcher(children, func(_ *search.SearchContext, res [][]docEntry) ([]docEntry, error) {
		return combineBool(res[:nm], res[nm:nm+nf], res[nm+nf:nm+nf+ns], res[nm+nf+ns:], q.minShould), nil
	}), nil
}

type entryIndex map[string]*docEntry

func indexEntries(list []docEntry) entryIndex {
	out := make(entryIndex, len(list))
	for i := range list {
		out[string(list[i].id)] = &list[i]
	}
	return out
}

func combineBool(must, filter, should, mustNot [][]docEntry, minShould int) []docEntry {
	required := append(append([][]docEntry{}, must...), filter...)
	shouldIdx := make([]entryIndex, len(should))
	for i, l := range should {
		shouldIdx[i] = indexEntries(l)
	}
	notIdx := make([]entryIndex, len(mustNot))
	for i, l := range mustNot {
		notIdx[i] = indexEntries(l)
	}
	needShould := minShould
	var candidates []docEntry
	if len(required) > 0 {
		base := 0
		for i, l := range required {
			if len(l) < len(required[base]) {
				base = i
			}
		}
		reqIdx := make([]entryIndex, len(required))
		for i, l := range required {
			if i != base {
				reqIdx[i] = indexEntries(l)
			}
		}
		for _, e := range required[base] {
			ok := true
			for i := range required {
				if i != base && reqIdx[i][string(e.id)] == nil {
					ok = false
					break
				}
			}
			if ok {
				candidates = append(candidates, docEntry{id: e.id})
			}
		}
	} else {
		if len(should) == 0 {
			return nil
		}
		if needShould < 1 {
			needShould = 1
		}
		seen := map[string]bool{}
		for _, l := range should {
			for _, e := range l {
				key := string(e.id)
				if !seen[key] {
					seen[key] = true
					candidates = append(candidates, docEntry{id: e.id})
				}
			}
		}
		sort.Slice(candidates, func(a, b int) bool { return bytes.Compare(candidates[a].id, candidates[b].id) < 0 })
	}
	var mustIdx []entryIndex
	for _, l := range must {
		mustIdx = append(mustIdx, indexEntries(l))
	}
	var filterIdx []entryIndex
	for _, l := range filter {
		filterIdx = append(filterIdx, indexEntries(l))
	}
	out := candidates[:0]
	for _, c := range candidates {
		key := string(c.id)
		excluded := false
		for _, ni := range notIdx {
			if ni[key] != nil {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		matched := 0
		score := 0.0
		var ftls []search.FieldTermLocation
		for _, si := range shouldIdx {
			if e := si[key]; e != nil {
				matched++
				score += e.score
				ftls = append(ftls, e.ftls...)
			}
		}
		if matched < needShould {
			continue
		}
		for _, mi := range mustIdx {
			e := mi[key]
			score += e.score
			ftls = append(ftls, e.ftls...)
		}
		for _, fi := range filterIdx {
			ftls = append(ftls, fi[key].ftls...)
		}
		c.score = score
		c.ftls = ftls
		out = append(out, c)
	}
	return out
}

// dis_max ----------------------------------------------------------------

// disMaxQuery is Lucene's DisjunctionMaxQuery: the best clause score plus
// tie_breaker times the scores of the other matching clauses.
type disMaxQuery struct {
	queries []query.Query
	tie     float64
}

func (q *disMaxQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	children, err := childSearchers(ctx, i, m, options, q.queries)
	if err != nil {
		return nil, err
	}
	return newEvalSearcher(children, func(_ *search.SearchContext, res [][]docEntry) ([]docEntry, error) {
		type acc struct {
			e        docEntry
			max, sum float64
		}
		byID := map[string]*acc{}
		var order []*acc
		for _, l := range res {
			for _, e := range l {
				key := string(e.id)
				a := byID[key]
				if a == nil {
					a = &acc{e: docEntry{id: e.id}, max: e.score}
					byID[key] = a
					order = append(order, a)
				} else if e.score > a.max {
					a.max = e.score
				}
				a.sum += e.score
				a.e.ftls = append(a.e.ftls, e.ftls...)
			}
		}
		out := make([]docEntry, 0, len(order))
		for _, a := range order {
			a.e.score = a.max + q.tie*(a.sum-a.max)
			out = append(out, a.e)
		}
		sort.Slice(out, func(a, b int) bool { return bytes.Compare(out[a].id, out[b].id) < 0 })
		return out, nil
	}), nil
}

// boosting ---------------------------------------------------------------

// boostingQuery scores the positive query and multiplies the score of
// documents also matching the negative query by negativeBoost.
type boostingQuery struct {
	positive, negative query.Query
	negativeBoost      float64
}

func (q *boostingQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	children, err := childSearchers(ctx, i, m, options, []query.Query{q.positive, q.negative})
	if err != nil {
		return nil, err
	}
	return newEvalSearcher(children, func(_ *search.SearchContext, res [][]docEntry) ([]docEntry, error) {
		neg := indexEntries(res[1])
		out := res[0]
		for i := range out {
			if neg[string(out[i].id)] != nil {
				out[i].score *= q.negativeBoost
			}
		}
		return out, nil
	}), nil
}

// boost ------------------------------------------------------------------

// boostQuery multiplies the scores of a query (Lucene's BoostQuery).
type boostQuery struct {
	inner query.Query
	boost float64
}

func newBoost(q query.Query, boost float64) query.Query {
	if boost == 1 {
		return q
	}
	switch t := q.(type) {
	case *constantScoreQuery:
		return &constantScoreQuery{inner: t.inner, score: t.score * boost}
	case *boostQuery:
		return &boostQuery{inner: t.inner, boost: t.boost * boost}
	case *query.MatchNoneQuery:
		return q
	}
	return &boostQuery{inner: q, boost: boost}
}

func (q *boostQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	s, err := q.inner.Searcher(ctx, i, m, options)
	if err != nil {
		return nil, err
	}
	return &boostSearcher{Searcher: s, boost: q.boost}, nil
}

type boostSearcher struct {
	search.Searcher
	boost float64
}

func (s *boostSearcher) Weight() float64      { return 0 }
func (s *boostSearcher) SetQueryNorm(float64) {}

func (s *boostSearcher) Next(ctx *search.SearchContext) (*search.DocumentMatch, error) {
	dm, err := s.Searcher.Next(ctx)
	if dm != nil {
		dm.Score *= s.boost
		dm.Expl = nil
	}
	return dm, err
}

func (s *boostSearcher) Advance(ctx *search.SearchContext, id index.IndexInternalID) (*search.DocumentMatch, error) {
	dm, err := s.Searcher.Advance(ctx, id)
	if dm != nil {
		dm.Score *= s.boost
		dm.Expl = nil
	}
	return dm, err
}

// isolatedQuery keeps bleve's query normalization away from a leaf query
// (its scores are the raw term scores).
type isolatedQuery struct {
	inner query.Query
}

func (q *isolatedQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	s, err := q.inner.Searcher(ctx, i, m, options)
	if err != nil {
		return nil, err
	}
	return &boostSearcher{Searcher: s, boost: 1}, nil
}

// preset scores ----------------------------------------------------------

// presetQuery matches a fixed set of documents with precomputed scores and
// term locations (the result of a query evaluated while it was built).
type presetQuery struct {
	scores map[string]float64 // external id -> score
	ftls   map[string][]search.FieldTermLocation
}

func (q *presetQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	ids := make([]string, 0, len(q.scores))
	for id := range q.scores {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	s, err := searcher.NewDocIDSearcher(ctx, i, ids, 1, options)
	if err != nil {
		return nil, err
	}
	return &presetSearcher{Searcher: s, reader: i, q: q}, nil
}

type presetSearcher struct {
	search.Searcher
	reader index.IndexReader
	q      *presetQuery
}

func (s *presetSearcher) Weight() float64      { return 0 }
func (s *presetSearcher) SetQueryNorm(float64) {}

func (s *presetSearcher) fill(dm *search.DocumentMatch) {
	if dm == nil {
		return
	}
	dm.Expl = nil
	id, err := s.reader.ExternalID(dm.IndexInternalID)
	if err != nil {
		return
	}
	dm.Score = s.q.scores[id]
	if l := s.q.ftls[id]; len(l) > 0 {
		dm.FieldTermLocations = append(dm.FieldTermLocations[:0], copyLocations(l)...)
	}
}

func (s *presetSearcher) Next(ctx *search.SearchContext) (*search.DocumentMatch, error) {
	dm, err := s.Searcher.Next(ctx)
	s.fill(dm)
	return dm, err
}

func (s *presetSearcher) Advance(ctx *search.SearchContext, id index.IndexInternalID) (*search.DocumentMatch, error) {
	dm, err := s.Searcher.Advance(ctx, id)
	s.fill(dm)
	return dm, err
}

// document predicates ----------------------------------------------------

// docFuncQuery keeps the matches of an inner query accepted by fn (called
// with the external document id and the inner score) and gives them the
// score fn returns.
type docFuncQuery struct {
	inner query.Query
	fn    func(id string, score float64) (float64, bool)
}

func (q *docFuncQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	s, err := q.inner.Searcher(ctx, i, m, options)
	if err != nil {
		return nil, err
	}
	return &docFuncSearcher{Searcher: s, reader: i, fn: q.fn}, nil
}

type docFuncSearcher struct {
	search.Searcher
	reader index.IndexReader
	fn     func(id string, score float64) (float64, bool)
}

func (s *docFuncSearcher) Weight() float64      { return 0 }
func (s *docFuncSearcher) SetQueryNorm(float64) {}

func (s *docFuncSearcher) accept(ctx *search.SearchContext, dm *search.DocumentMatch) bool {
	id, err := s.reader.ExternalID(dm.IndexInternalID)
	if err != nil {
		return false
	}
	score, ok := s.fn(id, dm.Score)
	if !ok {
		return false
	}
	dm.Score = score
	dm.Expl = nil
	return true
}

func (s *docFuncSearcher) Next(ctx *search.SearchContext) (*search.DocumentMatch, error) {
	for {
		dm, err := s.Searcher.Next(ctx)
		if err != nil || dm == nil {
			return dm, err
		}
		if s.accept(ctx, dm) {
			return dm, nil
		}
		ctx.DocumentMatchPool.Put(dm)
	}
}

func (s *docFuncSearcher) Advance(ctx *search.SearchContext, id index.IndexInternalID) (*search.DocumentMatch, error) {
	dm, err := s.Searcher.Advance(ctx, id)
	if err != nil || dm == nil {
		return dm, err
	}
	if s.accept(ctx, dm) {
		return dm, nil
	}
	ctx.DocumentMatchPool.Put(dm)
	return s.Next(ctx)
}

// term unions ------------------------------------------------------------

// termsQuery matches documents containing any of a list of terms in a
// field. Constant queries score boost (Lucene's constant score rewrite);
// otherwise each matching term adds its score times its weight.
type termsUnionQuery struct {
	field    string
	terms    []string
	weights  []float64 // nil: 1 for every term
	constant bool
	boost    float64
	// expand, when set, lists the terms from the index dictionary when the
	// searcher is created.
	expand func(i index.IndexReader) ([]string, []float64, error)
	// vectors forces term vectors (positions) on the term searchers.
	vectors bool
	// booleanSim scores every matching term by its weight (the boolean
	// similarity).
	booleanSim bool
}

func (q *termsUnionQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	terms, weights := q.terms, q.weights
	if q.expand != nil {
		var err error
		if terms, weights, err = q.expand(i); err != nil {
			return nil, err
		}
	}
	if len(terms) == 0 {
		return searcher.NewMatchNoneSearcher(i)
	}
	if q.vectors {
		options.IncludeTermVectors = true
	}
	if q.constant && !q.vectors && len(terms) > 16 {
		s, err := searcher.NewMultiTermSearcher(ctx, i, terms, q.field, 1, options, false)
		if err != nil {
			return nil, err
		}
		return &constantScoreSearcher{Searcher: s, score: q.boost}, nil
	}
	children := make([]search.Searcher, 0, len(terms))
	for _, t := range terms {
		s, err := searcher.NewTermSearcher(ctx, i, t, q.field, 1, options)
		if err != nil {
			for _, c := range children {
				_ = c.Close()
			}
			return nil, err
		}
		children = append(children, s)
	}
	return newEvalSearcher(children, func(_ *search.SearchContext, res [][]docEntry) ([]docEntry, error) {
		byID := map[string]*docEntry{}
		var order []*docEntry
		for ti, l := range res {
			w := 1.0
			if weights != nil {
				w = weights[ti]
			}
			for _, e := range l {
				key := string(e.id)
				a := byID[key]
				if a == nil {
					a = &docEntry{id: e.id}
					byID[key] = a
					order = append(order, a)
				}
				if q.booleanSim {
					a.score += w
				} else {
					a.score += e.score * w
				}
				a.ftls = append(a.ftls, e.ftls...)
			}
		}
		out := make([]docEntry, 0, len(order))
		for _, a := range order {
			if q.constant {
				a.score = q.boost
			} else {
				a.score *= q.boost
			}
			out = append(out, *a)
		}
		sort.Slice(out, func(a, b int) bool { return bytes.Compare(out[a].id, out[b].id) < 0 })
		return out, nil
	}), nil
}

// dictionary helpers -----------------------------------------------------

// dictTerms lists the terms of a field starting with prefix, in term order.
func dictTerms(i index.IndexReader, field, prefix string) ([]string, error) {
	var dict index.FieldDict
	var err error
	if prefix == "" {
		dict, err = i.FieldDict(field)
	} else {
		dict, err = i.FieldDictPrefix(field, []byte(prefix))
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = dict.Close() }()
	var out []string
	for {
		e, err := dict.Next()
		if err != nil {
			return nil, err
		}
		if e == nil {
			return out, nil
		}
		out = append(out, e.Term)
	}
}

// dictDocFreq returns the document frequency of a term in a field.
func dictDocFreq(i index.IndexReader, field, term string) (uint64, error) {
	dict, err := i.FieldDictRange(field, []byte(term), []byte(term))
	if err != nil {
		return 0, err
	}
	defer func() { _ = dict.Close() }()
	e, err := dict.Next()
	if err != nil || e == nil {
		return 0, err
	}
	if e.Term != term {
		return 0, nil
	}
	return e.Count, nil
}
