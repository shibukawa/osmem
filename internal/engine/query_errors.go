package engine

import (
	"fmt"
	"net/http"
	"sync"
)

// OpenSearch turns a query into a Lucene query in two steps: the request is
// parsed once on the coordinating node (a malformed query fails the whole
// request with the parser's exception) and the parsed query is then created
// on every shard (a failure there is a shard failure, reported inside a
// search_phase_execution_exception). The query builder parses the whole
// query before creating any part of it, and remembers the errors of the
// parse step so that the search layer can report them as they are.
var parseFailures = struct {
	sync.Mutex
	m map[*Error]struct{}
}{m: map[*Error]struct{}{}}

// parseFailure marks e as an error of the parse step.
func parseFailure(e *Error) *Error {
	parseFailures.Lock()
	if len(parseFailures.m) > 4096 {
		// errors of requests whose callers never asked; forget them
		parseFailures.m = map[*Error]struct{}{}
	}
	parseFailures.m[e] = struct{}{}
	parseFailures.Unlock()
	return e
}

// isQueryParseFailure reports whether err was raised while parsing a query
// rather than while creating it for an index. Parse failures are request
// errors, not shard failures, and must not be wrapped in
// search_phase_execution_exception.
func isQueryParseFailure(err error) bool {
	e, ok := err.(*Error)
	if !ok {
		return false
	}
	parseFailures.Lock()
	_, found := parseFailures.m[e]
	parseFailures.Unlock()
	return found
}

// parse step errors ------------------------------------------------------

func pParsing(format string, args ...any) *Error {
	return parseFailure(errParsing(format, args...))
}

func pIllegalArgument(format string, args ...any) *Error {
	return parseFailure(errIllegalArgument(format, args...))
}

func pXContent(format string, args ...any) *Error {
	return parseFailure(&Error{Status: http.StatusBadRequest, Type: "x_content_parse_exception", Reason: fmt.Sprintf(format, args...)})
}

// pNumberFormat is Java's NumberFormatException for a request value.
func pNumberFormat(input string) *Error {
	return parseFailure(&Error{Status: http.StatusBadRequest, Type: "number_format_exception", Reason: javaNumberFormatReason(input)})
}

func javaNumberFormatReason(input string) string {
	return fmt.Sprintf("For input string: \"%s\"", input)
}

// pInputCoercion is Jackson's InputCoercionException.
func pInputCoercion(reason string) *Error {
	return parseFailure(&Error{Status: http.StatusBadRequest, Type: "input_coercion_exception", Reason: reason})
}

// pIllegalState is an IllegalStateException raised while parsing (reported
// with status 500, as OpenSearch does).
func pIllegalState(reason string) *Error {
	return parseFailure(&Error{Status: http.StatusInternalServerError, Type: "illegal_state_exception", Reason: reason})
}

// pParse is OpenSearchParseException.
func pParse(format string, args ...any) *Error {
	return parseFailure(&Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: fmt.Sprintf(format, args...)})
}

// withCause sets the cause of an error and returns the error.
func withCause(e, cause *Error) *Error {
	e.Cause = cause
	return e
}

// shard step errors ------------------------------------------------------

// errCreateQuery is a failure to create a query on a shard: a
// query_shard_exception "failed to create query: ..." caused by the Java
// exception.
func errCreateQuery(typ, reason string) *Error {
	status := http.StatusBadRequest
	return &Error{Status: status, Type: "query_shard_exception", Reason: "failed to create query: " + reason,
		Cause: &Error{Type: typ, Reason: reason}}
}

// errCreateNumberFormat is a NumberFormatException while creating a query.
func errCreateNumberFormat(input string) *Error {
	return errCreateQuery("number_format_exception", javaNumberFormatReason(input))
}

// errShardFailure is an exception thrown while executing a query on a shard
// (it is not wrapped in query_shard_exception).
func errShardFailure(status int, typ, reason string) *Error {
	return &Error{Status: status, Type: typ, Reason: reason}
}
