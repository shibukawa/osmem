package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// values sources (ValuesSourceConfig) ------------------------------------------

type vsKind int

const (
	vsBytes vsKind = iota
	vsNumeric
	vsDate
	vsBoolean
	vsIP
	vsGeoPoint
	vsRange
)

var vsKindNames = map[vsKind]string{vsBytes: "BYTES", vsNumeric: "NUMERIC", vsDate: "DATE", vsBoolean: "BOOLEAN", vsIP: "IP", vsGeoPoint: "GEOPOINT", vsRange: "RANGE"}

// vsConfig is the values source part of an aggregation body.
type vsConfig struct {
	field      string
	hasField   bool
	missing    any
	hasMissing bool
	valueType  string
	format     string
	hasFormat  bool
	zone       *time.Location
	zoneID     string
	hasZone    bool
	script     bool
}

// valueTypeKind is ValueType.lenientParse.
func valueTypeKind(s string) (vsKind, bool) {
	switch s {
	case "string":
		return vsBytes, true
	case "double", "float", "number", "numeric", "long", "integer", "short", "byte", "unsigned_long":
		return vsNumeric, true
	case "date":
		return vsDate, true
	case "ip":
		return vsIP, true
	case "boolean":
		return vsBoolean, true
	}
	return 0, false
}

// parseVSConfig reads the values source fields of a body already checked
// with objFields.check.
func parseVSConfig(of objFields, body M) (vsConfig, error) {
	var c vsConfig
	if v, ok := body["field"]; ok {
		c.field, _ = v.(string)
		c.hasField = true
	}
	if v, ok := body["missing"]; ok && v != nil {
		c.missing, c.hasMissing = v, true
	}
	if v, ok := body["value_type"]; ok {
		s, _ := v.(string)
		if _, known := valueTypeKind(s); !known {
			return c, of.failed(body, "value_type", errIllegalArgument("Unknown value type [%s]", s))
		}
		c.valueType = s
	}
	if v, ok := body["format"]; ok {
		c.format, _ = v.(string)
		c.hasFormat = true
	}
	if v, ok := body["time_zone"]; ok {
		loc, id, err := parseAggZone(v)
		if err != nil {
			return c, of.failed(body, "time_zone", err)
		}
		c.zone, c.zoneID, c.hasZone = loc, id, true
	}
	if _, ok := body["script"]; ok {
		c.script = true
	}
	return c, nil
}

// valuesSource is a values source resolved for one index.
type valuesSource struct {
	cfg      *vsConfig
	ix       *Index
	name     string
	f        *Field // nil when unmapped
	kind     vsKind
	unmapped bool
	floating bool // numeric values are doubles
	missing  any  // typed missing value: string, float64 or [2]float64
	format   *valueFormat
}

type vsKey struct {
	d    *aggDef
	slot int
	ix   *Index
}

// fieldKind is the values source type of a mapped field.
func fieldKind(f *Field) vsKind {
	switch {
	case f.isNumeric():
		return vsNumeric
	case f.isDate():
		return vsDate
	case f.Type == TypeBoolean:
		return vsBoolean
	case f.Type == TypeIP:
		return vsIP
	case f.Type == TypeGeoPoint:
		return vsGeoPoint
	case f.Type == TypeIntegerRange, f.Type == TypeLongRange, f.Type == TypeFloatRange, f.Type == TypeDoubleRange, f.Type == TypeDateRange:
		return vsRange
	}
	return vsBytes
}

// aggField resolves the mapped field of an aggregation (nil when unmapped).
func aggField(ix *Index, name string) *Field {
	switch name {
	case "_id", "_index", "_routing":
		return &Field{Type: TypeKeyword}
	case "_seq_no", "_version", "_primary_term":
		return &Field{Type: TypeLong}
	}
	f, _, ok := ix.Mapping.resolve(name)
	if !ok || f.Type == TypeObject || f.Type == TypeNested || f.Type == TypeAlias {
		return nil
	}
	return f
}

