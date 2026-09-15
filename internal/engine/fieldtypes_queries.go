package engine

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
	index "github.com/blevesearch/bleve_index_api"
)

// Queries on rank_feature, rank_features and join fields.

// rank_feature ---------------------------------------------------------------------

// rankFeatureSpec is a parsed rank_feature query (RankFeatureQueryBuilder).
type rankFeatureSpec struct {
	field    string
	function string   // saturation (the default), log, sigmoid or linear
	pivot    *float32 // saturation (computed when missing) and sigmoid
	scaling  float32  // log
	exponent float32  // sigmoid
}

var rankFeatureParams = []string{"field", "boost", "_name", "log", "saturation", "sigmoid", "linear"}

// parseRankFeature is the ConstructingObjectParser "feature" of the query.
func parseRankFeature(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &rankFeatureSpec{}
	hasField, functions := false, 0
	for _, k := range objectKeys(m) {
		v := m[k]
		switch k {
		case "field", "_name":
			s, ok := v.(string)
			if !ok {
				return nil, pXContent("[feature] %s doesn't support values of type: %s", k, xTokenName(v)).at(valueTok(m, k))
			}
			if k == "field" {
				spec.field, hasField = s, true
			} else {
				n.name = s
			}
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, xFieldFailure("feature", m, k, err)
			}
			n.boost = float64(float32(f))
		case "log", "saturation", "sigmoid", "linear":
			inner, ok := v.(M)
			if !ok {
				return nil, pXContent("[feature] %s doesn't support values of type: %s", k, xTokenName(v)).at(valueTok(m, k))
			}
			if err := parseFeatureFunction(k, inner, spec); err != nil {
				return nil, xFieldFailure("feature", m, k, err)
			}
			spec.function = k
			functions++
		default:
			return nil, pXContent("[feature] unknown field [%s]%s", k, didYouMean(k, rankFeatureParams)).at(keyTok(m, k)).atParser(valueTok(m, k))
		}
	}
	if !hasField {
		return nil, pIllegalArgument("Required [field]")
	}
	if functions > 1 {
		return nil, withCause(pXContent("Failed to build [feature] after last required field arrived"),
			errIllegalArgument("Can only specify one of [log], [saturation], [sigmoid] and [linear]"))
	}
	if spec.function == "" {
		spec.function = "saturation"
	}
	n.spec = spec
	return n, nil
}

// xFieldFailure is ObjectParser's "failed to parse field", located where the
// parser stopped inside the field.
func xFieldFailure(kind string, m M, field string, cause *Error) *Error {
	return withCause(pXContent("[%s] failed to parse field [%s]", kind, field), cause).atCause(valueEndTok(m, field))
}

