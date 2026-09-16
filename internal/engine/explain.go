package engine

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Score explanations (Weight.explain) of the _explain API and of search hits
// with explain, and the query profile of searches with profile. Constant
// score queries and their boolean combinations are explained as Lucene
// does; the details of similarity scores are not reproduced.

// explainer explains a query for the documents of one index.
type explainer struct {
	qb       *queryBuilder
	d        *luceneDescriber
	cache    map[*qnode]map[string]evaluated
	ordinals map[string][2]int64
}

func (c *Cluster) newExplainer(ix *Index) *explainer {
	qb := &queryBuilder{c: c, ix: ix}
	return &explainer{qb: qb, d: &luceneDescriber{qb: qb, rewrite: true, plain: true}, cache: map[*qnode]map[string]evaluated{}}
}

// matches evaluates a query node on the index.
func (e *explainer) matches(n *qnode) map[string]evaluated {
	if res, ok := e.cache[n]; ok {
		return res
	}
	var res map[string]evaluated
	if q, err := e.qb.toQuery(n); err == nil {
		res, _ = e.qb.evaluate(q)
	}
	e.cache[n] = res
	return res
}

func explanation(value float64, description string, details ...M) M {
	list := make([]any, 0, len(details))
	for _, d := range details {
		list = append(list, d)
	}
	return M{"value": Float(float32(value)), "description": description, "details": list}
}

// luceneDocID is the Lucene doc id of a document within its shard.
func (e *explainer) luceneDocID(d *Doc) int64 {
	if e.ordinals == nil {
		e.ordinals = shardDocOrdinals(e.qb.ix)
	}
	return e.ordinals[d.rootDoc().ID][1]
}

// explain explains the score of doc for the query node n.
func (e *explainer) explain(n *qnode, doc *Doc) M {
	res := e.matches(n)
	ev, matched := res[doc.ID]
	switch spec := n.spec.(type) {
	case *boolSpec:
		if out := e.explainBool(n, spec, doc, matched, ev.score); out != nil {
			return out
		}
	case *constantScoreSpec:
		// ConstantScore of a constant score query is the inner query
		_ = spec
	}
	if n.kind == "match_none" {
		return explanation(0, "User requested \"match_none\" query.")
	}
	desc := e.d.describeQuery(&qnode{kind: n.kind, spec: n.spec, boost: 1})
	if !matched {
		return explanation(0, desc+" doesn't match id "+strconv.FormatInt(e.luceneDocID(doc), 10))
	}
	if n.boost != 1 {
		desc += "^" + javaNumberString(float64(float32(n.boost)), 32)
	}
	return explanation(ev.score, desc)
}

