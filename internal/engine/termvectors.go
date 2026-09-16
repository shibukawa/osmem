package engine

import (
	"net/http"
	"strings"
)

// Term vectors (GET/POST /{index}/_termvectors/{id} and
// GET/POST[/{index}]/_mtermvectors): per-field term statistics of a stored
// document. osmem has no access to bleve's own term vector storage, so
// fields are re-analyzed the way the highlighters analyze stored field
// values (hlFieldValues, hlAnalyze in highlight_hit.go) rather than reading
// a precomputed vector; this matches OpenSearch's own fallback of
// generating term vectors on the fly for fields that were not mapped with
// a term_vector option (verified against a live 3.8.0 server). The
// "doc"/"per_field_analyzer" on-the-fly analysis mode (term vectors of a
// document supplied in the request rather than one already indexed) is not
// implemented.

// termVectorsOptions are the flags of a term vectors request, read from
// query parameters and overridden by the request or per-doc body.
type termVectorsOptions struct {
	fields          []string
	fieldStatistics bool
	termStatistics  bool
	positions       bool
	offsets         bool
	realtime        bool
	routing         string
}

func defaultTermVectorsOptions() termVectorsOptions {
	return termVectorsOptions{fieldStatistics: true, positions: true, offsets: true, realtime: true}
}

// applyParams overlays URL parameters onto o.
func (o termVectorsOptions) applyParams(p Params) (termVectorsOptions, error) {
	if p.Has("fields") {
		o.fields = splitList(p.Get("fields"))
	}
	var err error
	if o.fieldStatistics, err = paramBool(p, "field_statistics", o.fieldStatistics); err != nil {
		return o, err
	}
	if o.termStatistics, err = paramBool(p, "term_statistics", o.termStatistics); err != nil {
		return o, err
	}
	if o.positions, err = paramBool(p, "positions", o.positions); err != nil {
		return o, err
	}
	if o.offsets, err = paramBool(p, "offsets", o.offsets); err != nil {
		return o, err
	}
	// payloads is accepted for compatibility; osmem has no payload data to
	// report regardless of a field's term_vector mapping.
	if _, err = paramBool(p, "payloads", false); err != nil {
		return o, err
	}
	if o.realtime, err = paramBool(p, "realtime", o.realtime); err != nil {
		return o, err
	}
	if p.Has("routing") {
		o.routing = p.Get("routing")
	}
	return o, nil
}

// applyBody overlays the fields a term vectors request or one doc of a
// multi term vectors request can carry, ignoring values of the wrong type
// (a full re-validation of the request body is not implemented).
func (o termVectorsOptions) applyBody(body M) termVectorsOptions {
	if v, ok := body["fields"].([]any); ok {
		fields := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				fields = append(fields, s)
			}
		}
		o.fields = fields
	}
	if v, ok := body["field_statistics"].(bool); ok {
		o.fieldStatistics = v
	}
	if v, ok := body["term_statistics"].(bool); ok {
		o.termStatistics = v
	}
	if v, ok := body["positions"].(bool); ok {
		o.positions = v
	}
	if v, ok := body["offsets"].(bool); ok {
		o.offsets = v
	}
	if v, ok := body["realtime"].(bool); ok {
		o.realtime = v
	}
	if v, ok := body["routing"].(string); ok {
		o.routing = v
	}
	return o
}

// termVectorMapped reports whether a field's mapping requests term vectors
// be stored; it is only used to pick the default fields of a request that
// names none explicitly (fields named explicitly get vectors regardless).
func termVectorMapped(f *Field) bool {
	tv, _ := f.Extra["term_vector"].(string)
	return tv != "" && tv != "no"
}

// termFieldStats are the corpus-wide statistics of one field's terms,
// gathered by re-analyzing every document that has a value for it (osmem's
// approximation of bleve/Lucene's field-level term dictionary).
type termFieldStats struct {
	docCount int
	docFreq  map[string]int
	ttf      map[string]int
}

func (s *termFieldStats) add(counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	s.docCount++
	for term, n := range counts {
		s.docFreq[term]++
		s.ttf[term] += n
	}
}

func (s *termFieldStats) sums() (sumDocFreq, sumTTF int) {
	for _, n := range s.docFreq {
		sumDocFreq += n
	}
	for _, n := range s.ttf {
		sumTTF += n
	}
	return
}

// fieldTermCounts analyzes one document's field the way the highlighters
// analyze stored field values, returning the occurrence count of each term.
func fieldTermCounts(ix *Index, f *Field, d *Doc, source string) map[string]int {
	values := hlFieldValues(&hit{ix: ix, doc: d}, &hlTarget{field: f, source: source})
	if len(values) == 0 {
		return nil
	}
	counts := map[string]int{}
	_, starts := hlValuesText(values, ' ')
	for _, tok := range hlAnalyze(ix, f, values, starts, nil) {
		counts[tok.term]++
	}
	return counts
}

