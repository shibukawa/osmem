package engine

import (
	"strings"
	"unicode"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/search/query"
)

// Full text queries (MatchQuery and MultiMatchQuery of OpenSearch).

// splitNormalizer is the search analyzer of a keyword field with
// split_queries_on_whitespace: whitespace tokens, each normalized.
type splitNormalizer struct {
	norm analysis.Analyzer
}

func (a *splitNormalizer) Analyze(input []byte) analysis.TokenStream {
	var out analysis.TokenStream
	text := string(input)
	pos := 0
	start := -1
	emit := func(s, e int) {
		term := text[s:e]
		if a.norm != nil {
			if ts := a.norm.Analyze([]byte(term)); len(ts) == 1 {
				term = string(ts[0].Term)
			}
		}
		pos++
		out = append(out, &analysis.Token{Term: []byte(term), Start: s, End: e, Position: pos})
	}
	for i, r := range text {
		if unicode.IsSpace(r) {
			if start >= 0 {
				emit(start, i)
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		emit(start, len(text))
	}
	return out
}

// searchAnalyzer returns the analyzer of query text for a field, or nil
// when the text is used as one term (Lucene's keyword analyzer). The name
// identifies the analyzer (cross_fields groups fields by it).
func (qb *queryBuilder) searchAnalyzer(kind string, f *Field, override string, quoted bool) (analysis.Analyzer, string, error) {
	if override != "" {
		an, err := qb.ix.analysis.analyzerNamed(override)
		if err != nil {
			return nil, "", errQueryShard("[%s] analyzer [%s] not found", kind, override)
		}
		return an, "analyzer:" + override, nil
	}
	switch {
	case f.Type == TypeText || f.Type == TypeMatchOnlyText || f.Type == TypeSearchAsYouType:
		name := f.SearchAnalyzer
		if quoted {
			if q := getString(f.Extra, "search_quote_analyzer"); q != "" {
				name = q
			}
		}
		if name == "" {
			name = f.Analyzer
		}
		an, err := qb.ix.analysis.analyzerNamed(name)
		if err != nil {
			return nil, "", err
		}
		if name == "" {
			name = "standard"
		}
		if f.shingles > 0 {
			// the subfields of search_as_you_type fields search shingles
			an, name = saytSearchAnalyzer(an, name, f)
		}
		return an, "analyzer:" + name, nil
	case f.isKeywordLike() && f.Type != TypeIP:
		var norm analysis.Analyzer
		if f.Normalizer != "" {
			var err error
			if norm, err = qb.ix.analysis.normalizerNamed(f.Normalizer); err != nil {
				return nil, "", err
			}
		}
		if getBool(f.Extra, "split_queries_on_whitespace", false) {
			return &splitNormalizer{norm: norm}, "split:" + f.Normalizer, nil
		}
		if norm != nil {
			return norm, "normalizer:" + f.Normalizer, nil
		}
	}
	return nil, "keyword", nil
}

// tokenGroup is the terms at one position of analyzed text.
type tokenGroup struct {
	pos   int
	terms []string
}

func analyzeGroups(an analysis.Analyzer, text string) []tokenGroup {
	var out []tokenGroup
	for _, t := range an.Analyze([]byte(text)) {
		if n := len(out); n > 0 && out[n-1].pos == t.Position {
			out[n-1].terms = append(out[n-1].terms, string(t.Term))
			continue
		}
		out = append(out, tokenGroup{pos: t.Position, terms: []string{string(t.Term)}})
	}
	return out
}

type matchOptions struct {
	fuzziness      *fuzzinessSpec
	prefixLength   int
	maxExpansions  int
	transpositions bool
	lenient        bool
	operator       string
	msm            *string
	zeroTermsAll   bool
	// zeroTermsNull makes text without terms produce no query at all (the
	// query parsers drop such clauses)
	zeroTermsNull bool
}

func (o *matchOptions) zeroTerms() query.Query {
	if o.zeroTermsNull {
		return nil
	}
	return zeroTermsQuery(o.zeroTermsAll)
}

func zeroTermsQuery(all bool) query.Query {
	if all {
		return &constantScoreQuery{inner: bleve.NewMatchAllQuery(), score: 1}
	}
	return bleve.NewMatchNoneQuery()
}

// matchTerm is MatchQuery's newTermQuery for one analyzed term.
func (qb *queryBuilder) matchTerm(field string, f *Field, token string, o *matchOptions) (query.Query, error) {
	lenient := func(err error) (query.Query, error) {
		if o.lenient {
			return bleve.NewMatchNoneQuery(), nil
		}
		return nil, err
	}
	if o.fuzziness != nil {
		if field == "_id" || field == "_index" || !isStringField(f) {
			typ := field
			if f != nil {
				typ = f.Type
			}
			return lenient(errCreateQuery("illegal_argument_exception", "Can only use fuzzy queries on keyword and text fields - not on ["+field+"] which is of type ["+typ+"]"))
		}
		if err := notSearchableError(field, f); err != nil {
			return lenient(err)
		}
		return qb.similarityTerms(f, qb.fuzzyTermQuery(qb.ix.Mapping.searchPath(field), token, o.fuzziness.distance(token), o.prefixLength, o.maxExpansions, o.transpositions)), nil
	}
	if f != nil && (f.Type == TypeText || f.Type == TypeMatchOnlyText || f.Type == TypeSearchAsYouType) {
		if err := notSearchableError(field, f); err != nil {
			return lenient(err)
		}
		tq := bleve.NewTermQuery(token)
		tq.SetField(qb.ix.Mapping.searchPath(field))
		if qb.ix.booleanSimilarity(f) || f.saytPrefix {
			// the boolean similarity scores 1 per term; the prefix field of
			// search_as_you_type fields has no norms (scored as keywords)
			return &constantScoreQuery{inner: tq, score: 1}, nil
		}
		return &isolatedQuery{inner: tq}, nil
	}
	if f != nil {
		if err := qb.checkExactField(field, f); err != nil {
			return lenient(err)
		}
	}
	q, err := qb.exactTermQuery(field, f, token)
	if err != nil {
		return lenient(err)
	}
	return qb.similarityLeaf(f, q), nil
}

// groupQuery creates the query of the terms at one position (a synonym
// query when there are several).
func (qb *queryBuilder) groupQuery(field string, f *Field, g tokenGroup, o *matchOptions, prefix bool) (query.Query, error) {
	var subs []query.Query
	for _, t := range g.terms {
		var q query.Query
		var err error
		if prefix {
			q, err = qb.prefixTerm(field, f, t, o)
		} else {
			q, err = qb.matchTerm(field, f, t, o)
		}
		if err != nil {
			return nil, err
		}
		subs = append(subs, q)
	}
	if len(subs) == 1 {
		return subs[0], nil
	}
	return &luceneBoolQuery{should: subs}, nil
}

func (qb *queryBuilder) prefixTerm(field string, f *Field, token string, o *matchOptions) (query.Query, error) {
	if f == nil || !isStringField(f) {
		typ := field
		if f != nil {
			typ = f.Type
		}
		err := errQueryShard("Can only use prefix queries on keyword and text fields - not on [%s] which is of type [%s]", field, typ)
		if o.lenient {
			return bleve.NewMatchNoneQuery(), nil
		}
		return nil, err
	}
	path := qb.ix.Mapping.searchPath(field)
	return &termsUnionQuery{field: path, constant: true, boost: 1, expand: prefixExpansion(path, token)}, nil
}

// booleanOf combines clauses with an operator (Lucene's BooleanQuery).
func booleanOf(clauses []query.Query, operator string) *luceneBoolQuery {
	if operator == "and" {
		return &luceneBoolQuery{must: clauses}
	}
	return &luceneBoolQuery{should: clauses}
}

// applyMSM is Queries.maybeApplyMinimumShouldMatch.
func applyMSM(q query.Query, msm *string) (query.Query, error) {
	bq, ok := q.(*luceneBoolQuery)
	if msm == nil || !ok {
		return q, nil
	}
	n, err := calcMinShouldMatch(len(bq.should), *msm)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		c := *bq
		c.minShould = n
		return &c, nil
	}
	return q, nil
}

// matchField is MatchQuery.parse for the BOOLEAN and BOOLEAN_PREFIX types.
func (qb *queryBuilder) matchField(kind, field string, value any, analyzer string, o *matchOptions, boolPrefix bool) (query.Query, error) {
	text := xText(value)
	if field == "_id" || field == "_index" {
		q, err := qb.exactTermQuery(field, nil, text)
		if err != nil {
			return nil, err
		}
		return &constantScoreQuery{inner: q, score: 1}, nil
	}
	f, _, mapped := qb.ix.Mapping.resolve(field)
	if !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	an, _, err := qb.searchAnalyzer(kind, f, analyzer, false)
	if err != nil {
		return nil, err
	}
	if an == nil {
		if boolPrefix && (f.Type == TypeText || f.Type == TypeMatchOnlyText || f.Type == TypeKeyword) {
			return qb.prefixTerm(field, f, text, o)
		}
		return qb.matchTerm(field, f, text, o)
	}
	groups := analyzeGroups(an, text)
	if len(groups) == 0 {
		return o.zeroTerms(), nil
	}
	if len(groups) == 1 && !boolPrefix {
		return qb.groupQuery(field, f, groups[0], o, false)
	}
	var clauses []query.Query
	for i, g := range groups {
		q, err := qb.groupQuery(field, f, g, o, boolPrefix && i == len(groups)-1)
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, q)
	}
	if len(clauses) == 1 {
		return clauses[0], nil
	}
	return booleanOf(clauses, o.operator), nil
}

