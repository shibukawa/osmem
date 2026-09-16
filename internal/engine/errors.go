package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Error is an OpenSearch-style exception. It renders as
//
//	{"error": {"root_cause": [...], "type": ..., "reason": ..., "caused_by": {...}}, "status": N}
//
// the way OpenSearchException does: root_cause lists the deepest OpenSearch
// exception of the cause chain (a plain Java exception such as
// illegal_argument_exception is its own root cause), with its metadata but
// without causes.
type Error struct {
	Status int
	Type   string
	Reason string
	// Index adds the index and index_uuid metadata.
	Index string
	// Extra holds further metadata (resource.id, shard, line, col, ...).
	Extra map[string]any
	// Cause is rendered as caused_by.
	Cause *Error
	// failure makes this a search_phase_execution_exception with one shard
	// failure (see errSearchPhase).
	failure *shardFailure
	// more lists further (grouped) shard failures of a search phase failure.
	more []*shardFailure
	// plain marks a Java exception whose type name is also used by an
	// OpenSearchException (Lucene's parse_exception).
	plain bool
	// noRootCause renders an empty root_cause list.
	noRootCause bool
	// nullReason renders a null reason (a Java exception without message).
	nullReason bool
	// pos is the location of the exception in the request body, which
	// LocateError renders (position.go).
	pos *errPosition
	// openSearch marks an OpenSearchException whose type name is also used
	// by plain Java exceptions (the bare "exception" type).
	openSearch bool
}

type shardFailure struct {
	shard int // -1 when the failure has no shard (a missing search context)
	index string
	cause *Error
}

// plainJavaExceptions are exception types that are not OpenSearchExceptions.
// They are their own root cause and, as the cause of a search phase
// failure, render wrapped once more.
var plainJavaExceptions = map[string]bool{
	"illegal_argument_exception": true, "illegal_state_exception": true, "number_format_exception": true,
	"x_content_parse_exception": true, "named_object_not_found_exception": true, "json_parse_exception": true,
	"stream_read_exception": true, "date_time_parse_exception": true, "unsupported_operation_exception": true,
	"array_index_out_of_bounds_exception": true, "null_pointer_exception": true, "arithmetic_exception": true,
	"not_x_content_exception": true, "input_coercion_exception": true, "class_cast_exception": true,
	"exception": true,
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s (status %d)", e.Type, e.Reason, e.Status)
}

// Body returns the JSON body for the error.
func (e *Error) Body() map[string]any {
	inner := e.content()
	roots := []any{}
	if !e.noRootCause {
		for _, r := range e.rootCauses() {
			roots = append(roots, r.header())
		}
	}
	inner["root_cause"] = roots
	return map[string]any{"error": inner, "status": e.Status}
}

func (e *Error) isPlain() bool { return !e.openSearch && (e.plain || plainJavaExceptions[e.Type]) }

// header renders the type, reason and metadata of the exception.
func (e *Error) header() M {
	out := M{"type": e.Type, "reason": e.Reason}
	if e.nullReason {
		out["reason"] = nil
	}
	if e.Index != "" {
		out["index"] = e.Index
		out["index_uuid"] = "_na_"
	}
	for k, v := range e.Extra {
		out[k] = v
	}
	return out
}

// content renders the exception with its causes (as bulk items and
// failed_shards show exceptions).
func (e *Error) content() M {
	out := e.header()
	if f := e.failure; f != nil {
		out["phase"] = "query"
		out["grouped"] = true
		failed := make([]any, 0, 1+len(e.more))
		for _, g := range append([]*shardFailure{f}, e.more...) {
			shard := M{"shard": g.shard, "reason": g.cause.content()}
			if g.shard >= 0 {
				shard["node"] = "osmem"
				if g.index != "" {
					shard["index"] = g.index
				}
			} else {
				shard["index"] = nil
			}
			failed = append(failed, shard)
		}
		out["failed_shards"] = failed
		if roots := f.cause.rootCauses(); len(roots) > 0 {
			out["caused_by"] = roots[0].rootContent()
		}
	}
	if e.Cause != nil {
		out["caused_by"] = e.Cause.content()
	}
	return out
}