// indexTermFieldStats accumulates field_statistics/term_statistics for f
// across every (non-nested) document of ix.
func indexTermFieldStats(ix *Index, f *Field, source string) *termFieldStats {
	s := &termFieldStats{docFreq: map[string]int{}, ttf: map[string]int{}}
	for _, d := range ix.docs {
		if d.nested != nil {
			continue
		}
		s.add(fieldTermCounts(ix, f, d, source))
	}
	return s
}

// buildTermVectors renders the term_vectors object of a document: one
// entry per field with a value, from the requested fields or, by default,
// the fields mapped with a term_vector option.
func buildTermVectors(ix *Index, d *Doc, o termVectorsOptions) M {
	names := o.fields
	if len(names) == 0 {
		for _, name := range ix.Mapping.leafFields("*") {
			if f, _, _ := hlResolveField(ix.Mapping, name); f != nil && termVectorMapped(f) {
				names = append(names, name)
			}
		}
	}
	out := M{}
	for _, name := range names {
		f, full, source := hlResolveField(ix.Mapping, name)
		if f == nil || out[full] != nil {
			continue
		}
		values := hlFieldValues(&hit{ix: ix, doc: d}, &hlTarget{field: f, source: source})
		if len(values) == 0 {
			continue
		}
		_, starts := hlValuesText(values, ' ')
		tokens := hlAnalyze(ix, f, values, starts, nil)
		if len(tokens) == 0 {
			continue
		}
		type termAgg struct {
			freq int
			toks []hlToken
		}
		agg := map[string]*termAgg{}
		for _, tok := range tokens {
			a := agg[tok.term]
			if a == nil {
				a = &termAgg{}
				agg[tok.term] = a
			}
			a.freq++
			a.toks = append(a.toks, tok)
		}
		var stats *termFieldStats
		if o.fieldStatistics || o.termStatistics {
			stats = indexTermFieldStats(ix, f, source)
		}
		terms := M{}
		for term, a := range agg {
			entry := M{"term_freq": a.freq}
			if o.termStatistics {
				entry["doc_freq"] = stats.docFreq[term]
				entry["ttf"] = stats.ttf[term]
			}
			if o.positions || o.offsets {
				list := make([]any, len(a.toks))
				for i, tok := range a.toks {
					tokenOut := M{}
					if o.positions {
						tokenOut["position"] = tok.pos
					}
					if o.offsets {
						tokenOut["start_offset"], tokenOut["end_offset"] = tok.start, tok.end
					}
					list[i] = tokenOut
				}
				entry["tokens"] = list
			}
			terms[term] = entry
		}
		field := M{}
		if o.fieldStatistics {
			sumDocFreq, sumTTF := stats.sums()
			field["field_statistics"] = M{"sum_doc_freq": sumDocFreq, "doc_count": stats.docCount, "sum_ttf": sumTTF}
		}
		field["terms"] = terms
		out[full] = field
	}
	return out
}

// termVectorsDocJSON is the response body of one document's term vectors,
// shared by TermVectors and MultiTermVectors.
func termVectorsDocJSON(ix *Index, id string, o termVectorsOptions) M {
	d := ix.docs[id]
	found := d != nil
	if found && !o.realtime {
		// realtime=false requires the document to be visible without
		// relying on osmem's always-live document map (GetDoc, MultiGet and
		// _explain read that map unconditionally).
		found = d.SeqNo <= ix.refreshedSeqNo
	}
	if !found {
		return M{"_index": ix.Name, "_id": id, "_version": 0, "found": false, "took": 0}
	}
	out := M{"_index": ix.Name, "_id": id, "_version": d.Version, "found": true, "took": 0}
	if tv := buildTermVectors(ix, d, o); len(tv) > 0 {
		out["term_vectors"] = tv
	}
	return out
}

// TermVectors implements GET/POST /{index}/_termvectors/{id}.
func (c *Cluster) TermVectors(index, id string, body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	o, err := defaultTermVectorsOptions().applyParams(p)
	if err != nil {
		return fail(err)
	}
	o = o.applyBody(body)
	ix, err := c.resolveDocIndex(index)
	if err != nil {
		return fail(err)
	}
	if err := requireRouting(ix, id, o.routing); err != nil {
		return fail(err)
	}
	return ok(termVectorsDocJSON(ix, id, o))
}

