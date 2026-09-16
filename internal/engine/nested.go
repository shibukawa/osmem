package engine

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/blevesearch/bleve/v2/search/searcher"
	index "github.com/blevesearch/bleve_index_api"
)

// Nested objects are indexed the way Lucene does it: each object of a
// nested field becomes its own bleve document, and the root document keeps
// only the fields outside nested paths. A nested object at mapping path p
// with position i among the objects of p under its parent is stored under
// the id parent + "\x00" + p + "\x00" + i and carries a _nested_path field
// naming p; root documents carry a _root field so ordinary searches can
// exclude the children. A nested query runs its inner query over the
// children of one path and joins the matches back to the documents of the
// enclosing level, which gives OpenSearch's semantics: all clauses must
// match inside the same object, plain queries never see nested fields, and
// inner_hits can report the matching objects with their _nested identity.

const (
	nestedSep       = "\x00"
	fieldNestedPath = "_nested_path"
	fieldRoot       = "_root"
)

// nestedLevel is one step of a nested identity: the full mapping path of
// the nested field and the position of the object among the objects of
// that path below the parent.
type nestedLevel struct {
	path   string
	offset int
}

func nestedID(parent, path string, offset int) string {
	return parent + nestedSep + path + nestedSep + strconv.Itoa(offset)
}

// chainID returns the bleve id of the nested object identified by chain
// under the root document id.
func chainID(root string, chain []nestedLevel) string {
	id := root
	for _, l := range chain {
		id = nestedID(id, l.path, l.offset)
	}
	return id
}

// parseNestedID splits a bleve document id into the root id and the nested
// chain. Root documents return a nil chain.
func parseNestedID(id string) (string, []nestedLevel) {
	parts := strings.Split(id, nestedSep)
	if len(parts) < 3 || len(parts)%2 == 0 {
		return id, nil
	}
	chain := make([]nestedLevel, 0, len(parts)/2)
	for i := 1; i+1 < len(parts); i += 2 {
		off, err := strconv.Atoi(parts[i+1])
		if err != nil {
			return id, nil
		}
		chain = append(chain, nestedLevel{path: parts[i], offset: off})
	}
	return parts[0], chain
}

// relativeName strips the parent nested path from a nested path
// ("comments.votes" below "comments" is "votes").
func relativeName(path, parent string) string {
	if parent != "" && hasPrefixDot(path, parent) {
		return path[len(parent)+1:]
	}
	return path
}

// nestedIdentityJSON renders the _nested value of a hit.
func nestedIdentityJSON(chain []nestedLevel) M {
	var out M
	for i := len(chain) - 1; i >= 0; i-- {
		parent := ""
		if i > 0 {
			parent = chain[i-1].path
		}
		m := M{"field": relativeName(chain[i].path, parent), "offset": chain[i].offset}
		if out != nil {
			m["_nested"] = out
		}
		out = m
	}
	return out
}

// nestedObjects lists the objects of a nested field value in index order;
// arrays of arrays are flattened like lookupPath does.
func nestedObjects(v any) []M {
	switch t := v.(type) {
	case M:
		return []M{t}
	case []any:
		var out []M
		for _, e := range t {
			out = append(out, nestedObjects(e)...)
		}
		return out
	}
	return nil
}

// level returns the nested path a document belongs to ("" for root
// documents).
func (d *Doc) level() string {
	if len(d.nested) == 0 {
		return ""
	}
	return d.nested[len(d.nested)-1].path
}

// bleveID returns the id of the bleve document backing d.
func (d *Doc) bleveID() string {
	if len(d.nested) == 0 {
		return d.ID
	}
	return chainID(d.ID, d.nested)
}

// rootDoc returns the root document of d.
func (d *Doc) rootDoc() *Doc {
	if d.root != nil {
		return d.root
	}
	return d
}

// nestedDoc builds the synthetic document of one nested object. Its source
// is the object wrapped in its path so that full field names resolve
// ({"comments": {"votes": obj}}); the object itself is kept in obj.
func nestedDoc(root *Doc, chain []nestedLevel, obj M) *Doc {
	src := M{}
	cur := src
	prev := ""
	for i, l := range chain {
		parts := strings.Split(relativeName(l.path, prev), ".")
		for j, p := range parts {
			if i == len(chain)-1 && j == len(parts)-1 {
				cur[p] = obj
				break
			}
			next := M{}
			cur[p] = next
			cur = next
		}
		prev = l.path
	}
	return &Doc{ID: root.ID, Src: src, Version: root.Version, SeqNo: root.SeqNo, PrimaryTerm: root.PrimaryTerm, nested: chain, obj: obj, root: root}
}

