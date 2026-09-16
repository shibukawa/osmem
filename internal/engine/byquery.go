package engine

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// by-query and reindex requests (AbstractBulkByScrollRequest)

type byQueryRequest struct {
	kind         string // delete, update, reindex
	proceed      bool
	maxDocs      int // -1: all matches
	batchSize    int
	slices       int // 0: auto
	rps          float64
	rpsSet       bool
	waitForDone  bool
	wait         activeShardCount
	timeout      timeValue
	pipeline     *string
	script       bool
	slice        *[2]int // manual slice id, max
	from         bool
	storedFields bool
	search       M
	searchParams Params
	// frame is the compact rendering the search source is parsed from
	// (parse errors are located in it)
	frame *compactFrame
}

func newByQueryRequest(kind string) *byQueryRequest {
	return &byQueryRequest{kind: kind, maxDocs: -1, batchSize: 1000, slices: 1, waitForDone: true, timeout: timeValue{text: "1m"}}
}

func (r *byQueryRequest) setConflicts(v any) error {
	s, _ := v.(string)
	switch s {
	case "proceed":
		r.proceed = true
	case "abort":
		r.proceed = false
	default:
		return errIllegalArgument("conflicts may only be \"proceed\" or \"abort\" but was [%s]", s)
	}
	return nil
}

func (r *byQueryRequest) setMaxDocs(n int) error {
	if r.maxDocs != -1 && r.maxDocs != n {
		return errIllegalArgument("[max_docs] set to two different values [%d] and [%d]", r.maxDocs, n)
	}
	if n < 0 {
		return errIllegalArgument("[max_docs] parameter cannot be negative, found [%d]", n)
	}
	if n < r.slices {
		return errIllegalArgument("[max_docs] should be >= [slices]")
	}
	r.maxDocs = n
	return nil
}

func intOf(v any) (int, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		return int(f), err == nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return int(f), err == nil
	case float64:
		return int(t), true
	}
	return 0, false
}

// commonOptions is AbstractBaseReindexRestHandler.setCommonOptions plus the
// wait_for_completion parameter.
func (r *byQueryRequest) commonOptions(p Params) error {
	if _, err := paramBool(p, "refresh", false); err != nil {
		return err
	}
	var err error
	if r.timeout, err = paramTime(p, "timeout", "1m"); err != nil {
		return err
	}
	if p.Has("slices") {
		s := p.Get("slices")
		if s == "auto" {
			r.slices = 0
		} else {
			n, perr := strconv.ParseInt(s, 10, 32)
			if perr != nil {
				return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception",
					Reason: "[slices] must be a positive integer or the string \"auto\", but was [" + s + "]", Cause: numberFormatCause(s)}
			}
			if n < 1 {
				return errIllegalArgument("[slices] must be a positive integer or the string \"auto\", but was [%s]", s)
			}
			if r.maxDocs != -1 && r.maxDocs < int(n) {
				return errIllegalArgument("[max_docs] should be >= [slices]")
			}
			r.slices = int(n)
		}
	}
	if r.wait, err = paramActiveShards(p); err != nil {
		return err
	}
	if p.Has("requests_per_second") {
		s := p.Get("requests_per_second")
		f, perr := strconv.ParseFloat(s, 32)
		if perr != nil {
			return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception",
				Reason: "[requests_per_second] must be a float greater than 0. Use -1 to disable throttling.", Cause: numberFormatCause(s)}
		}
		if f != -1 {
			if f <= 0 {
				return errIllegalArgument("[requests_per_second] must be a float greater than 0. Use -1 to disable throttling.")
			}
			r.rps, r.rpsSet = f, true
		}
	}
	if p.Has("max_docs") {
		n, _, err := paramInt(p, "max_docs")
		if err != nil {
			return err
		}
		if err := r.setMaxDocs(n); err != nil {
			return err
		}
	}
	if r.waitForDone, err = paramBool(p, "wait_for_completion", true); err != nil {
		return err
	}
	return nil
}

func (r *byQueryRequest) validate(v *docValidation) {
	if r.from {
		v.add("using [from] is not allowed in a scroll context")
		v.add("from is not supported in this context")
	}
	if r.storedFields {
		v.add("stored_fields is not supported in this context")
	}
	if r.slice != nil && r.slices != 1 {
		v.add("can't specify both manual and automatic slicing at the same time")
	}
}

