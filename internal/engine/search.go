package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
)

const maxResultWindow = 10000

// hit is one matched document.
type hit struct {
	ix        *Index
	doc       *Doc
	score     float64
	locations search.FieldTermLocationMap
	keys      []sortKey          // comparable sort keys
	order     int                // position before sorting, the last tie-breaker
	sortOut   []any              // sort values as reported in the response
	fields    M                  // collapse field values
	group     []*hit             // collapse: every hit of the group, this one first
	inner     []*innerHitsResult // inner_hits of the nested queries
	parent    *hit               // nested aggregation: the hit this object was taken from
	shard     int                // shard the document is routed to
	shardDoc  int64              // _shard_doc: shard << 32 | Lucene doc id
	filter    M                  // the index's alias filter, folded into _explanation
}

type sortSpec struct {
	field        string
	desc         bool
	missing      any // "_last", "_first" or a value
	mode         string
	unmappedType string
	nested       *nestedSort
	geo          *geoSortSpec
}

// nestedSort is the nested option of a sort on a field inside a nested
// object.
type nestedSort struct {
	path    string
	filter  any
	matched map[*Index]map[string]bool // objects matching filter, per index
}

type searchRequest struct {
	query           any
	postFilter      any
	size            int
	from            int
	sort            []sortSpec
	explicitSort    bool
	source          sourceFilter
	aggs            M
	trackTotal      int // -1 exact, 0 disabled, N cap
	searchAfter     []any
	highlight       M
	minScore        *float64
	fields          []any
	docvalueFields  []any
	storedFields    []any
	version         bool
	seqNoTerm       bool
	trackScores     bool
	terminateAfter  int
	collapse        string
	collapseInner   []*innerHitsSpec
	storedNone      bool
	storedFieldsSet bool // stored_fields given: _source is only returned when asked for
	sourceExplicit  bool
	scroll          time.Duration
	pitID           string
	pitKeepAlive    time.Duration
	pitKeepAliveSet bool
	totalAsInt      bool

	terminateAfterSet bool // terminate_after differs from the default 0
	trackTotalUpTo    *int // track_total_hits as given: true is MaxInt32, false -1
	scrollSet         bool
	requestCache      bool
	pitSet            bool
	pitShards         map[string]map[int]bool // shards a point in time selected by routing
	pitLost           map[string]bool         // indices whose point in time contexts were freed
	pitContext        int64                   // first reader context id of the point in time
	prefShards        map[string]map[int]bool // shards a _shards preference selects, by index
	collapseSet       bool
	rescore           []rescoreSpec
	indexBoosts       []indexBoost
	boostByIndex      map[string]float64
	slice             *sliceSpec

	timeout            time.Duration
	stats              []string
	explain            bool
	profile            bool
	scriptFields       []scriptFieldSpec
	searchPipeline     string // search_pipeline given as a pipeline name
	inlinePipeline     bool   // search_pipeline given as an ad hoc pipeline
	pipelineProcessors bool   // the ad hoc pipeline has processors
	pipelineError      *Error // the ad hoc pipeline is invalid
	verbosePipeline    bool
	suggestSet         bool // suggest given
	suggestions        bool // suggest holds suggestions
	suggest            *suggestSpec
}

// errSearchPhase wraps a shard failure in search_phase_execution_exception
// ("all shards failed"). The index of the failure is reported on the failed
// shard; only exceptions raised while building the query keep it as their
// own metadata. Search fills in the index when the failure has none.
func errSearchPhase(inner *Error) *Error {
	index := inner.Index
	if inner.Type != "query_shard_exception" {
		inner.Index = ""
	}
	return &Error{Status: inner.Status, Type: "search_phase_execution_exception", Reason: "all shards failed",
		failure: &shardFailure{index: index, cause: inner}}
}

func parseSearchRequest(body M, p Params) (*searchRequest, error) {
	return parseSearchSource(body, nil, p)
}

