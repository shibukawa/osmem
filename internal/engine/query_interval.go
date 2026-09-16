package engine

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/blevesearch/bleve/v2/search/searcher"
	index "github.com/blevesearch/bleve_index_api"
)

// The intervals query: positional matching over Lucene positions, built on
// the same position machinery as phrase queries (position.go, query_phrase.go).
//
// Parsing (parseIntervals et al.) produces an index-agnostic intervalSpec
// tree; qb.compileInterval resolves it against a field (analyzing "match"
// text, compiling wildcard/regexp patterns) into an intervalNode tree whose
// leaves carry either a fixed term set or a dictionary expansion run once
// the IndexReader is available. Evaluation (evalIntervalNode) computes, per
// document, every non-conflicting extent [start,end] a node can match; this
// is more than Lucene's minimal-interval set but existence and filtering
// checks give the same yes/no answer either way, and osmem's indexes are
// small enough that the extra candidates cost nothing that matters.

// intervalsSpec is the parsed body of an intervals query.
type intervalsSpec struct {
	field string
	root  *intervalSpec
}

// intervalSpec is one parsed interval rule, index-agnostic.
type intervalSpec struct {
	kind string // match, prefix, wildcard, fuzzy, regexp, all_of, any_of

	matchQuery string
	hasQuery   bool

	text    string // prefix text, wildcard pattern, regexp pattern or fuzzy term
	hasText bool

	caseInsensitive bool
	analyzer        string

	fuzziness      *fuzzinessSpec
	prefixLength   int
	maxExpansions  int
	transpositions bool

	regexpFlags int

	children []*intervalSpec
	maxGaps  int
	mode     string // "", ordered, unordered, unordered_no_overlap

	filterKind string
	filterSpec *intervalSpec
}

// intervalRuleNames are the interval rule types osmem recognizes. The
// "expecting one of" list OpenSearch 3.8 itself prints when the rule name
// is unrecognized omits "fuzzy" (a real-server quirk); intervalRuleNames
// still accepts it since it works when named directly.
var intervalRuleNames = map[string]bool{
	"match": true, "prefix": true, "wildcard": true, "fuzzy": true, "regexp": true, "all_of": true, "any_of": true,
}

var intervalFilterKinds = map[string]bool{
	"containing": true, "not_containing": true, "contained_by": true, "not_contained_by": true,
	"overlapping": true, "not_overlapping": true, "before": true, "after": true,
}

// intervalPositiveFilters are the filters that require a satisfying
// counterpart to exist (the not_* filters are vacuously satisfied instead
// when the filter source does not match the document at all).
var intervalPositiveFilters = map[string]bool{"containing": true, "contained_by": true, "overlapping": true, "before": true, "after": true}

func parseIntervals(body any) (*qnode, *Error) {
	m := body.(M)
	keys := objectKeys(m)
	switch {
	case len(keys) == 0:
		return nil, pParsing("Expected [FIELD_NAME] but got [END_OBJECT]")
	case len(keys) > 1:
		return nil, pParsing("Expected [END_OBJECT] but got [FIELD_NAME]")
	}
	field := keys[0]
	fieldObj, isObj := m[field].(M)
	if !isObj {
		return nil, pParsing("Expected [START_OBJECT] but got [%s]", xTokenName(m[field]))
	}
	root, boost, name, err := parseIntervalContainer(fieldObj)
	if err != nil {
		return nil, err
	}
	return &qnode{boost: boost, name: name, spec: &intervalsSpec{field: field, root: root}}, nil
}