// parseFeatureFunction parses the object of a score function.
func parseFeatureFunction(kind string, m M, spec *rankFeatureSpec) *Error {
	params := map[string][]string{"log": {"scaling_factor"}, "saturation": {"pivot"}, "sigmoid": {"pivot", "exponent"}}[kind]
	values := map[string]float32{}
	for _, k := range objectKeys(m) {
		if !slices.Contains(params, k) {
			return pXContent("[%s] unknown field [%s]%s", kind, k, didYouMean(k, params)).at(keyTok(m, k)).atParser(valueTok(m, k))
		}
		f, err := xFloat(m[k])
		if err != nil {
			return xFieldFailure(kind, m, k, err)
		}
		values[k] = float32(f)
	}
	var missing []string
	for _, p := range params {
		if _, ok := values[p]; !ok && (kind != "saturation") {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return pIllegalArgument("Required [%s]", strings.Join(missing, ", "))
	}
	if p, ok := values["pivot"]; ok {
		spec.pivot = &p
	}
	spec.scaling, spec.exponent = values["scaling_factor"], values["exponent"]
	return nil
}

// FeatureField keeps 9 significant bits of a feature value as the term
// frequency of the feature term.
var (
	maxFeatureFreq  = math.Float32bits(math.MaxFloat32) >> 15
	maxFeatureValue = math.Float32frombits(maxFeatureFreq << 15)
)

func encodeFeatureValue(v float32) uint32 {
	if v > maxFeatureValue {
		v = maxFeatureValue
	}
	return math.Float32bits(v) >> 15
}

func decodeFeatureFreq(freq float32) float32 {
	if freq > float32(maxFeatureFreq) {
		return maxFeatureValue
	}
	return math.Float32frombits(uint32(freq) << 15)
}

// featureNumber reads a feature value of a document.
func featureNumber(v any) (float32, bool) {
	switch t := v.(type) {
	case json.Number:
		d, err := javaParseDouble(t.String(), 32)
		if err != nil {
			return 0, false
		}
		return float32(d), true
	case string:
		d, err := javaParseDouble(t, 32)
		if err != nil {
			return 0, false
		}
		return float32(d), true
	}
	return 0, false
}

func (qb *queryBuilder) rankFeatureToQuery(spec *rankFeatureSpec) (query.Query, error) {
	path, feature := spec.field, ""
	positive := true
	f, _, ok := qb.ix.Mapping.resolve(spec.field)
	switch {
	case ok && f.Type == TypeRankFeature:
		if qb.ix.Mapping.searchPath(spec.field) != spec.field {
			// the feature term is the requested name, which an alias does not index
			return bleve.NewMatchNoneQuery(), nil
		}
		positive = getBool(f.Extra, "positive_score_impact", true)
	case ok:
		return nil, errCreateQuery("illegal_argument_exception", "[rank_feature] query only works on [rank_feature] fields and features of [rank_features] fields, not ["+f.typeName()+"]")
	default:
		dot := strings.LastIndexByte(spec.field, '.')
		if dot < 0 {
			return bleve.NewMatchNoneQuery(), nil
		}
		pf, _, pok := qb.ix.Mapping.resolve(spec.field[:dot])
		if !pok || pf.Type != TypeRankFeatures {
			return bleve.NewMatchNoneQuery(), nil
		}
		path, feature = spec.field[:dot], spec.field[dot+1:]
		positive = getBool(pf.Extra, "positive_score_impact", true)
	}
	invalid := func(v float32) bool { return v <= 0 || math.IsInf(float64(v), 0) || math.IsNaN(float64(v)) }
	switch spec.function {
	case "log":
		if !positive {
			return nil, errCreateQuery("illegal_argument_exception", "Cannot use the [log] function with a field that has a negative score impact as it would trigger negative scores")
		}
		if spec.scaling < 1 || math.IsInf(float64(spec.scaling), 0) || math.IsNaN(float64(spec.scaling)) {
			return nil, errCreateQuery("illegal_argument_exception", "scalingFactor must be >= 1, got: "+javaFloatText(float64(spec.scaling), 32))
		}
	case "saturation", "sigmoid":
		if spec.pivot != nil && invalid(*spec.pivot) {
			return nil, errCreateQuery("illegal_argument_exception", "pivot must be > 0, got: "+javaFloatText(float64(*spec.pivot), 32))
		}
		if spec.function == "sigmoid" && invalid(spec.exponent) {
			return nil, errCreateQuery("illegal_argument_exception", "exp must be > 0, got: "+javaFloatText(float64(spec.exponent), 32))
		}
	}
	value := func(d *Doc) (float32, bool) {
		raw, found := lookupPathFound(d.Src, path)
		if !found {
			return 0, false
		}
		if feature != "" {
			obj, isObj := raw.(M)
			if !isObj {
				return 0, false
			}
			if raw, found = obj[feature]; !found {
				return 0, false
			}
		}
		v, ok := featureNumber(raw)
		if !ok {
			return 0, false
		}
		if !positive {
			v = 1 / v
		}
		if featureValueError(v, spec.field, "_feature") != nil {
			return 0, false
		}
		return v, true
	}
	var pivot float32
	if spec.pivot != nil {
		pivot = *spec.pivot
	} else if spec.function == "saturation" {
		// FeatureField.computePivotFeatureValue: the decoded average frequency
		pivot = 1
		var total float64
		count := 0
		for _, d := range qb.ix.docs {
			if v, ok := value(d); ok {
				total += float64(encodeFeatureValue(v))
				count++
			}
		}
		if count > 0 {
			pivot = decodeFeatureFreq(float32(total / float64(count)))
		}
	}
	pivotPa := math.Pow(float64(pivot), float64(spec.exponent))
	ix := qb.ix
	return &docFuncQuery{inner: fieldPresenceQuery(path), fn: func(id string, _ float64) (float64, bool) {
		d := ix.docByBleveID(id)
		if d == nil {
			return 0, false
		}
		v, ok := value(d)
		if !ok {
			return 0, false
		}
		f := decodeFeatureFreq(float32(encodeFeatureValue(v)))
		switch spec.function {
		case "log":
			return float64(float32(math.Log(float64(spec.scaling + f)))), true
		case "sigmoid":
			return float64(float32(1 - pivotPa/(math.Pow(float64(f), float64(spec.exponent))+pivotPa))), true
		case "linear":
			return float64(f), true
		}
		return float64(1 - pivot/(f+pivot)), true
	}}, nil
}

// join fields ------------------------------------------------------------------------

// joinField returns the join field of a mapping (a root field, at most one).
func (m *Mapping) joinField() (string, *Field) {
	for _, name := range sortedFieldNames(m.Properties) {
		if f := m.Properties[name]; f.Type == TypeJoin {
			return name, f
		}
	}
	return "", nil
}

// joinParentOfChild returns the parent relation of a child relation.
func joinParentOfChild(f *Field, child string) (string, bool) {
	parents, children := joinRelationsOf(f)
	for _, p := range parents {
		if slices.Contains(children[p], child) {
			return p, true
		}
	}
	return "", false
}

// joinDocRelation reads the relation name and the parent id of a document.
func joinDocRelation(d *Doc, field string) (name, parent string, ok bool) {
	raw, found := lookupPathFound(d.Src, field)
	if !found {
		return "", "", false
	}
	switch t := raw.(type) {
	case string:
		return t, "", true
	case M:
		name, ok = joinText(t["name"])
		parent, _ = joinText(t["parent"])
		return name, parent, ok
	}
	return "", "", false
}

func joinText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return javaJSONNumberText(t), true
	case []any:
		// the join mapper keeps the last value
		for i := len(t) - 1; i >= 0; i-- {
			if s, ok := joinText(t[i]); ok {
				return s, true
			}
		}
	}
	return "", false
}

