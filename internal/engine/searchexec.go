package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Search execution features: slices, rescoring, indices_boost,
// terminate_after, the shard-level checks of a search context, scroll and
// point in time contexts, count and msearch.

func keysSorted(m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func errXContentParse(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: fmt.Sprintf(format, args...)}
}

func errSearchException(reason string) *Error {
	return &Error{Status: http.StatusInternalServerError, Type: "search_exception", Reason: reason}
}

func errNumberFormatPlain(s string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "number_format_exception", Reason: "For input string: \"" + s + "\""}
}

// xcontentInt is XContentParser.intValue: numbers truncate, numeric strings
// coerce.
func xcontentInt(v any) (int, *Error) {
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(int32(n)), nil
		}
		if f, err := t.Float64(); err == nil {
			return int(int32(f)), nil
		}
		return 0, errNumberFormatPlain(t.String())
	case float64:
		return int(int32(t)), nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, errNumberFormatPlain(t)
		}
		return int(int32(f)), nil
	}
	return 0, errNumberFormatPlain(fmt.Sprint(v))
}

// xcontentFloat is XContentParser.floatValue.
func xcontentFloat(v any) (float32, *Error) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, errNumberFormatPlain(t.String())
		}
		return float32(f), nil
	case float64:
		return float32(t), nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, errNumberFormatPlain(t)
		}
		return float32(f), nil
	}
	return 0, errNumberFormatPlain(fmt.Sprint(v))
}

// slice -----------------------------------------------------------------

type sliceSpec struct {
	id, max int
	field   string
}

// parseSlice is SliceBuilder.fromXContent.
func parseSlice(body M, key string) (*sliceSpec, error) {
	v := body[key]
	m, ok := v.(M)
	if !ok {
		return nil, errParsing("Unknown key for a %s in [slice].", jsonTokenName(v)).at(valueTok(body, key))
	}
	for _, k := range keysSorted(m) {
		if k != "id" && k != "max" && k != "field" {
			return nil, errXContentParse("[slice] unknown field [%s]", k).at(keyTok(m, k)).atParser(valueTok(m, k))
		}
	}
	spec := &sliceSpec{id: -1, max: -1, field: "_id"}
	if raw, ok := m["field"]; ok {
		s, isString := raw.(string)
		if !isString {
			return nil, errXContentParse("[slice] field doesn't support values of type: %s", jsonTokenName(raw)).at(valueTok(m, "field"))
		}
		spec.field = s
	}
	for _, k := range []string{"id", "max"} {
		raw, ok := m[k]
		if !ok {
			continue
		}
		n, nerr := xcontentInt(raw)
		if nerr != nil {
			return nil, (&Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: "[slice] failed to parse field [" + k + "]", Cause: nerr}).
				atCause(valueEndTok(m, k))
		}
		cause := ""
		if k == "id" {
			if n < 0 {
				cause = "id must be greater than or equal to 0"
			} else if spec.max != -1 && n >= spec.max {
				cause = "max must be greater than id"
			}
			spec.id = n
		} else {
			if n <= 1 {
				cause = "max must be greater than 1"
			} else if spec.id != -1 && spec.id >= n {
				cause = "max must be greater than id"
			}
			spec.max = n
		}
		if cause != "" {
			return nil, (&Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: "[slice] failed to parse field [" + k + "]",
				Cause: &Error{Type: "illegal_argument_exception", Reason: cause}}).atCause(valueEndTok(m, k))
		}
	}
	return spec, nil
}

// sliceSelection is SliceBuilder.toFilter for one shard: whether the shard
// takes part and the terms slice its documents are filtered with (max 0
// keeps every document).
func sliceSelection(spec *sliceSpec, numShards, shard int) (bool, int, int) {
	if spec.max <= 0 {
		// SliceBuilder.shardMatches without max: floorMod(shard, -1) is 0
		return spec.id == 0, spec.id, 1
	}
	if numShards == 1 {
		return true, spec.id, spec.max
	}
	if spec.max >= numShards {
		target := int(floorMod(int64(spec.id), int64(numShards)))
		if target != shard {
			return false, 0, 0
		}
		slices := spec.max / numShards
		if spec.max%numShards > target {
			slices++
		}
		if slices == 1 {
			return true, 0, 0
		}
		return true, spec.id / numShards, slices
	}
	return int(floorMod(int64(shard), int64(spec.max))) == spec.id, 0, 0
}

// sliceFieldError validates the slice field of an index.
func sliceFieldError(ix *Index, field string) *Error {
	if field == "_id" {
		return nil
	}
	if field == "_uid" {
		return errIllegalArgument("Computing slices on the [_uid] field is illegal for 7.x indices, use [_id] instead")
	}
	f, _, ok := ix.Mapping.resolve(field)
	if !ok {
		return errIllegalArgument("field %s not found", field)
	}
	if !(f.isNumeric() || f.isDate() || f.Type == TypeBoolean) || !getBool(f.Extra, "doc_values", true) {
		return errIllegalArgument("cannot load numeric doc values on %s", field)
	}
	return nil
}

// sliceLongValues returns the long doc values a numeric slice hashes.
func sliceLongValues(f *Field, raw []any) []int64 {
	var out []int64
	for _, v := range raw {
		cv, ok := convertValue(f, v)
		if !ok {
			continue
		}
		switch t := cv.(type) {
		case time.Time:
			out = append(out, t.UnixMilli())
		case bool:
			if t {
				out = append(out, 1)
			} else {
				out = append(out, 0)
			}
		case float64:
			switch f.Type {
			case TypeDouble:
				bits := int64(math.Float64bits(t))
				out = append(out, bits^((bits>>63)&0x7fffffffffffffff))
			case TypeFloat, TypeHalfFloat:
				bits := int32(math.Float32bits(float32(t)))
				out = append(out, int64(bits^((bits>>31)&0x7fffffff)))
			case TypeScaledFloat:
				out = append(out, int64(javaRound(t*scalingFactor(f))))
			default:
				out = append(out, int64(t))
			}
		}
	}
	return out
}

// sliceContains reports whether a document belongs to a terms or doc values
// slice.
func sliceContains(spec *sliceSpec, f *Field, h *hit, id, max int) bool {
	if max == 0 {
		return true
	}
	if spec.field == "_id" {
		return floorMod(int64(murmur3x86_32(encodeDocID(h.doc.ID), 7919)), int64(max)) == int64(id)
	}
	vals := sliceLongValues(f, h.ix.fieldValues(h.doc, spec.field))
	if len(vals) == 0 {
		return floorMod(0, int64(max)) == int64(id)
	}
	for _, v := range vals {
		if floorMod(int64(int32(bitMix64(uint64(v)))), int64(max)) == int64(id) {
			return true
		}
	}
	return false
}

// rescore ----------------------------------------------------------------

type rescoreSpec struct {
	window        int
	query         any
	queryWeight   float32
	rescoreWeight float32
	scoreMode     string
}

// parseRescore is RescorerBuilder.parseFromXContent for an object or array.
func parseRescore(body M, key string) ([]rescoreSpec, error) {
	v := body[key]
	var items []any
	switch t := v.(type) {
	case M:
		items = []any{t}
	case []any:
		items = t
	default:
		return nil, errParsing("Unknown key for a %s in [rescore].", jsonTokenName(v)).at(valueTok(body, key))
	}
	out := make([]rescoreSpec, 0, len(items))
	for _, item := range items {
		m, ok := item.(M)
		if !ok {
			return nil, errParsing("Unknown key for a %s in [rescore].", jsonTokenName(item))
		}
		spec := rescoreSpec{window: 10, queryWeight: 1, rescoreWeight: 1, scoreMode: "total"}
		found := false
		for _, k := range keysSorted(m) {
			switch val := m[k].(type) {
			case M:
				if k != "query" {
					return nil, (&Error{Status: http.StatusBadRequest, Type: "named_object_not_found_exception", Reason: "unknown field [" + k + "]"}).at(valueTok(m, k))
				}
				if err := parseQueryRescorer(val, &spec); err != nil {
					return nil, err
				}
				found = true
			case []any:
				return nil, errParsing("unexpected token [START_ARRAY] after [%s]", k).at(valueTok(m, k))
			default:
				if k != "window_size" {
					return nil, errParsing("rescore doesn't support [%s]", k).at(valueTok(m, k))
				}
				n, err := xcontentInt(val)
				if err != nil {
					return nil, err
				}
				spec.window = n
			}
		}
		if !found {
			return nil, errParsing("missing rescore type").at(endTok(m))
		}
		out = append(out, spec)
	}
	return out, nil
}

// parseQueryRescorer is QueryRescorerBuilder.fromXContent.
func parseQueryRescorer(m M, spec *rescoreSpec) error {
	for _, k := range keysSorted(m) {
		switch k {
		case "rescore_query", "query_weight", "rescore_query_weight", "score_mode":
		default:
			return errXContentParse("[query] unknown field [%s]", k).at(keyTok(m, k)).atParser(valueTok(m, k))
		}
	}
	for _, k := range []string{"query_weight", "rescore_query_weight"} {
		raw, ok := m[k]
		if !ok {
			continue
		}
		f, err := xcontentFloat(raw)
		if err != nil {
			return (&Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: "[query] failed to parse field [" + k + "]", Cause: err}).
				atCause(valueEndTok(m, k))
		}
		if k == "query_weight" {
			spec.queryWeight = f
		} else {
			spec.rescoreWeight = f
		}
	}
	if raw, ok := m["score_mode"]; ok {
		s, isString := raw.(string)
		if !isString {
			return errXContentParse("[query] score_mode doesn't support values of type: %s", jsonTokenName(raw)).at(valueTok(m, "score_mode"))
		}
		switch mode := strings.ToLower(s); mode {
		case "total", "multiply", "avg", "max", "min":
			spec.scoreMode = mode
		default:
			return (&Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: "[query] failed to parse field [score_mode]",
				Cause: &Error{Type: "illegal_argument_exception", Reason: "illegal score_mode [" + s + "]"}}).atCause(valueEndTok(m, "score_mode"))
		}
	}
	q, ok := m["rescore_query"].(M)
	if !ok {
		return errIllegalArgument("rescore_query cannot be null")
	}
	spec.query = q
	return nil
}

