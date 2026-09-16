package engine

import (
	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"
	index "github.com/blevesearch/bleve_index_api"
)

// span_term, span_near and span_multi, built on the same positional
// machinery as the intervals query (query_interval.go): each clause
// compiles to an intervalNode leaf and span_near's slop/in_order become a
// combo node's maxGaps/ordered, then intervalsQuery evaluates it. Only
// these three span types are implemented; span_or, span_first, span_not,
// span_containing, span_within and (field_)masking_span remain in
// unsupportedQueries.

type spanTermSpec struct {
	field string
	value string
}

func parseSpanTerm(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	var field, value string
	for _, k := range queryKeys(m) {
		v := m[k]
		if field != "" {
			return nil, pParsing("[span_term] query doesn't support multiple fields, found [%s] and [%s]", field, k)
		}
		field = k
		if obj, isObj := v.(M); isObj {
			for _, pk := range queryKeys(obj) {
				pv := obj[pk]
				switch pk {
				case "value", "term":
					value = xText(pv)
				case "boost":
					f, err := xFloat(pv)
					if err != nil {
						return nil, err
					}
					n.boost = f
				case "_name":
					n.name = xText(pv)
				default:
					return nil, pParsing("[span_term] query does not support [%s]", pk)
				}
			}
		} else {
			value = xText(v)
		}
	}
	if field == "" {
		return nil, pIllegalArgument("field name is null or empty")
	}
	n.spec = &spanTermSpec{field: field, value: value}
	return n, nil
}

type spanNearSpec struct {
	clauses []*qnode
	slop    int
	inOrder bool
}

func parseSpanNear(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &spanNearSpec{inOrder: true}
	hasClauses := false
	for _, k := range queryKeys(m) {
		v := m[k]
		switch k {
		case "clauses":
			arr, isArr := v.([]any)
			if !isArr {
				return nil, pParsing("[span_near] clauses must be an array")
			}
			hasClauses = true
			for _, e := range arr {
				cn, err := parseQuery(e)
				if err != nil {
					return nil, err
				}
				spec.clauses = append(spec.clauses, cn)
			}
		case "slop":
			nv, err := xInt(v)
			if err != nil {
				return nil, err
			}
			spec.slop = nv
		case "in_order":
			b, err := xBool(v)
			if err != nil {
				return nil, err
			}
			spec.inOrder = b
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			n.boost = f
		case "_name":
			n.name = xText(v)
		default:
			return nil, pParsing("[span_near] query does not support [%s]", k)
		}
	}
	if !hasClauses {
		return nil, pIllegalArgument("[span_near] must include [clauses]")
	}
	n.spec = spec
	return n, nil
}

// spanMultiSpec wraps a prefix, wildcard, fuzzy or regexp query (span_multi
// only supports leaf multi-term queries).
type spanMultiSpec struct {
	inner *qnode
}

func parseSpanMulti(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	matchBody, ok := m["match"]
	if !ok {
		return nil, pIllegalArgument("[span_multi] must have [match] child clause")
	}
	if len(m) > 1 {
		return nil, pParsing("[span_multi] query does not support multiple fields")
	}
	inner, err := parseQuery(matchBody)
	if err != nil {
		return nil, err
	}
	switch inner.kind {
	case "prefix", "wildcard", "fuzzy", "regexp":
	default:
		return nil, pIllegalArgument("[span_multi] query only supports leaf query clauses of type: [prefix, fuzzy, wildcard, regexp, term, range]")
	}
	n.spec = &spanMultiSpec{inner: inner}
	return n, nil
}

// build ------------------------------------------------------------------

func (qb *queryBuilder) spanTermToQuery(spec *spanTermSpec) (query.Query, error) {
	f, _, mapped := qb.ix.Mapping.resolve(spec.field)
	if !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	path := qb.ix.Mapping.searchPath(spec.field)
	term := qb.normalizeForField(f, spec.value)
	return &intervalsQuery{ix: qb.ix, field: path, f: f, root: &intervalNode{kind: "leaf", terms: []string{term}}}, nil
}

func (qb *queryBuilder) spanMultiToQuery(spec *spanMultiSpec) (query.Query, error) {
	field, leaf, err := qb.spanMultiLeaf(spec)
	if err != nil {
		return nil, err
	}
	f, _, mapped := qb.ix.Mapping.resolve(field)
	if !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	path := qb.ix.Mapping.searchPath(field)
	return &intervalsQuery{ix: qb.ix, field: path, f: f, root: leaf}, nil
}

