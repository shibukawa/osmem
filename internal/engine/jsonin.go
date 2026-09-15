package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Request bodies are parsed by Jackson with STRICT_DUPLICATE_DETECTION, so a
// repeated object key anywhere in a document is a json_parse_exception.

// jacksonLocation is the location suffix Jackson appends to parse errors.
func jacksonLocation(offset int) string {
	return fmt.Sprintf("\n at [Source: REDACTED (`StreamReadFeature.INCLUDE_SOURCE_IN_LOCATION` disabled); byte offset: #%d]", offset)
}

// DuplicateKeyError is a repeated object key in a JSON document. Offset is
// the byte offset just past the repeated key, where Jackson reports it.
type DuplicateKeyError struct {
	Name   string
	Offset int
}

func (e *DuplicateKeyError) Error() string {
	return "Duplicate Object property \"" + e.Name + "\"" + jacksonLocation(e.Offset)
}

// jsonParseError renders a Jackson parse failure: json_parse_exception
// caused by stream_read_exception with the same message.
func jsonParseError(reason string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "json_parse_exception", Reason: reason,
		Cause: &Error{Type: "stream_read_exception", Reason: reason}}
}

// JSONParseCause converts a failure of decodeJSON on data into the
// json_parse_exception OpenSearch reports, for wrapping exceptions such as
// mapper_parsing_exception. It returns nil for other errors.
func JSONParseCause(data []byte, err error) *Error {
	switch e := err.(type) {
	case *DuplicateKeyError:
		return jsonParseError(e.Error())
	case *json.SyntaxError:
		return errJSONParse(data, err)
	}
	return nil
}

// keySet records the keys of one JSON object while scanning for duplicates.
type keySet struct {
	object    bool
	expectKey bool
	keys      []string
	set       map[string]struct{}
}

func (k *keySet) addIfAbsent(name string) bool {
	if k.set != nil {
		if _, dup := k.set[name]; dup {
			return false
		}
		k.set[name] = struct{}{}
		return true
	}
	for _, existing := range k.keys {
		if existing == name {
			return false
		}
	}
	k.keys = append(k.keys, name)
	if len(k.keys) > 8 {
		k.set = make(map[string]struct{}, len(k.keys)*2)
		for _, existing := range k.keys {
			k.set[existing] = struct{}{}
		}
		k.keys = nil
	}
	return true
}

// findDuplicateKey scans the first JSON value of data, which must already be
// known to be valid JSON, and reports the first repeated object key.
func findDuplicateKey(data []byte) *DuplicateKeyError {
	var stack []*keySet
	value := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].expectKey = true
		}
	}
	for i := 0; i < len(data); {
		switch c := data[i]; c {
		case ' ', '\t', '\n', '\r', ',', ':':
			i++
		case '{', '[':
			value()
			stack = append(stack, &keySet{object: c == '{', expectKey: c == '{'})
			i++
		case '}', ']':
			if len(stack) == 0 {
				return nil
			}
			stack = stack[:len(stack)-1]
			i++
			if len(stack) == 0 {
				return nil
			}
		case '"':
			end, escaped := i+1, false
			for end < len(data) && data[end] != '"' {
				if data[end] == '\\' {
					escaped = true
					end++
				}
				end++
			}
			if end >= len(data) {
				return nil
			}
			if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].expectKey {
				name := string(data[i+1 : end])
				if escaped {
					var s string
					if json.Unmarshal(data[i:end+1], &s) == nil {
						name = s
					}
				}
				if !stack[n-1].addIfAbsent(name) {
					return &DuplicateKeyError{Name: name, Offset: end + 1}
				}
				stack[n-1].expectKey = false
			} else {
				value()
			}
			i = end + 1
			if len(stack) == 0 {
				return nil
			}
		default:
			for i < len(data) {
				b := data[i]
				if b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == ',' || b == ':' || b == ']' || b == '}' {
					break
				}
				i++
			}
			value()
			if len(stack) == 0 {
				return nil
			}
		}
	}
	return nil
}

// JSONToken describes the first token of a request body the way Jackson
// names it (START_OBJECT, VALUE_NULL, ...). Name is "null" when the body
// holds only whitespace. Line and Col are Jackson's 1-based token location
// (line 1, column 0 when there is no token).
type JSONToken struct {
	Name   string
	Offset int
	Line   int
	Col    int
}