func combineRescore(mode string, first, second float32) float32 {
	switch mode {
	case "multiply":
		return first * second
	case "avg":
		return (first + second) / 2
	case "max":
		return float32(math.Max(float64(first), float64(second)))
	case "min":
		return float32(math.Min(float64(first), float64(second)))
	}
	return first + second
}

// shardKey identifies the shard a hit was collected on.
type shardKey struct {
	ix    *Index
	shard int
}

// groupByShard groups hits by shard in shard order (index name, shard id).
func groupByShard(hits []*hit) ([]shardKey, map[shardKey][]*hit) {
	groups := map[shardKey][]*hit{}
	var keys []shardKey
	for _, h := range hits {
		k := shardKey{h.ix, h.shard}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], h)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ix.Name != keys[j].ix.Name {
			return keys[i].ix.Name < keys[j].ix.Name
		}
		return keys[i].shard < keys[j].shard
	})
	return keys, groups
}

func sortByScoreThenDoc(hits []*hit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].doc.SeqNo < hits[j].doc.SeqNo
	})
}

// rescoreHits applies the query rescorers shard by shard: each shard keeps
// its top max(from+size, window_size) hits, rescores the top window of them
// (QueryRescorer) and scales the rest by query_weight.
func (c *Cluster) rescoreHits(hits []*hit, sr *searchRequest) ([]*hit, error) {
	keys, groups := groupByShard(hits)
	keep := sr.from + sr.size
	for _, r := range sr.rescore {
		if r.window > keep {
			keep = r.window
		}
	}
	scores := map[*Index][]map[*Doc]float64{}
	var out []*hit
	for _, key := range keys {
		g := groups[key]
		sortByScoreThenDoc(g)
		if len(g) > keep {
			g = g[:keep]
		}
		for ri, r := range sr.rescore {
			if r.window < 0 {
				return nil, errShardFailures([]*shardFailure{{index: key.ix.Name, shard: key.shard,
					cause: &Error{Status: http.StatusInternalServerError, Type: "negative_array_size_exception", Reason: strconv.Itoa(r.window), plain: true}}})
			}
			if scores[key.ix] == nil {
				scores[key.ix] = make([]map[*Doc]float64, len(sr.rescore))
			}
			matched := scores[key.ix][ri]
			if matched == nil {
				rescored, err := c.executeTargets([]target{{ix: key.ix}}, r.query, false, false)
				if err != nil {
					if e, ok := err.(*Error); ok && (e.Type == "parsing_exception" || e.Type == "x_content_parse_exception") {
						return nil, (&Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: "[query] failed to parse field [rescore_query]", Cause: e}).
							atCause(nil)
					}
					return nil, err
				}
				matched = make(map[*Doc]float64, len(rescored))
				for _, rh := range rescored {
					matched[rh.doc] = rh.score
				}
				scores[key.ix][ri] = matched
			}
			window := r.window
			if window > len(g) {
				window = len(g)
			}
			for _, h := range g[:window] {
				first := float32(h.score) * r.queryWeight
				if rs, ok := matched[h.doc]; ok {
					h.score = float64(combineRescore(r.scoreMode, first, float32(rs)*r.rescoreWeight))
				} else {
					h.score = float64(first)
				}
			}
			sortByScoreThenDoc(g[:window])
			for _, h := range g[window:] {
				h.score = float64(float32(h.score) * r.queryWeight)
			}
			if len(g) > window {
				sortByScoreThenDoc(g)
			}
		}
		out = append(out, g...)
	}
	return out, nil
}

// indices_boost ------------------------------------------------------------

type indexBoost struct {
	expr  string
	boost float64
}

// parseIndicesBoost reads the array form [{"index": boost}] and the
// deprecated object form {"index": boost}.
func parseIndicesBoost(body M, key string) ([]indexBoost, error) {
	v := body[key]
	var out []indexBoost
	switch t := v.(type) {
	case []any:
		for i, e := range t {
			m, ok := e.(M)
			if !ok {
				return nil, errParsing("Expected [START_OBJECT] in [null] but found [%s]", jsonTokenName(e)).at(elemTok(t, i))
			}
			if len(m) == 0 {
				return nil, errParsing("Expected [FIELD_NAME] in [indices_boost] but found [END_OBJECT]").at(endTok(m))
			}
			if len(m) > 1 {
				return nil, errParsing("Expected [END_OBJECT] in [indices_boost] but found [FIELD_NAME]").at(nthKeyTok(m, 2))
			}
			for k, raw := range m {
				f, ok := boostNumber(raw)
				if !ok {
					return nil, errParsing("Expected [VALUE_NUMBER] in [indices_boost] but found [%s]", jsonTokenName(raw)).at(valueTok(m, k))
				}
				out = append(out, indexBoost{expr: k, boost: f})
			}
		}
	case M:
		for _, k := range keysSorted(t) {
			raw := t[k]
			switch raw.(type) {
			case M, []any:
				return nil, errParsing("Unknown key for a %s in [%s].", jsonTokenName(raw), k).at(valueTok(t, k))
			}
			f, err := xcontentFloat(raw)
			if err != nil {
				return nil, err
			}
			out = append(out, indexBoost{expr: k, boost: float64(f)})
		}
	default:
		return nil, errParsing("Unknown key for a %s in [indices_boost].", jsonTokenName(v)).at(valueTok(body, key))
	}
	return out, nil
}

func boostNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return float64(float32(f)), err == nil
	case float64:
		return float64(float32(t)), true
	}
	return 0, false
}

// resolveIndexBoosts resolves indices_boost entries with the request's
// indices options; the first entry naming an index wins.
func (c *Cluster) resolveIndexBoosts(sr *searchRequest, p Params) error {
	if len(sr.indexBoosts) == 0 {
		return nil
	}
	o, err := searchIndicesOptions.withParams(p)
	if err != nil {
		return err
	}
	r := c.newExprResolver()
	sr.boostByIndex = map[string]float64{}
	for _, b := range sr.indexBoosts {
		indices, err := r.concrete([]string{b.expr}, o)
		if err != nil {
			return err
		}
		for _, ix := range indices {
			if _, ok := sr.boostByIndex[ix.Name]; !ok {
				sr.boostByIndex[ix.Name] = b.boost
			}
		}
	}
	return nil
}

// shard failures ----------------------------------------------------------

// sortShardFailures orders failures by shard (index name, shard id).
func sortShardFailures(fs []*shardFailure) {
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].index != fs[j].index {
			return fs[i].index < fs[j].index
		}
		return fs[i].shard < fs[j].shard
	})
}

func groupShardFailures(fs []*shardFailure) []*shardFailure {
	seen := map[string]bool{}
	var out []*shardFailure
	for _, f := range fs {
		key := f.index + "\x00" + f.cause.Type + "\x00" + f.cause.Reason
		if !seen[key] {
			seen[key] = true
			out = append(out, f)
		}
	}
	return out
}

// errShardFailures is a search_phase_execution_exception for failures of
// every shard.
func errShardFailures(fs []*shardFailure) *Error {
	sortShardFailures(fs)
	grouped := groupShardFailures(fs)
	status := fs[0].cause.Status
	for _, f := range fs[1:] {
		if f.cause.Status >= 500 {
			status = f.cause.Status
		}
	}
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return &Error{Status: status, Type: "search_phase_execution_exception", Reason: "all shards failed", failure: grouped[0], more: grouped[1:]}
}

// aggregationShardError builds the aggregations of a request for one index
// as its shards do (AggregatorFactories.build): the error fails the shards
// of the index.
func (c *Cluster) aggregationShardError(sr *searchRequest, ix *Index) *Error {
	defs, _, err := parseAggregations(sr.aggs)
	if err != nil {
		return nil
	}
	ac := &aggContext{c: c, ts: []target{{ix: ix}}, now: c.now(), indices: []*Index{ix}}
	pc := &prepareCtx{ac: ac, ix: ix}
	if err := pc.prepareLevel(defs, nil); err != nil {
		e, isErr := err.(*Error)
		if !isErr {
			return nil
		}
		if e.failure != nil {
			return e.failure.cause
		}
		return e
	}
	return nil
}

// search context checks -----------------------------------------------------

// sortsOnFields reports a sort other than the default score order.
func sortsOnFields(sr *searchRequest) bool {
	if !sr.explicitSort {
		return false
	}
	return !(len(sr.sort) == 1 && sr.sort[0].field == "_score" && sr.sort[0].desc)
}