func joinTermQuery(field, value string) query.Query {
	tq := bleve.NewTermQuery(value)
	tq.SetField(field)
	return tq
}

// parent_id ----------------------------------------------------------------------------

type parentIDSpec struct {
	typ, id        *string
	ignoreUnmapped bool
}

func parseParentID(body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &parentIDSpec{}
	for _, k := range queryKeys(m) {
		v := m[k]
		if _, isObj := v.(M); isObj {
			return nil, pParsing("[parent_id] query does not support [%s]", k).at(valueTok(m, k))
		}
		if _, isArr := v.([]any); isArr {
			return nil, pParsing("[parent_id] query does not support [%s]", k).at(valueTok(m, k))
		}
		switch k {
		case "type", "id":
			if v == nil {
				continue
			}
			s := xText(v)
			if k == "type" {
				spec.typ = &s
			} else {
				spec.id = &s
			}
		case "ignore_unmapped":
			b, err := xBool(v)
			if err != nil {
				return nil, err
			}
			spec.ignoreUnmapped = b
		case "boost":
			f, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			n.boost = f
		case "_name":
			n.name = xText(v)
		default:
			return nil, pParsing("[parent_id] query does not support [%s]", k).at(valueTok(m, k))
		}
	}
	if n.boost < 0 {
		return nil, negativeBoostError("parent_id", body)
	}
	n.spec = spec
	return n, nil
}

