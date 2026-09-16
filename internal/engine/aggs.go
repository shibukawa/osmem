package engine

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"
)

// Aggregations
//
// A request's aggregations are parsed into a tree of aggDefs the way
// OpenSearch parses them (aggs_parse.go), their pipelines are validated
// (action_request_validation_exception), every searched index is checked
// like a shard creates its aggregator factories, and the results are
// computed over the matching documents in index order. Results are
// aggResults: pipelines read and rewrite them, and they are rendered at the
// end with meta and typed_keys.

// aggDef is one aggregation of a request.
type aggDef struct {
	name   string
	kind   string
	typ    *aggType
	body   M
	meta   M
	subs   []*aggDef // regular sub-aggregations, by name
	pipes  []*aggDef // pipeline sub-aggregations, in execution order
	parent *aggDef
	spec   any // parsed type configuration
}

// bucket cardinality of an aggregation type (AggregationBuilder.BucketCardinality)
const (
	cardNone = iota
	cardOne
	cardMany
)

// aggType describes an aggregation type.
type aggType struct {
	class    string // Java builder class, used by validation messages
	pipeline bool
	card     int
	leaf     bool // cannot accept sub-aggregations
	metric   int  // numeric metrics: metricSingle or metricMulti
	// hasMetric reports the metric names of a multi-value metric
	hasMetric func(d *aggDef, name string) bool
	parse     func(ps *aggParser, d *aggDef) error
	// prepare runs the shard-level checks of the aggregation for one
	// index (factory and aggregator creation).
	prepare func(pc *prepareCtx, d *aggDef) error
	collect func(ac *aggContext, d *aggDef, hits []*hit) (*aggResult, error)
	// pipelines: sibling pipelines compute a result from the aggregations
	// of their level; parent pipelines rewrite the buckets of their parent.
	sibling func(ac *aggContext, d *aggDef, level []*aggResult) (*aggResult, error)
	parent  func(ac *aggContext, d *aggDef, r *aggResult) error
	// validate is the pipeline validation of the search request
	validate func(vc *validationCtx, d *aggDef)
	paths    func(d *aggDef) []string // buckets paths of a pipeline
}

// numeric metric aggregations (NumericMetricsAggregator)
const (
	metricNone = iota
	metricSingle
	metricMulti
)

// result kinds
const (
	resValue        = iota // numeric single-value metric
	resMultiValue          // numeric multi-value metric
	resSingleBucket        // single-bucket aggregation
	resBuckets             // multi-bucket aggregation
	resOther               // top_hits, geo_bounds, ...
)

// aggResult is the final result of one aggregation.
type aggResult struct {
	name  string
	typed string // type name used by typed_keys
	meta  M
	kind  int
	def   *aggDef

	value     float64                            // resValue
	metric    func(name string) (float64, error) // resMultiValue
	javaClass string                             // Java class name in path resolution errors

	docCount int64        // resSingleBucket
	subs     []*aggResult // resSingleBucket

	buckets []*bucket // resBuckets
	keyed   bool      // render buckets as an object

	fields M // further JSON fields (value_as_string, sum_other_doc_count, ...)
	// render replaces the default rendering of fields, buckets and subs
	render func(ac *aggContext, r *aggResult) M
}

// bucket is one bucket of a multi-bucket aggregation.
type bucket struct {
	key       any    // "key" JSON value
	noKey     bool   // no key field (keyed ranges, filters)
	keyString string // the key as text (getKeyAsString)
	asString  bool   // render key_as_string
	keyNum    float64
	numeric   bool // keyNum is the key (histograms)
	keyedName string
	docCount  int64
	hits      []*hit
	subs      []*aggResult
	fields    M
	sortKey   any
}

// aggContext holds the state of one aggregation run.
type aggContext struct {
	c          *Cluster
	ts         []target
	indices    []*Index
	nested     []string // paths of the enclosing nested aggregations
	typedKeys  bool
	totalAsInt bool
	all        []*hit
	allLoaded  bool
	buckets    int // buckets consumed by the final reduce
	now        time.Time
	sources    map[vsKey]*valuesSource
	auxs       map[vsKey]any
	docsCache  map[*Index][]*hit
	matches    map[matchKey]map[string]bool
}

// maxBuckets is the default search.max_buckets setting.
const maxBuckets = 65535

