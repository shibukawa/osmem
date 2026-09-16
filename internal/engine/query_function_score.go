package engine

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
	painlessscript "github.com/shibukawa/painlessscript-go"
)

// function_score --------------------------------------------------------------

type functionScoreSpec struct {
	query     *qnode
	functions []*scoreFunctionSpec
	scoreMode string
	boostMode string
	maxBoost  float64
	minScore  *float64
}

type scoreFunctionSpec struct {
	filter *qnode
	weight *float64
	kind   string // weight, field_value_factor, random_score, gauss, exp, linear, script_score
	body   M
	script *scriptSpec // kind == "script_score"
}

var scoreModes = map[string]bool{"FIRST": true, "AVG": true, "MAX": true, "SUM": true, "MIN": true, "MULTIPLY": true}
var boostModes = map[string]bool{"MULTIPLY": true, "REPLACE": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true}

func unknownScoreFunction(m M, name string) *Error {
	return withCause(pParsing("unknown query [function_score] did you mean any of [random_score, script_score]?").at(valueTok(m, name)),
		(&Error{Type: "named_object_not_found_exception", Reason: "unknown field [" + name + "]"}).at(valueTok(m, name)))
}

func parseScoreFunctionBody(m M, name string) (*scoreFunctionSpec, *Error) {
	body, _ := m[name].(M)
	switch name {
	case "field_value_factor":
		for _, k := range objectKeys(body) {
			pv := body[k]
			switch k {
			case "field":
				_ = xText(pv)
			case "factor", "missing":
				if _, err := xFloat(pv); err != nil {
					return nil, err
				}
			case "modifier":
				mod := strings.ToUpper(xText(pv))
				if !fvfModifiers[mod] {
					return nil, pIllegalArgument("No enum constant org.opensearch.common.lucene.search.function.FieldValueFactorFunction.Modifier.%s", mod)
				}
			default:
				return nil, pParsing("field_value_factor query does not support [%s]", k).at(valueTok(body, k))
			}
		}
		if _, ok := body["field"]; !ok {
			return nil, pParsing("[field_value_factor] required field 'field' missing").at(endTok(body))
		}
	case "random_score":
		for _, k := range objectKeys(body) {
			switch k {
			case "seed", "field":
			default:
				return nil, pParsing("random_score query does not support [%s]", k).at(valueTok(body, k))
			}
		}
	case "gauss", "exp", "linear":
		// DecayFunctionParser reads the multi value mode while parsing
		if v, ok := body["multi_value_mode"]; ok {
			if err := checkMultiValueMode(body, v); err != nil {
				return nil, err
			}
		}
	case "script_score":
		var sc *scriptSpec
		for _, k := range objectKeys(body) {
			v := body[k]
			if k != "script" {
				return nil, pParsing("script_score does not support [%s]", k).at(valueTok(body, k))
			}
			parsed, err := parseScript(bodyReader{}, v, body, k, k)
			if err != nil {
				return nil, err.(*Error)
			}
			sc = parsed
		}
		if sc == nil {
			return nil, pParsing("[script_score] required field 'script' missing").at(endTok(body))
		}
		return &scoreFunctionSpec{kind: name, body: body, script: sc}, nil
	default:
		return nil, unknownScoreFunction(m, name)
	}
	return &scoreFunctionSpec{kind: name, body: body}, nil
}

var fvfModifiers = map[string]bool{"NONE": true, "LOG": true, "LOG1P": true, "LOG2P": true, "LN": true, "LN1P": true, "LN2P": true, "SQUARE": true, "SQRT": true, "RECIPROCAL": true}

// multiValueModes are the names of MultiValueMode.
var multiValueModes = map[string]bool{"SUM": true, "AVG": true, "MEDIAN": true, "MIN": true, "MAX": true}