// nestedChildren returns the objects of the nested field path directly
// below d's level as synthetic documents, in index order.
func (ix *Index) nestedChildren(d *Doc, path string) []*Doc {
	container, rel := d.Src, path
	if lvl := d.level(); lvl != "" {
		if !hasPrefixDot(path, lvl) {
			return nil
		}
		container, rel = d.obj, path[len(lvl)+1:]
	}
	objs := nestedObjects(lookupPath(container, rel))
	if len(objs) == 0 {
		return nil
	}
	root := d.rootDoc()
	out := make([]*Doc, 0, len(objs))
	for i, obj := range objs {
		chain := append(append([]nestedLevel(nil), d.nested...), nestedLevel{path: path, offset: i})
		out = append(out, nestedDoc(root, chain, obj))
	}
	return out
}

// nestedDescendants returns the objects of the nested path below d,
// crossing intermediate nested levels ("comments.votes" from a root
// document lists every vote of every comment).
func (ix *Index) nestedDescendants(d *Doc, path string) []*Doc {
	paths := ix.Mapping.nestedChain(path)
	if len(paths) <= len(d.nested) {
		return nil
	}
	docs := []*Doc{d}
	for _, p := range paths[len(d.nested):] {
		var next []*Doc
		for _, cur := range docs {
			next = append(next, ix.nestedChildren(cur, p)...)
		}
		docs = next
	}
	return docs
}

// nestedDocByChain resolves a nested identity to its synthetic document.
func (ix *Index) nestedDocByChain(root *Doc, chain []nestedLevel) *Doc {
	d := root
	for _, l := range chain {
		kids := ix.nestedChildren(d, l.path)
		if l.offset >= len(kids) {
			return nil
		}
		d = kids[l.offset]
	}
	return d
}

// nestedSource renders the _source of a nested hit: the object itself, or
// the object extracted from the root source filtered with full paths, as
// OpenSearch does.
func nestedSource(d *Doc, sf sourceFilter) M {
	return nestedSourceWithFilters(d, sf)
}

func nestedSourceWithFilters(d *Doc, filters ...sourceFilter) M {
	plain := true
	for _, sf := range filters {
		if !sf.isPlain() {
			plain = false
			break
		}
	}
	if plain {
		return d.obj
	}
	filtered, ok := applySourceFilters(d.Src, filters...)
	if !ok {
		return M{}
	}
	cur := any(filtered)
	prev := ""
	for _, l := range d.nested {
		for _, p := range strings.Split(relativeName(l.path, prev), ".") {
			m, ok := cur.(M)
			if !ok {
				return M{}
			}
			cur = m[p]
		}
		prev = l.path
	}
	if m, ok := cur.(M); ok {
		return m
	}
	return M{}
}

// nestedChain lists the nested field paths along a mapping path, outermost
// first ("comments.votes.value" -> ["comments", "comments.votes"]).
func (m *Mapping) nestedChain(path string) []string {
	parts := strings.Split(path, ".")
	fields := m.Properties
	var out []string
	for i, p := range parts {
		f, ok := fields[p]
		if !ok {
			break
		}
		if f.Type == TypeAlias && f.Path != "" {
			rest := strings.Join(parts[i+1:], ".")
			target := f.Path
			if rest != "" {
				target += "." + rest
			}
			return m.nestedChain(target)
		}
		if f.Type == TypeNested {
			out = append(out, strings.Join(parts[:i+1], "."))
		}
		fields = f.Properties
	}
	return out
}

// nestedAncestor returns the nested path a field belongs to ("" when it is
// a root field).
func (m *Mapping) nestedAncestor(path string) string {
	chain := m.nestedChain(path)
	if len(chain) == 0 {
		return ""
	}
	return chain[len(chain)-1]
}

