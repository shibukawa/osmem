package engine

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type paramErr struct {
	name string
	err  error
}

// DocParams are the parameters of a single-document write.
type DocParams struct {
	OpType        string // "index" (default), "create" or "delete"
	RawOpType     string // op_type as given in the request
	RequireAlias  bool
	Routing       string
	IfSeqNo       *int64
	IfPrimaryTerm *int64
	Version       *int64
	VersionType   string
	Pipeline      *string
	Timeout       timeValue
	WaitForActive activeShardCount
	// paramErrs are parse errors of the URL parameters in the order
	// OpenSearch's REST handlers read them; the handler reports them after
	// its own earlier checks (request body, op_type of _create).
	paramErrs []paramErr
}

// docParamsFrom parses the URL parameters of the index and delete APIs.
func docParamsFrom(p Params) (DocParams, error) {
	dp := DocParams{OpType: "index", RawOpType: p.Get("op_type"), Routing: p.Get("routing"), Timeout: timeValue{text: "1m"}}
	note := func(name string, err error) {
		if err != nil {
			dp.paramErrs = append(dp.paramErrs, paramErr{name, err})
		}
	}
	if p.Has("pipeline") {
		s := p.Get("pipeline")
		dp.Pipeline = &s
	}
	var err error
	if dp.Timeout, err = paramTime(p, "timeout", "1m"); err != nil {
		note("timeout", err)
	}
	note("refresh", checkRefreshParam(p))
	if v, has, err := paramLong(p, "version"); err != nil {
		note("version", err)
	} else if has {
		dp.Version = &v
	}
	if p.Has("version_type") {
		vt, err := parseVersionType(p.Get("version_type"))
		note("version_type", err)
		dp.VersionType = vt
	}
	if v, has, err := paramLong(p, "if_seq_no"); err != nil {
		note("if_seq_no", err)
	} else if has {
		if err := checkSeqNo(v); err != nil {
			note("if_seq_no", err)
		} else {
			dp.IfSeqNo = &v
		}
	}
	if v, has, err := paramLong(p, "if_primary_term"); err != nil {
		note("if_primary_term", err)
	} else if has {
		if err := checkPrimaryTerm(v); err != nil {
			note("if_primary_term", err)
		} else {
			dp.IfPrimaryTerm = &v
		}
	}
	ra, err := paramBool(p, "require_alias", false)
	note("require_alias", err)
	dp.RequireAlias = ra
	if dp.WaitForActive, err = paramActiveShards(p); err != nil {
		note("wait_for_active_shards", err)
	}
	if p.Has("op_type") {
		switch strings.ToLower(dp.RawOpType) {
		case "create":
			dp.OpType = "create"
		case "index":
		default:
			note("op_type", errIllegalArgument("opType must be 'create' or 'index', found: [%s]", dp.RawOpType))
		}
	}
	return dp, nil
}

// firstParamErr returns the first parameter error, ignoring the named
// parameters (the ones a REST handler does not read).
func (dp *DocParams) firstParamErr(skip ...string) error {
outer:
	for _, pe := range dp.paramErrs {
		for _, s := range skip {
			if pe.name == s {
				continue outer
			}
		}
		return pe.err
	}
	return nil
}

