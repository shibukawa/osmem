package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"unsafe"
)

// Request body locations of parse errors.
//
// OpenSearch reports where its parser was in the request body when parsing
// failed: an XContentParseException (x_content_parse_exception,
// named_object_not_found_exception) starts its message with "[line:col] "
// and a ParsingException (parsing_exception) carries line and col metadata.
// The location is the start of the token the parser was on, with a 1-based
// line and a 1-based column counting UTF-8 bytes.
//
// osmem parses decoded bodies, so its parsers name that token through the
// decoded values (a tokenRef: the key or value of an object member, an array
// element, or the start or end of an object or array) and LocateError
// resolves the names against the raw body the values were decoded from.
// Exceptions without a location in OpenSearch get no tokenRef; a token that
// cannot be found (the values were copied or rebuilt) leaves the error
// without a location.

// tokenPart selects a token relative to a decoded object or array.
type tokenPart uint8

const (
	tokKey      tokenPart = iota + 1 // the FIELD_NAME of an object member
	tokValue                         // the first token of a member's or element's value
	tokValueEnd                      // the last token of that value (END_OBJECT or END_ARRAY of a container)
	tokStart                         // the START_OBJECT or START_ARRAY of the object or array itself
	tokEnd                           // the END_OBJECT or END_ARRAY of the object or array itself
)

// tokenRef names a token of a request body.
type tokenRef struct {
	in   any    // the decoded object (M) or array ([]any) the token belongs to
	key  string // the member (objects)
	idx  int    // the element (arrays)
	part tokenPart
	// pick, when set, chooses the member instead of key from the names of
	// the object's members in document order: the member at which a parser
	// that reads the object in order detects a conflict.
	pick func(order []string) (string, bool)
	// via, with pick, names a token inside the chosen member's value
	via func(member string) *tokenRef
}

// keyTok is the name of member key of m.
func keyTok(m M, key string) *tokenRef { return &tokenRef{in: m, key: key, part: tokKey} }

// valueTok is the first token of the value of member key of m.
func valueTok(m M, key string) *tokenRef { return &tokenRef{in: m, key: key, part: tokValue} }

// valueEndTok is the last token of the value of member key of m: the value
// itself, or the END_OBJECT or END_ARRAY that closes it.
func valueEndTok(m M, key string) *tokenRef { return &tokenRef{in: m, key: key, part: tokValueEnd} }

// elemTok is the first token of element i of a.
func elemTok(a []any, i int) *tokenRef { return &tokenRef{in: a, idx: i, part: tokValue} }

// endTok is the END_OBJECT or END_ARRAY of v (an M or a []any).
func endTok(v any) *tokenRef { return &tokenRef{in: v, part: tokEnd} }

// nthValueTok is the value of the nth (1-based) member of m in document
// order among keys, or among all members when keys is empty.
func nthValueTok(m M, n int, keys ...string) *tokenRef {
	return &tokenRef{in: m, part: tokValue, pick: nthMember(n, keys)}
}

// nthKeyTok is the name of the nth member of m in document order among keys.
func nthKeyTok(m M, n int, keys ...string) *tokenRef {
	return &tokenRef{in: m, part: tokKey, pick: nthMember(n, keys)}
}

// viaNthTok is the token via names inside the value of the nth member of m
// in document order (via returns nil when there is none).
func viaNthTok(m M, n int, via func(member string) *tokenRef) *tokenRef {
	return &tokenRef{in: m, part: tokValue, pick: nthMember(n, nil), via: via}
}

// pickValueTok is the value of the member of m that pick chooses from the
// member names in document order.
func pickValueTok(m M, pick func(order []string) (string, bool)) *tokenRef {
	return &tokenRef{in: m, part: tokValue, pick: pick}
}

func nthMember(n int, keys []string) func([]string) (string, bool) {
	return func(order []string) (string, bool) {
		seen := 0
		for _, name := range order {
			if len(keys) > 0 && !slices.Contains(keys, name) {
				continue
			}
			if seen++; seen == n {
				return name, true
			}
		}
		return "", false
	}
}