// checkMultiValueMode is MultiValueMode.fromString(parser.text()) on the
// multi_value_mode of a decay function (an object is a field instead).
func checkMultiValueMode(body M, v any) *Error {
	switch v.(type) {
	case M:
		return nil
	case nil, []any:
		token := "VALUE_NULL"
		if v != nil {
			token = "START_ARRAY"
		}
		format := "Can't get text on a " + token + " at %s"
		return parseFailure(&Error{Status: http.StatusInternalServerError, Type: "illegal_state_exception", Reason: strings.TrimSuffix(format, " at %s")}).
			atInReason(valueTok(body, "multi_value_mode"), format)
	}
	if text := xText(v); !multiValueModes[strings.ToUpper(text)] {
		return pIllegalArgument("Illegal sort mode: %s", text).at(valueTok(body, "multi_value_mode"))
	}
	return nil
}

func checkFunctionWeight(w float64) *Error {
	if w < 0 {
		return pIllegalArgument("[weight] cannot be negative for a filtering function")
	}
	return nil
}

func parseFunctionScore(body any) (*qnode, *Error) {
	const misplaced = "you can either define [functions] array or a single function, not both. "
	m := body.(M)
	n := &qnode{boost: 1}
	spec := &functionScoreSpec{scoreMode: "MULTIPLY", maxBoost: math.MaxFloat32}
	singleName := ""
	arrayFound := false
	for _, k := range queryKeys(m) {
		v := m[k]
		switch t := v.(type) {
		case M:
			if k == "query" {
				if spec.query != nil {
					return nil, pParsing("failed to parse [function_score] query. [query] is already defined.")
				}
				q, err := parseQuery(t)
				if err != nil {
					return nil, err
				}
				spec.query = q
				continue
			}
			if singleName != "" {
				return nil, pParsing("failed to parse [function_score] query. already found function [%s], now encountering [%s]. use [functions] array if you want to define several functions.", singleName, k).at(valueTok(m, k))
			}
			if arrayFound {
				return nil, pParsing("failed to parse [function_score] query. [%salready found [functions] array, now encountering [%s].]", misplaced, k).at(valueTok(m, k))
			}
			fn, err := parseScoreFunctionBody(m, k)
			if err != nil {
				return nil, err
			}
			singleName = k
			spec.functions = append(spec.functions, fn)
		case []any:
			if k != "functions" {
				return nil, pParsing("failed to parse [function_score] query. array [%s] is not supported", k).at(valueTok(m, k))
			}
			if singleName != "" {
				return nil, pParsing("failed to parse [function_score] query. [%salready found [%s], now encountering [functions].]", misplaced, singleName).at(valueTok(m, k))
			}
			arrayFound = true
			for i, e := range t {
				em, ok := e.(M)
				if !ok {
					return nil, pParsing("failed to parse [START_OBJECT]. malformed query, expected a [%s] while parsing functions but got a [function_score] instead", xTokenName(e)).
						at(elemTok(t, i))
				}
				fn, err := parseFunctionEntry(em)
				if err != nil {
					return nil, err
				}
				spec.functions = append(spec.functions, fn)
			}
		default:
			if !isXValue(v) {
				continue
			}
			var err *Error
			switch k {
			case "score_mode":
				mode := strings.ToUpper(xText(v))
				if !scoreModes[mode] {
					return nil, pIllegalArgument("No enum constant org.opensearch.common.lucene.search.function.FunctionScoreQuery.ScoreMode.%s", mode)
				}
				spec.scoreMode = mode
			case "boost_mode":
				mode := strings.ToUpper(xText(v))
				if !boostModes[mode] {
					return nil, pIllegalArgument("No enum constant org.opensearch.common.lucene.search.function.CombineFunction.%s", mode)
				}
				spec.boostMode = mode
			case "max_boost":
				spec.maxBoost, err = xFloat(v)
			case "boost":
				n.boost, err = xFloat(v)
			case "_name":
				n.name = xText(v)
			case "min_score":
				var f float64
				f, err = xFloat(v)
				spec.minScore = &f
			case "weight":
				if singleName != "" {
					return nil, pParsing("failed to parse [function_score] query. already found function [%s], now encountering [%s]. use [functions] array if you want to define several functions.", singleName, k).at(valueTok(m, k))
				}
				if arrayFound {
					return nil, pParsing("failed to parse [function_score] query. [%salready found [functions] array, now encountering [%s].]", misplaced, k).at(valueTok(m, k))
				}
				var w float64
				if w, err = xFloat(v); err == nil {
					if err = checkFunctionWeight(w); err == nil {
						spec.functions = append(spec.functions, &scoreFunctionSpec{kind: "weight", weight: &w})
						singleName = k
					}
				}
			default:
				if singleName != "" {
					return nil, pParsing("failed to parse [function_score] query. already found function [%s], now encountering [%s]. use [functions] array if you want to define several functions.", singleName, k).at(valueTok(m, k))
				}
				if arrayFound {
					return nil, pParsing("failed to parse [function_score] query. [%salready found [functions] array, now encountering [%s].]", misplaced, k).at(valueTok(m, k))
				}
				return nil, pParsing("failed to parse [function_score] query. field [%s] is not supported", k).at(valueTok(m, k))
			}
			if err != nil {
				return nil, err
			}
		}
	}
	if spec.query == nil {
		spec.query = &qnode{kind: "match_all", boost: 1}
	}
	if n.boost < 0 {
		return nil, negativeBoostError("function_score", body)
	}
	n.spec = spec
	return n, nil
}

