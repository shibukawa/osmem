package engine

import (
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"
	index "github.com/blevesearch/bleve_index_api"
)

// Creation of the parsed queries for one index (the shard step).

// toQuery creates the bleve query of a parsed query, applies its boost and
// registers its name.
func (qb *queryBuilder) toQuery(n *qnode) (query.Query, error) {
	q, err := qb.createQuery(n)
	if err != nil {
		return nil, err
	}
	if q == nil {
		q = bleve.NewMatchNoneQuery()
	}
	if n.boost != 1 {
		q = newBoost(q, n.boost)
	}
	if n.name != "" {
		qb.named = append(qb.named, namedQuery{name: n.name, q: q})
	}
	return q, nil
}

func (qb *queryBuilder) createQuery(n *qnode) (query.Query, error) {
	switch spec := n.spec.(type) {
	case *matchSpec:
		if n.kind == "match_bool_prefix" {
			return qb.boolPrefixToQuery(spec)
		}
		return qb.matchToQuery(spec)
	case *phraseSpec:
		return qb.phraseToQuery(spec)
	case *termSpec:
		return qb.termToQuery(spec)
	case *multiTermSpec:
		return qb.multiTermToQuery(n.kind, spec)
	case *fuzzySpec:
		return qb.fuzzyToQuery(spec)
	case *termsSpec:
		return qb.termsToQuery(spec)
	case *termsSetSpec:
		return qb.termsSetToQuery(spec)
	case *idsSpec:
		return qb.idsToQuery(spec)
	case *existsSpec:
		return qb.existsToQuery(spec)
	case *rangeSpec:
		return qb.rangeToQuery(spec)
	case *boolSpec:
		return qb.boolToQuery(spec)
	case *constantScoreSpec:
		scored := qb.noScores
		qb.noScores = true
		inner, err := qb.toQuery(spec.filter)
		qb.noScores = scored
		if err != nil {
			return nil, err
		}
		return &constantScoreQuery{inner: inner, score: 1}, nil
	case *disMaxSpec:
		return qb.disMaxToQuery(spec)
	case *boostingSpec:
		pos, err := qb.toQuery(spec.positive)
		if err != nil {
			return nil, err
		}
		neg, err := qb.toQuery(spec.negative)
		if err != nil {
			return nil, err
		}
		return &boostingQuery{positive: pos, negative: neg, negativeBoost: float64(float32(spec.negativeBoost))}, nil
	case *functionScoreSpec:
		return qb.functionScoreToQuery(spec)
	case *nestedSpec:
		return qb.nestedQuery(spec)
	case *multiMatchSpec:
		return qb.multiMatchToQuery(spec)
	case *combinedFieldsSpec:
		return qb.combinedFieldsToQuery(spec)
	case *commonSpec:
		return qb.commonToQuery(spec)
	case *queryStringSpec:
		return qb.queryStringToQuery(spec)
	case *simpleQueryStringSpec:
		return qb.simpleQueryStringToQuery(spec)
	case *geoDistanceSpec:
		return qb.geoDistanceToQuery(spec)
	case *geoBBoxSpec:
		return qb.geoBoundingBoxToQuery(spec)
	case *geoPolygonSpec:
		return qb.geoPolygonToQuery(spec)
	case *rankFeatureSpec:
		return qb.rankFeatureToQuery(spec)
	case *parentIDSpec:
		return qb.parentIDToQuery(spec)
	case *joinQuerySpec:
		return qb.joinQueryToQuery(spec)
	case *intervalsSpec:
		return qb.intervalsToQuery(spec)
	case *moreLikeThisSpec:
		return qb.moreLikeThisToQuery(spec)
	case *distanceFeatureSpec:
		return qb.distanceFeatureToQuery(spec)
	case *spanTermSpec:
		return qb.spanTermToQuery(spec)
	case *spanNearSpec:
		return qb.spanNearToQuery(spec)
	case *spanMultiSpec:
		return qb.spanMultiToQuery(spec)
	case *geoShapeSpec:
		return qb.geoShapeToQuery(spec)
	case *scriptQuerySpec:
		return qb.scriptQueryToQuery(spec)
	case *scriptScoreQuerySpec:
		return qb.scriptScoreQueryToQuery(spec)
	}
	switch n.kind {
	case "match_all":
		return &constantScoreQuery{inner: bleve.NewMatchAllQuery(), score: 1}, nil
	case "match_none":
		return bleve.NewMatchNoneQuery(), nil
	}
	return nil, errUnsupported("[" + n.kind + "] query")
}