// searchQueryParams keeps the URL parameters that shape the query.
func searchQueryParams(p Params) Params {
	out := Params{}
	for _, k := range []string{"q", "df", "analyzer", "default_operator", "lenient", "analyze_wildcard", "sort"} {
		if p.Has(k) {
			out[k] = p.Get(k)
		}
	}
	return out
}

func parseSliceSpec(v any) (*[2]int, error) {
	m, _ := v.(M)
	id, max := -1, -1
	if n, ok := intOf(m["id"]); ok {
		id = n
	}
	if n, ok := intOf(m["max"]); ok {
		max = n
	}
	field := func(name, reason string) error {
		return xParseError("[slice] failed to parse field ["+name+"]", errIllegalArgument("%s", reason)).at(valueEndTok(m, name))
	}
	if id < 0 {
		return nil, field("id", "id must be greater than or equal to 0")
	}
	if max <= 1 {
		return nil, field("max", "max must be greater than 1")
	}
	if id >= max {
		// detected at the later of the two fields
		return nil, xParseError("[slice] failed to parse field [id]", errIllegalArgument("max must be greater than id")).at(nthValueTok(m, 2, "id", "max"))
	}
	return &[2]int{id, max}, nil
}

// parseByQuery reads a delete/update by query request the way
// AbstractBulkByQueryRestHandler does.
func parseByQuery(kind string, body M, p Params) (*byQueryRequest, error) {
	r := newByQueryRequest(kind)
	search := M{}
	for k, v := range body {
		search[k] = v
	}
	// OpenSearch writes the body without its request specific fields out as
	// compact JSON and parses that as the search source
	drop := []string{"conflicts", "max_docs"}
	if kind == "update" {
		drop = append(drop, "script")
	}
	r.frame = newCompactFrame(body, search, drop...)
	if v, ok := search["conflicts"]; ok {
		delete(search, "conflicts")
		if err := r.setConflicts(v); err != nil {
			return nil, err
		}
	}
	if v, ok := search["max_docs"]; ok {
		delete(search, "max_docs")
		n, _ := intOf(v)
		if err := r.setMaxDocs(n); err != nil {
			return nil, err
		}
	}
	if kind == "update" {
		if _, ok := search["script"]; ok {
			delete(search, "script")
			r.script = true
		}
	}
	if v, ok := search["size"]; ok {
		delete(search, "size")
		n, _ := intOf(v)
		if n < 0 {
			return nil, errIllegalArgument("[size] parameter cannot be negative, found [%d]", n)
		}
		r.batchSize = n
	}
	if _, ok := search["from"]; ok {
		delete(search, "from")
		r.from = true
	}
	if _, ok := search["stored_fields"]; ok {
		r.storedFields = true
	}
	if v, ok := search["slice"]; ok {
		delete(search, "slice")
		s, err := parseSliceSpec(v)
		if err != nil {
			r.frame.mark(err)
			return nil, err
		}
		r.slice = s
	}
	r.search, r.searchParams = search, searchQueryParams(p)
	if _, err := parseSearchRequest(r.search, r.searchParams); err != nil {
		r.frame.mark(err)
		return nil, err
	}
	if p.Has("from") {
		r.from = true
	}
	if p.Has("size") {
		n, _, err := paramInt(p, "size")
		if err != nil {
			return nil, err
		}
		if err := r.setMaxDocs(n); err != nil {
			return nil, err
		}
	}
	if p.Has("scroll") {
		if _, err := parseTimeText(p.Get("scroll"), "scroll"); err != nil {
			return nil, err
		}
	}
	if n, has, err := paramInt(p, "scroll_size"); err != nil {
		return nil, err
	} else if has {
		if n < 0 {
			return nil, errIllegalArgument("[size] parameter cannot be negative, found [%d]", n)
		}
		r.batchSize = n
	}
	if p.Has("conflicts") {
		if err := r.setConflicts(p.Get("conflicts")); err != nil {
			return nil, err
		}
	}
	if kind == "update" && p.Has("pipeline") {
		s := p.Get("pipeline")
		r.pipeline = &s
	}
	if err := r.commonOptions(p); err != nil {
		return nil, err
	}
	var v docValidation
	r.validate(&v)
	if kind == "delete" {
		if _, hasQuery := body["query"]; !hasQuery && !p.Has("q") {
			v.add("query is missing")
		}
	}
	if err := v.err(); err != nil {
		return nil, err
	}
	return r, nil
}