// resolve resolves (and caches) a values source of an aggregation for the
// index being prepared; registry is the aggregation name used by the
// unsupported type message, allowed the supported values source types.
func (pc *prepareCtx) resolve(d *aggDef, slot int, cfg *vsConfig, registry string, def vsKind, allowed ...vsKind) (*valuesSource, error) {
	vs, err := resolveValuesSource(pc.ix, cfg, registry, def, allowed...)
	if err != nil {
		return nil, err
	}
	pc.ac.setSource(d, slot, pc.ix, vs)
	return vs, nil
}

func (ac *aggContext) setSource(d *aggDef, slot int, ix *Index, vs *valuesSource) {
	if ac.sources == nil {
		ac.sources = map[vsKey]*valuesSource{}
	}
	ac.sources[vsKey{d, slot, ix}] = vs
}

// source returns the values source prepared for an index.
func (ac *aggContext) source(d *aggDef, slot int, ix *Index) *valuesSource {
	return ac.sources[vsKey{d, slot, ix}]
}

func resolveValuesSource(ix *Index, cfg *vsConfig, registry string, def vsKind, allowed ...vsKind) (*valuesSource, error) {
	if cfg.script {
		return nil, errUnsupported("scripts in aggregations")
	}
	if !cfg.hasField {
		return nil, errJava(http.StatusInternalServerError, "illegal_state_exception", "value source config is invalid; must have either a field or a script")
	}
	vs := &valuesSource{cfg: cfg, ix: ix, name: cfg.field}
	hint, hasHint := valueTypeKind(cfg.valueType)
	f := aggField(ix, cfg.field)
	if f == nil {
		vs.unmapped = true
		vs.kind = def
		if hasHint {
			vs.kind = hint
		}
	} else {
		if e := fielddataUnsupported(cfg.field, f); e != nil {
			return nil, e
		}
		if f.Type == TypeText && !getBool(f.Extra, "fielddata", false) {
			return nil, errTextFielddata(cfg.field, nil)
		}
		if !getBool(f.Extra, "doc_values", true) && f.Type != TypeText {
			return nil, errIllegalArgument("Can't load fielddata on [%s] because fielddata is unsupported on fields of type [%s]. Use doc values instead.", cfg.field, f.Type)
		}
		vs.f = f
		vs.kind = fieldKind(f)
		if hasHint {
			vs.kind = hint
		}
	}
	format, err := resolveFormat(vs, f, cfg)
	if err != nil {
		return nil, err
	}
	vs.format = format
	if f != nil && hasHint {
		fk := fieldKind(f)
		switch vs.kind {
		case vsNumeric, vsDate, vsBoolean:
			if fk != vsNumeric && fk != vsDate && fk != vsBoolean {
				return nil, errIllegalArgument("Expected numeric type on field [%s], but got [%s]", cfg.field, f.Type)
			}
		case vsIP:
			if fk != vsIP && fk != vsBytes {
				return nil, errIllegalArgument("Expected ip type on field [%s], but got [%s]", cfg.field, f.Type)
			}
		}
	}
	if vs.kind == vsNumeric {
		vs.floating = f != nil && fieldKind(f) == vsNumeric && !f.isIntegral()
		if f != nil && (hasHint && (cfg.valueType == "double" || cfg.valueType == "float")) && fieldKind(f) != vsNumeric {
			vs.floating = true
		}
	}
	if cfg.hasMissing {
		m, err := parseMissing(vs, cfg.missing)
		if err != nil {
			return nil, err
		}
		vs.missing = m
		if n, ok := m.(float64); ok && vs.kind == vsNumeric && math.Mod(n, 1) != 0 {
			vs.floating = true
		}
	}
	supported := false
	for _, k := range allowed {
		if k == vs.kind {
			supported = true
		}
	}
	if !supported {
		if f == nil {
			return nil, errIllegalArgument("unmapped field with value source type [%s] is not supported for aggregation [%s]", strings.ToLower(vsKindNames[vs.kind]), registry)
		}
		return nil, errIllegalArgument("Field [%s] of type [%s] is not supported for aggregation [%s]", cfg.field, f.Type, registry)
	}
	return vs, nil
}

