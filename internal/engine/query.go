package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
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
}

func errQueryShard(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "query_shard_exception", Reason: fmt.Sprintf(format, args...)}
}

// build converts a query object ({"match": {...}}).
func (qb *queryBuilder) build(v any) (query.Query, error) {
	m, ok := v.(M)
	if !ok {
		return nil, errParsing("[query] query malformed, expected object")
	}
	if len(m) == 0 {
		return nil, errParsing("query malformed, empty clause found at [query]")
	}
	if len(m) > 1 {
		return nil, errParsing("[bool] malformed query, expected [END_OBJECT] but found [FIELD_NAME]")
	}
	var kind string
	var body any
	for k, b := range m {
		kind, body = k, b
	}
	switch kind {
	case "match_all":
		q := bleve.NewMatchAllQuery()
		if bm, ok := body.(M); ok {
			q.SetBoost(getFloat(bm, "boost", 1))
		}
		return q, nil
	case "match_none":
		return bleve.NewMatchNoneQuery(), nil
	case "match":
		return qb.matchQuery(body, matchOpts{})
	case "match_phrase":
		return qb.matchPhraseQuery(body, false)
	case "match_phrase_prefix":
		return qb.matchPhraseQuery(body, true)
	case "match_bool_prefix":
		return qb.matchQuery(body, matchOpts{lastPrefix: true})
	case "multi_match":
		return qb.multiMatchQuery(body)
	case "term":
		return qb.termQuery(body)
	case "terms":
		return qb.termsQuery(body)
	case "range":
		return qb.rangeQuery(body)
	case "exists":
		bm, _ := body.(M)
		return qb.existsQuery(getString(bm, "field")), nil
	case "prefix":
		return qb.prefixQuery(body)
	case "wildcard":
		return qb.wildcardQuery(body)
	case "regexp":
		return qb.regexpQuery(body)
	case "fuzzy":
		return qb.fuzzyQuery(body)
	case "ids":
		bm, _ := body.(M)
		var vals []string
		for _, v := range getList(bm["values"]) {
			if s, err := stringValue("_id", &Field{Type: TypeKeyword}, v); err == nil && s != "" {
				vals = append(vals, s)
			}
		}
		if len(vals) == 0 {
			return bleve.NewMatchNoneQuery(), nil
		}
		return bleve.NewDocIDQuery(vals), nil
	case "bool":
		return qb.boolQuery(body)
	case "constant_score":
		bm, _ := body.(M)
		inner, err := qb.build(bm["filter"])
		if err != nil {
			return nil, err
		}
		return &constantScoreQuery{inner: inner, score: getFloat(bm, "boost", 1)}, nil
	case "dis_max":
		bm, _ := body.(M)
		dq := bleve.NewDisjunctionQuery()
		for _, sub := range getList(bm["queries"]) {
			q, err := qb.build(sub)
			if err != nil {
				return nil, err
			}
			dq.AddQuery(q)
		}
		dq.SetBoost(getFloat(bm, "boost", 1))
		return dq, nil
	case "boosting":
		bm, _ := body.(M)
		qb.c.warn("boosting query: negative clause is ignored")
		return qb.build(bm["positive"])
	case "function_score", "script_score":
		bm, _ := body.(M)
		qb.c.warn("%s query: scoring functions are ignored", kind)
		if inner, ok := bm["query"]; ok {
			return qb.build(inner)
		}
		return bleve.NewMatchAllQuery(), nil
	case "nested":
		return qb.nestedQuery(body)
	case "query_string":
		return qb.queryStringQuery(body, false)
	case "simple_query_string":
		return qb.queryStringQuery(body, true)
	case "geo_distance":
		return qb.geoDistanceQuery(body)
	case "geo_bounding_box":
		return qb.geoBoundingBoxQuery(body)
	case "wrapper":
		bm, _ := body.(M)
		raw, err := base64.StdEncoding.DecodeString(getString(bm, "query"))
		if err != nil {
			return nil, errParsing("wrapper query: %v", err)
		}
		inner, err := decodeObject(raw)
		if err != nil {
			return nil, err
		}
		return qb.build(inner)
	case "terms_set", "script", "knn", "neural", "more_like_this", "percolate", "has_child", "has_parent", "parent_id",
		"span_term", "span_near", "span_first", "span_or", "span_not", "span_multi", "span_containing", "span_within", "span_field_masking",
		"rank_feature", "pinned", "intervals", "distance_feature", "geo_shape", "geo_polygon", "hybrid", "match_only_text":
		return nil, errUnsupported("[" + kind + "] query")
	}
	return nil, errParsing("unknown query [%s]", kind)
}

