package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// parsing of aggregation definitions (AggregatorFactories.parseAggregators)

var aggNameRe = regexp.MustCompile(`^[^\[\]>]+$`)

// parseSearchAggregations parses and validates the aggregations of a search
// request body; the search runs them with runAggregations.
func parseSearchAggregations(body M, key string) (M, error) {
	v := body[key]
	am, ok := v.(M)
	if !ok {
		return nil, errParsing("Unknown key for a %s in [aggs].", jsonTokenName(v)).at(valueTok(body, key))
	}
	if _, _, err := parseAggregations(am); err != nil {
		return nil, err
	}
	return am, nil
}

// parseAggregations parses an aggregations object and validates its
// pipelines.
func parseAggregations(raw M) (defs, pipes []*aggDef, err error) {
	defs, pipes, err = parseAggLevel(raw, nil)
	if err != nil {
		return nil, nil, err
	}
	var errs []string
	pipes = validateAggLevel(nil, defs, pipes, &errs)
	if len(errs) > 0 {
		var b strings.Builder
		b.WriteString("Validation Failed: ")
		for i, e := range errs {
			fmt.Fprintf(&b, "%d: %s;", i+1, e)
		}
		return nil, nil, &Error{Status: http.StatusBadRequest, Type: "action_request_validation_exception", Reason: b.String()}
	}
	return defs, pipes, nil
}

func parseAggLevel(raw M, parent *aggDef) (defs, pipes []*aggDef, err error) {
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !aggNameRe.MatchString(name) {
			return nil, nil, errParsing("Invalid aggregation name [%s]. Aggregation names can contain any character except '[', ']', and '>'", name).
				at(keyTok(raw, name))
		}
		spec, ok := raw[name].(M)
		if !ok {
			return nil, nil, errParsing("Aggregation definition for [%s starts with a [%s], expected a [START_OBJECT].", name, jsonTokenName(raw[name])).
				at(valueTok(raw, name))
		}
		d, err := parseAggDef(name, spec, parent)
		if err != nil {
			return nil, nil, err
		}
		if d.typ.pipeline {
			pipes = append(pipes, d)
		} else {
			defs = append(defs, d)
		}
	}
	return defs, pipes, nil
}

func parseAggDef(name string, spec M, parent *aggDef) (*aggDef, error) {
	d := &aggDef{name: name, parent: parent}
	var types []string
	var subKeys []string
	for k := range spec {
		switch k {
		case "meta":
		case "aggs", "aggregations":
			subKeys = append(subKeys, k)
		default:
			types = append(types, k)
		}
	}
	// JSON order is lost: registered types are taken to come first
	sort.Slice(types, func(i, j int) bool {
		ki, kj := aggTypes[types[i]] != nil, aggTypes[types[j]] != nil
		if ki != kj {
			return ki
		}
		return types[i] < types[j]
	})
	if len(types) > 0 {
		kind := types[0]
		t := aggTypes[kind]
		if t == nil {
			return nil, (&Error{Status: http.StatusBadRequest, Type: "parsing_exception",
				Reason: "Unknown aggregation type [" + kind + "]" + suggestField(kind, registeredAggNames),
				Cause:  (&Error{Type: "named_object_not_found_exception", Reason: "unknown field [" + kind + "]"}).at(valueTok(spec, kind))}).
				at(valueTok(spec, kind))
		}
		body, ok := spec[kind].(M)
		if !ok {
			return nil, errParsing("Expected [START_OBJECT] under [%s], but got a [%s] in [%s]", kind, jsonTokenName(spec[kind]), name).
				at(valueTok(spec, kind))
		}
		d.kind, d.typ, d.body = kind, t, body
		if err := t.parse(&aggParser{def: d}, d); err != nil {
			return nil, err
		}
		if len(types) > 1 {
			return nil, errParsing("Found two aggregation type definitions in [%s]: [%s] and [%s]", name, kind, types[1]).
				at(valueTok(spec, types[1]))
		}
	}
	if m, ok := spec["meta"]; ok {
		mm, isObj := m.(M)
		if !isObj {
			return nil, errParsing("Expected [START_OBJECT] under [meta], but got a [%s] in [%s]", jsonTokenName(m), name).
				at(valueTok(spec, "meta"))
		}
		d.meta = mm
	}
	if len(subKeys) > 1 {
		return nil, errParsing("Found two sub aggregation definitions under [%s]", name).
			at(nthValueTok(spec, 2, "aggs", "aggregations"))
	}
	var subs M
	if len(subKeys) == 1 {
		sv := spec[subKeys[0]]
		sm, ok := sv.(M)
		if !ok {
			return nil, errParsing("Expected [START_OBJECT] under [%s], but got a [%s] in [%s]", subKeys[0], jsonTokenName(sv), name).
				at(valueTok(spec, subKeys[0]))
		}
		subs = sm
	}
	if d.typ == nil {
		return nil, errParsing("Missing definition for aggregation [%s]", name).at(endTok(spec))
	}
	if len(subs) > 0 {
		var err error
		if d.subs, d.pipes, err = parseAggLevel(subs, d); err != nil {
			return nil, err
		}
		if d.typ.pipeline {
			return nil, errIllegalArgument("Aggregation [%s] cannot define sub-aggregations", name)
		}
		if d.typ.leaf {
			return nil, &Error{Status: http.StatusInternalServerError, Type: "aggregation_initialization_exception",
				Reason: fmt.Sprintf("Aggregator [%s] of type [%s] cannot accept sub-aggregations", name, d.kind)}
		}
	}
	return d, nil
}