// minimumShouldMatch resolves a minimum_should_match spec for a number of
// optional clauses (Queries.calculateMinShouldMatch; 1 when the spec is
// invalid). The result is not capped at the number of clauses.
func minimumShouldMatch(spec string, clauses int) int {
	n, err := calcMinShouldMatch(clauses, spec)
	if err != nil {
		return 1
	}
	return n
}

// field helpers ----------------------------------------------------------------

// isStringField reports whether a field supports the term-level string
// queries (prefix, wildcard, regexp, fuzzy).
func isStringField(f *Field) bool {
	switch f.Type {
	case TypeKeyword, TypeText, TypeWildcard, TypeConstantKeyword, TypeSearchAsYouType, TypeFlatObject, TypeVersion, TypeIgnoredMeta:
		return true
	}
	return false
}

// normalizeForField applies a keyword field's normalizer to a query value.
func (qb *queryBuilder) normalizeForField(f *Field, s string) string {
	if f == nil || !f.isKeywordLike() || f.Normalizer == "" {
		return s
	}
	an, err := qb.ix.analysis.normalizerNamed(f.Normalizer)
	if err != nil {
		return s
	}
	if ts := tokens(an, s); len(ts) == 1 {
		return ts[0]
	}
	return s
}

// normalizeWildcard normalizes the literal parts of a wildcard pattern for
// a keyword field with a normalizer, keeping wildcards and escapes.
func (qb *queryBuilder) normalizeWildcard(f *Field, pattern string) string {
	if f == nil || !f.isKeywordLike() || f.Normalizer == "" {
		return pattern
	}
	var sb strings.Builder
	var chunk strings.Builder
	flush := func() {
		if chunk.Len() > 0 {
			sb.WriteString(qb.normalizeForField(f, chunk.String()))
			chunk.Reset()
		}
	}
	rs := []rune(pattern)
	for i := 0; i < len(rs); i++ {
		switch {
		case rs[i] == '\\' && i+1 < len(rs):
			flush()
			sb.WriteRune(rs[i])
			sb.WriteRune(rs[i+1])
			i++
		case rs[i] == '*' || rs[i] == '?':
			flush()
			sb.WriteRune(rs[i])
		default:
			chunk.WriteRune(rs[i])
		}
	}
	flush()
	return sb.String()
}

// notSearchable reports index: false fields without doc values.
func notSearchableError(field string, f *Field) error {
	if err := searchableError(field, f); err != nil {
		return err
	}
	return nil
}

// scoreLeaf gives an exact query the score OpenSearch gives it: text and
// boolean terms are scored, other exact queries score 1.
func scoreLeaf(f *Field, q query.Query) query.Query {
	if _, none := q.(*query.MatchNoneQuery); none {
		return q
	}
	if f != nil && (f.Type == TypeText || f.Type == TypeMatchOnlyText || f.Type == TypeSearchAsYouType || f.Type == TypeBoolean) {
		return &isolatedQuery{inner: q}
	}
	return &constantScoreQuery{inner: q, score: 1}
}

// term -------------------------------------------------------------------------

func (qb *queryBuilder) termToQuery(spec *termSpec) (query.Query, error) {
	field := spec.field
	f, _, mapped := qb.ix.Mapping.resolve(field)
	if field != "_id" && field != "_index" && !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	if mapped {
		if err := qb.checkExactField(field, f); err != nil {
			return nil, err
		}
	}
	if spec.caseInsensitive && field == "_id" {
		// the case insensitive query of _id runs on the encoded ids
		return bleve.NewMatchNoneQuery(), nil
	}
	if spec.caseInsensitive && field == "_index" && !mapped {
		if strings.EqualFold(xText(spec.value), qb.ix.Name) {
			return &constantScoreQuery{inner: bleve.NewMatchAllQuery(), score: 1}, nil
		}
		return bleve.NewMatchNoneQuery(), nil
	}
	if spec.caseInsensitive && (f == nil || f.Type != TypeBoolean) {
		if field == "_id" || field == "_index" || !isStringField(f) {
			typ := field
			if f != nil {
				typ = f.Type
			}
			return nil, errQueryShard("[%s] field which is of type [%s], does not support case insensitive term queries", field, typ)
		}
		value := qb.normalizeForField(f, xText(spec.value))
		path := qb.ix.Mapping.searchPath(field)
		return &termsUnionQuery{field: path, constant: true, boost: 1, expand: func(i index.IndexReader) ([]string, []float64, error) {
			terms, err := dictTerms(i, path, "")
			if err != nil {
				return nil, nil, err
			}
			var out []string
			for _, t := range terms {
				if len(t) == len(value) && strings.EqualFold(t, value) || equalFoldASCII(t, value) {
					out = append(out, t)
				}
			}
			return out, nil, nil
		}}, nil
	}
	q, err := qb.exactTermQuery(field, f, spec.value)
	if err != nil {
		return nil, err
	}
	return qb.similarityLeaf(f, q), nil
}

