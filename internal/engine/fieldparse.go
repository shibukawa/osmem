package engine

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Value parsing of document fields with the semantics (and messages) of
// OpenSearch's field mappers, Jackson and the Java number parsers.

// Java collections --------------------------------------------------------------

func javaStringHash(s string) int32 {
	var h int32
	for _, u := range utf16.Encode([]rune(s)) {
		h = 31*h + int32(u)
	}
	return h
}

// javaHashMapOrder orders keys the way a java.util.HashMap filled with them
// iterates (bucket order, keys of one bucket in the given order).
func javaHashMapOrder(keys []string) []string {
	capacity := 16
	for float64(len(keys)) > float64(capacity)*0.75 {
		capacity *= 2
	}
	bucket := func(k string) uint32 {
		h := javaStringHash(k)
		spread := h ^ int32(uint32(h)>>16)
		return uint32(spread) & uint32(capacity-1)
	}
	out := append([]string(nil), keys...)
	sort.SliceStable(out, func(i, j int) bool { return bucket(out[i]) < bucket(out[j]) })
	return out
}

func sortedMapKeys(m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// javaValueString renders a parsed JSON value like Java's toString of the
// map/list/number/string XContent produces ("{a=1}", "[1, 2]", "1.0E10").
func javaValueString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return javaJSONNumberText(t)
	case float64:
		return javaNumberString(t, 64)
	case float32:
		return javaNumberString(float64(t), 32)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case M:
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range javaHashMapOrder(sortedMapKeys(t)) {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(javaValueString(t[k]))
		}
		b.WriteByte('}')
		return b.String()
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = javaValueString(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprint(v)
}

func isIntToken(s string) bool { return !strings.ContainsAny(s, ".eE") }

// javaJSONNumberText is the Java value of a JSON number: Integer, Long or
// BigInteger for integer tokens, Double for the others.
func javaJSONNumberText(n json.Number) string {
	s := n.String()
	if isIntToken(s) {
		if bi, ok := new(big.Int).SetString(s, 10); ok {
			return bi.String()
		}
		return s
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !math.IsInf(f, 0) {
		return s
	}
	return javaNumberString(f, 64)
}

// jsonTokenName is Jackson's token name of a value.
func jsonTokenNameOf(v any) string {
	switch t := v.(type) {
	case nil:
		return "VALUE_NULL"
	case bool:
		if t {
			return "VALUE_TRUE"
		}
		return "VALUE_FALSE"
	case string:
		return "VALUE_STRING"
	case json.Number:
		if isIntToken(t.String()) {
			return "VALUE_NUMBER_INT"
		}
		return "VALUE_NUMBER_FLOAT"
	case float64, float32:
		return "VALUE_NUMBER_FLOAT"
	case int, int64, int32:
		return "VALUE_NUMBER_INT"
	case M:
		return "START_OBJECT"
	case []any:
		return "START_ARRAY"
	}
	return "VALUE_EMBEDDED_OBJECT"
}

// Java number parsing ----------------------------------------------------------

var (
	javaDecimalRe   = regexp.MustCompile(`^[+-]?(NaN|Infinity|((\d+\.?\d*|\.\d+)([eE][+-]?\d+)?)[fFdD]?)$`)
	javaHexFloatRe  = regexp.MustCompile(`^[+-]?0[xX]([0-9a-fA-F]+\.?[0-9a-fA-F]*|\.[0-9a-fA-F]+)[pP][+-]?\d+[fFdD]?$`)
	bigDecimalRe    = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
	javaLongTokenRe = regexp.MustCompile(`^[+-]?\d+$`)
)

func numberFormatError(s string) *Error {
	return &Error{Type: "number_format_exception", Reason: fmt.Sprintf("For input string: \"%s\"", s)}
}

// javaParseDouble is Double.parseDouble (bitSize 64) or Float.parseFloat (32).
func javaParseDouble(s string, bitSize int) (float64, *Error) {
	trimmed := strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
	if trimmed == "" {
		return 0, &Error{Type: "number_format_exception", Reason: "empty String"}
	}
	switch {
	case javaDecimalRe.MatchString(trimmed):
		body := strings.TrimRight(trimmed, "fFdD")
		if strings.HasSuffix(body, "NaN") {
			return math.NaN(), nil
		}
		if strings.HasSuffix(body, "Infinity") {
			if body[0] == '-' {
				return math.Inf(-1), nil
			}
			return math.Inf(1), nil
		}
		f, err := strconv.ParseFloat(body, bitSize)
		if err != nil && !math.IsInf(f, 0) {
			if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				return f, nil
			}
			return 0, numberFormatError(trimmed)
		}
		return f, nil
	case javaHexFloatRe.MatchString(trimmed):
		body := strings.TrimRight(trimmed, "fFdD")
		f, err := strconv.ParseFloat(body, bitSize)
		if err != nil && !math.IsInf(f, 0) {
			return 0, numberFormatError(trimmed)
		}
		return f, nil
	}
	return 0, numberFormatError(trimmed)
}

// bigDecimalOf is new BigDecimal(String).
func bigDecimalOf(s string) (*big.Rat, bool) {
	if !bigDecimalRe.MatchString(s) {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(strings.TrimPrefix(s, "+"))
	return r, ok
}

var (
	bigMaxLong  = new(big.Int).SetInt64(math.MaxInt64)
	bigMinLong  = new(big.Int).SetInt64(math.MinInt64)
	bigMaxULong = new(big.Int).SetUint64(math.MaxUint64)
)

func ratTrunc(r *big.Rat) *big.Int {
	return new(big.Int).Quo(r.Num(), r.Denom())
}

// numbersToLong is OpenSearch's Numbers.toLong.
func numbersToLong(s string, coerce bool) (int64, *Error) {
	if javaLongTokenRe.MatchString(s) {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v, nil
		}
	}
	r, ok := bigDecimalOf(s)
	if !ok {
		return 0, errIllegalArgument("For input string: \"%s\"", s)
	}
	limit := new(big.Rat).SetFrac(new(big.Int).Add(bigMaxLong, big.NewInt(1)), big.NewInt(1))
	lowLimit := new(big.Rat).SetFrac(new(big.Int).Sub(bigMinLong, big.NewInt(1)), big.NewInt(1))
	if r.Cmp(limit) >= 0 || r.Cmp(lowLimit) <= 0 {
		return 0, errIllegalArgument("Value [%s] is out of range for a long", s)
	}
	if !coerce && !r.IsInt() {
		return 0, errIllegalArgument("Value [%s] has a decimal part", s)
	}
	bi := ratTrunc(r)
	if bi.Cmp(bigMaxLong) > 0 || bi.Cmp(bigMinLong) < 0 {
		return 0, errIllegalArgument("Value [%s] is out of range for a long", s)
	}
	return bi.Int64(), nil
}

// numeric fields -----------------------------------------------------------------

// numericDocValue is a numeric field value as its field indexes it.
type numericDocValue struct {
	value float64 // doc value
	exact string  // exact integral value (integral types)
	null  bool    // an empty string that counts as null
	// preview overrides the value preview of an error (floats read as
	// Java floats)
}

func inputCoercion(format string, args ...any) *Error {
	return &Error{Type: "input_coercion_exception", Reason: fmt.Sprintf(format, args...)}
}

func javaSimpleType(typ string) string {
	switch typ {
	case TypeByte, TypeInteger, TypeTokenCount:
		return "Integer"
	case TypeShort:
		return "Short"
	case TypeLong:
		return "Long"
	case TypeFloat, TypeHalfFloat:
		return "Float"
	case TypeDouble, TypeScaledFloat:
		return "Double"
	case TypeUnsignedLong:
		return "BigInteger"
	}
	return "Number"
}

// parseNumericField parses a document value for a numeric field. The
// returned string is the preview OpenSearch reports when it differs from
// the plain value (floats parsed as Java floats).
func parseNumericField(f *Field, v any, coerce bool) (numericDocValue, string, *Error) {
	token := jsonTokenNameOf(v)
	switch token {
	case "VALUE_TRUE", "VALUE_FALSE", "START_OBJECT", "START_ARRAY":
		return numericDocValue{}, "", inputCoercion("Current token (%s) not numeric, cannot use numeric value accessors", token)
	}
	text, isString := v.(string)
	if !isString {
		if n, ok := v.(json.Number); ok {
			text = n.String()
		} else {
			text = javaValueString(v)
			if fv, ok := v.(float64); ok {
				text = strconv.FormatFloat(fv, 'g', -1, 64)
			}
		}
	}
	if isString && text == "" && coerce {
		return numericDocValue{null: true}, "", nil
	}
	if isString && !coerce {
		return numericDocValue{}, "", errIllegalArgument("%s value passed as String", javaSimpleType(f.Type))
	}
	intToken := !isString && isIntToken(text)
	switch f.Type {
	case TypeByte, TypeShort, TypeInteger:
		var iv int64
		if isString {
			d, nfe := javaParseDouble(text, 64)
			if nfe != nil {
				return numericDocValue{}, "", nfe
			}
			if f.Type == TypeShort {
				if d < math.MinInt16 || d > math.MaxInt16 {
					return numericDocValue{}, "", errIllegalArgument("Value [%s] is out of range for a short", text)
				}
			} else if d < math.MinInt32 || d > math.MaxInt32 {
				return numericDocValue{}, "", errIllegalArgument("Value [%s] is out of range for an integer", text)
			}
			if !math.IsNaN(d) {
				iv = int64(d)
			}
		} else if intToken {
			bi, ok := new(big.Int).SetString(text, 10)
			if !ok || !bi.IsInt64() || bi.Int64() < math.MinInt32 || bi.Int64() > math.MaxInt32 {
				return numericDocValue{}, "", inputCoercion("Numeric value (%s) out of range of `int` (-2147483648 - 2147483647)", text)
			}
			iv = bi.Int64()
			if f.Type == TypeShort && (iv < math.MinInt16 || iv > math.MaxInt16) {
				return numericDocValue{}, "", inputCoercion("Numeric value (%s) out of range of `short` (-32768 - 32767)", text)
			}
		} else {
			d, _ := strconv.ParseFloat(text, 64)
			if d < math.MinInt32 || d > math.MaxInt32 || math.IsNaN(d) {
				return numericDocValue{}, "", inputCoercion("Numeric value (%s) out of range of `int` (-2147483648 - 2147483647)", text)
			}
			iv = int64(d)
			if f.Type == TypeShort && (iv < math.MinInt16 || iv > math.MaxInt16) {
				return numericDocValue{}, "", inputCoercion("Numeric value (%s) out of range of `short` (-32768 - 32767)", text)
			}
			if !coerce && float64(iv) != d {
				return numericDocValue{}, "", errIllegalArgument("%s cannot be converted to %s without data loss", javaNumberString(d, 64), javaSimpleType(f.Type))
			}
		}
		if f.Type == TypeByte && (iv < math.MinInt8 || iv > math.MaxInt8) {
			return numericDocValue{}, "", errIllegalArgument("Value [%d] is out of range for a byte", iv)
		}
		return numericDocValue{value: float64(iv), exact: strconv.FormatInt(iv, 10)}, "", nil
	case TypeLong:
		var lv int64
		if isString {
			var err *Error
			if lv, err = numbersToLong(text, coerce); err != nil {
				return numericDocValue{}, "", err
			}
		} else if intToken {
			bi, ok := new(big.Int).SetString(text, 10)
			if !ok || !bi.IsInt64() {
				return numericDocValue{}, "", inputCoercion("Numeric value (%s) out of range of `long` (-9223372036854775808 - 9223372036854775807)", text)
			}
			lv = bi.Int64()
		} else {
			d, _ := strconv.ParseFloat(text, 64)
			if d < -9.223372036854775808e18 || d > 9.223372036854775807e18 || math.IsNaN(d) {
				return numericDocValue{}, "", inputCoercion("Numeric value (%s) out of range of `long` (-9223372036854775808 - 9223372036854775807)", text)
			}
			if d >= 9.223372036854775807e18 {
				lv = math.MaxInt64
			} else {
				lv = int64(d)
			}
			if !coerce && float64(lv) != d {
				return numericDocValue{}, "", errIllegalArgument("%s cannot be converted to Long without data loss", javaNumberString(d, 64))
			}
		}
		return numericDocValue{value: float64(lv), exact: strconv.FormatInt(lv, 10)}, "", nil
	case TypeUnsignedLong:
		r, ok := bigDecimalOf(text)
		if !ok {
			return numericDocValue{}, "", errIllegalArgument("For input string: \"%s\"", text)
		}
		if !coerce && !r.IsInt() {
			return numericDocValue{}, "", errIllegalArgument("Value [%s] has a decimal part", text)
		}
		bi := ratTrunc(r)
		if bi.Sign() < 0 || bi.Cmp(bigMaxULong) > 0 {
			return numericDocValue{}, "", errIllegalArgument("Value [%s] is out of range for an unsigned long", text)
		}
		fv, _ := new(big.Float).SetInt(bi).Float64()
		return numericDocValue{value: fv, exact: bi.String()}, "", nil
	case TypeFloat, TypeHalfFloat:
		var d float64
		if isString {
			var nfe *Error
			if d, nfe = javaParseDouble(text, 32); nfe != nil {
				return numericDocValue{}, "", nfe
			}
		} else {
			d64, _ := strconv.ParseFloat(text, 64)
			d = float64(float32(d64))
		}
		v32 := float32(d)
		preview := ""
		if !isString && !intToken {
			preview = javaFloatText(float64(v32), 32)
		}
		if f.Type == TypeFloat {
			if math.IsInf(float64(v32), 0) || math.IsNaN(float64(v32)) {
				return numericDocValue{}, preview, errIllegalArgument("[float] supports only finite values, but got [%s]", javaFloatText(float64(v32), 32))
			}
			return numericDocValue{value: float64(v32)}, preview, nil
		}
		h := halfFloat(v32)
		if math.IsInf(float64(h), 0) || math.IsNaN(float64(h)) {
			return numericDocValue{}, preview, errIllegalArgument("[half_float] supports only finite values, but got [%s]", javaFloatText(float64(v32), 32))
		}
		return numericDocValue{value: float64(h)}, preview, nil
	case TypeDouble, TypeScaledFloat:
		var d float64
		if isString {
			var nfe *Error
			if d, nfe = javaParseDouble(text, 64); nfe != nil {
				return numericDocValue{}, "", nfe
			}
		} else {
			d, _ = strconv.ParseFloat(text, 64)
		}
		if math.IsInf(d, 0) || math.IsNaN(d) {
			if f.Type == TypeScaledFloat {
				return numericDocValue{}, "", errIllegalArgument("[scaled_float] only supports finite values, but got [%s]", javaFloatText(d, 64))
			}
			return numericDocValue{}, "", errIllegalArgument("[double] supports only finite values, but got [%s]", javaFloatText(d, 64))
		}
		if f.Type == TypeScaledFloat {
			sf := scalingFactor(f)
			return numericDocValue{value: float64(javaMathRound(d*sf)) * (1 / sf)}, "", nil
		}
		return numericDocValue{value: d}, "", nil
	}
	return numericDocValue{}, "", errIllegalArgument("unsupported numeric type [%s]", f.Type)
}

// javaFloatText is Float/Double.toString including the non-finite values.
func javaFloatText(v float64, bitSize int) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Infinity"
	case math.IsInf(v, -1):
		return "-Infinity"
	}
	return javaNumberString(v, bitSize)
}

