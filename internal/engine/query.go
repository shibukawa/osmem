package engine

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
	index "github.com/blevesearch/bleve_index_api"
)

// queryBuilder translates the OpenSearch query DSL into bleve queries for
// one index.
type queryBuilder struct {
	c  *Cluster
	ix *Index
	// depth is the nested depth of the documents the query runs over (0
	// for root documents); inner collects the inner_hits of nested
	// queries at this level.
	depth int
	inner []*innerHitsResult
	// named collects the queries with a _name, in creation order.
	named []namedQuery
	// noScores builds queries whose scores are not needed (a sort without
	// the score, size 0, filter clauses): score functions are not run.
	noScores bool
}

// namedQuery is a query registered under its _name.
type namedQuery struct {
	name string
	q    query.Query
}

func errQueryShard(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "query_shard_exception", Reason: fmt.Sprintf(format, args...)}
}

// build parses a query object and creates its bleve query. Errors of the
// parse step are marked with parseFailure (see query_errors.go).
func (qb *queryBuilder) build(v any) (query.Query, error) {
	n, err := parseQuery(v)
	if err != nil {
		return nil, err
	}
	return qb.toQuery(n)
}

func setBoost(q query.Query, boost float64) query.Query {
	if boost == 1 {
		return q
	}
	if bq, ok := q.(query.BoostableQuery); ok {
		bq.SetBoost(boost)
		return q
	}
	return q
}

func tokens(an analysis.Analyzer, text string) []string {
	ts := an.Analyze([]byte(text))
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, string(t.Term))
	}
	return out
}

// exactTermQuery builds the query for a single, already normalized term
// against a typed field (TermQuery of the field type).
func (qb *queryBuilder) exactTermQuery(field string, f *Field, value any) (query.Query, error) {
	if field == "_id" {
		s, _ := stringValue(field, &Field{Type: TypeKeyword}, value)
		return bleve.NewDocIDQuery([]string{s}), nil
	}
	if field == "_index" {
		s, _ := stringValue(field, &Field{Type: TypeKeyword}, value)
		if indexMatchesName(qb.ix, s) {
			return bleve.NewMatchAllQuery(), nil
		}
		return bleve.NewMatchNoneQuery(), nil
	}
	if f == nil {
		return bleve.NewMatchNoneQuery(), nil
	}
	path := qb.ix.Mapping.searchPath(field)
	switch f.Type {
	case TypeObject, TypeNested:
		return bleve.NewMatchNoneQuery(), nil
	case TypeGeoPoint, TypeXYPoint, TypeGeoShape, TypeXYShape:
		return nil, errQueryShard("Geometry fields do not support exact searching, use dedicated geometry queries instead: [%s]", field)
	case TypeKNNVector:
		return nil, errQueryShard("KNN vector do not support exact searching, use KNN queries instead: [%s]", field)
	case TypeRankFeatures:
		return nil, errCreateQuery("illegal_argument_exception", "Queries on [rank_features] fields are not supported")
	case TypeConstantKeyword:
		s := queryValueText(value)
		if s == getString(f.Extra, "value") {
			return bleve.NewMatchAllQuery(), nil
		}
		return bleve.NewMatchNoneQuery(), nil
	}
	if err := searchableError(field, f); err != nil {
		return nil, err
	}
	switch {
	case isRangeType(f.Type):
		return qb.rangeFieldTermQuery(field, path, f, value)
	case f.Type == TypeFlatObject || f.Type == TypeJoin || f.Type == TypeCompletion || f.Type == TypeIgnoredMeta:
		s, err := stringValue(field, f, value)
		if err != nil {
			return nil, err
		}
		tq := bleve.NewTermQuery(s)
		tq.SetField(path)
		return tq, nil
	case f.isNumeric():
		q, err := numericTermQuery(path, f, value)
		if err != nil {
			return nil, err
		}
		return q, nil
	case f.isDate():
		lo, err := qb.dateQueryBound(f, f.Format, value, false, time.UTC)
		if err != nil {
			return nil, errCreateQueryCause(err)
		}
		hi, err := qb.dateQueryBound(f, f.Format, value, true, time.UTC)
		if err != nil {
			return nil, errCreateQueryCause(err)
		}
		return dateRangeBleveQuery(path, f, &lo, &hi), nil
	case f.Type == TypeBoolean:
		term, err := booleanQueryTerm(value)
		if err != nil {
			return nil, err
		}
		tq := bleve.NewTermQuery(term)
		tq.SetField(path)
		return tq, nil
	case f.Type == TypeIP:
		q, err := ipTermQuery(path, value)
		if err != nil {
			return nil, err
		}
		return q, nil
	case f.isKeywordLike():
		s, err := stringValue(field, f, value)
		if err != nil {
			return nil, err
		}
		an, err := qb.ix.analysis.normalizerNamed(f.Normalizer)
		if err != nil {
			return nil, err
		}
		if ts := tokens(an, s); len(ts) == 1 {
			s = ts[0]
		}
		tq := bleve.NewTermQuery(s)
		tq.SetField(path)
		return tq, nil
	case f.Type == TypeText, f.Type == TypeMatchOnlyText, f.Type == TypeSearchAsYouType:
		s, err := stringValue(field, f, value)
		if err != nil {
			return nil, err
		}
		tq := bleve.NewTermQuery(s)
		tq.SetField(path)
		return tq, nil
	}
	return bleve.NewMatchNoneQuery(), nil
}