func parseFunctionEntry(m M) (*scoreFunctionSpec, *Error) {
	var fn *scoreFunctionSpec
	var filter *qnode
	var weight *float64
	for _, k := range objectKeys(m) {
		v := m[k]
		switch t := v.(type) {
		case M:
			if k == "filter" {
				q, err := parseQuery(t)
				if err != nil {
					return nil, err
				}
				filter = q
				continue
			}
			if fn != nil {
				return nil, pParsing("failed to parse function_score functions. already found [%s], now encountering [%s].", fn.kind, k).at(valueTok(m, k))
			}
			f, err := parseScoreFunctionBody(m, k)
			if err != nil {
				return nil, err
			}
			fn = f
		default:
			if !isXValue(v) {
				continue
			}
			if k != "weight" {
				return nil, pParsing("failed to parse [function_score] query. field [%s] is not supported", k).at(valueTok(m, k))
			}
			w, err := xFloat(v)
			if err != nil {
				return nil, err
			}
			weight = &w
		}
	}
	if weight != nil {
		if err := checkFunctionWeight(*weight); err != nil {
			return nil, err
		}
		if fn == nil {
			fn = &scoreFunctionSpec{kind: "weight"}
		}
		fn.weight = weight
	}
	if fn == nil {
		return nil, pParsing("failed to parse [function_score] query. an entry in functions list is missing a function.").at(endTok(m))
	}
	fn.filter = filter
	return fn, nil
}

// evaluation -----------------------------------------------------------------

// evaluated is the result of running a query over every document of an
// index (root documents and nested objects).
type evaluated struct {
	score float64
	ftls  []search.FieldTermLocation
}

func (qb *queryBuilder) evaluate(q query.Query) (map[string]evaluated, error) {
	n, err := qb.ix.bleve.DocCount()
	if err != nil {
		return nil, err
	}
	out := map[string]evaluated{}
	if n == 0 {
		return out, nil
	}
	req := bleve.NewSearchRequestOptions(q, int(n), 0, false)
	req.Score = "default"
	req.IncludeLocations = true
	res, err := qb.ix.bleve.Search(req)
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Type: "search_phase_execution_exception", Reason: err.Error(), Index: qb.ix.Name}
	}
	for _, dm := range res.Hits {
		e := evaluated{score: dm.Score}
		for field, tlm := range dm.Locations {
			for term, locs := range tlm {
				for _, l := range locs {
					e.ftls = append(e.ftls, search.FieldTermLocation{Field: field, Term: term, Location: *l})
				}
			}
		}
		out[dm.ID] = e
	}
	return out, nil
}