// parseIntervalContainer parses the shape shared by an intervals query
// field object, an all_of/any_of element and a filter's rule: exactly one
// rule keyed by its name, plus optional boost and _name.
func parseIntervalContainer(m M) (*intervalSpec, float64, string, *Error) {
	boost := 1.0
	name := ""
	var kind string
	var spec *intervalSpec
	for _, k := range queryKeys(m) {
		v := m[k]
		switch k {
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, 0, "", err
			}
			boost = f
			continue
		case "_name":
			name = xText(v)
			continue
		}
		if kind != "" {
			return nil, 0, "", pParsing("Only one interval rule can be specified, found [%s] and [%s]", kind, k)
		}
		kind = k
		ruleBody, isObj := v.(M)
		if !isObj {
			return nil, 0, "", pParsing("Expected [START_OBJECT] but got [%s]", xTokenName(v))
		}
		if !intervalRuleNames[k] {
			continue
		}
		var err *Error
		if spec, err = parseIntervalRule(k, ruleBody); err != nil {
			return nil, 0, "", err
		}
	}
	if kind == "" {
		return nil, 0, "", pParsing("Missing intervals from interval query definition")
	}
	if !intervalRuleNames[kind] {
		return nil, 0, "", pParsing("Unknown interval type [%s], expecting one of [match, any_of, all_of, prefix, wildcard, regexp]", kind)
	}
	return spec, boost, name, nil
}

func parseIntervalRule(kind string, body M) (*intervalSpec, *Error) {
	switch kind {
	case "match":
		return parseIntervalMatch(body)
	case "prefix":
		return parseIntervalStringRule(kind, "prefix", body)
	case "wildcard":
		return parseIntervalStringRule(kind, "pattern", body)
	case "regexp":
		return parseIntervalRegexpRule(body)
	case "fuzzy":
		return parseIntervalFuzzyRule(body)
	default: // all_of, any_of
		return parseIntervalCombo(kind, body)
	}
}

func parseIntervalMatch(body M) (*intervalSpec, *Error) {
	spec := &intervalSpec{kind: "match", maxGaps: -1}
	for _, k := range queryKeys(body) {
		v := body[k]
		switch k {
		case "query":
			spec.matchQuery, spec.hasQuery = xText(v), true
		case "max_gaps":
			n, err := xInt(v)
			if err != nil {
				return nil, err
			}
			spec.maxGaps = n
		case "ordered":
			b, err := xBool(v)
			if err != nil {
				return nil, err
			}
			if spec.mode == "" {
				spec.mode = intervalModeFromBool(b)
			}
		case "mode":
			mode, err := parseIntervalMode(v, "match")
			if err != nil {
				return nil, err
			}
			spec.mode = mode
		case "analyzer":
			spec.analyzer = xText(v)
		case "use_field":
			_ = xText(v) // accepted but not implemented: matching still runs against the outer field
		case "filter":
			fk, fs, err := parseIntervalFilter(v)
			if err != nil {
				return nil, err
			}
			spec.filterKind, spec.filterSpec = fk, fs
		default:
			return nil, pXContent("[match] unknown field [%s]", k)
		}
	}
	if !spec.hasQuery {
		return nil, pIllegalArgument("Required [query]")
	}
	return spec, nil
}

func intervalModeFromBool(ordered bool) string {
	if ordered {
		return "ordered"
	}
	return "unordered"
}

func parseIntervalMode(v any, kind string) (string, *Error) {
	switch strings.ToUpper(xText(v)) {
	case "ORDERED":
		return "ordered", nil
	case "UNORDERED":
		return "unordered", nil
	case "UNORDERED_NO_OVERLAP":
		return "unordered_no_overlap", nil
	}
	return "", withCause(pXContent("Failed to build [%s] after last required field arrived", kind),
		errIllegalArgument("no mode can be parsed from ordinal %s", xText(v)))
}

// parseIntervalStringRule parses the prefix and wildcard rules, which share
// a shape: one text field (named textField), analyzer, use_field,
// case_insensitive and filter.
func parseIntervalStringRule(kind, textField string, body M) (*intervalSpec, *Error) {
	spec := &intervalSpec{kind: kind}
	for _, k := range queryKeys(body) {
		v := body[k]
		switch {
		case k == textField:
			spec.text, spec.hasText = xText(v), true
		case k == "analyzer":
			spec.analyzer = xText(v)
		case k == "use_field":
			_ = xText(v)
		case k == "case_insensitive":
			b, err := xBool(v)
			if err != nil {
				return nil, err
			}
			spec.caseInsensitive = b
		case k == "filter":
			fk, fs, err := parseIntervalFilter(v)
			if err != nil {
				return nil, err
			}
			spec.filterKind, spec.filterSpec = fk, fs
		default:
			return nil, pXContent("[%s] unknown field [%s]", kind, k)
		}
	}
	if !spec.hasText {
		return nil, pIllegalArgument("Required [%s]", textField)
	}
	return spec, nil
}

