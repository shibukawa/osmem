package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultDateFormat is the default format of OpenSearch date fields.
const DefaultDateFormat = "strict_date_optional_time||epoch_millis"

// DateFormat is a parsed OpenSearch date format ("a||b||c").
type DateFormat struct {
	Source     string
	formatters []*dtFormatter
	err        error
}

// dtFormatter is one element of a format: a named format or a pattern.
type dtFormatter struct {
	name      string
	parsers   []*dtSeq
	printer   *dtSeq
	epoch     bool
	seconds   bool
	proleptic bool // uses YEAR: dates are validated while parsing
	doy       bool
	week      bool
}

var (
	dateFormatCache   = map[string]*DateFormat{}
	dateFormatCacheMu sync.RWMutex
)

// ParseDateFormat parses a format string. An invalid format is kept with
// its error (see Err) and parses nothing.
func ParseDateFormat(s string) *DateFormat {
	if s == "" {
		s = DefaultDateFormat
	}
	dateFormatCacheMu.RLock()
	f, ok := dateFormatCache[s]
	dateFormatCacheMu.RUnlock()
	if ok {
		return f
	}
	df := &DateFormat{Source: s}
	df.formatters, df.err = compileDateFormat(s)
	dateFormatCacheMu.Lock()
	dateFormatCache[s] = df
	dateFormatCacheMu.Unlock()
	return df
}

// parseDateFormatChecked parses a mapping format, reporting the errors
// DateFormatter.forPattern raises.
func parseDateFormatChecked(s string) (*DateFormat, error) {
	if s == "" {
		return nil, errIllegalArgument("No date pattern provided")
	}
	df := ParseDateFormat(s)
	if df.err != nil {
		return nil, df.err
	}
	return df, nil
}

// Err reports why a format is invalid.
func (df *DateFormat) Err() error { return df.err }

func compileDateFormat(s string) ([]*dtFormatter, error) {
	format := s
	if strings.HasPrefix(format, "8") {
		format = format[1:]
	}
	parts := strings.Split(format, "||")
	out := make([]*dtFormatter, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, errIllegalArgument("Cannot have empty element in multi date format pattern: %s", s)
		}
		if f := namedDateFormatter(part); f != nil {
			out = append(out, f)
			continue
		}
		b, err := compileJavaPattern(part)
		if err != nil {
			return nil, &Error{Status: 400, Type: "illegal_argument_exception", Reason: "Invalid format: [" + part + "]: " + err.Error(),
				Cause: &Error{Type: "illegal_argument_exception", Reason: err.Error()}}
		}
		seq := b.seq()
		out = append(out, &dtFormatter{name: part, parsers: []*dtSeq{seq}, printer: seq, proleptic: b.hasYear, doy: b.hasDOY, week: b.hasWeek})
	}
	return out, nil
}

// named formats --------------------------------------------------------------

var namedFormatterCache sync.Map

func namedDateFormatter(name string) *dtFormatter {
	if v, ok := namedFormatterCache.Load(name); ok {
		return v.(*dtFormatter)
	}
	build, ok := namedFormatBuilders[name]
	if !ok {
		return nil
	}
	f := build()
	f.name = name
	namedFormatterCache.Store(name, f)
	return f
}

func strictYMD(b *dtBuilder, withMonth, withDay bool) {
	b.num(dtYear, 4, 10, signExceedsPad)
	if withMonth {
		b.lit("-").num(dtMonth, 2, 2, signNotNegative)
	}
	if withDay {
		b.lit("-").num(dtDay, 2, 2, signNotNegative)
	}
}

func lenientYMD(b *dtBuilder, withMonth, withDay bool) {
	b.num(dtYear, 1, 5, signNormal)
	if withMonth {
		b.lit("-").num(dtMonth, 1, 2, signNotNegative)
	}
	if withDay {
		b.lit("-").num(dtDay, 1, 2, signNotNegative)
	}
}

// zoneParser accepts Z, offsets with or without colon and zone ids.
func zoneParser(b *dtBuilder) {
	b.opt().elem(dtZoneElem{}).end()
	b.opt().elem(dtOffsetElem{pattern: "+HHmm", noOffset: "Z"}).end()
}

func timeParts(b *dtBuilder, strict bool, hour, minute, second bool, fraction int, sep bool) {
	w := 2
	if !strict {
		w = 1
	}
	if hour {
		b.num(dtHourOfDay, w, 2, signNotNegative)
	}
	if minute {
		if sep {
			b.lit(":")
		}
		b.num(dtMinute, w, 2, signNotNegative)
	}
	if second {
		if sep {
			b.lit(":")
		}
		b.num(dtSecond, w, 2, signNotNegative)
	}
	switch fraction {
	case 1: // required fraction
		b.frac(1, 9, true)
	case 3:
		b.frac(3, 9, true)
	}
}

