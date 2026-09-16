package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

const exactNumericFieldPrefix = "_osmem_exact_numeric."

func exactNumericField(field string) string { return exactNumericFieldPrefix + field }

// Params are URL query parameters of a request.
type Params map[string]string

func (p Params) Get(name string) string { return p[name] }

func (p Params) Has(name string) bool {
	_, ok := p[name]
	return ok
}

// Bool returns the parameter as a boolean; a bare flag ("?pretty") counts as true.
func (p Params) Bool(name string, def bool) bool {
	v, ok := p[name]
	if !ok {
		return def
	}
	if v == "" {
		return true
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func (p Params) Int(name string, def int) int {
	v, ok := p[name]
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Response is the outcome of an engine operation.
type Response struct {
	Status int
	Body   any
}

func ok(body any) (Response, error) { return Response{Status: 200, Body: body}, nil }

func fail(err error) (Response, error) { return Response{}, err }

// M is a JSON object.
type M = map[string]any

// helpers for reading loosely typed JSON

func getMap(m M, key string) M {
	if m == nil {
		return nil
	}
	v, _ := m[key].(M)
	return v
}

func getString(m M, key string) string {
	if m == nil {
		return ""
	}
	return stringOf(m[key])
}

// stringOf renders a scalar as a string ("" for anything else).
func stringOf(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func getBool(m M, key string, def bool) bool {
	if m == nil {
		return def
	}
	return boolOf(m[key], def)
}

// boolOf reads a bool or its string form; def for anything else.
func boolOf(v any, def bool) bool {
	switch v := v.(type) {
	case bool:
		return v
	case string:
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

func getInt(m M, key string, def int) int {
	if m == nil {
		return def
	}
	if f, ok := toFloat(m[key]); ok {
		return javaInt(f)
	}
	return def
}

func getFloat(m M, key string, def float64) float64 {
	if m == nil {
		return def
	}
	if f, ok := toFloat(m[key]); ok {
		return f
	}
	return def
}

func getStrings(m M, key string) []string {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func getList(v any) []any {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		return t
	default:
		return []any{t}
	}
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case Double:
		return float64(t), true
	case Float:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case int32:
		return float64(t), true
	case uint64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// decodeJSON parses JSON into loosely typed values keeping numbers as
// json.Number. Like Jackson with strict duplicate detection, a repeated
// object key is an error (*DuplicateKeyError).
func decodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dup := findDuplicateKey(data); dup != nil {
		return dup
	}
	return nil
}

func decodeObject(data []byte) (M, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return M{}, nil
	}
	var m M
	if err := decodeJSON(data, &m); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			if tok := FirstJSONToken(data, 0); tok.Name != "" && tok.Name != "START_OBJECT" {
				return nil, &Error{Status: 400, Type: "parsing_exception", Reason: "Expected [START_OBJECT] but found [" + tok.Name + "]",
					Extra: map[string]any{"line": tok.Line, "col": tok.Col}}
			}
			return nil, errParsing("%s", err.Error())
		}
		if cause := JSONParseCause(data, err); cause != nil {
			return nil, cause
		}
		return nil, errJSONParse(data, err)
	}
	if m == nil {
		m = M{}
	}
	return m, nil
}

// splitList splits a comma separated list, trimming blanks.
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// DecodeObject parses a JSON object body (exported for the HTTP layer).
func DecodeObject(data []byte) (M, error) { return decodeObject(data) }

// DocParamsFrom parses document write parameters (exported for the HTTP layer).
func DocParamsFrom(p Params) (DocParams, error) { return docParamsFrom(p) }

// WildcardMatch is wildcardMatch (exported for the HTTP layer).
func WildcardMatch(pattern, s string) bool { return wildcardMatch(pattern, s) }