// rootCauses is OpenSearchException.guessRootCauses.
func (e *Error) rootCauses() []*Error {
	if e.failure != nil {
		roots := e.failure.cause.rootCauses()
		for _, g := range e.more {
			roots = append(roots, g.cause.rootCauses()...)
		}
		return roots
	}
	// a parse exception caused by another parse exception or an
	// OpenSearchException reports the inner one
	if e.Type == "x_content_parse_exception" && e.Cause != nil && (e.Cause.Type == "x_content_parse_exception" || !e.Cause.isPlain()) {
		return e.Cause.rootCauses()
	}
	// an XContentParseException unwraps to its innermost parse exception (or
	// the OpenSearchException that caused it)
	if e.Type == "x_content_parse_exception" && e.Cause != nil && (e.Cause.Type == "x_content_parse_exception" || !e.Cause.isPlain()) {
		return e.Cause.rootCauses()
	}
	if !e.isPlain() && e.Cause != nil && !e.Cause.isPlain() {
		return e.Cause.rootCauses()
	}
	return []*Error{e}
}

// rootContent renders a root cause as the caused_by of a search phase
// failure: a plain Java exception is wrapped in an exception of its own name.
func (e *Error) rootContent() M {
	if e.isPlain() {
		out := M{"type": e.Type, "reason": e.Reason, "caused_by": e.content()}
		if e.nullReason {
			out["reason"] = nil
		}
		return out
	}
	return e.content()
}

func errIndexNotFound(name string) *Error {
	return &Error{Status: http.StatusNotFound, Type: "index_not_found_exception", Reason: "no such index [" + name + "]", Index: name,
		Extra: map[string]any{"resource.type": "index_or_alias", "resource.id": name}}
}

func errIndexExists(name string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "resource_already_exists_exception", Reason: "index [" + name + "/_na_] already exists", Index: name}
}

func errParsing(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "parsing_exception", Reason: fmt.Sprintf(format, args...)}
}

func errIllegalArgument(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: fmt.Sprintf(format, args...)}
}

func errMapperParsing(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "mapper_parsing_exception", Reason: fmt.Sprintf(format, args...)}
}

func errMapperException(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "mapper_exception", Reason: fmt.Sprintf(format, args...)}
}

func errActionRequestValidation(reason string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "action_request_validation_exception", Reason: "Validation Failed: 1: " + reason + ";"}
}

// shard-level document exceptions carry the shard id ("0": osmem models a
// single primary per index for document placement)
func errVersionConflict(index, id, reason string) *Error {
	return &Error{Status: http.StatusConflict, Type: "version_conflict_engine_exception", Reason: "[" + id + "]: " + reason, Index: index,
		Extra: map[string]any{"shard": "0"}}
}

func errDocumentMissing(index, id string) *Error {
	return &Error{Status: http.StatusNotFound, Type: "document_missing_exception", Reason: "[" + id + "]: document missing", Index: index,
		Extra: map[string]any{"shard": "0"}}
}

// errNumberFormat is the failure to parse a query value for a numeric field
// (Java's NumberFormatException).
func errNumberFormat(value any) *Error {
	reason := fmt.Sprintf("For input string: \"%v\"", value)
	return &Error{Status: http.StatusBadRequest, Type: "query_shard_exception", Reason: "failed to create query: " + reason,
		Cause: &Error{Type: "number_format_exception", Reason: reason}}
}