func generateID() string {
	var b [15]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// writeBatch groups bleve writes of one request per index so a bulk
// request commits once per index.
type writeBatch struct {
	batches map[*Index]*docBatch
	order   []*Index
}

func newWriteBatch() *writeBatch { return &writeBatch{batches: map[*Index]*docBatch{}} }

func (wb *writeBatch) forIndex(ix *Index) *docBatch {
	if wb == nil {
		return nil
	}
	b, ok := wb.batches[ix]
	if !ok {
		b = ix.newBatch()
		wb.batches[ix] = b
		wb.order = append(wb.order, ix)
	}
	return b
}

// flush commits the batches, then merges the segments of the written
// indices (their stored documents already reflect the request).
func (wb *writeBatch) flush() error {
	if wb == nil {
		return nil
	}
	for _, ix := range wb.order {
		if err := ix.commit(wb.batches[ix]); err != nil {
			return err
		}
	}
	for _, ix := range wb.order {
		if err := ix.compactRuns(); err != nil {
			return err
		}
	}
	wb.batches = map[*Index]*docBatch{}
	wb.order = nil
	return nil
}

// versionConflictForWrites is VersionType.isVersionConflictForWrites with
// its explanation.
func versionConflictForWrites(vt string, current, expected int64, deleted bool) (string, bool) {
	switch vt {
	case "external", "external_gt":
		if current == versionNotFound {
			return "", false
		}
		if expected == versionMatchAny || current >= expected {
			return "version conflict, current version [" + itoa(current) + "] is higher or equal to the one provided [" + itoa(expected) + "]", true
		}
	case "external_gte":
		if current == versionNotFound {
			return "", false
		}
		if expected == versionMatchAny || current > expected {
			return "version conflict, current version [" + itoa(current) + "] is higher than the one provided [" + itoa(expected) + "]", true
		}
	default:
		switch {
		case expected == versionMatchAny:
		case expected == versionMatchDeleted:
			if !deleted {
				return "version conflict, document already exists (current version [" + itoa(current) + "])", true
			}
		case current != expected:
			if deleted {
				return "version conflict, document does not exist (expected version [" + itoa(expected) + "])", true
			}
			return "version conflict, current version [" + itoa(current) + "] is different than the one provided [" + itoa(expected) + "]", true
		}
	}
	return "", false
}

// updateVersion is VersionType.updateVersion.
func updateVersion(vt string, current, expected int64) int64 {
	if isExternalVersioning(vt) {
		return expected
	}
	if current == versionNotFound {
		return 1
	}
	return current + 1
}

func casConflictReason(seqNo, term, curSeq, curTerm int64) string {
	r := "version conflict, required seqNo [" + itoa(seqNo) + "], primary term [" + itoa(term) + "]. "
	if curSeq == unassignedSeqNo {
		return r + "but no document was found"
	}
	return r + "current document has seqNo [" + itoa(curSeq) + "] and primary term [" + itoa(curTerm) + "]"
}

// currentVersion returns the version a write is planned against: the live
// document's, or its tombstone's.
func (ix *Index) currentVersion(tx *docTx, id string) (existing *Doc, current int64, deleted bool) {
	existing = ix.docs[id]
	if existing != nil {
		return existing, existing.Version, false
	}
	if t, ok := tx.tombstone(ix, id); ok {
		return nil, t.version, true
	}
	return nil, versionNotFound, true
}

// checkWrite runs the compare-and-set and version checks of
// InternalEngine.planIndexingAsPrimary / planDeletionAsPrimary.
func (ix *Index) checkWrite(id string, dp DocParams, existing *Doc, current int64, deleted bool) error {
	if seqNo, term := dp.ifSeqNo(), dp.ifPrimaryTerm(); seqNo != unassignedSeqNo {
		if existing == nil {
			return errVersionConflict(ix.Name, id, casConflictReason(seqNo, term, unassignedSeqNo, 0))
		}
		if existing.SeqNo != seqNo || existing.PrimaryTerm != term {
			return errVersionConflict(ix.Name, id, casConflictReason(seqNo, term, existing.SeqNo, existing.PrimaryTerm))
		}
	}
	version := dp.version()
	if dp.OpType == "create" && version == versionMatchAny {
		version = versionMatchDeleted
	}
	if reason, conflict := versionConflictForWrites(dp.versionType(), current, version, deleted); conflict {
		return errVersionConflict(ix.Name, id, reason)
	}
	return nil
}

// putDoc stores a document. The document is parsed first (mapping errors
// win over version conflicts, as on OpenSearch), then checked against the
// live document or its delete tombstone. When batch is non-nil the bleve
// write is queued on it.
func (ix *Index) putDoc(tx *docTx, id string, raw []byte, src M, dp DocParams, autoID bool, batch *docBatch) (*Doc, bool, error) {
	return ix.putDocFrom(tx, id, raw, nil, src, dp, autoID, batch)
}

// putDocFrom is putDoc for a source parsed from body, the request bytes
// parse errors are located in (nil: the stored source).
func (ix *Index) putDocFrom(tx *docTx, id string, raw, body []byte, src M, dp DocParams, autoID bool, batch *docBatch) (*Doc, bool, error) {
	d := &Doc{ID: id, Raw: raw, Src: src, SeqNo: ix.seqNo + 1, PrimaryTerm: 1}
	// join fields read the routing while the document is parsed
	setDocRouting(d, dp.Routing)
	bds, err := ix.buildDocumentFrom(d, body, true)
	if err != nil {
		return nil, false, err
	}
	existing, current, deleted := ix.currentVersion(tx, id)
	// documents with auto-generated ids are appended without version or
	// compare-and-set checks
	if !autoID {
		if err := ix.checkWrite(id, dp, existing, current, deleted); err != nil {
			return nil, false, err
		}
	}
	version := dp.version()
	if dp.OpType == "create" && version == versionMatchAny {
		version = versionMatchDeleted
	}
	d.Version = updateVersion(dp.versionType(), current, version)
	if autoID {
		d.Version = 1
	}
	b := batch
	if b == nil {
		b = ix.newBatch()
	}
	if err := ix.addDocuments(b, id, bds); err != nil {
		return nil, false, err
	}
	if batch == nil {
		if err := ix.commit(b); err != nil {
			return nil, false, err
		}
	}
	ix.seqNo = d.SeqNo
	ix.docs[id] = d
	if batch == nil {
		if err := ix.compactRuns(); err != nil {
			return nil, false, err
		}
	}
	tx.clearTombstone(ix, id)
	return d, existing == nil, nil
}

type deleteOutcome struct {
	found   bool
	version int64
	seqNo   int64
}

// deleteDoc deletes a document and records its tombstone. Deleting a
// missing document consumes a sequence number too. routed is true for a
// request that addresses the document by (possibly implicit) routing, as
// opposed to one a query already matched (delete_by_query): a document
// written with a different effective routing is then invisible, exactly
// like one that was never indexed on the shard this routing reaches.
func (ix *Index) deleteDoc(tx *docTx, id string, dp DocParams, batch *docBatch, routed bool) (deleteOutcome, error) {
	dp.OpType = "delete"
	existing, current, deleted := ix.currentVersion(tx, id)
	if routed && existing != nil && docShardMismatch(ix, id, dp.Routing, existing) {
		existing, current, deleted = nil, versionNotFound, true
	}
	if err := ix.checkWrite(id, dp, existing, current, deleted); err != nil {
		return deleteOutcome{}, err
	}
	version := updateVersion(dp.versionType(), current, dp.version())
	if existing != nil {
		b := batch
		if b == nil {
			b = ix.newBatch()
		}
		ix.removeDocuments(b, id)
		if batch == nil {
			if err := ix.commit(b); err != nil {
				return deleteOutcome{}, err
			}
		}
		delete(ix.children, id)
		delete(ix.docs, id)
		if batch == nil {
			if err := ix.compactRuns(); err != nil {
				return deleteOutcome{}, err
			}
		}
	}
	ix.seqNo++
	tx.addTombstone(ix, id, tombstone{version: version, seqNo: ix.seqNo, at: tx.now})
	return deleteOutcome{found: existing != nil, version: version, seqNo: ix.seqNo}, nil
}

func writeResult(ix *Index, d *Doc, result string) M {
	return M{
		"_index":        ix.Name,
		"_id":           d.ID,
		"_version":      d.Version,
		"result":        result,
		"_shards":       writeShards(ix),
		"_seq_no":       d.SeqNo,
		"_primary_term": d.PrimaryTerm,
	}
}

func deleteResult(ix *Index, id string, out deleteOutcome) (int, M) {
	result, status := "deleted", http.StatusOK
	if !out.found {
		result, status = "not_found", http.StatusNotFound
	}
	return status, M{"_index": ix.Name, "_id": id, "_version": out.version, "result": result,
		"_shards": writeShards(ix), "_seq_no": out.seqNo, "_primary_term": 1}
}

// consumeSeqNoOnFailure accounts for failures raised by Lucene while adding
// the document: the operation already had a sequence number.
func consumeSeqNoOnFailure(ix *Index, err error) error {
	if e, isErr := err.(*Error); isErr && strings.HasPrefix(e.Reason, "Inconsistency of field data structures") {
		ix.seqNo++
	}
	return err
}

// sourceFromOrdered returns the indexed form (dots expanded) and the stored
// compact bytes of a parsed source.
func sourceFromOrdered(raw []byte, doc *orderedObject) (M, []byte) {
	var buf bytes.Buffer
	if raw == nil || json.Compact(&buf, raw) != nil {
		buf.Reset()
		buf.Write(orderedJSON(doc))
	}
	src := valueFromOrdered(doc).(M)
	if hasDottedKeys(doc) {
		// expandDots copies the whole tree: only when a key needs splitting
		src = expandDots(src)
	}
	return src, buf.Bytes()
}

// hasDottedKeys reports whether any object key in a parsed source contains
// a '.' (see expandDots).
func hasDottedKeys(v any) bool {
	switch t := v.(type) {
	case *orderedObject:
		for _, k := range t.keys {
			if strings.Contains(k, ".") || hasDottedKeys(t.vals[k]) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if hasDottedKeys(e) {
				return true
			}
		}
	}
	return false
}

// IndexDoc implements PUT/POST /{index}/_doc/{id} and /_create/{id}.
func (c *Cluster) IndexDoc(index, id string, raw []byte, dp DocParams) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dp.OpType == "create" && dp.RawOpType != "" && strings.ToLower(dp.RawOpType) != "create" {
		return fail(errIllegalArgument("opType must be 'create', found: [%s]", dp.RawOpType))
	}
	if len(raw) == 0 {
		return fail(&Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "request body is required"})
	}
	if err := dp.firstParamErr(); err != nil {
		return fail(err)
	}
	hasID := id != ""
	if !hasID && dp.RawOpType == "" {
		// POST /{index}/_doc defaults to op_type create
		dp.OpType = "create"
	}
	if err := dp.validateIndexRequest(id, hasID); err != nil {
		return fail(err)
	}
	tx := c.newDocTx()
	defer tx.commit()
	status, body, err := c.indexOne(tx, index, id, raw, dp, nil)
	if err != nil {
		return fail(err)
	}
	return Response{Status: status, Body: body}, nil
}