func (qb *queryBuilder) matchToQuery(spec *matchSpec) (query.Query, error) {
	if spec.analyzer != "" {
		if _, err := qb.ix.analysis.analyzerNamed(spec.analyzer); err != nil {
			return nil, errQueryShard("[match] analyzer [%s] not found", spec.analyzer)
		}
	}
	if spec.fuzzyRewrite != nil && !validRewrite(*spec.fuzzyRewrite) {
		return nil, errCreateQuery("illegal_argument_exception", "Failed to parse rewrite_method ["+*spec.fuzzyRewrite+"]")
	}
	o := &matchOptions{fuzziness: spec.fuzziness, prefixLength: spec.prefixLength, maxExpansions: spec.maxExpansions,
		transpositions: spec.transpositions, lenient: spec.lenient, operator: spec.operator, zeroTermsAll: spec.zeroTermsAll}
	q, err := qb.matchField("match", spec.field, spec.query, spec.analyzer, o, false)
	if err != nil {
		return nil, err
	}
	return applyMSM(q, spec.msm)
}

func (qb *queryBuilder) boolPrefixToQuery(spec *matchSpec) (query.Query, error) {
	if spec.analyzer != "" {
		if _, err := qb.ix.analysis.analyzerNamed(spec.analyzer); err != nil {
			return nil, errQueryShard("[match_bool_prefix] analyzer [%s] not found", spec.analyzer)
		}
	}
	if spec.fuzzyRewrite != nil && !validRewrite(*spec.fuzzyRewrite) {
		return nil, errCreateQuery("illegal_argument_exception", "Failed to parse rewrite_method ["+*spec.fuzzyRewrite+"]")
	}
	o := &matchOptions{fuzziness: spec.fuzziness, prefixLength: spec.prefixLength, maxExpansions: spec.maxExpansions,
		transpositions: spec.transpositions, operator: spec.operator}
	q, err := qb.matchField("match_bool_prefix", spec.field, spec.query, spec.analyzer, o, true)
	if err != nil {
		return nil, err
	}
	return applyMSM(q, spec.msm)
}