// rangeQueryKeys are the parameters of a range query on a field.
var rangeQueryKeys = map[string]bool{"gt": true, "gte": true, "lt": true, "lte": true, "from": true, "to": true,
	"include_lower": true, "include_upper": true, "boost": true, "_name": true, "format": true, "time_zone": true, "relation": true}

func (qb *queryBuilder) rangeQuery(body any) (query.Query, error) {
	bm, ok := body.(M)
	if !ok || len(bm) == 0 {
		return nil, errParsing("query malformed, expected object with a field")
	}
	var field string
	var spec M
	for k, v := range bm {
		switch k {
		case "boost", "_name":
			continue
		}
		field = k
		sm, isObj := v.(M)
		if !isObj {
			return nil, errParsing("[range] query does not support [%s]", k)
		}
		spec = sm
	}
	if field == "" {
		return nil, errParsing("query malformed, no field specified")
	}
	for _, k := range sortedMapKeys(spec) {
		if !rangeQueryKeys[k] {
			return nil, errParsing("[range] query does not support [%s]", k)
		}
		if k == "gt" || k == "gte" || k == "lt" || k == "lte" || k == "from" || k == "to" {
			switch v := spec[k].(type) {
			case M:
				for _, inner := range sortedMapKeys(v) {
					return nil, errParsing("[range] query does not support [%s]", inner)
				}
				return nil, errParsing("[range] query does not support [%s]", k)
			case []any:
				if k == "gt" || k == "gte" || k == "from" {
					return nil, errParsing("invalid lower bound for [range] query")
				}
				return nil, errParsing("invalid upper bound for [range] query")
			}
		}
	}
	boost := 1.0
	if b, ok := bm["boost"]; ok {
		spec["boost"] = b
	}
	if b, ok := spec["boost"]; ok {
		d, err := objectToDouble(b)
		if err != nil {
			return nil, err
		}
		boost = d
	}
	// the lower and upper bounds, set once
	var lower, upper any
	includeLower, includeUpper := true, true
	lowerSet, upperSet := 0, 0
	incLower, hasIncLower := spec["include_lower"]
	incUpper, hasIncUpper := spec["include_upper"]
	for _, k := range []string{"from", "gt", "gte"} {
		v, ok := spec[k]
		if !ok {
			continue
		}
		lowerSet++
		lower = v
		switch k {
		case "gt":
			includeLower = false
			if hasIncLower && getBool(M{"b": incLower}, "b", false) {
				return nil, errParsing("invalid lower bound for [range] query")
			}
		case "gte":
			if hasIncLower && !getBool(M{"b": incLower}, "b", true) {
				return nil, errParsing("invalid lower bound for [range] query")
			}
		case "from":
			includeLower = getBool(spec, "include_lower", true)
		}
	}
	for _, k := range []string{"to", "lt", "lte"} {
		v, ok := spec[k]
		if !ok {
			continue
		}
		upperSet++
		upper = v
		switch k {
		case "lt":
			includeUpper = false
			if hasIncUpper && getBool(M{"b": incUpper}, "b", false) {
				return nil, errParsing("invalid upper bound for [range] query")
			}
		case "lte":
			if hasIncUpper && !getBool(M{"b": incUpper}, "b", true) {
				return nil, errParsing("invalid upper bound for [range] query")
			}
		case "to":
			includeUpper = getBool(spec, "include_upper", true)
		}
	}
	if lowerSet > 1 {
		return nil, errParsing("invalid lower bound for [range] query")
	}
	if upperSet > 1 {
		return nil, errParsing("invalid upper bound for [range] query")
	}
	if rel, ok := spec["relation"]; ok {
		name := strings.ToLower(queryValueText(rel))
		switch name {
		case "intersects", "contains", "within":
		case "disjoint":
			return nil, errIllegalArgument("[range] query does not support relation [%s]", queryValueText(rel))
		default:
			return nil, errIllegalArgument("%s is not a valid relation", queryValueText(rel))
		}
	}
	loc := time.UTC
	if tz := getString(spec, "time_zone"); tz != "" {
		l, err := parseTimeZone(tz)
		if err != nil {
			return nil, errIllegalArgument("java.time.zone.ZoneRulesException: Unknown time-zone ID: %s", tz)
		}
		loc = l
	}
	if field == "_id" {
		return nil, errCreateQueryCause(errIllegalArgument("Field [_id] of type [_id] does not support range queries"))
	}
	f, _, ok := qb.ix.Mapping.resolve(field)
	if !ok {
		return bleve.NewMatchNoneQuery(), nil
	}
	path := qb.ix.Mapping.searchPath(field)
	var q query.Query
	switch {
	case f.Type == TypeObject || f.Type == TypeNested:
		return bleve.NewMatchNoneQuery(), nil
	case f.Type == TypeGeoPoint || f.Type == TypeXYPoint || f.Type == TypeGeoShape || f.Type == TypeXYShape:
		return nil, errCreateQueryCause(errIllegalArgument("Field [%s] of type [%s] does not support range queries", field, f.Type))
	case isRangeType(f.Type):
		if err := searchableError(field, f); err != nil {
			return nil, err
		}
		var df *DateFormat
		if fmtStr := getString(spec, "format"); fmtStr != "" {
			if df = ParseDateFormat(fmtStr); df.Err() != nil {
				return nil, parseFailure(asError(df.Err()))
			}
		}
		rq, err := qb.rangeFieldQuery(field, path, f, lower, upper, includeLower, includeUpper, getString(spec, "relation"), df, loc)
		if err != nil {
			return nil, err
		}
		q = rq
	case f.isNumeric():
		if err := searchableError(field, f); err != nil {
			return nil, err
		}
		nq, err := numericRangeQuery(path, f, lower, upper, includeLower, includeUpper)
		if err != nil {
			return nil, err
		}
		q = nq
	case f.isDate():
		if !fieldIndexed(f) {
			return bleve.NewMatchNoneQuery(), nil
		}
		df := f.Format
		if fmtStr := getString(spec, "format"); fmtStr != "" {
			df = ParseDateFormat(fmtStr)
			if df.Err() != nil {
				// the format is checked while the query is parsed
				return nil, parseFailure(asError(df.Err()))
			}
		}
		var lo, hi *time.Time
		if lower != nil {
			t, err := qb.dateQueryBound(f, df, lower, !includeLower, loc)
			if err != nil {
				return nil, err
			}
			if !includeLower {
				t = adjustDateBound(f, t, true)
			}
			lo = &t
		}
		if upper != nil {
			t, err := qb.dateQueryBound(f, df, upper, includeUpper, loc)
			if err != nil {
				return nil, err
			}
			if !includeUpper {
				t = adjustDateBound(f, t, false)
			}
			hi = &t
		}
		if lo == nil && hi == nil {
			q = fieldPresenceQuery(path)
		} else {
			q = dateRangeBleveQuery(path, f, lo, hi)
		}
	case f.Type == TypeBoolean:
		if err := searchableError(field, f); err != nil {
			return nil, err
		}
		var min, max string
		if lower != nil {
			term, err := booleanQueryTerm(lower)
			if err != nil {
				return nil, err
			}
			min = term
		}
		if upper != nil {
			term, err := booleanQueryTerm(upper)
			if err != nil {
				return nil, err
			}
			max = term
		}
		if lower == nil && upper == nil {
			q = fieldPresenceQuery(path)
		} else {
			tq := bleve.NewTermRangeInclusiveQuery(min, max, &includeLower, &includeUpper)
			tq.SetField(path)
			q = tq
		}
	case f.Type == TypeIP:
		if err := searchableError(field, f); err != nil {
			return nil, err
		}
		iq, err := ipRangeQuery(path, lower, upper, includeLower, includeUpper)
		if err != nil {
			return nil, err
		}
		q = iq
	default:
		if err := searchableError(field, f); err != nil {
			return nil, err
		}
		if f.Type == TypeConstantKeyword {
			return bleve.NewMatchNoneQuery(), nil
		}
		str := func(v any) string {
			s := queryValueText(v)
			if f.isKeywordLike() {
				if an, err := qb.ix.analysis.normalizerNamed(f.Normalizer); err == nil {
					if ts := tokens(an, s); len(ts) == 1 {
						s = ts[0]
					}
				}
			}
			return s
		}
		if lower == nil && upper == nil {
			q = fieldPresenceQuery(path)
			break
		}
		var min, max string
		if lower != nil {
			min = str(lower)
		}
		if upper != nil {
			max = str(upper)
		}
		tq := bleve.NewTermRangeInclusiveQuery(min, max, &includeLower, &includeUpper)
		tq.SetField(path)
		q = tq
	}
	return setBoost(q, boost), nil
}