// fieldAndSpec unpacks {"field": "value"} / {"field": {"value": ...}} forms.
func fieldAndSpec(body any, valueKeys ...string) (field string, spec M, value any, err error) {
	bm, ok := body.(M)
	if !ok || len(bm) == 0 {
		return "", nil, nil, errParsing("query malformed, expected object with a field")
	}
	for k, v := range bm {
		switch k {
		case "boost", "_name":
			continue
		}
		field = k
		if sm, ok := v.(M); ok {
			spec = sm
			for _, vk := range valueKeys {
				if val, ok := sm[vk]; ok {
					value = val
					break
				}
			}
		} else {
			spec = M{}
			value = v
		}
		break
	}
	if field == "" {
		return "", nil, nil, errParsing("query malformed, no field specified")
	}
	if b, ok := bm["boost"]; ok {
		spec["boost"] = b
	}
	return field, spec, value, nil
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

// analyzerFor returns the search analyzer of a field.
func (qb *queryBuilder) analyzerFor(f *Field, override string) (analysis.Analyzer, error) {
	if override != "" {
		return qb.ix.analysis.analyzerNamed(override)
	}
	if f == nil {
		return qb.ix.analysis.analyzerNamed("standard")
	}
	if f.isKeywordLike() {
		return qb.ix.analysis.normalizerNamed(f.Normalizer)
	}
	name := f.SearchAnalyzer
	if name == "" {
		name = f.Analyzer
	}
	return qb.ix.analysis.analyzerNamed(name)
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
// against a typed field.
func (qb *queryBuilder) exactTermQuery(field string, f *Field, value any) (query.Query, error) {
	if field == "_id" {
		s, _ := stringValue(field, &Field{Type: TypeKeyword}, value)
		return bleve.NewDocIDQuery([]string{s}), nil
	}
	if field == "_index" {
		s, _ := stringValue(field, &Field{Type: TypeKeyword}, value)
		if s == qb.ix.Name || matchAny([]string{s}, qb.ix.Name) {
			return bleve.NewMatchAllQuery(), nil
		}
		return bleve.NewMatchNoneQuery(), nil
	}
	if f == nil {
		return bleve.NewMatchNoneQuery(), nil
	}
	switch {
	case f.isNumeric():
		n, ok := toFloat(value)
		if !ok {
			return nil, errQueryShard("failed to create query: For input string: \"%v\"", value)
		}
		min, max, inc1, inc2 := n, n, true, true
		q := bleve.NewNumericRangeInclusiveQuery(&min, &max, &inc1, &inc2)
		q.SetField(field)
		return q, nil
	case f.isDate():
		// a term on a date covers the whole interval the value denotes
		// ("2024-01" is the month), like OpenSearch's date termQuery
		var lo, hi time.Time
		if s, ok := value.(string); ok {
			var err error
			if lo, err = ParseDateMath(s, f.Format, qb.c.now(), time.UTC, false); err != nil {
				return nil, errQueryShard("failed to create query: %v", err)
			}
			if hi, err = ParseDateMath(s, f.Format, qb.c.now(), time.UTC, true); err != nil {
				return nil, errQueryShard("failed to create query: %v", err)
			}
		} else {
			t, err := f.Format.Parse(value)
			if err != nil {
				return nil, errQueryShard("failed to create query: %v", err)
			}
			lo, hi = t, t
		}
		inc1, inc2 := true, true
		q := bleve.NewDateRangeInclusiveQuery(lo, hi, &inc1, &inc2)
		q.SetField(field)
		return q, nil
	case f.Type == TypeBoolean:
		b, ok := boolValue(value)
		if !ok {
			return nil, errQueryShard("failed to create query: Can't parse boolean value [%v], expected [true] or [false]", value)
		}
		q := bleve.NewBoolFieldQuery(b)
		q.SetField(field)
		return q, nil
	case f.Type == TypeObject, f.Type == TypeNested, f.Type == TypeGeoPoint:
		return bleve.NewMatchNoneQuery(), nil
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
		tq.SetField(field)
		return tq, nil
	default:
		s, err := stringValue(field, f, value)
		if err != nil {
			return nil, err
		}
		tq := bleve.NewTermQuery(s)
		tq.SetField(field)
		return tq, nil
	}
}

func (qb *queryBuilder) termQuery(body any) (query.Query, error) {
	field, spec, value, err := fieldAndSpec(body, "value", "query", "term")
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errIllegalArgument("value cannot be null")
	}
	f, _, _ := qb.ix.Mapping.resolve(field)
	if getBool(spec, "case_insensitive", false) && f != nil && f.isKeywordLike() {
		s, _ := stringValue(field, f, value)
		rq := bleve.NewRegexpQuery("(?i)" + regexpEscape(s))
		rq.SetField(field)
		return setBoost(rq, getFloat(spec, "boost", 1)), nil
	}
	q, err := qb.exactTermQuery(field, f, value)
	if err != nil {
		return nil, err
	}
	return setBoost(q, getFloat(spec, "boost", 1)), nil
}

func (qb *queryBuilder) termsQuery(body any) (query.Query, error) {
	bm, ok := body.(M)
	if !ok {
		return nil, errParsing("[terms] query malformed")
	}
	boost := getFloat(bm, "boost", 1)
	var field string
	var values []any
	for k, v := range bm {
		if k == "boost" || k == "_name" {
			continue
		}
		field = k
		switch t := v.(type) {
		case []any:
			values = t
		case M:
			// terms lookup
			vals, err := qb.termsLookup(t)
			if err != nil {
				return nil, err
			}
			values = vals
		default:
			return nil, errParsing("[terms] query does not support [%s]", k)
		}
	}
	if field == "" {
		return nil, errParsing("[terms] query malformed, no field specified")
	}
	f, _, _ := qb.ix.Mapping.resolve(field)
	if len(values) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	dq := bleve.NewDisjunctionQuery()
	for _, v := range values {
		if v == nil {
			continue
		}
		q, err := qb.exactTermQuery(field, f, v)
		if err != nil {
			return nil, err
		}
		dq.AddQuery(q)
	}
	return setBoost(dq, boost), nil
}

func (qb *queryBuilder) termsLookup(spec M) ([]any, error) {
	idxName := getString(spec, "index")
	id := getString(spec, "id")
	path := getString(spec, "path")
	if idxName == "" || id == "" || path == "" {
		return nil, errParsing("[terms] query lookup element requires specifying the index, id and path")
	}
	ix, err := qb.c.resolveWriteIndex(idxName)
	if err != nil {
		return nil, err
	}
	d := ix.docs[id]
	if d == nil {
		return nil, nil
	}
	return flattenValues(lookupPath(d.Src, path)), nil
}

type matchOpts struct {
	lastPrefix bool
}

func (qb *queryBuilder) matchQuery(body any, opts matchOpts) (query.Query, error) {
	field, spec, value, err := fieldAndSpec(body, "query")
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errParsing("[match] requires query value")
	}
	return qb.matchOnField(field, value, spec, opts)
}