func parseIntervalRegexpRule(body M) (*intervalSpec, *Error) {
	spec := &intervalSpec{kind: "regexp", regexpFlags: reFlagAll}
	for _, k := range queryKeys(body) {
		v := body[k]
		switch k {
		case "pattern":
			spec.text, spec.hasText = xText(v), true
		case "analyzer":
			spec.analyzer = xText(v)
		case "use_field":
			_ = xText(v)
		case "case_insensitive":
			b, err := xBool(v)
			if err != nil {
				return nil, err
			}
			spec.caseInsensitive = b
		case "flags":
			flags, err := parseRegexpFlags(xText(v))
			if err != nil {
				return nil, err
			}
			spec.regexpFlags = flags
		case "filter":
			fk, fs, err := parseIntervalFilter(v)
			if err != nil {
				return nil, err
			}
			spec.filterKind, spec.filterSpec = fk, fs
		default:
			return nil, pXContent("[regexp] unknown field [%s]", k)
		}
	}
	if !spec.hasText {
		return nil, pIllegalArgument("Required [pattern]")
	}
	return spec, nil
}

func parseIntervalFuzzyRule(body M) (*intervalSpec, *Error) {
	spec := &intervalSpec{kind: "fuzzy", maxExpansions: 50, transpositions: true}
	for _, k := range queryKeys(body) {
		v := body[k]
		switch k {
		case "term":
			spec.text, spec.hasText = xText(v), true
		case "analyzer":
			spec.analyzer = xText(v)
		case "use_field":
			_ = xText(v)
		case "prefix_length":
			n, err := xInt(v)
			if err != nil {
				return nil, err
			}
			spec.prefixLength = n
		case "max_expansions":
			n, err := xInt(v)
			if err != nil {
				return nil, err
			}
			spec.maxExpansions = n
		case "transpositions":
			b, err := xBool(v)
			if err != nil {
				return nil, err
			}
			spec.transpositions = b
		case "fuzziness":
			fz, err := parseFuzziness("fuzzy", v, valueTok(body, k))
			if err != nil {
				return nil, err
			}
			spec.fuzziness = fz
		case "filter":
			fk, fs, err := parseIntervalFilter(v)
			if err != nil {
				return nil, err
			}
			spec.filterKind, spec.filterSpec = fk, fs
		default:
			return nil, pXContent("[fuzzy] unknown field [%s]", k)
		}
	}
	if !spec.hasText {
		return nil, pIllegalArgument("Required [term]")
	}
	return spec, nil
}

func parseIntervalCombo(kind string, body M) (*intervalSpec, *Error) {
	spec := &intervalSpec{kind: kind, maxGaps: -1}
	hasChildren := false
	for _, k := range queryKeys(body) {
		v := body[k]
		if kind == "any_of" {
			switch k {
			case "max_gaps", "ordered", "mode":
				return nil, pXContent("[any_of] unknown field [%s]", k)
			}
		}
		switch k {
		case "intervals":
			arr, isArr := v.([]any)
			if !isArr {
				return nil, pParsing("Expected [START_ARRAY] but got [%s]", xTokenName(v))
			}
			hasChildren = true
			for _, e := range arr {
				em, isObj := e.(M)
				if !isObj {
					return nil, pParsing("Expected [START_OBJECT] but got [%s]", xTokenName(e))
				}
				cs, _, _, err := parseIntervalContainer(em)
				if err != nil {
					return nil, err
				}
				spec.children = append(spec.children, cs)
			}
		case "max_gaps":
			n, err := xInt(v)
			if err != nil {
				return nil, err
			}
			spec.maxGaps = n
		case "ordered":
			b, err := xBool(v)
			if err != nil {
				return nil, err
			}
			if spec.mode == "" {
				spec.mode = intervalModeFromBool(b)
			}
		case "mode":
			mode, err := parseIntervalMode(v, kind)
			if err != nil {
				return nil, err
			}
			spec.mode = mode
		case "filter":
			fk, fs, err := parseIntervalFilter(v)
			if err != nil {
				return nil, err
			}
			spec.filterKind, spec.filterSpec = fk, fs
		default:
			return nil, pXContent("[%s] unknown field [%s]", kind, k)
		}
	}
	if !hasChildren {
		return nil, pIllegalArgument("Required [intervals]")
	}
	return spec, nil
}