func isoPrinter(nanos bool) *dtSeq {
	b := newDTBuilder()
	b.num(dtYear, 4, 9, signExceedsPad).lit("-").num(dtMonth, 2, 2, signNotNegative).lit("-").num(dtDay, 2, 2, signNotNegative)
	b.lit("T").num(dtHourOfDay, 2, 2, signNotNegative).lit(":").num(dtMinute, 2, 2, signNotNegative).lit(":").num(dtSecond, 2, 2, signNotNegative)
	if nanos {
		b.frac(3, 9, true)
	} else {
		b.frac(3, 3, true)
	}
	b.elem(dtOffsetElem{pattern: "+HH:MM", noOffset: "Z"})
	return b.seq()
}

func patternFormatter(pattern string) *dtFormatter {
	b, err := compileJavaPattern(pattern)
	if err != nil {
		panic(err)
	}
	seq := b.seq()
	return &dtFormatter{parsers: []*dtSeq{seq}, printer: seq, proleptic: b.hasYear, doy: b.hasDOY, week: b.hasWeek}
}

func optionalTimeParser(strict bool) *dtSeq {
	b := newDTBuilder()
	if strict {
		b.num(dtYear, 4, 10, signExceedsPad)
		b.opt().lit("-").num(dtMonth, 2, 2, signNotNegative)
		b.opt().lit("-").num(dtDay, 2, 2, signNotNegative).end()
		b.end()
	} else {
		b.num(dtYear, 1, 5, signNormal)
		b.opt().lit("-").num(dtMonth, 1, 2, signNotNegative)
		b.opt().lit("-").num(dtDay, 1, 2, signNotNegative).end()
		b.end()
	}
	w := 2
	if !strict {
		w = 1
	}
	b.opt().lit("T")
	b.opt().num(dtHourOfDay, w, 2, signNotNegative)
	b.opt().lit(":").num(dtMinute, w, 2, signNotNegative)
	b.opt().lit(":").num(dtSecond, w, 2, signNotNegative)
	b.opt().frac(1, 9, true).end()
	b.opt().lit(",").frac(1, 9, false).end()
	b.end() // seconds
	b.end() // minutes
	zoneParser(b)
	b.end() // hour
	b.end() // T
	return b.seq()
}

func namedSimple(parser func(b *dtBuilder), printer func(b *dtBuilder)) func() *dtFormatter {
	return func() *dtFormatter {
		pb := newDTBuilder()
		parser(pb)
		f := &dtFormatter{parsers: []*dtSeq{pb.seq()}, proleptic: pb.hasYear, doy: pb.hasDOY, week: pb.hasWeek}
		if printer != nil {
			qb := newDTBuilder()
			printer(qb)
			f.printer = qb.seq()
		} else {
			f.printer = f.parsers[0]
		}
		return f
	}
}

// namedFormatBuilders is initialized with the package variables so that
// formats parsed during initialization see the named formats.
var namedFormatBuilders = buildNamedFormats()