func (qb *queryBuilder) matchOnField(field string, value any, spec M, opts matchOpts) (query.Query, error) {
	boost := getFloat(spec, "boost", 1)
	f, _, ok := qb.ix.Mapping.resolve(field)
	if field == "_id" || field == "_index" {
		q, err := qb.exactTermQuery(field, f, value)
		if err != nil {
			return nil, err
		}
		return setBoost(q, boost), nil
	}
	if !ok {
		return bleve.NewMatchNoneQuery(), nil
	}
	if f.isNumeric() || f.isDate() || f.Type == TypeBoolean || f.isKeywordLike() {
		if f.isKeywordLike() {
			s, err := stringValue(field, f, value)
			if err != nil {
				return nil, err
			}
			value = s
		}
		q, err := qb.exactTermQuery(field, f, value)
		if err != nil {
			if getBool(spec, "lenient", false) {
				return bleve.NewMatchNoneQuery(), nil
			}
			return nil, err
		}
		return setBoost(q, boost), nil
	}
	text, err := stringValue(field, f, value)
	if err != nil {
		return nil, err
	}
	an, err := qb.analyzerFor(f, getString(spec, "analyzer"))
	if err != nil {
		return nil, err
	}
	terms := tokens(an, text)
	if len(terms) == 0 {
		if getString(spec, "zero_terms_query") == "all" {
			return bleve.NewMatchAllQuery(), nil
		}
		return bleve.NewMatchNoneQuery(), nil
	}
	fuzz := getString(spec, "fuzziness")
	prefixLen := getInt(spec, "prefix_length", 0)
	var subs []query.Query
	for i, t := range terms {
		if opts.lastPrefix && i == len(terms)-1 {
			pq := bleve.NewPrefixQuery(t)
			pq.SetField(field)
			subs = append(subs, pq)
			continue
		}
		subs = append(subs, termOrFuzzy(field, t, fuzz, prefixLen))
	}
	if len(subs) == 1 {
		return setBoost(subs[0], boost), nil
	}
	operator := strings.ToLower(getString(spec, "operator"))
	msm := getString(spec, "minimum_should_match")
	if operator == "and" {
		cq := bleve.NewConjunctionQuery(subs...)
		return setBoost(cq, boost), nil
	}
	dq := bleve.NewDisjunctionQuery(subs...)
	if msm != "" {
		dq.SetMin(float64(minimumShouldMatch(msm, len(subs))))
	}
	return setBoost(dq, boost), nil
}

func termOrFuzzy(field, term, fuzziness string, prefixLen int) query.Query {
	if fuzziness == "" || fuzziness == "0" {
		tq := bleve.NewTermQuery(term)
		tq.SetField(field)
		return tq
	}
	n := fuzzinessValue(fuzziness, term)
	if n == 0 {
		tq := bleve.NewTermQuery(term)
		tq.SetField(field)
		return tq
	}
	fq := bleve.NewFuzzyQuery(term)
	fq.SetField(field)
	fq.SetFuzziness(n)
	fq.SetPrefix(prefixLen)
	return fq
}

func fuzzinessValue(fuzz, term string) int {
	fuzz = strings.ToUpper(strings.TrimSpace(fuzz))
	if strings.HasPrefix(fuzz, "AUTO") {
		low, high := 3, 6
		if rest := strings.TrimPrefix(fuzz, "AUTO"); strings.HasPrefix(rest, ":") {
			parts := strings.Split(rest[1:], ",")
			if len(parts) == 2 {
				low, _ = strconv.Atoi(parts[0])
				high, _ = strconv.Atoi(parts[1])
			}
		}
		n := len([]rune(term))
		switch {
		case n < low:
			return 0
		case n < high:
			return 1
		default:
			return 2
		}
	}
	n, err := strconv.Atoi(fuzz)
	if err != nil {
		f, err := strconv.ParseFloat(fuzz, 64)
		if err != nil {
			return 0
		}
		n = int(f)
	}
	if n > 2 {
		n = 2
	}
	return n
}