func parseIntervalFilter(v any) (string, *intervalSpec, *Error) {
	fm, ok := v.(M)
	if !ok {
		return "", nil, pParsing("Expected [START_OBJECT] but got [%s]", xTokenName(v))
	}
	keys := objectKeys(fm)
	switch {
	case len(keys) == 0:
		return "", nil, pParsing("Expected [FIELD_NAME] but got [END_OBJECT]")
	case len(keys) > 1:
		return "", nil, pParsing("Expected [END_OBJECT] but got [FIELD_NAME]")
	}
	kind := keys[0]
	ruleContainer, isObj := fm[kind].(M)
	if !isObj {
		return "", nil, pParsing("Expected [START_OBJECT] but got [%s]", xTokenName(fm[kind]))
	}
	spec, _, _, err := parseIntervalContainer(ruleContainer)
	if err != nil {
		return "", nil, err
	}
	return kind, spec, nil
}

// build -----------------------------------------------------------------

// intervalNode is a compiled, index-bound interval source.
type intervalNode struct {
	kind string // leaf, combo, or

	terms  []string
	expand func(i index.IndexReader) ([]string, error) // prefix, wildcard, fuzzy, regexp

	children  []*intervalNode
	ordered   bool
	noOverlap bool
	maxGaps   int

	filterKind string
	filterNode *intervalNode
}

func (qb *queryBuilder) intervalsToQuery(spec *intervalsSpec) (query.Query, error) {
	f, _, mapped := qb.ix.Mapping.resolve(spec.field)
	if !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	switch f.Type {
	case TypeText:
	case TypeMatchOnlyText:
		return nil, errCreateQuery("illegal_argument_exception", fmt.Sprintf("Cannot create intervals over field [%s] with no positions indexed", spec.field))
	default:
		return nil, errQueryShard("failed to create query: Can only use interval queries on text fields - not on [%s] which is of type [%s]", spec.field, f.Type)
	}
	if err := searchableError(spec.field, f); err != nil {
		return nil, err
	}
	path := qb.ix.Mapping.searchPath(spec.field)
	root, err := qb.compileInterval(spec.root, path, f)
	if err != nil {
		return nil, err
	}
	return &intervalsQuery{ix: qb.ix, field: path, f: f, root: root}, nil
}