// runAggregations computes the aggregations of a search over the hits of its
// query; ts are the searched targets.
func (c *Cluster) runAggregations(sr *searchRequest, hits []*hit, ts []target, p Params) (M, error) {
	defs, pipes, err := parseAggregations(sr.aggs)
	if err != nil {
		return nil, err
	}
	ac := &aggContext{c: c, ts: ts, typedKeys: p.Bool("typed_keys", false), totalAsInt: sr.totalAsInt, now: c.now()}
	seen := map[*Index]bool{}
	for _, t := range ts {
		if !seen[t.ix] {
			seen[t.ix] = true
			ac.indices = append(ac.indices, t.ix)
		}
	}
	sort.Slice(ac.indices, func(i, j int) bool { return ac.indices[i].Name < ac.indices[j].Name })
	for _, ix := range ac.indices {
		pc := &prepareCtx{ac: ac, ix: ix}
		if err := pc.prepareLevel(defs, nil); err != nil {
			return nil, err
		}
	}
	results, err := ac.collectLevel(defs, pipes, docOrder(hits))
	if err != nil {
		return nil, err
	}
	out := M{}
	ac.putResults(out, results)
	return out, nil
}

// docOrder returns the hits in index order: by index, then by the order the
// documents were written (Lucene doc ids), nested objects in source order.
func docOrder(hits []*hit) []*hit {
	out := append([]*hit(nil), hits...)
	sort.SliceStable(out, func(i, j int) bool { return hitBefore(out[i], out[j]) })
	return out
}

func hitBefore(a, b *hit) bool {
	if a.ix != b.ix {
		return a.ix.Name < b.ix.Name
	}
	ra, rb := a.doc.rootDoc(), b.doc.rootDoc()
	if ra.SeqNo != rb.SeqNo {
		return ra.SeqNo < rb.SeqNo
	}
	for k := 0; k < len(a.doc.nested) && k < len(b.doc.nested); k++ {
		if a.doc.nested[k].offset != b.doc.nested[k].offset {
			return a.doc.nested[k].offset < b.doc.nested[k].offset
		}
	}
	// nested documents precede their parent in Lucene's block
	return len(a.doc.nested) > len(b.doc.nested)
}

// allHits returns every document of the searched targets (global).
func (ac *aggContext) allHits() ([]*hit, error) {
	if !ac.allLoaded {
		all, err := ac.c.executeTargets(ac.ts, nil, false, false)
		if err != nil {
			return nil, err
		}
		ac.all = docOrder(all)
		ac.allLoaded = true
	}
	return ac.all, nil
}

// collectLevel computes the aggregations of one level and then its sibling
// pipelines.
func (ac *aggContext) collectLevel(defs, pipes []*aggDef, hits []*hit) ([]*aggResult, error) {
	results := make([]*aggResult, 0, len(defs)+len(pipes))
	for _, d := range defs {
		r, err := ac.collectOne(d, hits)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	for _, p := range pipes {
		if p.typ.sibling == nil {
			continue
		}
		r, err := p.typ.sibling(ac, p, results)
		if err != nil {
			return nil, err
		}
		ac.finish(p, r)
		results = append(results, r)
	}
	return results, nil
}

// collectOne computes one aggregation with its sub-aggregations and applies
// its pipelines.
func (ac *aggContext) collectOne(d *aggDef, hits []*hit) (*aggResult, error) {
	r, err := d.typ.collect(ac, d, hits)
	if err != nil {
		return nil, err
	}
	ac.finish(d, r)
	for _, p := range d.pipes {
		switch {
		case p.typ.parent != nil:
			if err := p.typ.parent(ac, p, r); err != nil {
				return nil, err
			}
		case p.typ.sibling != nil:
			if err := ac.siblingInside(p, r); err != nil {
				return nil, err
			}
		}
	}
	if r.kind == resBuckets {
		ac.buckets += len(r.buckets)
		if ac.buckets > maxBuckets {
			return nil, errTooManyBuckets(ac.buckets)
		}
	}
	return r, nil
}

// siblingInside runs a sibling pipeline declared inside an aggregation over
// the aggregations of each of its buckets.
func (ac *aggContext) siblingInside(p *aggDef, r *aggResult) error {
	add := func(level []*aggResult) ([]*aggResult, error) {
		res, err := p.typ.sibling(ac, p, level)
		if err != nil {
			return nil, err
		}
		ac.finish(p, res)
		return append(level, res), nil
	}
	var err error
	switch r.kind {
	case resBuckets:
		for _, b := range r.buckets {
			if b.subs, err = add(b.subs); err != nil {
				return err
			}
		}
	case resSingleBucket:
		r.subs, err = add(r.subs)
	}
	return err
}

// collectSubs computes the sub-aggregations of every bucket; the pipelines
// declared with them run afterwards on the whole aggregation (collectOne).
func (ac *aggContext) collectSubs(d *aggDef, buckets []*bucket) error {
	for _, b := range buckets {
		subs, err := ac.collectLevel(d.subs, nil, b.hits)
		if err != nil {
			return err
		}
		b.subs = subs
	}
	return nil
}

func (ac *aggContext) finish(d *aggDef, r *aggResult) {
	r.name = d.name
	r.def = d
	r.meta = d.meta
	if r.typed == "" {
		r.typed = d.kind
	}
}

// singleBucket builds a single-bucket result over hits.
func (ac *aggContext) singleBucket(d *aggDef, hits []*hit) (*aggResult, error) {
	subs, err := ac.collectLevel(d.subs, nil, hits)
	if err != nil {
		return nil, err
	}
	return &aggResult{kind: resSingleBucket, docCount: int64(len(hits)), subs: subs}, nil
}

// rendering ---------------------------------------------------------------

func (ac *aggContext) putResults(out M, results []*aggResult) {
	for _, r := range results {
		key := r.name
		if ac.typedKeys {
			key = r.typed + "#" + r.name
		}
		out[key] = ac.resultJSON(r)
	}
}

func (ac *aggContext) resultJSON(r *aggResult) M {
	var out M
	if r.render != nil {
		out = r.render(ac, r)
	} else {
		out = M{}
		for k, v := range r.fields {
			out[k] = v
		}
		switch r.kind {
		case resSingleBucket:
			out["doc_count"] = r.docCount
			ac.putResults(out, r.subs)
		case resBuckets:
			out["buckets"] = ac.bucketsJSON(r)
		}
	}
	if r.meta != nil {
		out["meta"] = r.meta
	}
	return out
}

func (ac *aggContext) bucketsJSON(r *aggResult) any {
	if r.keyed {
		m := M{}
		for _, b := range r.buckets {
			m[b.keyedName] = ac.bucketJSON(b)
		}
		return m
	}
	list := make([]any, 0, len(r.buckets))
	for _, b := range r.buckets {
		list = append(list, ac.bucketJSON(b))
	}
	return list
}

func (ac *aggContext) bucketJSON(b *bucket) M {
	out := M{"doc_count": b.docCount}
	for k, v := range b.fields {
		out[k] = v
	}
	if !b.noKey {
		out["key"] = b.key
	}
	if b.asString {
		out["key_as_string"] = b.keyString
	}
	ac.putResults(out, b.subs)
	return out
}

// errors --------------------------------------------------------------------

// errXContent is an x_content_parse_exception ("[terms] failed to parse
// field [size]"); OpenSearch prefixes the reason with the location.
func errXContent(cause *Error, format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: fmt.Sprintf(format, args...), Cause: cause}
}