// minimumShouldMatch resolves an OpenSearch minimum_should_match spec for a
// number of optional clauses.
func minimumShouldMatch(spec string, clauses int) int {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 1
	}
	// combinations: "3<90%" or "2<-25% 9<-3"
	if strings.Contains(spec, "<") {
		result := 1
		matched := false
		for _, part := range strings.Fields(spec) {
			idx := strings.Index(part, "<")
			if idx < 0 {
				continue
			}
			n, err := strconv.Atoi(part[:idx])
			if err != nil {
				continue
			}
			if clauses > n {
				result = minimumShouldMatch(part[idx+1:], clauses)
				matched = true
			}
		}
		if !matched {
			return clauses
		}
		return result
	}
	if strings.HasSuffix(spec, "%") {
		pct, err := strconv.ParseFloat(strings.TrimSuffix(spec, "%"), 64)
		if err != nil {
			return 1
		}
		if pct < 0 {
			n := clauses - int(math.Floor(float64(clauses)*(-pct)/100))
			return clampMin(n, clauses)
		}
		return clampMin(int(math.Floor(float64(clauses)*pct/100)), clauses)
	}
	n, err := strconv.Atoi(spec)
	if err != nil {
		return 1
	}
	if n < 0 {
		return clampMin(clauses+n, clauses)
	}
	return clampMin(n, clauses)
}

func clampMin(n, clauses int) int {
	if n < 0 {
		return 0
	}
	if n > clauses {
		return clauses
	}
	return n
}

func (qb *queryBuilder) matchPhraseQuery(body any, prefix bool) (query.Query, error) {
	field, spec, value, err := fieldAndSpec(body, "query")
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errParsing("[match_phrase] requires query value")
	}
	return qb.phraseOnField(field, value, spec, prefix)
}

func (qb *queryBuilder) phraseOnField(field string, value any, spec M, prefix bool) (query.Query, error) {
	boost := getFloat(spec, "boost", 1)
	f, _, ok := qb.ix.Mapping.resolve(field)
	if !ok {
		return bleve.NewMatchNoneQuery(), nil
	}
	if f.Type != TypeText && f.Type != TypeSearchAsYouType {
		q, err := qb.exactTermQuery(field, f, value)
		if err != nil {
			return nil, err
		}
		return setBoost(q, boost), nil
	}
	text, err := stringValue(field, f, value)
	if err != nil {
		return nil, err
	}
	an, err := qb.analyzerFor(f, getString(spec, "analyzer"))
	if err != nil {
		return nil, err
	}
	ts := an.Analyze([]byte(text))
	if len(ts) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	if prefix {
		var subs []query.Query
		for i, t := range ts {
			if i == len(ts)-1 {
				pq := bleve.NewPrefixQuery(string(t.Term))
				pq.SetField(field)
				subs = append(subs, pq)
				continue
			}
			tq := bleve.NewTermQuery(string(t.Term))
			tq.SetField(field)
			subs = append(subs, tq)
		}
		if len(subs) == 1 {
			return setBoost(subs[0], boost), nil
		}
		if len(ts) > 1 {
			// phrase part then the prefix: approximate with phrase of the
			// leading terms AND the prefix
			terms := make([][]string, 0, len(ts)-1)
			for _, t := range ts[:len(ts)-1] {
				terms = append(terms, []string{string(t.Term)})
			}
			pq := query.NewMultiPhraseQuery(terms, field)
			cq := bleve.NewConjunctionQuery(pq, subs[len(subs)-1])
			return setBoost(cq, boost), nil
		}
		return setBoost(bleve.NewConjunctionQuery(subs...), boost), nil
	}
	if len(ts) == 1 {
		tq := bleve.NewTermQuery(string(ts[0].Term))
		tq.SetField(field)
		return setBoost(tq, boost), nil
	}
	slop := getInt(spec, "slop", 0)
	if slop > 0 {
		var subs []query.Query
		var terms []string
		for _, t := range ts {
			tq := bleve.NewTermQuery(string(t.Term))
			tq.SetField(field)
			subs = append(subs, tq)
			terms = append(terms, string(t.Term))
		}
		cq := bleve.NewConjunctionQuery(subs...)
		cq.SetBoost(boost)
		return &slopQuery{inner: cq, field: field, terms: terms, slop: slop}, nil
	}
	// group tokens by position to build the multi-phrase
	byPos := map[int][]string{}
	var positions []int
	for _, t := range ts {
		if _, ok := byPos[t.Position]; !ok {
			positions = append(positions, t.Position)
		}
		byPos[t.Position] = append(byPos[t.Position], string(t.Term))
	}
	sort.Ints(positions)
	terms := make([][]string, 0, len(positions))
	for _, p := range positions {
		terms = append(terms, byPos[p])
	}
	pq := query.NewMultiPhraseQuery(terms, field)
	return setBoost(pq, boost), nil
}