// resolveFormat is MappedFieldType.docValueFormat for mapped fields and
// ValuesSourceType.getFormatter otherwise.
func resolveFormat(vs *valuesSource, f *Field, cfg *vsConfig) (*valueFormat, error) {
	loc := time.UTC
	if cfg.hasZone {
		loc = cfg.zone
	}
	if f != nil {
		switch fieldKind(f) {
		case vsNumeric:
			if cfg.hasZone {
				return nil, errIllegalArgument("Field [%s] of type [%s] does not support custom time zones", cfg.field, f.Type)
			}
			if cfg.hasFormat {
				return decimalValueFormat(cfg.format)
			}
			if f.Type == TypeUnsignedLong {
				return unsignedLongFormat, nil
			}
			return rawFormat, nil
		case vsDate:
			df := f.Format
			if df == nil {
				df = ParseDateFormat(DefaultDateFormat)
			}
			if cfg.hasFormat {
				df = ParseDateFormat(cfg.format)
			}
			return &valueFormat{kind: fmtDate, date: df, loc: loc}, nil
		}
		if cfg.hasFormat {
			return nil, errIllegalArgument("Field [%s] of type [%s] does not support custom formats", cfg.field, f.Type)
		}
		if cfg.hasZone {
			return nil, errIllegalArgument("Field [%s] of type [%s] does not support custom time zones", cfg.field, f.Type)
		}
		switch fieldKind(f) {
		case vsBoolean:
			return boolFormat, nil
		case vsIP:
			return ipFormat, nil
		case vsGeoPoint:
			return geohashFormat, nil
		}
		return rawFormat, nil
	}
	switch vs.kind {
	case vsNumeric:
		if cfg.hasFormat {
			return decimalValueFormat(cfg.format)
		}
	case vsDate:
		df := ParseDateFormat(DefaultDateFormat)
		if cfg.hasFormat {
			df = ParseDateFormat(cfg.format)
		}
		return &valueFormat{kind: fmtDate, date: df, loc: loc}, nil
	case vsBoolean:
		return boolFormat, nil
	case vsIP:
		return ipFormat, nil
	case vsGeoPoint:
		return geohashFormat, nil
	}
	return rawFormat, nil
}

// parseMissing converts the missing parameter to a value of the source
// (ValuesSourceType.replaceMissing).
func parseMissing(vs *valuesSource, raw any) (any, error) {
	s := missingString(raw)
	switch vs.kind {
	case vsNumeric:
		return aggParseDouble(s, func(s string) *Error { return aggNumberFormatError(s) })
	case vsBoolean:
		switch s {
		case "true":
			return 1.0, nil
		case "false":
			return 0.0, nil
		}
		return nil, errIllegalArgument("Cannot parse boolean [%s], expected either [true] or [false]", s)
	case vsDate:
		df := vs.format.date
		if vs.f != nil && vs.f.Format != nil && !vs.cfg.hasFormat {
			df = vs.f.Format
		}
		t, err := ParseDateMath(s, df, time.Now(), time.UTC, false)
		if err != nil {
			return nil, errDateParse(s, df)
		}
		return float64(t.UnixMilli()), nil
	case vsIP:
		if net.ParseIP(s) == nil {
			return nil, errIllegalArgument("'%s' is not an IP string literal.", s)
		}
		return canonicalIP(s), nil
	case vsGeoPoint:
		lat, lon, ok := geoPointValue(s)
		if !ok {
			return nil, &Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "unsupported symbol [" + s + "] in geohash [" + s + "]"}
		}
		return [2]float64{lat, lon}, nil
	}
	return s, nil
}

func missingString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return javaNumberString(t, 64)
	}
	return fmt.Sprint(v)
}

// errDateParse is the failure of a date parser in a values source.
func errDateParse(input string, df *DateFormat) *Error {
	msg := fmt.Sprintf("failed to parse date field [%s] with format [%s]", input, df.Source)
	inner := "Failed to parse with all enclosed parsers"
	if _, de := df.parseDate(input, false, time.UTC); de != nil && de.reason != "" {
		inner = de.reason
	}
	return &Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: msg + ": [" + msg + "]",
		Cause: &Error{Type: "illegal_argument_exception", Reason: msg, Cause: &Error{Type: "date_time_parse_exception", Reason: inner}}}
}