// mtvErrorJSON is one failed doc of a multi term vectors response. Real
// OpenSearch fabricates a Java stack_trace for error_trace=true on every
// per-item error, not just the top-level response; osmem cannot reproduce
// a real one, so it renders a plausible first frame naming the exception
// (sufficient for clients that only check the trace mentions the failure).
func mtvErrorJSON(index, id string, err error, errorTrace bool) M {
	e, isErr := err.(*Error)
	if !isErr {
		e = &Error{Status: http.StatusInternalServerError, Type: "exception", Reason: err.Error()}
	}
	body := e.Body()["error"].(M)
	if errorTrace {
		body["stack_trace"] = fakeStackTrace(e)
		if causes, ok := body["root_cause"].([]any); ok {
			for i, root := range e.rootCauses() {
				if i < len(causes) {
					if cm, ok := causes[i].(M); ok {
						cm["stack_trace"] = fakeStackTrace(root)
					}
				}
			}
		}
	}
	return M{"_index": index, "_id": id, "error": body}
}

// fakeStackTrace renders a first stack frame in OpenSearchException's
// format ("[index] ExceptionClass[reason]" or "ExceptionClass[reason]"
// without an associated resource), followed by one filler frame.
func fakeStackTrace(e *Error) string {
	prefix := ""
	if e.Index != "" {
		prefix = "[" + e.Index + "] "
	}
	return prefix + javaExceptionClassName(e.Type) + "[" + e.Reason + "]\n\tat org.opensearch.osmem.Engine.execute(Engine.java:1)"
}

// javaExceptionClassName mirrors OpenSearchException's error type to its
// Java class name (the inverse of OpenSearchException.getExceptionName):
// index_not_found_exception becomes IndexNotFoundException.
func javaExceptionClassName(errType string) string {
	var b strings.Builder
	for _, part := range strings.Split(errType, "_") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		b.WriteString(part[1:])
	}
	return b.String()
}

// mtvItem is one document of a multi term vectors request.
type mtvItem struct {
	index string
	id    string
	idSet bool
	opts  termVectorsOptions
}

// parseMtvItems reads the docs/ids array of a multi term vectors request
// body (IndicesModule's docs form carries per-doc overrides; the ids form
// applies base to every id, the way MultiGet's mgetItem parsing does).
func parseMtvItems(body M, index string, base termVectorsOptions) ([]mtvItem, error) {
	var items []mtvItem
	switch docs := body["docs"].(type) {
	case []any:
		for _, raw := range docs {
			m, ok := raw.(M)
			if !ok {
				return nil, errIllegalArgument("docs array element should include an object")
			}
			it := mtvItem{index: index, opts: base}
			if v, ok := m["_index"].(string); ok {
				it.index = v
			}
			if v, ok := m["_id"].(string); ok {
				it.id, it.idSet = v, true
			}
			it.opts = it.opts.applyBody(m)
			items = append(items, it)
		}
		return items, nil
	case nil:
	default:
		return nil, errIllegalArgument("docs array element should include an object")
	}
	switch ids := body["ids"].(type) {
	case []any:
		for _, raw := range ids {
			s, ok := raw.(string)
			if !ok {
				return nil, errIllegalArgument("ids array element should only contain ids")
			}
			items = append(items, mtvItem{index: index, id: s, idSet: true, opts: base})
		}
	case nil:
	default:
		return nil, errIllegalArgument("ids array element should only contain ids")
	}
	return items, nil
}

// MultiTermVectors implements GET/POST /_mtermvectors and
// GET/POST /{index}/_mtermvectors.
func (c *Cluster) MultiTermVectors(index string, body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	base, err := defaultTermVectorsOptions().applyParams(p)
	if err != nil {
		return fail(err)
	}
	base = base.applyBody(body)
	items, err := parseMtvItems(body, index, base)
	if err != nil {
		return fail(err)
	}
	errorTrace, _ := paramBool(p, "error_trace", false)
	docs := make([]any, 0, len(items))
	for _, it := range items {
		if !it.idSet {
			docs = append(docs, mtvErrorJSON(it.index, it.id, errIllegalArgument("id is missing"), errorTrace))
			continue
		}
		ix, err := c.resolveDocIndex(it.index)
		if err != nil {
			docs = append(docs, mtvErrorJSON(it.index, it.id, err, errorTrace))
			continue
		}
		if err := requireRouting(ix, it.id, it.opts.routing); err != nil {
			docs = append(docs, mtvErrorJSON(ix.Name, it.id, err, errorTrace))
			continue
		}
		docs = append(docs, termVectorsDocJSON(ix, it.id, it.opts))
	}
	return ok(M{"docs": docs})
}