// javaMathRound is Math.round(double).
func javaMathRound(v float64) int64 {
	switch {
	case math.IsNaN(v):
		return 0
	case v >= 9.223372036854775807e18:
		return math.MaxInt64
	case v <= -9.223372036854775808e18:
		return math.MinInt64
	}
	return int64(math.Floor(v + 0.5))
}

// booleans -------------------------------------------------------------------------

// parseBooleanField parses a boolean field value (XContentParser.booleanValue).
func parseBooleanField(v any) (bool, *Error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch t {
		case "true":
			return true, nil
		case "false", "":
			return false, nil
		}
		return false, errIllegalArgument("Failed to parse value [%s] as only [true] or [false] are allowed.", t)
	}
	return false, inputCoercion("Current token (%s) not of boolean type", jsonTokenNameOf(v))
}

// ips --------------------------------------------------------------------------------

// parseIPString is InetAddresses.forString.
func parseIPString(s string) (net.IP, bool) {
	if strings.ContainsAny(s, "%/ ") {
		return nil, false
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, false
	}
	return ip, true
}

func errNotIP(s string) *Error {
	return errIllegalArgument("'%s' is not an IP string literal.", s)
}

// formatIP is NetworkAddress.format.
func formatIP(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

// ipTerm is the indexed form of an address: the 16 byte address in hex,
// which sorts like InetAddressPoint.
func ipTerm(ip net.IP) string { return hex.EncodeToString(ip.To16()) }

// normalizeIPValue returns the formatted address of a document value.
func normalizeIPValue(s string) (string, bool) {
	ip, ok := parseIPString(s)
	if !ok {
		return "", false
	}
	return formatIP(ip), true
}

// parseCIDR is InetAddresses.parseCidr; it returns the first and last
// address of the block.
func parseCIDR(s string) (net.IP, net.IP, *Error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return nil, nil, errIllegalArgument("Expected [ip/prefix] but was [%s]", s)
	}
	ip, ok := parseIPString(parts[0])
	if !ok {
		return nil, nil, errNotIP(parts[0])
	}
	bits := 128
	if ip.To4() != nil {
		if strings.Contains(parts[0], ":") {
			return nil, nil, errIllegalArgument("CIDR notation is not allowed with IPv6-mapped IPv4 address [%s as it introduces ambiguity as to whether the prefix length should be interpreted as a v4 prefix length or a v6 prefix length", parts[0])
		}
		bits = 32
	}
	prefix, err := strconv.Atoi(parts[1])
	if err != nil || !javaLongTokenRe.MatchString(parts[1]) {
		return nil, nil, &Error{Type: "number_format_exception", Reason: fmt.Sprintf("For input string: \"%s\"", parts[1])}
	}
	if prefix < 0 || prefix > bits {
		return nil, nil, errIllegalArgument("Illegal prefix length [%d] in [%s]. Must be 0-32 for IPv4 ranges, 0-128 for IPv6 ranges", prefix, s)
	}
	lo := make(net.IP, 16)
	hi := make(net.IP, 16)
	copy(lo, ip.To16())
	copy(hi, ip.To16())
	start := prefix
	if bits == 32 {
		start = prefix + 96
	}
	for i := start; i < 128; i++ {
		lo[i/8] &^= 1 << (7 - uint(i%8))
		hi[i/8] |= 1 << (7 - uint(i%8))
	}
	return lo, hi, nil
}