// slicing -----------------------------------------------------------------

// encodeUID is Uid.encodeId: the indexed form of an _id.
func encodeUID(id string) []byte {
	numeric := id != "" && id[0] != '0'
	for i := 0; i < len(id) && numeric; i++ {
		numeric = id[i] >= '0' && id[i] <= '9'
	}
	if numeric {
		b := make([]byte, 1+(len(id)+1)/2)
		b[0] = 0xfe
		for i := 0; i < len(id); i += 2 {
			b1 := id[i] - '0'
			b2 := byte(0x0f)
			if i+1 < len(id) {
				b2 = id[i+1] - '0'
			}
			b[1+i/2] = b1<<4 | b2
		}
		return b
	}
	if isURLBase64WithoutPadding(id) {
		if b, err := base64.RawURLEncoding.DecodeString(id); err == nil && len(b) > 0 {
			if b[0] >= 0xfd {
				b = append([]byte{0xfd}, b...)
			}
			return b
		}
	}
	return append([]byte{0xff}, id...)
}

// sliceOf is the slice (TermsSliceQuery on _id) a document belongs to.
func sliceOf(id string, max int) int {
	h := int64(murmur3x86_32(encodeUID(id), 7919))
	m := h % int64(max)
	if m < 0 {
		m += int64(max)
	}
	return int(m)
}

// execution -----------------------------------------------------------------

type bulkByScrollStatus struct {
	sliceID          *int
	total            int
	updated          int
	created          int
	deleted          int
	batches          int
	versionConflicts int
	noops            int
}

type bulkByScrollFailure struct {
	index string
	id    string
	err   *Error
}

// runWorker processes hits in scroll batches, stopping after the batch in
// which a failure happened (version conflicts only count with
// conflicts=proceed).
func (c *Cluster) runWorker(r *byQueryRequest, hits []*hit, maxDocs int, apply func(tx *docTx, wb *writeBatch, h *hit) (string, string, error)) (bulkByScrollStatus, []bulkByScrollFailure) {
	st := bulkByScrollStatus{total: len(hits)}
	if maxDocs >= 0 && maxDocs < st.total {
		st.total = maxDocs
	}
	batchSize := r.batchSize
	if batchSize <= 0 {
		batchSize = 1000
	}
	var failures []bulkByScrollFailure
	for start := 0; start < len(hits); start += batchSize {
		successful := st.created + st.updated + st.deleted
		if maxDocs >= 0 && successful >= maxDocs {
			break
		}
		batch := hits[start:min(start+batchSize, len(hits))]
		if maxDocs >= 0 && maxDocs-successful < len(batch) {
			batch = batch[:maxDocs-successful]
		}
		st.batches++
		tx := c.newDocTx()
		wb := newWriteBatch()
		for _, h := range batch {
			result, index, err := apply(tx, wb, h)
			if err != nil {
				e, isErr := err.(*Error)
				if !isErr {
					e = &Error{Status: http.StatusInternalServerError, Type: "exception", Reason: err.Error()}
				}
				if e.Type == "version_conflict_engine_exception" {
					st.versionConflicts++
					if r.proceed {
						continue
					}
				}
				failures = append(failures, bulkByScrollFailure{index: index, id: h.doc.ID, err: e})
				continue
			}
			switch result {
			case "created":
				st.created++
			case "updated":
				st.updated++
			case "deleted":
				st.deleted++
			case "noop":
				st.noops++
			}
		}
		_ = wb.flush()
		tx.commit()
		if len(failures) > 0 {
			break
		}
	}
	return st, failures
}