// indexOne runs a validated index request: ingest pipelines, require_alias,
// index auto-creation, active shards, routing, parsing and the engine write.
func (c *Cluster) indexOne(tx *docTx, index, id string, raw []byte, dp DocParams, wb *writeBatch) (int, M, error) {
	if err := c.pipelineFailure(index, dp.Pipeline); err != nil {
		return 0, nil, err
	}
	if dp.RequireAlias {
		if err := c.requireAliasFailure(index); err != nil {
			return 0, nil, err
		}
	}
	ix, err := c.docEnsureIndex(index)
	if err != nil {
		return 0, nil, err
	}
	if err := checkActiveShards(ix, dp.WaitForActive, dp.Timeout); err != nil {
		return 0, nil, err
	}
	autoID := id == ""
	shownID := id
	if autoID {
		shownID = "null"
		if dp.Routing == "" && routingRequired(ix) {
			return 0, nil, errIDMustNotBeNull()
		}
	}
	if err := requireRouting(ix, shownID, dp.Routing); err != nil {
		return 0, nil, err
	}
	if autoID {
		id = generateID()
	}
	doc, err := parseSourceDocument(raw)
	if err != nil {
		return 0, nil, err
	}
	if err := checkMetadataFields(doc, id); err != nil {
		return 0, nil, consumeSeqNoOnFailure(ix, err)
	}
	src, compact := sourceFromOrdered(raw, doc)
	d, created, err := ix.putDocFrom(tx, id, compact, raw, src, dp, autoID, wb.forIndex(ix))
	if err != nil {
		return 0, nil, err
	}
	if created {
		return http.StatusCreated, writeResult(ix, d, "created"), nil
	}
	return http.StatusOK, writeResult(ix, d, "updated"), nil
}