func (qb *queryBuilder) spanNearToQuery(spec *spanNearSpec) (query.Query, error) {
	if len(spec.clauses) == 0 {
		return bleve.NewMatchNoneQuery(), nil
	}
	children := make([]*intervalNode, len(spec.clauses))
	var field string
	for i, c := range spec.clauses {
		fld, leaf, err := qb.spanClauseLeaf(c)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			field = fld
		} else if fld != field {
			return nil, errQueryShard("[span_near] clauses must have the same field")
		}
		children[i] = leaf
	}
	f, _, mapped := qb.ix.Mapping.resolve(field)
	if !mapped {
		return bleve.NewMatchNoneQuery(), nil
	}
	path := qb.ix.Mapping.searchPath(field)
	root := &intervalNode{kind: "combo", children: children, ordered: spec.inOrder, maxGaps: spec.slop}
	return &intervalsQuery{ix: qb.ix, field: path, f: f, root: root}, nil
}

// spanClauseLeaf compiles one span_near clause (span_term or span_multi)
// into an intervalNode leaf, returning the (unresolved) field it runs on.
func (qb *queryBuilder) spanClauseLeaf(clause *qnode) (string, *intervalNode, error) {
	switch spec := clause.spec.(type) {
	case *spanTermSpec:
		f, _, mapped := qb.ix.Mapping.resolve(spec.field)
		if !mapped {
			return spec.field, &intervalNode{kind: "leaf"}, nil
		}
		return spec.field, &intervalNode{kind: "leaf", terms: []string{qb.normalizeForField(f, spec.value)}}, nil
	case *spanMultiSpec:
		return qb.spanMultiLeaf(spec)
	}
	return "", nil, errUnsupported("[" + clause.kind + "] query")
}

// spanMultiLeaf builds the leaf of a span_multi clause, whose "match" child
// is a normal field-keyed prefix/wildcard/fuzzy/regexp query.
func (qb *queryBuilder) spanMultiLeaf(spec *spanMultiSpec) (string, *intervalNode, error) {
	inner := spec.inner
	switch is := inner.spec.(type) {
	case *multiTermSpec:
		f, _, mapped := qb.ix.Mapping.resolve(is.field)
		if !mapped {
			return is.field, &intervalNode{kind: "leaf"}, nil
		}
		path := qb.ix.Mapping.searchPath(is.field)
		switch inner.kind {
		case "prefix":
			text, ci := qb.normalizeForField(f, is.value), is.caseInsensitive
			return is.field, &intervalNode{kind: "leaf", expand: func(i index.IndexReader) ([]string, error) {
				if !ci {
					return dictTerms(i, path, text)
				}
				terms, err := dictTerms(i, path, "")
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
			}}, nil
		case "wildcard":
			tokens := compileWildcard(qb.normalizeWildcard(f, is.value))
			literal := wildcardLiteralPrefix(tokens)
			ci := is.caseInsensitive
			return is.field, &intervalNode{kind: "leaf", expand: func(i index.IndexReader) ([]string, error) {
				start := literal
				if ci {
					start = ""
				}
				terms, err := dictTerms(i, path, start)
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
			}}, nil
		default: // regexp
			value := qb.normalizeForField(f, is.value)
			re, msg := compileLuceneRegexp(value, is.flags, is.caseInsensitive)
			if msg != "" {
				return "", nil, errCreateQuery("illegal_argument_exception", msg)
			}
			return is.field, &intervalNode{kind: "leaf", expand: func(i index.IndexReader) ([]string, error) {
				terms, err := dictTerms(i, path, "")
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
			}}, nil
		}
	case *fuzzySpec:
		f, _, mapped := qb.ix.Mapping.resolve(is.field)
		if !mapped {
			return is.field, &intervalNode{kind: "leaf"}, nil
		}
		path := qb.ix.Mapping.searchPath(is.field)
		fz := is.fuzziness
		if fz == nil {
			fz = &fuzzinessSpec{auto: true, low: 3, high: 6}
		}
		term := qb.normalizeForField(f, xText(is.value))
		maxEdits := fz.distance(term)
		return is.field, &intervalNode{kind: "leaf", expand: func(i index.IndexReader) ([]string, error) {
			terms, _, err := fuzzyCandidateTerms(i, path, term, maxEdits, is.prefixLength, is.maxExpansions, is.transpositions)
			return terms, err
		}}, nil
	}
	return "", nil, errUnsupported("[" + inner.kind + "] query")
}