// geo points ----------------------------------------------------------------------------

func geoParseError(format string, args ...any) *Error {
	return &Error{Type: "parse_exception", Reason: fmt.Sprintf(format, args...)}
}

// parseGeoPointValue parses the geo_point representations of OpenSearch:
// {"lat","lon"}, {"geohash"}, GeoJSON points, "lat,lon[,z]", WKT points,
// geohashes and [lon, lat[, z]].
func parseGeoPointValue(v any, ignoreZ bool) (lat, lon float64, err *Error) {
	checkZ := func(z float64) *Error {
		if !ignoreZ {
			return geoParseError("Exception parsing coordinates: found Z value [%s] but [ignore_z_value] parameter is [false]", javaNumberString(z, 64))
		}
		return nil
	}
	switch t := v.(type) {
	case M:
		_, hasLat := t["lat"]
		_, hasLon := t["lon"]
		gh, hasHash := t["geohash"]
		typ, hasType := t["type"]
		coords, hasCoords := t["coordinates"]
		for k := range t {
			switch k {
			case "lat", "lon", "geohash", "type", "coordinates":
			default:
				return 0, 0, geoParseError("field must be either [lon|lat], [type|coordinates], or [geohash]")
			}
		}
		switch {
		case (hasLat || hasLon) && !hasHash && !hasType && !hasCoords:
			var nfe *Error
			parse := func(key string) float64 {
				val, ok := t[key]
				if !ok {
					return math.NaN()
				}
				switch x := val.(type) {
				case json.Number:
					f, _ := strconv.ParseFloat(x.String(), 64)
					return f
				case string:
					f, e := javaParseDouble(x, 64)
					if e != nil {
						nfe = e
					}
					return f
				case float64:
					return x
				case Double:
					return float64(x)
				}
				nfe = &Error{Type: "number_format_exception", Reason: fmt.Sprintf("For input string: \"%s\"", javaValueString(val))}
				return math.NaN()
			}
			lat, lon = parse("lat"), parse("lon")
			if nfe != nil {
				return 0, 0, &Error{Type: "parse_exception", Reason: "[lon] and [lat] must be valid double values", Cause: nfe}
			}
			if !hasLat {
				return 0, 0, geoParseError("field [lat] missing")
			}
			if !hasLon {
				return 0, 0, geoParseError("field [lon] missing")
			}
			return lat, lon, nil
		case hasHash && !hasLat && !hasLon && !hasType && !hasCoords:
			s, ok := gh.(string)
			if !ok {
				return 0, 0, geoParseError("geohash must be a string")
			}
			return decodeGeohash(s)
		case (hasType || hasCoords) && !hasLat && !hasLon && !hasHash:
			if s, _ := typ.(string); !strings.EqualFold(s, "point") {
				return 0, 0, geoParseError("type must be Point")
			}
			arr, ok := coords.([]any)
			if !ok || len(arr) < 2 || len(arr) > 3 {
				return 0, 0, geoParseError("coordinates must be an array of 2 or 3 numbers")
			}
			nums := make([]float64, len(arr))
			for i, e := range arr {
				f, ok := toFloat(e)
				if !ok {
					return 0, 0, geoParseError("coordinates must be numbers")
				}
				nums[i] = f
			}
			if len(nums) == 3 {
				if e := checkZ(nums[2]); e != nil {
					return 0, 0, e
				}
			}
			return nums[1], nums[0], nil
		}
		return 0, 0, geoParseError("field must be either [lon|lat], [type|coordinates], or [geohash]")
	case []any:
		if len(t) > 3 {
			return 0, 0, geoParseError("geo_point expected")
		}
		nums := make([]float64, 0, 3)
		for _, e := range t {
			n, ok := e.(json.Number)
			if !ok {
				if f, isFloat := e.(float64); isFloat {
					nums = append(nums, f)
					continue
				}
				return 0, 0, geoParseError("geo_point expected")
			}
			f, _ := strconv.ParseFloat(n.String(), 64)
			nums = append(nums, f)
		}
		if len(nums) < 2 {
			return 0, 0, inputCoercion("Current token (END_ARRAY) not numeric, cannot use numeric value accessors")
		}
		if len(nums) == 3 {
			if e := checkZ(nums[2]); e != nil {
				return 0, 0, e
			}
		}
		return nums[1], nums[0], nil
	case string:
		s := t
		if strings.Contains(strings.ToLower(s), "point") {
			return parseWKTPoint(s, ignoreZ)
		}
		if strings.Contains(s, ",") {
			parts := strings.Split(s, ",")
			if len(parts) != 2 && len(parts) != 3 {
				return 0, 0, geoParseError("failed to parse [%s], expected 2 or 3 coordinates but found: [%d]", s, len(parts))
			}
			la, e1 := javaParseDouble(parts[0], 64)
			if e1 != nil {
				return 0, 0, geoParseError("latitude must be a number")
			}
			lo, e2 := javaParseDouble(parts[1], 64)
			if e2 != nil {
				return 0, 0, geoParseError("longitude must be a number")
			}
			if len(parts) == 3 {
				z, e3 := javaParseDouble(parts[2], 64)
				if e3 != nil {
					return 0, 0, geoParseError("altitude must be a number")
				}
				if e := checkZ(z); e != nil {
					return 0, 0, e
				}
			}
			return la, lo, nil
		}
		return decodeGeohash(s)
	}
	return 0, 0, geoParseError("geo_point expected")
}