// searchAfterKind is the Lucene sort type a sort field compares with.
func searchAfterKind(ix *Index, s sortSpec) string {
	switch s.field {
	case "_doc":
		return "int"
	case "_score":
		return "float"
	case "_shard_doc":
		return "long"
	case "_id", "_index":
		return "string"
	}
	f, _, ok := ix.Mapping.resolve(s.field)
	if !ok {
		if s.unmappedType == "" {
			return "string"
		}
		f = &Field{Type: s.unmappedType}
	}
	switch f.Type {
	case TypeInteger, TypeShort, TypeByte, TypeBoolean, TypeTokenCount:
		return "int"
	case TypeLong, TypeDate, TypeDateNanos, TypeUnsignedLong:
		return "long"
	case TypeDouble, TypeScaledFloat:
		return "double"
	case TypeFloat, TypeHalfFloat:
		return "float"
	}
	return "string"
}

// checkSearchAfterValues is SearchAfterBuilder.convertValueFromSortType.
func checkSearchAfterValues(ix *Index, sr *searchRequest) *Error {
	for i, s := range sr.sort {
		v := sr.searchAfter[i]
		if v == nil {
			continue
		}
		kind := searchAfterKind(ix, s)
		if kind == "string" {
			continue
		}
		if _, isNumber := v.(json.Number); isNumber {
			continue
		}
		if _, isFloat := v.(float64); isFloat {
			continue
		}
		text := fmt.Sprint(v)
		var err error
		switch kind {
		case "int":
			_, err = strconv.ParseInt(text, 10, 32)
		case "long":
			if f, _, ok := ix.Mapping.resolve(s.field); ok && f.isDate() {
				continue
			}
			if _, err = strconv.ParseInt(text, 10, 64); err != nil {
				_, err = parseJavaDouble(text)
			}
		default:
			_, err = parseJavaDouble(text)
		}
		if err != nil {
			return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "Failed to parse search_after value for field [" + s.field + "].",
				Cause: &Error{Type: "number_format_exception", Reason: "For input string: \"" + text + "\""}}
		}
	}
	return nil
}

func parseJavaDouble(s string) (float64, error) {
	t := strings.TrimSpace(s)
	t = strings.TrimRight(t, "fFdD")
	if t == "" || strings.EqualFold(t, "inf") {
		return 0, strconv.ErrSyntax
	}
	return strconv.ParseFloat(t, 64)
}

// nullSearchAfter is the NullPointerException of a null search_after value
// on a numeric sort.
func nullSearchAfter(ix *Index, sr *searchRequest) *Error {
	for i, s := range sr.sort {
		if sr.searchAfter[i] != nil {
			continue
		}
		boxed := ""
		switch searchAfterKind(ix, s) {
		case "int":
			boxed = "java.lang.Integer.intValue()"
		case "long":
			boxed = "java.lang.Long.longValue()"
		case "double":
			boxed = "java.lang.Double.doubleValue()"
		case "float":
			boxed = "java.lang.Float.floatValue()"
		}
		if boxed != "" {
			return &Error{Status: http.StatusInternalServerError, Type: "null_pointer_exception", Reason: "Cannot invoke \"" + boxed + "\" because \"value\" is null"}
		}
	}
	return nil
}

func collapseTypeSupported(f *Field) bool {
	return f.Type == TypeKeyword || f.isNumeric()
}

// checkShard runs the checks a shard performs while creating the search
// context (SearchService.parseSource, DefaultSearchContext.preProcess) and
// starting the query; the error fails the shards of this index.
func (c *Cluster) checkShard(ix *Index, sr *searchRequest) *Error {
	settings := getMap(ix.Settings, "index")
	if e := scriptFieldsError(ix, sr); e != nil {
		return e
	}
	if len(sr.searchAfter) > 0 {
		if sr.scrollSet {
			return errSearchException("`search_after` cannot be used in a scroll context.")
		}
		if sr.from > 0 {
			return errSearchException("`from` parameter must be set to 0 when `search_after` is used.")
		}
		if !sortsOnFields(sr) {
			return errIllegalArgument("Sort must contain at least one field.")
		}
		if len(sr.searchAfter) != len(sr.sort) {
			return errIllegalArgument("search_after has %d value(s) but sort has %d.", len(sr.searchAfter), len(sr.sort))
		}
		if sr.collapseSet && (len(sr.sort) != 1 || sr.sort[0].field != sr.collapse) {
			return errSearchException("collapse field and sort field must be the same when use `collapse` in conjunction with `search_after`")
		}
		if e := checkSearchAfterValues(ix, sr); e != nil {
			return e
		}
	}
	if sr.slice != nil && !sr.scrollSet && !sr.pitSet {
		return errSearchException("`slice` cannot be used outside of a scroll context or PIT context")
	}
	if sr.storedNone && sr.sourceExplicit && !sr.source.disabled {
		return errSearchException("[stored_fields] cannot be disabled if [_source] is requested")
	}
	if sr.collapseSet {
		if sr.scrollSet {
			return errSearchException("cannot use `collapse` in a scroll context")
		}
		if len(sr.rescore) > 0 {
			return errSearchException("cannot use `collapse` in conjunction with `rescore`")
		}
		if sr.collapse == "" {
			return &Error{Status: http.StatusInternalServerError, Type: "null_pointer_exception", Reason: "Cannot invoke \"Object.hashCode()\" because \"key\" is null"}
		}
		f, _, ok := ix.Mapping.resolve(sr.collapse)
		if !ok {
			return errIllegalArgument("no mapping found for `%s` in order to collapse on", sr.collapse)
		}
		if collapseTypeSupported(f) && !getBool(f.Extra, "doc_values", true) {
			return errIllegalArgument("cannot collapse on field `%s` without `doc_values`", sr.collapse)
		}
	}
	window := sr.from + sr.size
	maxWindow := getInt(settings, "max_result_window", maxResultWindow)
	if window > maxWindow {
		if sr.scrollSet {
			return errIllegalArgument("Batch size is too large, size must be less than or equal to: [%d] but was [%d]. Scroll batch sizes cost as much memory as result windows so they are controlled by the [index.max_result_window] index level setting.", maxWindow, window)
		}
		return errIllegalArgument("Result window is too large, from + size must be less than or equal to: [%d] but was [%d]. See the scroll api for a more efficient way to request large data sets. This limit can be set by changing the [index.max_result_window] index level setting.", maxWindow, window)
	}
	if len(sr.rescore) > 0 {
		if sortsOnFields(sr) {
			return errIllegalArgument("Cannot use [sort] option in conjunction with [rescore].")
		}
		maxRescore := getInt(settings, "max_rescore_window", maxWindow)
		for _, r := range sr.rescore {
			if r.window > maxRescore {
				return errIllegalArgument("Rescore window [%d] is too large. It must be less than [%d]. This prevents allocating massive heaps for storing the results to be rescored. This limit can be set by changing the [index.max_rescore_window] index level setting.", r.window, maxRescore)
			}
		}
	}
	if sr.slice != nil {
		if limit := getInt(settings, "max_slices_per_scroll", 1024); sr.slice.max > limit {
			return errIllegalArgument("The number of slices [%d] is too large. It must be less than [%d]. This limit can be set by changing the [index.max_slices_per_scroll] index level setting.", sr.slice.max, limit)
		}
		if e := sliceFieldError(ix, sr.slice.field); e != nil {
			return e
		}
	}
	if sr.collapse != "" {
		if f, _, ok := ix.Mapping.resolve(sr.collapse); ok && !collapseTypeSupported(f) {
			return errIllegalArgument("unknown type for collapse field `%s`, only keywords and numbers are accepted", sr.collapse)
		}
	}
	if len(sr.searchAfter) > 0 {
		if e := nullSearchAfter(ix, sr); e != nil {
			return e
		}
	}
	return nil
}

// shard documents ------------------------------------------------------------