// nestedPaths lists every nested field path of the mapping.
func (m *Mapping) nestedPaths() []string {
	var out []string
	var walk func(prefix string, fields map[string]*Field)
	walk = func(prefix string, fields map[string]*Field) {
		for name, f := range fields {
			full := prefix + name
			if f.Type == TypeNested {
				out = append(out, full)
			}
			if f.Type == TypeObject || f.Type == TypeNested {
				walk(full+".", f.Properties)
			}
		}
	}
	walk("", m.Properties)
	sort.Strings(out)
	return out
}

// query side -----------------------------------------------------------

// nestedMatch is one nested object matched by the inner query of a nested
// query.
type nestedMatch struct {
	root      string
	chain     []nestedLevel
	score     float64
	locations search.FieldTermLocationMap
}

// innerHitsSpec is a parsed inner_hits definition.
type innerHitsSpec struct {
	name string
	sr   *searchRequest
}

// innerHitsResult holds the objects matched by one nested query that asked
// for inner_hits, grouped by the document of the level the query was
// evaluated at (root documents for a top-level nested query).
type innerHitsResult struct {
	spec     *innerHitsSpec
	depth    int
	byParent map[string][]nestedMatch
	children []*innerHitsResult // inner_hits of nested queries inside this one
}

func newInnerHitsResult(spec *innerHitsSpec, depth int, matches []nestedMatch) *innerHitsResult {
	r := &innerHitsResult{spec: spec, depth: depth, byParent: map[string][]nestedMatch{}}
	for _, m := range matches {
		key := chainID(m.root, m.chain[:depth])
		r.byParent[key] = append(r.byParent[key], m)
	}
	return r
}

// rekey regroups the matches by the documents of a shallower level, used
// when the inner_hits of a nested query inside another nested query
// without inner_hits are reported at the outer level.
func (r *innerHitsResult) rekey(depth int) {
	if depth == r.depth {
		return
	}
	by := map[string][]nestedMatch{}
	for _, ms := range r.byParent {
		for _, m := range ms {
			key := chainID(m.root, m.chain[:depth])
			by[key] = append(by[key], m)
		}
	}
	r.depth = depth
	r.byParent = by
}

// parseInnerHits parses an inner_hits body. defaultName is used when the
// body has no name (the nested path; collapse requires a name).
func parseInnerHits(m M, defaultName string) (*innerHitsSpec, error) {
	sr := &searchRequest{size: 3, trackTotal: -1}
	name := defaultName
	for k, v := range m {
		switch k {
		case "name":
			name = getString(m, k)
		case "size":
			n, ok := toFloat(v)
			if !ok || n < 0 {
				return nil, errIllegalArgument("illegal from and size values, from must be >= 0 and size must be >= 0: found [%v]", v).atParser(valueTok(m, k))
			}
			sr.size = int(n)
		case "from":
			n, ok := toFloat(v)
			if !ok || n < 0 {
				return nil, errIllegalArgument("illegal from and size values, from must be >= 0 and size must be >= 0: found [%v]", v).atParser(valueTok(m, k))
			}
			sr.from = int(n)
		case "sort":
			specs, err := parseSort(v)
			if err != nil {
				return nil, err
			}
			sr.sort = specs
			sr.explicitSort = len(specs) > 0
		case "_source":
			sr.source = parseSourceParam(v)
		case "fields":
			sr.fields = getList(v)
		case "docvalue_fields":
			sr.docvalueFields = getList(v)
		case "stored_fields":
			if s := getStrings(m, k); len(s) == 1 && s[0] == "_none_" {
				sr.storedNone = true
			}
		case "highlight":
			if _, err := parseHighlight(m, k); err != nil {
				return nil, err
			}
			hm, _ := v.(M)
			sr.highlight = hm
		case "version":
			sr.version = getBool(m, k, false)
		case "seq_no_primary_term":
			sr.seqNoTerm = getBool(m, k, false)
		case "track_scores":
			sr.trackScores = getBool(m, k, false)
		case "explain", "ignore_unmapped", "script_fields", "collapse":
			// accepted and ignored
		default:
			return nil, (&Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: "[inner_hits] unknown field [" + k + "]"}).
				at(keyTok(m, k)).atParser(valueTok(m, k))
		}
	}
	if len(sr.sort) == 0 {
		sr.sort = []sortSpec{{field: "_score", desc: true}}
	}
	return &innerHitsSpec{name: name, sr: sr}, nil
}