// getOptions are the parameters of a get (GET, HEAD, multi-get item).
type getOptions struct {
	source      sourceFilter
	fetchSource bool
	stored      []string
	version     int64
	versionType string
	routing     string
}

// fetchSourceParams is FetchSourceContext.parseFromRestRequest.
func fetchSourceParams(p Params) (sourceFilter, bool, error) {
	set := false
	sf := sourceFilter{}
	if p.Has("_source") {
		set = true
		switch v := p.Get("_source"); v {
		case "true":
		case "false":
			sf.disabled = true
		default:
			sf.includes = splitList(v)
		}
	}
	if p.Has("_source_includes") {
		set = true
		sf.includes = splitList(p.Get("_source_includes"))
	}
	if p.Has("_source_excludes") {
		set = true
		sf.excludes = splitList(p.Get("_source_excludes"))
	}
	if set {
		if err := sf.validate(); err != nil {
			return sf, true, err
		}
	}
	return sf, set, nil
}

// parseGetOptions reads the parameters of RestGetAction in order.
func parseGetOptions(p Params) (getOptions, error) {
	o := getOptions{version: versionMatchAny, versionType: "internal", routing: p.Get("routing"), fetchSource: true}
	if _, err := paramBool(p, "refresh", false); err != nil {
		return o, err
	}
	if _, err := paramBool(p, "realtime", true); err != nil {
		return o, err
	}
	if p.Has("fields") {
		return o, errIllegalArgument("the parameter [fields] is no longer supported, please use [stored_fields] to retrieve stored fields or [_source] to load the field from _source")
	}
	storedSet := p.Has("stored_fields")
	if storedSet {
		o.stored = splitList(p.Get("stored_fields"))
	}
	if v, has, err := paramLong(p, "version"); err != nil {
		return o, err
	} else if has {
		o.version = v
	}
	if p.Has("version_type") {
		vt, err := parseVersionType(p.Get("version_type"))
		if err != nil {
			return o, err
		}
		o.versionType = vt
	}
	sf, set, err := fetchSourceParams(p)
	if err != nil {
		return o, err
	}
	o.source = sf
	switch {
	case set:
		o.fetchSource = !sf.disabled
	case storedSet:
		o.fetchSource = false
		for _, f := range o.stored {
			if f == "_source" {
				o.fetchSource = true
			}
		}
	}
	return o, nil
}

