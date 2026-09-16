package engine

import (
	"encoding/json"
	"net/http"
	"strings"
)

// updateRequest is a parsed update: UpdateRequest's body fields and URL
// parameters.
type updateRequest struct {
	doc             *orderedObject
	upsert          *orderedObject
	hasScript       bool
	scriptedUpsert  bool
	docAsUpsert     bool
	detectNoop      bool
	source          *sourceFilter
	ifSeqNo         int64
	ifPrimaryTerm   int64
	retryOnConflict int
	routing         string
	timeout         timeValue
	wait            activeShardCount
	requireAlias    bool
}

func newUpdateRequest() *updateRequest {
	return &updateRequest{detectNoop: true, ifSeqNo: unassignedSeqNo, ifPrimaryTerm: unassignedTerm, timeout: timeValue{text: "1m"}}
}

var updateRequestFields = []string{"script", "scripted_upsert", "upsert", "doc", "doc_as_upsert", "detect_noop", "_source", "if_seq_no", "if_primary_term"}

// bodyField is a top-level field of a request body with the locations of
// its name and value ("" when the body was already decoded).
type bodyField struct {
	name     string
	nameLoc  string
	kind     xKind
	value    any
	valueLoc string
	syntax   *xSyntaxError // a syntax error inside the value
	lastLoc  string        // location of the last token read before it
}

func kindOf(v any) xKind {
	switch t := v.(type) {
	case *orderedObject, M:
		return xStartObject
	case []any:
		return xStartArray
	case string:
		return xString
	case json.Number, float64:
		return xNumber
	case bool:
		if t {
			return xTrue
		}
		return xFalse
	}
	return xNull
}

// fieldsFromM lists the fields of a decoded body (in key order).
func fieldsFromM(body M) []bodyField {
	fields := make([]bodyField, 0, len(body))
	for _, k := range sortedMKeys(body) {
		v := orderedFromValue(body[k])
		fields = append(fields, bodyField{name: k, kind: kindOf(v), value: v})
	}
	return fields
}

func xParseError(reason string, cause *Error) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: reason, Cause: cause}
}

func locPrefix(loc string) string {
	if loc == "" {
		return ""
	}
	return loc + " "
}

// objectFields reads the fields of an object body with ObjectParser's
// checks: known names are tested before the value is read.
func objectFields(data []byte, name string, known []string) ([]bodyField, error) {
	s := newXScanner(data)
	t, err := s.next()
	if err != nil {
		return nil, err.jacksonError()
	}
	if t.kind != xStartObject {
		return nil, xParseError(s.location(t)+" ["+name+"] Expected START_OBJECT but was: "+t.kind.String(), nil)
	}
	var fields []bodyField
	for {
		ft, err := s.next()
		if err != nil {
			return nil, err.jacksonError()
		}
		if ft.kind == xEndObject {
			return fields, nil
		}
		f := bodyField{name: ft.text, nameLoc: s.location(ft)}
		isKnown := known == nil
		for _, k := range known {
			if k == ft.text {
				isKnown = true
			}
		}
		if !isKnown {
			return nil, xParseError(f.nameLoc+" ["+name+"] unknown field ["+ft.text+"]"+didYouMean(ft.text, known), nil)
		}
		vt, err := s.next()
		if err != nil {
			return nil, err.jacksonError()
		}
		f.kind, f.valueLoc = vt.kind, s.location(vt)
		v, verr := s.readXValue(vt)
		if verr != nil {
			f.syntax, f.lastLoc = verr, s.location(s.last)
			fields = append(fields, f)
			return fields, nil
		}
		f.value = v
		fields = append(fields, f)
	}
}

func unsupportedValue(object string, f bodyField) *Error {
	return xParseError(locPrefix(f.valueLoc)+"["+object+"] "+f.name+" doesn't support values of type: "+f.kind.String(), nil)
}

func failedField(object string, f bodyField, cause *Error) *Error {
	loc := f.valueLoc
	if f.lastLoc != "" {
		loc = f.lastLoc
	}
	return xParseError(locPrefix(loc)+"["+object+"] failed to parse field ["+f.name+"]", cause)
}

// boolField is ObjectParser's declareBoolean.
func boolField(object string, f bodyField) (bool, error) {
	switch f.kind {
	case xTrue:
		return true, nil
	case xFalse:
		return false, nil
	case xString:
		switch s := f.value.(string); s {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return false, failedField(object, f, errBooleanValue(s))
		}
	}
	return false, unsupportedValue(object, f)
}