func (qb *queryBuilder) multiMatchQuery(body any) (query.Query, error) {
	bm, ok := body.(M)
	if !ok {
		return nil, errParsing("[multi_match] query malformed")
	}
	value, ok := bm["query"]
	if !ok {
		return nil, errParsing("[multi_match] requires query value")
	}
	fields := getStrings(bm, "fields")
	if len(fields) == 0 {
		fields = []string{"*"}
	}
	typ := getString(bm, "type")
	var expanded []string
	boosts := map[string]float64{}
	for _, fspec := range fields {
		name, boost := fspec, 1.0
		if idx := strings.LastIndex(fspec, "^"); idx > 0 {
			name = fspec[:idx]
			boost, _ = strconv.ParseFloat(fspec[idx+1:], 64)
			if boost == 0 {
				boost = 1
			}
		}
		if strings.ContainsAny(name, "*?") {
			for _, lf := range qb.ix.Mapping.leafFields(name) {
				f, _, _ := qb.ix.Mapping.resolve(lf)
				if f != nil && (f.Type == TypeText || f.isKeywordLike()) {
					expanded = append(expanded, lf)
					boosts[lf] = boost
				}
			}
			continue
		}
		expanded = append(expanded, name)
		boosts[name] = boost
	}
	if len(expanded) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	spec := M{}
	for _, k := range []string{"operator", "fuzziness", "prefix_length", "minimum_should_match", "analyzer", "lenient", "zero_terms_query", "slop"} {
		if v, ok := bm[k]; ok {
			spec[k] = v
		}
	}
	if typ == "cross_fields" {
		return qb.crossFieldsQuery(expanded, boosts, value, spec, getFloat(bm, "boost", 1))
	}
	var subs []query.Query
	for _, field := range expanded {
		var q query.Query
		var err error
		switch typ {
		case "phrase":
			q, err = qb.phraseOnField(field, value, spec, false)
		case "phrase_prefix":
			q, err = qb.phraseOnField(field, value, spec, true)
		case "bool_prefix":
			q, err = qb.matchOnField(field, value, spec, matchOpts{lastPrefix: true})
		default:
			q, err = qb.matchOnField(field, value, spec, matchOpts{})
		}
		if err != nil {
			if getBool(bm, "lenient", false) {
				continue
			}
			return nil, err
		}
		if _, none := q.(*query.MatchNoneQuery); none {
			continue
		}
		subs = append(subs, setBoost(q, boosts[field]))
	}
	if len(subs) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	dq := bleve.NewDisjunctionQuery(subs...)
	return setBoost(dq, getFloat(bm, "boost", 1)), nil
}

// crossFieldsQuery implements multi_match type cross_fields: every term
// must appear in at least one of the fields (with operator and), terms
// are combined with the operator across the field group.
func (qb *queryBuilder) crossFieldsQuery(fields []string, boosts map[string]float64, value any, spec M, boost float64) (query.Query, error) {
	text, err := stringValue("", &Field{Type: TypeText}, value)
	if err != nil {
		return nil, err
	}
	var terms []string
	for _, field := range fields {
		f, _, ok := qb.ix.Mapping.resolve(field)
		if !ok || f.isNumeric() || f.isDate() || f.Type == TypeBoolean {
			continue
		}
		an, err := qb.analyzerFor(f, getString(spec, "analyzer"))
		if err != nil {
			return nil, err
		}
		terms = tokens(an, text)
		if len(terms) > 0 {
			break
		}
	}
	if len(terms) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	fuzz := getString(spec, "fuzziness")
	prefixLen := getInt(spec, "prefix_length", 0)
	var perTerm []query.Query
	for _, term := range terms {
		dq := bleve.NewDisjunctionQuery()
		for _, field := range fields {
			f, _, ok := qb.ix.Mapping.resolve(field)
			if !ok || f.isNumeric() || f.isDate() || f.Type == TypeBoolean {
				continue
			}
			var q query.Query
			if f.isKeywordLike() {
				q, err = qb.exactTermQuery(field, f, term)
				if err != nil {
					continue
				}
			} else {
				q = termOrFuzzy(field, term, fuzz, prefixLen)
			}
			dq.AddQuery(setBoost(q, boosts[field]))
		}
		perTerm = append(perTerm, dq)
	}
	if len(perTerm) == 1 {
		return setBoost(perTerm[0], boost), nil
	}
	if strings.EqualFold(getString(spec, "operator"), "and") {
		return setBoost(bleve.NewConjunctionQuery(perTerm...), boost), nil
	}
	dq := bleve.NewDisjunctionQuery(perTerm...)
	if msm := getString(spec, "minimum_should_match"); msm != "" {
		dq.SetMin(float64(minimumShouldMatch(msm, len(perTerm))))
	}
	return setBoost(dq, boost), nil
}