func (r *byQueryRequest) statusJSON(st bulkByScrollStatus) M {
	out := M{"total": st.total, "deleted": st.deleted, "batches": st.batches, "version_conflicts": st.versionConflicts,
		"noops": st.noops, "retries": M{"bulk": 0, "search": 0}, "throttled_millis": 0,
		"requests_per_second": Float(-1), "throttled_until_millis": 0}
	if r.rpsSet {
		out["requests_per_second"] = Float(float32(r.rps))
	}
	if r.kind != "delete" {
		out["updated"] = st.updated
	}
	if r.kind == "reindex" {
		out["created"] = st.created
	}
	if st.sliceID != nil {
		out["slice_id"] = *st.sliceID
	}
	return out
}

// execute runs the workers (one per slice) and renders BulkByScrollResponse.
func (c *Cluster) executeByScroll(r *byQueryRequest, hits []*hit, numShards int, apply func(tx *docTx, wb *writeBatch, h *hit) (string, string, error)) Response {
	start := time.Now()
	slices := r.slices
	if slices == 0 {
		slices = numShards
		if slices < 1 {
			slices = 1
		}
	}
	var total bulkByScrollStatus
	var failures []bulkByScrollFailure
	var sliceStatuses []any
	if r.slice != nil {
		id := r.slice[0]
		var mine []*hit
		for _, h := range hits {
			if sliceOf(h.doc.ID, r.slice[1]) == id {
				mine = append(mine, h)
			}
		}
		total, failures = c.runWorker(r, mine, r.maxDocs, apply)
		total.sliceID = &id
	} else if slices > 1 {
		parts := make([][]*hit, slices)
		for _, h := range hits {
			s := sliceOf(h.doc.ID, slices)
			parts[s] = append(parts[s], h)
		}
		maxDocs := r.maxDocs
		if maxDocs >= 0 {
			maxDocs /= slices
		}
		for i := 0; i < slices; i++ {
			st, f := c.runWorker(r, parts[i], maxDocs, apply)
			id := i
			st.sliceID = &id
			sliceStatuses = append(sliceStatuses, r.statusJSON(st))
			failures = append(failures, f...)
			total.total += st.total
			total.updated += st.updated
			total.created += st.created
			total.deleted += st.deleted
			total.batches += st.batches
			total.versionConflicts += st.versionConflicts
			total.noops += st.noops
		}
	} else {
		total, failures = c.runWorker(r, hits, r.maxDocs, apply)
	}
	out := r.statusJSON(total)
	if sliceStatuses != nil {
		out["slices"] = sliceStatuses
	}
	out["took"] = int(time.Since(start).Milliseconds())
	out["timed_out"] = false
	list := make([]any, 0, len(failures))
	status := http.StatusOK
	for _, f := range failures {
		list = append(list, M{"index": f.index, "id": f.id, "cause": f.err.content(), "status": f.err.Status})
		if f.err.Status > status {
			status = f.err.Status
		}
	}
	out["failures"] = list
	return Response{Status: status, Body: out}
}

// tasks -----------------------------------------------------------------------

var bulkByScrollTasks = struct {
	sync.Mutex
	seq     int64
	results map[string]M
}{results: map[string]M{}}

// asTask stores the outcome of a request run with wait_for_completion=false.
func asTask(action, description string, res Response, err error) Response {
	bulkByScrollTasks.Lock()
	defer bulkByScrollTasks.Unlock()
	bulkByScrollTasks.seq++
	seq := bulkByScrollTasks.seq
	id := "osmem-node:" + strconv.FormatInt(seq, 10)
	task := M{"node": "osmem-node", "id": seq, "type": "transport", "action": action, "description": description,
		"start_time_in_millis": time.Now().UnixMilli(), "running_time_in_nanos": 0, "cancellable": true, "cancelled": false, "headers": M{}}
	entry := M{"completed": true, "task": task}
	if err != nil {
		if e, isErr := err.(*Error); isErr {
			entry["error"] = e.content()
		} else {
			entry["error"] = M{"type": "exception", "reason": err.Error()}
		}
	} else if body, isMap := res.Body.(M); isMap {
		entry["response"] = body
		status := M{}
		for k, v := range body {
			switch k {
			case "took", "timed_out", "failures":
			default:
				status[k] = v
			}
		}
		task["status"] = status
	}
	bulkByScrollTasks.results[id] = entry
	return Response{Status: http.StatusOK, Body: M{"task": id}}
}