func (qb *queryBuilder) compileInterval(spec *intervalSpec, field string, f *Field) (*intervalNode, error) {
	var node *intervalNode
	switch spec.kind {
	case "match":
		an, _, err := qb.searchAnalyzer("intervals", f, spec.analyzer, false)
		if err != nil {
			return nil, err
		}
		var groups []tokenGroup
		switch {
		case an != nil:
			groups = analyzeGroups(an, spec.matchQuery)
		case spec.matchQuery != "":
			groups = []tokenGroup{{terms: []string{spec.matchQuery}}}
		}
		switch len(groups) {
		case 0:
			node = &intervalNode{kind: "leaf"}
		case 1:
			node = &intervalNode{kind: "leaf", terms: groups[0].terms}
		default:
			children := make([]*intervalNode, len(groups))
			for i, g := range groups {
				children[i] = &intervalNode{kind: "leaf", terms: g.terms}
			}
			mode := spec.mode
			if mode == "" {
				mode = "unordered"
			}
			node = &intervalNode{kind: "combo", children: children, maxGaps: spec.maxGaps,
				ordered: mode == "ordered", noOverlap: mode == "unordered_no_overlap"}
		}
	case "prefix":
		text, ci := spec.text, spec.caseInsensitive
		node = &intervalNode{kind: "leaf", expand: func(i index.IndexReader) ([]string, error) {
			if !ci {
				return dictTerms(i, field, text)
			}
			terms, err := dictTerms(i, field, "")
			if err != nil {
				return nil, err
			}
			var out []string
			for _, t := range terms {
				if hasPrefixFold(t, text, true) {
					out = append(out, t)
				}
			}
			return out, nil
		}}
	case "wildcard":
		tokens := compileWildcard(spec.text)
		literal := wildcardLiteralPrefix(tokens)
		ci := spec.caseInsensitive
		node = &intervalNode{kind: "leaf", expand: func(i index.IndexReader) ([]string, error) {
			start := literal
			if ci {
				start = ""
			}
			terms, err := dictTerms(i, field, start)
			if err != nil {
				return nil, err
			}
			var out []string
			for _, t := range terms {
				if wildcardMatches(tokens, t, ci) {
					out = append(out, t)
				}
			}
			return out, nil
		}}
	case "regexp":
		re, msg := compileLuceneRegexp(spec.text, spec.regexpFlags, spec.caseInsensitive)
		if msg != "" {
			return nil, errCreateQuery("illegal_argument_exception", msg)
		}
		node = &intervalNode{kind: "leaf", expand: func(i index.IndexReader) ([]string, error) {
			terms, err := dictTerms(i, field, "")
			if err != nil {
				return nil, err
			}
			var out []string
			for _, t := range terms {
				if re.Matches(t) {
					out = append(out, t)
				}
			}
			return out, nil
		}}
	case "fuzzy":
		fz := spec.fuzziness
		if fz == nil {
			fz = &fuzzinessSpec{auto: true, low: 3, high: 6}
		}
		term := spec.text
		maxEdits := fz.distance(term)
		prefixLength, maxExpansions, transpositions := spec.prefixLength, spec.maxExpansions, spec.transpositions
		node = &intervalNode{kind: "leaf", expand: func(i index.IndexReader) ([]string, error) {
			terms, _, err := fuzzyCandidateTerms(i, field, term, maxEdits, prefixLength, maxExpansions, transpositions)
			return terms, err
		}}
	default: // all_of, any_of
		children := make([]*intervalNode, len(spec.children))
		for i, c := range spec.children {
			cn, err := qb.compileInterval(c, field, f)
			if err != nil {
				return nil, err
			}
			children[i] = cn
		}
		if spec.kind == "any_of" {
			node = &intervalNode{kind: "or", children: children}
		} else {
			mode := spec.mode
			if mode == "" {
				mode = "unordered"
			}
			node = &intervalNode{kind: "combo", children: children, maxGaps: spec.maxGaps,
				ordered: mode == "ordered", noOverlap: mode == "unordered_no_overlap"}
		}
	}
	if spec.filterKind == "" {
		return node, nil
	}
	if !intervalFilterKinds[spec.filterKind] {
		return nil, errQueryShard("failed to create query: Unknown filter type [%s]", spec.filterKind)
	}
	fn, err := qb.compileInterval(spec.filterSpec, field, f)
	if err != nil {
		return nil, err
	}
	node.filterKind = spec.filterKind
	node.filterNode = fn
	return node, nil
}

// execution ---------------------------------------------------------------

// intervalsQuery evaluates a compiled interval tree against the term
// positions of one field, the same position source phraseQuery uses.
type intervalsQuery struct {
	ix    *Index
	field string
	f     *Field
	root  *intervalNode
}

func collectIntervalLeaves(n *intervalNode, out *[]*intervalNode) {
	if n == nil {
		return
	}
	if n.kind == "leaf" {
		*out = append(*out, n)
	}
	for _, c := range n.children {
		collectIntervalLeaves(c, out)
	}
	collectIntervalLeaves(n.filterNode, out)
}