// validateReadVersion is VersionType.validateVersionForReads.
func validateReadVersion(version int64, vt string) error {
	valid := version >= 0 || version == versionMatchAny
	if vt == "internal" {
		valid = version > 0 || version == versionMatchAny
	}
	if valid {
		return nil
	}
	return docValidation{"illegal version value [" + itoa(version) + "] for version type [" + strings.ToUpper(vt) + "]"}.err()
}

// checkReadVersion is VersionType.isVersionConflictForReads.
func checkReadVersion(ix *Index, d *Doc, version int64) error {
	if version == versionMatchAny || d.Version == version {
		return nil
	}
	return errVersionConflict(ix.Name, d.ID, "version conflict, current version ["+itoa(d.Version)+"] is different than the one provided ["+itoa(version)+"]")
}

func docJSON(ix *Index, d *Doc, o getOptions) M {
	out := M{"_index": ix.Name, "_id": d.ID, "_version": d.Version, "_seq_no": d.SeqNo, "_primary_term": d.PrimaryTerm, "found": true}
	// metadata stored fields (_routing) are only loaded together with the
	// source or requested stored fields
	if r := docRouting(d); r != "" && (o.fetchSource || len(o.stored) > 0) {
		out["_routing"] = r
	}
	// _ignored (fields whose malformed values were skipped) is a metadata
	// stored field loaded the same way
	if len(d.Ignored) > 0 && (o.fetchSource || len(o.stored) > 0) {
		out["_ignored"] = append([]string(nil), d.Ignored...)
	}
	if o.fetchSource {
		if src, ok := documentSource(ix, d, o.source); ok {
			out["_source"] = src
		}
	}
	if len(o.stored) > 0 {
		if fields := ix.storedFieldValues(d, o.stored); len(fields) > 0 {
			out["fields"] = fields
		}
	}
	return out
}

func documentSource(ix *Index, d *Doc, requestFilter sourceFilter) (any, bool) {
	indexFilter := mappingSourceFilter(ix.Mapping)
	if indexFilter.isPlain() && requestFilter.isPlain() {
		return json.RawMessage(d.Raw), true
	}
	src, ok := applySourceFilters(d.Src, indexFilter, requestFilter)
	return src, ok
}

// docIndexNotFound is the missing-index error of the document APIs, which
// resolve an index expression rather than an index or alias.
func docIndexNotFound(err error) error {
	if e, ok := err.(*Error); ok && e.Type == "index_not_found_exception" {
		e.Extra = M{"resource.type": "index_expression", "resource.id": e.Index}
	}
	return err
}