func (qb *queryBuilder) parentIDToQuery(spec *parentIDSpec) (query.Query, error) {
	field, jf := qb.ix.Mapping.joinField()
	if jf == nil {
		if spec.ignoreUnmapped {
			return bleve.NewMatchNoneQuery(), nil
		}
		return nil, errQueryShard("[parent_id] no join field found for index [%s]", qb.ix.Name)
	}
	typ := "null"
	if spec.typ != nil {
		typ = *spec.typ
	}
	parent, ok := joinParentOfChild(jf, typ)
	if spec.typ == nil || !ok {
		if spec.ignoreUnmapped {
			return bleve.NewMatchNoneQuery(), nil
		}
		return nil, errQueryShard("[parent_id] no relation found for child [%s]", typ)
	}
	if spec.id == nil {
		return nil, errShardFailure(500, "null_pointer_exception", "Cannot invoke \"org.apache.lucene.util.BytesRef.compareTo(org.apache.lucene.util.BytesRef)\" because \"target\" is null")
	}
	// the term query on the parent id field scores with BM25 on a field
	// indexed without frequencies and norms
	_, children := joinRelationsOf(jf)
	docCount, docFreq := 0, 0
	for _, d := range qb.ix.docs {
		name, pid, ok := joinDocRelation(d, field)
		if !ok {
			continue
		}
		value := pid
		switch {
		case name == parent:
			value = d.ID
		case !slices.Contains(children[parent], name):
			continue
		}
		docCount++
		if value == *spec.id {
			docFreq++
		}
	}
	idf := float32(math.Log(1 + (float64(docCount)-float64(docFreq)+0.5)/(float64(docFreq)+0.5)))
	normInverse := float32(1) / (float32(1.2) * ((1 - float32(0.75)) + float32(0.75)))
	score := float64(idf - idf/(1+normInverse))
	conj := &luceneBoolQuery{must: []query.Query{joinTermQuery(field+"#"+parent, *spec.id)}, filter: []query.Query{joinTermQuery(field, typ)}}
	return &docFuncQuery{inner: conj, fn: func(string, float64) (float64, bool) { return score, true }}, nil
}

// has_child and has_parent ------------------------------------------------------------

type joinQuerySpec struct {
	kind           string // has_child or has_parent
	typ            string // the child type or the parent type
	query          *qnode
	scoreMode      string
	minChildren    int
	maxChildren    int
	score          bool
	ignoreUnmapped bool
	innerHits      bool
}

func parseHasChild(body any) (*qnode, *Error)  { return parseJoinQuery("has_child", body) }
func parseHasParent(body any) (*qnode, *Error) { return parseJoinQuery("has_parent", body) }