func equalFoldASCII(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	if len(ra) != len(rb) {
		return false
	}
	for i := range ra {
		if !runeEqual(ra[i], rb[i], true) {
			return false
		}
	}
	return true
}

// checkExactField rejects exact queries on fields that do not support them.
func (qb *queryBuilder) checkExactField(field string, f *Field) error {
	switch f.Type {
	case TypeGeoPoint, TypeGeoShape:
		return errQueryShard("Geometry fields do not support exact searching, use dedicated geometry queries instead: [%s]", field)
	case TypeRankFeature:
		return errCreateQuery("illegal_argument_exception", "Queries on [rank_feature] fields are not supported")
	case TypeRankFeatures:
		return errCreateQuery("illegal_argument_exception", "Queries on [rank_features] fields are not supported")
	case TypeKNNVector:
		return errQueryShard("KNN vector do not support exact searching, use KNN queries instead: [%s]", field)
	}
	return notSearchableError(field, f)
}

// terms ------------------------------------------------------------------------

func (qb *queryBuilder) termsToQuery(spec *termsSpec) (query.Query, error) {
	values := spec.values
	if spec.lookup != nil {
		vals, err := qb.termsLookupValues(spec.lookup)
		if err != nil {
			return nil, err
		}
		values = vals
	}
	field := spec.field
	f, _, mapped := qb.ix.Mapping.resolve(field)
	if len(values) == 0 || (field != "_id" && field != "_index" && !mapped) {
		return bleve.NewMatchNoneQuery(), nil
	}
	if mapped {
		if err := qb.checkExactField(field, f); err != nil {
			return nil, err
		}
	}
	var subs []query.Query
	for _, v := range values {
		if v == nil {
			continue
		}
		q, err := qb.exactTermQuery(field, f, v)
		if err != nil {
			return nil, err
		}
		subs = append(subs, q)
	}
	if len(subs) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	return &constantScoreQuery{inner: &luceneBoolQuery{should: subs}, score: 1}, nil
}

func (qb *queryBuilder) termsLookupValues(spec *termsLookupSpec) ([]any, error) {
	ix, err := qb.c.resolveWriteIndex(spec.index)
	if err != nil {
		if e, ok := err.(*Error); ok {
			// the lookup runs while the request is rewritten, before any
			// shard is involved
			return nil, parseFailure(e)
		}
		return nil, err
	}
	if spec.query != nil {
		return qb.termsLookupQueryValues(ix, spec)
	}
	d := ix.docs[spec.id]
	if d == nil {
		return nil, nil
	}
	return flattenValues(lookupPath(d.Src, spec.path)), nil
}

// termsLookupQueryValues is the "lookup by query" form of terms_lookup
// (OpenSearch 3.2+): path values of every document of ix matching the
// query, deduplicated across documents.
func (qb *queryBuilder) termsLookupQueryValues(ix *Index, spec *termsLookupSpec) ([]any, error) {
	lookupQB := &queryBuilder{c: qb.c, ix: ix, noScores: true}
	q, err := lookupQB.toQuery(spec.query)
	if err != nil {
		return nil, err
	}
	matches, err := lookupQB.evaluate(q)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []any
	for id := range matches {
		d := ix.docs[id]
		if d == nil {
			continue
		}
		for _, v := range flattenValues(lookupPath(d.Src, spec.path)) {
			key := queryValueText(v)
			if !seen[key] {
				seen[key] = true
				out = append(out, v)
			}
		}
	}
	return out, nil
}

// terms_set --------------------------------------------------------------------