// GetTask implements GET /_tasks/{task_id} for the tasks of requests run
// with wait_for_completion=false.
func (c *Cluster) GetTask(id string) (Response, error) {
	bulkByScrollTasks.Lock()
	defer bulkByScrollTasks.Unlock()
	if entry, found := bulkByScrollTasks.results[id]; found {
		return ok(entry)
	}
	if !strings.Contains(id, ":") {
		return fail(errIllegalArgument("malformed task id %s", id))
	}
	return fail(&Error{Status: http.StatusNotFound, Type: "resource_not_found_exception", Reason: "task [" + id + "] isn't running and hasn't stored its results"})
}

func shardCount(ts []target) int {
	n := math.MaxInt
	for _, t := range ts {
		if s := getInt(getMap(t.ix.Settings, "index"), "number_of_shards", 1); s < n {
			n = s
		}
	}
	if n == math.MaxInt {
		return 1
	}
	return n
}

// sourceMissing fails a request that re-indexes documents without a stored
// source.
func sourceMissing(hits []*hit) error {
	for _, h := range hits {
		if mappingSourceFilter(h.ix.Mapping).disabled {
			return errIllegalArgument("[%s][%s] didn't store _source", h.ix.Name, h.doc.ID)
		}
	}
	return nil
}

// matchByScroll runs the search of a by-scroll request.
func (c *Cluster) matchByScroll(ts []target, r *byQueryRequest) ([]*hit, error) {
	sr, err := parseSearchRequest(r.search, r.searchParams)
	if err != nil {
		r.frame.mark(err)
		return nil, err
	}
	hits, err := c.executeTargets(ts, sr.query, false, false)
	if err != nil {
		r.frame.mark(err)
		return nil, err
	}
	if err := c.sortHits(hits, sr); err != nil {
		return nil, err
	}
	return hits, nil
}

// DeleteByQuery implements POST /{index}/_delete_by_query.
func (c *Cluster) DeleteByQuery(expr string, body M, p Params) (Response, error) {
	r, err := parseByQuery("delete", body, p)
	if err != nil {
		return fail(err)
	}
	res, err := c.deleteByQuery(expr, r, p)
	if !r.waitForDone {
		return asTask("indices:data/write/delete/byquery", "delete-by-query ["+expr+"]", res, err), nil
	}
	return res, err
}

func (c *Cluster) deleteByQuery(expr string, r *byQueryRequest, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	hits, err := c.matchByScroll(ts, r)
	if err != nil {
		return fail(err)
	}
	return c.executeByScroll(r, hits, shardCount(ts), func(tx *docTx, wb *writeBatch, h *hit) (string, string, error) {
		ix, err := c.docWritable(h.ix.Name)
		if err != nil {
			return "", h.ix.Name, err
		}
		if err := checkActiveShards(ix, r.wait, r.timeout); err != nil {
			return "", ix.Name, err
		}
		if _, err := ix.deleteDoc(tx, h.doc.ID, DocParams{}, wb.forIndex(ix), false); err != nil {
			return "", ix.Name, err
		}
		return "deleted", ix.Name, nil
	}), nil
}

// UpdateByQuery implements POST /{index}/_update_by_query (without scripts
// it re-indexes matching documents).
func (c *Cluster) UpdateByQuery(expr string, body M, p Params) (Response, error) {
	r, err := parseByQuery("update", body, p)
	if err != nil {
		return fail(err)
	}
	if r.script {
		return fail(errUnsupported("update_by_query with script"))
	}
	res, err := c.updateByQuery(expr, r, p)
	if !r.waitForDone {
		return asTask("indices:data/write/update/byquery", "update-by-query ["+expr+"]", res, err), nil
	}
	return res, err
}