func (qb *queryBuilder) rangeQuery(body any) (query.Query, error) {
	field, spec, _, err := fieldAndSpec(body)
	if err != nil {
		return nil, err
	}
	boost := getFloat(spec, "boost", 1)
	f, _, ok := qb.ix.Mapping.resolve(field)
	if !ok {
		return bleve.NewMatchNoneQuery(), nil
	}
	gt, hasGT := spec["gt"]
	gte, hasGTE := spec["gte"]
	lt, hasLT := spec["lt"]
	lte, hasLTE := spec["lte"]
	if v, ok := spec["from"]; ok && !hasGT && !hasGTE {
		if getBool(spec, "include_lower", true) {
			gte, hasGTE = v, v != nil
		} else {
			gt, hasGT = v, v != nil
		}
	}
	if v, ok := spec["to"]; ok && !hasLT && !hasLTE {
		if getBool(spec, "include_upper", true) {
			lte, hasLTE = v, v != nil
		} else {
			lt, hasLT = v, v != nil
		}
	}
	var q query.Query
	switch {
	case f.isNumeric():
		var min, max *float64
		minInc, maxInc := true, true
		if hasGTE {
			v, ok := toFloat(gte)
			if !ok {
				return nil, errQueryShard("failed to create query: For input string: \"%v\"", gte)
			}
			min = &v
		} else if hasGT {
			v, ok := toFloat(gt)
			if !ok {
				return nil, errQueryShard("failed to create query: For input string: \"%v\"", gt)
			}
			min, minInc = &v, false
		}
		if hasLTE {
			v, ok := toFloat(lte)
			if !ok {
				return nil, errQueryShard("failed to create query: For input string: \"%v\"", lte)
			}
			max = &v
		} else if hasLT {
			v, ok := toFloat(lt)
			if !ok {
				return nil, errQueryShard("failed to create query: For input string: \"%v\"", lt)
			}
			max, maxInc = &v, false
		}
		if min == nil && max == nil {
			return bleve.NewMatchAllQuery(), nil
		}
		q = bleve.NewNumericRangeInclusiveQuery(min, max, &minInc, &maxInc)
	case f.isDate():
		df := f.Format
		if fmtStr := getString(spec, "format"); fmtStr != "" {
			df = ParseDateFormat(fmtStr)
		}
		loc := time.UTC
		if tz := getString(spec, "time_zone"); tz != "" {
			loc, err = parseTimeZone(tz)
			if err != nil {
				return nil, errQueryShard("%v", err)
			}
		}
		parse := func(v any, roundUp bool) (time.Time, error) {
			if s, ok := v.(string); ok {
				return ParseDateMath(s, df, qb.c.now(), loc, roundUp)
			}
			return df.Parse(v)
		}
		var min, max time.Time
		minInc, maxInc := true, true
		if hasGTE {
			if min, err = parse(gte, false); err != nil {
				return nil, errQueryShard("failed to create query: %v", err)
			}
		} else if hasGT {
			if min, err = parse(gt, true); err != nil {
				return nil, errQueryShard("failed to create query: %v", err)
			}
			minInc = false
		}
		if hasLTE {
			if max, err = parse(lte, true); err != nil {
				return nil, errQueryShard("failed to create query: %v", err)
			}
		} else if hasLT {
			if max, err = parse(lt, false); err != nil {
				return nil, errQueryShard("failed to create query: %v", err)
			}
			maxInc = false
		}
		if min.IsZero() && max.IsZero() {
			return bleve.NewMatchAllQuery(), nil
		}
		q = bleve.NewDateRangeInclusiveQuery(min, max, &minInc, &maxInc)
	case f.Type == TypeObject, f.Type == TypeNested, f.Type == TypeBoolean, f.Type == TypeGeoPoint:
		return bleve.NewMatchNoneQuery(), nil
	default:
		var min, max string
		minInc, maxInc := true, true
		str := func(v any) string {
			s, _ := stringValue(field, f, v)
			return s
		}
		if hasGTE {
			min = str(gte)
		} else if hasGT {
			min, minInc = str(gt), false
		}
		if hasLTE {
			max = str(lte)
		} else if hasLT {
			max, maxInc = str(lt), false
		}
		if min == "" && max == "" {
			return bleve.NewMatchAllQuery(), nil
		}
		q = bleve.NewTermRangeInclusiveQuery(min, max, &minInc, &maxInc)
	}
	if fq, ok := q.(query.FieldableQuery); ok {
		fq.SetField(field)
	}
	return setBoost(q, boost), nil
}

func (qb *queryBuilder) existsQuery(field string) query.Query {
	if field == "" {
		return bleve.NewMatchNoneQuery()
	}
	if field == "_id" || field == "_index" {
		return bleve.NewMatchAllQuery()
	}
	_, base, ok := qb.ix.Mapping.resolve(field)
	if !ok {
		return bleve.NewMatchNoneQuery()
	}
	tq := bleve.NewTermQuery(base)
	tq.SetField("_exists_")
	return tq
}

