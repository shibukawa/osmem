package engine

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Double is a Java double in a response. OpenSearch writes doubles with
// Double.toString, so whole numbers keep a fraction (1.0) and small or large
// magnitudes use an exponent (1.0E-4, 1.2345678901234567E19).
type Double float64

// Float is a Java float in a response (Lucene scores, sort values of float
// and half_float fields), written with Float.toString.
type Float float32

func (d Double) MarshalJSON() ([]byte, error) { return appendJavaNumber(nil, float64(d), 64), nil }

func (f Float) MarshalJSON() ([]byte, error) { return appendJavaNumber(nil, float64(f), 32), nil }

// appendJavaNumber writes v like Java's Double.toString (bitSize 64) or
// Float.toString (bitSize 32). Jackson quotes the non-finite values.
func appendJavaNumber(dst []byte, v float64, bitSize int) []byte {
	switch {
	case math.IsNaN(v):
		return append(dst, `"NaN"`...)
	case math.IsInf(v, 1):
		return append(dst, `"Infinity"`...)
	case math.IsInf(v, -1):
		return append(dst, `"-Infinity"`...)
	}
	return append(dst, javaNumberString(v, bitSize)...)
}

// javaNumberString renders the shortest representation of a finite value
// with Java's layout: plain notation for magnitudes in [1e-3, 1e7), otherwise
// computerized scientific notation, always with a fraction digit.
func javaNumberString(v float64, bitSize int) string {
	if v == 0 {
		if math.Signbit(v) {
			return "-0.0"
		}
		return "0.0"
	}
	s := strconv.FormatFloat(v, 'e', -1, bitSize)
	var b strings.Builder
	if s[0] == '-' {
		b.WriteByte('-')
		s = s[1:]
	}
	e := strings.IndexByte(s, 'e')
	exp, _ := strconv.Atoi(s[e+1:])
	digits := strings.Replace(s[:e], ".", "", 1)
	if abs := math.Abs(v); abs >= 1e-3 && abs < 1e7 {
		point := exp + 1
		switch {
		case point <= 0:
			b.WriteString("0.")
			b.WriteString(strings.Repeat("0", -point))
			b.WriteString(digits)
		case point >= len(digits):
			b.WriteString(digits)
			b.WriteString(strings.Repeat("0", point-len(digits)))
			b.WriteString(".0")
		default:
			b.WriteString(digits[:point])
			b.WriteByte('.')
			b.WriteString(digits[point:])
		}
		return b.String()
	}
	b.WriteString(digits[:1])
	b.WriteByte('.')
	if len(digits) > 1 {
		b.WriteString(digits[1:])
	} else {
		b.WriteByte('0')
	}
	b.WriteByte('E')
	b.WriteString(strconv.Itoa(exp))
	return b.String()
}

// EncodeJSON renders a response body the way OpenSearch writes it: float64
// values are Java doubles and float32 values Java floats, json.Number and
// json.RawMessage (stored sources) keep their bytes, and pretty output uses
// Jackson's layout (" : " separators, two-space indentation, "{ }" for empty
// containers, a final newline). Object keys are sorted.
func EncodeJSON(v any, pretty bool) ([]byte, error) {
	e := &jsonEncoder{pretty: pretty}
	if err := e.value(v, 0); err != nil {
		return nil, err
	}
	if pretty {
		e.buf.WriteByte('\n')
	}
	return e.buf.Bytes(), nil
}

type jsonEncoder struct {
	buf    bytes.Buffer
	pretty bool
}

func (e *jsonEncoder) newline(depth int) {
	if !e.pretty {
		return
	}
	e.buf.WriteByte('\n')
	for i := 0; i < depth; i++ {
		e.buf.WriteString("  ")
	}
}