func (qb *queryBuilder) termsSetToQuery(spec *termsSetSpec) (query.Query, error) {
	if len(spec.terms) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	if spec.msmScript {
		return nil, errUnsupported("[terms_set] minimum_should_match_script")
	}
	if spec.msmField == "" {
		return nil, errCreateQuery("illegal_state_exception", "No minimum should match has been specified")
	}
	if _, _, ok := qb.ix.Mapping.resolve(spec.msmField); !ok {
		return nil, errQueryShard("failed to find minimum_should_match field [%s]", spec.msmField)
	}
	f, _, mapped := qb.ix.Mapping.resolve(spec.field)
	if !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	counts := map[string]float64{}
	matched := map[string]int{}
	for _, t := range spec.terms {
		q, err := qb.exactTermQuery(spec.field, f, t)
		if err != nil {
			return nil, err
		}
		res, err := qb.evaluate(scoreLeaf(f, q))
		if err != nil {
			return nil, err
		}
		for id, e := range res {
			counts[id] += e.score
			matched[id]++
		}
	}
	scores := map[string]float64{}
	for id, n := range matched {
		vals := qb.docValues(id, spec.msmField)
		if len(vals) == 0 {
			continue
		}
		required, ok := numericDocValueOf(vals[0])
		if !ok {
			continue
		}
		if float64(n) >= required {
			scores[id] = counts[id]
		}
	}
	return &presetQuery{scores: scores}, nil
}

// ids --------------------------------------------------------------------------

func (qb *queryBuilder) idsToQuery(spec *idsSpec) (query.Query, error) {
	for _, v := range spec.values {
		if v == "" {
			return nil, errCreateQuery("illegal_argument_exception", "Ids can't be empty")
		}
	}
	if len(spec.values) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	return &constantScoreQuery{inner: bleve.NewDocIDQuery(spec.values), score: 1}, nil
}

// exists -----------------------------------------------------------------------

func (qb *queryBuilder) existsToQuery(spec *existsSpec) (query.Query, error) {
	if spec.field == "_source" {
		return nil, errQueryShard("The _source field is not searchable")
	}
	q, err := qb.existsFieldQuery(spec.field)
	if err != nil {
		return nil, err
	}
	if _, none := q.(*query.MatchNoneQuery); none {
		return q, nil
	}
	return &constantScoreQuery{inner: q, score: 1}, nil
}

// range ------------------------------------------------------------------------

func (qb *queryBuilder) rangeToQuery(spec *rangeSpec) (query.Query, error) {
	field := spec.field
	if field == "_id" {
		return nil, errCreateQuery("illegal_argument_exception", "Field [_id] of type [_id] does not support range queries")
	}
	f, _, mapped := qb.ix.Mapping.resolve(field)
	if !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	switch f.Type {
	case TypeGeoPoint, TypeGeoShape:
		return nil, errCreateQuery("illegal_argument_exception", "Field ["+field+"] of type ["+f.Type+"] does not support range queries")
	}
	if err := notSearchableError(field, f); err != nil {
		return nil, err
	}
	var q query.Query
	if !spec.bounded {
		// no bound: every document with a value
		q = qb.existsQuery(field)
	} else {
		var err error
		if q, err = qb.rangeQuery(M{field: spec.params}); err != nil {
			return nil, err
		}
	}
	if _, none := q.(*query.MatchNoneQuery); none {
		return q, nil
	}
	return &constantScoreQuery{inner: q, score: 1}, nil
}

// bool -------------------------------------------------------------------------

func (qb *queryBuilder) boolToQuery(spec *boolSpec) (query.Query, error) {
	build := func(nodes []*qnode) ([]query.Query, error) {
		out := make([]query.Query, 0, len(nodes))
		for _, n := range nodes {
			q, err := qb.toQuery(n)
			if err != nil {
				return nil, err
			}
			out = append(out, q)
		}
		return out, nil
	}
	must, err := build(spec.must)
	if err != nil {
		return nil, err
	}
	scored := qb.noScores
	qb.noScores = true
	mustNot, err := build(spec.mustNot)
	qb.noScores = scored
	if err != nil {
		return nil, err
	}
	should, err := build(spec.should)
	if err != nil {
		return nil, err
	}
	qb.noScores = true
	filter, err := build(spec.filter)
	qb.noScores = scored
	if err != nil {
		return nil, err
	}
	if len(must)+len(mustNot)+len(should)+len(filter) == 0 {
		return &constantScoreQuery{inner: bleve.NewMatchAllQuery(), score: 1}, nil
	}
	bq := &luceneBoolQuery{must: must, filter: filter, should: should, mustNot: mustNot}
	if spec.msm != nil {
		msm, err := calcMinShouldMatch(len(should), *spec.msm)
		if err != nil {
			return nil, err
		}
		if msm > 0 {
			bq.minShould = msm
		}
	}
	if spec.adjustPureNegative && len(must)+len(should)+len(filter) == 0 {
		bq.filter = append(bq.filter, bleve.NewMatchAllQuery())
	}
	return bq, nil
}