// registeredAggNames are the aggregation types of OpenSearch 3.8 with its
// default modules (suggestions for unknown types).
var registeredAggNames = []string{"adjacency_matrix", "auto_date_histogram", "avg", "avg_bucket", "bucket_script", "bucket_selector",
	"bucket_sort", "cardinality", "children", "composite", "cumulative_sum", "date_histogram", "date_range", "derivative",
	"diversified_sampler", "extended_stats", "extended_stats_bucket", "filter", "filters", "geo_bounds", "geo_centroid",
	"geo_distance", "geohash_grid", "geohex_grid", "geotile_grid", "global", "histogram", "ip_range", "matrix_stats", "max",
	"max_bucket", "median_absolute_deviation", "min", "min_bucket", "missing", "moving_avg", "moving_fn", "multi_terms",
	"nested", "parent", "percentile_ranks", "percentiles", "percentiles_bucket", "range", "rare_terms", "reverse_nested",
	"sampler", "scripted_metric", "serial_diff", "significant_terms", "significant_text", "stats", "stats_bucket", "sum",
	"sum_bucket", "terms", "top_hits", "value_count", "variable_width_histogram", "weighted_avg"}

// suggestField is SuggestingErrorOnUnknown.suggest: candidates whose
// Levenshtein similarity is above 0.5, best first.
func suggestField(name string, candidates []string) string {
	type scored struct {
		d float32
		s string
	}
	var list []scored
	for _, c := range candidates {
		if d := levenshteinSimilarity(name, c); d > 0.5 {
			list = append(list, scored{d, c})
		}
	}
	if len(list) == 0 {
		return ""
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].d != list[j].d {
			return list[i].d > list[j].d
		}
		return list[i].s < list[j].s
	})
	if len(list) == 1 {
		return " did you mean [" + list[0].s + "]?"
	}
	keys := make([]string, len(list))
	for i, e := range list {
		keys[i] = e.s
	}
	return " did you mean any of [" + strings.Join(keys, ", ") + "]?"
}

// ObjectParser-style field handling --------------------------------------------

// token kinds accepted by a declared field
const (
	tkString = 1 << iota
	tkNumber
	tkBool
	tkNull
	tkObject
	tkArray
)