type boundFunction struct {
	spec    *scoreFunctionSpec
	matches map[string]evaluated // nil: every document
	score   func(id string, sub float64) (float64, error)
}

func (qb *queryBuilder) functionScoreToQuery(spec *functionScoreSpec) (query.Query, error) {
	inner, err := qb.toQuery(spec.query)
	if err != nil {
		return nil, err
	}
	var fns []*boundFunction
	for _, fs := range spec.functions {
		bf := &boundFunction{spec: fs}
		if qb.noScores && spec.minScore == nil && fs.kind == "script_score" {
			// the script would only run to score: osmem does not need it
			fns = append(fns, bf)
			continue
		}
		if bf.score, err = qb.scoreFunction(fs); err != nil {
			return nil, err
		}
		if fs.filter != nil {
			fq, err := qb.toQuery(fs.filter)
			if err != nil {
				return nil, err
			}
			if bf.matches, err = qb.evaluate(fq); err != nil {
				return nil, err
			}
		}
		fns = append(fns, bf)
	}
	if qb.noScores && spec.minScore == nil {
		// the scores are not needed: the functions are not run
		// (FunctionScoreQuery.createWeight)
		return inner, nil
	}
	docs, err := qb.evaluate(inner)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(docs))
	for id := range docs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	scores := map[string]float64{}
	ftls := map[string][]search.FieldTermLocation{}
	single := len(fns) == 1 && fns[0].spec.filter == nil
	for _, id := range ids {
		sub := docs[id].score
		factor := 1.0
		switch {
		case len(fns) == 0:
			factor = 1
		case single:
			if factor, err = fns[0].score(id, sub); err != nil {
				return nil, err
			}
		default:
			if factor, err = combineFunctions(spec.scoreMode, fns, id, sub); err != nil {
				return nil, err
			}
		}
		final := sub
		if len(fns) > 0 {
			final = combineScore(spec.boostMode, sub, factor, spec.maxBoost)
		}
		if spec.minScore != nil && final < *spec.minScore {
			continue
		}
		if math.IsNaN(final) || final < 0 {
			return nil, &Error{Status: http.StatusInternalServerError, Type: "exception", openSearch: true, Reason: "function score query returned an invalid score: " + javaNumberString(float64(float32(final)), 32) + " for doc: 0"}
		}
		scores[id] = final
		if l := docs[id].ftls; len(l) > 0 {
			ftls[id] = l
		}
	}
	return &presetQuery{scores: scores, ftls: ftls}, nil
}

func combineFunctions(mode string, fns []*boundFunction, id string, sub float64) (float64, error) {
	factor := 1.0
	switch mode {
	case "FIRST":
		for _, f := range fns {
			if f.applies(id) {
				return f.score(id, sub)
			}
		}
	case "MAX", "MIN":
		found := false
		best := 0.0
		for _, f := range fns {
			if !f.applies(id) {
				continue
			}
			s, err := f.score(id, sub)
			if err != nil {
				return 0, err
			}
			if !found || (mode == "MAX" && s > best) || (mode == "MIN" && s < best) {
				best = s
			}
			found = true
		}
		if found {
			factor = best
		}
	case "MULTIPLY":
		for _, f := range fns {
			if !f.applies(id) {
				continue
			}
			s, err := f.score(id, sub)
			if err != nil {
				return 0, err
			}
			factor *= s
		}
	default: // SUM, AVG
		total, weights := 0.0, 0.0
		for _, f := range fns {
			if !f.applies(id) {
				continue
			}
			s, err := f.score(id, sub)
			if err != nil {
				return 0, err
			}
			total += s
			if f.spec.weight != nil {
				weights += float64(float32(*f.spec.weight))
			} else {
				weights += 1
			}
		}
		if weights != 0 {
			factor = total
			if mode == "AVG" {
				factor /= weights
			}
		}
	}
	return factor, nil
}