func (c *Cluster) updateByQuery(expr string, r *byQueryRequest, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	hits, err := c.matchByScroll(ts, r)
	if err != nil {
		return fail(err)
	}
	if err := sourceMissing(hits); err != nil {
		return fail(err)
	}
	return c.executeByScroll(r, hits, shardCount(ts), func(tx *docTx, wb *writeBatch, h *hit) (string, string, error) {
		if err := c.pipelineFailure(h.ix.Name, r.pipeline); err != nil {
			return "", h.ix.Name, err
		}
		ix, err := c.docWritable(h.ix.Name)
		if err != nil {
			return "", h.ix.Name, err
		}
		if err := checkActiveShards(ix, r.wait, r.timeout); err != nil {
			return "", ix.Name, err
		}
		d := ix.docs[h.doc.ID]
		if d == nil {
			return "noop", ix.Name, nil
		}
		dp := DocParams{OpType: "index", Routing: docRouting(d)}
		if _, _, err := ix.putDoc(tx, d.ID, d.Raw, d.Src, dp, false, wb.forIndex(ix)); err != nil {
			return "", ix.Name, err
		}
		return "updated", ix.Name, nil
	}), nil
}

// reindex -----------------------------------------------------------------

var reindexFields = []string{"source", "dest", "max_docs", "size", "script", "conflicts"}
var reindexDestFields = []string{"index", "routing", "op_type", "pipeline", "version_type"}

type reindexRequest struct {
	*byQueryRequest
	indices     []string
	indicesSet  bool
	remote      M
	sourceOff   bool
	source      sourceFilter
	destIndex   string
	destSet     bool
	destRouting string
	opType      string
	versionType string
	destPipe    *string
}