// parseJoinQuery is HasChildQueryBuilder.fromXContent and
// HasParentQueryBuilder.fromXContent.
func parseJoinQuery(kind string, body any) (*qnode, *Error) {
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &joinQuerySpec{kind: kind, scoreMode: "none", minChildren: 1, maxChildren: math.MaxInt32}
	typeKey := "type"
	if kind == "has_parent" {
		typeKey = "parent_type"
	}
	var typ *string
	var handle func(k string, v any, at *tokenRef) *Error
	handle = func(k string, v any, at *tokenRef) *Error {
		switch t := v.(type) {
		case []any:
			for i, e := range t {
				if err := handle(k, e, elemTok(t, i)); err != nil {
					return err
				}
			}
			return nil
		case M:
			switch k {
			case "query":
				q, err := parseQuery(t)
				if err != nil {
					return err
				}
				spec.query = q
			case "inner_hits":
				spec.innerHits = true
			default:
				return pParsing("[%s] query does not support [%s]", kind, k).at(at)
			}
			return nil
		case nil:
			return nil
		}
		switch {
		case k == typeKey:
			s := xText(v)
			typ = &s
		case k == "score_mode" && kind == "has_child":
			s := xText(v)
			switch s {
			case "none", "min", "avg", "max", "sum":
				spec.scoreMode = s
			default:
				return pIllegalArgument("No score mode for child query [%s] found", s)
			}
		case (k == "min_children" || k == "max_children") && kind == "has_child":
			i, err := xInt(v)
			if err != nil {
				return err
			}
			if k == "min_children" {
				spec.minChildren = i
			} else {
				spec.maxChildren = i
			}
		case k == "score" && kind == "has_parent":
			b, err := xBool(v)
			if err != nil {
				return err
			}
			spec.score = b
		case k == "ignore_unmapped":
			b, err := xBool(v)
			if err != nil {
				return err
			}
			spec.ignoreUnmapped = b
		case k == "boost":
			f, err := xFloat(v)
			if err != nil {
				return err
			}
			n.boost = f
		case k == "_name":
			n.name = xText(v)
		default:
			return pParsing("[%s] query does not support [%s]", kind, k).at(at)
		}
		return nil
	}
	for _, k := range queryKeys(m) {
		if err := handle(k, m[k], valueTok(m, k)); err != nil {
			return nil, err
		}
	}
	if typ == nil {
		return nil, pIllegalArgument("[%s] requires '%s' field", kind, typeKey)
	}
	if spec.query == nil {
		return nil, pIllegalArgument("[%s] requires 'query' field", kind)
	}
	spec.typ = *typ
	if kind == "has_child" {
		switch {
		case spec.minChildren < 0:
			return nil, pIllegalArgument("[has_child] requires non-negative 'min_children' field")
		case spec.maxChildren < 0:
			return nil, pIllegalArgument("[has_child] requires non-negative 'max_children' field")
		case spec.maxChildren < spec.minChildren:
			return nil, pIllegalArgument("[has_child] 'max_children' is less than 'min_children'")
		}
	}
	if n.boost < 0 {
		return nil, negativeBoostError(kind, body)
	}
	n.spec = spec
	return n, nil
}

func (qb *queryBuilder) joinQueryToQuery(spec *joinQuerySpec) (query.Query, error) {
	field, jf := qb.ix.Mapping.joinField()
	if jf == nil {
		if spec.ignoreUnmapped {
			return bleve.NewMatchNoneQuery(), nil
		}
		return nil, errQueryShard("[%s] no join field has been configured", spec.kind)
	}
	parents, children := joinRelationsOf(jf)
	var parent string
	if spec.kind == "has_child" {
		p, ok := joinParentOfChild(jf, spec.typ)
		if !ok {
			if spec.ignoreUnmapped {
				return bleve.NewMatchNoneQuery(), nil
			}
			return nil, errQueryShard("[has_child] join field [%s] doesn't hold [%s] as a child", field, spec.typ)
		}
		parent = p
	} else if !slices.Contains(parents, spec.typ) {
		if spec.ignoreUnmapped {
			return bleve.NewMatchNoneQuery(), nil
		}
		return nil, errQueryShard("[has_parent] join field [%s] doesn't hold [%s] as a parent", field, spec.typ)
	}
	if spec.innerHits {
		return nil, errUnsupported("[inner_hits] of [" + spec.kind + "] queries")
	}
	inner, err := qb.toQuery(spec.query)
	if err != nil {
		return nil, err
	}
	if spec.kind == "has_child" {
		max := spec.maxChildren
		if max == 0 {
			max = math.MaxInt32
		}
		return &hasChildQuery{ix: qb.ix, field: field, childType: spec.typ, parentType: parent, inner: inner,
			scoreMode: spec.scoreMode, minChildren: spec.minChildren, maxChildren: max}, nil
	}
	return &hasParentQuery{ix: qb.ix, field: field, parentType: spec.typ, childTypes: children[spec.typ], inner: inner, score: spec.score}, nil
}