func (qb *queryBuilder) prefixQuery(body any) (query.Query, error) {
	field, spec, value, err := fieldAndSpec(body, "value", "prefix")
	if err != nil {
		return nil, err
	}
	f, _, ok := qb.ix.Mapping.resolve(field)
	if !ok {
		return bleve.NewMatchNoneQuery(), nil
	}
	s, err := stringValue(field, f, value)
	if err != nil {
		return nil, err
	}
	if f.isKeywordLike() {
		an, err := qb.ix.analysis.normalizerNamed(f.Normalizer)
		if err == nil {
			if ts := tokens(an, s); len(ts) == 1 {
				s = ts[0]
			}
		}
	}
	if getBool(spec, "case_insensitive", false) {
		rq := bleve.NewRegexpQuery("(?i)" + regexpEscape(s) + ".*")
		rq.SetField(field)
		return setBoost(rq, getFloat(spec, "boost", 1)), nil
	}
	pq := bleve.NewPrefixQuery(s)
	pq.SetField(field)
	return setBoost(pq, getFloat(spec, "boost", 1)), nil
}

func (qb *queryBuilder) wildcardQuery(body any) (query.Query, error) {
	field, spec, value, err := fieldAndSpec(body, "value", "wildcard")
	if err != nil {
		return nil, err
	}
	f, _, ok := qb.ix.Mapping.resolve(field)
	if !ok {
		return bleve.NewMatchNoneQuery(), nil
	}
	s, err := stringValue(field, f, value)
	if err != nil {
		return nil, err
	}
	if f.isKeywordLike() && f.Normalizer != "" {
		an, err := qb.ix.analysis.normalizerNamed(f.Normalizer)
		if err == nil {
			if ts := tokens(an, s); len(ts) == 1 {
				s = ts[0]
			}
		}
	}
	if getBool(spec, "case_insensitive", false) {
		rq := bleve.NewRegexpQuery("(?i)" + wildcardToRegexp(s))
		rq.SetField(field)
		return setBoost(rq, getFloat(spec, "boost", 1)), nil
	}
	wq := bleve.NewWildcardQuery(s)
	wq.SetField(field)
	return setBoost(wq, getFloat(spec, "boost", 1)), nil
}