// FirstJSONToken returns the first token of data starting at offset from.
func FirstJSONToken(data []byte, from int) JSONToken {
	i := from
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	if i >= len(data) {
		return JSONToken{Name: "null", Offset: i, Line: 1, Col: 0}
	}
	line, col := 1, 1
	for j := 0; j < i; j++ {
		if data[j] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	tok := JSONToken{Offset: i, Line: line, Col: col}
	switch c := data[i]; {
	case c == '{':
		tok.Name = "START_OBJECT"
	case c == '[':
		tok.Name = "START_ARRAY"
	case c == '}':
		tok.Name = "END_OBJECT"
	case c == ']':
		tok.Name = "END_ARRAY"
	case c == '"':
		tok.Name = "VALUE_STRING"
	case c == '-' || (c >= '0' && c <= '9'):
		tok.Name = "VALUE_NUMBER"
	case hasWord(data[i:], "true"), hasWord(data[i:], "false"):
		tok.Name = "VALUE_BOOLEAN"
	case hasWord(data[i:], "null"):
		tok.Name = "VALUE_NULL"
	default:
		tok.Name = ""
	}
	return tok
}

func hasWord(data []byte, word string) bool {
	if len(data) < len(word) || string(data[:len(word)]) != word {
		return false
	}
	if len(data) == len(word) {
		return true
	}
	c := data[len(word)]
	return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_')
}

// unrecognizedTokenError is Jackson's failure on a bare word where a value
// is expected ("Unrecognized token 'garbage': ...").
func unrecognizedTokenError(data []byte, offset int) *Error {
	return jsonParseError(unexpectedValueReason(data[offset], jsonWordAt(data, offset)) + jacksonLocation(offset))
}

// DecodeSearchBody parses a _search body like SearchSourceBuilder with
// trailing-token checks: the body must be one JSON object, possibly followed
// by whitespace. An empty body yields an empty object.
func DecodeSearchBody(data []byte) (M, error) {
	m, trailing, err := DecodeSearchBodyParts(data)
	if err != nil {
		return nil, err
	}
	if trailing != nil {
		return nil, trailing
	}
	return m, nil
}

// DecodeSearchBodyParts is DecodeSearchBody with the failure about content
// after the main object returned separately: OpenSearch only checks for it
// once the object itself has been parsed.
func DecodeSearchBodyParts(data []byte) (M, *Error, error) {
	if len(data) == 0 {
		return M{}, nil, nil
	}
	tok := FirstJSONToken(data, 0)
	switch tok.Name {
	case "START_OBJECT":
	case "":
		return nil, nil, unrecognizedTokenError(data, tok.Offset)
	default:
		return nil, nil, &Error{Status: http.StatusBadRequest, Type: "parsing_exception",
			Reason: "Expected [START_OBJECT] but found [" + tok.Name + "]",
			Extra:  map[string]any{"line": tok.Line, "col": tok.Col}}
	}
	m, end, err := decodeObjectPrefix(data)
	if err != nil {
		return nil, nil, err
	}
	next := FirstJSONToken(data, end)
	switch next.Name {
	case "null":
		return m, nil, nil
	case "":
		return m, unrecognizedTokenError(data, next.Offset), nil
	case "END_OBJECT":
		return m, jsonParseError("Unexpected close marker '}': no open Object to close" + jacksonLocation(next.Offset)), nil
	case "END_ARRAY":
		return m, jsonParseError("Unexpected close marker ']': no open Array to close" + jacksonLocation(next.Offset)), nil
	}
	return m, &Error{Status: http.StatusBadRequest, Type: "parsing_exception",
		Reason: "Unexpected token [" + next.Name + "] found after the main object.",
		Extra:  map[string]any{"line": next.Line, "col": next.Col}}, nil
}

// decodeObjectPrefix decodes the object at the start of data and returns
// the offset just past it.
func decodeObjectPrefix(data []byte) (M, int, error) {
	m, err := decodeObject(data)
	if err != nil {
		return nil, 0, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, 0, errJSONParse(data, err)
	}
	return m, int(dec.InputOffset()), nil
}

// unexpectedValueReason is Jackson's message for a character that cannot
// start a value: bare words are unrecognized tokens, separators and close
// markers "expected a value", anything else "expected a valid value".
func unexpectedValueReason(c byte, word string) string {
	switch {
	case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		return "Unrecognized token '" + word + "': was expecting (JSON String, Number, Array, Object or token 'null', 'true' or 'false')"
	case c == ',' || c == ']' || c == '}':
		return fmt.Sprintf("Unexpected character ('%c' (code %d)): expected a value", c, c)
	}
	return fmt.Sprintf("Unexpected character ('%c' (code %d)): expected a valid value (JSON String, Number, Array, Object or token 'null', 'true' or 'false')", c, c)
}

func isJSONDigit(c byte) bool { return c >= '0' && c <= '9' }

// jsonWordAt returns the run of non-separator characters starting at offset.
func jsonWordAt(data []byte, offset int) string {
	end := offset
	for end < len(data) {
		c := data[end]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || c == ':' || c == '[' || c == ']' || c == '{' || c == '}' || c == '"' {
			break
		}
		end++
	}
	if end == offset && offset < len(data) {
		end++
	}
	return string(data[offset:end])
}

// documentParseError is the mapper_parsing_exception OpenSearch reports for
// a document source it cannot parse; err is the decoding failure, nil when
// the source is valid JSON but not an object.
func documentParseError(raw []byte, err error) *Error {
	wrap := func(cause *Error) *Error {
		return &Error{Status: http.StatusBadRequest, Type: "mapper_parsing_exception", Reason: "failed to parse", Cause: cause}
	}
	tok := FirstJSONToken(raw, 0)
	if tok.Name != "START_OBJECT" && tok.Name != "" {
		return wrap(&Error{Type: "not_x_content_exception", Reason: "Compressor detection can only be called on some xcontent bytes or compressed xcontent bytes"})
	}
	if cause := JSONParseCause(raw, err); cause != nil && !isTrailingDataError(err) {
		return wrap(cause)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var first json.RawMessage
	if dec.Decode(&first) == nil {
		next := FirstJSONToken(raw, int(dec.InputOffset()))
		switch next.Name {
		case "null":
		case "":
			return wrap(unrecognizedTokenError(raw, next.Offset))
		default:
			return wrap(&Error{Type: "illegal_argument_exception", Reason: "Malformed content, found extra data after parsing: " + next.Name})
		}
	}
	if err == nil {
		return wrap(&Error{Type: "not_x_content_exception", Reason: "Compressor detection can only be called on some xcontent bytes or compressed xcontent bytes"})
	}
	return errMapperParsing("failed to parse: %s", err.Error())
}

func isTrailingDataError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "after top-level value")
}