// hasChildQuery matches the parents of the child documents matching a query,
// scored by the scores of their matching children (JoinUtil).
type hasChildQuery struct {
	ix                       *Index
	field                    string
	childType, parentType    string
	inner                    query.Query
	scoreMode                string
	minChildren, maxChildren int
}

func joinDoc(ix *Index, i index.IndexReader, id index.IndexInternalID) *Doc {
	ext, err := i.ExternalID(id)
	if err != nil {
		return nil
	}
	return ix.docByBleveID(ext)
}

func (q *hasChildQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	children := &luceneBoolQuery{must: []query.Query{q.inner}, filter: []query.Query{joinTermQuery(q.field, q.childType)}}
	searchers, err := childSearchers(ctx, i, m, options, []query.Query{children, joinTermQuery(q.field, q.parentType)})
	if err != nil {
		return nil, err
	}
	return newEvalSearcher(searchers, func(_ *search.SearchContext, res [][]docEntry) ([]docEntry, error) {
		type group struct {
			n             int
			sum, min, max float32
		}
		groups := map[string]*group{}
		for _, e := range res[0] {
			d := joinDoc(q.ix, i, e.id)
			if d == nil {
				continue
			}
			_, parent, ok := joinDocRelation(d, q.field)
			if !ok || parent == "" {
				continue
			}
			s := float32(e.score)
			g := groups[parent]
			if g == nil {
				g = &group{min: s, max: s}
				groups[parent] = g
			}
			g.n++
			g.sum += s
			g.min = min(g.min, s)
			g.max = max(g.max, s)
		}
		var out []docEntry
		for _, e := range res[1] {
			d := joinDoc(q.ix, i, e.id)
			if d == nil {
				continue
			}
			g := groups[d.ID]
			if g == nil || g.n < q.minChildren || g.n > q.maxChildren {
				continue
			}
			score := float32(1)
			switch q.scoreMode {
			case "max":
				score = g.max
			case "min":
				score = g.min
			case "sum":
				score = g.sum
			case "avg":
				score = g.sum / float32(g.n)
			}
			out = append(out, docEntry{id: e.id, score: float64(score)})
		}
		return out, nil
	}), nil
}

// hasParentQuery matches the children of the parent documents matching a
// query, scored 1 or by the score of their parent.
type hasParentQuery struct {
	ix         *Index
	field      string
	parentType string
	childTypes []string
	inner      query.Query
	score      bool
}

func (q *hasParentQuery) Searcher(ctx context.Context, i index.IndexReader, m mapping.IndexMapping, options search.SearcherOptions) (search.Searcher, error) {
	parents := &luceneBoolQuery{must: []query.Query{q.inner}, filter: []query.Query{joinTermQuery(q.field, q.parentType)}}
	var kinds []query.Query
	for _, c := range q.childTypes {
		kinds = append(kinds, joinTermQuery(q.field, c))
	}
	searchers, err := childSearchers(ctx, i, m, options, []query.Query{parents, &luceneBoolQuery{should: kinds}})
	if err != nil {
		return nil, err
	}
	return newEvalSearcher(searchers, func(_ *search.SearchContext, res [][]docEntry) ([]docEntry, error) {
		scores := map[string]float32{}
		for _, e := range res[0] {
			if d := joinDoc(q.ix, i, e.id); d != nil {
				scores[d.ID] = float32(e.score)
			}
		}
		var out []docEntry
		for _, e := range res[1] {
			d := joinDoc(q.ix, i, e.id)
			if d == nil {
				continue
			}
			_, parent, ok := joinDocRelation(d, q.field)
			s, matched := scores[parent]
			if !ok || !matched {
				continue
			}
			if !q.score {
				s = 1
			}
			out = append(out, docEntry{id: e.id, score: float64(s)})
		}
		return out, nil
	}), nil
}
