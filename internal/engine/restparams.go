package engine

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// URL parameter parsing with OpenSearch's RestRequest semantics and error
// messages (paramAsBoolean, paramAsInt, TimeValue.parseTimeValue, ...). Every
// failure is an *Error, so HTTP handlers and engine code report exactly what
// OpenSearch reports for an invalid parameter value.

func illegalArgument(reason string, cause *Error) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: reason, Cause: cause}
}

// numberFormatException is Java's NumberFormatException.forInputString.
func numberFormatException(input string) *Error {
	return &Error{Type: "number_format_exception", Reason: "For input string: \"" + input + "\""}
}

// BoolValueError is Booleans.parseBoolean's failure for value.
func BoolValueError(value string) *Error {
	return illegalArgument("Failed to parse value ["+value+"] as only [true] or [false] are allowed.", nil)
}

func isBlank(s string) bool { return strings.TrimSpace(s) == "" }

// ParseBoolValue parses a present boolean parameter like paramAsBoolean: an
// empty value means true, a blank value the default, otherwise only "true"
// and "false" are accepted.
func ParseBoolValue(value string, def bool) (bool, *Error) {
	switch {
	case value == "":
		return true, nil
	case isBlank(value):
		return def, nil
	case value == "true":
		return true, nil
	case value == "false":
		return false, nil
	}
	return def, BoolValueError(value)
}

// javaParseInt parses like Integer.parseInt (bits 32) or Long.parseLong (64).
func javaParseInt(s string, bits int) (int64, bool) {
	if s == "" {
		return 0, false
	}
	i := 0
	if s[0] == '+' || s[0] == '-' {
		if len(s) == 1 {
			return 0, false
		}
		i = 1
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, bits)
	return n, err == nil
}

// ParseIntValue is paramAsInt for a present parameter.
func ParseIntValue(name, value string) (int, *Error) {
	n, ok := javaParseInt(value, 32)
	if !ok {
		return 0, illegalArgument("Failed to parse int parameter ["+name+"] with value ["+value+"]", numberFormatException(value))
	}
	return int(n), nil
}

// ParseLongValue is paramAsLong for a present parameter.
func ParseLongValue(name, value string) (int64, *Error) {
	n, ok := javaParseInt(value, 64)
	if !ok {
		return 0, illegalArgument("Failed to parse long parameter ["+name+"] with value ["+value+"]", numberFormatException(value))
	}
	return n, nil
}