// existsQuery is existsFieldQuery for callers that do not report errors.
func (qb *queryBuilder) existsQuery(field string) query.Query {
	q, err := qb.existsFieldQuery(field)
	if err != nil {
		return bleve.NewMatchNoneQuery()
	}
	return q
}

// existsFieldQuery builds an exists query: documents that index a value
// (or keep doc values) for the field, a field pattern or an object.
func (qb *queryBuilder) existsFieldQuery(field string) (query.Query, error) {
	if field == "" {
		return nil, errIllegalArgument("field name is null or empty")
	}
	var names []string
	if strings.ContainsAny(field, "*?") {
		// a pattern expands to the matching field types, metadata fields
		// included, and runs their exists queries in hash set order
		seen := map[string]bool{}
		for _, meta := range metaFieldNames {
			if wildcardMatch(field, meta) {
				seen[meta] = true
				names = append(names, meta)
			}
		}
		for _, lf := range qb.ix.Mapping.leafFields(field) {
			if !seen[lf] {
				seen[lf] = true
				names = append(names, lf)
			}
		}
		names = javaHashMapOrder(names)
		var objects []string
		for _, p := range qb.ix.Mapping.objectPaths() {
			if wildcardMatch(field, p) && !seen[p] {
				seen[p] = true
				objects = append(objects, p)
			}
		}
		sortStrings(objects)
		names = append(names, objects...)
	} else {
		names = []string{field}
	}
	var subs []query.Query
	for _, name := range names {
		if isMetaFieldName(name) {
			mq, err := qb.metaExistsQuery(name)
			if err != nil {
				return nil, err
			}
			if mq != nil {
				subs = append(subs, mq)
			}
			continue
		}
		f, base, ok := qb.ix.Mapping.resolve(name)
		if !ok {
			continue
		}
		switch f.Type {
		case TypeConstantKeyword:
			// every document has the value (the other fields of a pattern
			// still build their queries)
			subs = append(subs, bleve.NewMatchAllQuery())
			continue
		case TypeRankFeatures:
			return nil, errCreateQueryCause(errIllegalArgument("[rank_features] fields do not support [exists] queries"))
		}
		presence := qb.ix.Mapping.searchPath(name)
		if f.saytPrefix {
			// the prefix field type of search_as_you_type fields has no exists query
			return nil, &Error{Status: 400, Type: "query_shard_exception", Reason: "failed to create query: null",
				Cause: &Error{Type: "unsupported_operation_exception", nullReason: true}}
		}
		if f.shingles > 0 {
			// shingle subfields exist where their search_as_you_type field does
			presence = base
		}
		tq := bleve.NewTermQuery(presence)
		tq.SetField("_exists_")
		subs = append(subs, tq)
	}
	switch len(subs) {
	case 0:
		return bleve.NewMatchNoneQuery(), nil
	case 1:
		return subs[0], nil
	}
	return bleve.NewDisjunctionQuery(subs...), nil
}