// applySearchParams reads the URL parameters of a search once its body has
// been parsed (search_source.go).
func (sr *searchRequest) applySearchParams(p Params) (*searchRequest, error) {
	if p.Has("size") {
		n, err := strconv.Atoi(p.Get("size"))
		if err != nil || n < 0 {
			return nil, errIllegalArgument("[size] parameter cannot be negative, found [%s]", p.Get("size"))
		}
		sr.size = n
	}
	if p.Has("from") {
		n, err := strconv.Atoi(p.Get("from"))
		if err != nil || n < 0 {
			return nil, errIllegalArgument("[from] parameter cannot be negative, found [%s]", p.Get("from"))
		}
		sr.from = n
	}
	if n, has, err := parseIntParam(p, "terminate_after"); err != nil {
		return nil, err
	} else if has {
		if n < 0 {
			return nil, errIllegalArgument("terminateAfter must be > 0")
		}
		if n > 0 {
			sr.terminateAfter, sr.terminateAfterSet = n, true
		}
	}
	if p.Has("sort") {
		var specs []sortSpec
		for _, s := range splitList(p.Get("sort")) {
			field, order := s, "asc"
			if idx := strings.Index(s, ":"); idx > 0 {
				field, order = s[:idx], s[idx+1:]
			}
			specs = append(specs, sortSpec{field: field, desc: order == "desc"})
		}
		sr.setSort(specs)
	}
	if sourceParamsSet(p) {
		sr.source = sourceFilterFromParams(p)
		sr.sourceExplicit = true
	}
	if err := sr.source.validate(); err != nil {
		return nil, err
	}
	if v := p.Get("track_total_hits"); v != "" {
		switch v {
		case "true":
			sr.trackTotal = -1
			upTo := math.MaxInt32
			sr.trackTotalUpTo = &upTo
		case "false":
			sr.trackTotal = 0
			upTo := -1
			sr.trackTotalUpTo = &upTo
		default:
			upTo := p.Int("track_total_hits", sr.trackTotal)
			sr.trackTotalUpTo = &upTo
			if err := sr.setTrackTotalHits(upTo); err != nil {
				return nil, err
			}
		}
	}
	if p.Has("stored_fields") {
		sr.storedFieldsSet = true
		for _, field := range splitList(p.Get("stored_fields")) {
			if field == "_none_" {
				sr.storedNone = true
				continue
			}
			sr.storedFields = append(sr.storedFields, field)
		}
	}
	if p.Has("docvalue_fields") {
		// RestSearchAction adds the fields after those of the body
		for _, field := range splitList(p.Get("docvalue_fields")) {
			sr.docvalueFields = append(sr.docvalueFields, field)
		}
	}
	if p.Has("version") {
		sr.version = p.Bool("version", false)
	}
	if p.Has("explain") {
		sr.explain = p.Bool("explain", false)
	}
	if p.Has("seq_no_primary_term") {
		sr.seqNoTerm = p.Bool("seq_no_primary_term", false)
	}
	if p.Has("track_scores") {
		sr.trackScores = p.Bool("track_scores", false)
	}
	sr.totalAsInt = p.Bool("rest_total_hits_as_int", false)
	if s, has := p["scroll"]; has {
		d, err := parseTimeValue(s, "scroll")
		if err != nil {
			return nil, err
		}
		sr.scroll = d
		sr.scrollSet = true
	}
	sr.requestCache = p.Get("request_cache") == "true"
	if len(sr.sort) == 0 {
		sr.sort = []sortSpec{{field: "_score", desc: true}}
	}
	return sr, nil
}

// setTrackTotalHits applies a numeric track_total_hits: -1 disables the
// count; smaller values only fail when hits are fetched, which OpenSearch
// reports without shard failures.
func (sr *searchRequest) setTrackTotalHits(n int) error {
	switch {
	case n == -1:
		sr.trackTotal = 0
	case n < -1:
		return &Error{Status: http.StatusBadRequest, Type: "search_phase_execution_exception", Reason: "",
			Extra: M{"phase": "fetch", "grouped": true, "failed_shards": []any{}}, noRootCause: true,
			Cause: &Error{Type: "illegal_argument_exception", Reason: fmt.Sprintf("value must be >= 0, got %d", n)}}
	default:
		sr.trackTotal = n
	}
	return nil
}