// longField is ObjectParser's declareLong.
func longField(object string, f bodyField) (int64, error) {
	if f.kind != xNumber && f.kind != xString {
		return 0, unsupportedValue(object, f)
	}
	text, _ := scalarText(f.value)
	n, err := bigLongValue(text)
	if err != nil {
		return 0, failedField(object, f, err.(*Error))
	}
	return n, nil
}

// fetchSourceValue is FetchSourceContext.fromXContent on a parsed value.
func fetchSourceValue(v any) (sourceFilter, error) {
	sf := sourceFilter{}
	switch t := v.(type) {
	case bool:
		sf.disabled = !t
	case string:
		if strings.Contains(t, ",") {
			for _, part := range strings.Split(t, ",") {
				sf.includes = append(sf.includes, strings.TrimSpace(part))
			}
		} else {
			sf.includes = []string{t}
		}
	case []any:
		for _, e := range t {
			s, _ := scalarText(e)
			sf.includes = append(sf.includes, s)
		}
	case *orderedObject:
		for _, k := range t.keys {
			var list *[]string
			switch k {
			case "includes", "include":
				list = &sf.includes
			case "excludes", "exclude":
				list = &sf.excludes
			}
			switch val := t.vals[k].(type) {
			case []any:
				if list == nil {
					return sf, errParsing("Unknown key for a START_ARRAY in [%s].", k)
				}
				var items []string
				for _, e := range val {
					s, isString := e.(string)
					if !isString {
						return sf, errParsing("Unknown key for a %s in [%s].", kindOf(e).String(), k)
					}
					items = append(items, s)
				}
				*list = items
			case string:
				if list == nil {
					return sf, errParsing("Unknown key for a VALUE_STRING in [%s].", k)
				}
				*list = []string{val}
			default:
				return sf, errParsing("Unknown key for a %s in [%s].", kindOf(val).String(), k)
			}
		}
	default:
		return sf, errParsing("Expected one of [VALUE_BOOLEAN, START_OBJECT] but found [%s]", kindOf(v).String())
	}
	return sf, sf.validate()
}

// parseBody applies UpdateRequest.PARSER to the body fields.
func (req *updateRequest) parseBody(fields []bodyField) error {
	const object = "UpdateRequest"
	for _, f := range fields {
		switch f.name {
		case "doc", "upsert":
			if f.kind != xStartObject {
				return unsupportedValue(object, f)
			}
			if f.syntax != nil {
				return failedField(object, f, f.syntax.jacksonError())
			}
			if f.name == "doc" {
				req.doc = f.value.(*orderedObject)
			} else {
				req.upsert = f.value.(*orderedObject)
			}
		case "script":
			if f.kind != xStartObject && f.kind != xString {
				return unsupportedValue(object, f)
			}
			if f.syntax != nil {
				return failedField(object, f, f.syntax.jacksonError())
			}
			req.hasScript = true
		case "scripted_upsert", "doc_as_upsert", "detect_noop":
			b, err := boolField(object, f)
			if err != nil {
				return err
			}
			switch f.name {
			case "scripted_upsert":
				req.scriptedUpsert = b
			case "doc_as_upsert":
				req.docAsUpsert = b
			default:
				req.detectNoop = b
			}
		case "_source":
			switch f.kind {
			case xStartObject, xStartArray, xTrue, xFalse, xString:
			default:
				return unsupportedValue(object, f)
			}
			if f.syntax != nil {
				return failedField(object, f, f.syntax.jacksonError())
			}
			sf, err := fetchSourceValue(f.value)
			if err != nil {
				if e, isErr := err.(*Error); isErr {
					return failedField(object, f, e)
				}
				return err
			}
			req.source = &sf
		case "if_seq_no", "if_primary_term":
			n, err := longField(object, f)
			if err != nil {
				return err
			}
			if f.name == "if_seq_no" {
				if err := checkSeqNo(n); err != nil {
					return failedField(object, f, err.(*Error))
				}
				req.ifSeqNo = n
			} else {
				if err := checkPrimaryTerm(n); err != nil {
					return failedField(object, f, err.(*Error))
				}
				req.ifPrimaryTerm = n
			}
		default:
			return xParseError(locPrefix(f.nameLoc)+"["+object+"] unknown field ["+f.name+"]"+didYouMean(f.name, updateRequestFields), nil)
		}
	}
	return nil
}

