package engine

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/blevesearch/bleve/v2/search/query"
)

// hlTarget is one field to highlight in a hit.
type hlTarget struct {
	name   string // response key: the requested or expanded name
	full   string // concrete field name (alias target), matched against query fields
	source string // path of the values in the source
	field  *Field
	opts   hlFieldOptions
}

// hlValidType reports whether a highlighter type exists.
func hlValidType(t string) bool {
	switch t {
	case "unified", "plain", "fvh":
		return true
	}
	return false
}

// hlResolveField resolves a field name through aliases; objects are not
// highlightable fields.
func hlResolveField(m *Mapping, name string) (f *Field, full, source string) {
	parts := strings.Split(name, ".")
	fields := m.Properties
	for i, p := range parts {
		nf, ok := fields[p]
		if !ok {
			break
		}
		if nf.Type == TypeAlias && nf.Path != "" {
			target := nf.Path
			if rest := strings.Join(parts[i+1:], "."); rest != "" {
				target += "." + rest
			}
			name = target
			break
		}
		fields = nf.Properties
	}
	f, source, ok := m.resolve(name)
	if !ok || f.Type == TypeObject || f.Type == TypeNested || f.Type == TypeAlias {
		return nil, "", ""
	}
	return f, name, source
}

// hlTermVectorsWithOffsets reports whether a field stores term vectors with
// positions and offsets (required by the fast vector highlighter).
func hlTermVectorsWithOffsets(f *Field) bool {
	tv, _ := f.Extra["term_vector"].(string)
	return tv == "with_positions_offsets" || tv == "with_positions_offsets_payloads"
}

// highlightTargets lists the fields a highlight spec highlights in an
// index, as HighlightPhase does: wildcard patterns only expand to text and
// keyword fields (and, for fvh, to fields with term vectors); a field named
// twice keeps its first position and the last options.
func highlightTargets(ix *Index, spec *highlightSpec) ([]hlTarget, error) {
	var out []hlTarget
	seen := map[string]int{}
	for i := range spec.fields {
		fs := &spec.fields[i]
		opts := spec.options(&fs.opts)
		if !hlValidType(opts.highlighterType) {
			return nil, errSearchPhase(errIllegalArgument("unknown highlighter type [%s] for the field [%s]", opts.highlighterType, fs.name))
		}
		wildcard := strings.Contains(fs.name, "*")
		names := []string{fs.name}
		if wildcard {
			names = nil
			for _, name := range ix.Mapping.leafFields(fs.name) {
				if wildcardMatch(fs.name, name) {
					names = append(names, name)
				}
			}
		}
		for _, name := range names {
			f, full, source := hlResolveField(ix.Mapping, name)
			if f == nil {
				continue
			}
			if wildcard {
				if f.Type != TypeText && f.Type != TypeMatchOnlyText && f.Type != TypeKeyword {
					continue
				}
				if opts.highlighterType == "fvh" && !hlTermVectorsWithOffsets(f) {
					continue
				}
			}
			t := hlTarget{name: name, full: full, source: source, field: f, opts: opts}
			if j, ok := seen[name]; ok {
				out[j] = t
				continue
			}
			seen[name] = len(out)
			out = append(out, t)
		}
	}
	return out, nil
}

// hlValueString renders a source value the way the field's value fetcher
// hands it to the highlighter.
func hlValueString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