// lookupDoc resolves the document of a GET/HEAD request (d is nil when it
// does not exist). Called with c.mu held.
func (c *Cluster) lookupDoc(index, id string, p Params) (ix *Index, d *Doc, o getOptions, err error) {
	if o, err = parseGetOptions(p); err != nil {
		return nil, nil, o, err
	}
	if err := validateReadVersion(o.version, o.versionType); err != nil {
		return nil, nil, o, err
	}
	if ix, err = c.resolveDocIndex(index); err != nil {
		return nil, nil, o, err
	}
	if err := requireRouting(ix, id, o.routing); err != nil {
		return nil, nil, o, err
	}
	d = ix.docs[id]
	if d == nil || docShardMismatch(ix, id, o.routing, d) {
		return ix, nil, o, nil
	}
	if err := checkReadVersion(ix, d, o.version); err != nil {
		return nil, nil, o, err
	}
	return ix, d, o, nil
}

// GetDoc implements GET /{index}/_doc/{id}.
func (c *Cluster) GetDoc(index, id string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ix, d, o, err := c.lookupDoc(index, id, p)
	if err != nil {
		return fail(err)
	}
	if d == nil {
		return Response{Status: http.StatusNotFound, Body: M{"_index": ix.Name, "_id": id, "found": false}}, nil
	}
	return ok(docJSON(ix, d, o))
}

// DocExists implements HEAD /{index}/_doc/{id} and /_source/{id}: the
// status of the GET without rendering the document.
func (c *Cluster) DocExists(index, id string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, d, _, err := c.lookupDoc(index, id, p)
	if err != nil {
		return fail(err)
	}
	if d == nil {
		return Response{Status: http.StatusNotFound}, nil
	}
	return Response{Status: http.StatusOK}, nil
}

// GetSource implements GET /{index}/_source/{id}.
func (c *Cluster) GetSource(index, id string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, err := paramBool(p, "refresh", false); err != nil {
		return fail(err)
	}
	if _, err := paramBool(p, "realtime", true); err != nil {
		return fail(err)
	}
	sf, _, err := fetchSourceParams(p)
	if err != nil {
		return fail(err)
	}
	if sf.disabled {
		return fail(docValidation{"fetching source can not be disabled"}.err())
	}
	ix, err := c.resolveDocIndex(index)
	if err != nil {
		return fail(err)
	}
	routing := p.Get("routing")
	if err := requireRouting(ix, id, routing); err != nil {
		return fail(err)
	}
	d := ix.docs[id]
	if d == nil || docShardMismatch(ix, id, routing, d) {
		return fail(&Error{Status: http.StatusNotFound, Type: "resource_not_found_exception", Reason: "Document not found [" + ix.Name + "]/[" + id + "]"})
	}
	source, found := documentSource(ix, d, sf)
	if !found {
		return fail(&Error{Status: http.StatusNotFound, Type: "resource_not_found_exception", Reason: "Source not found [" + ix.Name + "]/[" + id + "]"})
	}
	return ok(source)
}

// DeleteDoc implements DELETE /{index}/_doc/{id}.
func (c *Cluster) DeleteDoc(index, id string, dp DocParams) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := dp.firstParamErr("require_alias", "op_type"); err != nil {
		return fail(err)
	}
	if err := dp.validateDeleteRequest(id); err != nil {
		return fail(err)
	}
	tx := c.newDocTx()
	defer tx.commit()
	status, body, err := c.deleteOne(tx, index, id, dp, nil)
	if err != nil {
		return fail(err)
	}
	return Response{Status: status, Body: body}, nil
}

func (c *Cluster) deleteOne(tx *docTx, index, id string, dp DocParams, wb *writeBatch) (int, M, error) {
	var ix *Index
	var err error
	if isExternalVersioning(dp.versionType()) {
		// deletes with external versioning auto-create the index
		ix, err = c.docEnsureIndex(index)
	} else {
		var target *Index
		if target, err = c.resolveWriteIndex(index); err != nil {
			return 0, nil, docIndexNotFound(err)
		}
		ix, err = c.docWritable(target.Name)
	}
	if err != nil {
		return 0, nil, err
	}
	if err := checkActiveShards(ix, dp.WaitForActive, dp.Timeout); err != nil {
		return 0, nil, err
	}
	if err := requireRouting(ix, id, dp.Routing); err != nil {
		return 0, nil, err
	}
	out, err := ix.deleteDoc(tx, id, dp, wb.forIndex(ix), true)
	if err != nil {
		return 0, nil, err
	}
	status, body := deleteResult(ix, id, out)
	return status, body, nil
}

