package engine

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
)

// bulkRequest is one parsed item of a bulk request.
type bulkRequest struct {
	action        string // index, create, update, delete
	index         string
	id            string
	idSet         bool
	routing       string
	opType        string
	version       int64
	versionSet    bool
	versionType   string
	ifSeqNo       int64
	ifPrimaryTerm int64
	retry         int
	pipeline      *string
	requireAlias  bool
	source        []byte
	update        *updateRequest

	done     bool
	response M
}

func (r *bulkRequest) docParams() DocParams {
	dp := DocParams{OpType: "index", VersionType: r.versionType, Routing: r.routing, Pipeline: r.pipeline, RequireAlias: r.requireAlias}
	if r.action == "create" || (r.action == "index" && r.opType == "create") {
		dp.OpType = "create"
	}
	if r.action == "delete" {
		dp.OpType = "delete"
	}
	if r.versionSet {
		v := r.version
		dp.Version = &v
	}
	if r.ifSeqNo != unassignedSeqNo {
		v := r.ifSeqNo
		dp.IfSeqNo = &v
	}
	if r.ifPrimaryTerm != unassignedTerm {
		v := r.ifPrimaryTerm
		dp.IfPrimaryTerm = &v
	}
	return dp
}

// itemKey is the key of the item in the response.
func (r *bulkRequest) itemKey() string {
	if r.action == "index" && r.opType == "create" {
		return "create"
	}
	return r.action
}

func (r *bulkRequest) idValue() any {
	if !r.idSet {
		return nil
	}
	return r.id
}

func malformedLine(line int, format string, args ...any) error {
	return errIllegalArgument("Malformed action/metadata line [%d], %s", line, fmt.Sprintf(format, args...))
}

func bulkInputCoercion(t xToken) *Error {
	name := "VALUE_TRUE"
	if t.kind == xFalse {
		name = "VALUE_FALSE"
	}
	reason := fmt.Sprintf("Current token (%s) not numeric, cannot use numeric value accessors\n at [Source: REDACTED (`StreamReadFeature.INCLUDE_SOURCE_IN_LOCATION` disabled); byte offset: #%d]", name, t.end)
	return &Error{Status: http.StatusBadRequest, Type: "input_coercion_exception", Reason: reason,
		Cause: &Error{Type: "input_coercion_exception", Reason: reason}}
}

// tokenLong is XContentParser.longValue.
func tokenLong(t xToken) (int64, error) {
	switch t.kind {
	case xTrue, xFalse:
		return 0, bulkInputCoercion(t)
	}
	return bigLongValue(t.text)
}

// tokenInt is XContentParser.intValue.
func tokenInt(t xToken) (int, error) {
	switch t.kind {
	case xTrue, xFalse:
		return 0, bulkInputCoercion(t)
	case xString:
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return 0, &Error{Status: http.StatusBadRequest, Type: "number_format_exception", Reason: "For input string: \"" + t.text + "\""}
		}
		return int(f), nil
	}
	f, err := strconv.ParseFloat(t.text, 64)
	if err != nil {
		return 0, errIllegalArgument("For input string: \"%s\"", t.text)
	}
	return int(f), nil
}

func nextLine(data []byte, from int) (int, error) {
	i := bytes.IndexByte(data[from:], '\n')
	if i < 0 {
		if from != len(data) {
			return -1, errIllegalArgument("The bulk request must be terminated by a newline [\\n]")
		}
		return -1, nil
	}
	return from + i, nil
}