// dis_max ----------------------------------------------------------------------

func (qb *queryBuilder) disMaxToQuery(spec *disMaxSpec) (query.Query, error) {
	var qs []query.Query
	for _, n := range spec.queries {
		q, err := qb.toQuery(n)
		if err != nil {
			return nil, err
		}
		qs = append(qs, q)
	}
	if len(qs) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	tie := float64(float32(spec.tie))
	if tie < 0 || tie > 1 {
		return nil, errCreateQuery("illegal_argument_exception", "tieBreakerMultiplier must be in [0, 1]")
	}
	return &disMaxQuery{queries: qs, tie: tie}, nil
}

// prefix, wildcard, regexp -------------------------------------------------------

func stringQueryTypeError(kind, field string, f *Field) error {
	typ := field
	if f != nil {
		typ = f.Type
	}
	return errQueryShard("Can only use %s queries on keyword and text fields - not on [%s] which is of type [%s]", kind, field, typ)
}

func (qb *queryBuilder) multiTermToQuery(kind string, spec *multiTermSpec) (query.Query, error) {
	if spec.rewrite != nil && !validRewrite(*spec.rewrite) {
		return nil, errCreateQuery("illegal_argument_exception", "Failed to parse rewrite_method ["+*spec.rewrite+"]")
	}
	field := spec.field
	constant := spec.rewrite == nil || !scoringRewrite(*spec.rewrite)
	if field == "_id" {
		return nil, stringQueryTypeError(kind, field, nil)
	}
	f, _, mapped := qb.ix.Mapping.resolve(field)
	if field == "_index" && !mapped {
		matched := false
		switch kind {
		case "prefix":
			matched = hasPrefixFold(qb.ix.Name, spec.value, spec.caseInsensitive)
		case "wildcard":
			matched = wildcardMatches(compileWildcard(spec.value), qb.ix.Name, spec.caseInsensitive)
		default:
			return nil, stringQueryTypeError(kind, field, nil)
		}
		if matched {
			return &constantScoreQuery{inner: bleve.NewMatchAllQuery(), score: 1}, nil
		}
		return bleve.NewMatchNoneQuery(), nil
	}
	var re *luceneRegexp
	if kind == "regexp" {
		value := spec.value
		if mapped {
			value = qb.normalizeForField(f, value)
		}
		var msg string
		if re, msg = compileLuceneRegexp(value, spec.flags, spec.caseInsensitive); msg != "" {
			return nil, errCreateQuery("illegal_argument_exception", msg)
		}
	}
	if !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	if !isStringField(f) {
		return nil, stringQueryTypeError(kind, field, f)
	}
	if err := notSearchableError(field, f); err != nil {
		return nil, err
	}
	// aliases search their target field
	field = qb.ix.Mapping.searchPath(field)
	ci := spec.caseInsensitive
	var expand func(i index.IndexReader) ([]string, []float64, error)
	switch kind {
	case "prefix":
		prefix := qb.normalizeForField(f, spec.value)
		expand = func(i index.IndexReader) ([]string, []float64, error) {
			if !ci {
				terms, err := dictTerms(i, field, prefix)
				return terms, nil, err
			}
			terms, err := dictTerms(i, field, "")
			if err != nil {
				return nil, nil, err
			}
			var out []string
			for _, t := range terms {
				if hasPrefixFold(t, prefix, true) {
					out = append(out, t)
				}
			}
			return out, nil, nil
		}
	case "wildcard":
		tokens := compileWildcard(qb.normalizeWildcard(f, spec.value))
		literal := wildcardLiteralPrefix(tokens)
		expand = func(i index.IndexReader) ([]string, []float64, error) {
			start := literal
			if ci {
				start = ""
			}
			terms, err := dictTerms(i, field, start)
			if err != nil {
				return nil, nil, err
			}
			var out []string
			for _, t := range terms {
				if wildcardMatches(tokens, t, ci) {
					out = append(out, t)
				}
			}
			return out, nil, nil
		}
	default:
		expand = func(i index.IndexReader) ([]string, []float64, error) {
			terms, err := dictTerms(i, field, "")
			if err != nil {
				return nil, nil, err
			}
			var out []string
			for _, t := range terms {
				if re.Matches(t) {
					out = append(out, t)
				}
			}
			return out, nil, nil
		}
	}
	return &termsUnionQuery{field: field, constant: constant, boost: 1, expand: expand}, nil
}

