package engine

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
)

const exactNumericFieldPrefix = "_osmem_exact_numeric."

func exactNumericField(field string) string { return exactNumericFieldPrefix + field }

// integralString preserves integer JSON values that cannot be represented by
// float64. Non-integral numeric inputs are truncated toward zero, matching
// the default coercion behavior of integral OpenSearch fields.
func integralString(v any) (string, bool) {
	var s string
	switch n := v.(type) {
	case json.Number:
		s = n.String()
	case string:
		s = strings.TrimSpace(n)
	case int:
		return strconv.Itoa(n), true
	case int64:
		return strconv.FormatInt(n, 10), true
	case int32:
		return strconv.FormatInt(int64(n), 10), true
	case uint64:
		return strconv.FormatUint(n, 10), true
	case float64:
		s = strconv.FormatFloat(n, 'f', -1, 64)
	case float32:
		s = strconv.FormatFloat(float64(n), 'f', -1, 32)
	default:
		return "", false
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", false
	}
	return new(big.Int).Quo(r.Num(), r.Denom()).String(), true
}

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
	switch v := m[key].(type) {
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
	switch v := m[key].(type) {
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
		return int(f)
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

// numberValue normalizes a numeric JSON value for output: integral values stay
// integral (int64), other values become float64.
func numberValue(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	case float64:
		if t == float64(int64(t)) && t < 1e15 && t > -1e15 {
			return int64(t)
		}
		return t
	case int:
		return int64(t)
	case int64:
		return t
	}
	return v
}

// decodeJSON parses JSON into loosely typed values keeping numbers as json.Number.
func decodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func decodeObject(data []byte) (M, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return M{}, nil
	}
	var m M
	if err := decodeJSON(data, &m); err != nil {
		return nil, errParsing("%s", err.Error())
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