// parseBulk is BulkRequestParser.parse.
func parseBulk(data []byte, index, routing string, pipeline *string, requireAlias *bool, source *sourceFilter) ([]*bulkRequest, error) {
	var reqs []*bulkRequest
	line, from := 0, 0
	for {
		end, err := nextLine(data, from)
		if err != nil {
			return nil, err
		}
		if end < 0 {
			break
		}
		line++
		s := newXScanner(data[from:end])
		from = end + 1
		t, serr := s.next()
		if serr != nil {
			return nil, serr.jacksonError()
		}
		if t.kind == xEOF {
			continue
		}
		if t.kind != xStartObject {
			return nil, malformedLine(line, "expected START_OBJECT but found [%s]", t.kind)
		}
		if t, serr = s.next(); serr != nil {
			return nil, serr.jacksonError()
		}
		if t.kind != xFieldName {
			return nil, malformedLine(line, "expected FIELD_NAME but found [%s]", t.kind)
		}
		r := &bulkRequest{action: t.text, index: index, routing: routing, version: versionMatchAny, versionType: "internal",
			ifSeqNo: unassignedSeqNo, ifPrimaryTerm: unassignedTerm, pipeline: pipeline}
		switch r.action {
		case "index", "create", "update", "delete":
		default:
			return nil, malformedLine(line, "expected one of [create, delete, index, update] but found [%s]", r.action)
		}
		if requireAlias != nil {
			r.requireAlias = *requireAlias
		}
		fetch := source
		if t, serr = s.next(); serr != nil {
			return nil, serr.jacksonError()
		}
		switch t.kind {
		case xStartObject:
			field := ""
			for {
				mt, serr := s.next()
				if serr != nil {
					return nil, serr.jacksonError()
				}
				if mt.kind == xEndObject || mt.kind == xEOF {
					break
				}
				if mt.kind == xFieldName {
					field = mt.text
					continue
				}
				switch {
				case mt.kind.isValue():
					switch field {
					case "_index":
						r.index = mt.text
					case "_id":
						r.id, r.idSet = mt.text, true
					case "routing":
						r.routing = mt.text
					case "op_type":
						r.opType = mt.text
					case "version":
						n, err := tokenLong(mt)
						if err != nil {
							return nil, err
						}
						r.version, r.versionSet = n, true
					case "version_type":
						vt, err := parseVersionType(mt.text)
						if err != nil {
							return nil, err
						}
						r.versionType = vt
					case "if_seq_no":
						n, err := tokenLong(mt)
						if err != nil {
							return nil, err
						}
						if err := checkSeqNo(n); err != nil {
							return nil, err
						}
						r.ifSeqNo = n
					case "if_primary_term":
						n, err := tokenLong(mt)
						if err != nil {
							return nil, err
						}
						if err := checkPrimaryTerm(n); err != nil {
							return nil, err
						}
						r.ifPrimaryTerm = n
					case "retry_on_conflict":
						n, err := tokenInt(mt)
						if err != nil {
							return nil, err
						}
						r.retry = n
					case "pipeline":
						p := mt.text
						r.pipeline = &p
					case "_source":
						v, _ := s.readXValue(mt)
						sf, err := fetchSourceValue(v)
						if err != nil {
							return nil, err
						}
						fetch = &sf
					case "require_alias":
						switch {
						case mt.kind == xTrue || mt.text == "true":
							r.requireAlias = true
						case mt.kind == xFalse || mt.text == "false":
							r.requireAlias = false
						default:
							return nil, errBooleanValue(mt.text)
						}
					default:
						return nil, errIllegalArgument("Action/metadata line [%d] contains an unknown parameter [%s]", line, field)
					}
				case mt.kind == xStartObject && field == "_source":
					v, verr := s.readXValue(mt)
					if verr != nil {
						return nil, verr.jacksonError()
					}
					sf, err := fetchSourceValue(v)
					if err != nil {
						return nil, err
					}
					fetch = &sf
				case mt.kind == xNull:
				default:
					return nil, malformedLine(line, "expected a simple value for field [%s] but found [%s]", field, mt.kind)
				}
			}
		case xEndObject:
		default:
			return nil, malformedLine(line, "expected START_OBJECT or END_OBJECT but found [%s]", t.kind)
		}
		if r.action == "delete" {
			reqs = append(reqs, r)
			continue
		}
		end, err = nextLine(data, from)
		if err != nil {
			return nil, err
		}
		if end < 0 {
			break
		}
		line++
		src := bytes.TrimRight(data[from:end], "\r")
		from = end + 1
		if r.action == "update" {
			if r.versionSet || r.versionType != "internal" {
				return nil, errIllegalArgument("Update requests do not support versioning. Please use `if_seq_no` and `if_primary_term` instead")
			}
			req := newUpdateRequest()
			req.ifSeqNo, req.ifPrimaryTerm, req.retryOnConflict = r.ifSeqNo, r.ifPrimaryTerm, r.retry
			req.routing, req.requireAlias = r.routing, r.requireAlias
			fields, err := objectFields(src, "UpdateRequest", updateRequestFields)
			if err != nil {
				return nil, err
			}
			if err := req.parseBody(fields); err != nil {
				return nil, err
			}
			if req.source == nil {
				req.source = fetch
			}
			r.update = req
		} else {
			r.source = src
		}
		reqs = append(reqs, r)
	}
	return reqs, nil
}