// hlFieldValues loads the values of a field from a hit's source: keyword
// values longer than ignore_above are skipped and normalized values are
// highlighted.
func hlFieldValues(h *hit, t *hlTarget) []string {
	f := t.field
	var out []string
	for _, v := range flattenValues(lookupPath(h.doc.Src, t.source)) {
		s, ok := hlValueString(v)
		if !ok {
			continue
		}
		if f.isKeywordLike() {
			if (f.IgnoreAbove > 0 || f.ignoreAboveSet) && len(utf16.Encode([]rune(s))) > f.IgnoreAbove {
				continue
			}
			if f.Normalizer != "" {
				if an, err := h.ix.analysis.normalizerNamed(f.Normalizer); err == nil {
					if ts := tokens(an, s); len(ts) == 1 {
						s = ts[0]
					}
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// highlightHit renders the highlight object of a hit.
func (c *Cluster) highlightHit(h *hit, sr *searchRequest) (M, error) {
	spec, err := parseHighlight(M{"highlight": sr.highlight}, "highlight")
	if err != nil {
		return nil, err
	}
	targets, err := highlightTargets(h.ix, spec)
	if err != nil {
		return nil, err
	}
	out := M{}
	extracted := map[string]*hlQueryTerms{}
	unified := map[string]*hlTarget{} // the unified highlighter is built once per concrete field
	for i := range targets {
		t := &targets[i]
		if t.opts.highlighterType == "unified" {
			if first, ok := unified[t.full]; ok {
				name := t.name
				t = &hlTarget{name: name, full: first.full, source: first.source, field: first.field, opts: first.opts}
			} else {
				unified[t.full] = t
			}
		}
		q := t.opts.highlightQuery
		if q == nil {
			q = sr.query
		}
		key := fmt.Sprintf("%p", q)
		if m, ok := q.(M); ok {
			key = fmt.Sprintf("%p", m)
		}
		qt, ok := extracted[key]
		if !ok {
			qt = c.hlExtractQuery(h.ix, q)
			extracted[key] = qt
		}
		var frags []string
		switch t.opts.highlighterType {
		case "unified":
			frags, err = unifiedHighlight(h, t, qt)
		case "plain":
			frags, err = plainHighlight(h, t, qt)
		case "fvh":
			frags, err = fvhHighlight(h, t, qt)
		}
		if err != nil {
			return nil, err
		}
		if len(frags) > 0 {
			list := make([]any, len(frags))
			for j, s := range frags {
				list[j] = s
			}
			out[t.name] = list
		}
	}
	return out, nil
}

// checkHighlightQueries builds the highlight queries of a request against
// every target index, so a malformed highlight_query fails the search even
// when nothing matches.
func (c *Cluster) checkHighlightQueries(ts []target, hm M) error {
	if hm == nil {
		return nil
	}
	spec, err := parseHighlight(M{"highlight": hm}, "highlight")
	if err != nil {
		return err
	}
	check := func(q any, field string) error {
		for _, t := range ts {
			qb := &queryBuilder{c: c, ix: t.ix}
			_, err := qb.build(q)
			if err == nil {
				continue
			}
			e, ok := err.(*Error)
			if !ok {
				return err
			}
			switch e.Type {
			case "parsing_exception", "x_content_parse_exception":
				if field == "" {
					return hlFailedToParse("highlight", hm, "highlight_query", e)
				}
				fields, _ := hm["fields"].(M)
				fieldBody, _ := fields[field].(M)
				return hlFailedToParse("highlight", hm, "fields", hlFailedToParse("fields", fields, field, hlFailedToParse("highlight_field", fieldBody, "highlight_query", e)))
			case "search_phase_execution_exception", "unsupported_operation_exception":
				return e
			}
			e.Index = t.ix.Name
			return errSearchPhase(e)
		}
		return nil
	}
	if spec.global.highlightQuery != nil {
		if err := check(spec.global.highlightQuery, ""); err != nil {
			return err
		}
	}
	for _, f := range spec.fields {
		if f.opts.highlightQuery != nil {
			if err := check(f.opts.highlightQuery, f.name); err != nil {
				return err
			}
		}
	}
	return nil
}

// collapseGroupQuery is the query the inner hits of a collapse group are
// fetched (and highlighted) with: the collapse value as a filter and the
// original query.
func collapseGroupQuery(sr *searchRequest, h *hit) any {
	boolQ := M{}
	if vals := h.ix.fieldValues(h.doc, sr.collapse); len(vals) == 1 {
		v := vals[0]
		if s, ok := v.(string); !ok {
			v = fmt.Sprint(v)
		} else {
			v = s
		}
		boolQ["filter"] = []any{M{"match": M{sr.collapse: v}}}
	} else {
		boolQ["must_not"] = []any{M{"exists": M{"field": sr.collapse}}}
	}
	if sr.query != nil {
		boolQ["must"] = []any{sr.query}
	}
	return M{"bool": boolQ}
}

// query term extraction -----------------------------------------------------

// hlQueryTerms are the parts of a query the highlighters look for: plain
// terms, multi-term queries matched against the terms of the text, and
// position sensitive span queries (phrases).
type hlQueryTerms struct {
	terms    []hlTerm
	automata []hlAutomaton
	spans    []hlSpanNear
	seq      int    // query order of the parts, across the three lists
	ix       *Index // index the multi-term queries expand against
}

type hlTerm struct {
	field, text string
	boost       float64
	seq         int
}

type hlAutomaton struct {
	field, label string
	match        func(term string) bool
	boost        float64
	seq          int
	fuzzy        []rune             // target of a fuzzy query (its matches are weighted by similarity)
	weights      map[string]float64 // per-term weights of an expanded query (fuzzy similarity)
}

// hlSpanClause matches the terms at one phrase position (or a prefix).
type hlSpanClause struct {
	terms  []string
	prefix *string
}

func (sc *hlSpanClause) matches(term string) bool {
	if sc.prefix != nil {
		return strings.HasPrefix(term, *sc.prefix)
	}
	for _, t := range sc.terms {
		if t == term {
			return true
		}
	}
	return false
}

type hlSpanNear struct {
	field   string
	clauses []hlSpanClause
	slop    int
	inOrder bool
	boost   float64
	seq     int
	multi   bool // a multi phrase query (not a plain phrase)
}

// hlExtractQuery walks a query: compound queries are followed here
// (must_not clauses and the negative query of boosting are skipped, the
// query of a nested query is followed like OpenSearch does), leaves are
// built by the query builder and their terms are collected.
func (c *Cluster) hlExtractQuery(ix *Index, q any) *hlQueryTerms {
	x := &hlQueryTerms{ix: ix}
	if q != nil {
		x.walkJSON(&queryBuilder{c: c, ix: ix}, q, 1)
	}
	return x
}

func (x *hlQueryTerms) walkJSON(qb *queryBuilder, q any, boost float64) {
	m, ok := q.(M)
	if !ok || len(m) != 1 {
		return
	}
	for kind, body := range m {
		bm, _ := body.(M)
		b := boost * getFloat(bm, "boost", 1)
		switch kind {
		case "bool":
			for _, key := range []string{"must", "should", "filter"} {
				for _, sub := range getList(bm[key]) {
					x.walkJSON(qb, sub, b)
				}
			}
		case "constant_score":
			x.walkJSON(qb, bm["filter"], b)
		case "dis_max":
			for _, sub := range getList(bm["queries"]) {
				x.walkJSON(qb, sub, b)
			}
		case "boosting":
			x.walkJSON(qb, bm["positive"], b)
		case "function_score", "script_score":
			if inner, ok := bm["query"]; ok {
				x.walkJSON(qb, inner, b)
			}
		case "nested":
			child := &queryBuilder{c: qb.c, ix: qb.ix, depth: len(qb.ix.Mapping.nestedChain(getString(bm, "path")))}
			x.walkJSON(child, bm["query"], b)
		case "wrapper":
			raw, err := base64.StdEncoding.DecodeString(getString(bm, "query"))
			if err != nil {
				return
			}
			if inner, err := decodeObject(raw); err == nil {
				x.walkJSON(qb, inner, boost)
			}
		case "match_all", "match_none", "exists", "ids":
		default:
			built, err := qb.build(q)
			if err != nil {
				return
			}
			x.walkBleve(built, boost)
		}
	}
}

func (x *hlQueryTerms) nextSeq() int {
	x.seq++
	return x.seq
}

func hlBoost(q query.Query, boost float64) float64 {
	if bq, ok := q.(query.BoostableQuery); ok {
		return boost * bq.Boost()
	}
	return boost
}

func (x *hlQueryTerms) walkBleve(q query.Query, boost float64) {
	switch t := q.(type) {
	case *luceneBoolQuery:
		// clause order of Lucene's BooleanQuery: must, should, filter (must_not skipped)
		for _, part := range [][]query.Query{t.must, t.should, t.filter} {
			for _, sub := range part {
				x.walkBleve(sub, boost)
			}
		}
	case *disMaxQuery:
		for _, sub := range t.queries {
			x.walkBleve(sub, boost)
		}
	case *boostingQuery:
		x.walkBleve(t.positive, boost)
	case *boostQuery:
		x.walkBleve(t.inner, boost*t.boost)
	case *isolatedQuery:
		x.walkBleve(t.inner, boost)
	case *docFuncQuery:
		x.walkBleve(t.inner, boost)
	case *termsUnionQuery:
		x.walkTermsUnion(t, boost)
	case *phraseQuery:
		x.walkPhrase(t, boost)
	case *query.BooleanQuery:
		if t == nil {
			return
		}
		b := hlBoost(t, boost)
		for _, part := range []query.Query{t.Must, t.Should, t.Filter} {
			switch p := part.(type) {
			case *query.ConjunctionQuery:
				if p != nil {
					for _, sub := range p.Conjuncts {
						x.walkBleve(sub, b)
					}
				}
			case nil:
			default:
				x.walkBleve(part, b)
			}
		}
	case *query.ConjunctionQuery:
		b := hlBoost(t, boost)
		if len(t.Conjuncts) == 2 {
			mp, ok1 := t.Conjuncts[0].(*query.MultiPhraseQuery)
			pq, ok2 := t.Conjuncts[1].(*query.PrefixQuery)
			if ok1 && ok2 && mp.Field() == pq.Field() {
				sp := hlSpanNear{field: mp.Field(), inOrder: true, boost: b, multi: true}
				for _, ts := range mp.Terms {
					sp.clauses = append(sp.clauses, hlSpanClause{terms: ts})
				}
				prefix := pq.Prefix
				sp.clauses = append(sp.clauses, hlSpanClause{prefix: &prefix})
				sp.seq = x.nextSeq()
				x.spans = append(x.spans, sp)
				return
			}
		}
		for _, sub := range t.Conjuncts {
			x.walkBleve(sub, b)
		}
	case *query.DisjunctionQuery:
		b := hlBoost(t, boost)
		for _, sub := range t.Disjuncts {
			x.walkBleve(sub, b)
		}
	case *constantScoreQuery:
		if t.score > 0 {
			boost *= t.score
		}
		x.walkBleve(t.inner, boost)
	case *query.TermQuery:
		x.terms = append(x.terms, hlTerm{field: t.Field(), text: t.Term, boost: hlBoost(t, boost), seq: x.nextSeq()})
	case *query.MultiPhraseQuery:
		sp := hlSpanNear{field: t.Field(), inOrder: true, boost: hlBoost(t, boost)}
		for _, ts := range t.Terms {
			sp.clauses = append(sp.clauses, hlSpanClause{terms: ts})
			sp.multi = sp.multi || len(ts) > 1
		}
		sp.seq = x.nextSeq()
		x.spans = append(x.spans, sp)
	case *query.PrefixQuery:
		prefix := t.Prefix
		x.automata = append(x.automata, hlAutomaton{seq: x.nextSeq(), field: t.Field(), label: t.Field() + ":" + prefix + "*", boost: hlBoost(t, boost),
			match: func(term string) bool { return strings.HasPrefix(term, prefix) }})
	case *query.WildcardQuery:
		pattern := []rune(t.Wildcard)
		x.automata = append(x.automata, hlAutomaton{seq: x.nextSeq(), field: t.Field(), label: t.Field() + ":" + t.Wildcard, boost: hlBoost(t, boost),
			match: func(term string) bool { return wildcardRunesMatch(pattern, []rune(term)) }})
	case *query.RegexpQuery:
		re, err := regexp.Compile("^(?:" + t.Regexp + ")$")
		if err != nil {
			return
		}
		x.automata = append(x.automata, hlAutomaton{seq: x.nextSeq(), field: t.Field(), label: t.Field() + ":/" + t.Regexp + "/", boost: hlBoost(t, boost),
			match: re.MatchString})
	case *query.FuzzyQuery:
		target, prefix, edits := []rune(t.Term), t.Prefix, t.Fuzziness
		x.automata = append(x.automata, hlAutomaton{seq: x.nextSeq(), fuzzy: target, field: t.Field(), label: t.Field() + ":" + t.Term + "~" + strconv.Itoa(edits), boost: hlBoost(t, boost),
			match: func(term string) bool { return fuzzyMatch(target, []rune(term), prefix, edits) }})
	case *query.TermRangeQuery:
		lo, hi := t.Min, t.Max
		incLo := t.InclusiveMin == nil || *t.InclusiveMin
		incHi := t.InclusiveMax == nil || *t.InclusiveMax
		label := t.Field() + ":"
		if incLo {
			label += "["
		} else {
			label += "{"
		}
		label += hlRangeBound(lo) + " TO " + hlRangeBound(hi)
		if incHi {
			label += "]"
		} else {
			label += "}"
		}
		x.automata = append(x.automata, hlAutomaton{seq: x.nextSeq(), field: t.Field(), label: label, boost: hlBoost(t, boost),
			match: func(term string) bool {
				if lo != "" && (term < lo || !incLo && term == lo) {
					return false
				}
				if hi != "" && (term > hi || !incHi && term == hi) {
					return false
				}
				return true
			}})
	}
}

// walkTermsUnion collects a term union: its terms, or, when the terms come
// from the index dictionary (prefix, wildcard, regexp, fuzzy and their
// query_string forms), one multi-term part matching the expanded terms.
func (x *hlQueryTerms) walkTermsUnion(t *termsUnionQuery, boost float64) {
	if t.boost > 0 {
		boost *= t.boost
	}
	if t.expand == nil {
		for i, term := range t.terms {
			w := boost
			if i < len(t.weights) {
				w *= t.weights[i]
			}
			x.terms = append(x.terms, hlTerm{field: t.field, text: term, boost: w, seq: x.nextSeq()})
		}
		return
	}
	terms, weights := hlExpandTerms(x.ix, t)
	set := make(map[string]bool, len(terms))
	var ws map[string]float64
	if weights != nil {
		ws = make(map[string]float64, len(terms))
	}
	for i, term := range terms {
		set[term] = true
		if ws != nil && i < len(weights) {
			ws[term] = weights[i]
		}
	}
	seq := x.nextSeq()
	x.automata = append(x.automata, hlAutomaton{seq: seq, field: t.field, label: t.field + ":#" + strconv.Itoa(seq), boost: boost, weights: ws,
		match: func(term string) bool { return set[term] }})
}

// hlExpandTerms runs the dictionary expansion of a term union.
func hlExpandTerms(ix *Index, t *termsUnionQuery) ([]string, []float64) {
	if ix == nil || ix.bleve == nil {
		return nil, nil
	}
	adv, err := ix.bleve.Advanced()
	if err != nil {
		return nil, nil
	}
	r, err := adv.Reader()
	if err != nil {
		return nil, nil
	}
	defer r.Close()
	terms, weights, err := t.expand(r)
	if err != nil {
		return nil, nil
	}
	return terms, weights
}

// walkPhrase collects a (multi) phrase as a span near query; gaps between
// the phrase positions widen the slop as in Lucene's span conversion.
func (x *hlQueryTerms) walkPhrase(t *phraseQuery, boost float64) {
	n := len(t.slots)
	if n == 0 {
		return
	}
	gaps := 0
	if n >= 2 {
		gaps = max(0, t.slots[n-1].offset-t.slots[0].offset-n+1)
	}
	sp := hlSpanNear{field: t.field, slop: t.slop + gaps, inOrder: t.slop == 0, boost: boost}
	for i, slot := range t.slots {
		c := hlSpanClause{terms: slot.terms}
		if t.prefix && i == n-1 && len(slot.terms) > 0 {
			prefix := slot.terms[0]
			c = hlSpanClause{prefix: &prefix}
		}
		sp.clauses = append(sp.clauses, c)
		sp.multi = sp.multi || len(slot.terms) > 1 || c.prefix != nil
	}
	sp.seq = x.nextSeq()
	x.spans = append(x.spans, sp)
}

func hlRangeBound(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

// wildcardRunesMatch matches Lucene wildcards: * any string, ? any
// character, \ escapes.
func wildcardRunesMatch(p, s []rune) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for len(p) > 1 && p[1] == '*' {
				p = p[1:]
			}
			for i := len(s); i >= 0; i-- {
				if wildcardRunesMatch(p[1:], s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
			p, s = p[1:], s[1:]
		default:
			c := p[0]
			if c == '\\' && len(p) > 1 {
				p = p[1:]
				c = p[0]
			}
			if len(s) == 0 || s[0] != c {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}

// fuzzyMatch reports whether term is within edits of target (Damerau
// distance with adjacent transpositions) and shares its prefix.
func fuzzyMatch(target, term []rune, prefix, edits int) bool {
	if prefix > len(target) {
		prefix = len(target)
	}
	if len(term) < prefix || string(term[:prefix]) != string(target[:prefix]) {
		return false
	}
	a, b := target[prefix:], term[prefix:]
	if d := len(a) - len(b); d > edits || -d > edits {
		return false
	}
	prev2 := make([]int, len(b)+1)
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(b)] <= edits
}

// hlFieldTerms filters the extracted query parts with the field matcher:
// only parts on the highlighted field, or on any string field when
// require_field_match is false.
func (x *hlQueryTerms) forField(ix *Index, t *hlTarget) *hlQueryTerms {
	accept := func(field string) bool {
		f, full, _ := hlResolveField(ix.Mapping, field)
		if f == nil {
			return false
		}
		if t.opts.requireFieldMatch {
			return full == t.full
		}
		return f.Type == TypeText || f.Type == TypeMatchOnlyText || f.Type == TypeKeyword || f.Type == TypeSearchAsYouType
	}
	out := &hlQueryTerms{}
	for _, term := range x.terms {
		if accept(term.field) {
			out.terms = append(out.terms, term)
		}
	}
	for _, a := range x.automata {
		if accept(a.field) {
			out.automata = append(out.automata, a)
		}
	}
	for _, sp := range x.spans {
		if accept(sp.field) {
			out.spans = append(out.spans, sp)
		}
	}
	return out
}

func (x *hlQueryTerms) empty() bool {
	return len(x.terms) == 0 && len(x.automata) == 0 && len(x.spans) == 0
}

// analysis of field values ----------------------------------------------------

// hlToken is a token of the merged field values; offsets are UTF-16 offsets
// in the merged text.
type hlToken struct {
	term       string
	start, end int
	pos        int
}

// hlValuesText joins the values with the separator the highlighters use
// between values.
func hlValuesText(values []string, sep uint16) (text []uint16, starts []int) {
	for i, v := range values {
		if i > 0 {
			text = append(text, sep)
		}
		starts = append(starts, len(text))
		text = append(text, utf16.Encode([]rune(v))...)
	}
	return text, starts
}

// hlAnalyze tokenizes the values with the field's index analyzer. Values
// are separated by a position gap of 100; max_analyzer_offset stops the
// token stream at the first token starting after it.
func hlAnalyze(ix *Index, f *Field, values []string, starts []int, maxOffset *int) []hlToken {
	var tokensOut []hlToken
	base := 0
	for i, v := range values {
		var ts []hlToken
		switch f.Type {
		case TypeText, TypeMatchOnlyText, TypeSearchAsYouType:
			an, err := ix.analysis.analyzerNamed(f.Analyzer)
			if err != nil {
				return tokensOut
			}
			if f.shingles > 0 {
				// the subfields of search_as_you_type fields hold shingles
				an = &shingleAnalyzer{base: an, size: f.shingles, prefixes: f.saytPrefix}
			}
			offsets := utf16Offsets(v)
			for _, tok := range an.Analyze([]byte(v)) {
				ts = append(ts, hlToken{term: string(tok.Term), start: starts[i] + offsets[tok.Start], end: starts[i] + offsets[tok.End], pos: base + tok.Position - 1})
			}
		default:
			if v != "" {
				ts = append(ts, hlToken{term: v, start: starts[i], end: starts[i] + len(utf16.Encode([]rune(v))), pos: base})
			}
		}
		last := base
		for _, tok := range ts {
			if maxOffset != nil && tok.start > *maxOffset {
				return tokensOut
			}
			tokensOut = append(tokensOut, tok)
			last = max(last, tok.pos)
		}
		base = last + 100
	}
	return tokensOut
}

// utf16Offsets maps byte offsets of a string to UTF-16 offsets.
func utf16Offsets(s string) []int {
	out := make([]int, len(s)+1)
	u := 0
	for i, r := range s {
		for j := i; j < i+len(string(r)) && j < len(out); j++ {
			out[j] = u
		}
		if r >= 0x10000 {
			u += 2
		} else {
			u++
		}
	}
	out[len(s)] = u
	return out
}

// hlSpanMatches evaluates a span near query over the tokens
// (NearSpansOrdered or NearSpansUnordered semantics) and returns the token
// indexes taking part in the matches and the position range of every match
// (start, inclusive end).
func hlSpanMatches(sp *hlSpanNear, tokens []hlToken) (map[int]bool, [][2]int) {
	n := len(sp.clauses)
	positions := make([][]int, n)
	byPos := map[int][]int{}
	for i, tok := range tokens {
		byPos[tok.pos] = append(byPos[tok.pos], i)
	}
	for c := range sp.clauses {
		seen := map[int]bool{}
		for _, tok := range tokens {
			if !seen[tok.pos] && sp.clauses[c].matches(tok.term) {
				seen[tok.pos] = true
				positions[c] = append(positions[c], tok.pos)
			}
		}
		sort.Ints(positions[c])
		if len(positions[c]) == 0 {
			return nil, nil
		}
	}
	matched := map[int]bool{}
	var ranges [][2]int
	idx := make([]int, n)
	collect := func(start, end int) {
		ranges = append(ranges, [2]int{start, end - 1})
		for c, k := range idx {
			for _, ti := range byPos[positions[c][k]] {
				if sp.clauses[c].matches(tokens[ti].term) {
					matched[ti] = true
				}
			}
		}
	}
	if sp.inOrder {
		for idx[0] < len(positions[0]) {
			prevEnd := positions[0][idx[0]] + 1
			width := 0
			exhausted := false
			for c := 1; c < n; c++ {
				for idx[c] < len(positions[c]) && positions[c][idx[c]] < prevEnd {
					idx[c]++
				}
				if idx[c] == len(positions[c]) {
					exhausted = true
					break
				}
				width += positions[c][idx[c]] - prevEnd
				prevEnd = positions[c][idx[c]] + 1
			}
			if exhausted {
				break
			}
			if width <= sp.slop {
				collect(positions[0][idx[0]], prevEnd)
			}
			idx[0]++
		}
		return matched, ranges
	}
	// unordered: always advance the cell with the smallest position
	minCell := func() int {
		best := 0
		for c := 1; c < n; c++ {
			if positions[c][idx[c]] < positions[best][idx[best]] {
				best = c
			}
		}
		return best
	}
	maxEnd := 0
	for c := 0; c < n; c++ {
		maxEnd = max(maxEnd, positions[c][0]+1)
	}
	atMatch := func() bool {
		m := minCell()
		return maxEnd-positions[m][idx[m]]-n <= sp.slop
	}
	advance := func() bool {
		m := minCell()
		idx[m]++
		if idx[m] == len(positions[m]) {
			return false
		}
		maxEnd = max(maxEnd, positions[m][idx[m]]+1)
		return true
	}
	for !atMatch() {
		if !advance() {
			return matched, ranges
		}
	}
	collect(positions[minCell()][idx[minCell()]], maxEnd)
	for advance() {
		if atMatch() {
			collect(positions[minCell()][idx[minCell()]], maxEnd)
		}
	}
	return matched, ranges
}
