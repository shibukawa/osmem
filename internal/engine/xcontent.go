package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file is a small JSON reader with the token model, locations and error
// messages of OpenSearch's XContent/Jackson parser. Request bodies that
// OpenSearch parses with ObjectParser (update requests, bulk action lines)
// and document sources need them to report the same errors, and document
// sources keep their key order through updates.

type xKind int

const (
	xEOF xKind = iota
	xStartObject
	xEndObject
	xStartArray
	xEndArray
	xFieldName
	xString
	xNumber
	xTrue
	xFalse
	xNull
)

// String is the XContentParser.Token name.
func (k xKind) String() string {
	switch k {
	case xStartObject:
		return "START_OBJECT"
	case xEndObject:
		return "END_OBJECT"
	case xStartArray:
		return "START_ARRAY"
	case xEndArray:
		return "END_ARRAY"
	case xFieldName:
		return "FIELD_NAME"
	case xString:
		return "VALUE_STRING"
	case xNumber:
		return "VALUE_NUMBER"
	case xTrue, xFalse:
		return "VALUE_BOOLEAN"
	case xNull:
		return "VALUE_NULL"
	}
	return "null"
}

// isValue is XContentParser.Token.isValue (null is not a value).
func (k xKind) isValue() bool {
	return k == xString || k == xNumber || k == xTrue || k == xFalse
}

type xToken struct {
	kind  xKind
	start int
	end   int
	text  string // field name, decoded string or number text
}

type xSyntaxError struct {
	offset int
	msg    string
}

func (e *xSyntaxError) Error() string { return e.msg }

// jacksonError renders a syntax error as OpenSearch reports Jackson's
// exception: json_parse_exception caused by stream_read_exception.
func (e *xSyntaxError) jacksonError() *Error {
	reason := e.msg + fmt.Sprintf("\n at [Source: REDACTED (`StreamReadFeature.INCLUDE_SOURCE_IN_LOCATION` disabled); byte offset: #%d]", e.offset)
	return &Error{Status: http.StatusBadRequest, Type: "json_parse_exception", Reason: reason,
		Cause: &Error{Type: "stream_read_exception", Reason: reason}}
}

const (
	xsValue = iota
	xsObjectFirst
	xsObjectKey
	xsColon
	xsObjectComma
	xsArrayFirst
	xsArrayComma
	xsDone
)

type xScanner struct {
	data  []byte
	pos   int
	stack []byte
	state int
	last  xToken // the last token read successfully
}

func newXScanner(data []byte) *xScanner { return &xScanner{data: data} }