// firstInsideTok is the token after the START_ARRAY of the array value of
// member key: its first element, or the END_ARRAY of an empty array.
func firstInsideTok(m M, key string) *tokenRef {
	if a, ok := m[key].([]any); ok && len(a) > 0 {
		return elemTok(a, 0)
	}
	return valueEndTok(m, key)
}

// memberElemTok is element i of the array value of member key, or the value
// itself when it is not an array (a single value where a parser accepts an
// array).
func memberElemTok(m M, key string, i int) *tokenRef {
	if a, ok := m[key].([]any); ok {
		return elemTok(a, i)
	}
	return valueTok(m, key)
}

// noTok is a token osmem cannot name: an exception at it, and a wrapper
// located where the parser stopped inside it, get no location.
var noTok = &tokenRef{}

// errPosition is the request body location of an exception.
type errPosition struct {
	at     *tokenRef // the token the exception reports
	parser *tokenRef // the token the parser was on, when it is not at
	frame  *compactFrame
	// causePrefix copies the location prefix of the cause's message (a
	// ParsingException created from the message of a located
	// XContentParseException); format renders the location into the reason
	// instead ("... empty clause found at [%s]").
	causePrefix bool
	format      string
	prefix      string // the "[line:col] " prefix LocateError added
}

func (e *Error) position() *errPosition {
	if e.pos == nil {
		e.pos = &errPosition{}
	}
	return e.pos
}

// at sets the token the exception reports, which is also the token the
// parser is on.
func (e *Error) at(t *tokenRef) *Error {
	e.position().at = t
	return e
}

// atParser sets the token the parser is on when the exception propagates,
// for exceptions that report no location or another token (ObjectParser
// reports an unknown field at its name while the parser is on its value).
func (e *Error) atParser(t *tokenRef) *Error {
	e.position().parser = t
	return e
}

// atInReason locates an exception whose Java message includes the location:
// format has one %s for "line:col".
func (e *Error) atInReason(t *tokenRef, format string) *Error {
	p := e.position()
	p.at, p.format = t, format
	return e
}

// parserTok is the token the parser was on when e was raised.
func (e *Error) parserTok() *tokenRef {
	if e == nil || e.pos == nil {
		return nil
	}
	if e.pos.parser != nil {
		return e.pos.parser
	}
	return e.pos.at
}

// atCause locates a wrapping exception ("[x] failed to parse field [f]")
// where the parser stopped inside the field: the token of its cause, or def
// when the cause does not know it.
func (e *Error) atCause(def *tokenRef) *Error {
	if t := e.Cause.parserTok(); t != nil {
		p := e.position()
		p.at, p.frame = t, e.Cause.pos.frame
		return e
	}
	if def != nil {
		e.position().at = def
	}
	return e
}

// withCauseLocation makes e render the location prefix of its cause's
// message: a ParsingException created from the message of a located
// XContentParseException.
func (e *Error) withCauseLocation() *Error {
	e.position().causePrefix = true
	return e
}

// each calls fn for e and every exception it wraps.
func (e *Error) each(fn func(*Error)) {
	if e == nil {
		return
	}
	fn(e)
	e.Cause.each(fn)
	if e.failure != nil {
		e.failure.cause.each(fn)
	}
	for _, f := range e.more {
		f.cause.each(fn)
	}
}

// hasLocations reports whether e or one of its causes names a token.
func (e *Error) hasLocations() bool {
	found := false
	e.each(func(x *Error) {
		found = found || x.pos != nil && x.pos.at != nil
	})
	return found
}

// compactFrame is a part of a request body that OpenSearch parses again
// after writing it out as compact JSON: the object root with its members in
// document order, but without the members drop. Delete and update by query
// bodies, reindex sources and alias filters are parsed that way, so the
// locations of their parse errors are columns of that single line. copy is
// a shallow copy of root that osmem's parser was given instead, or nil.
type compactFrame struct {
	root, copy M
	drop       []string
}

