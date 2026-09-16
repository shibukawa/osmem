package engine

import (
	"math"
	"math/bits"
	"net"
	"sort"
	"time"
)

// javaRound is Math.round for scaled_float values.
func javaRound(v float64) float64 { return math.Floor(v + 0.5) }

// halfFloat rounds a float to the binary16 value Lucene's HalfFloatPoint
// stores for half_float fields (round half to even).
func halfFloat(v float32) float32 {
	return shortBitsToHalfFloat(halfFloatToShortBits(v))
}

func halfFloatToShortBits(v float32) uint16 {
	b := math.Float32bits(v)
	sign := b >> 31
	exp := int((b >> 23) & 0xff)
	mantissa := b & 0x7fffff
	if exp == 0xff {
		return uint16(sign<<15 | 0x1f<<10 | mantissa>>13)
	}
	exp = exp - 127 + 15
	if exp >= 0x1f {
		return uint16(sign<<15 | 0x1f<<10)
	}
	if exp <= 0 {
		shift := 23 - 10 - exp + 1
		if shift >= 32 {
			return uint16(sign << 15)
		}
		mantissa |= 0x00800000
		return uint16(sign<<15 | roundShift(mantissa, uint(shift)))
	}
	return uint16(sign<<15 | uint32(exp)<<10 | roundShift(mantissa, 23-10))
}

// roundShift divides by 2^shift rounding to the nearest, ties to even.
func roundShift(i uint32, shift uint) uint32 {
	i += 1 << (shift - 1)
	i -= (i >> shift) & 1
	return i >> shift
}

func shortBitsToHalfFloat(s uint16) float32 {
	sign := uint32(s >> 15)
	exp := int((s >> 10) & 0x1f)
	mantissa := uint32(s & 0x3ff)
	switch {
	case exp == 0x1f:
		exp = 0xff
		mantissa <<= 23 - 10
	case mantissa == 0 && exp == 0:
	default:
		if exp == 0 {
			shift := bits.LeadingZeros32(mantissa) - (32 - 11)
			mantissa = (mantissa << uint(shift)) & 0x3ff
			exp = exp - shift + 1
		}
		exp = exp + 127 - 15
		mantissa <<= 23 - 10
	}
	return math.Float32frombits(sign<<31 | uint32(exp)<<23 | mantissa)
}

func scalingFactor(f *Field) float64 {
	return getFloat(f.Extra, "scaling_factor", 1)
}

// docValue is the value a numeric field keeps in doc values, which sorts,
// aggregations and docvalue_fields read: float has float32 precision,
// half_float binary16, scaled_float the rounded long times 1/factor, and
// integral types the truncated long.
func docValue(f *Field, n float64) float64 {
	if f == nil {
		return n
	}
	switch f.Type {
	case TypeFloat:
		return float64(float32(n))
	case TypeHalfFloat:
		return float64(halfFloat(float32(n)))
	case TypeScaledFloat:
		sf := scalingFactor(f)
		return float64(javaMathRound(n*sf)) * (1 / sf)
	}
	if f.isIntegral() {
		return math.Trunc(n)
	}
	return n
}

// numericOutput renders a doc value of a numeric field: longs for integral
// types, doubles otherwise (float doc values are widened to double).
func numericOutput(f *Field, n float64) any {
	if f != nil && f.Type == TypeUnsignedLong && n >= 9.223372036854775807e18 {
		if n >= 1.8446744073709552e19 {
			return uint64(math.MaxUint64)
		}
		return uint64(n)
	}
	if f != nil && f.isIntegral() {
		return int64(n)
	}
	return Double(n)
}

// sourceNumberOutput renders a numeric value the fields option reads from
// _source: parsed as the field's Java type (float keeps float precision,
// scaled_float is rounded by its factor, half_float is not).
func sourceNumberOutput(f *Field, n float64) any {
	switch f.Type {
	case TypeFloat, TypeHalfFloat:
		return Float(float32(n))
	case TypeScaledFloat:
		sf := scalingFactor(f)
		return Double(float64(javaMathRound(n*sf)) / sf)
	}
	if f.Type == TypeUnsignedLong && n >= 9.223372036854775807e18 {
		if n >= 1.8446744073709552e19 {
			return uint64(math.MaxUint64)
		}
		return uint64(n)
	}
	if f.isIntegral() {
		return int64(math.Trunc(n))
	}
	return Double(n)
}

// geo_point doc values are quantized to 32 bits per coordinate
// (GeoEncodingUtils).
const (
	latDecode = 180.0 / (1 << 32)
	lonDecode = 360.0 / (1 << 32)
)

func encodedLatLon(lat, lon float64) (float64, float64) {
	if lat == 90 {
		lat = math.Nextafter(90, 0)
	}
	if lon == 180 {
		lon = math.Nextafter(180, 0)
	}
	return float64(int32(math.Floor(lat/latDecode))) * latDecode, float64(int32(math.Floor(lon/lonDecode))) * lonDecode
}