// shardDocOrdinals returns each document's shard and Lucene doc id within
// the shard: documents in index order, each after its nested objects.
func shardDocOrdinals(ix *Index) map[string][2]int64 {
	type entry struct {
		id    string
		seq   int64
		shard int
	}
	entries := make([]entry, 0, len(ix.docs))
	for id, d := range ix.docs {
		if d.nested != nil {
			continue
		}
		entries = append(entries, entry{id: id, seq: d.SeqNo, shard: shardOf(ix, id)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
	next := map[int]int64{}
	out := make(map[string][2]int64, len(entries))
	for _, e := range entries {
		next[e.shard] += int64(len(ix.children[e.id]))
		out[e.id] = [2]int64{int64(e.shard), next[e.shard]}
		next[e.shard]++
	}
	return out
}

// terminate_after -----------------------------------------------------------

// countableQuery reports queries whose hit count a shard reads without
// collecting (Weight#count): match_all, and single term, range and match
// queries on indices without deleted documents.
func countableQuery(q any, deletions bool) bool {
	if q == nil {
		return true
	}
	m, ok := q.(M)
	if !ok || len(m) != 1 {
		return false
	}
	for k, v := range m {
		switch k {
		case "match_all":
			return true
		case "term", "range", "match":
			return !deletions
		case "constant_score":
			if vm, ok := v.(M); ok {
				return countableQuery(vm["filter"], deletions)
			}
		}
	}
	return false
}

func indexHasDeletions(ix *Index) bool {
	return ix.seqNo+1 > int64(len(ix.docs))
}

// collectTerminated collects every shard in index order up to limit hits
// (EarlyTerminatingCollector): documents rejected by the post filter do not
// count, aggregations see every document before the termination.
func collectTerminated(hits []*hit, keep map[*Doc]bool, limit int) (kept, seen []*hit, terminated bool, counts map[shardKey]int) {
	keys, groups := groupByShard(hits)
	counts = map[shardKey]int{}
	for _, key := range keys {
		g := groups[key]
		sort.SliceStable(g, func(i, j int) bool { return g[i].doc.SeqNo < g[j].doc.SeqNo })
		collected := 0
		for _, h := range g {
			if keep == nil || keep[h.doc] {
				counts[key]++
				if collected >= limit {
					terminated = true
					break
				}
				collected++
				kept = append(kept, h)
			}
			seen = append(seen, h)
		}
	}
	return kept, seen, terminated, counts
}

// search -----------------------------------------------------------------------

// restSearchChecks are RestSearchAction's checks on the parsed request.
func restSearchChecks(expr string, sr *searchRequest, p Params) error {
	if _, err := searchIndicesOptions.withParams(p); err != nil {
		return err
	}
	if sr.totalAsInt && sr.trackTotalUpTo != nil && *sr.trackTotalUpTo != math.MaxInt32 && *sr.trackTotalUpTo != -1 {
		return errIllegalArgument("[rest_total_hits_as_int] cannot be used if the tracking of total hits is not accurate, got %d", *sr.trackTotalUpTo)
	}
	if sr.pitSet {
		var v validationErrors
		if expr != "" {
			v.add("[indices] cannot be used with point in time")
		}
		for _, k := range indicesOptionParams {
			if p.Has(k) {
				v.add("[indicesOptions] cannot be used with point in time")
				break
			}
		}
		if p.Has("routing") {
			v.add("[routing] cannot be used with point in time")
		}
		if p.Has("preference") {
			v.add("[preference] cannot be used with point in time")
		}
		return v.err()
	}
	return nil
}

// searchRequestChecks is SearchRequest.validate.
func searchRequestChecks(sr *searchRequest) error {
	var v validationErrors
	if sr.scrollSet {
		if sr.trackTotalUpTo != nil && *sr.trackTotalUpTo != math.MaxInt32 {
			v.add("disabling [track_total_hits] is not allowed in a scroll context")
		}
		if sr.from > 0 {
			v.add("using [from] is not allowed in a scroll context")
		}
		if sr.size == 0 {
			v.add("[size] cannot be [0] in a scroll context")
		}
		if len(sr.rescore) > 0 {
			v.add("using [rescore] is not allowed in a scroll context")
		}
		if sr.requestCache {
			v.add("[request_cache] cannot be used in a scroll context")
		}
	}
	if sr.pitSet && sr.scrollSet {
		v.add("using [point in time] is not allowed in a scroll context")
	}
	for _, s := range sr.sort {
		if s.field == "_shard_doc" {
			if sr.scrollSet {
				v.add("_shard_doc cannot be used with scroll. Use PIT + search_after instead.")
			}
			if !sr.pitSet {
				v.add("_shard_doc is only supported with point-in-time (PIT). Add a PIT or remove _shard_doc.")
			}
			break
		}
	}
	return v.err()
}

// Search implements _search.
func (c *Cluster) Search(expr string, body M, p Params) (Response, error) {
	return c.SearchSource(expr, nil, body, p)
}

// SearchSource implements _search for a body whose JSON text raw is known
// (the key order and locations of parse errors come from it).
func (c *Cluster) SearchSource(expr string, raw []byte, body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	sr, err := parseSearchSource(body, raw, p)
	if err != nil {
		return fail(err)
	}
	if err := restSearchChecks(expr, sr, p); err != nil {
		return fail(err)
	}
	return c.executeSearch(expr, sr, p)
}

// executeSearch runs a parsed search request (the transport action).
func (c *Cluster) executeSearch(expr string, sr *searchRequest, p Params) (Response, error) {
	if err := searchRequestChecks(sr); err != nil {
		return fail(err)
	}
	if err := searchPipelineError(sr, p); err != nil {
		return fail(err)
	}
	if sr.suggestions {
		return fail(errUnsupported("suggest"))
	}
	var ts []target
	if sr.pitSet {
		pts, release, err := c.pitTargets(sr)
		if err != nil {
			return fail(err)
		}
		defer release()
		ts = pts
	} else {
		var err error
		if ts, err = c.resolve(expr, resolveOpts(p)); err != nil {
			return fail(err)
		}
	}
	if err := c.checkBlocks(uniqueTargetIndices(ts), blockRead); err != nil {
		return fail(err)
	}
	if err := c.resolveIndexBoosts(sr, p); err != nil {
		return fail(err)
	}
	res, err := c.runSearch(ts, sr, p)
	if err != nil {
		if e, isErr := err.(*Error); isErr && e.failure != nil && e.failure.index == "" && len(ts) > 0 {
			e.failure.index = ts[0].ix.Name
		}
		return fail(err)
	}
	if sr.pitSet && len(ts) > 0 {
		res["pit_id"] = sr.pitID
	}
	return ok(res)
}

// pitTargets looks up the indices of a point in time.
func (c *Cluster) pitTargets(sr *searchRequest) ([]target, func(), error) {
	ctx, shards, err := decodePITID(sr.pitID)
	if err != nil {
		return nil, nil, err
	}
	if len(shards) == 0 {
		return nil, func() {}, nil
	}
	c.scrollMu.Lock()
	pit, found := c.pits[sr.pitID]
	now := c.now()
	if found && !now.Before(pit.expires) {
		delete(c.pits, sr.pitID)
		releasePIT(pit)
		found = false
	}
	var ts []target
	if found {
		if sr.pitKeepAliveSet {
			pit.expires = now.Add(sr.pitKeepAlive)
			pit.keepAlive = sr.pitKeepAlive
		}
		ts = append([]target(nil), pit.targets...)
		sr.pitShards = pit.shards
		sr.pitContext = ctx
		sr.pitLost = map[string]bool{}
		for name := range pit.lost {
			sr.pitLost[name] = true
		}
		// a concurrent PIT deletion must not close the snapshot while this
		// search is using it
		for _, t := range ts {
			t.ix.refs.Add(1)
		}
	}
	c.scrollMu.Unlock()
	if !found {
		s := shards[0]
		if _, exists := c.indices[s.index]; !exists {
			return nil, nil, &Error{Status: http.StatusNotFound, Type: "index_not_found_exception", Reason: "no such index [" + s.index + "]", Index: s.index}
		}
		return nil, nil, &Error{Status: http.StatusNotFound, Type: "search_phase_execution_exception", Reason: "all shards failed",
			failure: &shardFailure{shard: s.shard, index: s.index, cause: &Error{Status: http.StatusNotFound, Type: "search_context_missing_exception", Reason: fmt.Sprintf("No search context found for id [%d]", ctx)}}}
	}
	release := func() {
		for _, t := range ts {
			t.ix.release()
		}
	}
	for _, t := range ts {
		if cur := c.indices[t.ix.Name]; cur != nil && cur.stateClosed {
			release()
			return nil, nil, errIndexClosed(cur)
		}
	}
	return ts, release, nil
}

// includedShards returns the shards of an index a request searches: a point
// in time created with routing and a slice narrow them down.
func (sr *searchRequest) includedShards(ix *Index) []int {
	n := indexShardCount(ix)
	var out []int
	for s := 0; s < n; s++ {
		if sr.pitShards != nil {
			if allowed := sr.pitShards[ix.Name]; allowed != nil && !allowed[s] {
				continue
			}
		}
		if sr.prefShards != nil && !sr.prefShards[ix.Name][s] {
			continue
		}
		if sr.slice != nil {
			if in, _, _ := sliceSelection(sr.slice, n, s); !in {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// runSearch executes a search over resolved targets.
func (c *Cluster) runSearch(ts []target, sr *searchRequest, p Params) (M, error) {
	start := time.Now()
	if pref := p.Get("preference"); pref != "" {
		selected, err := preferenceShards(pref, ts)
		if err != nil {
			return nil, err
		}
		if selected != nil {
			// an index none of whose shards are selected is not searched
			sr.prefShards = selected
			searched := make([]target, 0, len(ts))
			for _, t := range ts {
				if len(selected[t.ix.Name]) > 0 {
					searched = append(searched, t)
				}
			}
			if len(searched) == 0 {
				return c.emptySearchResponse(sr, p, start), nil
			}
			ts = searched
		}
	}
	if sr.slice != nil && len(ts) > 0 {
		shards := 0
		for _, t := range ts {
			shards += len(sr.includedShards(t.ix))
		}
		if shards == 0 {
			// the slice selects no shard: nothing is searched
			return c.emptySearchResponse(sr, p, start), nil
		}
	}
	if sr.scrollSet {
		if max := c.maxKeepAlive("search.max_keep_alive"); sr.scroll > max {
			fs := make([]*shardFailure, 0, len(ts))
			for _, t := range ts {
				fs = append(fs, &shardFailure{index: t.ix.Name, cause: errKeepAliveTooLarge(sr.scroll, max, "search.max_keep_alive")})
			}
			if len(fs) > 0 {
				return nil, errShardFailures(fs)
			}
		}
	}
	hits, err := c.executeTargetsScoring(ts, sr.query, false, sr.terminateAfterSet, !sr.scoresNeeded())
	if err != nil {
		return nil, err
	}
	if err := c.checkHighlightQueries(ts, sr.highlight); err != nil {
		return nil, err
	}
	if len(sr.boostByIndex) > 0 {
		boosted := hits[:0]
		for _, h := range hits {
			if b, ok := sr.boostByIndex[h.ix.Name]; ok {
				if b < 0 {
					continue
				}
				h.score = float64(float32(h.score) * float32(b))
			}
			boosted = append(boosted, h)
		}
		hits = boosted
	}
	needShards := sr.terminateAfterSet || len(sr.rescore) > 0 || sr.slice != nil || sr.pitShards != nil || sr.prefShards != nil
	shardDocSort := false
	for _, s := range sr.sort {
		if s.field == "_shard_doc" {
			shardDocSort = true
		}
	}
	if needShards || shardDocSort || sr.collapse != "" {
		ordinals := map[*Index]map[string][2]int64{}
		for _, h := range hits {
			ord, ok := ordinals[h.ix]
			if !ok {
				ord = shardDocOrdinals(h.ix)
				ordinals[h.ix] = ord
			}
			o := ord[h.doc.ID]
			h.shard = int(o[0])
			h.shardDoc = o[0]<<32 | o[1]
		}
	}
	if err := c.sortHits(hits, sr); err != nil {
		return nil, err
	}
	// shard level checks: an index failing one fails its shards
	var failures []*shardFailure
	failedShards := 0
	totalShards := 0
	live := make(map[*Index]bool, len(ts))
	position := int64(0)
	for _, t := range ts {
		shards := sr.includedShards(t.ix)
		totalShards += len(shards)
		if sr.pitLost[t.ix.Name] {
			for i, s := range shards {
				failures = append(failures, &shardFailure{index: t.ix.Name, shard: s, cause: &Error{Status: http.StatusNotFound, Type: "search_context_missing_exception",
					Reason: fmt.Sprintf("No search context found for id [%d]", sr.pitContext+position+int64(i))}})
			}
			position += int64(indexShardCount(t.ix))
			failedShards += len(shards)
			continue
		}
		position += int64(indexShardCount(t.ix))
		if len(sr.aggs) > 0 {
			// the aggregations are built on every shard: an index they do
			// not apply to fails its shards, the others still answer
			if e := c.aggregationShardError(sr, t.ix); e != nil {
				for _, s := range shards {
					failures = append(failures, &shardFailure{index: t.ix.Name, shard: s, cause: e})
				}
				failedShards += len(shards)
				continue
			}
		}
		if e := c.checkShard(t.ix, sr); e != nil {
			for _, s := range shards {
				failures = append(failures, &shardFailure{index: t.ix.Name, shard: s, cause: e})
			}
			failedShards += len(shards)
			continue
		}
		live[t.ix] = true
	}
	if len(failures) > 0 {
		if failedShards == totalShards {
			return nil, errShardFailures(failures)
		}
		kept := hits[:0]
		for _, h := range hits {
			if live[h.ix] {
				kept = append(kept, h)
			}
		}
		hits = kept
	}
	if sr.slice != nil || sr.pitShards != nil || sr.prefShards != nil {
		fields := map[*Index]*Field{}
		kept := hits[:0]
		for _, h := range hits {
			if sr.prefShards != nil && !sr.prefShards[h.ix.Name][h.shard] {
				continue
			}
			if sr.pitShards != nil {
				if allowed := sr.pitShards[h.ix.Name]; allowed != nil && !allowed[h.shard] {
					continue
				}
			}
			if sr.slice != nil {
				in, id, max := sliceSelection(sr.slice, indexShardCount(h.ix), h.shard)
				if !in {
					continue
				}
				f, ok := fields[h.ix]
				if !ok && sr.slice.field != "_id" {
					f, _, _ = h.ix.Mapping.resolve(sr.slice.field)
					fields[h.ix] = f
				}
				if !sliceContains(sr.slice, f, h, id, max) {
					continue
				}
			}
			kept = append(kept, h)
		}
		hits = kept
	}
	if len(sr.scriptFields) > 0 && sr.size != 0 {
		// the scripts compiled: osmem cannot run them
		return nil, errUnsupported("[script_fields]")
	}
	if sr.suggestSet && sr.query == nil && len(sr.aggs) == 0 {
		// a suggest-only request runs no query (QueryPhase)
		hits = nil
	}
	if sr.minScore != nil {
		filtered := hits[:0]
		for _, h := range hits {
			if h.score >= *sr.minScore {
				filtered = append(filtered, h)
			}
		}
		hits = filtered
	}
	var keep map[*Doc]bool
	if sr.postFilter != nil {
		liveTargets := make([]target, 0, len(ts))
		for _, t := range ts {
			if live[t.ix] {
				liveTargets = append(liveTargets, t)
			}
		}
		pf, err := c.executeTargetsScoring(liveTargets, sr.postFilter, false, false, true)
		if err != nil {
			return nil, err
		}
		keep = map[*Doc]bool{}
		for _, h := range pf {
			keep[h.doc] = true
		}
		// inner_hits names are shared between the query and the post_filter
		if len(pf) > 0 && len(hits) > 0 {
			names := map[string]bool{}
			for _, r := range hits[0].inner {
				names[r.spec.name] = true
			}
			for _, r := range pf[0].inner {
				if names[r.spec.name] {
					return nil, errSearchPhase(&Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "[inner_hits] already contains an entry for key [" + r.spec.name + "]", Index: pf[0].ix.Name})
				}
			}
		}
	}
	aggHits := hits
	terminated := false
	var total int
	if sr.terminateAfterSet {
		all := len(hits)
		allHits := hits
		var counts map[shardKey]int
		hits, aggHits, terminated, counts = collectTerminated(hits, keep, sr.terminateAfter)
		total = len(hits)
		if len(sr.aggs) > 0 {
			// aggregations a shard precomputes from the index when the query
			// matches every document see all documents despite the
			// termination (Aggregator.tryPrecomputeAggregationForLeaf)
			whole := map[*Index]bool{}
			for _, t := range ts {
				if live[t.ix] && t.filter == nil && !indexHasDeletions(t.ix) && queryMatchesAll(sr.query, t.ix) && aggsPrecomputable(sr.aggs, t.ix) {
					whole[t.ix] = true
				}
			}
			if len(whole) > 0 {
				seen := make(map[*hit]bool, len(aggHits))
				for _, h := range aggHits {
					seen[h] = true
				}
				for _, h := range allHits {
					if whole[h.ix] && !seen[h] {
						aggHits = append(aggHits, h)
					}
				}
			}
		}
		// the hit count is read without collecting (Weight#count) only for a
		// positive limit
		if sr.size == 0 && keep == nil && sr.minScore == nil && sr.terminateAfter > 0 {
			filtered := false
			countable := true
			for _, t := range ts {
				filtered = filtered || t.filter != nil
				countable = countable && countableQueryOn(sr.query, t.ix, indexHasDeletions(t.ix))
			}
			if !filtered && countable {
				total = all
				terminated = false
				for _, n := range counts {
					if n > sr.terminateAfter {
						terminated = true
					}
				}
			}
		}
	} else {
		if keep != nil {
			filtered := make([]*hit, 0, len(hits))
			for _, h := range hits {
				if keep[h.doc] {
					filtered = append(filtered, h)
				}
			}
			hits = filtered
		}
		total = len(hits)
	}
	if len(sr.rescore) > 0 && len(hits) > 0 {
		if hits, err = c.rescoreHits(hits, sr); err != nil {
			return nil, err
		}
	}
	if sr.terminateAfterSet || len(sr.rescore) > 0 {
		if err := c.sortHits(hits, sr); err != nil {
			return nil, err
		}
	}
	if sr.collapse != "" {
		if hits, err = c.collapseHits(hits, sr.collapse); err != nil {
			return nil, err
		}
	}
	if len(sr.searchAfter) > 0 {
		liveTargets := make([]target, 0, len(ts))
		for _, t := range ts {
			if live[t.ix] {
				liveTargets = append(liveTargets, t)
			}
		}
		after, err := c.normalizeSearchAfter(sr.searchAfter, sr.sort, liveTargets)
		if err != nil {
			return nil, err
		}
		// a new slice: the aggregations read the hits this one filters
		filtered := make([]*hit, 0, len(hits))
		for _, h := range hits {
			if compareTuples(h.keys, after, sr.sort) > 0 {
				filtered = append(filtered, h)
			}
		}
		hits = filtered
	}
	var aggResult M
	if len(sr.aggs) > 0 {
		// [aggs hook] the searched targets and typed_keys (aggs.go)
		liveTargets := make([]target, 0, len(ts))
		for _, t := range ts {
			if live[t.ix] {
				liveTargets = append(liveTargets, t)
			}
		}
		aggResult, err = c.runAggregations(sr, aggHits, liveTargets, p)
		if err != nil {
			return nil, err
		}
	}
	shardsSection := M{"total": totalShards, "successful": totalShards - failedShards, "skipped": preFilterSkipped(sr, ts, live), "failed": failedShards}
	if len(failures) > 0 {
		sortShardFailures(failures)
		list := make([]any, 0, len(failures))
		for _, f := range groupShardFailures(failures) {
			list = append(list, M{"shard": f.shard, "index": f.index, "node": "osmem", "reason": f.cause.content()})
		}
		shardsSection["failures"] = list
	}
	res := M{"took": int(time.Since(start).Milliseconds()), "timed_out": false, "_shards": shardsSection}
	if c.phaseTookEnabled(p) {
		res["phase_took"] = phaseTookSection()
	}
	if sr.profile {
		res["profile"] = c.profileSection(ts, sr, live)
	}
	switch {
	case sr.terminateAfterSet:
		res["terminated_early"] = terminated
	case sr.size == 0 && len(sr.aggs) > 0 && !sr.scrollSet:
		// a request collecting only aggregations reports an early
		// termination of its (empty) top hits collection
		res["terminated_early"] = true
	}
	if sr.scrollSet {
		page := hits
		if len(page) > sr.size {
			page = page[:sr.size]
		}
		res["_scroll_id"] = c.newScroll(hits[len(page):], total, sr, ts)
		if res["hits"], err = c.hitsJSON(page, sr, total); err != nil {
			return nil, err
		}
	} else {
		page := hits
		if sr.from < len(page) {
			page = page[sr.from:]
		} else {
			page = nil
		}
		if len(page) > sr.size {
			page = page[:sr.size]
		}
		if res["hits"], err = c.hitsJSON(page, sr, total); err != nil {
			return nil, err
		}
	}
	if aggResult != nil {
		res["aggregations"] = aggResult
	}
	if len(ts) == 0 {
		// nothing to search: the empty response reports max_score 0
		if hm, ok := res["hits"].(M); ok {
			hm["max_score"] = Float(0)
		}
	}
	return res, nil
}

// collapseHits keeps the first hit for each value of a field and records
// the members of each group on it for inner_hits. Documents without a
// value form one group and report no fields, as on OpenSearch. A document
// with several values fails its shard while collecting.
func (c *Cluster) collapseHits(hits []*hit, field string) ([]*hit, error) {
	var multi *hit
	for _, h := range hits {
		if len(h.ix.fieldValues(h.doc, field)) > 1 {
			if multi == nil || h.ix.Name < multi.ix.Name || (h.ix == multi.ix && h.shardDoc < multi.shardDoc) {
				multi = h
			}
		}
	}
	if multi != nil {
		return nil, errShardFailures([]*shardFailure{{index: multi.ix.Name, shard: multi.shard, cause: &Error{Status: http.StatusInternalServerError, Type: "illegal_state_exception",
			Reason: fmt.Sprintf("failed to collapse %d, the collapse field must be single valued", multi.shardDoc&0xffffffff)}}})
	}
	groups := map[string]*hit{}
	out := make([]*hit, 0, len(hits))
	for _, h := range hits {
		f, _, ok := h.ix.Mapping.resolve(field)
		if !ok {
			continue
		}
		// a field inside a nested object has no value on the root, so every
		// document lands in the group without a value, as on OpenSearch
		vals := h.ix.fieldValues(h.doc, field)
		key := "\x00missing"
		if len(vals) == 1 {
			key = fmt.Sprint(vals[0])
			dv, err := docValueOutput(f, vals, "")
			if err != nil {
				return nil, err
			}
			h.fields = M{field: dv}
		}
		if g, ok := groups[key]; ok {
			g.group = append(g.group, h)
			continue
		}
		h.group = []*hit{h}
		groups[key] = h
		out = append(out, h)
	}
	return out, nil
}

// scroll ---------------------------------------------------------------------

type scrollState struct {
	remaining []*hit
	sr        *searchRequest
	total     int
	targets   []target
	expires   time.Time
	keepAlive time.Duration
}

type pitState struct {
	lost      map[string]bool // indices whose reader contexts were freed
	targets   []target
	shards    map[string]map[int]bool // shards selected by routing (nil: all)
	created   time.Time
	expires   time.Time
	keepAlive time.Duration
}

var scrollCounter atomic.Int64

func (c *Cluster) newScroll(remaining []*hit, total int, sr *searchRequest, ts []target) string {
	shards := targetShardContexts(ts)
	ctx := scrollCounter.Add(int64(len(shards)) + 1)
	id := encodeScrollID(ctx, shards)
	c.scrollMu.Lock()
	defer c.scrollMu.Unlock()
	c.scrolls[id] = &scrollState{remaining: remaining, sr: sr, total: total, targets: ts, expires: c.now().Add(sr.scroll), keepAlive: sr.scroll}
	return id
}

func errUnknownBodyParameter(name string, v any) *Error {
	return errIllegalArgument("Unknown parameter [%s] in request body or parameter is of the wrong type[%s] ", name, jsonTokenName(v))
}

func errScrollContextMissing(ctx int64) *Error {
	return &Error{Status: http.StatusNotFound, Type: "search_phase_execution_exception", Reason: "all shards failed",
		failure: &shardFailure{shard: -1, cause: &Error{Status: http.StatusNotFound, Type: "search_context_missing_exception", Reason: fmt.Sprintf("No search context found for id [%d]", ctx)}}}
}

// Scroll implements POST /_search/scroll.
func (c *Cluster) Scroll(body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id := p.Get("scroll_id")
	var keep *time.Duration
	if s, ok := p["scroll"]; ok {
		d, err := parseTimeValue(s, "scroll")
		if err != nil {
			return fail(err)
		}
		keep = &d
	}
	for _, k := range keysSorted(body) {
		v := body[k]
		switch k {
		case "scroll_id":
			s, isString := v.(string)
			if !isString {
				return fail(errUnknownBodyParameter(k, v))
			}
			id = s
		case "scroll":
			s, isString := v.(string)
			if !isString {
				return fail(errUnknownBodyParameter(k, v))
			}
			d, err := parseTimeValue(s, "scroll")
			if err != nil {
				return fail(err)
			}
			keep = &d
		default:
			return fail(errUnknownBodyParameter(k, v))
		}
	}
	if _, hasParam := p["scroll_id"]; id == "" && !hasParam {
		return fail(errActionRequestValidation("scrollId is missing"))
	}
	ctx, node, err := decodeScrollID(id)
	if err != nil {
		return fail(err)
	}
	if node != osmemNodeID {
		cause := errIllegalArgument("scroll_id references node [%s] which was not found in the cluster", node)
		return fail(&Error{Status: http.StatusBadRequest, Type: "search_phase_execution_exception", Reason: "all shards failed",
			failure: &shardFailure{shard: -1, cause: cause}, Cause: cause})
	}
	c.scrollMu.Lock()
	defer c.scrollMu.Unlock()
	st, found := c.scrolls[id]
	if !found || c.now().After(st.expires) {
		delete(c.scrolls, id)
		return fail(errScrollContextMissing(ctx))
	}
	if keep != nil {
		if max := c.maxKeepAlive("search.max_keep_alive"); *keep > max {
			fs := make([]*shardFailure, 0, len(st.targets))
			for _, t := range st.targets {
				fs = append(fs, &shardFailure{index: t.ix.Name, cause: errKeepAliveTooLarge(*keep, max, "search.max_keep_alive")})
			}
			if len(fs) > 0 {
				return fail(errShardFailures(fs))
			}
		}
		st.keepAlive = *keep
	}
	st.expires = c.now().Add(st.keepAlive)
	page := st.remaining
	if len(page) > st.sr.size {
		page = page[:st.sr.size]
	}
	st.remaining = st.remaining[len(page):]
	sr := *st.sr
	sr.totalAsInt = p.Bool("rest_total_hits_as_int", false)
	hits, err := c.hitsJSON(page, &sr, st.total)
	if err != nil {
		return fail(err)
	}
	res := M{"took": 1, "timed_out": false, "_shards": searchShards(st.targets), "hits": hits}
	if keep != nil {
		res["_scroll_id"] = id
	}
	return ok(res)
}

// ClearScroll implements DELETE /_search/scroll.
func (c *Cluster) ClearScroll(body M, ids string) (Response, error) {
	list := splitCommaJava(ids)
	if len(body) > 0 {
		list = nil
		for _, k := range keysSorted(body) {
			v := body[k]
			if k != "scroll_id" {
				return fail(errUnknownBodyParameter(k, v))
			}
			switch t := v.(type) {
			case []any:
				for _, e := range t {
					switch e.(type) {
					case M, []any:
						return fail(errIllegalArgument("scroll_id array element should only contain scroll_id"))
					}
					list = append(list, fmt.Sprint(e))
				}
			case M:
				return fail(errIllegalArgument("scroll_id element should only contain scroll_id"))
			default:
				list = append(list, fmt.Sprint(t))
			}
		}
	}
	if len(list) == 0 {
		return fail(errActionRequestValidation("no scroll ids specified"))
	}
	c.scrollMu.Lock()
	defer c.scrollMu.Unlock()
	freed := 0
	if len(list) == 1 && list[0] == "_all" {
		freed = len(c.scrolls)
		c.scrolls = map[string]*scrollState{}
	} else {
		for _, id := range list {
			if _, _, err := decodeScrollID(id); err != nil {
				return fail(err)
			}
		}
		for _, id := range list {
			if _, found := c.scrolls[id]; found {
				delete(c.scrolls, id)
				freed++
			}
		}
	}
	status := http.StatusOK
	if freed == 0 {
		status = http.StatusNotFound
	}
	return Response{Status: status, Body: M{"succeeded": true, "num_freed": freed}}, nil
}

// point in time ------------------------------------------------------------------

// CreatePIT implements POST /{index}/_search/point_in_time.
func (c *Cluster) CreatePIT(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	raw, hasKeepAlive := p["keep_alive"]
	var keep time.Duration
	if hasKeepAlive {
		d, err := parseTimeValue(raw, "keep_alive")
		if err != nil {
			return fail(err)
		}
		keep = d
	}
	if _, err := searchIndicesOptions.withParams(p); err != nil {
		return fail(err)
	}
	if !hasKeepAlive {
		return fail(errActionRequestValidation("keep alive not specified"))
	}
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	if err := c.checkBlocks(uniqueTargetIndices(ts), blockRead); err != nil {
		return fail(err)
	}
	if max := c.maxKeepAlive("point_in_time.max_keep_alive"); keep > max {
		return fail(errKeepAliveTooLarge(keep, max, "point_in_time.max_keep_alive"))
	}
	var pitShards []shardContext
	var selected map[string]map[int]bool
	routing := splitList(p.Get("routing"))
	for _, t := range ts {
		n := indexShardCount(t.ix)
		allowed := map[int]bool{}
		for _, r := range routing {
			allowed[shardOf(t.ix, r)] = true
		}
		for s := 0; s < n; s++ {
			if len(routing) > 0 && !allowed[s] {
				continue
			}
			pitShards = append(pitShards, shardContext{index: t.ix.Name, uuid: t.ix.UUID, shard: s})
		}
		if len(routing) > 0 {
			if selected == nil {
				selected = map[string]map[int]bool{}
			}
			selected[t.ix.Name] = allowed
		}
	}
	if len(pitShards) == 0 {
		return fail(&Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "invalid id: [null]",
			Cause: &Error{Type: "null_pointer_exception", Reason: "Cannot invoke \"String.getBytes(java.nio.charset.Charset)\" because \"src\" is null"}})
	}
	ctx := scrollCounter.Add(int64(len(pitShards)) + 1)
	id := encodePITID(ctx, pitShards)
	for _, t := range ts {
		// Retaining the index makes subsequent writes copy-on-write, preserving
		// the exact index state captured by this PIT.
		t.ix.refs.Add(1)
	}
	now := c.now()
	c.scrollMu.Lock()
	c.pits[id] = &pitState{targets: append([]target(nil), ts...), shards: selected, created: now, expires: now.Add(keep), keepAlive: keep}
	c.scrollMu.Unlock()
	return ok(M{"pit_id": id, "_shards": shards(len(pitShards)), "creation_time": now.UnixMilli()})
}

// DeletePIT implements DELETE /_search/point_in_time.
func (c *Cluster) DeletePIT(body M, all bool) (Response, error) {
	c.scrollMu.Lock()
	defer c.scrollMu.Unlock()
	pits := []any{}
	if all {
		ids := make([]string, 0, len(c.pits))
		for id := range c.pits {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			releasePIT(c.pits[id])
			delete(c.pits, id)
			pits = append(pits, M{"pit_id": id, "successful": true})
		}
		return ok(M{"pits": pits})
	}
	var list []string
	for _, k := range keysSorted(body) {
		v := body[k]
		if k != "pit_id" {
			return fail(errUnknownBodyParameter(k, v))
		}
		switch t := v.(type) {
		case []any:
			for _, e := range t {
				switch e.(type) {
				case M, []any:
					return fail(errIllegalArgument("pit_id array element should only contain pit_id"))
				}
				list = append(list, fmt.Sprint(e))
			}
		case M:
			return fail(errIllegalArgument("pit_id element should only contain pit_id"))
		default:
			list = append(list, fmt.Sprint(t))
		}
	}
	if len(list) == 0 {
		return fail(errActionRequestValidation("no pit ids specified"))
	}
	valid := map[string]bool{}
	for _, id := range list {
		_, shards, err := decodePITID(id)
		if err != nil {
			return fail(err)
		}
		if len(shards) > 0 {
			valid[id] = true
		}
	}
	ids := make([]string, 0, len(valid))
	for id := range valid {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if st, found := c.pits[id]; found {
			releasePIT(st)
			delete(c.pits, id)
		}
		pits = append(pits, M{"pit_id": id, "successful": true})
	}
	return ok(M{"pits": pits})
}

func releasePIT(st *pitState) {
	if st == nil {
		return
	}
	for _, t := range st.targets {
		t.ix.release()
	}
}

// ListPITs implements GET /_search/point_in_time/_all.
func (c *Cluster) ListPITs() (Response, error) {
	c.scrollMu.Lock()
	defer c.scrollMu.Unlock()
	now := c.now()
	ids := make([]string, 0, len(c.pits))
	for id, st := range c.pits {
		if !now.Before(st.expires) {
			delete(c.pits, id)
			releasePIT(st)
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	pits := make([]any, 0, len(ids))
	for _, id := range ids {
		st := c.pits[id]
		pits = append(pits, M{"pit_id": id, "creation_time": st.created.UnixMilli(), "keep_alive": st.keepAlive.Milliseconds()})
	}
	return ok(M{"pits": pits})
}

// count -----------------------------------------------------------------------

// Count implements _count.
func (c *Cluster) Count(expr string, body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// RestActions.getQueryContent: the body may only hold a query
	for _, key := range sortedMKeys(body) {
		if key != "query" {
			return fail(errParsing("request does not support [%s]", key).at(keyTok(body, key)))
		}
	}
	if _, isObject := body["query"].(M); isObject {
		if _, perr := parseQuery(body["query"]); perr != nil && isQueryParseFailure(perr) {
			return fail(queryContentError(perr))
		}
	}
	terminateAfter, terminateSet := 0, false
	if n, has, err := parseIntParam(p, "terminate_after"); err != nil {
		return fail(err)
	} else if has {
		if n < 0 {
			return fail(errIllegalArgument("terminateAfter must be > 0"))
		}
		terminateAfter, terminateSet = n, n > 0
	}
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	if err := c.checkBlocks(uniqueTargetIndices(ts), blockRead); err != nil {
		return fail(err)
	}
	selected, err := preferenceShards(p.Get("preference"), ts)
	if err != nil {
		return fail(err)
	}
	if selected != nil {
		searched := make([]target, 0, len(ts))
		for _, t := range ts {
			if len(selected[t.ix.Name]) > 0 {
				searched = append(searched, t)
			}
		}
		ts = searched
	}
	var q any
	if body != nil {
		q = body["query"]
	}
	if qs := p.Get("q"); qs != "" {
		q = M{"query_string": M{"query": qs}}
	}
	hits, err := c.executeTargetsScoring(ts, q, false, false, true)
	if err != nil {
		return fail(err)
	}
	shardsSection := searchShards(ts)
	if selected != nil {
		searchedShards := 0
		for _, t := range ts {
			searchedShards += len(selected[t.ix.Name])
		}
		kept := hits[:0]
		for _, h := range hits {
			if selected[h.ix.Name][shardOf(h.ix, h.doc.ID)] {
				kept = append(kept, h)
			}
		}
		hits = kept
		shardsSection = shards(searchedShards)
	}
	res := M{"count": len(hits), "_shards": shardsSection}
	if terminateSet {
		for _, h := range hits {
			h.shard = shardOf(h.ix, h.doc.ID)
		}
		kept, _, terminated, counts := collectTerminated(hits, nil, terminateAfter)
		filtered := false
		countable := true
		for _, t := range ts {
			filtered = filtered || t.filter != nil
			countable = countable && countableQueryOn(q, t.ix, indexHasDeletions(t.ix))
		}
		count := len(kept)
		if !filtered && countable {
			count = len(hits)
			terminated = false
			for _, n := range counts {
				if n > terminateAfter {
					terminated = true
				}
			}
		}
		res["count"] = count
		res["terminated_early"] = terminated
	}
	return ok(res)
}

// msearch -----------------------------------------------------------------------

// msearchItem is one request of a multi search.
type msearchItem struct {
	expr   string
	sr     *searchRequest
	params Params
	// line is the search body line and body its decoded value, where parse
	// errors raised while the search runs are located
	line []byte
	body M
}

// MultiSearch implements _msearch (MultiSearchRequest.readMultiLineFormat).
// Framing, header and body parse errors fail the whole request; execution
// errors are reported per search.
func (c *Cluster) MultiSearch(expr string, data []byte, p Params) (Response, error) {
	if n, has, err := parseIntParam(p, "max_concurrent_searches"); err != nil {
		return fail(err)
	} else if has && n < 1 {
		return fail(errIllegalArgument("maxConcurrentSearchRequests must be positive"))
	}
	if st, ok := p["search_type"]; ok && st != "query_then_fetch" && st != "dfs_query_then_fetch" {
		return fail(errIllegalArgument("No search type for [%s]", st))
	}
	if len(data) == 0 {
		return fail(&Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "request body or source parameter is required"})
	}
	nextMarker := func(from int) (int, error) {
		if i := bytes.IndexByte(data[from:], '\n'); i >= 0 {
			return from + i, nil
		}
		if from != len(data) {
			return -1, errIllegalArgument("The msearch request must be terminated by a newline [\n]")
		}
		return -1, nil
	}
	var items []msearchItem
	from := 0
	for {
		marker, err := nextMarker(from)
		if err != nil {
			return fail(err)
		}
		if marker == -1 {
			break
		}
		if marker == 0 {
			from = 1
			continue
		}
		item := msearchItem{expr: expr, params: Params{}}
		for k, v := range p {
			item.params[k] = v
		}
		if header := data[from:marker]; len(header) > 0 {
			hm, err := decodeObject(header)
			if err != nil {
				return fail(err)
			}
			if err := applyMsearchHeader(&item, hm); err != nil {
				return fail(err)
			}
		}
		from = marker + 1
		marker, err = nextMarker(from)
		if err != nil {
			return fail(err)
		}
		if marker == -1 {
			break
		}
		line := data[from:marker]
		from = marker + 1
		if len(bytes.TrimSpace(line)) == 0 {
			tok := FirstJSONToken(line, 0)
			return fail(&Error{Status: http.StatusBadRequest, Type: "parsing_exception", Reason: "Expected [START_OBJECT] but found [null]",
				Extra: map[string]any{"line": tok.Line, "col": tok.Col}})
		}
		body, err := decodeObject(line)
		if err != nil {
			return fail(err)
		}
		sr, err := parseSearchSource(body, line, item.params)
		if err != nil {
			// each search body is parsed on its own: locations are relative
			// to its line
			LocateError(err, line, body)
			return fail(err)
		}
		item.line, item.body = line, body
		if err := restSearchChecks(item.expr, sr, item.params); err != nil {
			return fail(err)
		}
		item.sr = sr
		items = append(items, item)
	}
	if len(items) == 0 {
		return fail(errActionRequestValidation("no requests added"))
	}
	responses := make([]any, 0, len(items))
	for _, item := range items {
		res, err := c.searchItem(item)
		if err != nil {
			e, ok := err.(*Error)
			if !ok {
				e = &Error{Status: http.StatusInternalServerError, Type: "exception", Reason: err.Error()}
			}
			// queries and aggregations are parsed with the request body on
			// OpenSearch: their parse errors fail the whole multi search
			LocateError(e, item.line, item.body)
			switch e.Type {
			case "parsing_exception", "x_content_parse_exception", "named_object_not_found_exception", "json_parse_exception":
				return fail(e)
			}
			responses = append(responses, e.Body())
			continue
		}
		rb := res.Body.(M)
		rb["status"] = 200
		responses = append(responses, rb)
	}
	return ok(M{"took": 1, "responses": responses})
}

func (c *Cluster) searchItem(item msearchItem) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.executeSearch(item.expr, item.sr, item.params)
}

// applyMsearchHeader reads the metadata line of a multi search request.
func applyMsearchHeader(item *msearchItem, header M) error {
	for _, k := range keysSorted(header) {
		v := header[k]
		switch k {
		case "index", "indices":
			item.expr = headerString(v)
		case "search_type", "searchType":
			if s := headerString(v); s != "query_then_fetch" && s != "dfs_query_then_fetch" {
				return errIllegalArgument("No search type for [%s]", s)
			}
		case "request_cache", "requestCache", "ccs_minimize_roundtrips", "ccsMinimizeRoundtrips", "allow_partial_search_results", "phase_took":
			if _, err := optionBool(Params{k: headerString(v)}, k, false); err != nil {
				return err
			}
			if k == "request_cache" || k == "requestCache" {
				item.params["request_cache"] = headerString(v)
			}
			if k == "phase_took" {
				item.params["phase_took"] = headerString(v)
			}
		case "preference", "routing":
			item.params[k] = headerString(v)
		case "expand_wildcards", "expandWildcards", "ignore_unavailable", "ignoreUnavailable", "allow_no_indices", "allowNoIndices", "ignore_throttled", "ignoreThrottled":
		case "cancel_after_time_interval", "cancelAfterTimeInterval":
			if _, err := parseTimeValue(headerString(v), k); err != nil {
				return err
			}
		default:
			return errIllegalArgument("key [%s] is not supported in the metadata section", k)
		}
	}
	if _, err := searchIndicesOptions.withValues(header); err != nil {
		return err
	}
	for _, pair := range [][2]string{{"expand_wildcards", "expandWildcards"}, {"ignore_unavailable", "ignoreUnavailable"}, {"allow_no_indices", "allowNoIndices"}, {"ignore_throttled", "ignoreThrottled"}} {
		for _, key := range pair {
			if v, ok := header[key]; ok {
				item.params[pair[0]] = headerString(v)
			}
		}
	}
	return nil
}

// validate query ------------------------------------------------------------------

// validateQueryOptions are ValidateQueryRequest's indices options.
var validateQueryOptions = indicesOptions{expandOpen: true}

// ValidateQuery checks whether a query can be built for each resolved index
// without executing it against documents. body is the decoded request body
// (nil when there is none).
func (c *Cluster) ValidateQuery(expr string, body M, p Params) (Response, error) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	return c.ValidateQueryJSON(expr, raw, p)
}

// validateQueryContent reads a _validate/query body the way
// RestActions.getQueryContent does: an object holding only a query.
func validateQueryContent(raw []byte) (any, *Error) {
	tok := FirstJSONToken(raw, 0)
	switch tok.Name {
	case "null":
		return nil, nil
	case "START_OBJECT":
	case "":
		return nil, &Error{Status: http.StatusBadRequest, Type: "parsing_exception", Reason: "Failed to parse", Cause: unrecognizedTokenError(raw, tok.Offset)}
	default:
		return nil, &Error{Status: http.StatusBadRequest, Type: "parsing_exception", Reason: "Expected [START_OBJECT] but found [" + tok.Name + "]",
			Extra: map[string]any{"line": tok.Line, "col": tok.Col}}
	}
	m, err := decodeObject(raw)
	if err != nil {
		cause, isErr := err.(*Error)
		if !isErr {
			cause = errJSONParse(raw, err)
		}
		return nil, &Error{Status: http.StatusBadRequest, Type: "parsing_exception", Reason: "Failed to parse", Cause: cause}
	}
	r := bodyReader{raw: raw}
	var q any
	for _, k := range r.keys(m) {
		var perr *Error
		switch _, isObject := m[k].(M); {
		case k != "query":
			perr = errParsing("request does not support [%s]", k).at(keyTok(m, k))
		case !isObject:
			perr = errParsing("[_na] query malformed, must start with start_object").at(valueTok(m, k))
		default:
			if _, perr = parseQuery(m[k]); perr != nil && isQueryParseFailure(perr) {
				perr = queryContentError(perr)
			}
		}
		if perr != nil {
			// parse errors report where the parser stopped in the body
			LocateError(perr, raw, m)
			return nil, perr
		}
		q = m[k]
	}
	return q, nil
}

// ValidateQueryJSON implements _validate/query for the raw request body
// (RestValidateQueryAction, TransportValidateQueryAction): a body that
// fails to parse makes the query invalid rather than failing the request,
// and each index validates the query on one shard (every shard with
// all_shards).
func (c *Cluster) ValidateQueryJSON(expr string, raw []byte, p Params) (Response, error) {
	explain := p.Bool("explain", false)
	rewrite := p.Bool("rewrite", false)
	allShards := p.Bool("all_shards", false)
	var q any = M{"match_all": M{}}
	switch {
	case len(raw) > 0:
		content, perr := validateQueryContent(raw)
		if perr != nil {
			out := M{"valid": false}
			if explain {
				if perr.Type == "parsing_exception" {
					out["error"] = openSearchDetailedMessage(perr, nil)
				} else {
					out["error"] = perr.Reason
				}
			}
			return ok(out)
		}
		q = content
	case p.Has("q"):
		queryString := M{"query": p.Get("q")}
		if value := p.Get("df"); value != "" {
			queryString["default_field"] = value
		}
		if value := p.Get("default_operator"); value != "" {
			queryString["default_operator"] = value
		}
		if value := p.Get("analyzer"); value != "" {
			queryString["analyzer"] = value
		}
		q = M{"query_string": queryString}
	}
	if q == nil {
		return fail(errActionRequestValidation("query cannot be null"))
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	o, err := validateQueryOptions.withParams(p)
	if err != nil {
		return fail(err)
	}
	// the shards validate the query with the alias filters
	ts, err := c.newExprResolver().targets(expr, o, true)
	if err != nil {
		return fail(err)
	}
	// a closed index is blocked: the request fails on the coordinating node
	var blocked strings.Builder
	for _, t := range ts {
		if t.ix.stateClosed {
			fmt.Fprintf(&blocked, "index [%s] blocked by: [FORBIDDEN/4/index closed];", t.ix.Name)
		}
	}
	if blocked.Len() > 0 {
		return ok(M{"_shards": M{"total": 0, "successful": 0, "failed": 0}, "valid": false,
			"explanations": []any{M{"valid": false, "error": blocked.String()}}})
	}
	uuid := func(name string) string {
		if ix := c.indices[name]; ix != nil {
			return ix.UUID
		}
		return "_na_"
	}
	listed := explain || rewrite || allShards
	valid := true
	explanations := make([]any, 0)
	totalShards := 0
	seen := map[*Index]bool{}
	for _, t := range ts {
		if seen[t.ix] {
			continue
		}
		seen[t.ix] = true
		shardIDs := []int{0}
		if allShards {
			shardIDs = shardIDs[:0]
			for s := 0; s < indexShardCount(t.ix); s++ {
				shardIDs = append(shardIDs, s)
			}
		}
		query := q
		if t.filter != nil {
			// the alias filter restricts the query
			query = M{"bool": M{"must": []any{q}, "filter": []any{t.filter}}}
		}
		for _, shard := range shardIDs {
			totalShards++
			entry := M{"index": t.ix.Name}
			if allShards {
				entry["shard"] = shard
			}
			// validation creates the query without running it
			qb := &queryBuilder{c: c, ix: t.ix, noScores: true}
			n, perr := parseQuery(query)
			var buildErr error
			if perr != nil {
				buildErr = perr
			} else if _, buildErr = qb.toQuery(n); buildErr == nil {
				buildErr = qb.checkInnerNames()
			}
			if buildErr != nil {
				valid = false
				entry["valid"] = false
				if e, isErr := buildErr.(*Error); isErr {
					if e.Type == "query_shard_exception" && e.Index == "" {
						e.Index = t.ix.Name
					}
					entry["error"] = openSearchDetailedMessage(e, uuid)
				} else {
					entry["error"] = buildErr.Error()
				}
			} else {
				entry["valid"] = true
				d := &luceneDescriber{qb: &queryBuilder{c: c, ix: t.ix}, rewrite: rewrite || allShards}
				entry["explanation"] = d.describeQuery(n)
			}
			if listed {
				explanations = append(explanations, entry)
			}
		}
	}
	result := M{"_shards": M{"total": totalShards, "successful": totalShards, "failed": 0}, "valid": valid}
	if listed {
		result["explanations"] = explanations
	}
	return ok(result)
}