// nestedQuery creates a nested query: the inner query runs over the objects
// of the path and the matches are joined back to the documents of the
// enclosing level with the score_mode combination of their scores (the
// boost is applied by toQuery).
func (qb *queryBuilder) nestedQuery(spec *nestedSpec) (query.Query, error) {
	path := spec.path
	f, _, ok := qb.ix.Mapping.resolve(path)
	if !ok || (f.Type != TypeNested && f.Type != TypeObject) {
		if spec.ignoreUnmapped {
			return bleve.NewMatchNoneQuery(), nil
		}
		return nil, errNestedPath("[nested] failed to find nested object under path [" + path + "]")
	}
	if f.Type != TypeNested {
		return nil, errNestedPath("[nested] nested object under path [" + path + "] is not of nested type")
	}
	var ihs *innerHitsSpec
	if spec.hasInnerHits {
		var err error
		if ihs, err = parseInnerHits(spec.innerHits, path); err != nil {
			return nil, err
		}
		// nested inner hits highlight with the nested query
		ihs.sr.query = spec.rawQuery
		if ihs.sr.seqNoTerm {
			return nil, errSearchPhase(&Error{Status: http.StatusInternalServerError, Type: "unsupported_operation_exception", Reason: "nested documents are not assigned sequence numbers", Index: qb.ix.Name})
		}
	}
	child := &queryBuilder{c: qb.c, ix: qb.ix, depth: len(qb.ix.Mapping.nestedChain(path)), noScores: qb.noScores}
	innerQ, err := child.toQuery(spec.query)
	if err != nil {
		return nil, err
	}
	if err := child.checkInnerNames(); err != nil {
		return nil, err
	}
	matches, err := qb.c.nestedSearch(qb.ix, path, innerQ, false)
	if err != nil {
		return nil, err
	}
	if ihs != nil {
		r := newInnerHitsResult(ihs, qb.depth, matches)
		r.children = child.inner
		qb.inner = append(qb.inner, r)
	} else {
		for _, r := range child.inner {
			r.rekey(qb.depth)
			qb.inner = append(qb.inner, r)
		}
	}
	if len(matches) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	scoreMode := spec.scoreMode
	scores := map[string]float64{}
	counts := map[string]int{}
	for _, m := range matches {
		key := chainID(m.root, m.chain[:qb.depth])
		n := counts[key]
		counts[key] = n + 1
		s := scores[key]
		switch scoreMode {
		case "none":
			s = 0
		case "sum", "avg":
			s += m.score
		case "max":
			if n == 0 || m.score > s {
				s = m.score
			}
		case "min":
			if n == 0 || m.score < s {
				s = m.score
			}
		}
		scores[key] = s
	}
	ids := make([]string, 0, len(scores))
	for id := range scores {
		if scoreMode == "avg" {
			scores[id] /= float64(counts[id])
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return &docScoresQuery{ids: ids, scores: scores}, nil
}

// checkInnerNames rejects two inner_hits with the same name at one level.
func (qb *queryBuilder) checkInnerNames() error {
	seen := map[string]bool{}
	for _, r := range qb.inner {
		if seen[r.spec.name] {
			return errSearchPhase(&Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "[inner_hits] already contains an entry for key [" + r.spec.name + "]", Index: qb.ix.Name})
		}
		seen[r.spec.name] = true
	}
	return nil
}

// nestedSearch runs a query over the nested objects of one path.
func (c *Cluster) nestedSearch(ix *Index, path string, inner query.Query, locations bool) ([]nestedMatch, error) {
	n, err := ix.bleve.DocCount()
	if err != nil || n == 0 {
		return nil, err
	}
	tq := bleve.NewTermQuery(path)
	tq.SetField(fieldNestedPath)
	q := bleve.NewConjunctionQuery(inner, &constantScoreQuery{inner: tq, score: 0})
	req := bleve.NewSearchRequestOptions(q, int(n), 0, false)
	req.IncludeLocations = locations
	req.Score = "default"
	res, err := ix.bleve.Search(req)
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Type: "search_phase_execution_exception", Reason: err.Error(), Index: ix.Name}
	}
	var out []nestedMatch
	for _, dm := range res.Hits {
		root, chain := parseNestedID(dm.ID)
		if chain == nil || ix.docs[root] == nil {
			continue
		}
		out = append(out, nestedMatch{root: root, chain: chain, score: dm.Score, locations: dm.Locations})
	}
	return out, nil
}

