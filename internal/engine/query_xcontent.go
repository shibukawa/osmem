package engine

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Readers for the values of a query object, reproducing the conversions
// and errors of OpenSearch's XContentParser (backed by Jackson) on the
// decoded JSON tree.

// xToken is the XContentParser token name of a value.
func xTokenName(v any) string {
	switch t := v.(type) {
	case nil:
		return "VALUE_NULL"
	case M:
		return "START_OBJECT"
	case []any:
		return "START_ARRAY"
	case string:
		return "VALUE_STRING"
	case bool:
		return "VALUE_BOOLEAN"
	case json.Number, float64, float32, int, int64:
		_ = t
		return "VALUE_NUMBER"
	}
	return "VALUE_EMBEDDED_OBJECT"
}

// jacksonToken is the Jackson token name of a value.
func jacksonToken(v any) string {
	switch t := v.(type) {
	case nil:
		return "VALUE_NULL"
	case M:
		return "START_OBJECT"
	case []any:
		return "START_ARRAY"
	case string:
		return "VALUE_STRING"
	case bool:
		if t {
			return "VALUE_TRUE"
		}
		return "VALUE_FALSE"
	case json.Number:
		if isIntegerLiteral(t.String()) {
			return "VALUE_NUMBER_INT"
		}
		return "VALUE_NUMBER_FLOAT"
	case float64:
		if t == math.Trunc(t) {
			return "VALUE_NUMBER_INT"
		}
		return "VALUE_NUMBER_FLOAT"
	case int, int64:
		return "VALUE_NUMBER_INT"
	}
	return "VALUE_EMBEDDED_OBJECT"
}

func isIntegerLiteral(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '-' && i == 0 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isXValue reports whether the token of v is a value (XContentParser.Token
// isValue: strings, numbers and booleans, not null).
func isXValue(v any) bool {
	switch v.(type) {
	case string, bool, json.Number, float64, float32, int, int64:
		return true
	}
	return false
}

// xText is parser.text() for a value token.
func xText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

// noTextError is the IllegalStateException of parser.text() on a
// non-value token.
func noTextError(v any) *Error {
	return pIllegalState("Can't get text on a " + jacksonTokenForText(v))
}

// noTextAt is noTextError for the value at t: Java's message ends with the
// location.
func noTextAt(v any, t *tokenRef) *Error {
	e := noTextError(v)
	return e.atInReason(t, e.Reason+" at %s")
}

func jacksonTokenForText(v any) string {
	if v == nil {
		return "VALUE_NULL"
	}
	return jacksonToken(v)
}

// xFloat is parser.floatValue() (and doubleValue()): numbers as they are,
// numeric strings parsed, anything else an error.
func xFloat(v any) (float64, *Error) {
	switch t := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(t.String(), 64)
		if err != nil {
			return 0, pNumberFormat(t.String())
		}
		return f, nil
	case float64:
		return t, nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case string:
		f, ok := javaDoubleOK(t)
		if !ok {
			if strings.TrimSpace(t) == "" {
				return 0, parseFailure(&Error{Status: 400, Type: "number_format_exception", Reason: "empty String"})
			}
			return 0, pNumberFormat(t)
		}
		return f, nil
	case bool:
		return 0, pInputCoercion("Current token (" + jacksonToken(v) + ") not numeric, cannot use numeric value accessors")
	}
	return 0, pInputCoercion("Current token (" + jacksonToken(v) + ") not numeric, cannot use numeric value accessors")
}

// javaParseDouble parses a number the way Double.parseDouble does
// (surrounding whitespace allowed, optional d/f suffix).
func javaDoubleOK(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	switch s {
	case "NaN":
		return math.NaN(), true
	case "Infinity", "+Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	if last := s[len(s)-1]; last == 'd' || last == 'D' || last == 'f' || last == 'F' {
		s = s[:len(s)-1]
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r == '.' || r == 'e' || r == 'E' || r == '+' || r == '-') {
			return 0, false
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// xInt is parser.intValue(): numbers truncated, strings parsed as doubles.
func xInt(v any) (int, *Error) {
	switch t := v.(type) {
	case string:
		f, ok := javaDoubleOK(t)
		if !ok {
			return 0, pNumberFormat(t)
		}
		if f < math.MinInt32 || f > math.MaxInt32 {
			return 0, pIllegalArgument("Value [%s] is out of range for an integer", t)
		}
		return int(f), nil
	case bool:
		return 0, pInputCoercion("Current token (" + jacksonToken(v) + ") not numeric, cannot use numeric value accessors")
	}
	f, err := xFloat(v)
	if err != nil {
		return 0, err
	}
	return int(f), nil
}

// xBool is parser.booleanValue(): booleans and the strings "true" and
// "false".
func xBool(v any) (bool, *Error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch t {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return false, pIllegalArgument("Failed to parse value [%s] as only [true] or [false] are allowed.", t)
	}
	return false, pInputCoercion("Current token (" + jacksonToken(v) + ") not of boolean type")
}

// objectKeys returns the keys of an object in a stable order (request
// bodies are decoded into maps, so the original order is not available).
func objectKeys(m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// javaFloatString renders a float the way Java's Float.toString does for
// the values used in messages ("1.0", "0.5").
func javaFloatString(f float64) string {
	s := strconv.FormatFloat(float64(float32(f)), 'f', -1, 32)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}