func (e *jsonEncoder) value(v any, depth int) error {
	switch t := v.(type) {
	case nil:
		e.buf.WriteString("null")
	case string:
		writeJSONString(&e.buf, t)
	case bool:
		e.buf.WriteString(strconv.FormatBool(t))
	case float64:
		e.buf.Write(appendJavaNumber(nil, t, 64))
	case float32:
		e.buf.Write(appendJavaNumber(nil, float64(t), 32))
	case Double:
		e.buf.Write(appendJavaNumber(nil, float64(t), 64))
	case Float:
		e.buf.Write(appendJavaNumber(nil, float64(t), 32))
	case int:
		e.buf.WriteString(strconv.Itoa(t))
	case int32:
		e.buf.WriteString(strconv.FormatInt(int64(t), 10))
	case int64:
		e.buf.WriteString(strconv.FormatInt(t, 10))
	case uint64:
		e.buf.WriteString(strconv.FormatUint(t, 10))
	case json.Number:
		if t == "" {
			e.buf.WriteByte('0')
		} else {
			e.buf.WriteString(string(t))
		}
	case json.RawMessage:
		// OpenSearch copies stored sources as they are, even when pretty
		if t == nil {
			e.buf.WriteString("null")
		} else {
			e.buf.Write(t)
		}
	case M:
		if t == nil {
			e.buf.WriteString("null")
			return nil
		}
		return e.object(t, depth)
	case []any:
		if t == nil {
			e.buf.WriteString("null")
			return nil
		}
		return e.array(len(t), func(i int) any { return t[i] }, depth)
	case []string:
		if t == nil {
			e.buf.WriteString("null")
			return nil
		}
		return e.array(len(t), func(i int) any { return t[i] }, depth)
	default:
		return e.reflectValue(v, depth)
	}
	return nil
}

func (e *jsonEncoder) object(m M, depth int) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	e.buf.WriteByte('{')
	if len(keys) == 0 {
		if e.pretty {
			e.buf.WriteByte(' ')
		}
		e.buf.WriteByte('}')
		return nil
	}
	for i, k := range keys {
		if i > 0 {
			e.buf.WriteByte(',')
		}
		e.newline(depth + 1)
		writeJSONString(&e.buf, k)
		if e.pretty {
			e.buf.WriteString(" : ")
		} else {
			e.buf.WriteByte(':')
		}
		if err := e.value(m[k], depth+1); err != nil {
			return err
		}
	}
	e.newline(depth)
	e.buf.WriteByte('}')
	return nil
}

func (e *jsonEncoder) array(n int, at func(int) any, depth int) error {
	e.buf.WriteByte('[')
	if n == 0 {
		if e.pretty {
			e.buf.WriteByte(' ')
		}
		e.buf.WriteByte(']')
		return nil
	}
	for i := 0; i < n; i++ {
		if i > 0 {
			e.buf.WriteByte(',')
		}
		e.newline(depth + 1)
		if err := e.value(at(i), depth+1); err != nil {
			return err
		}
	}
	e.newline(depth)
	e.buf.WriteByte(']')
	return nil
}

func (e *jsonEncoder) reflectValue(v any, depth int) error {
	if m, ok := v.(json.Marshaler); ok {
		data, err := m.MarshalJSON()
		if err != nil {
			return err
		}
		return e.value(json.RawMessage(data), depth)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			if rv.IsNil() {
				e.buf.WriteString("null")
				return nil
			}
			m := make(M, rv.Len())
			iter := rv.MapRange()
			for iter.Next() {
				m[iter.Key().String()] = iter.Value().Interface()
			}
			return e.object(m, depth)
		}
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			break
		}
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			e.buf.WriteString("null")
			return nil
		}
		return e.array(rv.Len(), func(i int) any { return rv.Index(i).Interface() }, depth)
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			e.buf.WriteString("null")
			return nil
		}
		return e.value(rv.Elem().Interface(), depth)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.buf.WriteString(strconv.FormatInt(rv.Int(), 10))
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		e.buf.WriteString(strconv.FormatUint(rv.Uint(), 10))
		return nil
	case reflect.String:
		writeJSONString(&e.buf, rv.String())
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return e.value(json.RawMessage(data), depth)
}

// writeJSONString escapes like Jackson: quotes, backslashes and control
// characters (short escapes where JSON has them, otherwise upper-case
// \u00XX); invalid UTF-8 becomes the replacement character.
func writeJSONString(buf *bytes.Buffer, s string) {
	const hex = "0123456789ABCDEF"
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"':
				buf.WriteString(`\"`)
			case c == '\\':
				buf.WriteString(`\\`)
			case c == '\n':
				buf.WriteString(`\n`)
			case c == '\r':
				buf.WriteString(`\r`)
			case c == '\t':
				buf.WriteString(`\t`)
			case c == '\b':
				buf.WriteString(`\b`)
			case c == '\f':
				buf.WriteString(`\f`)
			case c < 0x20:
				buf.WriteString(`\`)
				buf.WriteString("u00")
				buf.WriteByte(hex[c>>4])
				buf.WriteByte(hex[c&0xF])
			default:
				buf.WriteByte(c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			buf.WriteRune(utf8.RuneError)
		} else {
			buf.WriteString(s[i : i+size])
		}
		i += size
	}
	buf.WriteByte('"')
}