// validate is UpdateRequest.validate.
func (req *updateRequest) validate(id string, v *docValidation) {
	if id == "" {
		v.add("id is missing")
	}
	if req.ifPrimaryTerm == unassignedTerm && req.ifSeqNo != unassignedSeqNo {
		v.add("ifSeqNo is set, but primary term is [0]")
	}
	if req.ifPrimaryTerm != unassignedTerm && req.ifSeqNo == unassignedSeqNo {
		v.add("ifSeqNo is unassigned, but primary term is [" + itoa(req.ifPrimaryTerm) + "]")
	}
	validateDocIDLength(id, v)
	if req.ifSeqNo != unassignedSeqNo {
		if req.retryOnConflict > 0 {
			v.add("compare and write operations can not be retried")
		}
		if req.docAsUpsert {
			v.add("compare and write operations can not be used with upsert")
		}
		if req.upsert != nil {
			v.add("upsert requests don't support `if_seq_no` and `if_primary_term`")
		}
	}
	if !req.hasScript && req.doc == nil {
		v.add("script or doc is missing")
	}
	if req.hasScript && req.doc != nil {
		v.add("can't provide both script and doc")
	}
	if req.doc == nil && req.docAsUpsert {
		v.add("doc must be specified if doc_as_upsert is enabled")
	}
}

// parseUpdateParams reads the parameters of RestUpdateAction in order.
func parseUpdateParams(p Params) (*updateRequest, error) {
	req := newUpdateRequest()
	req.routing = p.Get("routing")
	var err error
	if req.timeout, err = paramTime(p, "timeout", "1m"); err != nil {
		return nil, err
	}
	if err := checkRefreshParam(p); err != nil {
		return nil, err
	}
	if req.wait, err = paramActiveShards(p); err != nil {
		return nil, err
	}
	if req.docAsUpsert, err = paramBool(p, "doc_as_upsert", false); err != nil {
		return nil, err
	}
	sf, set, err := fetchSourceParams(p)
	if err != nil {
		return nil, err
	}
	if set {
		req.source = &sf
	}
	if n, has, err := paramInt(p, "retry_on_conflict"); err != nil {
		return nil, err
	} else if has {
		req.retryOnConflict = n
	}
	if p.Has("version") || p.Has("version_type") {
		return nil, docValidation{"internal versioning can not be used for optimistic concurrency control. Please use `if_seq_no` and `if_primary_term` instead"}.err()
	}
	if n, has, err := paramLong(p, "if_seq_no"); err != nil {
		return nil, err
	} else if has {
		if err := checkSeqNo(n); err != nil {
			return nil, err
		}
		req.ifSeqNo = n
	}
	if n, has, err := paramLong(p, "if_primary_term"); err != nil {
		return nil, err
	} else if has {
		if err := checkPrimaryTerm(n); err != nil {
			return nil, err
		}
		req.ifPrimaryTerm = n
	}
	if req.requireAlias, err = paramBool(p, "require_alias", false); err != nil {
		return nil, err
	}
	return req, nil
}

// UpdateDoc implements POST /{index}/_update/{id} for a decoded body.
func (c *Cluster) UpdateDoc(index, id string, body M, p Params) (Response, error) {
	req, err := parseUpdateParams(p)
	if err != nil {
		return fail(err)
	}
	if err := req.parseBody(fieldsFromM(body)); err != nil {
		return fail(err)
	}
	return c.runUpdate(index, id, req)
}

// UpdateDocRaw implements POST /{index}/_update/{id} from the raw body, so
// parse errors carry their locations.
func (c *Cluster) UpdateDocRaw(index, id string, body []byte, p Params) (Response, error) {
	req, err := parseUpdateParams(p)
	if err != nil {
		return fail(err)
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		fields, err := objectFields(body, "UpdateRequest", updateRequestFields)
		if err != nil {
			return fail(err)
		}
		if err := req.parseBody(fields); err != nil {
			return fail(err)
		}
	}
	return c.runUpdate(index, id, req)
}