// JSONKey is a top-level object key with Jackson's 1-based token location.
type JSONKey struct {
	Name      string
	Line, Col int
}

// TopLevelKeys lists the keys of the object at the start of data, which
// must be valid JSON.
func TopLevelKeys(data []byte) []JSONKey {
	var keys []JSONKey
	depth, expectKey := 0, false
	line, lineStart := 1, 0
	for i := 0; i < len(data); i++ {
		switch c := data[i]; c {
		case '\n':
			line, lineStart = line+1, i+1
		case '{', '[':
			depth++
			expectKey = c == '{' && depth == 1
		case '}', ']':
			depth--
			if depth == 0 {
				return keys
			}
		case ',':
			expectKey = depth == 1
		case '"':
			end := i + 1
			for end < len(data) && data[end] != '"' {
				if data[end] == '\\' {
					end++
				}
				end++
			}
			if depth == 1 && expectKey && end < len(data) {
				name := string(data[i+1 : end])
				var s string
				if json.Unmarshal(data[i:end+1], &s) == nil {
					name = s
				}
				keys = append(keys, JSONKey{Name: name, Line: line, Col: i - lineStart + 1})
				expectKey = false
			}
			i = end
		}
	}
	return keys
}

// FindDuplicateKey reports the first repeated object key of valid JSON data.
func FindDuplicateKey(data []byte) *DuplicateKeyError { return findDuplicateKey(data) }

// TokenLocationBefore returns Jackson's location (1-based line and column)
// of the token that ends before offset: the location a parser reports for
// the last token it read when the next one fails.
func TokenLocationBefore(data []byte, offset int) (line, col int) {
	i := offset - 1
	for i >= 0 && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r' || data[i] == ',' || data[i] == ':') {
		i--
	}
	if i < 0 {
		return 1, 0
	}
	switch c := data[i]; {
	case c == '"':
		i--
		for i >= 0 && !(data[i] == '"' && (i == 0 || data[i-1] != '\\')) {
			i--
		}
	case c == '}' || c == ']':
	default:
		for i > 0 && !strings.ContainsRune(" \t\n\r,:[{", rune(data[i-1])) {
			i--
		}
	}
	if i < 0 {
		i = 0
	}
	line, lineStart := 1, 0
	for j := 0; j < i; j++ {
		if data[j] == '\n' {
			line, lineStart = line+1, j+1
		}
	}
	return line, i - lineStart + 1
}