var wktPointRe = regexp.MustCompile(`(?i)^\s*point\s*\(\s*([^\s()]+)\s+([^\s()]+)(?:\s+([^\s()]+))?\s*\)\s*$`)

func parseWKTPoint(s string, ignoreZ bool) (float64, float64, *Error) {
	m := wktPointRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, geoParseError("Invalid WKT format")
	}
	lon, err1 := strconv.ParseFloat(m[1], 64)
	lat, err2 := strconv.ParseFloat(m[2], 64)
	if err1 != nil || err2 != nil {
		return 0, 0, geoParseError("Invalid WKT format")
	}
	if lon < -180 || lon > 180 {
		return 0, 0, &Error{Type: "parse_exception", Reason: "Invalid WKT format",
			Cause: errIllegalArgument("invalid longitude %s; must be between -180.0 and 180.0", javaNumberString(lon, 64))}
	}
	if lat < -90 || lat > 90 {
		return 0, 0, &Error{Type: "parse_exception", Reason: "Invalid WKT format",
			Cause: errIllegalArgument("invalid latitude %s; must be between -90.0 and 90.0", javaNumberString(lat, 64))}
	}
	if m[3] != "" {
		z, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			return 0, 0, geoParseError("Invalid WKT format")
		}
		if !ignoreZ {
			return 0, 0, geoParseError("Exception parsing coordinates: found Z value [%s] but [ignore_z_value] parameter is [false]", javaNumberString(z, 64))
		}
	}
	return lat, lon, nil
}