// fieldSortKeys are the options of a field sort.
var fieldSortKeys = map[string]bool{"order": true, "missing": true, "mode": true, "unmapped_type": true, "nested": true,
	"numeric_type": true, "nested_path": true, "nested_filter": true}

// parseSortOrder is SortOrder.fromString.
func parseSortOrder(s string) (desc bool, err error) {
	switch strings.ToUpper(s) {
	case "ASC":
		return false, nil
	case "DESC":
		return true, nil
	}
	return false, errIllegalArgument("No enum constant org.opensearch.search.sort.SortOrder.%s", strings.ToUpper(s))
}

func jsonTokenName(v any) string {
	switch v.(type) {
	case M:
		return "START_OBJECT"
	case []any:
		return "START_ARRAY"
	case string:
		return "VALUE_STRING"
	case bool:
		return "VALUE_BOOLEAN"
	case nil:
		return "VALUE_NULL"
	}
	return "VALUE_NUMBER"
}

func parseSort(v any) ([]sortSpec, error) {
	var specs []sortSpec
	for _, item := range getList(v) {
		switch t := item.(type) {
		case string:
			specs = append(specs, sortSpec{field: t, desc: t == "_score", missing: "_last"})
		case M:
			for field, spec := range t {
				if field == "_geo_distance" {
					ss, err := parseGeoDistanceSort(spec)
					if err != nil {
						return nil, err
					}
					specs = append(specs, ss)
					continue
				}
				if field == "_script" {
					return nil, errUnsupported("sort by " + field)
				}
				ss := sortSpec{field: field, missing: "_last"}
				switch sv := spec.(type) {
				case string:
					desc, err := parseSortOrder(sv)
					if err != nil {
						return nil, err
					}
					ss.desc = desc
				case M:
					for key := range sv {
						if !fieldSortKeys[key] {
							return nil, (&Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: "[field_sort] unknown field [" + key + "]"}).
								at(keyTok(sv, key)).atParser(valueTok(sv, key))
						}
					}
					if order, ok := sv["order"]; ok {
						desc, err := parseSortOrder(fmt.Sprint(order))
						if err != nil {
							return nil, err
						}
						ss.desc = desc
					} else if field == "_score" {
						ss.desc = true
					}
					if m, ok := sv["missing"]; ok {
						ss.missing = m
					}
					ss.mode = getString(sv, "mode")
					ss.unmappedType = getString(sv, "unmapped_type")
					if nm, ok := sv["nested"].(M); ok {
						ss.nested = &nestedSort{path: getString(nm, "path"), filter: nm["filter"], matched: map[*Index]map[string]bool{}}
					} else if np := getString(sv, "nested_path"); np != "" {
						ss.nested = &nestedSort{path: np, filter: sv["nested_filter"], matched: map[*Index]map[string]bool{}}
					}
				default:
					return nil, errParsing("[sort] malformed sort for field [%s]", field)
				}
				specs = append(specs, ss)
			}
		default:
			return nil, errParsing("[sort] malformed sort")
		}
	}
	return specs, nil
}

// executeTargets runs a query over targets and returns all matching hits,
// target after target: in index order, or by descending score when ranked
// (the order terminate_after truncates).
func (c *Cluster) executeTargets(ts []target, q any, needLocations, ranked bool) ([]*hit, error) {
	return c.executeTargetsScoring(ts, q, needLocations, ranked, false)
}