// javaParseFloat parses like Float.parseFloat / Double.parseDouble and
// returns the NumberFormatException on failure.
func javaParseFloat(s string) (float64, *Error) {
	t := strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
	if t == "" {
		return 0, &Error{Type: "number_format_exception", Reason: "empty String"}
	}
	fail := func() (float64, *Error) { return 0, numberFormatException(t) }
	sign, body := "", t
	if body[0] == '+' || body[0] == '-' {
		sign, body = body[:1], body[1:]
	}
	switch body {
	case "NaN":
		return math.NaN(), nil
	case "Infinity":
		if sign == "-" {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	}
	if n := len(body); n > 0 && strings.ContainsRune("fFdD", rune(body[n-1])) && !strings.HasPrefix(strings.ToLower(body), "0x") {
		body = body[:n-1]
	}
	if strings.HasPrefix(body, "0x") || strings.HasPrefix(body, "0X") {
		f, err := strconv.ParseFloat(sign+body, 64)
		if err != nil {
			return fail()
		}
		return f, nil
	}
	digits, i := 0, 0
	for i < len(body) && body[i] >= '0' && body[i] <= '9' {
		i, digits = i+1, digits+1
	}
	if i < len(body) && body[i] == '.' {
		i++
		for i < len(body) && body[i] >= '0' && body[i] <= '9' {
			i, digits = i+1, digits+1
		}
	}
	if digits == 0 {
		return fail()
	}
	if i < len(body) && (body[i] == 'e' || body[i] == 'E') {
		i++
		if i < len(body) && (body[i] == '+' || body[i] == '-') {
			i++
		}
		exp := 0
		for i < len(body) && body[i] >= '0' && body[i] <= '9' {
			i, exp = i+1, exp+1
		}
		if exp == 0 {
			return fail()
		}
	}
	if i != len(body) {
		return fail()
	}
	f, err := strconv.ParseFloat(sign+body, 64)
	if err != nil && !strings.Contains(err.Error(), "value out of range") {
		return fail()
	}
	return f, nil
}

// ParseFloatValue is paramAsFloat for a present parameter.
func ParseFloatValue(name, value string) (float64, *Error) {
	f, nfe := javaParseFloat(value)
	if nfe != nil {
		return 0, illegalArgument("Failed to parse float parameter ["+name+"] with value ["+value+"]", nfe)
	}
	return f, nil
}

// ParseTimeValue is TimeValue.parseTimeValue(value, null, setting). The
// magic value -1 is returned as -1 (a negative duration).
func ParseTimeValue(setting, value string) (time.Duration, *Error) {
	normalized := strings.ToLower(strings.TrimFunc(value, func(r rune) bool { return r <= ' ' }))
	var suffix string
	var unit time.Duration
	switch {
	case strings.HasSuffix(normalized, "nanos"):
		suffix, unit = "nanos", time.Nanosecond
	case strings.HasSuffix(normalized, "micros"):
		suffix, unit = "micros", time.Microsecond
	case strings.HasSuffix(normalized, "ms"):
		suffix, unit = "ms", time.Millisecond
	case strings.HasSuffix(normalized, "s"):
		suffix, unit = "s", time.Second
	case strings.HasSuffix(value, "m"):
		// minutes are case-sensitive: 'M' would be months
		suffix, unit = "m", time.Minute
	case strings.HasSuffix(normalized, "h"):
		suffix, unit = "h", time.Hour
	case strings.HasSuffix(normalized, "d"):
		suffix, unit = "d", 24*time.Hour
	case isMinusOne(normalized):
		return -1, nil
	case normalized != "" && strings.Trim(normalized, "0") == "":
		return 0, nil
	default:
		return 0, illegalArgument("failed to parse setting ["+setting+"] with value ["+value+"] as a time value: unit is missing or unrecognized", nil)
	}
	numeric := strings.TrimFunc(normalized[:len(normalized)-len(suffix)], func(r rune) bool { return r <= ' ' })
	n, ok := javaParseInt(numeric, 64)
	if !ok {
		if _, nfe := javaParseFloat(numeric); nfe == nil {
			return 0, illegalArgument("failed to parse ["+value+"], fractional time values are not supported", numberFormatException(numeric))
		}
		return 0, illegalArgument("failed to parse ["+value+"]", numberFormatException(numeric))
	}
	if n < -1 {
		return 0, illegalArgument("failed to parse setting ["+setting+"] with value ["+value+"] as a time value: negative durations are not supported", nil)
	}
	return time.Duration(n) * unit, nil
}

// isMinusOne matches Java's "-0*1".
func isMinusOne(s string) bool {
	if len(s) < 2 || s[0] != '-' || s[len(s)-1] != '1' {
		return false
	}
	return strings.Trim(s[1:len(s)-1], "0") == ""
}

// ParseActiveShardCount is ActiveShardCount.parseString.
func ParseActiveShardCount(value string) *Error {
	if value == "all" {
		return nil
	}
	n, ok := javaParseInt(value, 32)
	if !ok {
		return illegalArgument("cannot parse ActiveShardCount["+value+"]", numberFormatException(value))
	}
	if n < 0 {
		return illegalArgument("shard count cannot be a negative value", nil)
	}
	return nil
}

// JavaSplitComma is String.split(","): trailing empty strings are dropped,
// and an empty input yields no elements (Strings.splitStringByCommaToArray).
func JavaSplitComma(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// ParseExpandWildcards validates expand_wildcards like
// IndicesOptions.WildcardStates.parseParameter.
func ParseExpandWildcards(value string) *Error {
	for _, w := range JavaSplitComma(value) {
		switch w {
		case "open", "closed", "hidden", "none", "all":
		default:
			return illegalArgument("No valid expand wildcard value ["+w+"]", nil)
		}
	}
	return nil
}

// ParseIndicesOptionBool validates ignore_unavailable, allow_no_indices and
// ignore_throttled (XContentMapValues.nodeBooleanValue): blank values keep
// the default.
func ParseIndicesOptionBool(name, value string) *Error {
	if isBlank(value) || value == "true" || value == "false" {
		return nil
	}
	return illegalArgument("Could not convert ["+name+"] to boolean", BoolValueError(value))
}

// BoolParam is RestRequest.paramAsBoolean: a missing parameter is def, an
// empty value is true, anything but "true" or "false" fails.
func (p Params) BoolParam(name string, def bool) (bool, error) {
	v, ok := p[name]
	if !ok {
		return def, nil
	}
	b, err := ParseBoolValue(v, def)
	if err != nil {
		return def, err
	}
	return b, nil
}

// IntParam is RestRequest.paramAsInt.
func (p Params) IntParam(name string, def int) (int, error) {
	v, ok := p[name]
	if !ok {
		return def, nil
	}
	n, err := ParseIntValue(name, v)
	if err != nil {
		return def, err
	}
	return n, nil
}

// LongParam is RestRequest.paramAsLong.
func (p Params) LongParam(name string, def int64) (int64, error) {
	v, ok := p[name]
	if !ok {
		return def, nil
	}
	n, err := ParseLongValue(name, v)
	if err != nil {
		return def, err
	}
	return n, nil
}

// FloatParam is RestRequest.paramAsFloat.
func (p Params) FloatParam(name string, def float64) (float64, error) {
	v, ok := p[name]
	if !ok {
		return def, nil
	}
	f, err := ParseFloatValue(name, v)
	if err != nil {
		return def, err
	}
	return f, nil
}

// TimeParam is RestRequest.paramAsTime.
func (p Params) TimeParam(name string, def time.Duration) (time.Duration, error) {
	v, ok := p[name]
	if !ok {
		return def, nil
	}
	d, err := ParseTimeValue(name, v)
	if err != nil {
		return def, err
	}
	return d, nil
}

// UnrecognizedError is BaseRestHandler.unrecognized: the invalid names in
// sorted order, each followed by the candidates whose Levenshtein similarity
// exceeds 0.5 ("-> did you mean [x]?" or "any of [x, y]").
func UnrecognizedError(path string, invalid, candidates []string, detail string) *Error {
	invalid = append([]string(nil), invalid...)
	sort.Strings(invalid)
	seen := map[string]bool{}
	var cands []string
	for _, c := range candidates {
		if !seen[c] {
			seen[c] = true
			cands = append(cands, c)
		}
	}
	var b strings.Builder
	plural := ""
	if len(invalid) > 1 {
		plural = "s"
	}
	fmt.Fprintf(&b, "request [%s] contains unrecognized %s%s: ", path, detail, plural)
	for i, name := range invalid {
		type scored struct {
			similarity float32
			name       string
		}
		var list []scored
		for _, c := range cands {
			if s := levenshteinSimilarity(name, c); s > 0.5 {
				list = append(list, scored{s, c})
			}
		}
		sort.SliceStable(list, func(a, c int) bool {
			if list[a].similarity != list[c].similarity {
				return list[a].similarity > list[c].similarity
			}
			return list[a].name < list[c].name
		})
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("[" + name + "]")
		switch len(list) {
		case 0:
		case 1:
			b.WriteString(" -> did you mean [" + list[0].name + "]?")
		default:
			names := make([]string, len(list))
			for j, s := range list {
				names[j] = s.name
			}
			b.WriteString(" -> did you mean any of [" + strings.Join(names, ", ") + "]?")
		}
	}
	return illegalArgument(b.String(), nil)
}

// levenshteinSimilarity is Lucene's LevenshteinDistance.getDistance.
func levenshteinSimilarity(target, other string) float32 {
	sa, t := []rune(target), []rune(other)
	n, m := len(sa), len(t)
	if n == 0 || m == 0 {
		if n == m {
			return 1
		}
		return 0
	}
	p, d := make([]int, n+1), make([]int, n+1)
	for i := range p {
		p[i] = i
	}
	for j := 1; j <= m; j++ {
		d[0] = j
		for i := 1; i <= n; i++ {
			cost := 1
			if sa[i-1] == t[j-1] {
				cost = 0
			}
			d[i] = min(d[i-1]+1, p[i]+1, p[i-1]+cost)
		}
		p, d = d, p
	}
	return 1.0 - float32(p[n])/float32(max(m, n))
}