// errDateQuery is the failure to parse a date in a query: the date field
// reports a parse_exception around the formatter's failure.
func errDateQuery(err error) *Error {
	msg := err.Error()
	if !strings.HasPrefix(msg, "failed to parse date field") {
		return &Error{Status: http.StatusBadRequest, Type: "query_shard_exception", Reason: "failed to create query: " + msg}
	}
	parse := &Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: msg + ": [" + msg + "]",
		Cause: &Error{Type: "illegal_argument_exception", Reason: msg,
			Cause: &Error{Type: "date_time_parse_exception", Reason: "Failed to parse with all enclosed parsers"}}}
	return &Error{Status: http.StatusBadRequest, Type: "query_shard_exception", Reason: "failed to create query: " + parse.Reason, Cause: parse}
}

// errQueryStringSyntax is Lucene's query parser rejecting a query_string.
func errQueryStringSyntax(text string) *Error {
	column := utf8.RuneCountInString(text)
	encountered := fmt.Sprintf("Encountered \"<EOF>\" at line 1, column %d.", column)
	return &Error{Status: http.StatusBadRequest, Type: "query_shard_exception", Reason: "Failed to parse query [" + text + "]",
		Cause: &Error{Type: "parse_exception", plain: true, Reason: "Cannot parse '" + text + "': " + encountered,
			Cause: &Error{Type: "parse_exception", plain: true, Reason: encountered}}}
}

// errJSONParse reports a malformed request body the way Jackson does.
func errJSONParse(data []byte, err error) *Error {
	offset := len(data)
	var reason string
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) && syntax.Offset > 0 && int(syntax.Offset) <= len(data) {
		offset = int(syntax.Offset) - 1
		c := data[offset]
		expecting := ""
		switch msg := syntax.Error(); {
		case strings.Contains(msg, "looking for beginning of object key string"):
			expecting = "was expecting double-quote to start property name"
		case strings.Contains(msg, "after object key:value pair") || strings.Contains(msg, "after array element"):
			if c >= '0' && c <= '9' && offset > 0 && data[offset-1] == '0' && (offset < 2 || !isJSONDigit(data[offset-2])) {
				// "01": Jackson rejects the leading zero at the second digit
				reason = "Invalid numeric value: Leading zeroes not allowed"
				break
			}
			if strings.Contains(msg, "after array element") {
				expecting = "was expecting comma to separate Array entries"
			} else {
				expecting = "was expecting comma to separate Object entries"
			}
		case strings.Contains(msg, "after object key"):
			expecting = "was expecting a colon to separate property name and value"
		case strings.Contains(msg, "in literal"):
			// "tru}": Jackson reports the whole bare word at its start
			start := offset
			for start > 0 && (data[start-1] >= 'a' && data[start-1] <= 'z' || data[start-1] >= 'A' && data[start-1] <= 'Z') {
				start--
			}
			offset = start
			reason = unexpectedValueReason(data[start], jsonWordAt(data, start))
		case strings.Contains(msg, "looking for beginning of value"):
			reason = unexpectedValueReason(c, jsonWordAt(data, offset))
		default:
			expecting = "expected a valid value (JSON String, Number, Array, Object or token 'null', 'true' or 'false')"
		}
		if reason == "" {
			reason = fmt.Sprintf("Unexpected character ('%c' (code %d)): %s", c, c, expecting)
		}
	} else {
		reason = "Unexpected end-of-input: expected close marker for Object"
	}
	reason += fmt.Sprintf("\n at [Source: REDACTED (`StreamReadFeature.INCLUDE_SOURCE_IN_LOCATION` disabled); byte offset: #%d]", offset)
	return &Error{Status: http.StatusBadRequest, Type: "json_parse_exception", Reason: reason,
		Cause: &Error{Type: "stream_read_exception", Reason: reason}}
}

func errUnsupported(what string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "unsupported_operation_exception", Reason: what + " is not supported by osmem"}
}

func errAliasMissing(name string) *Error {
	return &Error{Status: http.StatusNotFound, Type: "aliases_not_found_exception", Reason: "aliases [" + name + "] missing"}
}

func errStrictDynamic(field string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "strict_dynamic_mapping_exception", Reason: "mapping set to strict, dynamic introduction of [" + field + "] within [_doc] is not allowed"}
}