func parseReindex(body M, p Params) (*reindexRequest, error) {
	r := &reindexRequest{byQueryRequest: newByQueryRequest("reindex"), opType: "index", versionType: "internal"}
	if len(body) == 0 {
		return nil, &Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "request body is required"}
	}
	if p.Has("pipeline") {
		return nil, errIllegalArgument("_reindex doesn't support [pipeline] as a query parameter. Specify it in the [dest] object instead.")
	}
	failed := func(field string, cause *Error) error {
		return xParseError("[reindex] failed to parse field ["+field+"]", cause).atCause(valueEndTok(body, field))
	}
	for _, key := range sortedMKeys(body) {
		v := body[key]
		switch key {
		case "source":
			src, isObj := v.(M)
			if !isObj {
				return nil, xParseError("[reindex] source doesn't support values of type: "+jsonTokenKind(v), nil).at(valueTok(body, key))
			}
			search := M{}
			for k, e := range src {
				search[k] = e
			}
			// the source without index and remote is written out as compact
			// JSON and parsed as a search source; the failure is reported at
			// the end of the source object
			frame := newCompactFrame(src, search, "index", "remote")
			r.frame = frame
			sourceFailed := func(cause *Error) error {
				frame.mark(cause)
				return xParseError("[reindex] failed to parse field [source]", cause).at(valueEndTok(body, "source"))
			}
			if iv, ok := search["index"]; ok {
				delete(search, "index")
				r.indicesSet = true
				switch t := iv.(type) {
				case string:
					r.indices = []string{t}
				case []any:
					for _, e := range t {
						s, _ := scalarText(e)
						r.indices = append(r.indices, s)
					}
				default:
					return nil, sourceFailed(errIllegalArgument("Expected [index] to be a list of a string but was [%s]", javaToString(orderedFromValue(iv))))
				}
			}
			if rv, ok := search["remote"]; ok {
				delete(search, "remote")
				r.remote, _ = rv.(M)
				if r.remote == nil {
					r.remote = M{}
				}
			}
			if sv, ok := search["size"]; ok {
				delete(search, "size")
				n, _ := intOf(sv)
				if n < 0 {
					return nil, sourceFailed(errIllegalArgument("[size] parameter cannot be negative, found [%d]", n))
				}
				r.batchSize = n
			}
			if _, ok := search["from"]; ok {
				delete(search, "from")
				r.from = true
			}
			if _, ok := search["stored_fields"]; ok {
				r.storedFields = true
			}
			if sl, ok := search["slice"]; ok {
				delete(search, "slice")
				spec, err := parseSliceSpec(sl)
				if err != nil {
					return nil, sourceFailed(err.(*Error))
				}
				r.slice = spec
			}
			if sv, ok := search["_source"]; ok {
				sf, err := fetchSourceValue(orderedFromValue(sv))
				if err != nil {
					return nil, sourceFailed(err.(*Error))
				}
				r.source = sf
				r.sourceOff = sf.disabled
				delete(search, "_source")
			}
			if _, err := parseSearchRequest(search, Params{}); err != nil {
				if e, isErr := err.(*Error); isErr {
					return nil, sourceFailed(e)
				}
				return nil, err
			}
			r.search = search
			r.searchParams = Params{}
		case "dest":
			dest, isObj := v.(M)
			if !isObj {
				return nil, xParseError("[reindex] dest doesn't support values of type: "+jsonTokenKind(v), nil).at(valueTok(body, key))
			}
			for _, dk := range sortedMKeys(dest) {
				dv := dest[dk]
				known := false
				for _, f := range reindexDestFields {
					known = known || f == dk
				}
				if !known {
					return nil, failed("dest", xParseError("[dest] unknown field ["+dk+"]"+didYouMean(dk, reindexDestFields), nil).at(keyTok(dest, dk)).atParser(valueTok(dest, dk)))
				}
				s, isString := dv.(string)
				if !isString {
					return nil, failed("dest", xParseError("[dest] "+dk+" doesn't support values of type: "+jsonTokenKind(dv), nil).at(valueTok(dest, dk)))
				}
				switch dk {
				case "index":
					r.destIndex, r.destSet = s, true
				case "routing":
					r.destRouting = s
				case "op_type":
					switch strings.ToLower(s) {
					case "create":
						r.opType = "create"
					case "index":
						r.opType = "index"
					default:
						return nil, failed("dest", xParseError("[dest] failed to parse field [op_type]", errIllegalArgument("opType must be 'create' or 'index', found: [%s]", s)).atCause(valueEndTok(dest, dk)))
					}
				case "pipeline":
					r.destPipe = &s
				case "version_type":
					vt, err := parseVersionType(s)
					if err != nil {
						return nil, failed("dest", xParseError("[dest] failed to parse field [version_type]", err.(*Error)).atCause(valueEndTok(dest, dk)))
					}
					r.versionType = vt
				}
			}
		case "max_docs", "size":
			var n int
			switch t := v.(type) {
			case json.Number, string:
				f, err := strconv.ParseFloat(func() string { s, _ := scalarText(t); return s }(), 64)
				if err != nil {
					s, _ := scalarText(t)
					return nil, failed("max_docs", (&Error{Type: "number_format_exception", Reason: "For input string: \"" + s + "\""}).atParser(valueTok(body, key)))
				}
				n = int(f)
			default:
				return nil, xParseError("[reindex] "+key+" doesn't support values of type: "+jsonTokenKind(v), nil).at(valueTok(body, key))
			}
			if err := r.setMaxDocs(n); err != nil {
				return nil, failed("max_docs", err.(*Error).atParser(valueTok(body, key)))
			}
		case "script":
			if _, isObj := v.(M); !isObj {
				return nil, xParseError("[reindex] script doesn't support values of type: "+jsonTokenKind(v), nil).at(valueTok(body, key))
			}
			r.script = true
		case "conflicts":
			s, isString := v.(string)
			if !isString {
				return nil, xParseError("[reindex] conflicts doesn't support values of type: "+jsonTokenKind(v), nil).at(valueTok(body, key))
			}
			if err := r.setConflicts(s); err != nil {
				return nil, failed("conflicts", err.(*Error))
			}
		default:
			return nil, xParseError("[reindex] unknown field ["+key+"]"+didYouMean(key, reindexFields), nil).at(keyTok(body, key)).atParser(valueTok(body, key))
		}
	}
	if p.Has("scroll") {
		if _, err := parseTimeText(p.Get("scroll"), "scroll"); err != nil {
			return nil, err
		}
	}
	if _, err := paramBool(p, "require_alias", false); err != nil {
		return nil, err
	}
	if err := r.commonOptions(p); err != nil {
		return nil, err
	}
	var v docValidation
	r.validate(&v)
	if len(r.indices) == 0 {
		v.add("use _all if you really want to copy from all existing indexes")
	}
	if r.sourceOff {
		v.add("_source:false is not supported in this context")
	}
	if !r.destSet {
		v.add("index must be specified")
		return nil, v.err()
	}
	switch rt := r.destRouting; {
	case rt == "", rt == "keep", rt == "discard", strings.HasPrefix(rt, "="):
	default:
		v.add("routing must be unset, [keep], [discard] or [=<some new value>]")
	}
	if err := v.err(); err != nil {
		return nil, err
	}
	return r, nil
}