// phrases ------------------------------------------------------------------------

func hasPositions(f *Field) bool {
	if f.Type != TypeText && f.Type != TypeMatchOnlyText && f.Type != TypeSearchAsYouType {
		return false
	}
	switch getString(f.Extra, "index_options") {
	case "docs", "freqs":
		return false
	}
	return true
}

// phraseField is MatchQuery.parse for the PHRASE and PHRASE_PREFIX types.
func (qb *queryBuilder) phraseField(kind, field string, value any, analyzer string, slop int, prefix bool, maxExpansions int, o *matchOptions) (query.Query, error) {
	text := xText(value)
	if field == "_id" || field == "_index" {
		q, err := qb.exactTermQuery(field, nil, text)
		if err != nil {
			return nil, err
		}
		return &constantScoreQuery{inner: q, score: 1}, nil
	}
	f, _, mapped := qb.ix.Mapping.resolve(field)
	if !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	anField := f
	if f.saytPrefix {
		// the prefix field of search_as_you_type fields analyzes phrases
		// without shingles and has no phrase queries
		if prefix {
			return nil, errCreateQuery("illegal_argument_exception", "Can only use phrase prefix queries on text fields - not on ["+field+"] which is of type [prefix]")
		}
		plain := *f
		plain.shingles, plain.saytPrefix = 0, false
		anField = &plain
	}
	an, _, err := qb.searchAnalyzer(kind, anField, analyzer, true)
	if err != nil {
		return nil, err
	}
	if prefix && f.Type != TypeText && f.Type != TypeMatchOnlyText && f.Type != TypeSearchAsYouType {
		return nil, errCreateQuery("illegal_argument_exception", "Can only use phrase prefix queries on text fields - not on ["+field+"] which is of type ["+f.Type+"]")
	}
	if an == nil {
		return qb.matchTerm(field, f, text, o)
	}
	groups := analyzeGroups(an, text)
	if len(groups) == 0 {
		return o.zeroTerms(), nil
	}
	if len(groups) == 1 && !prefix {
		return qb.groupQuery(field, f, groups[0], o, false)
	}
	if len(groups) > 1 && f.saytPrefix {
		return nil, errCreateQuery("illegal_argument_exception", "Can only use phrase queries on text fields - not on ["+field+"] which is of type [prefix]")
	}
	if len(groups) > 1 && !hasPositions(f) {
		return nil, errCreateQuery("illegal_state_exception", "field:["+field+"] was indexed without position data; cannot run PhraseQuery")
	}
	if err := notSearchableError(field, f); err != nil {
		return nil, err
	}
	first := groups[0].pos
	slots := make([]phraseSlot, len(groups))
	for i, g := range groups {
		slots[i] = phraseSlot{offset: g.pos - first, terms: g.terms}
	}
	if maxExpansions < 1 {
		maxExpansions = 1
	}
	return &phraseQuery{ix: qb.ix, field: qb.ix.Mapping.searchPath(field), f: f, slots: slots, slop: slop, prefix: prefix, maxExpansions: maxExpansions,
		constant: qb.ix.booleanSimilarity(f)}, nil
}