// executeTargetsScoring is executeTargets for a query whose scores may not
// be needed (noScores).
func (c *Cluster) executeTargetsScoring(ts []target, q any, needLocations, ranked, noScores bool) ([]*hit, error) {
	var hits []*hit
	for _, t := range ts {
		qb := &queryBuilder{c: c, ix: t.ix, noScores: noScores}
		var bq query.Query
		if q == nil {
			bq = bleve.NewMatchAllQuery()
		} else {
			var err error
			bq, err = qb.build(q)
			if err != nil {
				if e, ok := err.(*Error); ok && (isShardParsingFailure(e) || (e.Type != "parsing_exception" && e.Type != "unsupported_operation_exception" && e.Type != "x_content_parse_exception" && e.Type != "search_phase_execution_exception" && !isQueryParseFailure(err))) {
					e.Index = t.ix.Name
					return nil, errSearchPhase(e)
				}
				return nil, err
			}
			if err := qb.checkInnerNames(); err != nil {
				return nil, err
			}
		}
		if t.filter != nil {
			fq, err := qb.build(t.filter)
			if err != nil {
				return nil, err
			}
			bq = bleve.NewConjunctionQuery(bq, &constantScoreQuery{inner: fq, score: 0})
		}
		n := t.ix.DocCount()
		if n == 0 {
			continue
		}
		// nested objects are documents of their own; only roots are hits
		rootQ := bleve.NewTermQuery("1")
		rootQ.SetField(fieldRoot)
		bq = bleve.NewConjunctionQuery(bq, &constantScoreQuery{inner: rootQ, score: 0})
		req := bleve.NewSearchRequestOptions(bq, n, 0, false)
		req.IncludeLocations = needLocations
		req.Score = "default"
		// take the matches as bleve collects them: its top-n store would sort
		// every one, and hits are ordered later anyway
		var matches []*search.DocumentMatch
		ctx := context.WithValue(context.Background(), search.MakeDocumentMatchHandlerKey,
			search.MakeDocumentMatchHandler(func(*search.SearchContext) (search.DocumentMatchHandler, bool, error) {
				return func(dm *search.DocumentMatch) error {
					if dm != nil {
						matches = append(matches, dm)
					}
					return nil
				}, true, nil
			}))
		if _, err := t.ix.bleve.SearchInContext(ctx, req); err != nil {
			return nil, &Error{Status: http.StatusBadRequest, Type: "search_phase_execution_exception", Reason: err.Error(), Index: t.ix.Name}
		}
		if ranked {
			slices.SortFunc(matches, search.CompareScoreDescending)
		}
		block := make([]hit, len(matches))
		for i, dm := range matches {
			d := t.ix.docs[dm.ID]
			if d == nil {
				continue
			}
			dm.Complete(nil)
			block[i] = hit{ix: t.ix, doc: d, score: dm.Score, locations: dm.Locations, inner: qb.inner, filter: t.filter}
			hits = append(hits, &block[i])
		}
	}
	return hits, nil
}

// matchDocs runs the query of a by-query request (delete_by_query etc.).
func (c *Cluster) matchDocs(ts []target, body M, p Params) ([]*hit, error) {
	sr, err := parseSearchRequest(body, p)
	if err != nil {
		return nil, err
	}
	hits, err := c.executeTargets(ts, sr.query, false, false)
	if err != nil {
		return nil, err
	}
	limit := len(hits)
	if v, ok := toFloat(body["max_docs"]); ok && int(v) < limit {
		limit = int(v)
	}
	if p.Has("max_docs") {
		if n := p.Int("max_docs", limit); n < limit {
			limit = n
		}
	}
	if _, ok := body["size"]; ok && sr.size < limit {
		limit = sr.size
	}
	return c.orderHits(hits, sr, max(limit, 0), nil)
}