func buildNamedFormats() map[string]func() *dtFormatter {
	epoch := func(seconds bool) func() *dtFormatter {
		return func() *dtFormatter {
			seq := &dtSeq{elems: []dtElem{dtEpoch{seconds: seconds}}}
			return &dtFormatter{parsers: []*dtSeq{seq, {elems: []dtElem{dtEpoch{seconds: seconds}}}}, printer: seq, epoch: true, seconds: seconds}
		}
	}
	optionalTime := func(strict, nanos bool) func() *dtFormatter {
		return func() *dtFormatter {
			parsers := []*dtSeq{optionalTimeParser(strict)}
			if !strict {
				parsers = append(parsers, optionalTimeParser(strict))
			}
			return &dtFormatter{parsers: parsers, printer: isoPrinter(nanos), proleptic: true}
		}
	}
	ymd := func(strict, month, day bool) func(b *dtBuilder) {
		return func(b *dtBuilder) {
			if strict {
				strictYMD(b, month, day)
			} else {
				lenientYMD(b, month, day)
			}
		}
	}
	printYMD := func(month, day bool) func(b *dtBuilder) {
		return func(b *dtBuilder) { strictYMD(b, month, day) }
	}
	dateTime := func(strict, hour, minute, second bool, fraction int, zone bool, tPrefix bool, withDate bool) func(b *dtBuilder) {
		return func(b *dtBuilder) {
			if withDate {
				if strict {
					strictYMD(b, true, true)
				} else {
					lenientYMD(b, true, true)
				}
			}
			if tPrefix {
				b.lit("T")
			}
			timeParts(b, strict, hour, minute, second, fraction, true)
			if zone {
				b.elem(dtZoneElem{})
			}
		}
	}
	printDateTime := func(hour, minute, second bool, fraction int, zone, tPrefix, withDate bool) func(b *dtBuilder) {
		return func(b *dtBuilder) {
			if withDate {
				strictYMD(b, true, true)
			}
			if tPrefix {
				b.lit("T")
			}
			timeParts(b, true, hour, minute, second, 0, true)
			if fraction > 0 {
				b.frac(3, 3, true)
			}
			if zone {
				b.elem(dtOffsetElem{pattern: "+HH:MM", noOffset: "Z"})
			}
		}
	}
	basic := func(pattern string) func() *dtFormatter {
		return func() *dtFormatter { return patternFormatter(pattern) }
	}
	return map[string]func() *dtFormatter{
		"epoch_millis":                            epoch(false),
		"epoch_second":                            epoch(true),
		"strict_date_optional_time":               optionalTime(true, false),
		"date_optional_time":                      optionalTime(false, false),
		"strict_date_optional_time_nanos":         optionalTime(true, true),
		"iso8601":                                 optionalTime(false, false),
		"rfc3339_lenient":                         optionalTime(true, false),
		"basic_date":                              basic("uuuuMMdd"),
		"basic_date_time":                         basic("uuuuMMdd'T'HHmmss.SSSX"),
		"basic_date_time_no_millis":               basic("uuuuMMdd'T'HHmmssX"),
		"basic_ordinal_date":                      basic("uuuuDDD"),
		"basic_ordinal_date_time":                 basic("uuuuDDD'T'HHmmss.SSSX"),
		"basic_ordinal_date_time_no_millis":       basic("uuuuDDD'T'HHmmssX"),
		"basic_time":                              basic("HHmmss.SSSX"),
		"basic_time_no_millis":                    basic("HHmmssX"),
		"basic_t_time":                            basic("'T'HHmmss.SSSX"),
		"basic_t_time_no_millis":                  basic("'T'HHmmssX"),
		"basic_week_date":                         basic("YYYY'W'wwe"),
		"strict_basic_week_date":                  basic("YYYY'W'wwe"),
		"basic_week_date_time":                    basic("YYYY'W'wwe'T'HHmmss.SSSX"),
		"strict_basic_week_date_time":             basic("YYYY'W'wwe'T'HHmmss.SSSX"),
		"basic_week_date_time_no_millis":          basic("YYYY'W'wwe'T'HHmmssX"),
		"strict_basic_week_date_time_no_millis":   basic("YYYY'W'wwe'T'HHmmssX"),
		"date":                                    namedSimple(ymd(false, true, true), printYMD(true, true)),
		"strict_date":                             namedSimple(ymd(true, true, true), printYMD(true, true)),
		"year_month_day":                          namedSimple(ymd(false, true, true), printYMD(true, true)),
		"strict_year_month_day":                   namedSimple(ymd(true, true, true), printYMD(true, true)),
		"year_month":                              namedSimple(ymd(false, true, false), printYMD(true, false)),
		"strict_year_month":                       namedSimple(ymd(true, true, false), printYMD(true, false)),
		"year":                                    namedSimple(ymd(false, false, false), printYMD(false, false)),
		"strict_year":                             namedSimple(ymd(true, false, false), printYMD(false, false)),
		"date_hour":                               namedSimple(dateTime(false, true, false, false, 0, false, true, true), printDateTime(true, false, false, 0, false, true, true)),
		"strict_date_hour":                        namedSimple(dateTime(true, true, false, false, 0, false, true, true), printDateTime(true, false, false, 0, false, true, true)),
		"date_hour_minute":                        namedSimple(dateTime(false, true, true, false, 0, false, true, true), printDateTime(true, true, false, 0, false, true, true)),
		"strict_date_hour_minute":                 namedSimple(dateTime(true, true, true, false, 0, false, true, true), printDateTime(true, true, false, 0, false, true, true)),
		"date_hour_minute_second":                 namedSimple(dateTime(false, true, true, true, 0, false, true, true), printDateTime(true, true, true, 0, false, true, true)),
		"strict_date_hour_minute_second":          namedSimple(dateTime(true, true, true, true, 0, false, true, true), printDateTime(true, true, true, 0, false, true, true)),
		"date_hour_minute_second_fraction":        namedSimple(dateTime(false, true, true, true, 1, false, true, true), printDateTime(true, true, true, 3, false, true, true)),
		"strict_date_hour_minute_second_fraction": namedSimple(dateTime(true, true, true, true, 3, false, true, true), printDateTime(true, true, true, 3, false, true, true)),
		"date_hour_minute_second_millis":          namedSimple(dateTime(false, true, true, true, 1, false, true, true), printDateTime(true, true, true, 3, false, true, true)),
		"strict_date_hour_minute_second_millis":   namedSimple(dateTime(true, true, true, true, 3, false, true, true), printDateTime(true, true, true, 3, false, true, true)),
		"date_time":                               namedSimple(dateTime(false, true, true, true, 1, true, true, true), printDateTime(true, true, true, 3, true, true, true)),
		"strict_date_time":                        namedSimple(dateTime(true, true, true, true, 3, true, true, true), printDateTime(true, true, true, 3, true, true, true)),
		"date_time_no_millis":                     namedSimple(dateTime(false, true, true, true, 0, true, true, true), printDateTime(true, true, true, 0, true, true, true)),
		"strict_date_time_no_millis":              namedSimple(dateTime(true, true, true, true, 0, true, true, true), printDateTime(true, true, true, 0, true, true, true)),
		"hour":                                    namedSimple(dateTime(false, true, false, false, 0, false, false, false), printDateTime(true, false, false, 0, false, false, false)),
		"strict_hour":                             namedSimple(dateTime(true, true, false, false, 0, false, false, false), printDateTime(true, false, false, 0, false, false, false)),
		"hour_minute":                             namedSimple(dateTime(false, true, true, false, 0, false, false, false), printDateTime(true, true, false, 0, false, false, false)),
		"strict_hour_minute":                      namedSimple(dateTime(true, true, true, false, 0, false, false, false), printDateTime(true, true, false, 0, false, false, false)),
		"hour_minute_second":                      namedSimple(dateTime(false, true, true, true, 0, false, false, false), printDateTime(true, true, true, 0, false, false, false)),
		"strict_hour_minute_second":               namedSimple(dateTime(true, true, true, true, 0, false, false, false), printDateTime(true, true, true, 0, false, false, false)),
		"hour_minute_second_fraction":             namedSimple(dateTime(false, true, true, true, 1, false, false, false), printDateTime(true, true, true, 3, false, false, false)),
		"strict_hour_minute_second_fraction":      namedSimple(dateTime(true, true, true, true, 3, false, false, false), printDateTime(true, true, true, 3, false, false, false)),
		"hour_minute_second_millis":               namedSimple(dateTime(false, true, true, true, 1, false, false, false), printDateTime(true, true, true, 3, false, false, false)),
		"strict_hour_minute_second_millis":        namedSimple(dateTime(true, true, true, true, 3, false, false, false), printDateTime(true, true, true, 3, false, false, false)),
		"time":                                    namedSimple(dateTime(false, true, true, true, 1, true, false, false), printDateTime(true, true, true, 3, true, false, false)),
		"strict_time":                             namedSimple(dateTime(true, true, true, true, 3, true, false, false), printDateTime(true, true, true, 3, true, false, false)),
		"time_no_millis":                          namedSimple(dateTime(false, true, true, true, 0, true, false, false), printDateTime(true, true, true, 0, true, false, false)),
		"strict_time_no_millis":                   namedSimple(dateTime(true, true, true, true, 0, true, false, false), printDateTime(true, true, true, 0, true, false, false)),
		"t_time":                                  namedSimple(dateTime(false, true, true, true, 1, true, true, false), printDateTime(true, true, true, 3, true, true, false)),
		"strict_t_time":                           namedSimple(dateTime(true, true, true, true, 3, true, true, false), printDateTime(true, true, true, 3, true, true, false)),
		"t_time_no_millis":                        namedSimple(dateTime(false, true, true, true, 0, true, true, false), printDateTime(true, true, true, 0, true, true, false)),
		"strict_t_time_no_millis":                 namedSimple(dateTime(true, true, true, true, 0, true, true, false), printDateTime(true, true, true, 0, true, true, false)),
		"ordinal_date":                            basic("uuuu-DDD"),
		"strict_ordinal_date":                     basic("uuuu-DDD"),
		"ordinal_date_time":                       basic("uuuu-DDD'T'HH:mm:ss.SSSXXX"),
		"strict_ordinal_date_time":                basic("uuuu-DDD'T'HH:mm:ss.SSSXXX"),
		"ordinal_date_time_no_millis":             basic("uuuu-DDD'T'HH:mm:ssXXX"),
		"strict_ordinal_date_time_no_millis":      basic("uuuu-DDD'T'HH:mm:ssXXX"),
		"weekyear":                                basic("YYYY"),
		"strict_weekyear":                         basic("YYYY"),
		"weekyear_week":                           basic("YYYY-'W'ww"),
		"strict_weekyear_week":                    basic("YYYY-'W'ww"),
		"weekyear_week_day":                       basic("YYYY-'W'ww-e"),
		"strict_weekyear_week_day":                basic("YYYY-'W'ww-e"),
		"week_date":                               basic("YYYY-'W'ww-e"),
		"strict_week_date":                        basic("YYYY-'W'ww-e"),
		"week_date_time":                          basic("YYYY-'W'ww-e'T'HH:mm:ss.SSSXXX"),
		"strict_week_date_time":                   basic("YYYY-'W'ww-e'T'HH:mm:ss.SSSXXX"),
		"week_date_time_no_millis":                basic("YYYY-'W'ww-e'T'HH:mm:ssXXX"),
		"strict_week_date_time_no_millis":         basic("YYYY-'W'ww-e'T'HH:mm:ssXXX"),
	}
}

