package engine

import (
	"fmt"
	"net/http"
)

// Error is an OpenSearch-style exception. It renders as
//
//	{"error": {"root_cause": [...], "type": ..., "reason": ...}, "status": N}
type Error struct {
	Status int
	Type   string
	Reason string
	Index  string
	Extra  map[string]any
	// RootType/RootReason override the root_cause entry (search errors wrap
	// the underlying exception).
	RootType   string
	RootReason string
	RootIndex  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s (status %d)", e.Type, e.Reason, e.Status)
}

// Body returns the JSON body for the error.
func (e *Error) Body() map[string]any {
	cause := map[string]any{"type": e.Type, "reason": e.Reason}
	if e.RootType != "" {
		cause = map[string]any{"type": e.RootType, "reason": e.RootReason}
		if e.RootIndex != "" {
			cause["index"] = e.RootIndex
			cause["index_uuid"] = "_na_"
		}
	}
	inner := map[string]any{"type": e.Type, "reason": e.Reason}
	if e.Index != "" {
		cause["index"] = e.Index
		cause["index_uuid"] = "_na_"
		inner["index"] = e.Index
		inner["index_uuid"] = "_na_"
	}
	for k, v := range e.Extra {
		inner[k] = v
	}
	inner["root_cause"] = []any{cause}
	return map[string]any{"error": inner, "status": e.Status}
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

func errActionRequestValidation(reason string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "action_request_validation_exception", Reason: "Validation Failed: 1: " + reason + ";"}
}

func errVersionConflict(index, id, reason string) *Error {
	return &Error{Status: http.StatusConflict, Type: "version_conflict_engine_exception", Reason: "[" + id + "]: " + reason, Index: index}
}

func errDocumentMissing(index, id string) *Error {
	return &Error{Status: http.StatusNotFound, Type: "document_missing_exception", Reason: "[" + id + "]: document missing", Index: index}
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
