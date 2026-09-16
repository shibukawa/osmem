package engine

import (
	"sort"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
	painlessscript "github.com/shibukawa/painlessscript-go"
)

// scriptEvalOrder returns docs's ids sorted, so a script error is reported
// for the same document on every run (map iteration order is randomized).
func scriptEvalOrder(docs map[string]evaluated) []string {
	ids := make([]string, 0, len(docs))
	for id := range docs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// script query: {"script": {"script": {...}}}. A boolean filter, scored
// like other term-level queries (constant 1 x boost, applied by toQuery).

type scriptQuerySpec struct {
	script *scriptSpec
}

func parseScriptQuery(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &scriptQuerySpec{}
	for _, k := range queryKeys(m) {
		v := m[k]
		switch k {
		case "script":
			sc, err := parseScript(bodyReader{}, v, m, k, k)
			if err != nil {
				return nil, err.(*Error)
			}
			spec.script = sc
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			n.boost = f
		case "_name":
			n.name = xText(v)
		default:
			return nil, pParsing("[script] query does not support [%s]", k).at(valueTok(m, k))
		}
	}
	if spec.script == nil {
		return nil, pIllegalArgument("script cannot be null")
	}
	if n.boost < 0 {
		return nil, negativeBoostError("script", body)
	}
	n.spec = spec
	return n, nil
}

func (qb *queryBuilder) scriptQueryToQuery(spec *scriptQuerySpec) (query.Query, error) {
	if _, _, cerr := spec.script.compile(painlessscript.ContextFilter); cerr != nil {
		return nil, cerr
	}
	docs, err := qb.evaluate(bleve.NewMatchAllQuery())
	if err != nil {
		return nil, err
	}
	scores := map[string]float64{}
	for _, id := range scriptEvalOrder(docs) {
		d := qb.ix.docByExternalID(id)
		if d == nil {
			continue
		}
		v, serr := evalDocScript(spec.script, painlessscript.ContextFilter, qb.ix, d, docs[id].score)
		if serr != nil {
			return nil, serr
		}
		if ok, _ := v.Bool(); ok {
			scores[id] = 1
		}
	}
	return &presetQuery{scores: scores}, nil
}

// script_score query: {"script_score": {"query": ..., "script": ...,
// "min_score": ...}}, distinct from function_score's script_score function
// (query_function_score.go).

type scriptScoreQuerySpec struct {
	query    *qnode
	script   *scriptSpec
	minScore *float64
}

func parseScriptScoreQuery(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &scriptScoreQuerySpec{}
	for _, k := range queryKeys(m) {
		v := m[k]
		switch k {
		case "query":
			q, err := parseQuery(v)
			if err != nil {
				return nil, err
			}
			spec.query = q
		case "script":
			sc, err := parseScript(bodyReader{}, v, m, k, k)
			if err != nil {
				return nil, err.(*Error)
			}
			spec.script = sc
		case "min_score":
			f, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			spec.minScore = &f
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			n.boost = f
		case "_name":
			n.name = xText(v)
		default:
			return nil, pParsing("[script_score] query does not support [%s]", k).at(valueTok(m, k))
		}
	}
	if spec.query == nil {
		return nil, pIllegalArgument("query must be provided")
	}
	if spec.script == nil {
		return nil, pIllegalArgument("script must be provided")
	}
	if n.boost < 0 {
		return nil, negativeBoostError("script_score", body)
	}
	n.spec = spec
	return n, nil
}

func (qb *queryBuilder) scriptScoreQueryToQuery(spec *scriptScoreQuerySpec) (query.Query, error) {
	inner, err := qb.toQuery(spec.query)
	if err != nil {
		return nil, err
	}
	if _, _, cerr := spec.script.compile(painlessscript.ContextScore); cerr != nil {
		return nil, cerr
	}
	docs, err := qb.evaluate(inner)
	if err != nil {
		return nil, err
	}
	scores := map[string]float64{}
	ftls := map[string][]search.FieldTermLocation{}
	for _, id := range scriptEvalOrder(docs) {
		e := docs[id]
		d := qb.ix.docByExternalID(id)
		if d == nil {
			continue
		}
		v, serr := evalDocScript(spec.script, painlessscript.ContextScore, qb.ix, d, e.score)
		if serr != nil {
			return nil, serr
		}
		f, _ := v.Float64()
		if spec.minScore != nil && f < *spec.minScore {
			continue
		}
		scores[id] = f
		if len(e.ftls) > 0 {
			ftls[id] = e.ftls
		}
	}
	return &presetQuery{scores: scores, ftls: ftls}, nil
}