func (qb *queryBuilder) phraseToQuery(spec *phraseSpec) (query.Query, error) {
	kind := "match_phrase"
	if spec.prefix {
		kind = "match_phrase_prefix"
	}
	if spec.analyzer != "" {
		if _, err := qb.ix.analysis.analyzerNamed(spec.analyzer); err != nil {
			return nil, errQueryShard("[%s] analyzer [%s] not found", kind, spec.analyzer)
		}
	}
	o := &matchOptions{operator: "or", zeroTermsAll: spec.zeroTermsAll}
	return qb.phraseField(kind, spec.field, spec.query, spec.analyzer, spec.slop, spec.prefix, spec.maxExpansions, o)
}

// field resolution ---------------------------------------------------------------

// indexDefaultFields is the index.query.default_field setting.
func (ix *Index) defaultFields() []string {
	idx := getMap(ix.Settings, "index")
	if fields := getStrings(getMap(idx, "query"), "default_field"); len(fields) > 0 {
		return fields
	}
	if fields := getStrings(idx, "query.default_field"); len(fields) > 0 {
		return fields
	}
	return []string{"*"}
}

func hasAllFieldsWildcard(fields []fieldWeight) bool {
	for _, f := range fields {
		if f.field == "*" {
			return true
		}
	}
	return false
}