// constant score wrapper ------------------------------------------------

// constantScoreQuery wraps a query and replaces the scores of its matches.
type constantScoreQuery struct {
	inner query.Query
	score float64
}

func (q *constantScoreQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	s, err := q.inner.Searcher(ctx, i, m, options)
	if err != nil {
		return nil, err
	}
	return &constantScoreSearcher{Searcher: s, score: q.score}, nil
}

type constantScoreSearcher struct {
	search.Searcher
	score float64
}

// Weight and SetQueryNorm keep filters out of the query normalization of
// enclosing conjunctions so they do not change the scores of other clauses.
func (s *constantScoreSearcher) Weight() float64 { return 0 }

func (s *constantScoreSearcher) SetQueryNorm(float64) {}

func (s *constantScoreSearcher) Next(ctx *search.SearchContext) (*search.DocumentMatch, error) {
	dm, err := s.Searcher.Next(ctx)
	if dm != nil {
		dm.Score = s.score
		dm.Expl = nil
	}
	return dm, err
}

func (s *constantScoreSearcher) Advance(ctx *search.SearchContext, id index.IndexInternalID) (*search.DocumentMatch, error) {
	dm, err := s.Searcher.Advance(ctx, id)
	if dm != nil {
		dm.Score = s.score
		dm.Expl = nil
	}
	return dm, err
}