const geohashBase32 = "0123456789bcdefghjkmnpqrstuvwxyz"

// decodeGeohash returns the south-west corner of a geohash cell.
func decodeGeohash(s string) (float64, float64, *Error) {
	if s == "" {
		return 0, 0, geoParseError("empty geohash")
	}
	latLo, latHi := -90.0, 90.0
	lonLo, lonHi := -180.0, 180.0
	even := true
	for _, c := range s {
		idx := strings.IndexRune(geohashBase32, c)
		if idx < 0 || len(s) > 12 {
			reason := fmt.Sprintf("unsupported symbol [%c] in geohash [%s]", c, s)
			return 0, 0, &Error{Type: "parse_exception", Reason: reason, Cause: errIllegalArgument("%s", reason)}
		}
		for bit := 4; bit >= 0; bit-- {
			on := idx&(1<<uint(bit)) != 0
			if even {
				mid := (lonLo + lonHi) / 2
				if on {
					lonLo = mid
				} else {
					lonHi = mid
				}
			} else {
				mid := (latLo + latHi) / 2
				if on {
					latLo = mid
				} else {
					latHi = mid
				}
			}
			even = !even
		}
	}
	return latLo, lonLo, nil
}

func checkGeoRange(lat, lon float64, field string) *Error {
	if math.IsNaN(lat) || lat < -90 || lat > 90 {
		return errIllegalArgument("illegal latitude value [%s] for %s", javaNumberString(lat, 64), field)
	}
	if math.IsNaN(lon) || lon < -180 || lon > 180 {
		return errIllegalArgument("illegal longitude value [%s] for %s", javaNumberString(lon, 64), field)
	}
	return nil
}