// parsing ------------------------------------------------------------------------

const (
	dateErrParse    = iota // DateTimeParseException wrapped in "failed to parse date field"
	dateErrDateTime        // DateTimeException raised while building the date
	dateErrEmpty           // "cannot parse empty date"
	dateErrOther           // other IllegalArgumentException (nanosecond range, math)
)

// dateError is a failure to parse a date with a format.
type dateError struct {
	kind   int
	input  string
	format string
	reason string // cause reason (date_time_parse_exception or date_time_exception) or the message
}

func (e *dateError) Error() string {
	switch e.kind {
	case dateErrParse:
		return fmt.Sprintf("failed to parse date field [%s] with format [%s]", e.input, e.format)
	case dateErrDateTime:
		return e.reason
	}
	return e.reason
}

// cause renders the exception a date field reports while indexing.
func (e *dateError) cause() *Error {
	switch e.kind {
	case dateErrParse:
		return &Error{Type: "illegal_argument_exception", Reason: e.Error(),
			Cause: &Error{Type: "date_time_parse_exception", Reason: e.reason}}
	case dateErrDateTime:
		return &Error{Type: "date_time_exception", Reason: e.reason, plain: true}
	}
	return &Error{Type: "illegal_argument_exception", Reason: e.reason}
}

// queryError renders the parse failure of a date in a range or term query
// (JavaDateMathParser's OpenSearchParseException).
func (e *dateError) queryError() *Error {
	switch e.kind {
	case dateErrParse, dateErrDateTime:
		return &Error{Status: 400, Type: "parse_exception",
			Reason: fmt.Sprintf("failed to parse date field [%s] with format [%s]: [%s]", e.input, e.format, e.Error()), Cause: e.cause()}
	case dateErrEmpty, dateErrMath:
		return &Error{Status: 400, Type: "parse_exception", Reason: e.reason}
	}
	return &Error{Status: 400, Type: "illegal_argument_exception", Reason: e.reason}
}