// textSearchable reports whether wildcard field patterns expand to a field.
func textSearchable(f *Field) bool {
	switch f.Type {
	case TypeObject, TypeNested, TypeGeoPoint, TypeGeoShape, TypeBinary, TypeCompletion, TypeKNNVector, TypeRankFeature,
		TypePercolator, TypeAlias, TypeIntegerRange, TypeLongRange, TypeFloatRange, TypeDoubleRange, TypeDateRange:
		return false
	}
	return true
}

// resolveFields is QueryParserHelper.resolveMappingFields: patterns expand to
// the searchable fields, unmapped names are dropped, weights of repeated
// fields multiply.
func (qb *queryBuilder) resolveFields(fields []fieldWeight, suffix string) []fieldWeight {
	var out []fieldWeight
	index := map[string]int{}
	for _, fw := range fields {
		allField := fw.field == "*"
		multi := strings.ContainsAny(fw.field, "*?")
		names := []string{fw.field}
		if multi {
			names = qb.ix.Mapping.leafFields(fw.field)
		}
		matched := map[string]bool{}
		for _, n := range names {
			matched[n] = true
		}
		for _, name := range names {
			if suffix != "" {
				if _, _, ok := qb.ix.Mapping.resolve(name + suffix); ok {
					name += suffix
				}
			}
			f, base, ok := qb.ix.Mapping.resolve(name)
			if !ok {
				continue
			}
			if allField && strings.HasPrefix(name, "_") {
				continue
			}
			if multi && !textSearchable(f) {
				continue
			}
			if base != name && matched[base] && f.Fields == nil {
				// an alias of a field the pattern also matched
				if target, _, _ := qb.ix.Mapping.resolve(base); target == f {
					name = base
				}
			}
			if i, seen := index[name]; seen {
				out[i].boost *= fw.boost
				continue
			}
			index[name] = len(out)
			out = append(out, fieldWeight{field: name, boost: fw.boost})
		}
	}
	return out
}

// multi_match --------------------------------------------------------------------