func newCompactFrame(root, copy M, drop ...string) *compactFrame {
	return &compactFrame{root: root, copy: copy, drop: drop}
}

// mark places the located exceptions of err in the frame.
func (f *compactFrame) mark(err error) {
	e, ok := err.(*Error)
	if f == nil || !ok {
		return
	}
	e.each(func(x *Error) {
		if p := x.pos; p != nil && p.frame == nil && (p.at != nil || p.parser != nil) {
			p.frame = f
		}
	})
}

// LocateError adds OpenSearch's parser locations to the exceptions of err
// that name tokens of root, the value decoded from data. Exceptions whose
// tokens are not found keep their names, so the error can be located
// against another body later.
func LocateError(err error, data []byte, root any) {
	e, ok := err.(*Error)
	if !ok || !e.hasLocations() {
		return
	}
	ix := newTokenIndex(data, root)
	ix.apply(e)
}

// locatedTypes are the exceptions that render a location, and how: a
// reason prefix (true) or line and col metadata (false).
var locatedTypes = map[string]bool{
	"x_content_parse_exception":        true,
	"named_object_not_found_exception": true,
	"parsing_exception":                false,
}

func (ix *tokenIndex) apply(e *Error) {
	if e == nil {
		return
	}
	ix.apply(e.Cause)
	if e.failure != nil {
		ix.apply(e.failure.cause)
	}
	for _, f := range e.more {
		ix.apply(f.cause)
	}
	p := e.pos
	if p == nil {
		return
	}
	if p.causePrefix && p.prefix == "" && e.Cause != nil && e.Cause.pos != nil && e.Cause.pos.prefix != "" {
		p.prefix = e.Cause.pos.prefix
		e.Reason = p.prefix + e.Reason
	}
	if p.at == nil {
		return
	}
	line, col, found := ix.resolve(p.at, p.frame)
	if !found {
		return
	}
	p.at, p.parser = nil, nil
	if p.format != "" {
		e.Reason = fmt.Sprintf(p.format, fmt.Sprintf("%d:%d", line, col))
		return
	}
	prefix, located := locatedTypes[e.Type]
	switch {
	case !located:
	case prefix:
		if p.prefix == "" {
			p.prefix = fmt.Sprintf("[%d:%d] ", line, col)
			e.Reason = p.prefix + e.Reason
		}
	default:
		if e.Extra == nil {
			e.Extra = map[string]any{}
		}
		e.Extra["line"] = line
		e.Extra["col"] = col
	}
}

// token index ----------------------------------------------------------------

type objectPos struct {
	start, end int
	members    map[string]memberPos
	order      []string
}

type memberPos struct {
	key, value, valueEnd int
}

type arrayPos struct {
	start, end int
	elems      []memberPos // key unused
}

// arrayID identifies a decoded array: its backing array and length (empty
// arrays share storage and are not indexed).
type arrayID struct {
	data unsafe.Pointer
	n    int
}

// tokenIndex maps the objects and arrays of a decoded body to the offsets
// of their tokens in the raw body.
type tokenIndex struct {
	data       []byte
	objects    map[unsafe.Pointer]*objectPos
	arrays     map[arrayID]*arrayPos
	lineStarts []int
	frames     map[*compactFrame]map[int]int // raw token offset -> compact column - 1
}

func newTokenIndex(data []byte, root any) *tokenIndex {
	ix := &tokenIndex{data: data, objects: map[unsafe.Pointer]*objectPos{}, arrays: map[arrayID]*arrayPos{},
		frames: map[*compactFrame]map[int]int{}}
	func() {
		// the body was decoded from data, so it is valid JSON; stop quietly
		// if it is not
		defer func() { _ = recover() }()
		ix.walk(0, root)
	}()
	// Jackson counts CR, LF and CRLF as line breaks
	ix.lineStarts = []int{0}
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			ix.lineStarts = append(ix.lineStarts, i+1)
		case '\r':
			if i+1 < len(data) && data[i+1] == '\n' {
				i++
			}
			ix.lineStarts = append(ix.lineStarts, i+1)
		}
	}
	return ix
}