func (c *Cluster) hitsJSON(page []*hit, sr *searchRequest, total int) (M, error) {
	out := M{}
	switch {
	case sr.trackTotal == 0:
		if sr.totalAsInt {
			// rest_total_hits_as_int renders untracked totals as -1
			out["total"] = -1
		}
	case sr.totalAsInt:
		out["total"] = total
	case sr.trackTotal < 0 || total <= sr.trackTotal:
		out["total"] = M{"value": total, "relation": "eq"}
	default:
		out["total"] = M{"value": sr.trackTotal, "relation": "gte"}
	}
	scoreVisible := sr.trackScores || !sr.explicitSort || sortsByScore(sr.sort)
	// the maximum score is tracked without a sort or when the primary sort
	// is the score (TopDocsCollectorContext)
	maxScoreVisible := sr.trackScores || !sr.explicitSort || (len(sr.sort) > 0 && sr.sort[0].field == "_score" && sr.sort[0].desc)
	var maxScore any
	list := make([]any, 0, len(page))
	named := c.matchedQueries(page, sr)
	explainers := map[*Index]*explainer{}
	for _, h := range page {
		hj := M{"_index": h.ix.Name}
		if sr.explain && h.doc.nested == nil {
			hj["_shard"] = "[" + h.ix.Name + "][" + strconv.Itoa(shardOf(h.ix, h.doc.ID)) + "]"
			hj["_node"] = osmemNodeID
			hj["_explanation"] = c.hitExplanation(h, sr, explainers)
		}
		if !sr.storedNone {
			// stored_fields _none_ loads no stored field, not even _id
			hj["_id"] = h.doc.ID
			if routing := docRouting(h.doc); routing != "" && h.doc.nested == nil {
				hj["_routing"] = routing
			}
			if len(h.doc.Ignored) > 0 && h.doc.nested == nil {
				hj["_ignored"] = append([]string(nil), h.doc.Ignored...)
			}
		}
		if h.doc.nested != nil {
			hj["_nested"] = nestedIdentityJSON(h.doc.nested)
		}
		if scoreVisible {
			// Lucene scores are floats
			score := Float(float32(h.score))
			hj["_score"] = score
			if maxScoreVisible && (maxScore == nil || score > maxScore.(Float)) {
				maxScore = score
			}
		} else {
			hj["_score"] = nil
		}
		if sr.version {
			hj["_version"] = h.doc.Version
		}
		if sr.seqNoTerm && h.doc.nested == nil {
			hj["_seq_no"] = h.doc.SeqNo
			hj["_primary_term"] = h.doc.PrimaryTerm
		}
		indexSource := mappingSourceFilter(h.ix.Mapping)
		if !sr.source.disabled && !indexSource.disabled && !sr.storedNone && (!sr.storedFieldsSet || sr.sourceExplicit) {
			switch {
			case h.doc.nested != nil:
				hj["_source"] = nestedSourceWithFilters(h.doc, indexSource, sr.source)
			case indexSource.isPlain() && sr.source.isPlain():
				hj["_source"] = json.RawMessage(h.doc.Raw)
			default:
				src, _ := applySourceFilters(h.doc.Src, indexSource, sr.source)
				hj["_source"] = src
			}
		}
		if len(sr.fields) > 0 || len(sr.docvalueFields) > 0 || len(sr.storedFields) > 0 || h.fields != nil {
			fm := M{}
			requested := map[string]bool{}
			for k, v := range h.fields {
				fm[k] = v
			}
			// "fields" reads the source and sees nested fields from the
			// root; docvalue_fields only see the fields of their level.
			// Stored fields are loaded first, docvalue_fields add their
			// values (FetchDocValuesPhase) and "fields" replaces them
			// (FetchFieldsPhase).
			for _, grp := range []struct {
				specs    []any
				anyLevel bool
				stored   bool
			}{{sr.storedFields, false, true}, {sr.docvalueFields, false, false}, {sr.fields, true, false}} {
				for _, spec := range grp.specs {
					name, format := "", ""
					switch t := spec.(type) {
					case string:
						name = t
					case M:
						name = getString(t, "field")
						format = getString(t, "format")
					}
					if name == "" {
						continue
					}
					if name == "_ignored" && !grp.anyLevel && !grp.stored {
						e := fielddataUnsupported(name, &Field{Type: TypeIgnoredMeta})
						e.Index = h.ix.Name
						return nil, errSearchPhase(e)
					}
					for _, path := range h.ix.Mapping.leafFields(name) {
						f, _, _ := h.ix.Mapping.resolve(path)
						if f == nil || (grp.stored && !getBool(f.Extra, "store", false)) {
							continue
						}
						docValues := !grp.anyLevel && !grp.stored
						if docValues {
							if err := checkDocValues(h, path, f); err != nil {
								return nil, err
							}
						}
						if grp.anyLevel && !grp.stored {
							// the fields option skips the fields listed in
							// _ignored and parses the source values again
							if stringsContain(h.doc.rootDoc().Ignored, path) {
								continue
							}
							if f.isDate() {
								if err := h.ix.fieldsDateSourceError(h.doc, path, f); err != nil {
									return nil, err
								}
							}
						}
						if (f.Type == TypeJoin || f.Type == TypeCompletion || f.Type == TypeConstantKeyword) && grp.anyLevel && !grp.stored {
							// these value fetchers return the source values as they are
							if raw := h.ix.sourceLeafValues(h.doc, path); len(raw) > 0 {
								if f.Type == TypeConstantKeyword {
									raw = []any{raw}
								}
								fm[path] = raw
							}
							continue
						}
						if isRangeType(f.Type) {
							if grp.anyLevel && !grp.stored {
								if out := rangeFieldsOutput(f, h.ix.sourceLeafValues(h.doc, path), format); len(out) > 0 {
									fm[path] = out
								}
							}
							continue
						}
						conv := convertValue
						if f.isIntegral() {
							// integral values keep the digits a double cannot hold
							conv = exactIntegralValue
						}
						vals := h.ix.fieldValuesWith(h.doc, path, grp.anyLevel, conv)
						if len(vals) == 0 {
							continue
						}
						if !docValues {
							if grp.stored {
								fm[path] = storedFieldOutput(f, vals)
								requested[path] = true
							} else {
								fm[path] = fieldsOutput(f, vals, format)
							}
							continue
						}
						if f.Type == TypeText {
							terms, err := h.ix.fielddataTerms(f, vals)
							if err != nil {
								return nil, err
							}
							vals = terms
						}
						dv, err := docValueOutput(f, vals, format)
						if err != nil {
							return nil, err
						}
						if prev, again := fm[path].([]any); again && requested[path] {
							// a field requested again adds its values
							dv = append(append([]any(nil), prev...), dv...)
						}
						fm[path] = dv
						requested[path] = true
					}
				}
			}
			if len(fm) > 0 {
				hj["fields"] = fm
			}
		}
		if sr.explicitSort {
			hj["sort"] = h.sortOut
		}
		if mq := named[h]; len(mq) > 0 {
			hj["matched_queries"] = mq
		}
		if sr.highlight != nil {
			hl, err := c.highlightHit(h, sr)
			if err != nil {
				return nil, err
			}
			if len(hl) > 0 {
				hj["highlight"] = hl
			}
		}
		if len(h.inner) > 0 || (h.group != nil && len(sr.collapseInner) > 0) {
			ih, err := c.innerHitsJSON(h, sr)
			if err != nil {
				return nil, err
			}
			if len(ih) > 0 {
				hj["inner_hits"] = ih
			}
		}
		list = append(list, hj)
	}
	out["max_score"] = maxScore
	out["hits"] = list
	return out, nil
}