func (qb *queryBuilder) multiMatchToQuery(spec *multiMatchSpec) (query.Query, error) {
	if spec.analyzer != "" {
		if _, err := qb.ix.analysis.analyzerNamed(spec.analyzer); err != nil {
			return nil, errQueryShard("[multi_match] analyzer [%s] not found", spec.analyzer)
		}
	}
	if spec.fuzzyRewrite != nil && !validRewrite(*spec.fuzzyRewrite) {
		return nil, errCreateQuery("illegal_argument_exception", "Failed to parse rewrite_method ["+*spec.fuzzyRewrite+"]")
	}
	fields := spec.fields
	if len(fields) == 0 {
		for _, df := range qb.ix.defaultFields() {
			fw, err := parseFieldAndWeight(df)
			if err != nil {
				return nil, err
			}
			fields = append(fields, fw)
		}
	}
	lenient := false
	if spec.lenient != nil {
		lenient = *spec.lenient
	} else if hasAllFieldsWildcard(fields) {
		lenient = true
	}
	resolved := qb.resolveFields(fields, "")
	if len(resolved) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	tie := 0.0
	if spec.typ == "most_fields" || spec.typ == "bool_prefix" {
		tie = 1
	}
	if spec.tieBreaker != nil {
		tie = float64(float32(*spec.tieBreaker))
	}
	o := &matchOptions{fuzziness: spec.fuzziness, prefixLength: spec.prefixLength, maxExpansions: spec.maxExpansions,
		transpositions: spec.transpositions, lenient: lenient, operator: spec.operator, msm: spec.msm, zeroTermsAll: spec.zeroTermsAll}
	var groups []query.Query
	if spec.typ == "cross_fields" {
		var err error
		if groups, err = qb.crossFieldsGroups(resolved, spec.query, spec.analyzer, tie, o); err != nil {
			return nil, err
		}
	} else {
		for _, fw := range resolved {
			var q query.Query
			var err error
			switch spec.typ {
			case "phrase", "phrase_prefix":
				q, err = qb.phraseField("multi_match", fw.field, spec.query, spec.analyzer, spec.slop, spec.typ == "phrase_prefix", spec.maxExpansions, o)
			default:
				q, err = qb.matchField("multi_match", fw.field, spec.query, spec.analyzer, o, spec.typ == "bool_prefix")
			}
			if err != nil {
				return nil, err
			}
			if q == nil {
				continue
			}
			if q, err = applyMSM(q, spec.msm); err != nil {
				return nil, err
			}
			groups = append(groups, newBoost(q, fw.boost))
		}
	}
	switch len(groups) {
	case 0:
		return zeroTermsQuery(spec.zeroTermsAll), nil
	case 1:
		return groups[0], nil
	}
	return &disMaxQuery{queries: groups, tie: tie}, nil
}