// mgetItem is one document of a multi-get request.
type mgetItem struct {
	index       string
	id          string
	idSet       bool
	routing     string
	stored      []string
	storedSet   bool
	version     int64
	versionType string
	source      *sourceFilter
}

func jsonTokenKind(v any) string {
	switch v.(type) {
	case nil:
		return "VALUE_NULL"
	case string:
		return "VALUE_STRING"
	case json.Number, float64:
		return "VALUE_NUMBER"
	case bool:
		return "VALUE_BOOLEAN"
	case M:
		return "START_OBJECT"
	case []any:
		return "START_ARRAY"
	}
	return "VALUE_EMBEDDED_OBJECT"
}

// scalarText is XContentParser.text of a value token.
func scalarText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return string(t), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

// xLongValue is XContentParser.longValue of a value token.
func xLongValue(v any) (int64, error) {
	switch t := v.(type) {
	case json.Number:
		return bigLongValue(string(t))
	case string:
		return bigLongValue(t)
	}
	return 0, errIllegalArgument("Current token (%s) not numeric, cannot use numeric value accessors", jsonTokenKind(v))
}

func sortedMKeys(m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func parseMgetDocs(arr []any, index, routing string) ([]mgetItem, error) {
	var items []mgetItem
	for _, raw := range arr {
		m, isObj := raw.(M)
		if !isObj {
			return nil, errIllegalArgument("docs array element should include an object")
		}
		it := mgetItem{index: index, routing: routing, version: versionMatchAny, versionType: "internal"}
		for _, key := range sortedMKeys(m) {
			v := m[key]
			if text, isValue := scalarText(v); isValue {
				switch key {
				case "_index":
					it.index = text
				case "_id":
					it.id, it.idSet = text, true
				case "routing":
					it.routing = text
				case "fields":
					return nil, errParsing("Unsupported field [fields] used, expected [stored_fields] instead")
				case "stored_fields":
					it.stored, it.storedSet = []string{text}, true
				case "version":
					n, err := xLongValue(v)
					if err != nil {
						return nil, err
					}
					it.version = n
				case "version_type":
					vt, err := parseVersionType(text)
					if err != nil {
						return nil, err
					}
					it.versionType = vt
				case "_source":
					if b, isBool := v.(bool); isBool {
						sf := sourceFilter{disabled: !b}
						it.source = &sf
					} else if s, isString := v.(string); isString {
						sf := sourceFilter{includes: []string{s}}
						it.source = &sf
					} else {
						return nil, &Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "illegal type for _source: [" + jsonTokenKind(v) + "]"}
					}
				default:
					return nil, &Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "failed to parse multi get request. unknown field [" + key + "]"}
				}
				continue
			}
			switch t := v.(type) {
			case []any:
				var texts []string
				for _, e := range t {
					s, _ := scalarText(e)
					texts = append(texts, s)
				}
				switch key {
				case "fields":
					return nil, errParsing("Unsupported field [fields] used, expected [stored_fields] instead")
				case "stored_fields":
					it.stored, it.storedSet = texts, true
				case "_source":
					sf := sourceFilter{includes: texts}
					it.source = &sf
				default:
					if len(t) > 0 {
						return nil, &Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "failed to parse multi get request. unknown field [" + key + "]"}
					}
				}
			case M:
				if key != "_source" {
					continue
				}
				sf := sourceFilter{}
				for _, sk := range sortedMKeys(t) {
					var list *[]string
					switch sk {
					case "includes", "include":
						list = &sf.includes
					case "excludes", "exclude":
						list = &sf.excludes
					default:
						return nil, &Error{Status: http.StatusInternalServerError, Type: "illegal_state_exception", Reason: "Can't get text on a FIELD_NAME"}
					}
					switch sv := t[sk].(type) {
					case []any:
						for _, e := range sv {
							s, _ := scalarText(e)
							*list = append(*list, s)
						}
					default:
						if s, isValue := scalarText(sv); isValue {
							*list = append(*list, s)
						}
					}
				}
				it.source = &sf
			}
		}
		items = append(items, it)
	}
	return items, nil
}