func sortsByScore(specs []sortSpec) bool {
	for _, s := range specs {
		if s.field == "_score" {
			return true
		}
	}
	return false
}

// sorting --------------------------------------------------------------

// missingSortValue returns the comparable key and reported value for a
// document without a value: numeric and date fields get the sentinels
// OpenSearch reports (Long.MAX_VALUE/MIN_VALUE, "Infinity"), others null.
func missingSortValue(f *Field, s sortSpec) (any, any) {
	if f == nil {
		return nil, nil
	}
	if !(f.isNumeric() || f.isDate() || f.Type == TypeBoolean) {
		return nil, nil
	}
	last := s.missing != "_first"
	positive := (last && !s.desc) || (!last && s.desc)
	switch {
	case f.Type == TypeDouble || f.Type == TypeFloat || f.Type == TypeHalfFloat || f.Type == TypeScaledFloat:
		if positive {
			return math.Inf(1), "Infinity"
		}
		return math.Inf(-1), "-Infinity"
	case intSortField(f):
		if positive {
			return math.Inf(1), int64(math.MaxInt32)
		}
		return math.Inf(-1), int64(math.MinInt32)
	default:
		if positive {
			return math.Inf(1), int64(math.MaxInt64)
		}
		return math.Inf(-1), int64(math.MinInt64)
	}
}