func (q *intervalsQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	var leaves []*intervalNode
	collectIntervalLeaves(q.root, &leaves)
	termSet := map[string]bool{}
	for _, lf := range leaves {
		if lf.expand != nil {
			terms, err := lf.expand(i)
			if err != nil {
				return nil, err
			}
			lf.terms = terms
		}
		for _, t := range lf.terms {
			termSet[t] = true
		}
	}
	if len(termSet) == 0 {
		return searcher.NewMatchNoneSearcher(i)
	}
	terms := make([]string, 0, len(termSet))
	for t := range termSet {
		terms = append(terms, t)
	}
	sort.Strings(terms)
	termIndex := make(map[string]int, len(terms))
	for idx, t := range terms {
		termIndex[t] = idx
	}
	options.IncludeTermVectors = true
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
	pm := newPositionMapper(q.ix, i, q.field, q.f)
	field, root := q.field, q.root
	return newEvalSearcher(children, func(_ *search.SearchContext, res [][]docEntry) ([]docEntry, error) {
		type docAcc struct {
			id    index.IndexInternalID
			score float64
			pos   [][]int // by term index
			ftls  []search.FieldTermLocation
		}
		byID := map[string]*docAcc{}
		var order []*docAcc
		for ti, list := range res {
			for _, e := range list {
				key := string(e.id)
				a := byID[key]
				if a == nil {
					a = &docAcc{id: e.id, pos: make([][]int, len(terms))}
					byID[key] = a
					order = append(order, a)
				}
				a.score += e.score
				a.ftls = append(a.ftls, e.ftls...)
				for li := range e.ftls {
					l := &e.ftls[li]
					if l.Field != field {
						continue
					}
					a.pos[ti] = append(a.pos[ti], pm.position(e.id, &l.Location))
				}
			}
		}
		var out []docEntry
		for _, a := range order {
			leafPos := make(map[*intervalNode][]int, len(leaves))
			for _, lf := range leaves {
				var merged []int
				for _, t := range lf.terms {
					if idx, ok := termIndex[t]; ok {
						merged = append(merged, a.pos[idx]...)
					}
				}
				sort.Ints(merged)
				leafPos[lf] = dedupeInts(merged)
			}
			if len(evalIntervalNode(root, leafPos)) == 0 {
				continue
			}
			out = append(out, docEntry{id: a.id, score: a.score, ftls: a.ftls})
		}
		sort.Slice(out, func(x, y int) bool { return bytes.Compare(out[x].id, out[y].id) < 0 })
		return out, nil
	}), nil
}

// extent is a matched span of Lucene positions, inclusive of both ends.
type extent struct{ start, end int }