// remoteCheck is the host parsing and allowlist check of a remote source.
func (r *reindexRequest) remoteCheck() error {
	if r.remote == nil {
		return nil
	}
	host, _ := r.remote["host"].(string)
	if host == "" {
		return errIllegalArgument("[host] must be specified to reindex from a remote cluster")
	}
	u, err := url.Parse(host)
	if err != nil || u.Port() == "" {
		return errIllegalArgument("[host] must be of the form [scheme]://[host]:[port](/[pathPrefix])? but was [%s]", host)
	}
	var unsupported []string
	for k := range r.remote {
		switch k {
		case "host", "username", "password", "headers", "socket_timeout", "connect_timeout":
		default:
			unsupported = append(unsupported, k)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return errIllegalArgument("Unsupported fields in [remote]: [%s]", strings.Join(unsupported, ","))
	}
	return errIllegalArgument("[%s:%s] not allowlisted in reindex.remote.allowlist", u.Hostname(), u.Port())
}

// Reindex implements POST /_reindex.
func (c *Cluster) Reindex(body M, p Params) (Response, error) {
	r, err := parseReindex(body, p)
	if err != nil {
		return fail(err)
	}
	res, err := c.reindex(r)
	if !r.waitForDone {
		return asTask("indices:data/write/reindex", "reindex from "+strings.Join(r.indices, ",")+" to ["+r.destIndex+"]", res, err), nil
	}
	return res, err
}

func (c *Cluster) reindex(r *reindexRequest) (Response, error) {
	if err := r.remoteCheck(); err != nil {
		return fail(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	target := r.destIndex
	if _, exists := c.indices[target]; exists || len(c.aliasTargets(target)) > 0 {
		w, err := c.resolveWriteIndex(target)
		if err != nil {
			return fail(err)
		}
		target = w.Name
	}
	ts, err := c.resolve(strings.Join(r.indices, ","), resolveOptions{allowAliases: true, allowNoIndices: true})
	if err != nil {
		return fail(err)
	}
	for _, t := range ts {
		if t.ix.Name == target {
			return fail(docValidation{"reindex cannot write into an index its reading from [" + target + "]"}.err())
		}
	}
	if r.script {
		return fail(errUnsupported("reindex with script"))
	}
	hits, err := c.matchByScroll(ts, r.byQueryRequest)
	if err != nil {
		return fail(err)
	}
	if err := sourceMissing(hits); err != nil {
		return fail(err)
	}
	return c.executeByScroll(r.byQueryRequest, hits, shardCount(ts), func(tx *docTx, wb *writeBatch, h *hit) (string, string, error) {
		if err := c.pipelineFailure(r.destIndex, r.destPipe); err != nil {
			return "", r.destIndex, err
		}
		ix, err := c.docEnsureIndex(r.destIndex)
		if err != nil {
			return "", r.destIndex, err
		}
		if err := checkActiveShards(ix, r.wait, r.timeout); err != nil {
			return "", ix.Name, err
		}
		dp := DocParams{OpType: r.opType, VersionType: r.versionType}
		if isExternalVersioning(r.versionType) {
			v := h.doc.Version
			dp.Version = &v
		}
		switch rt := r.destRouting; {
		case rt == "" || rt == "keep":
			dp.Routing = docRouting(h.doc)
		case strings.HasPrefix(rt, "="):
			dp.Routing = rt[1:]
		}
		raw, src := h.doc.Raw, h.doc.Src
		if indexFilter := mappingSourceFilter(h.ix.Mapping); !indexFilter.isPlain() || !r.source.isPlain() {
			filtered, sourceOK := applySourceFilters(h.doc.Src, indexFilter, r.source)
			if !sourceOK {
				filtered = M{}
			}
			src = filtered
			raw = orderedJSON(orderedFromValue(filtered))
		}
		_, created, err := ix.putDoc(tx, h.doc.ID, raw, src, dp, false, wb.forIndex(ix))
		if err != nil {
			return "", ix.Name, err
		}
		if created {
			return "created", ix.Name, nil
		}
		return "updated", ix.Name, nil
	}), nil
}