// crossFieldsGroups builds one query per group of fields sharing a search
// analyzer: every analyzed term is blended over the fields of the group.
func (qb *queryBuilder) crossFieldsGroups(fields []fieldWeight, value any, analyzer string, tie float64, o *matchOptions) ([]query.Query, error) {
	type group struct {
		an     analysis.Analyzer
		fields []fieldWeight
	}
	var order []string
	groups := map[string]*group{}
	for _, fw := range fields {
		f, _, ok := qb.ix.Mapping.resolve(fw.field)
		if !ok {
			continue
		}
		an, key, err := qb.searchAnalyzer("multi_match", f, analyzer, false)
		if err != nil {
			return nil, err
		}
		g := groups[key]
		if g == nil {
			g = &group{an: an}
			groups[key] = g
			order = append(order, key)
		}
		g.fields = append(g.fields, fw)
	}
	text := xText(value)
	var out []query.Query
	for _, key := range order {
		g := groups[key]
		var tokensAt []tokenGroup
		if g.an == nil {
			tokensAt = []tokenGroup{{pos: 1, terms: []string{text}}}
		} else {
			tokensAt = analyzeGroups(g.an, text)
		}
		if len(tokensAt) == 0 {
			if q := o.zeroTerms(); q != nil {
				out = append(out, q)
			}
			continue
		}
		var clauses []query.Query
		for _, tg := range tokensAt {
			var perField []query.Query
			for _, fw := range g.fields {
				f, _, _ := qb.ix.Mapping.resolve(fw.field)
				q, err := qb.groupQuery(fw.field, f, tg, o, false)
				if err != nil {
					return nil, err
				}
				perField = append(perField, newBoost(q, fw.boost))
			}
			if len(perField) == 1 {
				clauses = append(clauses, perField[0])
			} else {
				clauses = append(clauses, &disMaxQuery{queries: perField, tie: tie})
			}
		}
		var q query.Query
		if len(clauses) == 1 {
			q = clauses[0]
		} else {
			q = booleanOf(clauses, o.operator)
		}
		q, err := applyMSM(q, o.msm)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, nil
}

// combined_fields ------------------------------------------------------------------

func (qb *queryBuilder) combinedFieldsToQuery(spec *combinedFieldsSpec) (query.Query, error) {
	resolved := qb.resolveFields(spec.fields, "")
	var an analysis.Analyzer
	for _, fw := range resolved {
		f, _, _ := qb.ix.Mapping.resolve(fw.field)
		if f.Type != TypeText && f.Type != TypeMatchOnlyText {
			return nil, errCreateQuery("illegal_argument_exception", "Field ["+fw.field+"] of type ["+f.Type+"] does not support [combined_fields] queries")
		}
		if an == nil {
			var err error
			if an, _, err = qb.searchAnalyzer("combined_fields", f, "", false); err != nil {
				return nil, err
			}
		}
	}
	if len(resolved) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	groups := analyzeGroups(an, xText(spec.query))
	if len(groups) == 0 {
		return zeroTermsQuery(spec.zeroTermsAll), nil
	}
	o := &matchOptions{operator: spec.operator}
	var clauses []query.Query
	for _, tg := range groups {
		var perField []query.Query
		for _, fw := range resolved {
			f, _, _ := qb.ix.Mapping.resolve(fw.field)
			q, err := qb.groupQuery(fw.field, f, tg, o, false)
			if err != nil {
				return nil, err
			}
			perField = append(perField, newBoost(q, fw.boost))
		}
		clauses = append(clauses, &luceneBoolQuery{should: perField})
	}
	var q query.Query = booleanOf(clauses, spec.operator)
	if len(clauses) == 1 {
		q = clauses[0]
	}
	return applyMSM(q, spec.msm)
}

// common ---------------------------------------------------------------------------

func (qb *queryBuilder) commonToQuery(spec *commonSpec) (query.Query, error) {
	f, _, mapped := qb.ix.Mapping.resolve(spec.field)
	if !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	an, _, err := qb.searchAnalyzer("common", f, spec.analyzer, false)
	if err != nil {
		return nil, err
	}
	var terms []string
	if an == nil {
		terms = []string{xText(spec.query)}
	} else {
		for _, g := range analyzeGroups(an, xText(spec.query)) {
			terms = append(terms, g.terms...)
		}
	}
	if len(terms) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	maxDoc, err := qb.ix.bleve.DocCount()
	if err != nil {
		return nil, err
	}
	o := &matchOptions{operator: "or"}
	var low, high []query.Query
	for _, t := range terms {
		q, err := qb.matchTerm(spec.field, f, t, o)
		if err != nil {
			return nil, err
		}
		res, err := qb.evaluate(q)
		if err != nil {
			return nil, err
		}
		df := float64(len(res))
		cut := spec.cutoff
		isHigh := (cut >= 1 && df > cut) || df > float64(int64(ceilFloat32(float32(cut)*float32(maxDoc))))
		if isHigh {
			high = append(high, q)
		} else {
			low = append(low, q)
		}
	}
	lowOp, highOp := spec.lowFreqOperator, spec.highFreqOperator
	bq := &luceneBoolQuery{}
	if len(low) > 0 {
		lq := booleanOf(low, lowOp)
		if lowOp == "or" && spec.lowFreqMSM != nil {
			n, err := calcMinShouldMatch(len(low), *spec.lowFreqMSM)
			if err != nil {
				return nil, err
			}
			lq.minShould = n
		}
		bq.must = append(bq.must, lq)
	}
	if len(high) > 0 {
		minHigh := 0
		if highOp == "or" && spec.highFreqMSM != nil {
			n, err := calcMinShouldMatch(len(high), *spec.highFreqMSM)
			if err != nil {
				return nil, err
			}
			minHigh = n
		}
		if len(low) == 0 && minHigh == 0 {
			highOp = "and"
		}
		hq := booleanOf(high, highOp)
		hq.minShould = minHigh
		bq.should = append(bq.should, hq)
	}
	return bq, nil
}

func ceilFloat32(f float32) float32 {
	i := float32(int64(f))
	if i < f {
		return i + 1
	}
	return i
}