// nestedFilterMatches runs a filter over the objects of a nested path and
// returns the ids of the matching objects (used by nested sorts).
func (c *Cluster) nestedFilterMatches(ix *Index, path string, filter any) (map[string]bool, error) {
	qb := &queryBuilder{c: c, ix: ix, depth: len(ix.Mapping.nestedChain(path))}
	q, err := qb.build(filter)
	if err != nil {
		return nil, err
	}
	matches, err := c.nestedSearch(ix, path, q, false)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(matches))
	for _, m := range matches {
		out[chainID(m.root, m.chain)] = true
	}
	return out, nil
}

// docScoresQuery matches a fixed set of documents with precomputed scores
// (the parent side of a nested join).
type docScoresQuery struct {
	ids    []string
	scores map[string]float64
}

func (q *docScoresQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	s, err := searcher.NewDocIDSearcher(ctx, i, q.ids, 1, options)
	if err != nil {
		return nil, err
	}
	return &docScoresSearcher{Searcher: s, reader: i, scores: q.scores}, nil
}

type docScoresSearcher struct {
	search.Searcher
	reader index.IndexReader
	scores map[string]float64
}

func (s *docScoresSearcher) rescore(dm *search.DocumentMatch) {
	if dm == nil {
		return
	}
	if id, err := s.reader.ExternalID(dm.IndexInternalID); err == nil {
		dm.Score = s.scores[id]
	}
	dm.Expl = nil
}

func (s *docScoresSearcher) Next(ctx *search.SearchContext) (*search.DocumentMatch, error) {
	dm, err := s.Searcher.Next(ctx)
	s.rescore(dm)
	return dm, err
}

func (s *docScoresSearcher) Advance(ctx *search.SearchContext, id index.IndexInternalID) (*search.DocumentMatch, error) {
	dm, err := s.Searcher.Advance(ctx, id)
	s.rescore(dm)
	return dm, err
}

// rendering ------------------------------------------------------------

// innerHitsJSON renders the inner_hits of a hit: the nested objects matched
// by nested queries with inner_hits and the collapse groups.
func (c *Cluster) innerHitsJSON(h *hit, sr *searchRequest) (M, error) {
	out := M{}
	for _, r := range h.inner {
		list := make([]*hit, 0)
		for _, m := range r.byParent[h.doc.bleveID()] {
			root := h.ix.docs[m.root]
			if root == nil {
				continue
			}
			d := h.ix.nestedDocByChain(root, m.chain)
			if d == nil {
				continue
			}
			list = append(list, &hit{ix: h.ix, doc: d, score: m.score, locations: m.locations, inner: r.children})
		}
		hits, err := c.innerPageJSON(list, r.spec.sr, sr)
		if err != nil {
			return nil, err
		}
		out[r.spec.name] = hits
	}
	if h.group != nil {
		for _, spec := range sr.collapseInner {
			list := make([]*hit, 0, len(h.group))
			for _, g := range h.group {
				gc := *g
				gc.group = nil
				gc.fields = nil
				if sr.query == nil {
					gc.score = 0
				}
				list = append(list, &gc)
			}
			isr := *spec.sr
			isr.query = collapseGroupQuery(sr, h)
			hits, err := c.innerPageJSON(list, &isr, sr)
			if err != nil {
				return nil, err
			}
			out[spec.name] = hits
		}
	}
	return out, nil
}

// innerPageJSON sorts and pages inner hits and renders them as
// {"hits": {...}} with the inner request's options.
func (c *Cluster) innerPageJSON(list []*hit, inner, outer *searchRequest) (M, error) {
	isr := *inner
	isr.totalAsInt = outer.totalAsInt
	page, err := c.orderHits(list, &isr, isr.from+isr.size, nil)
	if err != nil {
		return nil, err
	}
	if isr.from < len(page) {
		page = page[isr.from:]
	} else {
		page = nil
	}
	hits, err := c.hitsJSON(page, &isr, len(list))
	if err != nil {
		return nil, err
	}
	return M{"hits": hits}, nil
}