// validateBulk is BulkRequest.validate: the validation errors of every item.
func validateBulk(reqs []*bulkRequest) error {
	var v docValidation
	if len(reqs) == 0 {
		v.add("no requests added")
	}
	for _, r := range reqs {
		if r.index == "" {
			v.add("index is missing")
		}
		dp := r.docParams()
		switch r.action {
		case "index", "create":
			dp.collectIndexValidation(r.id, r.idSet, &v)
		case "delete":
			if r.id == "" {
				v.add("id is missing")
			}
			dp.validateCASParams(&v)
		case "update":
			r.update.validate(r.id, &v)
		}
	}
	return v.err()
}

// Bulk implements POST /_bulk and /{index}/_bulk.
func (c *Cluster) Bulk(index string, data []byte, p Params) (Response, error) {
	source, sourceSet, err := fetchSourceParams(p)
	if err != nil {
		return fail(err)
	}
	var defaultSource *sourceFilter
	if sourceSet {
		defaultSource = &source
	}
	var pipeline *string
	if p.Has("pipeline") {
		s := p.Get("pipeline")
		pipeline = &s
	}
	wait, err := paramActiveShards(p)
	if err != nil {
		return fail(err)
	}
	var requireAlias *bool
	if p.Has("require_alias") {
		b, err := paramBool(p, "require_alias", false)
		if err != nil {
			return fail(err)
		}
		requireAlias = &b
	}
	timeout, err := paramTime(p, "timeout", "1m")
	if err != nil {
		return fail(err)
	}
	if err := checkRefreshParam(p); err != nil {
		return fail(err)
	}
	if len(data) == 0 {
		return fail(&Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "request body is required"})
	}
	reqs, err := parseBulk(data, index, p.Get("routing"), pipeline, requireAlias, defaultSource)
	if err != nil {
		return fail(err)
	}
	if err := validateBulk(reqs); err != nil {
		return fail(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	errorTrace, _ := paramBool(p, "error_trace", false)
	errorsSeen := false
	failItem := func(r *bulkRequest, key, index string, id any, err error) {
		e, isErr := err.(*Error)
		if !isErr {
			e = &Error{Status: http.StatusInternalServerError, Type: "exception", Reason: err.Error()}
		}
		body := e.content(errorTrace)
		if e.Type == "mapper_parsing_exception" && e.Cause == nil {
			body["caused_by"] = M{"type": "illegal_argument_exception", "reason": e.Reason}
		}
		r.done = true
		errorsSeen = true
		r.response = M{key: M{"_index": index, "_id": id, "status": e.Status, "error": body}}
	}
	// ingest: every referenced pipeline is missing
	ingest := false
	for _, r := range reqs {
		switch r.action {
		case "index", "create":
			if c.hasPipelines(r.index, r.pipeline) {
				ingest = true
				if err := c.pipelineFailure(r.index, r.pipeline); err != nil {
					failItem(r, r.itemKey(), r.index, r.idValue(), err)
				}
			}
		case "update":
			var upsertPipeline *string
			switch {
			case r.update.docAsUpsert:
			case r.update.upsert != nil:
				upsertPipeline = pipeline
			default:
				continue
			}
			if c.hasPipelines(r.index, upsertPipeline) {
				ingest = true
				if err := c.pipelineFailure(r.index, upsertPipeline); err != nil {
					failItem(r, "index", r.index, nil, err)
				}
			}
		}
	}
	// a document without id and routing for an index that requires routing
	// fails the whole request (the routing exception rejects the null id)
	for _, r := range reqs {
		if r.done || (r.action != "index" && r.action != "create") || r.idSet || r.routing != "" {
			continue
		}
		if target, err := c.resolveWriteIndex(r.index); err == nil && routingRequired(target) {
			return fail(errIDMustNotBeNull())
		}
	}
	tx := c.newDocTx()
	defer tx.commit()
	wb := newWriteBatch()
	for _, r := range reqs {
		if r.done {
			continue
		}
		if r.requireAlias && r.action != "delete" {
			if err := c.requireAliasFailure(r.index); err != nil {
				failItem(r, r.itemKey(), r.index, r.idValue(), err)
				continue
			}
		}
		var ix *Index
		var err error
		if r.action == "delete" && !isExternalVersioning(r.versionType) {
			var target *Index
			if target, err = c.resolveWriteIndex(r.index); err != nil {
				failItem(r, r.itemKey(), r.index, r.idValue(), docIndexNotFound(err))
				continue
			}
			ix, err = c.docWritable(target.Name)
		} else {
			ix, err = c.docEnsureIndex(r.index)
		}
		if err != nil {
			failItem(r, r.itemKey(), r.index, r.idValue(), err)
			continue
		}
		shownID := r.id
		if !r.idSet {
			shownID = "null"
		}
		if err := requireRouting(ix, shownID, r.routing); err != nil {
			failItem(r, r.itemKey(), ix.Name, r.idValue(), err)
			continue
		}
		if (r.action == "index" || r.action == "create") && r.idSet && r.id == "" {
			failItem(r, r.itemKey(), ix.Name, r.idValue(), errIllegalArgument("if _id is specified it must not be empty"))
			continue
		}
		if err := checkActiveShards(ix, wait, timeout); err != nil {
			failItem(r, r.itemKey(), ix.Name, r.idValue(), err)
			continue
		}
		var status int
		var out M
		switch r.action {
		case "index", "create":
			dp := r.docParams()
			id := r.id
			autoID := !r.idSet
			if autoID {
				id = generateID()
			}
			doc, perr := parseSourceDocument(r.source)
			if perr == nil {
				perr = consumeSeqNoOnFailure(ix, checkMetadataFields(doc, id))
			}
			if perr != nil {
				failItem(r, r.itemKey(), ix.Name, id, perr)
				continue
			}
			src, compact := sourceFromOrdered(r.source, doc)
			d, created, perr := ix.putDocFrom(tx, id, compact, r.source, src, dp, autoID, wb.forIndex(ix))
			if perr != nil {
				failItem(r, r.itemKey(), ix.Name, id, perr)
				continue
			}
			status, out = http.StatusOK, writeResult(ix, d, "updated")
			if created {
				status, out = http.StatusCreated, writeResult(ix, d, "created")
			}
		case "delete":
			res, derr := ix.deleteDoc(tx, r.id, r.docParams(), wb.forIndex(ix), true)
			if derr != nil {
				failItem(r, r.itemKey(), ix.Name, r.idValue(), derr)
				continue
			}
			status, out = deleteResult(ix, r.id, res)
		case "update":
			status, out, err = c.applyUpdate(tx, ix, r.id, r.update, true, wb)
			if err != nil {
				failItem(r, r.itemKey(), ix.Name, r.idValue(), err)
				continue
			}
		}
		out["status"] = status
		r.response = M{r.itemKey(): out}
	}
	if err := wb.flush(); err != nil {
		return fail(err)
	}
	items := make([]any, len(reqs))
	for i, r := range reqs {
		items[i] = r.response
	}
	res := M{"took": 1, "errors": errorsSeen, "items": items}
	if ingest {
		res["ingest_took"] = 0
	}
	return ok(res)
}
