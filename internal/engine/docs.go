package engine

import (
	"bytes"
	"crypto/rand"

	"github.com/blevesearch/bleve/v2"

	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

// DocParams are the parameters of a single-document write.
type DocParams struct {
	OpType        string // "index" (default) or "create"
	RequireAlias  bool
	Routing       string
	IfSeqNo       *int64
	IfPrimaryTerm *int64
	Version       *int64
	VersionType   string
}

func docParamsFrom(p Params) (DocParams, error) {
	dp := DocParams{OpType: p.Get("op_type")}
	dp.RequireAlias = p.Bool("require_alias", false)
	dp.Routing = p.Get("routing")
	if dp.OpType == "" {
		dp.OpType = "index"
	}
	if err := validateDocParams(dp); err != nil {
		return dp, err
	}
	parse := func(name string) (*int64, error) {
		if !p.Has(name) {
			return nil, nil
		}
		n, err := strconv.ParseInt(p.Get(name), 10, 64)
		if err != nil {
			return nil, errIllegalArgument("Failed to parse int parameter [%s] with value [%s]", name, p.Get(name))
		}
		return &n, nil
	}
	var err error
	if dp.IfSeqNo, err = parse("if_seq_no"); err != nil {
		return dp, err
	}
	if dp.IfPrimaryTerm, err = parse("if_primary_term"); err != nil {
		return dp, err
	}
	if dp.Version, err = parse("version"); err != nil {
		return dp, err
	}
	dp.VersionType = p.Get("version_type")
	if err := validateDocParams(dp); err != nil {
		return dp, err
	}
	return dp, nil
}

func validateDocParams(dp DocParams) error {
	if dp.OpType != "" && dp.OpType != "index" && dp.OpType != "create" {
		return errIllegalArgument("op_type must be one of [index, create], found [%s]", dp.OpType)
	}
	switch dp.VersionType {
	case "", "internal", "external", "external_gt", "external_gte":
	default:
		return errIllegalArgument("version_type must be one of [internal, external, external_gt, external_gte], found [%s]", dp.VersionType)
	}
	if dp.Version != nil && (dp.VersionType == "external" || dp.VersionType == "external_gt" || dp.VersionType == "external_gte") && *dp.Version < 0 {
		return errIllegalArgument("version must be greater than or equal to 0 for external versioning")
	}
	if dp.Version == nil && (dp.VersionType == "external" || dp.VersionType == "external_gt" || dp.VersionType == "external_gte") {
		return errActionRequestValidation("version type [" + dp.VersionType + "] requires a version")
	}
	return nil
}

func generateID() string {
	var b [15]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// writeBatch groups bleve writes of one request per index so a bulk
// request commits once per index.
type writeBatch struct {
	batches map[*Index]*bleve.Batch
	order   []*Index
}

func newWriteBatch() *writeBatch { return &writeBatch{batches: map[*Index]*bleve.Batch{}} }

func (wb *writeBatch) forIndex(ix *Index) *bleve.Batch {
	if wb == nil {
		return nil
	}
	b, ok := wb.batches[ix]
	if !ok {
		b = ix.bleve.NewBatch()
		wb.batches[ix] = b
		wb.order = append(wb.order, ix)
	}
	return b
}

func (wb *writeBatch) flush() error {
	if wb == nil {
		return nil
	}
	for _, ix := range wb.order {
		b := wb.batches[ix]
		if b.Size() == 0 {
			continue
		}
		if err := ix.bleve.Batch(b); err != nil {
			return err
		}
	}
	wb.batches = map[*Index]*bleve.Batch{}
	wb.order = nil
	return nil
}

// putDoc stores a document in the index. When batch is non-nil the bleve
// write is queued on it instead of being committed immediately.
func (ix *Index) putDoc(id string, raw []byte, src M, dp DocParams, batch *bleve.Batch) (*Doc, bool, error) {
	if err := validateDocParams(dp); err != nil {
		return nil, false, err
	}
	if len(id) > 512 {
		return nil, false, errIllegalArgument("Document id length [%d] is greater than the maximum allowed length [512]", len(id))
	}
	existing := ix.docs[id]
	if dp.OpType == "create" && existing != nil {
		return nil, false, errVersionConflict(ix.Name, id, "version conflict, document already exists (current version ["+strconv.FormatInt(existing.Version, 10)+"])")
	}
	if dp.IfSeqNo != nil || dp.IfPrimaryTerm != nil {
		if dp.IfSeqNo == nil || dp.IfPrimaryTerm == nil {
			return nil, false, errActionRequestValidation("compare and write operations require both if_seq_no and if_primary_term")
		}
		if existing == nil {
			return nil, false, errVersionConflict(ix.Name, id, "version conflict, required seqNo ["+strconv.FormatInt(*dp.IfSeqNo, 10)+"], primary term ["+strconv.FormatInt(*dp.IfPrimaryTerm, 10)+"]. but no document was found")
		}
		if existing.SeqNo != *dp.IfSeqNo || existing.PrimaryTerm != *dp.IfPrimaryTerm {
			return nil, false, errVersionConflict(ix.Name, id, "version conflict, required seqNo ["+strconv.FormatInt(*dp.IfSeqNo, 10)+"], primary term ["+strconv.FormatInt(*dp.IfPrimaryTerm, 10)+"]. current document has seqNo ["+strconv.FormatInt(existing.SeqNo, 10)+"] and primary term ["+strconv.FormatInt(existing.PrimaryTerm, 10)+"]")
		}
	}
	version := int64(1)
	if existing != nil {
		version = existing.Version + 1
	}
	if dp.Version != nil {
		switch dp.VersionType {
		case "external", "external_gt", "external_gte":
			if existing != nil && (*dp.Version < existing.Version || ((dp.VersionType == "external" || dp.VersionType == "external_gt") && *dp.Version == existing.Version)) {
				return nil, false, errVersionConflict(ix.Name, id, "version conflict, current version ["+strconv.FormatInt(existing.Version, 10)+"] is higher or equal to the one provided ["+strconv.FormatInt(*dp.Version, 10)+"]")
			}
			version = *dp.Version
		default:
			if existing == nil || existing.Version != *dp.Version {
				cur := "-1"
				if existing != nil {
					cur = strconv.FormatInt(existing.Version, 10)
				}
				return nil, false, errVersionConflict(ix.Name, id, "version conflict, current version ["+cur+"] is different than the one provided ["+strconv.FormatInt(*dp.Version, 10)+"]")
			}
		}
	}
	ix.seqNo++
	d := &Doc{ID: id, Raw: raw, Src: src, Version: version, SeqNo: ix.seqNo, PrimaryTerm: 1}
	bds, err := ix.buildDocument(d, true)
	if err != nil {
		ix.seqNo--
		return nil, false, err
	}
	b := batch
	if b == nil {
		b = ix.bleve.NewBatch()
	}
	// nested objects of the previous version that no longer exist must go;
	// the ones that still exist are overwritten by the new documents
	for _, cid := range ix.children[id] {
		b.Delete(cid)
	}
	if err := ix.addDocuments(b, id, bds); err != nil {
		return nil, false, err
	}
	if batch == nil {
		if err := ix.bleve.Batch(b); err != nil {
			return nil, false, err
		}
	}
	ix.docs[id] = d
	return d, existing == nil, nil
}

func (ix *Index) validateWriteConditions(id string, dp DocParams) error {
	if dp.IfSeqNo == nil && dp.IfPrimaryTerm == nil {
		return nil
	}
	if dp.IfSeqNo == nil || dp.IfPrimaryTerm == nil {
		return errActionRequestValidation("compare and write operations require both if_seq_no and if_primary_term")
	}
	existing := ix.docs[id]
	if existing == nil {
		return errVersionConflict(ix.Name, id, "version conflict, required seqNo ["+strconv.FormatInt(*dp.IfSeqNo, 10)+"], primary term ["+strconv.FormatInt(*dp.IfPrimaryTerm, 10)+"]. but no document was found")
	}
	if existing.SeqNo != *dp.IfSeqNo || existing.PrimaryTerm != *dp.IfPrimaryTerm {
		return errVersionConflict(ix.Name, id, "version conflict, required seqNo ["+strconv.FormatInt(*dp.IfSeqNo, 10)+"], primary term ["+strconv.FormatInt(*dp.IfPrimaryTerm, 10)+"]. current document has seqNo ["+strconv.FormatInt(existing.SeqNo, 10)+"] and primary term ["+strconv.FormatInt(existing.PrimaryTerm, 10)+"]")
	}
	return nil
}

func (ix *Index) deleteDoc(id string, dp DocParams, batch *bleve.Batch) (*Doc, error) {
	if err := validateDocParams(dp); err != nil {
		return nil, err
	}
	if dp.IfSeqNo != nil || dp.IfPrimaryTerm != nil {
		if dp.IfSeqNo == nil || dp.IfPrimaryTerm == nil {
			return nil, errActionRequestValidation("compare and write operations require both if_seq_no and if_primary_term")
		}
	}
	existing := ix.docs[id]
	if existing == nil {
		return nil, nil
	}
	if dp.IfSeqNo != nil {
		if existing.SeqNo != *dp.IfSeqNo || existing.PrimaryTerm != *dp.IfPrimaryTerm {
			return nil, errVersionConflict(ix.Name, id, "version conflict, required seqNo ["+strconv.FormatInt(*dp.IfSeqNo, 10)+"], primary term ["+strconv.FormatInt(*dp.IfPrimaryTerm, 10)+"]. current document has seqNo ["+strconv.FormatInt(existing.SeqNo, 10)+"] and primary term ["+strconv.FormatInt(existing.PrimaryTerm, 10)+"]")
		}
	}
	b := batch
	if b == nil {
		b = ix.bleve.NewBatch()
	}
	b.Delete(id)
	for _, cid := range ix.children[id] {
		b.Delete(cid)
	}
	if batch == nil {
		if err := ix.bleve.Batch(b); err != nil {
			return nil, err
		}
	}
	delete(ix.children, id)
	delete(ix.docs, id)
	ix.seqNo++
	return existing, nil
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

// IndexDoc implements PUT/POST /{index}/_doc/{id} and /_create/{id}.
func (c *Cluster) IndexDoc(index, id string, raw []byte, dp DocParams) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dp.RequireAlias {
		if err := c.validateRequireAlias(index); err != nil {
			return fail(err)
		}
	}
	if id == "" {
		id = generateID()
	}
	src, compact, err := parseSource(raw)
	if err != nil {
		return fail(err)
	}
	ix, err := c.ensureIndex(index)
	if err != nil {
		return fail(err)
	}
	if err := validateRequiredRouting(ix, id, dp); err != nil {
		return fail(err)
	}
	d, created, err := ix.putDoc(id, compact, src, dp, nil)
	if err != nil {
		return fail(err)
	}
	if created {
		return Response{Status: http.StatusCreated, Body: writeResult(ix, d, "created")}, nil
	}
	return ok(writeResult(ix, d, "updated"))
}

func docJSON(ix *Index, d *Doc, sf sourceFilter, storedFields ...[]string) M {
	out := M{"_index": ix.Name, "_id": d.ID, "_version": d.Version, "_seq_no": d.SeqNo, "_primary_term": d.PrimaryTerm, "found": true}
	if src, ok := documentSource(ix, d, sf); ok {
		out["_source"] = src
	}
	if len(storedFields) > 0 {
		if fields := ix.storedFieldValues(d, storedFields[0]); len(fields) > 0 {
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

// GetDoc implements GET /{index}/_doc/{id}.
func (c *Cluster) GetDoc(index, id string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ix, err := c.resolveWriteIndex(index)
	if err != nil {
		return fail(err)
	}
	if err := validateRequiredRouting(ix, id, DocParams{Routing: p.Get("routing")}); err != nil {
		return fail(err)
	}
	d := ix.docs[id]
	if d == nil {
		return Response{Status: 404, Body: M{"_index": ix.Name, "_id": id, "found": false}}, nil
	}
	return ok(docJSON(ix, d, sourceFilterFromParams(p), splitList(p.Get("stored_fields"))))
}

// GetSource implements GET /{index}/_source/{id}.
func (c *Cluster) GetSource(index, id string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ix, err := c.resolveWriteIndex(index)
	if err != nil {
		return fail(err)
	}
	if err := validateRequiredRouting(ix, id, DocParams{Routing: p.Get("routing")}); err != nil {
		return fail(err)
	}
	d := ix.docs[id]
	if d == nil {
		return fail(&Error{Status: 404, Type: "resource_not_found_exception", Reason: "Document not found [" + ix.Name + "]/[_doc]/[" + id + "]"})
	}
	source, found := documentSource(ix, d, sourceFilterFromParams(p))
	if !found {
		return fail(&Error{Status: 404, Type: "resource_not_found_exception", Reason: "Source is disabled for document [" + ix.Name + "]/[_doc]/[" + id + "]"})
	}
	return ok(source)
}

// DocExists implements HEAD /{index}/_doc/{id}.
func (c *Cluster) DocExists(index, id string, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ix, err := c.resolveWriteIndex(index)
	if err != nil {
		return Response{Status: 404}, nil
	}
	if err := validateRequiredRouting(ix, id, DocParams{Routing: p.Get("routing")}); err != nil {
		return fail(err)
	}
	if ix.docs[id] == nil {
		return Response{Status: 404}, nil
	}
	return Response{Status: 200}, nil
}

// DeleteDoc implements DELETE /{index}/_doc/{id}.
func (c *Cluster) DeleteDoc(index, id string, dp DocParams) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dp.RequireAlias {
		if err := c.validateRequireAlias(index); err != nil {
			return fail(err)
		}
	}
	target, err := c.resolveWriteIndex(index)
	if err != nil {
		return fail(err)
	}
	ix, err := c.writable(target.Name)
	if err != nil {
		return fail(err)
	}
	if err := validateRequiredRouting(ix, id, dp); err != nil {
		return fail(err)
	}
	d, err := ix.deleteDoc(id, dp, nil)
	if err != nil {
		return fail(err)
	}
	if d == nil {
		return Response{Status: 404, Body: M{
			"_index": ix.Name, "_id": id, "_version": 1, "result": "not_found",
			"_shards": writeShards(ix), "_seq_no": ix.seqNo, "_primary_term": 1,
		}}, nil
	}
	res := writeResult(ix, d, "deleted")
	res["_version"] = d.Version + 1
	res["_seq_no"] = ix.seqNo
	return ok(res)
}

// UpdateDoc implements POST /{index}/_update/{id}.
func (c *Cluster) UpdateDoc(index, id string, body M, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dp, err := docParamsFrom(p)
	if err != nil {
		return fail(err)
	}
	if dp.RequireAlias {
		if err := c.validateRequireAlias(index); err != nil {
			return fail(err)
		}
	}
	target, err := c.resolveWriteIndex(index)
	if err != nil {
		if _, isNF := err.(*Error); isNF && strings.Contains(err.Error(), "index_not_found") {
			return fail(err)
		}
		return fail(err)
	}
	ix, err := c.writable(target.Name)
	if err != nil {
		return fail(err)
	}
	if err := validateRequiredRouting(ix, id, dp); err != nil {
		return fail(err)
	}
	res, err := ix.update(id, body, dp, p, nil)
	if err != nil {
		return fail(err)
	}
	return res, nil
}

func (ix *Index) update(id string, body M, dp DocParams, p Params, batch *bleve.Batch) (Response, error) {
	existing := ix.docs[id]
	if _, hasScript := body["script"]; hasScript {
		return Response{}, errUnsupported("update with script")
	}
	docPart, hasDoc := body["doc"].(M)
	upsert, hasUpsert := body["upsert"].(M)
	docAsUpsert := getBool(body, "doc_as_upsert", false)
	detectNoop := getBool(body, "detect_noop", true)
	if !hasDoc && !hasUpsert {
		return Response{}, errActionRequestValidation("script or doc is missing")
	}
	var newSrc M
	if existing == nil {
		switch {
		case docAsUpsert && hasDoc:
			newSrc = expandDots(cloneDeep(docPart).(M))
		case hasUpsert:
			newSrc = expandDots(cloneDeep(upsert).(M))
		default:
			return Response{}, errDocumentMissing(ix.Name, id)
		}
	} else {
		newSrc = cloneDeep(existing.Src).(M)
		if hasDoc {
			deepMergeSource(newSrc, expandDots(cloneDeep(docPart).(M)))
		}
		if detectNoop && reflect.DeepEqual(newSrc, existing.Src) {
			if err := ix.validateWriteConditions(id, dp); err != nil {
				return Response{}, err
			}
			out := writeResult(ix, existing, "noop")
			out["_shards"] = M{"total": 0, "successful": 0, "failed": 0}
			addUpdateSource(out, ix, existing, body, p)
			return Response{Status: 200, Body: out}, nil
		}
	}
	raw, err := json.Marshal(newSrc)
	if err != nil {
		return Response{}, errMapperParsing("%s", err.Error())
	}
	if existing != nil {
		raw = mergeRaw(existing.Raw, newSrc)
	}
	d, created, err := ix.putDoc(id, raw, newSrc, dp, batch)
	if err != nil {
		return Response{}, err
	}
	result := "updated"
	if created {
		result = "created"
	}
	out := writeResult(ix, d, result)
	addUpdateSource(out, ix, d, body, p)
	status := 200
	if created {
		status = 201
	}
	return Response{Status: status, Body: out}, nil
}

// mergeRaw re-serializes a merged source, keeping the key order of the
// original document where possible.
func mergeRaw(orig []byte, merged M) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(merged)
	out := bytes.TrimRight(buf.Bytes(), "\n")
	var compact bytes.Buffer
	if err := json.Compact(&compact, out); err != nil {
		return out
	}
	return compact.Bytes()
}

func addUpdateSource(out M, ix *Index, d *Doc, body M, p Params) {
	var sf sourceFilter
	has := false
	if v, ok := body["_source"]; ok {
		sf = parseSourceParam(v)
		has = true
	} else if p.Has("_source") || p.Has("_source_includes") || p.Has("_source_excludes") {
		sf = sourceFilterFromParams(p)
		has = true
	}
	if !has || sf.disabled {
		return
	}
	get := M{"_seq_no": d.SeqNo, "_primary_term": d.PrimaryTerm, "found": true}
	if src, ok := documentSource(ix, d, sf); ok {
		get["_source"] = src
	}
	out["get"] = get
}

// deepMergeSource merges a partial document into a source: objects merge
// recursively, everything else is replaced.
func deepMergeSource(dst, src M) {
	for k, v := range src {
		if sv, ok := v.(M); ok {
			if dv, ok := dst[k].(M); ok {
				deepMergeSource(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
}

// MultiGet implements GET /_mget and /{index}/_mget.
func (c *Cluster) MultiGet(index string, body M, p Params) (Response, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var docs []any
	baseFilter := sourceFilterFromParams(p)
	add := func(idxName, id, routing string, sf sourceFilter, stored []string) error {
		if idxName == "" {
			return errActionRequestValidation("index is missing")
		}
		ix, err := c.resolveWriteIndex(idxName)
		if err != nil {
			if e, ok := err.(*Error); ok && e.Type == "index_not_found_exception" {
				docs = append(docs, M{"_index": idxName, "_id": id, "error": M{"root_cause": []any{M{"type": e.Type, "reason": e.Reason}}, "type": e.Type, "reason": e.Reason, "index": idxName, "index_uuid": "_na_"}})
				return nil
			}
			return err
		}
		if err := validateRequiredRouting(ix, id, DocParams{Routing: routing}); err != nil {
			return err
		}
		d := ix.docs[id]
		if d == nil {
			docs = append(docs, M{"_index": ix.Name, "_id": id, "found": false})
			return nil
		}
		if len(stored) == 0 {
			stored = splitList(p.Get("stored_fields"))
		}
		docs = append(docs, docJSON(ix, d, sf, stored))
		return nil
	}
	if list, ok := body["docs"].([]any); ok {
		for _, raw := range list {
			m, _ := raw.(M)
			idxName := getString(m, "_index")
			if idxName == "" {
				idxName = index
			}
			sf := baseFilter
			if v, ok := m["_source"]; ok {
				sf = parseSourceParam(v)
			}
			routing := getString(m, "routing")
			if routing == "" {
				routing = p.Get("routing")
			}
			if err := add(idxName, getString(m, "_id"), routing, sf, getStrings(m, "stored_fields")); err != nil {
				return fail(err)
			}
		}
	}
	if ids, ok := body["ids"].([]any); ok {
		for _, raw := range ids {
			id, _ := raw.(string)
			if err := add(index, id, p.Get("routing"), baseFilter, nil); err != nil {
				return fail(err)
			}
		}
	}
	if docs == nil {
		docs = []any{}
	}
	return ok(M{"docs": docs})
}

// Bulk implements POST /_bulk and /{index}/_bulk.
func (c *Cluster) Bulk(index string, data []byte, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(data) > 0 && data[len(data)-1] != '\n' {
		return fail(errIllegalArgument("The bulk request must be terminated by a newline [\\n]"))
	}
	lines := bytes.Split(data, []byte("\n"))
	var items []any
	errors := false
	i := 0
	nextLine := func() ([]byte, bool) {
		for i < len(lines) {
			l := bytes.TrimSpace(lines[i])
			i++
			if len(l) > 0 {
				return l, true
			}
		}
		return nil, false
	}
	// validate every action line first: OpenSearch rejects the whole
	// request when an action has no index
	if err := validateBulkActions(lines, index); err != nil {
		return fail(err)
	}
	wb := newWriteBatch()
	for {
		actionLine, ok := nextLine()
		if !ok {
			break
		}
		var action M
		if err := decodeJSON(actionLine, &action); err != nil || len(action) != 1 {
			return fail(errIllegalArgument("Malformed action/metadata line [%d], expected START_OBJECT or END_OBJECT but found [VALUE_STRING]", i))
		}
		var kind string
		var meta M
		for k, v := range action {
			kind = k
			meta, _ = v.(M)
		}
		if meta == nil {
			meta = M{}
		}
		idxName := getString(meta, "_index")
		if idxName == "" {
			idxName = index
		}
		id := getString(meta, "_id")
		var source []byte
		if kind != "delete" {
			src, ok := nextLine()
			if !ok {
				return fail(errIllegalArgument("Validation Failed: 1: no requests added;"))
			}
			source = src
		}
		if idxName == "" {
			items = append(items, M{kind: M{"_index": "", "_id": id, "status": 400, "error": M{"type": "action_request_validation_exception", "reason": "Validation Failed: 1: index is missing;"}}})
			errors = true
			continue
		}
		dp := DocParams{OpType: "index"}
		dp.VersionType = getString(meta, "version_type")
		dp.RequireAlias = p.Bool("require_alias", false) || getBool(meta, "_require_alias", false)
		dp.Routing = p.Get("routing")
		if routing := getString(meta, "routing"); routing != "" {
			dp.Routing = routing
		}
		if routing := getString(meta, "_routing"); routing != "" {
			dp.Routing = routing
		}
		if kind == "create" {
			dp.OpType = "create"
		}
		if v, ok := toFloat(meta["if_seq_no"]); ok {
			n := int64(v)
			dp.IfSeqNo = &n
		}
		if v, ok := toFloat(meta["if_primary_term"]); ok {
			n := int64(v)
			dp.IfPrimaryTerm = &n
		}
		if v, ok := toFloat(meta["version"]); ok {
			n := int64(v)
			dp.Version = &n
		}
		item, itemErr := c.bulkItem(kind, idxName, id, source, dp, meta, wb)
		if itemErr != nil {
			errors = true
			e, ok := itemErr.(*Error)
			if !ok {
				e = &Error{Status: 500, Type: "exception", Reason: itemErr.Error()}
			}
			errBody := M{"type": e.Type, "reason": e.Reason}
			if e.Index != "" {
				errBody["index"] = e.Index
				errBody["index_uuid"] = "_na_"
			}
			if e.Type == "mapper_parsing_exception" {
				errBody["caused_by"] = M{"type": "illegal_argument_exception", "reason": e.Reason}
			}
			item = M{"_index": idxName, "_id": id, "status": e.Status, "error": errBody}
		}
		items = append(items, M{kind: item})
	}
	if items == nil {
		return fail(errActionRequestValidation("no requests added"))
	}
	if err := wb.flush(); err != nil {
		return fail(err)
	}
	return ok(M{"took": 1, "errors": errors, "items": items})
}

func validateBulkActions(lines [][]byte, index string) error {
	i := 0
	n := 0
	var problems []string
	for i < len(lines) {
		l := bytes.TrimSpace(lines[i])
		i++
		if len(l) == 0 {
			continue
		}
		var action M
		if err := decodeJSON(l, &action); err != nil || len(action) != 1 {
			return errIllegalArgument("Malformed action/metadata line [%d], expected START_OBJECT or END_OBJECT but found [VALUE_STRING]", i)
		}
		var kind string
		var meta M
		for k, v := range action {
			kind = k
			meta, _ = v.(M)
		}
		if getString(meta, "_index") == "" && index == "" {
			n++
			problems = append(problems, strconv.Itoa(n)+": index is missing;")
		}
		if kind != "delete" {
			// skip the source line
			for i < len(lines) && len(bytes.TrimSpace(lines[i])) == 0 {
				i++
			}
			i++
		}
	}
	if len(problems) > 0 {
		return &Error{Status: http.StatusBadRequest, Type: "action_request_validation_exception", Reason: "Validation Failed: " + strings.Join(problems, "")}
	}
	return nil
}

func (c *Cluster) bulkItem(kind, idxName, id string, source []byte, dp DocParams, meta M, wb *writeBatch) (M, error) {
	if dp.RequireAlias {
		if err := c.validateRequireAlias(idxName); err != nil {
			return nil, err
		}
	}
	switch kind {
	case "index", "create":
		if id == "" {
			id = generateID()
		}
		src, compact, err := parseSource(source)
		if err != nil {
			return nil, err
		}
		ix, err := c.ensureIndex(idxName)
		if err != nil {
			return nil, err
		}
		if err := validateRequiredRouting(ix, id, dp); err != nil {
			return nil, err
		}
		d, created, err := ix.putDoc(id, compact, src, dp, wb.forIndex(ix))
		if err != nil {
			return nil, err
		}
		res := writeResult(ix, d, "updated")
		res["status"] = 200
		if created {
			res["result"] = "created"
			res["status"] = 201
		}
		return res, nil
	case "update":
		if id == "" {
			return nil, errActionRequestValidation("id is missing")
		}
		var body M
		if err := decodeJSON(source, &body); err != nil {
			return nil, errParsing("%s", err.Error())
		}
		ix, err := c.ensureIndex(idxName)
		if err != nil {
			return nil, err
		}
		if err := validateRequiredRouting(ix, id, dp); err != nil {
			return nil, err
		}
		if _, ok := body["retry_on_conflict"]; ok {
			delete(body, "retry_on_conflict")
		}
		if v, ok := toFloat(meta["retry_on_conflict"]); ok {
			_ = v
		}
		res, err := ix.update(id, body, dp, Params{}, wb.forIndex(ix))
		if err != nil {
			return nil, err
		}
		out := res.Body.(M)
		out["status"] = res.Status
		return out, nil
	case "delete":
		if id == "" {
			return nil, errActionRequestValidation("id is missing")
		}
		target, err := c.resolveWriteIndex(idxName)
		if err != nil {
			return nil, err
		}
		ix, err := c.writable(target.Name)
		if err != nil {
			return nil, err
		}
		if err := validateRequiredRouting(ix, id, dp); err != nil {
			return nil, err
		}
		d, err := ix.deleteDoc(id, dp, wb.forIndex(ix))
		if err != nil {
			return nil, err
		}
		if d == nil {
			return M{"_index": ix.Name, "_id": id, "_version": 1, "result": "not_found", "_shards": writeShards(ix), "_seq_no": ix.seqNo, "_primary_term": 1, "status": 404}, nil
		}
		res := writeResult(ix, d, "deleted")
		res["_version"] = d.Version + 1
		res["_seq_no"] = ix.seqNo
		res["status"] = 200
		return res, nil
	}
	return nil, errIllegalArgument("Malformed action/metadata line, expected one of [create, delete, index, update] but found [%s]", kind)
}

// DeleteByQuery implements POST /{index}/_delete_by_query.
func (c *Cluster) DeleteByQuery(expr string, body M, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	matches, err := c.matchDocs(ts, body, p)
	if err != nil {
		return fail(err)
	}
	deleted := 0
	wb := newWriteBatch()
	for _, m := range matches {
		ix, err := c.writable(m.ix.Name)
		if err != nil {
			return fail(err)
		}
		if d, _ := ix.deleteDoc(m.doc.ID, DocParams{}, wb.forIndex(ix)); d != nil {
			deleted++
		}
	}
	if err := wb.flush(); err != nil {
		return fail(err)
	}
	return ok(byQueryResult(len(matches), deleted, 0, 0))
}

func byQueryResult(total, deleted, updated, created int) M {
	out := M{
		"took": 1, "timed_out": false, "total": total, "deleted": deleted, "batches": 1, "version_conflicts": 0, "noops": 0,
		"retries": M{"bulk": 0, "search": 0}, "throttled_millis": 0, "requests_per_second": -1.0, "throttled_until_millis": 0, "failures": []any{},
	}
	if updated > 0 || created > 0 || deleted == 0 {
		out["updated"] = updated
	}
	if created > 0 {
		out["created"] = created
	}
	return out
}

// UpdateByQuery implements POST /{index}/_update_by_query (without scripts
// it re-indexes matching documents).
func (c *Cluster) UpdateByQuery(expr string, body M, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := body["script"]; ok {
		return fail(errUnsupported("update_by_query with script"))
	}
	ts, err := c.resolve(expr, resolveOpts(p))
	if err != nil {
		return fail(err)
	}
	matches, err := c.matchDocs(ts, body, p)
	if err != nil {
		return fail(err)
	}
	updated := 0
	wb := newWriteBatch()
	for _, m := range matches {
		ix, err := c.writable(m.ix.Name)
		if err != nil {
			return fail(err)
		}
		if _, _, err := ix.putDoc(m.doc.ID, m.doc.Raw, m.doc.Src, DocParams{OpType: "index"}, wb.forIndex(ix)); err == nil {
			updated++
		}
	}
	if err := wb.flush(); err != nil {
		return fail(err)
	}
	res := byQueryResult(len(matches), 0, updated, 0)
	delete(res, "deleted")
	res["deleted"] = 0
	return ok(res)
}

// Reindex implements POST /_reindex.
func (c *Cluster) Reindex(body M, p Params) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	src := getMap(body, "source")
	dest := getMap(body, "dest")
	if src == nil || dest == nil {
		return fail(errActionRequestValidation("source and dest are required"))
	}
	if _, ok := body["script"]; ok {
		return fail(errUnsupported("reindex with script"))
	}
	var maxDocs int64
	hasMaxDocs := false
	if raw, ok := body["max_docs"]; ok {
		var n int64
		switch v := raw.(type) {
		case json.Number:
			var err error
			n, err = v.Int64()
			if err != nil {
				return fail(errIllegalArgument("[max_docs] must be an integer"))
			}
		case int:
			n = int64(v)
		case int64:
			n = v
		default:
			return fail(errIllegalArgument("[max_docs] must be an integer"))
		}
		if n < 0 {
			return fail(errIllegalArgument("max_docs must be greater than or equal to 0"))
		}
		maxDocs, hasMaxDocs = n, true
	}
	versionType, versionTypeIsString := dest["version_type"].(string)
	if raw, exists := dest["version_type"]; exists {
		if !versionTypeIsString || (versionType != "internal" && versionType != "external" && versionType != "external_gt" && versionType != "external_gte") {
			return fail(errIllegalArgument("[dest.version_type] must be one of [internal, external, external_gt, external_gte], found [%v]", raw))
		}
	}
	opType, opTypeIsString := dest["op_type"].(string)
	if raw, exists := dest["op_type"]; exists {
		if !opTypeIsString || (opType != "index" && opType != "create") {
			return fail(errIllegalArgument("[dest.op_type] must be one of [index, create], found [%v]", raw))
		}
	}
	srcExpr := strings.Join(getStrings(src, "index"), ",")
	ts, err := c.resolve(srcExpr, resolveOptions{allowAliases: true, allowNoIndices: true})
	if err != nil {
		return fail(err)
	}
	for _, sourceIndex := range ts {
		if mappingSourceFilter(sourceIndex.ix.Mapping).disabled {
			return fail(errIllegalArgument("reindex from an index without _source is not supported"))
		}
	}
	q := M{}
	if qq, ok := src["query"]; ok {
		q["query"] = qq
	}
	if size, ok := src["size"]; ok {
		q["size"] = size
	}
	if s, ok := src["_source"]; ok {
		q["_source"] = s
	}
	matches, err := c.matchDocs(ts, q, p)
	if err != nil {
		return fail(err)
	}
	if hasMaxDocs && maxDocs < int64(len(matches)) {
		matches = matches[:int(maxDocs)]
	}
	destName := getString(dest, "index")
	if destName == "" {
		return fail(errActionRequestValidation("dest index is missing"))
	}
	ix, err := c.ensureIndex(destName)
	if err != nil {
		return fail(err)
	}
	dp := DocParams{OpType: "index", VersionType: versionType}
	if opType == "create" {
		dp.OpType = opType
	}
	sf := parseSourceParam(src["_source"])
	created, updated, conflicts := 0, 0, 0
	wb := newWriteBatch()
	for _, m := range matches {
		itemDP := dp
		if itemDP.VersionType == "external" || itemDP.VersionType == "external_gt" || itemDP.VersionType == "external_gte" {
			version := m.doc.Version
			itemDP.Version = &version
		}
		raw, s := m.doc.Raw, m.doc.Src
		indexFilter := mappingSourceFilter(m.ix.Mapping)
		if !indexFilter.isPlain() || !sf.isPlain() {
			var sourceOK bool
			s, sourceOK = applySourceFilters(m.doc.Src, indexFilter, sf)
			if !sourceOK {
				s = M{}
			}
			raw, _ = json.Marshal(s)
		}
		_, wasCreated, err := ix.putDoc(m.doc.ID, raw, s, itemDP, wb.forIndex(ix))
		if err != nil {
			if e, ok := err.(*Error); ok && e.Status == 409 {
				conflicts++
				if getString(body, "conflicts") == "proceed" {
					continue
				}
				return fail(err)
			}
			return fail(err)
		}
		if wasCreated {
			created++
		} else {
			updated++
		}
	}
	if err := wb.flush(); err != nil {
		return fail(err)
	}
	res := byQueryResult(len(matches), 0, updated, created)
	res["created"] = created
	res["updated"] = updated
	res["version_conflicts"] = conflicts
	delete(res, "deleted")
	return ok(res)
}