// prefixExpansion lists the index terms of a field starting with prefix.
func prefixExpansion(field, prefix string) func(i index.IndexReader) ([]string, []float64, error) {
	return func(i index.IndexReader) ([]string, []float64, error) {
		terms, err := dictTerms(i, field, prefix)
		return terms, nil, err
	}
}

// fuzzy ------------------------------------------------------------------------

func (qb *queryBuilder) fuzzyToQuery(spec *fuzzySpec) (query.Query, error) {
	if spec.rewrite != nil && !validRewrite(*spec.rewrite) {
		return nil, errCreateQuery("illegal_argument_exception", "Failed to parse rewrite_method ["+*spec.rewrite+"]")
	}
	field := spec.field
	f, _, mapped := qb.ix.Mapping.resolve(field)
	if !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	if !isStringField(f) {
		return nil, errCreateQuery("illegal_argument_exception", "Can only use fuzzy queries on keyword and text fields - not on ["+field+"] which is of type ["+f.Type+"]")
	}
	if err := notSearchableError(field, f); err != nil {
		return nil, err
	}
	if spec.prefixLength < 0 {
		return nil, errCreateQuery("illegal_argument_exception", "prefixLength cannot be negative.")
	}
	if spec.maxExpansions <= 0 {
		return nil, errCreateQuery("illegal_argument_exception", "maxExpansions must be positive.")
	}
	fz := spec.fuzziness
	if fz == nil {
		fz = &fuzzinessSpec{auto: true, low: 3, high: 6}
	}
	term := qb.normalizeForField(f, xText(spec.value))
	return qb.similarityTerms(f, qb.fuzzyTermQuery(qb.ix.Mapping.searchPath(field), term, fz.distance(term), spec.prefixLength, spec.maxExpansions, spec.transpositions)), nil
}

// fuzzyTermQuery is Lucene's FuzzyQuery with its top-terms rewrite: the
// maxExpansions most similar index terms, each scored by its similarity.
func (qb *queryBuilder) fuzzyTermQuery(field, term string, maxEdits, prefixLength, maxExpansions int, transpositions bool) query.Query {
	return &termsUnionQuery{field: field, boost: 1, expand: func(i index.IndexReader) ([]string, []float64, error) {
		return fuzzyCandidateTerms(i, field, term, maxEdits, prefixLength, maxExpansions, transpositions)
	}}
}

// fuzzyCandidateTerms lists the index terms of a field within maxEdits of
// term (top maxExpansions by similarity), the expansion fuzzyTermQuery and
// the intervals and span_multi fuzzy rules share.
func fuzzyCandidateTerms(i index.IndexReader, field, term string, maxEdits, prefixLength, maxExpansions int, transpositions bool) ([]string, []float64, error) {
	termRunes := []rune(term)
	if maxEdits == 0 || prefixLength >= len(termRunes) {
		return []string{term}, nil, nil
	}
	prefix := string(termRunes[:prefixLength])
	suffix := termRunes[prefixLength:]
	candidates, err := dictTerms(i, field, prefix)
	if err != nil {
		return nil, nil, err
	}
	type scored struct {
		term  string
		boost float32
	}
	var list []scored
	for _, c := range candidates {
		cr := []rune(c)
		if len(cr) < prefixLength {
			continue
		}
		ed := editDistance(cr[prefixLength:], suffix, transpositions, maxEdits)
		if ed > maxEdits {
			continue
		}
		boost := float32(1)
		if ed > 0 {
			min := utf8.RuneCountInString(c)
			if len(termRunes) < min {
				min = len(termRunes)
			}
			boost = 1 - float32(ed)/float32(min)
		}
		list = append(list, scored{c, boost})
	}
	sort.SliceStable(list, func(a, b int) bool {
		if list[a].boost != list[b].boost {
			return list[a].boost > list[b].boost
		}
		return list[a].term < list[b].term
	})
	if len(list) > maxExpansions {
		list = list[:maxExpansions]
	}
	terms := make([]string, len(list))
	weights := make([]float64, len(list))
	for k, s := range list {
		terms[k] = s.term
		weights[k] = float64(s.boost)
	}
	return terms, weights, nil
}

// nested helpers -----------------------------------------------------------------

// errNestedPath is a failure to resolve the path of a nested query.
func errNestedPath(reason string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "query_shard_exception", Reason: "failed to create query: " + reason,
		Cause: &Error{Type: "illegal_state_exception", Reason: reason}}
}