func wildcardToRegexp(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteString(".")
		default:
			if strings.ContainsRune(`\^$.|+()[]{}`, r) {
				sb.WriteRune('\\')
			}
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func (qb *queryBuilder) regexpQuery(body any) (query.Query, error) {
	field, spec, value, err := fieldAndSpec(body, "value", "regexp")
	if err != nil {
		return nil, err
	}
	if _, _, ok := qb.ix.Mapping.resolve(field); !ok {
		return bleve.NewMatchNoneQuery(), nil
	}
	s := fmt.Sprint(value)
	if getBool(spec, "case_insensitive", false) {
		s = "(?i)" + s
	}
	rq := bleve.NewRegexpQuery(s)
	rq.SetField(field)
	return setBoost(rq, getFloat(spec, "boost", 1)), nil
}

func (qb *queryBuilder) fuzzyQuery(body any) (query.Query, error) {
	field, spec, value, err := fieldAndSpec(body, "value")
	if err != nil {
		return nil, err
	}
	f, _, ok := qb.ix.Mapping.resolve(field)
	if !ok {
		return bleve.NewMatchNoneQuery(), nil
	}
	s, err := stringValue(field, f, value)
	if err != nil {
		return nil, err
	}
	fuzz := getString(spec, "fuzziness")
	if fuzz == "" {
		fuzz = "AUTO"
	}
	q := termOrFuzzy(field, s, fuzz, getInt(spec, "prefix_length", 0))
	return setBoost(q, getFloat(spec, "boost", 1)), nil
}

func (qb *queryBuilder) boolQuery(body any) (query.Query, error) {
	bm, ok := body.(M)
	if !ok {
		return nil, errParsing("[bool] query malformed")
	}
	bq := bleve.NewBooleanQuery()
	hasMust, hasShould, hasMustNot := false, false, false
	for _, sub := range getList(bm["must"]) {
		q, err := qb.build(sub)
		if err != nil {
			return nil, err
		}
		bq.AddMust(q)
		hasMust = true
	}
	for _, sub := range getList(bm["filter"]) {
		q, err := qb.build(sub)
		if err != nil {
			return nil, err
		}
		bq.AddMust(&constantScoreQuery{inner: q, score: 0})
		hasMust = true
	}
	shouldCount := 0
	for _, sub := range getList(bm["should"]) {
		q, err := qb.build(sub)
		if err != nil {
			return nil, err
		}
		bq.AddShould(q)
		hasShould = true
		shouldCount++
	}
	for _, sub := range getList(bm["must_not"]) {
		q, err := qb.build(sub)
		if err != nil {
			return nil, err
		}
		bq.AddMustNot(q)
		hasMustNot = true
	}
	if !hasMust && !hasShould && !hasMustNot {
		return bleve.NewMatchAllQuery(), nil
	}
	if hasShould {
		msm := getString(bm, "minimum_should_match")
		min := 0
		if msm != "" {
			min = minimumShouldMatch(msm, shouldCount)
		} else if !hasMust {
			min = 1
		}
		if min > 0 {
			bq.SetMinShould(float64(min))
		} else if !hasMust {
			bq.SetMinShould(1)
		}
	}
	return setBoost(bq, getFloat(bm, "boost", 1)), nil
}

func (qb *queryBuilder) geoDistanceQuery(body any) (query.Query, error) {
	bm, ok := body.(M)
	if !ok {
		return nil, errParsing("[geo_distance] query malformed")
	}
	distance := getString(bm, "distance")
	var field string
	var center any
	for k, v := range bm {
		switch k {
		case "distance", "distance_type", "validation_method", "boost", "_name", "ignore_unmapped":
			continue
		}
		field, center = k, v
	}
	if field == "" || distance == "" {
		return nil, errParsing("[geo_distance] requires distance and a field")
	}
	if _, _, ok := qb.ix.Mapping.resolve(field); !ok {
		return bleve.NewMatchNoneQuery(), nil
	}
	lat, lon, ok := geoPointValue(center)
	if !ok {
		return nil, errParsing("[geo_distance] invalid point")
	}
	q := bleve.NewGeoDistanceQuery(lon, lat, distance)
	q.SetField(field)
	return setBoost(q, getFloat(bm, "boost", 1)), nil
}

func (qb *queryBuilder) geoBoundingBoxQuery(body any) (query.Query, error) {
	bm, ok := body.(M)
	if !ok {
		return nil, errParsing("[geo_bounding_box] query malformed")
	}
	var field string
	var spec M
	for k, v := range bm {
		switch k {
		case "validation_method", "type", "boost", "_name", "ignore_unmapped":
			continue
		}
		field = k
		spec, _ = v.(M)
	}
	if field == "" || spec == nil {
		return nil, errParsing("[geo_bounding_box] requires a field")
	}
	if _, _, ok := qb.ix.Mapping.resolve(field); !ok {
		return bleve.NewMatchNoneQuery(), nil
	}
	var tlLat, tlLon, brLat, brLon float64
	if tl, ok := spec["top_left"]; ok {
		tlLat, tlLon, _ = geoPointValue(tl)
		brLat, brLon, _ = geoPointValue(spec["bottom_right"])
	} else {
		tlLat = getFloat(spec, "top", 0)
		tlLon = getFloat(spec, "left", 0)
		brLat = getFloat(spec, "bottom", 0)
		brLon = getFloat(spec, "right", 0)
	}
	q := bleve.NewGeoBoundingBoxQuery(tlLon, tlLat, brLon, brLat)
	q.SetField(field)
	return setBoost(q, getFloat(bm, "boost", 1)), nil
}

// sloppy phrase --------------------------------------------------------

// slopQuery matches documents containing all terms of a phrase within
// `slop` position moves of each other (match_phrase with slop).
type slopQuery struct {
	inner query.Query
	field string
	terms []string
	slop  int
}

func (q *slopQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	options.IncludeTermVectors = true
	s, err := q.inner.Searcher(ctx, i, m, options)
	if err != nil {
		return nil, err
	}
	return &slopSearcher{Searcher: s, q: q}, nil
}

type slopSearcher struct {
	search.Searcher
	q *slopQuery
}

func (s *slopSearcher) matches(dm *search.DocumentMatch) bool {
	positions := make([][]int, len(s.q.terms))
	for _, ftl := range dm.FieldTermLocations {
		if ftl.Field != s.q.field {
			continue
		}
		for i, t := range s.q.terms {
			if ftl.Term == t {
				positions[i] = append(positions[i], int(ftl.Location.Pos))
			}
		}
	}
	for _, p := range positions {
		if len(p) == 0 {
			return false
		}
	}
	// find positions p_i (one per term) such that the spread of p_i - i is
	// within slop, which is the sloppy phrase condition
	var dfs func(i int, min, max int) bool
	dfs = func(i int, min, max int) bool {
		if i == len(positions) {
			return true
		}
		for _, p := range positions[i] {
			v := p - i
			nmin, nmax := min, max
			if i == 0 || v < nmin {
				nmin = v
			}
			if i == 0 || v > nmax {
				nmax = v
			}
			if nmax-nmin > s.q.slop {
				continue
			}
			if dfs(i+1, nmin, nmax) {
				return true
			}
		}
		return false
	}
	return dfs(0, 0, 0)
}

func (s *slopSearcher) Next(ctx *search.SearchContext) (*search.DocumentMatch, error) {
	for {
		dm, err := s.Searcher.Next(ctx)
		if err != nil || dm == nil {
			return dm, err
		}
		if s.matches(dm) {
			return dm, nil
		}
		ctx.DocumentMatchPool.Put(dm)
	}
}

func (s *slopSearcher) Advance(ctx *search.SearchContext, id index.IndexInternalID) (*search.DocumentMatch, error) {
	dm, err := s.Searcher.Advance(ctx, id)
	if err != nil || dm == nil {
		return dm, err
	}
	if s.matches(dm) {
		return dm, nil
	}
	ctx.DocumentMatchPool.Put(dm)
	return s.Next(ctx)
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