func (c *Cluster) runUpdate(index, id string, req *updateRequest) (Response, error) {
	var v docValidation
	req.validate(id, &v)
	if err := v.err(); err != nil {
		return fail(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if req.requireAlias {
		if err := c.requireAliasFailure(index); err != nil {
			return fail(err)
		}
	}
	ix, err := c.docEnsureIndex(index)
	if err != nil {
		return fail(err)
	}
	if err := checkActiveShards(ix, req.wait, req.timeout); err != nil {
		return fail(err)
	}
	if err := requireRouting(ix, id, req.routing); err != nil {
		return fail(err)
	}
	tx := c.newDocTx()
	defer tx.commit()
	status, out, err := c.applyUpdate(tx, ix, id, req, false, nil)
	if err != nil {
		return fail(err)
	}
	return Response{Status: status, Body: out}, nil
}

// storedOrderedSource returns a document's stored source with its key
// order (after index-level source filtering, which OpenSearch applies when
// storing).
func storedOrderedSource(ix *Index, d *Doc) *orderedObject {
	doc, err := parseSourceDocument(d.Raw)
	if err != nil {
		doc = orderedFromValue(d.Src).(*orderedObject)
	}
	if f := mappingSourceFilter(ix.Mapping); !f.isPlain() && !f.disabled {
		if src, ok := applySourceFilters(valueFromOrdered(doc).(M), f); ok {
			doc = orderedFromValue(src).(*orderedObject)
		}
	}
	return doc
}

// applyUpdate is UpdateHelper.prepare and the resulting write. bulk
// selects the bulk flavor (noop shard counts, routing, pipelines checked
// by the caller).
func (c *Cluster) applyUpdate(tx *docTx, ix *Index, id string, req *updateRequest, bulk bool, wb *writeBatch) (int, M, error) {
	existing := ix.docs[id]
	if existing != nil && req.ifSeqNo != unassignedSeqNo && (existing.SeqNo != req.ifSeqNo || existing.PrimaryTerm != req.ifPrimaryTerm) {
		return 0, nil, errVersionConflict(ix.Name, id, casConflictReason(req.ifSeqNo, req.ifPrimaryTerm, existing.SeqNo, existing.PrimaryTerm))
	}
	if existing == nil {
		upsert := req.upsert
		if req.docAsUpsert {
			upsert = req.doc
		}
		if req.hasScript && req.scriptedUpsert {
			return 0, nil, errUnsupported("update with script")
		}
		if upsert == nil {
			return 0, nil, errDocumentMissing(ix.Name, id)
		}
		if !bulk {
			if err := c.pipelineFailure(ix.Name, nil); err != nil {
				return 0, nil, err
			}
		}
		doc := cloneOrdered(upsert).(*orderedObject)
		if err := checkMetadataFields(doc, id); err != nil {
			return 0, nil, err
		}
		src, raw := sourceFromOrdered(nil, doc)
		d, _, err := ix.putDoc(tx, id, raw, src, DocParams{OpType: "create", Routing: req.routing}, false, wb.forIndex(ix))
		if err != nil {
			return 0, nil, err
		}
		out := writeResult(ix, d, "created")
		addUpdateGet(out, d, req, doc)
		return http.StatusCreated, out, nil
	}
	if mappingSourceFilter(ix.Mapping).disabled {
		return 0, nil, &Error{Status: http.StatusBadRequest, Type: "document_source_missing_exception", Reason: "[" + id + "]: document source missing",
			Index: ix.Name, Extra: map[string]any{"shard": "0"}}
	}
	if req.hasScript {
		return 0, nil, errUnsupported("update with script")
	}
	merged := storedOrderedSource(ix, existing)
	changed := xcontentUpdate(merged, cloneOrdered(req.doc).(*orderedObject), req.detectNoop)
	if req.detectNoop && !changed {
		out := writeResult(ix, existing, "noop")
		if bulk {
			out["_shards"] = writeShards(ix)
		} else {
			out["_shards"] = M{"total": 0, "successful": 0, "failed": 0}
		}
		addUpdateGet(out, existing, req, merged)
		return http.StatusOK, out, nil
	}
	if !bulk {
		if err := c.pipelineFailure(ix.Name, nil); err != nil {
			return 0, nil, err
		}
	}
	if err := checkMetadataFields(merged, id); err != nil {
		return 0, nil, err
	}
	src, raw := sourceFromOrdered(nil, merged)
	routing := docRouting(existing)
	if !bulk && req.routing != "" {
		routing = req.routing
	}
	d, _, err := ix.putDoc(tx, id, raw, src, DocParams{OpType: "index", Routing: routing}, false, wb.forIndex(ix))
	if err != nil {
		return 0, nil, err
	}
	out := writeResult(ix, d, "updated")
	addUpdateGet(out, d, req, merged)
	return http.StatusOK, out, nil
}

// addUpdateGet adds the "get" section of an update response when the
// request asked for the source.
func addUpdateGet(out M, d *Doc, req *updateRequest, merged *orderedObject) {
	if req.source == nil || req.source.disabled {
		return
	}
	get := M{"_seq_no": d.SeqNo, "_primary_term": d.PrimaryTerm, "found": true}
	if req.source.isPlain() {
		get["_source"] = json.RawMessage(d.Raw)
	} else {
		get["_source"] = req.source.apply(valueFromOrdered(merged).(M))
	}
	out["get"] = get
}