// ObjectParser.ValueType token sets
const (
	vtString              = tkString
	vtStringOrNull        = tkString | tkNull
	vtNumber              = tkNumber | tkString
	vtBool                = tkBool | tkString
	vtObject              = tkObject
	vtObjectArray         = tkObject | tkArray
	vtObjectArrayOrString = tkObject | tkArray | tkString
	vtObjectOrString      = tkObject | tkString
	vtValue               = tkBool | tkNull | tkNumber | tkString
	vtStringArray         = tkArray | tkString
	vtNumberArray         = tkArray | tkNumber | tkString
	vtObjectOrBool        = tkObject | tkBool
	vtAny                 = tkString | tkNumber | tkBool | tkNull | tkObject | tkArray
)

func tokenKind(v any) int {
	switch v.(type) {
	case string:
		return tkString
	case bool:
		return tkBool
	case nil:
		return tkNull
	case M:
		return tkObject
	case []any:
		return tkArray
	}
	return tkNumber
}

// aggParser parses the body of one aggregation.
type aggParser struct {
	def *aggDef
}

// objFields declares the fields of an ObjectParser named name.
type objFields struct {
	name   string
	fields map[string]int
}

// valuesSourceFields are the fields of ValuesSourceAggregationBuilder.declareFields.
func valuesSourceFields(name string, formattable, timezone bool, extra map[string]int) objFields {
	f := map[string]int{"field": vtString, "missing": vtValue, "value_type": vtString, "script": vtObjectOrString}
	if formattable {
		f["format"] = vtString
	}
	if timezone {
		f["time_zone"] = vtNumber
	}
	for k, v := range extra {
		f[k] = v
	}
	return objFields{name: name, fields: f}
}

// check rejects unknown fields and fields with unsupported token types.
func (of objFields) check(body M) error {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kinds, ok := of.fields[k]
		if !ok {
			cands := make([]string, 0, len(of.fields))
			for c := range of.fields {
				cands = append(cands, c)
			}
			return errXContent(nil, "[%s] unknown field [%s]%s", of.name, k, suggestField(k, cands)).
				at(keyTok(body, k)).atParser(valueTok(body, k))
		}
		if kinds&tokenKind(body[k]) == 0 {
			return errXContent(nil, "[%s] %s doesn't support values of type: %s", of.name, k, jsonTokenName(body[k])).at(valueTok(body, k))
		}
	}
	return nil
}

// failed wraps the failure to parse the value of field key of body. It is
// located where the parser stopped: the token of the cause, or the last
// token of the value (validation after the value was read).
func (of objFields) failed(body M, key string, cause *Error) *Error {
	return errXContent(cause, "[%s] failed to parse field [%s]", of.name, key).atCause(valueEndTok(body, key))
}

func aggNumberFormatError(s string) *Error {
	return errJava(http.StatusBadRequest, "number_format_exception", "For input string: \""+s+"\"")
}

// intValue is XContentParser.intValue for a number or a string.
func (of objFields) intValue(body M, key string) (int, error) {
	switch t := body[key].(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil || !javaDoubleString(t) {
			return 0, of.failed(body, key, aggNumberFormatError(t))
		}
		if f < math.MinInt32 || f > math.MaxInt32 {
			return 0, of.failed(body, key, errIllegalArgument("Value [%s] is out of range for an integer", t))
		}
		return int(f), nil
	default:
		f, _ := toFloat(t)
		return javaInt(f), nil
	}
}

// longValue is XContentParser.longValue for a number or a string.
func (of objFields) longValue(body M, key string) (int64, error) {
	switch t := body[key].(type) {
	case string:
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return n, nil
		}
		bf, ok := new(big.Float).SetString(t)
		if !ok {
			return 0, of.failed(body, key, errIllegalArgument("Cannot convert [%s] to a number", t))
		}
		bi, _ := bf.Int(nil)
		if !bi.IsInt64() {
			return 0, of.failed(body, key, errIllegalArgument("Value [%s] is out of range for a long", t))
		}
		return bi.Int64(), nil
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, nil
		}
		f, _ := t.Float64()
		return int64(f), nil
	default:
		f, _ := toFloat(t)
		return int64(f), nil
	}
}