func (f *boundFunction) applies(id string) bool {
	if f.matches == nil {
		return true
	}
	_, ok := f.matches[id]
	return ok
}

// combineScore is CombineFunction.combine.
func combineScore(mode string, query, fn, maxBoost float64) float64 {
	fn = math.Min(fn, maxBoost)
	switch mode {
	case "REPLACE":
		return fn
	case "SUM":
		return query + fn
	case "AVG":
		return (query + fn) / 2
	case "MIN":
		return math.Min(query, fn)
	case "MAX":
		return math.Max(query, fn)
	}
	return float64(float32(query * fn))
}

// scoreFunction creates the scoring function of a function_score entry.
func (qb *queryBuilder) scoreFunction(fs *scoreFunctionSpec) (func(id string, sub float64) (float64, error), error) {
	weight := 1.0
	if fs.weight != nil {
		weight = float64(float32(*fs.weight))
	}
	withWeight := func(fn func(id string, sub float64) (float64, error)) func(id string, sub float64) (float64, error) {
		if fs.weight == nil {
			return fn
		}
		return func(id string, sub float64) (float64, error) {
			s, err := fn(id, sub)
			return s * weight, err
		}
	}
	switch fs.kind {
	case "weight":
		return func(string, float64) (float64, error) { return weight, nil }, nil
	case "script_score":
		fn, err := qb.scriptScoreFunction(fs.script)
		if err != nil {
			return nil, err
		}
		return withWeight(fn), nil
	case "field_value_factor":
		fn, err := qb.fieldValueFactor(fs.body)
		if err != nil {
			return nil, err
		}
		return withWeight(fn), nil
	case "random_score":
		fn, err := qb.randomScore(fs.body)
		if err != nil {
			return nil, err
		}
		return withWeight(fn), nil
	case "gauss", "exp", "linear":
		fn, err := qb.decayFunction(fs.kind, fs.body)
		if err != nil {
			return nil, err
		}
		return withWeight(fn), nil
	}
	return nil, errUnsupported("[" + fs.kind + "] function")
}

// scriptScoreFunction is the script_score function of a function_score
// entry: the script's _score is the sub-query score being replaced.
func (qb *queryBuilder) scriptScoreFunction(sc *scriptSpec) (func(id string, sub float64) (float64, error), error) {
	if _, _, cerr := sc.compile(painlessscript.ContextScore); cerr != nil {
		return nil, cerr
	}
	return func(id string, sub float64) (float64, error) {
		d := qb.ix.docByExternalID(id)
		if d == nil {
			return 0, nil
		}
		v, serr := evalDocScript(sc, painlessscript.ContextScore, qb.ix, d, sub)
		if serr != nil {
			return 0, serr
		}
		f, _ := v.Float64()
		return f, nil
	}, nil
}

func (qb *queryBuilder) docValues(id, field string) []any {
	d := qb.ix.docByExternalID(id)
	if d == nil {
		return nil
	}
	return qb.ix.fieldValues(d, field)
}