func canonicalIP(s string) string {
	ip := net.ParseIP(s)
	if ip == nil {
		return s
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

// doc values -----------------------------------------------------------------

// nums returns the numeric doc values of a hit in ascending order (dates as
// epoch millis, booleans as 0 and 1), or the missing value.
func (vs *valuesSource) nums(h *hit) []float64 {
	var out []float64
	if vs.f != nil {
		for _, v := range h.ix.fieldValues(h.doc, vs.name) {
			switch t := v.(type) {
			case float64:
				out = append(out, docValue(vs.f, t))
			case exactInt:
				out = append(out, docValue(vs.f, t.n))
			case time.Time:
				out = append(out, float64(t.UnixMilli()))
			case bool:
				if t {
					out = append(out, 1)
				} else {
					out = append(out, 0)
				}
			}
		}
		sort.Float64s(out)
	}
	if len(out) == 0 && vs.missing != nil {
		if n, ok := vs.missing.(float64); ok {
			return []float64{n}
		}
	}
	return out
}

// strs returns the terms of a hit: unique and sorted like SortedSetDocValues
// (ip addresses by their binary form), or the missing value.
func (vs *valuesSource) strs(h *hit) []string {
	var out []string
	if vs.f != nil {
		for _, v := range h.ix.fieldValues(h.doc, vs.name) {
			switch t := v.(type) {
			case string:
				if vs.kind == vsIP || vs.f.Type == TypeIP {
					t = canonicalIP(t)
				}
				out = append(out, t)
			case float64:
				n := docValue(vs.f, t)
				if vs.f.isIntegral() {
					out = append(out, strconv.FormatInt(int64(n), 10))
				} else {
					out = append(out, javaNumberString(n, 64))
				}
			case exactInt:
				if t.exact != "" {
					out = append(out, t.exact)
				} else {
					out = append(out, strconv.FormatInt(int64(docValue(vs.f, t.n)), 10))
				}
			case time.Time:
				out = append(out, strconv.FormatInt(t.UnixMilli(), 10))
			case bool:
				if t {
					out = append(out, "T")
				} else {
					out = append(out, "F")
				}
			}
		}
		if vs.kind == vsIP {
			sort.Slice(out, func(i, j int) bool { return string(ipBytes(out[i])) < string(ipBytes(out[j])) })
		} else {
			sort.Strings(out)
		}
		uniq := out[:0]
		for i, s := range out {
			if i == 0 || s != out[i-1] {
				uniq = append(uniq, s)
			}
		}
		out = uniq
	}
	if len(out) == 0 && vs.missing != nil {
		if s, ok := vs.missing.(string); ok {
			return []string{s}
		}
	}
	return out
}

// points returns the geo points of a hit ([lat, lon] as indexed).
func (vs *valuesSource) points(h *hit) [][2]float64 {
	var out [][2]float64
	if vs.f != nil {
		for _, v := range h.ix.fieldValues(h.doc, vs.name) {
			if p, ok := v.([2]float64); ok {
				lat, lon := encodedLatLon(p[0], p[1])
				out = append(out, [2]float64{lat, lon})
			}
		}
	}
	if len(out) == 0 {
		if p, ok := vs.missing.([2]float64); ok {
			return [][2]float64{p}
		}
	}
	return out
}

// hasValue reports whether a hit has a value (or the missing value).
func (vs *valuesSource) hasValue(h *hit) bool {
	switch vs.kind {
	case vsBytes, vsIP:
		return len(vs.strs(h)) > 0
	case vsGeoPoint:
		return len(vs.points(h)) > 0
	}
	return len(vs.nums(h)) > 0
}

// DocValueFormat ------------------------------------------------------------------

const (
	fmtRaw = iota
	fmtDate
	fmtBool
	fmtIP
	fmtDecimal
	fmtGeoHash
	fmtGeoTile
	fmtUnsignedLong
)

type valueFormat struct {
	kind    int
	date    *DateFormat
	loc     *time.Location
	pattern string
}

var (
	rawFormat          = &valueFormat{kind: fmtRaw}
	boolFormat         = &valueFormat{kind: fmtBool}
	ipFormat           = &valueFormat{kind: fmtIP}
	geohashFormat      = &valueFormat{kind: fmtGeoHash}
	unsignedLongFormat = &valueFormat{kind: fmtUnsignedLong}
)

func decimalValueFormat(pattern string) (*valueFormat, error) {
	if _, ok := parseDecimalFormat(pattern); !ok && strings.ContainsAny(pattern, "0#") {
		return nil, errIllegalArgument("Malformed pattern \"%s\"", pattern)
	}
	return &valueFormat{kind: fmtDecimal, pattern: pattern}, nil
}

func (vf *valueFormat) raw() bool { return vf == nil || vf.kind == fmtRaw }

// javaLong is the (long) cast of a double: saturating, NaN to 0.
func javaLong(v float64) int64 {
	switch {
	case math.IsNaN(v):
		return 0
	case v >= math.MaxInt64:
		return math.MaxInt64
	case v <= math.MinInt64:
		return math.MinInt64
	}
	return int64(v)
}

// formatDate renders epoch millis with java.time's year sign rules: years
// beyond four digits get a leading '+'.
func (vf *valueFormat) formatDate(ms int64) string {
	t := time.UnixMilli(ms).In(vf.loc)
	s := vf.date.Format(t)
	if y := t.Year(); y > 9999 {
		ys := strconv.Itoa(y)
		if i := strings.Index(s, ys); i >= 0 {
			s = s[:i] + "+" + s[i:]
		}
	}
	return s
}

// formatDouble is DocValueFormat.format(double) (an Object).
func (vf *valueFormat) formatDouble(v float64) any {
	switch vf.kind {
	case fmtDate:
		return vf.formatDate(javaLong(v))
	case fmtBool:
		return v != 0
	case fmtDecimal:
		return decimalString(vf.pattern, v)
	}
	return v
}

// decimalString formats with a DecimalFormat pattern; a pattern without digit
// placeholders is a prefix for the integral number, as in Java.
func decimalString(pattern string, v float64) string {
	if s, ok := formatDecimal(pattern, v); ok {
		return s
	}
	if !strings.ContainsAny(pattern, "0#.,E%\u2030;") {
		s, _ := formatDecimal("#", v)
		return strings.ReplaceAll(pattern, "''", "'") + s
	}
	return javaDoubleToString(v)
}

// formatLong is DocValueFormat.format(long).
func (vf *valueFormat) formatLong(v int64) any {
	switch vf.kind {
	case fmtDate:
		return vf.formatDate(v)
	case fmtBool:
		return v != 0
	case fmtDecimal:
		return decimalString(vf.pattern, float64(v))
	}
	return v
}

// stringDouble is format(double).toString().
func (vf *valueFormat) stringDouble(v float64) string { return objectString(vf.formatDouble(v)) }

// stringLong is format(long).toString().
func (vf *valueFormat) stringLong(v int64) string { return objectString(vf.formatLong(v)) }

func objectString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return javaDoubleToString(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return strconv.FormatBool(t)
	}
	return fmt.Sprint(v)
}

// javaDoubleToString is Double.toString, including NaN and infinities.
func javaDoubleToString(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Infinity"
	case math.IsInf(v, -1):
		return "-Infinity"
	}
	return javaNumberString(v, 64)
}

// prepare ---------------------------------------------------------------------------

// prepareCtx runs the shard-level checks of the aggregations for one index.
type prepareCtx struct {
	ac     *aggContext
	ix     *Index
	nested []string
}

func (pc *prepareCtx) prepareLevel(defs []*aggDef, parent *aggDef) error {
	for _, d := range defs {
		if d.typ.prepare != nil {
			if err := d.typ.prepare(pc, d); err != nil {
				return shardError(err, pc.ix)
			}
		}
		saved := pc.nested
		switch d.kind {
		case "nested":
			pc.nested = append(append([]string(nil), saved...), getString(d.body, "path"))
		case "reverse_nested":
			path := getString(d.body, "path")
			var kept []string
			for _, p := range saved {
				if path != "" && (p == path || hasPrefixDot(path, p)) {
					kept = append(kept, p)
				}
			}
			pc.nested = kept
		}
		err := pc.prepareLevel(d.subs, d)
		pc.nested = saved
		if err != nil {
			return err
		}
	}
	return nil
}