// MultiGet implements GET /_mget and /{index}/_mget.
func (c *Cluster) MultiGet(index string, body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, err := paramBool(p, "refresh", false); err != nil {
		return fail(err)
	}
	if _, err := paramBool(p, "realtime", true); err != nil {
		return fail(err)
	}
	if p.Has("fields") {
		return fail(errIllegalArgument("The parameter [fields] is no longer supported, please use [stored_fields] to retrieve stored fields or _source filtering if the field is not stored"))
	}
	var defaultStored []string
	defaultStoredSet := p.Has("stored_fields")
	if defaultStoredSet {
		defaultStored = splitList(p.Get("stored_fields"))
	}
	defaultSource, defaultSourceSet, err := fetchSourceParams(p)
	if err != nil {
		return fail(err)
	}
	var items []mgetItem
	for _, key := range sortedMKeys(body) {
		arr, isArr := body[key].([]any)
		if !isArr {
			return fail(errParsing("unexpected token [%s], expected [FIELD_NAME] or [START_ARRAY]", jsonTokenKind(body[key])).at(valueTok(body, key)))
		}
		switch key {
		case "docs":
			docs, err := parseMgetDocs(arr, index, p.Get("routing"))
			if err != nil {
				return fail(err)
			}
			items = append(items, docs...)
		case "ids":
			for _, raw := range arr {
				text, isValue := scalarText(raw)
				if !isValue {
					return fail(errIllegalArgument("ids array element should only contain ids"))
				}
				items = append(items, mgetItem{index: index, id: text, idSet: true, routing: p.Get("routing"), version: versionMatchAny, versionType: "internal"})
			}
		default:
			return fail(errParsing("unknown key [%s] for a START_ARRAY, expected [docs] or [ids]", key).at(valueTok(body, key)))
		}
	}
	var v docValidation
	if len(items) == 0 {
		v.add("no documents to get")
	}
	for i, it := range items {
		if it.index == "" {
			v.add("index is missing for doc " + strconv.Itoa(i))
		}
		if !it.idSet {
			v.add("id is missing for doc " + strconv.Itoa(i))
		}
	}
	if err := v.err(); err != nil {
		return fail(err)
	}
	errorTrace, _ := paramBool(p, "error_trace", false)
	docs := make([]any, 0, len(items))
	failure := func(index, id string, err error) {
		e, isErr := err.(*Error)
		if !isErr {
			e = &Error{Status: http.StatusInternalServerError, Type: "exception", Reason: err.Error()}
		}
		docs = append(docs, M{"_index": index, "_id": id, "error": e.Body(errorTrace)["error"]})
	}
	for _, it := range items {
		ix, err := c.resolveDocIndex(it.index)
		if err != nil {
			failure(it.index, it.id, err)
			continue
		}
		if err := requireRouting(ix, it.id, it.routing); err != nil {
			failure(ix.Name, it.id, err)
			continue
		}
		d := ix.docs[it.id]
		if d == nil || docShardMismatch(ix, it.id, it.routing, d) {
			docs = append(docs, M{"_index": ix.Name, "_id": it.id, "found": false})
			continue
		}
		if err := checkReadVersion(ix, d, it.version); err != nil {
			failure(ix.Name, it.id, err)
			continue
		}
		o := getOptions{source: defaultSource, fetchSource: true, stored: defaultStored}
		storedSet := defaultStoredSet
		if it.storedSet {
			o.stored, storedSet = it.stored, true
		}
		sourceSet := defaultSourceSet
		if it.source != nil {
			o.source, sourceSet = *it.source, true
		}
		if err := o.source.validate(); err != nil {
			return fail(err)
		}
		switch {
		case sourceSet:
			o.fetchSource = !o.source.disabled
		case storedSet:
			o.fetchSource = false
			for _, f := range o.stored {
				if f == "_source" {
					o.fetchSource = true
				}
			}
		}
		docs = append(docs, docJSON(ix, d, o))
	}
	return ok(M{"docs": docs})
}