// errAggExecution is an AggregationExecutionException (HTTP 500).
func errAggExecution(format string, args ...any) *Error {
	return &Error{Status: http.StatusInternalServerError, Type: "aggregation_execution_exception", Reason: fmt.Sprintf(format, args...)}
}

// errJava is a plain Java exception of the given type.
func errJava(status int, typ, reason string) *Error {
	return &Error{Status: status, Type: typ, Reason: reason, plain: true}
}

// errReduce is a failure of the final reduce on the coordinating node.
func errReduce(cause *Error) *Error {
	status := cause.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return &Error{Status: status, Type: "search_phase_execution_exception", Reason: "",
		Extra: M{"phase": "fetch", "grouped": true, "failed_shards": []any{}}, noRootCause: true, Cause: cause}
}

func errTooManyBuckets(count int) *Error {
	return errReduce(&Error{Status: http.StatusServiceUnavailable, Type: "too_many_buckets_exception",
		Reason: fmt.Sprintf("Trying to create too many buckets. Must be less than or equal to: [%d] but was [%d]. This limit can be set by changing the [search.max_buckets] cluster level setting.", maxBuckets, count),
		Extra:  map[string]any{"max_buckets": maxBuckets}})
}

// shardError reports an error of the aggregation phase of one index as a
// shard failure.
func shardError(err error, ix *Index) error {
	e, ok := err.(*Error)
	if !ok {
		return err
	}
	if e.Type == "search_phase_execution_exception" {
		return e
	}
	if ix != nil && e.Index == "" {
		e.Index = ix.Name
	}
	return errSearchPhase(e)
}

func errAggField(h *hit, field string) error {
	return errSearchPhase(errTextFielddata(field, h.ix))
}

func errTextFielddata(field string, ix *Index) *Error {
	e := &Error{Status: 400, Type: "illegal_argument_exception", Reason: "Text fields are not optimised for operations that require per-document field data like aggregations and sorting, so these operations are disabled by default. Please use a keyword field instead. Alternatively, set fielddata=true on [" + field + "] in order to load field data by uninverting the inverted index. Note that this can use significant memory."}
	if ix != nil {
		e.Index = ix.Name
	}
	return e
}

func errDocValuesDisabled(h *hit, field string, f *Field) error {
	return errSearchPhase(&Error{Status: 400, Type: "illegal_argument_exception", Reason: "Can't load fielddata on [" + field + "] because fielddata is unsupported on fields of type [" + f.Type + "]. Use doc values instead.", Index: h.ix.Name})
}

// finite reports whether v is neither NaN nor infinite.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// nullable renders a double that OpenSearch writes as null when not finite.
func nullable(v float64) any {
	if !finite(v) {
		return nil
	}
	return v
}