func (e *explainer) explainBool(n *qnode, spec *boolSpec, doc *Doc, matched bool, score float64) M {
	type clause struct {
		occur string
		q     *qnode
	}
	var clauses []clause
	for _, q := range spec.must {
		clauses = append(clauses, clause{"+", q})
	}
	for _, q := range spec.mustNot {
		clauses = append(clauses, clause{"-", q})
	}
	for _, q := range spec.should {
		clauses = append(clauses, clause{"", q})
	}
	for _, q := range spec.filter {
		clauses = append(clauses, clause{"#", q})
	}
	if len(clauses) == 0 {
		return nil
	}
	pureNegative := len(spec.must)+len(spec.should)+len(spec.filter) == 0 && spec.adjustPureNegative
	if len(clauses) == 1 && n.boost == 1 {
		c := clauses[0]
		switch c.occur {
		case "+", "":
			// a single scoring clause is rewritten to the clause itself
			return e.explain(c.q, doc)
		case "#":
			inner := e.d.describeQuery(c.q)
			for strings.HasPrefix(inner, "ConstantScore(") && strings.HasSuffix(inner, ")") && balancedInner(inner[len("ConstantScore("):len(inner)-1]) {
				inner = inner[len("ConstantScore(") : len(inner)-1]
			}
			if !matched {
				return explanation(0, "ConstantScore("+inner+")^0.0 doesn't match id "+strconv.FormatInt(e.luceneDocID(doc), 10))
			}
			return explanation(0, "ConstantScore("+inner+")^0.0")
		}
	}
	var details []M
	var failed []M
	sum := 0.0
	for _, c := range clauses {
		sub := e.explain(c.q, doc)
		_, clauseMatches := e.matches(c.q)[doc.ID]
		desc := e.d.describeQuery(c.q)
		switch c.occur {
		case "+", "":
			if clauseMatches {
				details = append(details, sub)
				v, _ := toFloat(sub["value"])
				sum += v
			} else if c.occur == "+" {
				failed = append(failed, explanation(0, "no match on required clause ("+desc+")", sub))
			}
		case "#":
			if clauseMatches {
				filter := explanation(1, strings.TrimSuffix(desc, "^0.0"))
				details = append(details, explanation(0, "match on required clause, product of:", explanation(0, "# clause"), filter))
			} else {
				failed = append(failed, explanation(0, "no match on required clause ("+desc+")", sub))
			}
		case "-":
			if clauseMatches {
				failed = append(failed, explanation(0, "match on prohibited clause ("+desc+")", sub))
			}
		}
	}
	if pureNegative {
		details = append(details, explanation(0, "match on required clause, product of:", explanation(0, "# clause"), explanation(1, "*:*")))
	}
	if !matched {
		if len(failed) > 0 {
			return explanation(0, "Failure to meet condition(s) of required/prohibited clause(s)", failed...)
		}
		return explanation(0, "No matching clauses", details...)
	}
	out := explanation(sum, "sum of:", details...)
	if n.boost != 1 {
		return explanation(sum*n.boost, "product of:", out, explanation(n.boost, "boost"))
	}
	return out
}

// hitExplanation is the _explanation of a search hit.
func (c *Cluster) hitExplanation(h *hit, sr *searchRequest, cache map[*Index]*explainer) M {
	q := sr.query
	if q == nil {
		q = M{"match_all": M{}}
	}
	n, err := parseQuery(q)
	if err != nil {
		return explanation(h.score, "*:*")
	}
	e := cache[h.ix]
	if e == nil {
		e = c.newExplainer(h.ix)
		cache[h.ix] = e
	}
	return e.explain(n, h.doc)
}