// intSortField reports fields sorted as Java ints, whose missing values
// sort as Integer.MAX_VALUE/MIN_VALUE.
func intSortField(f *Field) bool {
	switch f.Type {
	case TypeInteger, TypeShort, TypeByte, TypeBoolean, TypeTokenCount:
		return true
	}
	return false
}

// checkDocValues rejects reading doc values of a field that has none (text
// without fielddata, doc_values: false), as docvalue_fields does.
func checkDocValues(h *hit, field string, f *Field) error {
	if e := fielddataUnsupported(field, f); e != nil {
		e.Index = h.ix.Name
		return errSearchPhase(e)
	}
	if f.Type == TypeText {
		if !getBool(f.Extra, "fielddata", false) {
			return errAggField(h, field)
		}
		return nil
	}
	if !getBool(f.Extra, "doc_values", true) {
		return errDocValuesDisabled(h, field, f)
	}
	return nil
}

// fielddataTerms analyzes text values into the terms fielddata exposes.
func (ix *Index) fielddataTerms(f *Field, vals []any) ([]any, error) {
	analyzer, err := ix.analysis.analyzerNamed(f.Analyzer)
	if err != nil {
		return nil, err
	}
	var terms []any
	for _, v := range vals {
		text, err := stringValue("", f, v)
		if err != nil {
			continue
		}
		for _, term := range tokens(analyzer, text) {
			terms = append(terms, term)
		}
	}
	return terms, nil
}

// sortFieldValues returns the values a sort reads from a hit. A field
// inside a nested object is invisible from the root document unless the
// sort has a nested option, which then reads every object of that path
// (restricted by the nested filter when there is one).
func (c *Cluster) sortFieldValues(h *hit, s sortSpec, base string) ([]any, error) {
	anc := h.ix.Mapping.nestedAncestor(base)
	if anc == h.doc.level() || s.nested == nil {
		return h.ix.fieldValues(h.doc, s.field), nil
	}
	path := s.nested.path
	if path == "" {
		path = anc
	}
	var matched map[string]bool
	if s.nested.filter != nil {
		var ok bool
		if matched, ok = s.nested.matched[h.ix]; !ok {
			var err error
			if matched, err = c.nestedFilterMatches(h.ix, path, s.nested.filter); err != nil {
				return nil, err
			}
			s.nested.matched[h.ix] = matched
		}
	}
	depth := len(h.ix.Mapping.nestedChain(path))
	var vals []any
	for _, d := range h.ix.nestedDescendants(h.doc, anc) {
		if matched != nil && depth <= len(d.nested) && !matched[chainID(d.ID, d.nested[:depth])] {
			continue
		}
		vals = append(vals, h.ix.fieldValues(d, s.field)...)
	}
	return vals, nil
}

// sortOutput renders a comparable sort key the way OpenSearch reports it:
// longs for dates, booleans and integral fields, Java floats for float and
// half_float, doubles for the other numeric fields.
func sortOutput(f *Field, v any, s sortSpec) any {
	n, ok := v.(float64)
	if f == nil || !ok {
		return v
	}
	switch {
	case f.isDate():
		return int64(n)
	case f.isIntegral() || f.Type == TypeBoolean:
		return int64(n)
	case f.Type == TypeFloat || f.Type == TypeHalfFloat:
		return Float(float32(n))
	case f.isNumeric():
		return Double(n)
	}
	return v
}