// doubleValue is XContentParser.doubleValue for a number or a string.
func (of objFields) doubleValue(body M, key string) (float64, error) {
	return aggParseDouble(body[key], func(s string) *Error { return of.failed(body, key, aggNumberFormatError(s)) })
}

func aggParseDouble(v any, fail func(string) *Error) (float64, error) {
	switch t := v.(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil && !isRangeErr(err) || !javaDoubleString(t) {
			return 0, fail(t)
		}
		return f, nil
	default:
		f, _ := toFloat(t)
		return f, nil
	}
}

func isRangeErr(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

// javaDoubleString reports whether Double.parseDouble accepts s (Go also
// accepts forms like "0x1p3", "inf" or underscores that Java rejects).
func javaDoubleString(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "_xXpP") {
		return false
	}
	switch strings.TrimLeft(s, "+-") {
	case "Infinity", "NaN":
		return true
	}
	return !strings.ContainsAny(strings.ToLower(s), "infa")
}

// boolValue is XContentParser.booleanValue.
func (of objFields) boolValue(body M, key string) (bool, error) {
	switch t := body[key].(type) {
	case bool:
		return t, nil
	case string:
		switch t {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return false, of.failed(body, key, errIllegalArgument("Failed to parse value [%s] as only [true] or [false] are allowed.", t))
	}
	return false, nil
}

// requireFieldOrScript is the required field set of a values source parser.
func requireFieldOrScript(body M) error {
	_, f := body["field"]
	_, s := body["script"]
	if !f && !s {
		return errIllegalArgument("Required one of fields [field, script], but none were specified. ")
	}
	return nil
}

// errScript rejects scripts, which osmem does not run.
func errScript(d *aggDef) *Error {
	return errUnsupported("script in aggregation [" + d.name + "]")
}

// pipeline validation (AggregatorFactories.Builder.validate) --------------------

type validationCtx struct {
	parent *aggDef   // nil at the root of the tree
	root   []*aggDef // the regular aggregations of the level
	errs   *[]string
}

func (vc *validationCtx) add(format string, args ...any) {
	*vc.errs = append(*vc.errs, fmt.Sprintf(format, args...))
}

// validateAggLevel resolves the order of the pipelines of a level, validates
// them and the levels below; it returns the pipelines in execution order.
func validateAggLevel(parent *aggDef, defs, pipes []*aggDef, errs *[]string) []*aggDef {
	vc := &validationCtx{parent: parent, root: defs, errs: errs}
	ordered, err := resolvePipelineOrder(defs, pipes)
	if err != nil {
		vc.add("%s", err.Reason)
		ordered = pipes
	} else {
		for _, p := range ordered {
			if p.typ.validate != nil {
				p.typ.validate(vc, p)
			}
		}
	}
	for _, d := range defs {
		d.pipes = validateAggLevel(d, d.subs, d.pipes, errs)
	}
	return ordered
}

// aggPathElement is an element of an AggregationPath ("name[key]" or
// "name.key" for the last element).
type aggPathElement struct {
	name, key string
	hasKey    bool
}

// parseAggPath is AggregationPath.parse.
func parseAggPath(path string) ([]aggPathElement, error) {
	var parts []string
	for _, p := range strings.Split(path, ">") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	invalid := func(el string) error {
		return errAggExecution("Invalid path element [%s] in path [%s]", el, path)
	}
	out := make([]aggPathElement, 0, len(parts))
	for i, el := range parts {
		if idx := strings.LastIndex(el, "["); idx >= 0 {
			if idx == 0 || idx > len(el)-3 || el[len(el)-1] != ']' {
				return nil, invalid(el)
			}
			out = append(out, aggPathElement{name: el[:idx], key: el[idx+1 : len(el)-1], hasKey: true})
			continue
		}
		if i == len(parts)-1 {
			if idx := strings.LastIndex(el, "."); idx >= 0 {
				if idx == 0 || idx > len(el)-2 {
					return nil, invalid(el)
				}
				out = append(out, aggPathElement{name: el[:idx], key: el[idx+1:], hasKey: true})
				continue
			}
		}
		out = append(out, aggPathElement{name: el})
	}
	return out, nil
}

// aggPathStrings is AggregationPath.getPathElementsAsStringList.
func aggPathStrings(els []aggPathElement) []string {
	var out []string
	for _, e := range els {
		out = append(out, e.name)
		if e.hasKey {
			out = append(out, e.key)
		}
	}
	return out
}

// resolvePipelineOrder is AggregatorFactories.resolvePipelineAggregatorOrder.
func resolvePipelineOrder(defs, pipes []*aggDef) ([]*aggDef, *Error) {
	aggsByName := map[string]*aggDef{}
	for _, d := range defs {
		aggsByName[d.name] = d
	}
	pipesByName := map[string]*aggDef{}
	for _, p := range pipes {
		pipesByName[p.name] = p
	}
	var ordered []*aggDef
	unmarked := map[*aggDef]bool{}
	for _, p := range pipes {
		unmarked[p] = true
	}
	marked := map[*aggDef]bool{}
	var visit func(p *aggDef) *Error
	visit = func(p *aggDef) *Error {
		if marked[p] {
			return errIllegalArgument("Cyclical dependency found with pipeline aggregator [%s]", p.name)
		}
		if !unmarked[p] {
			return nil
		}
		marked[p] = true
		var paths []string
		if p.typ.paths != nil {
			paths = p.typ.paths(p)
		}
		for _, path := range paths {
			if path == "_count" || path == "_key" {
				continue
			}
			els, err := parseAggPath(path)
			if err != nil || len(els) == 0 {
				continue
			}
			if agg, ok := aggsByName[els[0].name]; ok {
				for i := 1; i < len(els); i++ {
					name := els[i].name
					if i == len(els)-1 && (strings.EqualFold(name, "_key") || name == "_count") {
						break
					}
					found := false
					for _, s := range agg.subs {
						if s.name == name {
							agg, found = s, true
							break
						}
					}
					if !found && i == len(els)-1 {
						for _, s := range agg.pipes {
							if s.name == name {
								found = true
								break
							}
						}
					}
					if !found {
						return errIllegalArgument("No aggregation [%s] found for path [%s]", name, path)
					}
				}
				continue
			}
			if dep, ok := pipesByName[els[0].name]; ok {
				if err := visit(dep); err != nil {
					return err
				}
				continue
			}
			return errIllegalArgument("No aggregation found for path [%s]", path)
		}
		delete(unmarked, p)
		delete(marked, p)
		ordered = append(ordered, p)
		return nil
	}
	for _, p := range pipes {
		if err := visit(p); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

// validateParentHistogram is ValidationContext.validateParentAggSequentiallyOrdered.
func (vc *validationCtx) validateParentHistogram(kind, name string) {
	switch {
	case vc.parent == nil:
		vc.add("%s aggregation [%s] must have a histogram, date_histogram or auto_date_histogram as parent but doesn't have a parent", kind, name)
	case vc.parent.kind == "histogram" || vc.parent.kind == "date_histogram":
		if n, _ := toFloat(vc.parent.body["min_doc_count"]); n != 0 {
			vc.add("parent histogram of %s aggregation [%s] must have min_doc_count of 0", kind, name)
		}
	case vc.parent.kind == "auto_date_histogram":
	default:
		vc.add("%s aggregation [%s] must have a histogram, date_histogram or auto_date_histogram as parent", kind, name)
	}
}

// validateHasParent is ValidationContext.validateHasParent.
func (vc *validationCtx) validateHasParent(kind, name string) {
	if vc.parent == nil {
		vc.add("%s aggregation [%s] must be declared inside of another aggregation", kind, name)
	}
}
