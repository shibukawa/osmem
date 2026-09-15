package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
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
}

type sortSpec struct {
	field        string
	desc         bool
	missing      any // "_last", "_first" or a value
	mode         string
	unmappedType string
	format       *DateFormat
	nested       *nestedSort
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
	scroll          time.Duration
	pitID           string
	pitKeepAlive    time.Duration
	pitKeepAliveSet bool
	totalAsInt      bool
}

func errSearchPhase(inner *Error) *Error {
	reason := M{"type": inner.Type, "reason": inner.Reason}
	shard := M{"shard": 0, "node": "osmem", "reason": reason}
	rootIndex := ""
	if inner.Index != "" {
		shard["index"] = inner.Index
		// only exceptions raised while building the query carry the index
		if inner.Type == "query_shard_exception" {
			reason["index"] = inner.Index
			reason["index_uuid"] = "_na_"
			rootIndex = inner.Index
		}
	}
	return &Error{Status: inner.Status, Type: "search_phase_execution_exception", Reason: "all shards failed",
		Extra:    M{"phase": "query", "grouped": true, "failed_shards": []any{shard}},
		RootType: inner.Type, RootReason: inner.Reason, RootIndex: rootIndex}
}

func parseSearchRequest(body M, p Params) (*searchRequest, error) {
	sr := &searchRequest{size: 10, trackTotal: maxResultWindow}
	if body == nil {
		body = M{}
	}
	if q := p.Get("q"); q != "" {
		qs := M{"query": q}
		if df := p.Get("df"); df != "" {
			qs["default_field"] = df
		}
		if op := p.Get("default_operator"); op != "" {
			qs["default_operator"] = op
		}
		if an := p.Get("analyzer"); an != "" {
			qs["analyzer"] = an
		}
		sr.query = M{"query_string": qs}
	}
	for k, v := range body {
		switch k {
		case "query":
			sr.query = v
		case "post_filter":
			sr.postFilter = v
		case "size":
			n, ok := toFloat(v)
			if !ok || n < 0 {
				return nil, errIllegalArgument("[size] parameter cannot be negative, found [%v]", v)
			}
			sr.size = int(n)
		case "from":
			n, ok := toFloat(v)
			if !ok || n < 0 {
				return nil, errIllegalArgument("[from] parameter cannot be negative but was [%v]", v)
			}
			sr.from = int(n)
		case "sort":
			specs, err := parseSort(v)
			if err != nil {
				return nil, err
			}
			sr.sort = specs
			sr.explicitSort = len(specs) > 0
		case "_source":
			sr.source = parseSourceParam(v)
		case "aggs", "aggregations":
			am, ok := v.(M)
			if !ok {
				return nil, errParsing("[aggregations] must be an object")
			}
			if sr.aggs == nil {
				sr.aggs = M{}
			}
			for ak, av := range am {
				sr.aggs[ak] = av
			}
		case "track_total_hits":
			switch t := v.(type) {
			case bool:
				if t {
					sr.trackTotal = -1
				} else {
					sr.trackTotal = 0
				}
			default:
				if n, ok := toFloat(t); ok {
					sr.trackTotal = int(n)
				}
			}
		case "search_after":
			list, ok := v.([]any)
			if !ok {
				return nil, errParsing("[search_after] must be an array")
			}
			sr.searchAfter = list
		case "highlight":
			hm, _ := v.(M)
			sr.highlight = hm
		case "min_score":
			if n, ok := toFloat(v); ok {
				sr.minScore = &n
			}
		case "fields":
			sr.fields = getList(v)
		case "docvalue_fields":
			sr.docvalueFields = getList(v)
		case "version":
			sr.version = getBool(body, k, false)
		case "seq_no_primary_term":
			sr.seqNoTerm = getBool(body, k, false)
		case "track_scores":
			sr.trackScores = getBool(body, k, false)
		case "collapse":
			cm, ok := v.(M)
			if !ok {
				return nil, errParsing("[collapse] must be an object")
			}
			sr.collapse = getString(cm, "field")
			if sr.collapse == "" {
				return nil, errIllegalArgument("collapse field cannot be null")
			}
			var specs []M
			switch ih := cm["inner_hits"].(type) {
			case M:
				specs = []M{ih}
			case []any:
				for _, e := range ih {
					if em, ok := e.(M); ok {
						specs = append(specs, em)
					}
				}
			}
			for _, ihm := range specs {
				spec, err := parseInnerHits(ihm, "")
				if err != nil {
					return nil, err
				}
				if spec.name == "" {
					return nil, errIllegalArgument("Field name cannot be null")
				}
				replaced := false
				for i, prev := range sr.collapseInner {
					if prev.name == spec.name {
						sr.collapseInner[i] = spec
						replaced = true
					}
				}
				if !replaced {
					sr.collapseInner = append(sr.collapseInner, spec)
				}
			}
		case "stored_fields":
			for _, field := range getStrings(body, k) {
				if field == "_none_" {
					sr.storedNone = true
					continue
				}
				sr.storedFields = append(sr.storedFields, field)
			}
		case "pit":
			pm, _ := v.(M)
			sr.pitID = getString(pm, "id")
			if rawKeepAlive, ok := pm["keep_alive"]; ok {
				keepAlive, isString := rawKeepAlive.(string)
				d, valid := parseDuration(keepAlive)
				if !isString || !valid || d <= 0 {
					return nil, errIllegalArgument("failed to parse setting [pit.keep_alive] with value [%v]", rawKeepAlive)
				}
				sr.pitKeepAlive = d
				sr.pitKeepAliveSet = true
			}
		case "suggest":
			return nil, errUnsupported("suggest")
		case "knn", "ext", "rank":
			return nil, errUnsupported("[" + k + "]")
		case "terminate_after":
			n, ok := toFloat(v)
			if !ok || n < 1 || n != math.Trunc(n) {
				return nil, errIllegalArgument("[terminate_after] must be a positive integer")
			}
			sr.terminateAfter = int(n)
		case "explain", "timeout", "profile", "rescore", "indices_boost", "script_fields", "runtime_mappings", "stats", "slice", "search_pipeline", "verbose_pipeline":
			// accepted and ignored
		default:
			return nil, errParsing("Unknown key for a %s in [%s].", jsonTokenName(v), k)
		}
	}
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
			return nil, errIllegalArgument("[from] parameter cannot be negative but was [%s]", p.Get("from"))
		}
		sr.from = n
	}
	if p.Has("terminate_after") {
		n, err := strconv.Atoi(p.Get("terminate_after"))
		if err != nil || n < 1 {
			return nil, errIllegalArgument("[terminate_after] must be a positive integer")
		}
		sr.terminateAfter = n
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
		sr.sort = specs
		sr.explicitSort = len(specs) > 0
	}
	if p.Has("_source") || p.Has("_source_includes") || p.Has("_source_excludes") {
		sr.source = sourceFilterFromParams(p)
	}
	if v := p.Get("track_total_hits"); v != "" {
		switch v {
		case "true":
			sr.trackTotal = -1
		case "false":
			sr.trackTotal = 0
		default:
			sr.trackTotal = p.Int("track_total_hits", sr.trackTotal)
		}
	}
	if p.Has("version") {
		sr.version = p.Bool("version", false)
	}
	if p.Has("seq_no_primary_term") {
		sr.seqNoTerm = p.Bool("seq_no_primary_term", false)
	}
	if p.Has("track_scores") {
		sr.trackScores = p.Bool("track_scores", false)
	}
	sr.totalAsInt = p.Bool("rest_total_hits_as_int", false)
	if s := p.Get("scroll"); s != "" {
		d, ok := parseDuration(s)
		if !ok {
			return nil, errIllegalArgument("failed to parse setting [scroll] with value [%s]", s)
		}
		sr.scroll = d
	}
	if len(sr.sort) == 0 {
		sr.sort = []sortSpec{{field: "_score", desc: true}}
	}
	return sr, nil
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
				ss := sortSpec{field: field, missing: "_last"}
				switch sv := spec.(type) {
				case string:
					ss.desc = sv == "desc"
				case M:
					ss.desc = getString(sv, "order") == "desc"
					if m, ok := sv["missing"]; ok {
						ss.missing = m
					}
					ss.mode = getString(sv, "mode")
					ss.unmappedType = getString(sv, "unmapped_type")
					if f := getString(sv, "format"); f != "" {
						ss.format = ParseDateFormat(f)
					}
					if getString(sv, "order") == "" && field == "_score" {
						ss.desc = true
					}
					if nm, ok := sv["nested"].(M); ok {
						ss.nested = &nestedSort{path: getString(nm, "path"), filter: nm["filter"], matched: map[*Index]map[string]bool{}}
					} else if np := getString(sv, "nested_path"); np != "" {
						ss.nested = &nestedSort{path: np, filter: sv["nested_filter"], matched: map[*Index]map[string]bool{}}
					}
				default:
					return nil, errParsing("[sort] malformed sort for field [%s]", field)
				}
				if field == "_geo_distance" || field == "_script" {
					return nil, errUnsupported("sort by " + field)
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
	var hits []*hit
	for _, t := range ts {
		qb := &queryBuilder{c: c, ix: t.ix}
		var bq query.Query
		if q == nil {
			bq = bleve.NewMatchAllQuery()
		} else {
			var err error
			bq, err = qb.build(q)
			if err != nil {
				if e, ok := err.(*Error); ok && e.Type != "parsing_exception" && e.Type != "unsupported_operation_exception" && e.Type != "x_content_parse_exception" && e.Type != "search_phase_execution_exception" {
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
			block[i] = hit{ix: t.ix, doc: d, score: dm.Score, locations: dm.Locations, inner: qb.inner}
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

// Search implements _search.
func (c *Cluster) Search(expr string, body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	sr, err := parseSearchRequest(body, p)
	if err != nil {
		return fail(err)
	}
	var ts []target
	if sr.pitID != "" {
		c.scrollMu.Lock()
		pit, ok := c.pits[sr.pitID]
		now := c.now()
		if ok && !now.Before(pit.expires) {
			delete(c.pits, sr.pitID)
			releasePIT(pit)
			ok = false
		}
		if ok {
			if sr.pitKeepAliveSet {
				pit.expires = now.Add(sr.pitKeepAlive)
			}
			ts = append([]target(nil), pit.targets...)
			// A concurrent PIT deletion must not close the snapshot while this
			// search is using it.
			for _, t := range ts {
				t.ix.refs.Add(1)
			}
		}
		c.scrollMu.Unlock()
		if !ok {
			return fail(&Error{Status: 404, Type: "search_context_missing_exception", Reason: "No search context found for id [" + sr.pitID + "]"})
		}
		defer func() {
			for _, t := range ts {
				t.ix.release()
			}
		}()
	} else {
		ts, err = c.resolve(expr, resolveOpts(p))
		if err != nil {
			return fail(err)
		}
	}
	if sr.pitID == "" && len(ts) == 0 && !strings.ContainsAny(expr, "*?") && expr != "" && expr != "_all" && !p.Bool("ignore_unavailable", false) {
		return fail(errIndexNotFound(expr))
	}
	res, err := c.runSearch(ts, sr, p)
	if err != nil {
		return fail(err)
	}
	if sr.pitID != "" {
		res["pit_id"] = sr.pitID
	}
	return ok(res)
}

// ValidateQuery checks whether a query can be built for each resolved index
// without executing it against documents.
func (c *Cluster) ValidateQuery(expr string, body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	if len(ts) == 0 && !strings.ContainsAny(expr, "*?") && expr != "" && expr != "_all" && !p.Bool("ignore_unavailable", false) {
		return fail(errIndexNotFound(expr))
	}
	var q any
	if body != nil {
		q = body["query"]
	}
	if p.Has("q") {
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
	valid := true
	explain := p.Bool("explain", false)
	explanations := make([]any, 0)
	for _, t := range ts {
		qb := &queryBuilder{c: c, ix: t.ix}
		if q != nil {
			if _, buildErr := qb.build(q); buildErr != nil {
				valid = false
				if explain {
					explanations = append(explanations, M{"index": t.ix.Name, "valid": false, "error": buildErr.Error()})
				}
				continue
			}
			if namesErr := qb.checkInnerNames(); namesErr != nil {
				valid = false
				if explain {
					explanations = append(explanations, M{"index": t.ix.Name, "valid": false, "error": namesErr.Error()})
				}
			}
		}
	}
	totalShards := len(ts)
	if p.Bool("all_shards", false) {
		shards := searchShards(ts)
		totalShards = getInt(shards, "total", len(ts))
	}
	result := M{"_shards": M{"total": totalShards, "successful": totalShards, "skipped": 0, "failed": 0}, "valid": valid}
	if explain {
		result["explanations"] = explanations
	}
	return ok(result)
}

func (c *Cluster) runSearch(ts []target, sr *searchRequest, p Params) (M, error) {
	start := time.Now()
	if sr.from+sr.size > maxResultWindow && sr.scroll == 0 {
		return nil, errSearchPhase(errIllegalArgument("Result window is too large, from + size must be less than or equal to: [%d] but was [%d]. See the scroll api for a more efficient way to request large data sets. This limit can be set by changing the [index.max_result_window] index level setting.", maxResultWindow, sr.from+sr.size))
	}
	if len(sr.searchAfter) > 0 && sr.from > 0 {
		return nil, errSearchPhase(errIllegalArgument("[from] parameter must be set to 0 when [search_after] is used."))
	}
	if sr.collapse != "" {
		idx := ""
		if len(ts) > 0 {
			idx = ts[0].ix.Name
		}
		if sr.scroll > 0 {
			return nil, errSearchPhase(&Error{Status: http.StatusInternalServerError, Type: "search_exception", Reason: "cannot use `collapse` in a scroll context", Index: idx})
		}
		if len(sr.searchAfter) > 0 {
			return nil, errSearchPhase(&Error{Status: http.StatusInternalServerError, Type: "search_exception", Reason: "cannot use `collapse` in conjunction with `search_after`", Index: idx})
		}
	}
	needLoc := sr.highlight != nil
	hits, err := c.executeTargets(ts, sr.query, needLoc, sr.terminateAfter > 0)
	if err != nil {
		return nil, err
	}
	terminatedEarly := sr.terminateAfter > 0 && len(hits) > sr.terminateAfter
	if terminatedEarly {
		hits = hits[:sr.terminateAfter]
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
	aggHits := hits
	if sr.postFilter != nil {
		pf, err := c.executeTargets(ts, sr.postFilter, false, false)
		if err != nil {
			return nil, err
		}
		keep := map[*Doc]bool{}
		for _, h := range pf {
			keep[h.doc] = true
		}
		filtered := make([]*hit, 0, len(hits))
		for _, h := range hits {
			if keep[h.doc] {
				filtered = append(filtered, h)
			}
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
		hits = filtered
	}
	// total counts the documents before collapsing, as on OpenSearch
	total := len(hits)
	var keep func(*hit) bool
	var afterErr error
	if len(sr.searchAfter) > 0 {
		if len(sr.searchAfter) != len(sr.sort) {
			afterErr = errSearchPhase(errIllegalArgument("search_after has %d value(s) but sort has %d.", len(sr.searchAfter), len(sr.sort)))
		} else if after, err := c.normalizeSearchAfter(sr.searchAfter, sr.sort, ts); err != nil {
			afterErr = err
		} else {
			keep = func(h *hit) bool { return compareTuples(h.keys, after, sr.sort) > 0 }
		}
	}
	// a scroll and collapsing need every hit in order, a page only its first ones
	limit := -1
	if sr.scroll == 0 && sr.collapse == "" {
		limit = sr.from + sr.size
	}
	hits, err = c.orderHits(hits, sr, limit, keep)
	if err != nil {
		return nil, err
	}
	if afterErr != nil {
		return nil, afterErr
	}
	if sr.collapse != "" {
		hits, err = c.collapseHits(hits, sr.collapse)
		if err != nil {
			return nil, err
		}
	}
	var aggResult M
	if len(sr.aggs) > 0 {
		var all []*hit
		aggResult, err = c.runAggregations(sr.aggs, aggHits, func() []*hit {
			if all == nil {
				all, _ = c.executeTargets(ts, nil, false, false)
			}
			return all
		})
		if err != nil {
			return nil, err
		}
	}
	res := M{"took": int(time.Since(start).Milliseconds()), "timed_out": false, "_shards": searchShards(ts)}
	if terminatedEarly {
		res["terminated_early"] = true
	}
	if sr.scroll > 0 {
		page := hits
		if len(page) > sr.size {
			page = page[:sr.size]
		}
		res["_scroll_id"] = c.newScroll(hits[len(page):], len(hits), sr, ts)
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
	return res, nil
}

func (c *Cluster) hitsJSON(page []*hit, sr *searchRequest, total int) (M, error) {
	out := M{}
	switch {
	case sr.trackTotal == 0:
	case sr.totalAsInt:
		out["total"] = total
	case sr.trackTotal < 0 || total <= sr.trackTotal:
		out["total"] = M{"value": total, "relation": "eq"}
	default:
		out["total"] = M{"value": sr.trackTotal, "relation": "gte"}
	}
	scoreVisible := sr.trackScores || !sr.explicitSort || sortsByScore(sr.sort)
	var maxScore any
	list := make([]any, 0, len(page))
	for _, h := range page {
		hj := M{"_index": h.ix.Name, "_id": h.doc.ID}
		if h.doc.nested != nil {
			hj["_nested"] = nestedIdentityJSON(h.doc.nested)
		}
		if scoreVisible {
			hj["_score"] = h.score
			if maxScore == nil || h.score > maxScore.(float64) {
				maxScore = h.score
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
		if !sr.source.disabled && !indexSource.disabled && !sr.storedNone {
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
			for k, v := range h.fields {
				fm[k] = v
			}
			// "fields" reads the source and sees nested fields from the
			// root; docvalue_fields only see the fields of their level
			for _, grp := range []struct {
				specs    []any
				anyLevel bool
				stored   bool
			}{{sr.fields, true, false}, {sr.docvalueFields, false, false}, {sr.storedFields, false, true}} {
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
					for _, path := range h.ix.Mapping.leafFields(name) {
						f, _, _ := h.ix.Mapping.resolve(path)
						if grp.stored && (f == nil || !getBool(f.Extra, "store", false)) {
							continue
						}
						vals := h.ix.fieldValuesAt(h.doc, path, grp.anyLevel)
						if len(vals) == 0 {
							continue
						}
						outVals := make([]any, 0, len(vals))
						for _, v := range vals {
							outVals = append(outVals, formatFieldValue(f, v, format))
						}
						fm[path] = outVals
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
		if sr.highlight != nil {
			if hl := c.highlightHit(h, sr.highlight); len(hl) > 0 {
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

func formatFieldValue(f *Field, v any, format string) any {
	switch t := v.(type) {
	case time.Time:
		df := f.Format
		if format != "" {
			df = ParseDateFormat(format)
		}
		if df == nil {
			df = ParseDateFormat(DefaultDateFormat)
		}
		return df.Format(t)
	case float64:
		return numberValue(t)
	case [2]float64:
		return M{"lat": t[0], "lon": t[1]}
	}
	return v
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
	default:
		if positive {
			return math.Inf(1), int64(math.MaxInt64)
		}
		return math.Inf(-1), int64(math.MinInt64)
	}
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

// sortOutput renders a comparable sort key the way OpenSearch reports it.
func sortOutput(f *Field, v any, s sortSpec) any {
	if f != nil && f.isDate() {
		if n, ok := v.(float64); ok {
			if s.format != nil {
				return s.format.Format(time.UnixMilli(int64(n)).UTC())
			}
			return int64(n)
		}
	}
	if f != nil && (f.isIntegral() || f.Type == TypeBoolean) {
		if n, ok := v.(float64); ok && n == math.Trunc(n) {
			return int64(n)
		}
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
		case s.field == "_score" || s.field == "_doc":
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
				if s.format != nil {
					df = s.format
				}
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
			out[i] = sentinelToInf(n)
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

// collapseHits keeps the first hit for each value of a field and records
// the members of each group on it for inner_hits. Documents without a
// value form one group and report no fields, as on OpenSearch.
func (c *Cluster) collapseHits(hits []*hit, field string) ([]*hit, error) {
	groups := map[string]*hit{}
	out := make([]*hit, 0, len(hits))
	for _, h := range hits {
		f, _, ok := h.ix.Mapping.resolve(field)
		if !ok {
			return nil, errSearchPhase(&Error{Status: 400, Type: "illegal_argument_exception", Reason: "no mapping found for `" + field + "` in order to collapse on", Index: h.ix.Name})
		}
		if f.Type == TypeText {
			return nil, errSearchPhase(&Error{Status: 400, Type: "illegal_argument_exception", Reason: "collapse is not supported for the field [" + field + "] of the type [text]", Index: h.ix.Name})
		}
		// a field inside a nested object has no value on the root, so every
		// document lands in the group without a value, as on OpenSearch
		vals := h.ix.fieldValues(h.doc, field)
		if len(vals) > 1 {
			return nil, errSearchPhase(&Error{Status: 400, Type: "illegal_argument_exception", Reason: "failed to collapse " + h.doc.ID + ", the collapse field must be single valued", Index: h.ix.Name})
		}
		key := "\x00missing"
		if len(vals) == 1 {
			key = fmt.Sprint(vals[0])
			h.fields = M{field: []any{formatFieldValue(f, vals[0], "")}}
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

// highlighting -----------------------------------------------------------

type valPos struct {
	value string
	pos   []uint64
}

func leafPositions(v any, parts []string, pos []uint64, out *[]valPos) {
	if len(parts) == 0 {
		vals := flattenValues(v)
		for i, e := range vals {
			p := pos
			if len(vals) > 1 || len(pos) > 0 {
				p = append(append([]uint64(nil), pos...), uint64(i))
			}
			s, ok := e.(string)
			if !ok {
				s = fmt.Sprint(e)
			}
			*out = append(*out, valPos{value: s, pos: p})
		}
		return
	}
	switch t := v.(type) {
	case M:
		if next, ok := t[parts[0]]; ok {
			leafPositions(next, parts[1:], pos, out)
		}
	case []any:
		for i, e := range t {
			leafPositions(e, parts, append(append([]uint64(nil), pos...), uint64(i)), out)
		}
	}
}

func samePos(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (c *Cluster) highlightHit(h *hit, spec M) M {
	fields := getMap(spec, "fields")
	if fields == nil {
		return nil
	}
	preTags := getStrings(spec, "pre_tags")
	postTags := getStrings(spec, "post_tags")
	if len(preTags) == 0 {
		preTags = []string{"<em>"}
	}
	if len(postTags) == 0 {
		postTags = []string{"</em>"}
	}
	out := M{}
	for pattern, rawOpts := range fields {
		opts, _ := rawOpts.(M)
		nFrag := getInt(spec, "number_of_fragments", 5)
		fragSize := getInt(spec, "fragment_size", 100)
		if opts != nil {
			nFrag = getInt(opts, "number_of_fragments", nFrag)
			fragSize = getInt(opts, "fragment_size", fragSize)
			if pt := getStrings(opts, "pre_tags"); len(pt) > 0 {
				preTags = pt
			}
			if pt := getStrings(opts, "post_tags"); len(pt) > 0 {
				postTags = pt
			}
		}
		var names []string
		if strings.ContainsAny(pattern, "*?") {
			for name := range h.locations {
				if wildcardMatch(pattern, name) {
					names = append(names, name)
				}
			}
			sort.Strings(names)
		} else {
			names = []string{pattern}
		}
		for _, name := range names {
			locs, ok := h.locations[name]
			if !ok {
				continue
			}
			_, base, ok := h.ix.Mapping.resolve(name)
			if !ok {
				continue
			}
			var vals []valPos
			leafPositions(h.doc.Src, strings.Split(base, "."), nil, &vals)
			var fragments []any
			for _, vp := range vals {
				var spans []span
				for _, lset := range locs {
					for _, l := range lset {
						if !samePos(l.ArrayPositions, vp.pos) {
							continue
						}
						if int(l.End) <= len(vp.value) {
							spans = append(spans, span{int(l.Start), int(l.End)})
						}
					}
				}
				if len(spans) == 0 {
					continue
				}
				sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
				// merge overlapping spans
				merged := spans[:1]
				for _, sp := range spans[1:] {
					last := &merged[len(merged)-1]
					if sp.start <= last.end {
						if sp.end > last.end {
							last.end = sp.end
						}
						continue
					}
					merged = append(merged, sp)
				}
				var sb strings.Builder
				prev := 0
				for _, sp := range merged {
					sb.WriteString(vp.value[prev:sp.start])
					sb.WriteString(preTags[0])
					sb.WriteString(vp.value[sp.start:sp.end])
					sb.WriteString(postTags[0])
					prev = sp.end
				}
				sb.WriteString(vp.value[prev:])
				frag := sb.String()
				if nFrag > 0 && fragSize > 0 && len([]rune(vp.value)) > fragSize {
					frag = windowFragment(vp.value, merged[0].start, fragSize, preTags[0], postTags[0], merged)
				}
				fragments = append(fragments, frag)
				if nFrag > 0 && len(fragments) >= nFrag {
					break
				}
			}
			if len(fragments) > 0 {
				out[name] = fragments
			}
		}
	}
	return out
}

// windowFragment cuts a fragment of about fragSize runes around the first
// match and applies tags to the spans inside it.
type span struct{ start, end int }

func windowFragment(value string, firstStart, fragSize int, pre, post string, spans []span) string {
	runes := []rune(value)
	// byte offset -> rune index
	byteToRune := make([]int, len(value)+1)
	ri := 0
	for bi := range value {
		byteToRune[bi] = ri
		ri++
	}
	byteToRune[len(value)] = ri
	startRune := byteToRune[firstStart] - fragSize/4
	if startRune < 0 {
		startRune = 0
	}
	endRune := startRune + fragSize
	if endRune > len(runes) {
		endRune = len(runes)
	}
	var sb strings.Builder
	cursor := startRune
	for _, sp := range spans {
		s, e := byteToRune[sp.start], byteToRune[sp.end]
		if s < startRune || e > endRune {
			continue
		}
		sb.WriteString(string(runes[cursor:s]))
		sb.WriteString(pre)
		sb.WriteString(string(runes[s:e]))
		sb.WriteString(post)
		cursor = e
	}
	sb.WriteString(string(runes[cursor:endRune]))
	return sb.String()
}

// scroll and point in time ----------------------------------------------

type scrollState struct {
	remaining []*hit
	sr        *searchRequest
	total     int
	targets   []target
	expires   time.Time
	keepAlive time.Duration
}

type pitState struct {
	targets []target
	expires time.Time
}

var scrollCounter atomic.Int64

func (c *Cluster) newScroll(remaining []*hit, total int, sr *searchRequest, ts []target) string {
	n := scrollCounter.Add(1)
	id := "osmem-scroll-" + strconv.FormatInt(n, 10) + "-" + strconv.FormatInt(c.now().UnixNano(), 36)
	c.scrollMu.Lock()
	defer c.scrollMu.Unlock()
	c.scrolls[id] = &scrollState{remaining: remaining, sr: sr, total: total, targets: ts, expires: c.now().Add(sr.scroll), keepAlive: sr.scroll}
	return id
}

// Scroll implements POST /_search/scroll.
func (c *Cluster) Scroll(body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	c.scrollMu.Lock()
	defer c.scrollMu.Unlock()
	id := getString(body, "scroll_id")
	if id == "" {
		id = p.Get("scroll_id")
	}
	if id == "" {
		return fail(errIllegalArgument("scroll_id is missing"))
	}
	st, found := c.scrolls[id]
	if !found || c.now().After(st.expires) {
		delete(c.scrolls, id)
		return fail(&Error{Status: 404, Type: "search_phase_execution_exception", Reason: "all shards failed", RootType: "search_context_missing_exception", RootReason: "No search context found for id [" + id + "]"})
	}
	keep := st.keepAlive
	if s := getString(body, "scroll"); s != "" {
		if d, ok := parseDuration(s); ok {
			keep = d
		}
	} else if s := p.Get("scroll"); s != "" {
		if d, ok := parseDuration(s); ok {
			keep = d
		}
	}
	st.expires = c.now().Add(keep)
	page := st.remaining
	if len(page) > st.sr.size {
		page = page[:st.sr.size]
	}
	st.remaining = st.remaining[len(page):]
	hits, err := c.hitsJSON(page, st.sr, st.total)
	if err != nil {
		return fail(err)
	}
	res := M{"_scroll_id": id, "took": 1, "timed_out": false, "_shards": searchShards(st.targets), "hits": hits}
	return ok(res)
}

// ClearScroll implements DELETE /_search/scroll.
func (c *Cluster) ClearScroll(body M, ids string) (Response, error) {
	c.scrollMu.Lock()
	defer c.scrollMu.Unlock()
	var list []string
	list = append(list, getStrings(body, "scroll_id")...)
	list = append(list, splitList(ids)...)
	freed := 0
	for _, id := range list {
		if id == "_all" {
			freed += len(c.scrolls)
			c.scrolls = map[string]*scrollState{}
			continue
		}
		if _, ok := c.scrolls[id]; ok {
			delete(c.scrolls, id)
			freed++
		}
	}
	return ok(M{"succeeded": true, "num_freed": freed})
}

// CreatePIT implements POST /{index}/_search/point_in_time.
func (c *Cluster) CreatePIT(expr string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	keep, valid := parseDuration(p.Get("keep_alive"))
	if !valid {
		return fail(errIllegalArgument("[keep_alive] is required"))
	}
	id := "osmem-pit-" + strconv.FormatInt(scrollCounter.Add(1), 10)
	for _, t := range ts {
		// Retaining the index makes subsequent writes copy-on-write, preserving
		// the exact index state captured by this PIT.
		t.ix.refs.Add(1)
	}
	c.scrollMu.Lock()
	c.pits[id] = &pitState{targets: append([]target(nil), ts...), expires: c.now().Add(keep)}
	c.scrollMu.Unlock()
	return ok(M{"pit_id": id, "_shards": searchShards(ts), "creation_time": c.now().UnixMilli()})
}

// DeletePIT implements DELETE /_search/point_in_time.
func (c *Cluster) DeletePIT(body M, all bool) (Response, error) {
	c.scrollMu.Lock()
	defer c.scrollMu.Unlock()
	var pits []any
	if all {
		for id, st := range c.pits {
			pits = append(pits, M{"pit_id": id, "successful": true})
			releasePIT(st)
			delete(c.pits, id)
		}
	} else {
		for _, id := range getStrings(body, "pit_id") {
			st, ok := c.pits[id]
			if ok {
				releasePIT(st)
			}
			delete(c.pits, id)
			pits = append(pits, M{"pit_id": id, "successful": ok})
		}
	}
	if pits == nil {
		pits = []any{}
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
	var pits []any
	for id, st := range c.pits {
		pits = append(pits, M{"pit_id": id, "creation_time": st.expires.UnixMilli(), "keep_alive": 0})
	}
	if pits == nil {
		pits = []any{}
	}
	return ok(M{"pits": pits})
}

// Count implements _count.
func (c *Cluster) Count(expr string, body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	var q any
	if body != nil {
		q = body["query"]
	}
	if qs := p.Get("q"); qs != "" {
		q = M{"query_string": M{"query": qs}}
	}
	hits, err := c.executeTargets(ts, q, false, false)
	if err != nil {
		return fail(err)
	}
	return ok(M{"count": len(hits), "_shards": searchShards(ts)})
}

// MultiSearch implements _msearch.
func (c *Cluster) MultiSearch(expr string, data []byte, p Params) (Response, error) {
	lines := bytes.Split(data, []byte("\n"))
	var responses []any
	i := 0
	next := func() ([]byte, bool) {
		for i < len(lines) {
			l := bytes.TrimSpace(lines[i])
			i++
			if len(l) > 0 {
				return l, true
			}
		}
		return nil, false
	}
	for {
		headerLine, ok := next()
		if !ok {
			break
		}
		header, err := decodeObject(headerLine)
		if err != nil {
			return fail(err)
		}
		bodyLine, ok := next()
		if !ok {
			return fail(errActionRequestValidation("msearch request body is missing"))
		}
		body, err := decodeObject(bodyLine)
		if err != nil {
			return fail(err)
		}
		target := strings.Join(getStrings(header, "index"), ",")
		if target == "" {
			target = expr
		}
		params := Params{}
		for k, v := range p {
			params[k] = v
		}
		for _, k := range []string{"ignore_unavailable", "allow_no_indices", "expand_wildcards"} {
			if v, ok := header[k]; ok {
				params[k] = fmt.Sprint(v)
			}
		}
		res, err := c.Search(target, body, params)
		if err != nil {
			e, ok := err.(*Error)
			if !ok {
				e = &Error{Status: 500, Type: "exception", Reason: err.Error()}
			}
			responses = append(responses, e.Body())
			continue
		}
		rb := res.Body.(M)
		rb["status"] = 200
		responses = append(responses, rb)
	}
	if responses == nil {
		responses = []any{}
	}
	return ok(M{"took": 1, "responses": responses})
}

// Analyze implements _analyze.
func (c *Cluster) Analyze(expr string, body M) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var as *analysisSet
	if expr != "" {
		ix, err := c.resolveWriteIndex(expr)
		if err != nil {
			return fail(err)
		}
		as = ix.analysis
	} else {
		var err error
		as, err = buildAnalysis(nil, nil)
		if err != nil {
			return fail(err)
		}
	}
	name := getString(body, "analyzer")
	if name == "" {
		if field := getString(body, "field"); field != "" && expr != "" {
			ix, _ := c.resolveWriteIndex(expr)
			if f, _, ok := ix.Mapping.resolve(field); ok {
				if f.isKeywordLike() {
					name = "keyword"
				} else {
					name = f.Analyzer
				}
			}
		}
		if name == "" {
			if norm := getString(body, "normalizer"); norm != "" {
				name = "normalizer:" + norm
				if norm == "lowercase" {
					name = "lowercase"
				}
			} else if tok := getString(body, "tokenizer"); tok != "" {
				name = "standard"
				switch tok {
				case "keyword":
					name = "keyword"
				case "whitespace":
					name = "whitespace"
				case "kuromoji_tokenizer":
					name = "kuromoji"
				}
			}
		}
		if name == "" {
			name = "standard"
		}
	}
	an, err := as.analyzerNamed(name)
	if err != nil {
		if strings.HasPrefix(name, "normalizer:") {
			an, err = as.normalizerNamed(strings.TrimPrefix(name, "normalizer:"))
		}
		if err != nil {
			return fail(err)
		}
	}
	var texts []string
	switch t := body["text"].(type) {
	case string:
		texts = []string{t}
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				texts = append(texts, s)
			}
		}
	}
	var out []any
	offset := 0
	position := 0
	for _, text := range texts {
		for _, t := range an.Analyze([]byte(text)) {
			out = append(out, M{
				"token":        string(t.Term),
				"start_offset": offset + t.Start,
				"end_offset":   offset + t.End,
				"type":         "<ALPHANUM>",
				"position":     position + t.Position - 1,
			})
		}
		offset += len(text) + 1
		position += len(an.Analyze([]byte(text))) + 100
	}
	if out == nil {
		out = []any{}
	}
	return ok(M{"tokens": out})
}
