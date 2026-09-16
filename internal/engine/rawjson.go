package engine

import (
	"encoding/json"
	"strings"
)

// Locations in the stored (compact) source, for the Jackson messages that
// report byte offsets or line and column numbers.

type rawJSONWalker struct {
	data    []byte
	pos     int
	onValue func(path []string, token byte, end int)
	onKey   func(parent []string, key string)
}

func (w *rawJSONWalker) skipWS() {
	for w.pos < len(w.data) {
		switch w.data[w.pos] {
		case ' ', '\t', '\n', '\r':
			w.pos++
		default:
			return
		}
	}
}

func (w *rawJSONWalker) readString() (string, bool) {
	if w.pos >= len(w.data) || w.data[w.pos] != '"' {
		return "", false
	}
	start := w.pos
	w.pos++
	escaped := false
	for w.pos < len(w.data) {
		c := w.data[w.pos]
		w.pos++
		if c == '\\' {
			escaped = true
			w.pos++
			continue
		}
		if c == '"' {
			tok := w.data[start:w.pos]
			if !escaped {
				return string(tok[1 : len(tok)-1]), true
			}
			var s string
			if err := json.Unmarshal(tok, &s); err != nil {
				return "", false
			}
			return s, true
		}
	}
	return "", false
}

func (w *rawJSONWalker) value(path []string) bool {
	w.skipWS()
	if w.pos >= len(w.data) {
		return false
	}
	switch c := w.data[w.pos]; c {
	case '{':
		w.pos++
		if w.onValue != nil {
			w.onValue(path, '{', w.pos)
		}
		for {
			w.skipWS()
			if w.pos < len(w.data) && w.data[w.pos] == '}' {
				w.pos++
				return true
			}
			key, ok := w.readString()
			if !ok {
				return false
			}
			if w.onKey != nil {
				w.onKey(path, key)
			}
			w.skipWS()
			if w.pos >= len(w.data) || w.data[w.pos] != ':' {
				return false
			}
			w.pos++
			child := append(append([]string(nil), path...), splitJavaPath(key)...)
			if !w.value(child) {
				return false
			}
			w.skipWS()
			if w.pos < len(w.data) && w.data[w.pos] == ',' {
				w.pos++
			}
		}
	case '[':
		w.pos++
		for {
			w.skipWS()
			if w.pos < len(w.data) && w.data[w.pos] == ']' {
				w.pos++
				if w.onValue != nil {
					w.onValue(path, ']', w.pos)
				}
				return true
			}
			if !w.value(path) {
				return false
			}
			w.skipWS()
			if w.pos < len(w.data) && w.data[w.pos] == ',' {
				w.pos++
			}
		}
	case '"':
		if _, ok := w.readString(); !ok {
			return false
		}
		if w.onValue != nil {
			w.onValue(path, '"', w.pos)
		}
		return true
	default:
		start := w.pos
		for w.pos < len(w.data) && !strings.ContainsRune(",]} \t\r\n", rune(w.data[w.pos])) {
			w.pos++
		}
		if w.pos == start {
			return false
		}
		if w.onValue != nil {
			w.onValue(path, c, w.pos)
		}
		return true
	}
}

// jacksonLineCol returns Jackson's 1-based line and column of a byte offset
// (CR, LF and CRLF end lines; columns count bytes).
func jacksonLineCol(data []byte, offset int) (line, col int) {
	line, start := 1, 0
	for i := 0; i < offset && i < len(data); i++ {
		switch data[i] {
		case '\n':
			line, start = line+1, i+1
		case '\r':
			if i+1 < len(data) && data[i+1] == '\n' {
				i++
			}
			line, start = line+1, i+1
		}
	}
	return line, offset - start + 1
}

// splitJavaPath splits a dotted key like String.split("\\."): trailing empty
// parts are dropped.
func splitJavaPath(key string) []string {
	if !strings.Contains(key, ".") {
		return []string{key}
	}
	parts := strings.Split(key, ".")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// rawTokenEnd returns the end offset of the n-th value token (objects:
// after the opening brace; the closing bracket of arrays is reported with
// token ']') found at a field path, or -1.
func rawTokenEnd(raw []byte, path string, n int, wantClose bool) int {
	result := -1
	count := 0
	w := &rawJSONWalker{data: raw}
	w.onValue = func(p []string, token byte, end int) {
		if result >= 0 || strings.Join(p, ".") != path {
			return
		}
		if (token == ']') != wantClose {
			return
		}
		if count == n {
			result = end
		}
		count++
	}
	w.value(nil)
	return result
}

// rawDottedKey returns the dotted key below name held by the object at
// parent ({"k.sub": ...} rather than {"k": {"sub": ...}}), or "".
func rawDottedKey(raw []byte, parent, name string) string {
	found := ""
	w := &rawJSONWalker{data: raw}
	w.onKey = func(p []string, key string) {
		if found == "" && strings.Join(p, ".") == parent && strings.HasPrefix(key, name+".") {
			found = key
		}
	}
	w.value(nil)
	return found
}