func mapPointer(m M) unsafe.Pointer { return reflect.ValueOf(m).UnsafePointer() }

func sliceID(a []any) arrayID { return arrayID{unsafe.Pointer(unsafe.SliceData(a)), len(a)} }

func (ix *tokenIndex) skipSpace(i int) int {
	for i < len(ix.data) {
		switch ix.data[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// keyName decodes the member name spanning data[start:end].
func (ix *tokenIndex) keyName(start, end int) string {
	raw := ix.data[start:end]
	if slices.Contains(raw, '\\') {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw[1 : len(raw)-1])
}

// walk indexes the value starting at or after offset i, which v was decoded
// from, and returns the offsets of its first and last tokens and the offset
// after it.
func (ix *tokenIndex) walk(i int, v any) (start, last, next int) {
	data := ix.data
	i = ix.skipSpace(i)
	start = i
	switch data[i] {
	case '{':
		m, _ := v.(M)
		var op *objectPos
		if m != nil {
			op = &objectPos{start: i, members: make(map[string]memberPos, len(m))}
			ix.objects[mapPointer(m)] = op
		}
		i++
		for {
			i = ix.skipSpace(i)
			switch data[i] {
			case '}':
				if op != nil {
					op.end = i
				}
				return start, i, i + 1
			case ',':
				i++
				continue
			}
			keyStart, keyEnd := i, ix.stringEnd(i)
			name := ix.keyName(keyStart, keyEnd)
			i = ix.skipSpace(keyEnd) + 1 // the colon
			var child any
			if m != nil {
				child = m[name]
			}
			vs, vl, vn := ix.walk(i, child)
			if op != nil {
				if _, dup := op.members[name]; !dup {
					op.order = append(op.order, name)
				}
				op.members[name] = memberPos{key: keyStart, value: vs, valueEnd: vl}
			}
			i = vn
		}
	case '[':
		a, _ := v.([]any)
		var ap *arrayPos
		if len(a) > 0 {
			ap = &arrayPos{start: i}
			ix.arrays[sliceID(a)] = ap
		}
		i++
		for n := 0; ; {
			i = ix.skipSpace(i)
			switch data[i] {
			case ']':
				if ap != nil {
					ap.end = i
				}
				return start, i, i + 1
			case ',':
				i++
				continue
			}
			var child any
			if n < len(a) {
				child = a[n]
			}
			vs, vl, vn := ix.walk(i, child)
			if ap != nil {
				ap.elems = append(ap.elems, memberPos{value: vs, valueEnd: vl})
			}
			n++
			i = vn
		}
	case '"':
		return start, start, ix.stringEnd(i)
	}
	return start, start, ix.scalarEnd(i)
}

// stringEnd returns the offset after the string starting at offset i.
func (ix *tokenIndex) stringEnd(i int) int {
	data := ix.data
	for j := i + 1; j < len(data); j++ {
		switch data[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return len(data)
}

// scalarEnd returns the offset after the number or literal at offset i.
func (ix *tokenIndex) scalarEnd(i int) int {
	for ; i < len(ix.data); i++ {
		switch ix.data[i] {
		case ' ', '\t', '\n', '\r', ',', ':', ']', '}':
			return i
		}
	}
	return i
}

// resolve returns the location of a token (in the compact rendering of
// frame when it is set).
func (ix *tokenIndex) resolve(t *tokenRef, frame *compactFrame) (line, col int, found bool) {
	in := t.in
	if frame != nil && frame.copy != nil {
		if m, ok := in.(M); ok && m != nil && mapPointer(m) == mapPointer(frame.copy) {
			in = frame.root
		}
	}
	offset := ix.offset(t, in)
	if offset < 0 {
		return 0, 0, false
	}
	if frame == nil {
		line, col = ix.lineCol(offset)
		return line, col, true
	}
	c, ok := ix.compactColumns(frame)[offset]
	if !ok {
		return 0, 0, false
	}
	return 1, c + 1, true
}

// offset returns the raw offset of token t of in, or -1.
func (ix *tokenIndex) offset(t *tokenRef, in any) int {
	switch in := in.(type) {
	case M:
		op := ix.objects[mapPointer(in)]
		if in == nil || op == nil {
			return -1
		}
		switch t.part {
		case tokStart:
			return op.start
		case tokEnd:
			return op.end
		}
		key := t.key
		if t.pick != nil {
			var ok bool
			if key, ok = t.pick(op.order); !ok {
				return -1
			}
			if t.via != nil {
				sub := t.via(key)
				if sub == nil {
					return -1
				}
				return ix.offset(sub, sub.in)
			}
		}
		mp, ok := op.members[key]
		if !ok {
			return -1
		}
		switch t.part {
		case tokKey:
			return mp.key
		case tokValue:
			return mp.value
		case tokValueEnd:
			return mp.valueEnd
		}
	case []any:
		ap := ix.arrays[sliceID(in)]
		if len(in) == 0 || ap == nil {
			return -1
		}
		switch t.part {
		case tokStart:
			return ap.start
		case tokEnd:
			return ap.end
		case tokValue, tokValueEnd:
			if t.idx < 0 || t.idx >= len(ap.elems) {
				return -1
			}
			if t.part == tokValueEnd {
				return ap.elems[t.idx].valueEnd
			}
			return ap.elems[t.idx].value
		}
	}
	return -1
}

func (ix *tokenIndex) lineCol(offset int) (int, int) {
	lo, hi := 0, len(ix.lineStarts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if ix.lineStarts[mid] <= offset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo + 1, offset - ix.lineStarts[lo] + 1
}

// compactColumns maps the raw offsets of the tokens of a frame to their
// offsets in its compact rendering. Strings and numbers are taken to be
// written as they were read.
func (ix *tokenIndex) compactColumns(f *compactFrame) map[int]int {
	if cols, ok := ix.frames[f]; ok {
		return cols
	}
	cols := map[int]int{}
	ix.frames[f] = cols
	if op := ix.objects[mapPointer(f.root)]; f.root != nil && op != nil {
		func() {
			defer func() { _ = recover() }()
			out := 0
			ix.compactValue(op.start, f.drop, cols, &out)
		}()
	}
	return cols
}

// compactValue renders the value at offset i as compact JSON without the
// members drop, recording the output offset of each token, and returns the
// offset after the value.
func (ix *tokenIndex) compactValue(i int, drop []string, cols map[int]int, out *int) int {
	data := ix.data
	i = ix.skipSpace(i)
	switch data[i] {
	case '{', '[':
		object := data[i] == '{'
		closing := byte(']')
		if object {
			closing = '}'
		}
		cols[i] = *out
		*out++
		i++
		for n := 0; ; {
			i = ix.skipSpace(i)
			switch data[i] {
			case closing:
				cols[i] = *out
				*out++
				return i + 1
			case ',':
				i++
				continue
			}
			if object {
				keyStart, keyEnd := i, ix.stringEnd(i)
				i = ix.skipSpace(keyEnd) + 1
				if slices.Contains(drop, ix.keyName(keyStart, keyEnd)) {
					_, _, i = ix.walk(i, nil)
					continue
				}
				if n > 0 {
					*out++ // the comma
				}
				cols[keyStart] = *out
				*out += keyEnd - keyStart + 1 // the name and the colon
			} else if n > 0 {
				*out++
			}
			n++
			i = ix.compactValue(i, nil, cols, out)
		}
	case '"':
		end := ix.stringEnd(i)
		cols[i] = *out
		*out += end - i
		return end
	}
	end := ix.scalarEnd(i)
	cols[i] = *out
	*out += end - i
	return end
}