func dedupeInts(sorted []int) []int {
	if len(sorted) < 2 {
		return sorted
	}
	out := sorted[:1]
	for _, v := range sorted[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func sortDedupeExtents(es []extent) []extent {
	if len(es) == 0 {
		return es
	}
	sort.Slice(es, func(a, b int) bool {
		if es[a].start != es[b].start {
			return es[a].start < es[b].start
		}
		return es[a].end < es[b].end
	})
	out := es[:1]
	for _, e := range es[1:] {
		if e != out[len(out)-1] {
			out = append(out, e)
		}
	}
	return out
}

// evalIntervalNode returns every extent a node matches in one document
// (leafPos gives the sorted, deduplicated Lucene positions of each leaf).
func evalIntervalNode(n *intervalNode, leafPos map[*intervalNode][]int) []extent {
	var base []extent
	switch n.kind {
	case "leaf":
		pos := leafPos[n]
		base = make([]extent, len(pos))
		for i, p := range pos {
			base[i] = extent{p, p}
		}
	case "or":
		for _, c := range n.children {
			base = append(base, evalIntervalNode(c, leafPos)...)
		}
		base = sortDedupeExtents(base)
	default: // combo
		childExtents := make([][]extent, len(n.children))
		for i, c := range n.children {
			childExtents[i] = evalIntervalNode(c, leafPos)
		}
		switch {
		case n.noOverlap:
			base = combineIntervalsNoOverlap(childExtents, n.maxGaps)
		case n.ordered:
			base = combineIntervalsOrdered(childExtents, n.maxGaps)
		default:
			base = combineIntervalsUnordered(childExtents, n.maxGaps)
		}
	}
	if n.filterKind == "" {
		return base
	}
	return applyIntervalFilter(n.filterKind, base, evalIntervalNode(n.filterNode, leafPos))
}

// maxIntervalStates caps the states kept while combining sub-extents, so a
// field with very high term frequency cannot blow up combination time; it
// is far above anything a realistic test document produces.
const maxIntervalStates = 4096

// combineIntervalsOrdered is Lucine's Intervals.ordered: one extent per
// child, in child order, each strictly before the next.
func combineIntervalsOrdered(children [][]extent, maxGaps int) []extent {
	return foldIntervalStates(children, maxGaps, func(env extent, e extent) (extent, bool) {
		if env.end >= e.start {
			return extent{}, false
		}
		return extent{env.start, e.end}, true
	})
}

// combineIntervalsUnordered is Lucene's Intervals.unordered: one extent per
// child in any order and possibly overlapping, spanning their envelope.
func combineIntervalsUnordered(children [][]extent, maxGaps int) []extent {
	return foldIntervalStates(children, maxGaps, func(env extent, e extent) (extent, bool) {
		return extent{min(env.start, e.start), max(env.end, e.end)}, true
	})
}

// combineIntervalsNoOverlap is Lucene's Intervals.unorderedNoOverlaps: a
// left fold over the children in the given order, where each next extent
// must fall entirely outside the envelope accumulated so far (order matters:
// the running envelope, not each child individually, is what must not
// overlap the next pick).
func combineIntervalsNoOverlap(children [][]extent, maxGaps int) []extent {
	return foldIntervalStates(children, maxGaps, func(env extent, e extent) (extent, bool) {
		if env.start <= e.end && e.start <= env.end {
			return extent{}, false
		}
		return extent{min(env.start, e.start), max(env.end, e.end)}, true
	})
}

// foldIntervalStates combines the extent lists of a combo node's children
// left to right with combine, which reports whether adding e to the
// envelope env is allowed and, if so, the new envelope. The final envelopes
// are kept when their gap count (width minus the sum of the widths of the
// combined child extents) is within maxGaps (negative: unlimited).
func foldIntervalStates(children [][]extent, maxGaps int, combine func(env extent, e extent) (extent, bool)) []extent {
	if len(children) == 0 {
		return nil
	}
	type state struct {
		env   extent
		width int
	}
	cur := make([]state, 0, len(children[0]))
	for _, e := range children[0] {
		cur = append(cur, state{env: e, width: e.end - e.start + 1})
	}
	for _, list := range children[1:] {
		if len(cur) == 0 {
			return nil
		}
		next := make([]state, 0, len(cur))
	combine:
		for _, c := range cur {
			for _, e := range list {
				env, ok := combine(c.env, e)
				if !ok {
					continue
				}
				next = append(next, state{env: env, width: c.width + e.end - e.start + 1})
				if len(next) >= maxIntervalStates {
					break combine
				}
			}
		}
		cur = next
	}
	out := make([]extent, 0, len(cur))
	for _, c := range cur {
		if maxGaps < 0 || (c.env.end-c.env.start+1)-c.width <= maxGaps {
			out = append(out, c.env)
		}
	}
	return sortDedupeExtents(out)
}

// intervalRelationHolds is the positive form of an interval filter relation
// between a base extent e and a filter-source extent f.
func intervalRelationHolds(kind string, e, f extent) bool {
	switch kind {
	case "containing", "not_containing":
		return f.start >= e.start && f.end <= e.end
	case "contained_by", "not_contained_by":
		return e.start >= f.start && e.end <= f.end
	case "overlapping", "not_overlapping":
		return e.start <= f.end && f.start <= e.end
	case "before":
		return e.end < f.start
	case "after":
		return e.start > f.end
	}
	return false
}

// applyIntervalFilter keeps the extents of base that satisfy kind against
// some extent of filterExtents (containing, contained_by, overlapping,
// before, after), or that satisfy none of them (the not_* filters, which
// are vacuously true when the filter source does not match the document).
func applyIntervalFilter(kind string, base, filterExtents []extent) []extent {
	positive := intervalPositiveFilters[kind]
	if len(filterExtents) == 0 {
		if positive {
			return nil
		}
		return base
	}
	var out []extent
	for _, e := range base {
		matched := false
		for _, f := range filterExtents {
			if intervalRelationHolds(kind, e, f) {
				matched = true
				break
			}
		}
		if matched == positive {
			out = append(out, e)
		}
	}
	return out
}