// normalizeSearchAfter converts the search_after values into comparable
// keys matching the sort specs (dates parsed with the field format,
// sentinels mapped to infinities).
func (c *Cluster) normalizeSearchAfter(after []any, specs []sortSpec, ts []target) ([]any, error) {
	out := make([]any, len(after))
	for i, s := range specs {
		v := after[i]
		var f *Field
		for _, t := range ts {
			if ff, _, ok := t.ix.Mapping.resolve(s.field); ok {
				f = ff
				break
			}
		}
		if s.unmappedType != "" && f == nil {
			f = &Field{Type: s.unmappedType}
		}
		switch {
		case v == nil:
			out[i] = nil
		case s.field == "_score" || s.field == "_doc" || s.field == "_shard_doc":
			n, ok := toFloat(v)
			if !ok {
				return nil, errSearchPhase(errIllegalArgument("Failed to parse search_after value for field [%s]: %v", s.field, v))
			}
			out[i] = n
		case f != nil && f.isDate():
			var ms float64
			switch t := v.(type) {
			case string:
				if t == "Infinity" || t == "-Infinity" {
					ms = math.Inf(1)
					if t[0] == '-' {
						ms = math.Inf(-1)
					}
					break
				}
				df := f.Format
				if isDigits(t) {
					n, _ := strconv.ParseFloat(t, 64)
					ms = n
					break
				}
				parsed, err := ParseDateMath(t, df, c.now(), time.UTC, false)
				if err != nil {
					return nil, errSearchPhase(errIllegalArgument("Failed to parse search_after value for field [%s]: %v", s.field, err))
				}
				ms = float64(parsed.UnixMilli())
				if f.Type == TypeDateNanos {
					ms = float64(parsed.UnixNano())
				}
			default:
				n, ok := toFloat(v)
				if !ok {
					return nil, errSearchPhase(errIllegalArgument("Failed to parse search_after value for field [%s]: %v", s.field, v))
				}
				ms = n
			}
			out[i] = sentinelToInf(ms)
		case f != nil && (f.isNumeric() || f.Type == TypeBoolean):
			if str, ok := v.(string); ok && (str == "Infinity" || str == "-Infinity") {
				if str[0] == '-' {
					out[i] = math.Inf(-1)
				} else {
					out[i] = math.Inf(1)
				}
				break
			}
			n, ok := toFloat(v)
			if !ok {
				return nil, errSearchPhase(errIllegalArgument("Failed to parse search_after value for field [%s]: %v", s.field, v))
			}
			if intSortField(f) && (n >= math.MaxInt32 || n <= math.MinInt32) {
				out[i] = math.Inf(int(math.Copysign(1, n)))
				break
			}
			if inf := sentinelToInf(n); math.IsInf(inf, 0) {
				out[i] = inf
				break
			}
			out[i] = docValue(f, n)
		default:
			if n, ok := v.(json.Number); ok {
				out[i] = n.String()
			} else {
				out[i] = fmt.Sprint(v)
			}
		}
	}
	return out, nil
}

func sentinelToInf(n float64) float64 {
	if n >= float64(math.MaxInt64) {
		return math.Inf(1)
	}
	if n <= float64(math.MinInt64) {
		return math.Inf(-1)
	}
	return n
}

// compareSortValues orders two sort keys according to the spec (missing
// values last unless missing == "_first").
func compareSortValues(a, b any, s sortSpec) int {
	if a == nil || b == nil {
		if a == nil && b == nil {
			return 0
		}
		first := s.missing == "_first"
		if a == nil {
			if first {
				return -1
			}
			return 1
		}
		if first {
			return 1
		}
		return -1
	}
	cmp := compareValues(a, b)
	if s.desc {
		return -cmp
	}
	return cmp
}

func compareValues(a, b any) int {
	switch av := a.(type) {
	case float64:
		if bv, ok := toFloat(b); ok {
			switch {
			case av < bv:
				return -1
			case av > bv:
				return 1
			}
			return 0
		}
	case string:
		if bv, ok := b.(string); ok {
			return strings.Compare(av, bv)
		}
		if bf, ok := toFloat(b); ok {
			if af, ok := toFloat(av); ok {
				return compareValues(af, bf)
			}
		}
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

// compareTuples compares a hit's sort keys with normalized search_after
// keys.
func compareTuples(keys []sortKey, after []any, specs []sortSpec) int {
	for i := range specs {
		if after[i] == nil && keys[i].kind == keyNone {
			continue
		}
		if c := compareKeyValue(keys[i], after[i], specs[i]); c != 0 {
			return c
		}
	}
	return 0
}

// scroll and point in time ----------------------------------------------