// fieldsOutput renders the values of the fields option (read from _source).
func fieldsOutput(f *Field, vals []any, format string) []any {
	out := make([]any, 0, len(vals))
	for _, v := range vals {
		switch t := v.(type) {
		case time.Time:
			out = append(out, dateFormatFor(f, format).Format(t.UTC()))
		case float64:
			if f != nil && f.isNumeric() {
				out = append(out, sourceNumberOutput(f, t))
			} else {
				out = append(out, Double(t))
			}
		case exactInt:
			out = append(out, t.output(f))
		case [2]float64:
			out = append(out, M{"type": "Point", "coordinates": []any{Double(t[1]), Double(t[0])}})
		default:
			out = append(out, v)
		}
	}
	return out
}

func dateFormatFor(f *Field, format string) *DateFormat {
	if format != "" {
		return ParseDateFormat(format)
	}
	if f != nil && f.Format != nil {
		return f.Format
	}
	return ParseDateFormat(DefaultDateFormat)
}

// docValueOutput renders the values of docvalue_fields the way doc values
// hold them: numbers and dates sorted (duplicates kept), keywords and ips
// sorted and deduplicated, geo points as "lat, lon" of the encoded values.
func docValueOutput(f *Field, vals []any, format string) ([]any, error) {
	switch {
	case f.isNumeric():
		if f.Type == TypeUnsignedLong {
			// unsigned_long doc values ignore the format
			format = ""
		}
		nums := make([]exactInt, 0, len(vals))
		for _, v := range vals {
			switch t := v.(type) {
			case float64:
				nums = append(nums, exactInt{n: docValue(f, t)})
			case exactInt:
				nums = append(nums, t)
			}
		}
		sort.SliceStable(nums, func(i, j int) bool { return nums[i].less(nums[j]) })
		out := make([]any, len(nums))
		var df *decimalFormat // the format, compiled once when a value needs it
		dfParsed := false
		for i, e := range nums {
			n := e.n
			if e.exact != "" {
				if format == "" {
					out[i] = e.output(f)
					continue
				}
				if s, ok := e.formatted(format); ok {
					out[i] = s
					continue
				}
			}
			if format != "" {
				if !dfParsed {
					df, _ = parseDecimalFormat(format)
					dfParsed = true
				}
				if df == nil {
					return nil, errIllegalArgument("Invalid format: [%s]", format)
				}
				out[i] = df.format(n)
				continue
			}
			out[i] = numericOutput(f, n)
		}
		return out, nil
	case f.isDate():
		times := make([]time.Time, 0, len(vals))
		for _, v := range vals {
			if t, ok := v.(time.Time); ok {
				times = append(times, t)
			}
		}
		sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
		df := dateFormatFor(f, format)
		out := make([]any, len(times))
		for i, t := range times {
			if f.Type == TypeDate {
				// date doc values have millisecond resolution
				t = time.UnixMilli(epochMillis(t))
			}
			out[i] = df.Format(t.UTC())
		}
		return out, nil
	case f.Type == TypeBoolean:
		falses, trues := 0, 0
		for _, v := range vals {
			if b, ok := v.(bool); ok {
				if b {
					trues++
				} else {
					falses++
				}
			}
		}
		out := make([]any, 0, falses+trues)
		for i := 0; i < falses; i++ {
			out = append(out, false)
		}
		for i := 0; i < trues; i++ {
			out = append(out, true)
		}
		return out, nil
	case f.Type == TypeGeoPoint:
		// doc values hold the encoded points sorted as longs (latitude in
		// the high 32 bits)
		type encoded struct {
			key      int64
			lat, lon float64
		}
		points := make([]encoded, 0, len(vals))
		for _, v := range vals {
			if p, ok := v.([2]float64); ok {
				lat, lon := encodedLatLon(p[0], p[1])
				latBits := int64(int32(math.Floor(lat / latDecode)))
				lonBits := int64(uint32(int32(math.Floor(lon / lonDecode))))
				points = append(points, encoded{key: latBits<<32 | lonBits, lat: lat, lon: lon})
			}
		}
		sort.SliceStable(points, func(i, j int) bool { return points[i].key < points[j].key })
		out := make([]any, 0, len(points))
		for _, p := range points {
			out = append(out, javaNumberString(p.lat, 64)+", "+javaNumberString(p.lon, 64))
		}
		return out, nil
	}
	strs := make([]string, 0, len(vals))
	for _, v := range vals {
		if s, ok := v.(string); ok {
			strs = append(strs, s)
		}
	}
	if f.Type == TypeIP {
		sort.Slice(strs, func(i, j int) bool { return string(ipBytes(strs[i])) < string(ipBytes(strs[j])) })
	} else {
		sort.Strings(strs)
	}
	out := make([]any, 0, len(strs))
	for i, s := range strs {
		if i > 0 && s == strs[i-1] {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// javaInt is the (int) cast of a double: saturating to the 32-bit range,
// NaN to 0, so a conversion does not depend on the width of Go's int.
func javaInt(v float64) int {
	switch {
	case math.IsNaN(v):
		return 0
	case v >= math.MaxInt32:
		return math.MaxInt32
	case v <= math.MinInt32:
		return math.MinInt32
	}
	return int(v)
}

// ipBytes is the 16-byte form ip fields index and sort by.
func ipBytes(s string) []byte {
	if ip := net.ParseIP(s); ip != nil {
		return ip.To16()
	}
	return []byte(s)
}