func numericDocValueOf(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case time.Time:
		return float64(t.UnixMilli()), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func (qb *queryBuilder) fieldValueFactor(body M) (func(id string, sub float64) (float64, error), error) {
	field := getString(body, "field")
	factor := 1.0
	if f, ok := toFloat(body["factor"]); ok {
		factor = float64(float32(f))
	}
	modifier := strings.ToUpper(getString(body, "modifier"))
	var missing *float64
	if f, ok := toFloat(body["missing"]); ok {
		missing = &f
	}
	f, _, mapped := qb.ix.Mapping.resolve(field)
	if !mapped {
		if missing == nil {
			msg := "Unable to find a field mapper for field [" + field + "]. No 'missing' value defined."
			return nil, &Error{Status: http.StatusBadRequest, Type: "query_shard_exception", Reason: "failed to create query: " + msg,
				Cause: &Error{Type: "exception", Reason: msg, openSearch: true}}
		}
	} else if !f.isNumeric() && !f.isDate() && f.Type != TypeBoolean {
		return nil, errCreateQuery("class_cast_exception", "class org.opensearch.index.fielddata.plain.SortedSetOrdinalsIndexFieldData cannot be cast to class org.opensearch.index.fielddata.IndexNumericFieldData (org.opensearch.index.fielddata.plain.SortedSetOrdinalsIndexFieldData and org.opensearch.index.fielddata.IndexNumericFieldData are in unnamed module of loader 'app')")
	}
	return func(id string, sub float64) (float64, error) {
		var value float64
		found := false
		if mapped {
			for _, v := range qb.docValues(id, field) {
				if n, ok := numericDocValueOf(v); ok {
					value, found = n, true
					break
				}
			}
		}
		if !found {
			if missing == nil {
				return 0, &Error{Status: http.StatusInternalServerError, Type: "exception", Reason: "Missing value for field [" + field + "]", openSearch: true}
			}
			value = *missing
		}
		val := value * factor
		var result float64
		switch modifier {
		case "LOG":
			result = math.Log10(val)
		case "LOG1P":
			result = math.Log10(val + 1)
		case "LOG2P":
			result = math.Log10(val + 2)
		case "LN":
			result = math.Log(val)
		case "LN1P":
			result = math.Log(val + 1)
		case "LN2P":
			result = math.Log(val + 2)
		case "SQUARE":
			result = val * val
		case "SQRT":
			result = math.Sqrt(val)
		case "RECIPROCAL":
			result = 1 / val
		default:
			result = val
		}
		if result < 0 {
			msg := "field value function must not produce negative scores, but got: [" + javaNumberString(result, 64) + "] for field value: [" + javaNumberString(value, 64) + "]"
			switch modifier {
			case "LN":
				msg += "; consider using ln1p or ln2p instead of ln to avoid negative scores"
			case "LOG":
				msg += "; consider using log1p or log2p instead of log to avoid negative scores"
			}
			return 0, errShardFailure(http.StatusBadRequest, "illegal_argument_exception", msg)
		}
		return result, nil
	}, nil
}

func (qb *queryBuilder) decayFunction(kind string, body M) (func(id string, sub float64) (float64, error), error) {
	var field string
	var params M
	mode := "MIN"
	for _, k := range objectKeys(body) {
		switch k {
		case "multi_value_mode":
			if _, isObject := body[k].(M); !isObject {
				// checked while parsing (checkMultiValueMode)
				mode = strings.ToUpper(xText(body[k]))
				break
			}
			fallthrough
		default:
			field = k
			params, _ = body[k].(M)
		}
	}
	f, _, mapped := qb.ix.Mapping.resolve(field)
	if !mapped {
		return nil, errShardParsing(1, 0, "unknown field [%s]", field)
	}
	decay := 0.5
	if d, ok := toFloat(params["decay"]); ok {
		decay = d
	}
	var origin, scale, offset float64
	var distance func(id string) []float64
	switch {
	case f.Type == TypeGeoPoint:
		o, hasOrigin := params["origin"]
		s := getString(params, "scale")
		if !hasOrigin || s == "" {
			return nil, errCreateQuery("parse_exception", "[origin] and [scale] must be set for geo fields.")
		}
		center, perr := parseGeoPointAny(o, "")
		if perr != nil {
			return nil, perr
		}
		var ok bool
		if scale, ok = geoDistanceMeters(s); !ok {
			return nil, errCreateNumberFormat(s)
		}
		if off := getString(params, "offset"); off != "" {
			offset, _ = geoDistanceMeters(off)
		}
		distance = func(id string) []float64 {
			var out []float64
			for _, v := range qb.docValues(id, field) {
				if p, ok := v.([2]float64); ok {
					out = append(out, arcDistanceMeters(center.lat, center.lon, p[0], p[1]))
				}
			}
			return out
		}
	case f.isDate():
		s := getString(params, "scale")
		if s == "" {
			return nil, errCreateQuery("parse_exception", "[scale] must be set for date fields.")
		}
		origin = float64(qb.c.now().UnixMilli())
		if o, ok := params["origin"]; ok {
			t, err := ParseDateMath(xText(o), f.Format, qb.c.now(), time.UTC, false)
			if err != nil {
				return nil, errDateQuery(err)
			}
			origin = float64(t.UnixMilli())
		}
		var ok bool
		if scale, ok = timeValueMillis(s); !ok {
			return nil, errCreateQuery("illegal_argument_exception", "failed to parse setting [DecayFunctionParser.scale] with value ["+s+"] as a time value: unit is missing or unrecognized")
		}
		if off := getString(params, "offset"); off != "" {
			offset, _ = timeValueMillis(off)
		}
		distance = func(id string) []float64 {
			var out []float64
			for _, v := range qb.docValues(id, field) {
				if t, ok := v.(time.Time); ok {
					out = append(out, math.Max(0, math.Abs(float64(t.UnixMilli())-origin)-offset))
				}
			}
			return out
		}
	case f.isNumeric():
		o, hasOrigin := toFloat(params["origin"])
		s, hasScale := toFloat(params["scale"])
		if !hasOrigin || !hasScale {
			return nil, errCreateQuery("parse_exception", "both [scale] and [origin] must be set for numeric fields.")
		}
		origin, scale = o, s
		offset, _ = toFloat(params["offset"])
		distance = func(id string) []float64 {
			var out []float64
			for _, v := range qb.docValues(id, field) {
				if n, ok := v.(float64); ok {
					out = append(out, math.Max(0, math.Abs(n-origin)-offset))
				}
			}
			return out
		}
	default:
		return nil, errShardParsing(1, 1, "field [%s] is of type [%s], but only numeric types are supported.", field, fieldTypeClass(f))
	}
	if f.Type == TypeGeoPoint {
		inner := distance
		distance = func(id string) []float64 {
			d := inner(id)
			for i := range d {
				d[i] = math.Max(0, d[i]-offset)
			}
			return d
		}
	}
	var fn func(d float64) float64
	switch kind {
	case "gauss":
		s := 0.5 * scale * scale / math.Log(decay)
		fn = func(d float64) float64 { return math.Exp(0.5 * d * d / s) }
	case "exp":
		s := math.Log(decay) / scale
		fn = func(d float64) float64 { return math.Exp(s * d) }
	default:
		s := scale / (1.0 - decay)
		fn = func(d float64) float64 { return math.Max(0.0, (s-d)/s) }
	}
	return func(id string, sub float64) (float64, error) {
		ds := distance(id)
		if len(ds) == 0 {
			return 1, nil
		}
		v := ds[0]
		switch mode {
		case "MAX":
			for _, d := range ds {
				v = math.Max(v, d)
			}
		case "AVG", "SUM":
			sum := 0.0
			for _, d := range ds {
				sum += d
			}
			v = sum
			if mode == "AVG" {
				v = sum / float64(len(ds))
			}
		case "MEDIAN":
			sorted := append([]float64(nil), ds...)
			sort.Float64s(sorted)
			mid := len(sorted) / 2
			v = sorted[mid]
			if len(sorted)%2 == 0 {
				v = (sorted[mid-1] + sorted[mid]) / 2
			}
		default:
			for _, d := range ds {
				v = math.Min(v, d)
			}
		}
		return fn(v), nil
	}, nil
}

// fieldTypeClass names the Java field type class of a mapped field (as
// rendered in some OpenSearch error messages).
func fieldTypeClass(f *Field) string {
	switch {
	case f.isKeywordLike() && f.Type == TypeKeyword:
		return "org.opensearch.index.mapper.KeywordFieldMapper$KeywordFieldType@1"
	case f.Type == TypeText:
		return "org.opensearch.index.mapper.TextFieldMapper$TextFieldType@1"
	case f.Type == TypeIP:
		return "org.opensearch.index.mapper.IpFieldMapper$IpFieldType@1"
	case f.Type == TypeBoolean:
		return "org.opensearch.index.mapper.BooleanFieldMapper$BooleanFieldType@1"
	case f.isNumeric():
		return "org.opensearch.index.mapper.NumberFieldMapper$NumberFieldType@1"
	case f.isDate():
		return "org.opensearch.index.mapper.DateFieldMapper$DateFieldType@1"
	}
	return "org.opensearch.index.mapper.MappedFieldType@1"
}

func (qb *queryBuilder) randomScore(body M) (func(id string, sub float64) (float64, error), error) {
	salt := javaStringHash(qb.ix.Name) << 10
	seedValue, hasSeed := body["seed"]
	if !hasSeed {
		// document-order random scores without a seed cannot be reproduced;
		// use a seed derived from the current time
		seed := int32(qb.c.now().UnixMilli())
		salted := bitMix(seed, salt)
		return func(id string, sub float64) (float64, error) {
			h := luceneMurmur3([]byte(id), salted)
			return float64(float32(h&0x00FFFFFF) / float32(1<<24)), nil
		}, nil
	}
	var seed int32
	switch t := seedValue.(type) {
	case string:
		if n, ok := javaDoubleOK(t); ok {
			seed = int32(int64(n))
		} else {
			seed = javaStringHash(t)
		}
	default:
		n, _ := toFloat(t)
		seed = int32(int64(n))
	}
	field := getString(body, "field")
	if field == "" {
		field = "_id"
	}
	salted := bitMix(seed, salt)
	return func(id string, sub float64) (float64, error) {
		var value string
		switch field {
		case "_id":
			root, _ := parseNestedID(id)
			value = root
		case "_seq_no":
			if d := qb.ix.docByExternalID(id); d != nil {
				value = javaNumberString(float64(d.SeqNo), 64)
				value = strings.TrimSuffix(value, ".0")
			}
		default:
			vals := qb.docValues(id, field)
			if len(vals) == 0 {
				return float64(float32(salted&0x00FFFFFF) / float32(1<<24)), nil
			}
			value = xText(vals[0])
		}
		h := luceneMurmur3([]byte(value), salted)
		return float64(float32(h&0x00FFFFFF) / float32(1<<24)), nil
	}, nil
}

// bitMix is HPPC's BitMixer.mix(int, int).
func bitMix(key, seed int32) int32 {
	k := uint32(key ^ seed)
	k = (k ^ (k >> 16)) * 0x85ebca6b
	k = (k ^ (k >> 13)) * 0xc2b2ae35
	return int32(k ^ (k >> 16))
}

// luceneMurmur3 is Lucene's StringHelper.murmurhash3_x86_32.
func luceneMurmur3(data []byte, seed int32) int32 {
	const c1, c2 = 0xcc9e2d51, 0x1b873593
	h1 := uint32(seed)
	n := len(data) / 4 * 4
	for i := 0; i < n; i += 4 {
		k1 := uint32(data[i]) | uint32(data[i+1])<<8 | uint32(data[i+2])<<16 | uint32(data[i+3])<<24
		k1 *= c1
		k1 = (k1 << 15) | (k1 >> 17)
		k1 *= c2
		h1 ^= k1
		h1 = (h1 << 13) | (h1 >> 19)
		h1 = h1*5 + 0xe6546b64
	}
	var k1 uint32
	switch len(data) & 3 {
	case 3:
		k1 = uint32(data[n+2]) << 16
		fallthrough
	case 2:
		k1 |= uint32(data[n+1]) << 8
		fallthrough
	case 1:
		k1 |= uint32(data[n])
		k1 *= c1
		k1 = (k1 << 15) | (k1 >> 17)
		k1 *= c2
		h1 ^= k1
	}
	h1 ^= uint32(len(data))
	h1 ^= h1 >> 16
	h1 *= 0x85ebca6b
	h1 ^= h1 >> 13
	h1 *= 0xc2b2ae35
	h1 ^= h1 >> 16
	return int32(h1)
}