// Explain implements GET/POST /{index}/_explain/{id}.
func (c *Cluster) Explain(indexName, id string, raw []byte, p Params) (Response, error) {
	var q any
	switch {
	case len(raw) > 0:
		content, perr := validateQueryContent(raw)
		if perr != nil {
			return fail(perr)
		}
		q = content
	case p.Has("q"):
		queryString := M{"query": p.Get("q")}
		for param, key := range map[string]string{"df": "default_field", "default_operator": "default_operator", "analyzer": "analyzer"} {
			if value := p.Get(param); value != "" {
				queryString[key] = value
			}
		}
		q = M{"query_string": queryString}
	}
	if q == nil {
		return fail(errActionRequestValidation("query is missing"))
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	ix, err := c.resolveDocIndex(indexName)
	if err != nil {
		return fail(err)
	}
	n, perr := parseQuery(q)
	if perr != nil {
		return fail(perr)
	}
	doc := ix.docs[id]
	if doc == nil || doc.nested != nil {
		return Response{Status: http.StatusNotFound, Body: M{"_index": ix.Name, "_id": id, "matched": false}}, nil
	}
	e := c.newExplainer(ix)
	if _, err := e.qb.toQuery(n); err != nil {
		if qe, isErr := err.(*Error); isErr && qe.Type == "query_shard_exception" && qe.Index == "" {
			qe.Index = ix.Name
		}
		return fail(err)
	}
	_, matched := e.matches(n)[doc.ID]
	body := M{"_index": ix.Name, "_id": id, "matched": matched, "explanation": e.explain(n, doc)}
	if sourceParamsSet(p) || p.Has("stored_fields") {
		get := M{"_seq_no": doc.SeqNo, "_primary_term": doc.PrimaryTerm, "found": true}
		sf := sourceFilterFromParams(p)
		if !p.Has("stored_fields") || sourceParamsSet(p) {
			if !sf.disabled {
				src, _ := applySourceFilters(doc.Src, mappingSourceFilter(ix.Mapping), sf)
				if src != nil {
					get["_source"] = src
				}
			}
		}
		body["get"] = get
	}
	return ok(body)
}

// profiles ----------------------------------------------------------------------------

func zeroBreakdown(keys ...string) M {
	out := M{}
	for _, k := range keys {
		out[k] = 0
		out[k+"_count"] = 0
	}
	return out
}

// luceneQueryType is the class name of a described query.
func luceneQueryType(desc string) string {
	switch {
	case strings.HasPrefix(desc, "ConstantScore("):
		return "ConstantScoreQuery"
	case strings.HasPrefix(desc, "IndexOrDocValuesQuery("):
		return "IndexOrDocValuesQuery"
	case strings.HasPrefix(desc, "MatchNoDocsQuery("):
		return "MatchNoDocsQuery"
	case desc == "*:*":
		return "MatchAllDocsQuery"
	case strings.HasPrefix(desc, "_id:("):
		return "TermInSetQuery"
	case strings.HasSuffix(desc, "^0.0") || (strings.HasPrefix(desc, "(") && strings.Contains(desc, ")^")):
		return "BoostQuery"
	case strings.HasPrefix(desc, "(") && strings.Contains(desc, " | "):
		return "DisjunctionMaxQuery"
	case strings.ContainsAny(desc, " "):
		return "BooleanQuery"
	case strings.Contains(desc, ":\""):
		return "PhraseQuery"
	case strings.Contains(desc, ":/"):
		return "RegexpQuery"
	case strings.HasSuffix(desc, "*"):
		return "PrefixQuery"
	}
	return "TermQuery"
}

// profileSection is the profile of a search: one entry per searched shard.
func (c *Cluster) profileSection(ts []target, sr *searchRequest, live map[*Index]bool) M {
	targets := make([]target, 0, len(ts))
	seen := map[*Index]bool{}
	for _, t := range ts {
		if live[t.ix] && !seen[t.ix] {
			seen[t.ix] = true
			targets = append(targets, t)
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ix.Name < targets[j].ix.Name })
	shards := make([]any, 0, len(targets))
	for _, t := range targets {
		desc := "*:*"
		if sr.query != nil {
			if n, err := parseQuery(sr.query); err == nil {
				desc = (&luceneDescriber{qb: &queryBuilder{c: c, ix: t.ix}, rewrite: true, plain: true}).describeQuery(n)
			}
		}
		collector := M{"name": "TopScoreDocCollector", "reason": "search_top_hits", "time_in_nanos": 0}
		switch {
		case sr.size == 0:
			collector = M{"name": "EarlyTerminatingCollector", "reason": "search_count", "time_in_nanos": 0}
		case sr.explicitSort:
			collector["name"] = "SimpleFieldCollector"
		}
		fetch := []any{}
		if sr.size > 0 {
			fetch = append(fetch, M{"type": "fetch", "description": "fetch", "time_in_nanos": 0,
				"breakdown": zeroBreakdown("build_sub_phase_processors", "create_stored_fields_visitor", "get_next_reader", "load_source", "load_stored_fields")})
		}
		query := M{"type": luceneQueryType(desc), "description": desc, "time_in_nanos": 0,
			"breakdown": zeroBreakdown("advance", "build_scorer", "compute_max_score", "create_weight", "match", "next_doc", "score", "set_min_competitive_score", "shallow_advance")}
		for _, s := range sr.includedShards(t.ix) {
			shards = append(shards, M{
				"id":                              "[" + osmemNodeID + "][" + t.ix.Name + "][" + strconv.Itoa(s) + "]",
				"inbound_network_time_in_millis":  0,
				"outbound_network_time_in_millis": 0,
				"searches":                        []any{M{"query": []any{query}, "rewrite_time": 0, "collector": []any{collector}}},
				"aggregations":                    []any{},
				"fetch":                           fetch,
			})
		}
	}
	return M{"shards": shards}
}