func (s *xScanner) skipWS() {
	for s.pos < len(s.data) {
		switch s.data[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}

func jacksonCharDesc(c rune) string {
	if c < 0x20 || (c >= 0x7f && c <= 0x9f) {
		return fmt.Sprintf("(CTRL-CHAR, code %d)", c)
	}
	if c > 255 {
		return fmt.Sprintf("'%c' (code %d / 0x%x)", c, c, c)
	}
	return fmt.Sprintf("'%c' (code %d)", c, c)
}

func (s *xScanner) unexpected(msg string) *xSyntaxError {
	r, _ := utf8.DecodeRune(s.data[s.pos:])
	return &xSyntaxError{offset: s.pos, msg: "Unexpected character (" + jacksonCharDesc(r) + "): " + msg}
}

// lineCol returns the 1-based line and column of a byte offset.
func (s *xScanner) lineCol(offset int) (int, int) {
	line, col := 1, 1
	for i := 0; i < offset && i < len(s.data); i++ {
		if s.data[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// location renders a token location the way XContentLocation does.
func (s *xScanner) location(t xToken) string {
	l, c := s.lineCol(t.start)
	return fmt.Sprintf("[%d:%d]", l, c)
}

func (s *xScanner) next() (xToken, *xSyntaxError) {
	t, err := s.next0()
	if err == nil {
		s.last = t
	}
	return t, err
}

func (s *xScanner) next0() (xToken, *xSyntaxError) {
	for {
		s.skipWS()
		if s.pos >= len(s.data) {
			if len(s.stack) > 0 {
				marker := "Object"
				if s.stack[len(s.stack)-1] == '[' {
					marker = "Array"
				}
				return xToken{}, &xSyntaxError{offset: s.pos, msg: "Unexpected end-of-input: expected close marker for " + marker}
			}
			return xToken{kind: xEOF, start: s.pos, end: s.pos}, nil
		}
		c := s.data[s.pos]
		switch s.state {
		case xsObjectFirst, xsObjectKey:
			if c == '}' && s.state == xsObjectFirst {
				return s.closeContainer(xEndObject), nil
			}
			if c != '"' {
				return xToken{}, s.unexpected("was expecting double-quote to start property name")
			}
			start := s.pos
			str, err := s.scanString()
			if err != nil {
				return xToken{}, err
			}
			s.state = xsColon
			return xToken{kind: xFieldName, start: start, end: s.pos, text: str}, nil
		case xsColon:
			if c != ':' {
				return xToken{}, s.unexpected("was expecting a colon to separate property name and value")
			}
			s.pos++
			s.state = xsValue
		case xsObjectComma:
			switch c {
			case ',':
				s.pos++
				s.state = xsObjectKey
			case '}':
				return s.closeContainer(xEndObject), nil
			default:
				return xToken{}, s.unexpected("was expecting comma to separate Object entries")
			}
		case xsArrayFirst:
			if c == ']' {
				return s.closeContainer(xEndArray), nil
			}
			return s.scanValue()
		case xsArrayComma:
			switch c {
			case ',':
				s.pos++
				s.state = xsValue
			case ']':
				return s.closeContainer(xEndArray), nil
			default:
				return xToken{}, s.unexpected("was expecting comma to separate Array entries")
			}
		default:
			return s.scanValue()
		}
	}
}

func (s *xScanner) closeContainer(kind xKind) xToken {
	start := s.pos
	s.pos++
	s.stack = s.stack[:len(s.stack)-1]
	s.afterValue()
	return xToken{kind: kind, start: start, end: s.pos}
}

func (s *xScanner) afterValue() {
	switch {
	case len(s.stack) == 0:
		s.state = xsDone
	case s.stack[len(s.stack)-1] == '{':
		s.state = xsObjectComma
	default:
		s.state = xsArrayComma
	}
}

func isJSONIdentStart(r rune) bool { return unicode.IsLetter(r) || r == '_' || r == '$' }

func (s *xScanner) scanValue() (xToken, *xSyntaxError) {
	start := s.pos
	c := s.data[s.pos]
	switch {
	case c == '{':
		s.pos++
		s.stack = append(s.stack, '{')
		s.state = xsObjectFirst
		return xToken{kind: xStartObject, start: start, end: s.pos}, nil
	case c == '[':
		s.pos++
		s.stack = append(s.stack, '[')
		s.state = xsArrayFirst
		return xToken{kind: xStartArray, start: start, end: s.pos}, nil
	case c == '"':
		str, err := s.scanString()
		if err != nil {
			return xToken{}, err
		}
		s.afterValue()
		return xToken{kind: xString, start: start, end: s.pos, text: str}, nil
	case c == '-' || (c >= '0' && c <= '9'):
		if err := s.scanNumber(); err != nil {
			return xToken{}, err
		}
		s.afterValue()
		return xToken{kind: xNumber, start: start, end: s.pos, text: string(s.data[start:s.pos])}, nil
	case c == '}' || c == ',' || (c == ']' && len(s.stack) > 0 && s.stack[len(s.stack)-1] == '['):
		return xToken{}, s.unexpected("expected a value")
	}
	r, _ := utf8.DecodeRune(s.data[s.pos:])
	if isJSONIdentStart(r) {
		end := s.pos
		for end < len(s.data) {
			r2, size := utf8.DecodeRune(s.data[end:])
			if !isJSONIdentStart(r2) && !unicode.IsDigit(r2) {
				break
			}
			end += size
		}
		word := string(s.data[s.pos:end])
		kind := xEOF
		switch word {
		case "true":
			kind = xTrue
		case "false":
			kind = xFalse
		case "null":
			kind = xNull
		}
		if kind != xEOF {
			s.pos = end
			s.afterValue()
			return xToken{kind: kind, start: start, end: end, text: word}, nil
		}
		return xToken{}, &xSyntaxError{offset: start, msg: "Unrecognized token '" + word + "': was expecting (JSON String, Number, Array, Object or token 'null', 'true' or 'false')"}
	}
	return xToken{}, s.unexpected("expected a valid value (JSON String, Number, Array, Object or token 'null', 'true' or 'false')")
}

func (s *xScanner) scanNumber() *xSyntaxError {
	if s.data[s.pos] == '-' {
		s.pos++
		if s.pos >= len(s.data) || s.data[s.pos] < '0' || s.data[s.pos] > '9' {
			if s.pos >= len(s.data) {
				return &xSyntaxError{offset: s.pos, msg: "Unexpected end-of-input in a Number value"}
			}
			r, _ := utf8.DecodeRune(s.data[s.pos:])
			return &xSyntaxError{offset: s.pos, msg: "Unexpected character (" + jacksonCharDesc(r) + ") in numeric value: expected digit (0-9) to follow minus sign, for valid numeric value"}
		}
	}
	digits := func() int {
		n := 0
		for s.pos < len(s.data) && s.data[s.pos] >= '0' && s.data[s.pos] <= '9' {
			s.pos++
			n++
		}
		return n
	}
	intStart := s.pos
	if digits() > 1 && s.data[intStart] == '0' {
		return &xSyntaxError{offset: intStart, msg: "Invalid numeric value: Leading zeroes not allowed"}
	}
	if s.pos < len(s.data) && s.data[s.pos] == '.' {
		s.pos++
		if digits() == 0 {
			if s.pos >= len(s.data) {
				return &xSyntaxError{offset: s.pos, msg: "Unexpected end-of-input in a Number value"}
			}
			r, _ := utf8.DecodeRune(s.data[s.pos:])
			return &xSyntaxError{offset: s.pos, msg: "Unexpected character (" + jacksonCharDesc(r) + ") in numeric value: Decimal point not followed by a digit"}
		}
	}
	if s.pos < len(s.data) && (s.data[s.pos] == 'e' || s.data[s.pos] == 'E') {
		s.pos++
		if s.pos < len(s.data) && (s.data[s.pos] == '+' || s.data[s.pos] == '-') {
			s.pos++
		}
		if digits() == 0 {
			if s.pos >= len(s.data) {
				return &xSyntaxError{offset: s.pos, msg: "Unexpected end-of-input in a Number value"}
			}
			r, _ := utf8.DecodeRune(s.data[s.pos:])
			return &xSyntaxError{offset: s.pos, msg: "Unexpected character (" + jacksonCharDesc(r) + ") in numeric value: Exponent indicator not followed by a digit"}
		}
	}
	return nil
}

func (s *xScanner) scanString() (string, *xSyntaxError) {
	s.pos++ // opening quote
	var b strings.Builder
	for {
		if s.pos >= len(s.data) {
			return "", &xSyntaxError{offset: s.pos, msg: "Unexpected end-of-input in VALUE_STRING"}
		}
		c := s.data[s.pos]
		switch {
		case c == '"':
			s.pos++
			return b.String(), nil
		case c == '\\':
			if s.pos+1 >= len(s.data) {
				return "", &xSyntaxError{offset: s.pos + 1, msg: "Unexpected end-of-input in character escape sequence"}
			}
			e := s.data[s.pos+1]
			s.pos += 2
			switch e {
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				if s.pos+4 > len(s.data) {
					return "", &xSyntaxError{offset: len(s.data), msg: "Unexpected end-of-input in character escape sequence"}
				}
				v, err := strconv.ParseUint(string(s.data[s.pos:s.pos+4]), 16, 32)
				if err != nil {
					return "", &xSyntaxError{offset: s.pos, msg: "Unexpected character in character escape sequence: expected a hex-digit for character escape sequence"}
				}
				s.pos += 4
				r := rune(v)
				if r >= 0xD800 && r < 0xDC00 && s.pos+6 <= len(s.data) && s.data[s.pos] == '\\' && s.data[s.pos+1] == 'u' {
					if lo, err := strconv.ParseUint(string(s.data[s.pos+2:s.pos+6]), 16, 32); err == nil && lo >= 0xDC00 && lo < 0xE000 {
						r = 0x10000 + (r-0xD800)<<10 + (rune(lo) - 0xDC00)
						s.pos += 6
					}
				}
				b.WriteRune(r)
			default:
				return "", &xSyntaxError{offset: s.pos - 1, msg: "Unrecognized character escape " + jacksonCharDesc(rune(e))}
			}
		case c < 0x20:
			return "", &xSyntaxError{offset: s.pos, msg: "Illegal unquoted character (" + jacksonCharDesc(rune(c)) + "): has to be escaped using backslash to be included in string value"}
		default:
			b.WriteByte(c)
			s.pos++
		}
	}
}

// skipValue skips the children of a container token already returned.
func (s *xScanner) skipChildren(t xToken) *xSyntaxError {
	if t.kind != xStartObject && t.kind != xStartArray {
		return nil
	}
	depth := 1
	for depth > 0 {
		nt, err := s.next()
		if err != nil {
			return err
		}
		switch nt.kind {
		case xStartObject, xStartArray:
			depth++
		case xEndObject, xEndArray:
			depth--
		case xEOF:
			return nil
		}
	}
	return nil
}

// orderedObject is a JSON object that keeps its key order (a LinkedHashMap).
type orderedObject struct {
	keys []string
	vals map[string]any
}

func newOrderedObject() *orderedObject { return &orderedObject{vals: map[string]any{}} }

func (o *orderedObject) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// readXValue reads the value starting at token t: objects become
// *orderedObject, arrays []any, numbers json.Number.
func (s *xScanner) readXValue(t xToken) (any, *xSyntaxError) {
	switch t.kind {
	case xStartObject:
		obj := newOrderedObject()
		for {
			ft, err := s.next()
			if err != nil {
				return nil, err
			}
			if ft.kind == xEndObject {
				return obj, nil
			}
			if _, dup := obj.vals[ft.text]; dup {
				return nil, &xSyntaxError{offset: ft.end, msg: "Duplicate Object property \"" + ft.text + "\""}
			}
			vt, err := s.next()
			if err != nil {
				return nil, err
			}
			v, err := s.readXValue(vt)
			if err != nil {
				return nil, err
			}
			obj.set(ft.text, v)
		}
	case xStartArray:
		arr := []any{}
		for {
			et, err := s.next()
			if err != nil {
				return nil, err
			}
			if et.kind == xEndArray {
				return arr, nil
			}
			v, err := s.readXValue(et)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
	case xString:
		return t.text, nil
	case xNumber:
		return json.Number(t.text), nil
	case xTrue:
		return true, nil
	case xFalse:
		return false, nil
	}
	return nil, nil
}

func errNotXContent() *Error {
	return &Error{Type: "not_x_content_exception", Reason: "Compressor detection can only be called on some xcontent bytes or compressed xcontent bytes"}
}

// parseSourceDocument parses a document source like DocumentParser: the
// errors are mapper_parsing_exception "failed to parse" with the parser's
// failure as the cause.
func parseSourceDocument(raw []byte) (*orderedObject, error) {
	failed := func(cause *Error) error {
		return &Error{Status: http.StatusBadRequest, Type: "mapper_parsing_exception", Reason: "failed to parse", Cause: cause}
	}
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, failed(errNotXContent())
	}
	s := newXScanner(raw)
	t, err := s.next()
	if err != nil {
		return nil, failed(err.jacksonError())
	}
	v, err := s.readXValue(t)
	if err != nil {
		return nil, failed(err.jacksonError())
	}
	extra, err := s.next()
	if err != nil {
		return nil, failed(err.jacksonError())
	}
	if extra.kind != xEOF {
		return nil, failed(&Error{Type: "illegal_argument_exception", Reason: "Malformed content, found extra data after parsing: " + extra.kind.String()})
	}
	return v.(*orderedObject), nil
}

// metadataFieldNames are the metadata mappers a document may not contain.
var metadataFieldNames = map[string]bool{
	"_id": true, "_source": true, "_index": true, "_routing": true, "_version": true, "_seq_no": true,
	"_ignored": true, "_field_names": true, "_nested_path": true, "_data_stream_timestamp": true, "_feature": true,
}

// checkMetadataFields rejects metadata fields at the root of a document.
func checkMetadataFields(doc *orderedObject, id string) error {
	for _, k := range doc.keys {
		if k == "_primary_term" {
			// Lucene rejects the doc values of a user field named like the
			// primary term field
			return errIllegalArgument("Inconsistency of field data structures across documents for field [_primary_term] of doc [0]. doc values type: expected 'NUMERIC', but it has 'SORTED_NUMERIC'.")
		}
		if dot := strings.IndexByte(k, '.'); dot > 0 && metadataFieldNames[k[:dot]] {
			return errMapperParsing("Could not dynamically add mapping for field [%s]. Existing mapping for [%s] must be of type object but found [%s].", k, k[:dot], k[:dot])
		}
		if !metadataFieldNames[k] {
			continue
		}
		v := doc.vals[k]
		empty := false
		for {
			arr, ok := v.([]any)
			if !ok {
				break
			}
			if len(arr) == 0 {
				empty = true
				break
			}
			v = arr[0]
		}
		if empty {
			continue
		}
		return &Error{Status: http.StatusBadRequest, Type: "mapper_parsing_exception",
			Reason: "failed to parse field [" + k + "] of type [" + k + "] in document with id '" + id + "'. Preview of field's value: '" + javaToString(v) + "'",
			Cause:  &Error{Type: "mapper_parsing_exception", Reason: "Field [" + k + "] is a metadata field and cannot be added inside a document. Use the index API request parameters."}}
	}
	return nil
}

func isJSONFloatText(s string) bool { return strings.ContainsAny(s, ".eE") }

// javaNumberText renders a JSON number as the Java Number Jackson parses it
// into (Integer/Long/BigInteger or Double).
func javaNumberText(s string) string {
	if isJSONFloatText(s) {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return s
		}
		return javaNumberString(f, 64)
	}
	if n, ok := new(big.Int).SetString(s, 10); ok {
		return n.String()
	}
	return s
}

// javaToString is Object.toString of a parsed JSON value (maps render as
// {k=v, ...}).
func javaToString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return javaNumberText(string(t))
	case *orderedObject:
		parts := make([]string, 0, len(t.keys))
		for _, k := range t.keys {
			parts = append(parts, k+"="+javaToString(t.vals[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, javaToString(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case M:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+javaToString(t[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case float64:
		return javaNumberString(t, 64)
	}
	return fmt.Sprint(v)
}

// javaEqual is Objects.equals on values parsed by Jackson: integers and
// floating point numbers are different types.
func javaEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case json.Number:
		y, ok := b.(json.Number)
		if !ok || isJSONFloatText(string(x)) != isJSONFloatText(string(y)) {
			return false
		}
		if isJSONFloatText(string(x)) {
			fx, err1 := strconv.ParseFloat(string(x), 64)
			fy, err2 := strconv.ParseFloat(string(y), 64)
			return err1 == nil && err2 == nil && fx == fy
		}
		bx, ok1 := new(big.Int).SetString(string(x), 10)
		by, ok2 := new(big.Int).SetString(string(y), 10)
		return ok1 && ok2 && bx.Cmp(by) == 0
	case *orderedObject:
		y, ok := b.(*orderedObject)
		if !ok || len(x.keys) != len(y.keys) {
			return false
		}
		for k, v := range x.vals {
			w, exists := y.vals[k]
			if !exists || !javaEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !javaEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// orderedFromValue converts a decoded JSON value (M with sorted keys) to the
// ordered representation.
func orderedFromValue(v any) any {
	switch t := v.(type) {
	case M:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		obj := newOrderedObject()
		for _, k := range keys {
			obj.set(k, orderedFromValue(t[k]))
		}
		return obj
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = orderedFromValue(e)
		}
		return out
	case float64:
		return json.Number(strconv.FormatFloat(t, 'f', -1, 64))
	case *orderedObject:
		return t
	}
	return v
}

// valueFromOrdered converts ordered values to M trees.
func valueFromOrdered(v any) any {
	switch t := v.(type) {
	case *orderedObject:
		m := make(M, len(t.keys))
		for _, k := range t.keys {
			m[k] = valueFromOrdered(t.vals[k])
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = valueFromOrdered(e)
		}
		return out
	}
	return v
}

// cloneOrdered deep-copies an ordered value.
func cloneOrdered(v any) any {
	switch t := v.(type) {
	case *orderedObject:
		obj := &orderedObject{keys: append([]string(nil), t.keys...), vals: make(map[string]any, len(t.vals))}
		for k, e := range t.vals {
			obj.vals[k] = cloneOrdered(e)
		}
		return obj
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneOrdered(e)
		}
		return out
	}
	return v
}

// appendOrderedJSON writes an ordered value as compact JSON.
func appendOrderedJSON(buf *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case string:
		writeJSONString(buf, t)
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case json.Number:
		buf.WriteString(string(t))
	case *orderedObject:
		buf.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, k)
			buf.WriteByte(':')
			appendOrderedJSON(buf, t.vals[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			appendOrderedJSON(buf, e)
		}
		buf.WriteByte(']')
	case M:
		appendOrderedJSON(buf, orderedFromValue(t))
	default:
		data, _ := json.Marshal(t)
		buf.Write(data)
	}
}

func orderedJSON(v any) []byte {
	var buf bytes.Buffer
	appendOrderedJSON(&buf, v)
	return buf.Bytes()
}

// xcontentUpdate is XContentHelper.update: it merges changes into source
// (objects recursively, new keys appended) and reports whether anything
// changed.
func xcontentUpdate(source, changes *orderedObject, checkUpdatedValues bool) bool {
	modified := false
	for _, k := range changes.keys {
		value := changes.vals[k]
		old, exists := source.vals[k]
		if !exists {
			source.set(k, value)
			modified = true
			continue
		}
		if om, ok := old.(*orderedObject); ok {
			if cm, ok := value.(*orderedObject); ok {
				modified = xcontentUpdate(om, cm, checkUpdatedValues && !modified) || modified
				continue
			}
		}
		source.set(k, value)
		if modified {
			continue
		}
		if !checkUpdatedValues {
			modified = true
			continue
		}
		modified = !javaEqual(old, value)
	}
	return modified
}