func abbreviateDateText(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

// dtResult is a successfully parsed date.
type dtResult struct {
	t       time.Time
	hasZone bool
}

// parseDate parses s with the format. roundUp applies the defaults of the
// round-up parser range queries use for upper bounds; loc is the zone of
// dates without one.
func (df *DateFormat) parseDate(s string, roundUp bool, loc *time.Location) (dtResult, *dateError) {
	if loc == nil {
		loc = time.UTC
	}
	if df.err != nil {
		return dtResult{}, &dateError{kind: dateErrOther, input: s, format: df.Source, reason: df.err.Error()}
	}
	if s == "" {
		return dtResult{}, &dateError{kind: dateErrEmpty, input: s, format: df.Source, reason: "cannot parse empty date"}
	}
	total := 0
	for _, f := range df.formatters {
		total += len(f.parsers)
	}
	if total == 1 {
		f := df.formatters[0]
		p := &dtParsed{}
		end := f.parsers[0].parse(p, s, 0)
		if end < 0 {
			return dtResult{}, &dateError{kind: dateErrParse, input: s, format: df.Source,
				reason: fmt.Sprintf("Text '%s' could not be parsed at index %d", abbreviateDateText(s), ^end)}
		}
		if end != len(s) {
			return dtResult{}, &dateError{kind: dateErrParse, input: s, format: df.Source,
				reason: fmt.Sprintf("Text '%s' could not be parsed, unparsed text found at index %d", abbreviateDateText(s), end)}
		}
		f.applyDefaults(p, roundUp)
		if msg := f.resolveStrict(p); msg != "" {
			return dtResult{}, &dateError{kind: dateErrParse, input: s, format: df.Source,
				reason: fmt.Sprintf("Text '%s' could not be parsed: %s", abbreviateDateText(s), msg)}
		}
		return f.from(p, s, df.Source, loc)
	}
	for _, f := range df.formatters {
		for _, parser := range f.parsers {
			p := &dtParsed{}
			end := parser.parse(p, s, 0)
			if end < 0 || end != len(s) {
				continue
			}
			f.applyDefaults(p, roundUp)
			if msg := f.resolveStrict(p); msg != "" {
				continue
			}
			return f.from(p, s, df.Source, loc)
		}
	}
	return dtResult{}, &dateError{kind: dateErrParse, input: s, format: df.Source, reason: "Failed to parse with all enclosed parsers"}
}

func (f *dtFormatter) applyDefaults(p *dtParsed, roundUp bool) {
	if f.epoch {
		if roundUp && !p.set[dtEpochFraction] {
			if f.seconds {
				p.setField(dtEpochFraction, 999_999_999)
			} else {
				p.setField(dtEpochFraction, 999_999)
			}
		}
		return
	}
	if !roundUp {
		return
	}
	def := func(field dtField, v int64) {
		if !p.set[field] {
			p.setField(field, v)
		}
	}
	switch {
	case f.doy:
		def(dtDayOfYear, 1)
	case f.week:
		def(dtWeekOfWeekBasedYear, 1)
		def(dtLocalDayOfWeek, 1)
	default:
		def(dtMonth, 1)
		def(dtDay, 1)
	}
	if !p.set[dtClockHourOfAmPm] && !p.set[dtHourOfAmPm] && !p.set[dtAmPm] && !p.set[dtClockHourOfDay] && !p.set[dtMilliOfDay] && !p.set[dtNanoOfDay] {
		def(dtHourOfDay, 23)
	}
	def(dtMinute, 59)
	def(dtSecond, 59)
	def(dtNano, 999_999_999)
}

func rangeMsg(field string, lo, hi string, v int64) string {
	return fmt.Sprintf("Invalid value for %s (valid values %s - %s): %d", field, lo, hi, v)
}

func daysIn(year int64, month int64) int64 {
	switch month {
	case 2:
		if isLeap(year) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

func isLeap(y int64) bool { return (y%4 == 0 && y%100 != 0) || y%400 == 0 }

var javaMonthNames = []string{"JANUARY", "FEBRUARY", "MARCH", "APRIL", "MAY", "JUNE", "JULY", "AUGUST", "SEPTEMBER", "OCTOBER", "NOVEMBER", "DECEMBER"}

// validateDate is LocalDate.of: it returns the DateTimeException message.
func validateDate(y, m, d int64) string {
	if y < -999999999 || y > 999999999 {
		return rangeMsg("Year", "-999999999", "999999999", y)
	}
	if m < 1 || m > 12 {
		return rangeMsg("MonthOfYear", "1", "12", m)
	}
	if d < 1 || d > 31 {
		return rangeMsg("DayOfMonth", "1", "28/31", d)
	}
	if d > 28 && d > daysIn(y, m) {
		if d == 29 {
			return fmt.Sprintf("Invalid date 'February 29' as '%d' is not a leap year", y)
		}
		return fmt.Sprintf("Invalid date '%s %d'", javaMonthNames[m-1], d)
	}
	return ""
}

// resolveStrict applies the checks the STRICT resolver makes while parsing;
// a message means the parser did not match.
func (f *dtFormatter) resolveStrict(p *dtParsed) string {
	if p.conflict {
		return "Conflict found: field resolves to different values"
	}
	if f.epoch {
		return ""
	}
	if p.set[dtYearOfEra] {
		if v := p.val[dtYearOfEra]; v < 1 || v > 999999999 {
			return rangeMsg("YearOfEra", "1", "999999999", v)
		}
		if p.set[dtEra] {
			y := p.val[dtYearOfEra]
			if p.val[dtEra] == 0 {
				y = 1 - y
			}
			if p.set[dtYear] && p.val[dtYear] != y {
				return "Conflict found: Year differs from YearOfEra"
			}
			p.setField(dtYear, y)
		}
	}
	// time fields
	if p.set[dtClockHourOfDay] {
		v := p.val[dtClockHourOfDay]
		if v < 1 || v > 24 {
			return rangeMsg("ClockHourOfDay", "1", "24", v)
		}
		if v == 24 {
			v = 0
		}
		if p.set[dtHourOfDay] && p.val[dtHourOfDay] != v {
			return "Conflict found"
		}
		p.setField(dtHourOfDay, v)
	}
	if p.set[dtClockHourOfAmPm] {
		v := p.val[dtClockHourOfAmPm]
		if v < 1 || v > 12 {
			return rangeMsg("ClockHourOfAmPm", "1", "12", v)
		}
		if v == 12 {
			v = 0
		}
		p.setField(dtHourOfAmPm, v)
	}
	if p.set[dtAmPm] && p.set[dtHourOfAmPm] {
		ap, hap := p.val[dtAmPm], p.val[dtHourOfAmPm]
		if hap < 0 || hap > 11 {
			return rangeMsg("HourOfAmPm", "0", "11", hap)
		}
		h := ap*12 + hap
		if p.set[dtHourOfDay] && p.val[dtHourOfDay] != h {
			return "Conflict found"
		}
		p.setField(dtHourOfDay, h)
	}
	if p.set[dtMilliOfDay] {
		v := p.val[dtMilliOfDay]
		p.setField(dtHourOfDay, v/3600000)
		p.setField(dtMinute, v/60000%60)
		p.setField(dtSecond, v/1000%60)
		p.setField(dtNano, v%1000*1e6)
	}
	if p.set[dtNanoOfDay] {
		v := p.val[dtNanoOfDay]
		p.setField(dtHourOfDay, v/3600e9)
		p.setField(dtMinute, v/60e9%60)
		p.setField(dtSecond, v/1e9%60)
		p.setField(dtNano, v%1e9)
	}
	if p.conflict {
		return "Conflict found"
	}
	if p.set[dtNano] {
		if v := p.val[dtNano]; v < 0 || v > 999_999_999 {
			return rangeMsg("NanoOfSecond", "0", "999999999", v)
		}
	}
	if p.set[dtHourOfDay] {
		if v := p.val[dtHourOfDay]; v < 0 || v > 23 {
			return rangeMsg("HourOfDay", "0", "23", v)
		}
	}
	if p.set[dtMinute] {
		if v := p.val[dtMinute]; v < 0 || v > 59 {
			return rangeMsg("MinuteOfHour", "0", "59", v)
		}
	}
	if p.set[dtSecond] {
		if v := p.val[dtSecond]; v < 0 || v > 59 {
			return rangeMsg("SecondOfMinute", "0", "59", v)
		}
	}
	// dates of the proleptic year are resolved (and validated) strictly
	if p.set[dtYear] {
		y := p.val[dtYear]
		switch {
		case p.set[dtMonth] && p.set[dtDay]:
			if msg := validateDate(y, p.val[dtMonth], p.val[dtDay]); msg != "" {
				return msg
			}
		case p.set[dtDayOfYear]:
			if y < -999999999 || y > 999999999 {
				return rangeMsg("Year", "-999999999", "999999999", y)
			}
			max := int64(365)
			if isLeap(y) {
				max = 366
			}
			if d := p.val[dtDayOfYear]; d < 1 || d > max {
				return rangeMsg("DayOfYear", "1", "365/366", d)
			}
		}
		if p.set[dtMonth] {
			if m := p.val[dtMonth]; m < 1 || m > 12 {
				if p.set[dtDay] || f.proleptic {
					return rangeMsg("MonthOfYear", "1", "12", m)
				}
			}
		}
	}
	return ""
}

// from is DateFormatters.from: it builds the instant, reporting the
// DateTimeExceptions of LocalDate and Year.
func (f *dtFormatter) from(p *dtParsed, input, format string, loc *time.Location) (dtResult, *dateError) {
	res := dtResult{}
	fail := func(msg string) (dtResult, *dateError) {
		return dtResult{}, &dateError{kind: dateErrDateTime, input: input, format: format, reason: msg}
	}
	if f.epoch {
		v := p.val[dtEpochValue]
		frac := p.val[dtEpochFraction]
		var secs, nanos int64
		if f.seconds {
			secs, nanos = v, frac
		} else {
			secs, nanos = v/1000, v%1000*1e6+frac
		}
		if p.set[dtEpochNegative] {
			secs, nanos = -secs, -nanos
		}
		res.t = time.Unix(secs, nanos).UTC()
		return res, nil
	}
	zone := loc
	if p.zoneSet {
		zone = p.loc
		res.hasZone = true
	} else if p.set[dtOffset] {
		zone = time.FixedZone("", int(p.val[dtOffset]))
		res.hasZone = true
	}
	hour, minute, second, nano := int64(0), int64(0), int64(0), int64(0)
	if p.set[dtHourOfDay] {
		hour = p.val[dtHourOfDay]
		minute = p.val[dtMinute]
		second = p.val[dtSecond]
		nano = p.val[dtNano]
	}
	var year int64 = 1970
	yearSet := false
	if p.set[dtYear] {
		year, yearSet = p.val[dtYear], true
	} else if p.set[dtYearOfEra] {
		year, yearSet = p.val[dtYearOfEra], true
	}
	var month, day int64 = 1, 1
	switch {
	case yearSet && p.set[dtDayOfYear]:
		if year < -999999999 || year > 999999999 {
			return fail(rangeMsg("Year", "-999999999", "999999999", year))
		}
		max := int64(365)
		if isLeap(year) {
			max = 366
		}
		d := p.val[dtDayOfYear]
		if d < 1 || d > 366 {
			return fail(rangeMsg("DayOfYear", "1", "365/366", d))
		}
		if d > max {
			return fail(fmt.Sprintf("Invalid date 'DayOfYear 366' as '%d' is not a leap year", year))
		}
		t := time.Date(int(year), 1, 1, int(hour), int(minute), int(second), int(nano), zone).AddDate(0, 0, int(d-1))
		res.t = t
		return res, nil
	case p.set[dtWeekBasedYear] || p.set[dtWeekOfWeekBasedYear]:
		wby := year
		if p.set[dtWeekBasedYear] {
			wby = p.val[dtWeekBasedYear]
		}
		week := int64(1)
		if p.set[dtWeekOfWeekBasedYear] {
			week = p.val[dtWeekOfWeekBasedYear]
		}
		dow := int64(1)
		if p.set[dtLocalDayOfWeek] {
			dow = p.val[dtLocalDayOfWeek]
		} else if p.set[dtDayOfWeek] {
			dow = p.val[dtDayOfWeek]
		}
		// ISO week 1 contains January 4th
		jan4 := time.Date(int(wby), 1, 4, 0, 0, 0, 0, time.UTC)
		wd := int64(jan4.Weekday())
		if wd == 0 {
			wd = 7
		}
		start := jan4.AddDate(0, 0, int(-(wd - 1)))
		d := start.AddDate(0, 0, int((week-1)*7+(dow-1)))
		res.t = time.Date(d.Year(), d.Month(), d.Day(), int(hour), int(minute), int(second), int(nano), zone)
		return res, nil
	}
	if p.set[dtMonth] {
		month = p.val[dtMonth]
	}
	if p.set[dtDay] {
		day = p.val[dtDay]
	}
	if yearSet || p.set[dtMonth] || p.set[dtDay] {
		if msg := validateDate(year, month, day); msg != "" {
			return fail(msg)
		}
	}
	if p.set[dtDayOfWeek] && (p.set[dtDay] || p.set[dtMonth]) {
		wd := int64(time.Date(int(year), time.Month(month), int(day), 0, 0, 0, 0, time.UTC).Weekday())
		if wd == 0 {
			wd = 7
		}
		if wd != p.val[dtDayOfWeek] {
			return fail(fmt.Sprintf("Conflict found: Field DayOfWeek %d differs from DayOfWeek %d derived from %04d-%02d-%02d", wd, p.val[dtDayOfWeek], year, month, day))
		}
	}
	res.t = time.Date(int(year), time.Month(month), int(day), int(hour), int(minute), int(second), int(nano), zone)
	return res, nil
}

// Parse parses a JSON value (string or number) as a date.
func (df *DateFormat) Parse(v any) (time.Time, error) {
	s, ok := dateText(v)
	if !ok {
		if t, isTime := v.(time.Time); isTime {
			return t, nil
		}
		return time.Time{}, fmt.Errorf("cannot parse %v as date", v)
	}
	res, err := df.parseDate(s, false, time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	return res.t, nil
}

// dateText is the text a date field parses from a JSON value.
func dateText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e18 {
			return strconv.FormatInt(int64(t), 10), true
		}
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case int:
		return strconv.Itoa(t), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

// ParseString parses a string with the format.
func (df *DateFormat) ParseString(s string) (time.Time, error) {
	res, err := df.parseDate(s, false, time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	return res.t, nil
}

// Format renders a time with the printer of the first format.
func (df *DateFormat) Format(t time.Time) string {
	if df == nil || df.err != nil || len(df.formatters) == 0 {
		return formatOptionalTime(t)
	}
	f := df.formatters[0]
	var b strings.Builder
	if !f.printer.print(&b, t) {
		return formatOptionalTime(t)
	}
	return b.String()
}

var defaultISOPrinter = isoPrinter(false)

func formatOptionalTime(t time.Time) string {
	var b strings.Builder
	defaultISOPrinter.print(&b, t)
	return b.String()
}

// javaInstantString is Instant.toString (the date_nanos range messages).
func javaInstantString(t time.Time) string {
	t = t.UTC()
	var b strings.Builder
	year := t.Year()
	switch {
	case year > 9999:
		b.WriteString("+" + strconv.Itoa(year))
	case year < 0:
		fmt.Fprintf(&b, "-%04d", -year)
	default:
		fmt.Fprintf(&b, "%04d", year)
	}
	fmt.Fprintf(&b, "-%02d-%02dT%02d:%02d", t.Month(), t.Day(), t.Hour(), t.Minute())
	fmt.Fprintf(&b, ":%02d", t.Second())
	if ns := t.Nanosecond(); ns != 0 {
		switch {
		case ns%1e6 == 0:
			fmt.Fprintf(&b, ".%03d", ns/1e6)
		case ns%1e3 == 0:
			fmt.Fprintf(&b, ".%06d", ns/1e3)
		default:
			fmt.Fprintf(&b, ".%09d", ns)
		}
	}
	b.WriteByte('Z')
	return b.String()
}

// epochMillis is Instant.toEpochMilli (floor).
func epochMillis(t time.Time) int64 {
	return t.Unix()*1000 + int64(t.Nanosecond())/1e6
}

var (
	minNanosTime = time.Unix(0, 0).UTC()
	maxNanosTime = time.Unix(0, math.MaxInt64).UTC()
)

// nanosRangeError is DateUtils.toLong's check for date_nanos values.
func nanosRangeError(t time.Time) string {
	if t.Before(minNanosTime) {
		return "date[" + javaInstantString(t) + "] is before the epoch in 1970 and cannot be stored in nanosecond resolution"
	}
	if t.After(maxNanosTime) {
		return "date[" + javaInstantString(t) + "] is after 2262-04-11T23:47:16.854775807 and cannot be stored in nanosecond resolution"
	}
	return ""
}
