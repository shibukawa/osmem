package engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultDateFormat is the default format of OpenSearch date fields.
const DefaultDateFormat = "strict_date_optional_time||epoch_millis"

// namedFormats maps OpenSearch built-in date format names to Go layouts.
// Names handled specially (optional-time, epoch) are not listed here.
var namedFormats = map[string]string{
	"date":                             "2006-01-02",
	"strict_date":                      "2006-01-02",
	"basic_date":                       "20060102",
	"basic_date_time":                  "20060102T150405.000Z0700",
	"basic_date_time_no_millis":        "20060102T150405Z0700",
	"basic_ordinal_date":               "2006002",
	"basic_time":                       "150405.000Z0700",
	"basic_time_no_millis":             "150405Z0700",
	"basic_t_time":                     "T150405.000Z0700",
	"date_hour":                        "2006-01-02T15",
	"date_hour_minute":                 "2006-01-02T15:04",
	"date_hour_minute_second":          "2006-01-02T15:04:05",
	"date_hour_minute_second_fraction": "2006-01-02T15:04:05.000",
	"date_hour_minute_second_millis":   "2006-01-02T15:04:05.000",
	"date_time":                        "2006-01-02T15:04:05.000Z07:00",
	"date_time_no_millis":              "2006-01-02T15:04:05Z07:00",
	"hour":                             "15",
	"hour_minute":                      "15:04",
	"hour_minute_second":               "15:04:05",
	"hour_minute_second_fraction":      "15:04:05.000",
	"hour_minute_second_millis":        "15:04:05.000",
	"ordinal_date":                     "2006-002",
	"time":                             "15:04:05.000Z07:00",
	"time_no_millis":                   "15:04:05Z07:00",
	"t_time":                           "T15:04:05.000Z07:00",
	"t_time_no_millis":                 "T15:04:05Z07:00",
	"year":                             "2006",
	"year_month":                       "2006-01",
	"year_month_day":                   "2006-01-02",
	"strict_basic_week_date":           "2006W001",
	"strict_date_hour":                 "2006-01-02T15",
	"strict_date_hour_minute":          "2006-01-02T15:04",
	"strict_date_hour_minute_second":   "2006-01-02T15:04:05",
	"strict_date_hour_minute_second_fraction": "2006-01-02T15:04:05.000",
	"strict_date_hour_minute_second_millis":   "2006-01-02T15:04:05.000",
	"strict_date_time":                        "2006-01-02T15:04:05.000Z07:00",
	"strict_date_time_no_millis":              "2006-01-02T15:04:05Z07:00",
	"strict_hour":                             "15",
	"strict_hour_minute":                      "15:04",
	"strict_hour_minute_second":               "15:04:05",
	"strict_hour_minute_second_fraction":      "15:04:05.000",
	"strict_hour_minute_second_millis":        "15:04:05.000",
	"strict_ordinal_date":                     "2006-002",
	"strict_time":                             "15:04:05.000Z07:00",
	"strict_time_no_millis":                   "15:04:05Z07:00",
	"strict_year":                             "2006",
	"strict_year_month":                       "2006-01",
	"strict_year_month_day":                   "2006-01-02",
}

// DateFormat is a parsed OpenSearch date format ("a||b||c").
type DateFormat struct {
	Source  string
	formats []singleFormat
}

type singleFormat struct {
	name   string // named format or ""
	layout string // Go layout when pattern based
}

var (
	dateFormatCache   = map[string]*DateFormat{}
	dateFormatCacheMu sync.RWMutex
)

// ParseDateFormat parses a format string. Unknown names are treated as Java
// DateTimeFormatter patterns.
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
	for _, part := range strings.Split(s, "||") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		switch part {
		case "strict_date_optional_time", "date_optional_time", "strict_date_optional_time_nanos", "date_optional_time_nanos",
			"epoch_millis", "epoch_second":
			df.formats = append(df.formats, singleFormat{name: part})
		default:
			if layout, ok := namedFormats[part]; ok {
				df.formats = append(df.formats, singleFormat{name: part, layout: layout})
			} else {
				df.formats = append(df.formats, singleFormat{layout: javaToGoLayout(part)})
			}
		}
	}
	dateFormatCacheMu.Lock()
	dateFormatCache[s] = df
	dateFormatCacheMu.Unlock()
	return df
}

// Parse parses a JSON value (string or number) as a date.
func (df *DateFormat) Parse(v any) (time.Time, error) {
	switch t := v.(type) {
	case json.Number:
		return df.parseNumber(t.String())
	case float64:
		return df.parseNumber(strconv.FormatFloat(t, 'f', -1, 64))
	case int64:
		return df.parseNumber(strconv.FormatInt(t, 10))
	case int:
		return df.parseNumber(strconv.Itoa(t))
	case string:
		return df.ParseString(t)
	case time.Time:
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot parse %v as date", v)
}

func (df *DateFormat) parseNumber(s string) (time.Time, error) {
	for _, f := range df.formats {
		switch f.name {
		case "epoch_millis":
			if t, ok := parseEpoch(s, time.Millisecond); ok {
				return t, nil
			}
		case "epoch_second":
			if t, ok := parseEpoch(s, time.Second); ok {
				return t, nil
			}
		}
	}
	// OpenSearch's date_optional_time accepts a plain year, so bare numbers
	// fall through to string parsing only when no epoch format exists.
	return df.ParseString(s)
}

func parseEpoch(s string, unit time.Duration) (time.Time, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, false
	}
	ns := int64(f * float64(unit))
	return time.Unix(0, ns).UTC(), true
}

// ParseString parses a string with the first matching format.
func (df *DateFormat) ParseString(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, f := range df.formats {
		switch f.name {
		case "strict_date_optional_time", "date_optional_time", "strict_date_optional_time_nanos", "date_optional_time_nanos":
			if t, ok := parseOptionalTime(s); ok {
				return t, nil
			}
		case "epoch_millis":
			if isDigits(s) {
				if t, ok := parseEpoch(s, time.Millisecond); ok {
					return t, nil
				}
			}
		case "epoch_second":
			if isDigits(s) {
				if t, ok := parseEpoch(s, time.Second); ok {
					return t, nil
				}
			}
		default:
			if t, err := time.ParseInLocation(f.layout, s, time.UTC); err == nil {
				return t, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("failed to parse date field [%s] with format [%s]", s, df.Source)
}

// Format renders a time using the first format.
func (df *DateFormat) Format(t time.Time) string {
	if len(df.formats) == 0 {
		return formatOptionalTime(t)
	}
	f := df.formats[0]
	switch f.name {
	case "strict_date_optional_time", "date_optional_time", "strict_date_optional_time_nanos", "date_optional_time_nanos":
		return formatOptionalTime(t)
	case "epoch_millis":
		return strconv.FormatInt(t.UnixMilli(), 10)
	case "epoch_second":
		return strconv.FormatInt(t.Unix(), 10)
	}
	return t.Format(f.layout)
}

func formatOptionalTime(t time.Time) string {
	if t.Location() == time.UTC {
		return t.Format("2006-01-02T15:04:05.000Z")
	}
	return t.Format("2006-01-02T15:04:05.000Z07:00")
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		s = s[1:]
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

var optionalTimeRe = regexp.MustCompile(`^(-?\d{4,})(?:-(\d{2})(?:-(\d{2})(?:[T ](\d{2})(?::(\d{2})(?::(\d{2})(?:[.,](\d{1,9}))?)?)?)?)?)?(Z|[+-]\d{2}(?::?\d{2})?)?$`)

// parseOptionalTime implements strict_date_optional_time: yyyy, yyyy-MM,
// yyyy-MM-dd, yyyy-MM-ddTHH, ...:mm, ...:ss, ...:ss.SSS with an optional zone.
func parseOptionalTime(s string) (time.Time, bool) {
	m := optionalTimeRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	year, _ := strconv.Atoi(m[1])
	month, day := 1, 1
	if m[2] != "" {
		month, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		day, _ = strconv.Atoi(m[3])
	}
	hour, _ := strconv.Atoi(m[4])
	minute, _ := strconv.Atoi(m[5])
	sec, _ := strconv.Atoi(m[6])
	nsec := 0
	if m[7] != "" {
		frac := m[7]
		for len(frac) < 9 {
			frac += "0"
		}
		nsec, _ = strconv.Atoi(frac)
	}
	loc := time.UTC
	if m[8] != "" && m[8] != "Z" {
		z := strings.ReplaceAll(m[8], ":", "")
		sign := 1
		if z[0] == '-' {
			sign = -1
		}
		hh, _ := strconv.Atoi(z[1:3])
		mm := 0
		if len(z) >= 5 {
			mm, _ = strconv.Atoi(z[3:5])
		}
		loc = time.FixedZone("", sign*(hh*3600+mm*60))
	}
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || sec > 60 {
		return time.Time{}, false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, nsec, loc)
	if t.Day() != day {
		return time.Time{}, false
	}
	return t, true
}

// javaToGoLayout converts a Java DateTimeFormatter pattern to a Go layout.
func javaToGoLayout(p string) string {
	var out strings.Builder
	i := 0
	for i < len(p) {
		c := p[i]
		if c == '\'' {
			j := i + 1
			for j < len(p) && p[j] != '\'' {
				j++
			}
			if j == i+1 {
				out.WriteByte('\'')
			} else {
				out.WriteString(p[i+1 : j])
			}
			i = j + 1
			continue
		}
		if !isPatternLetter(c) {
			out.WriteByte(c)
			i++
			continue
		}
		j := i
		for j < len(p) && p[j] == c {
			j++
		}
		n := j - i
		switch c {
		case 'y', 'u', 'Y':
			if n == 2 {
				out.WriteString("06")
			} else {
				out.WriteString("2006")
			}
		case 'M', 'L':
			switch {
			case n >= 4:
				out.WriteString("January")
			case n == 3:
				out.WriteString("Jan")
			case n == 2:
				out.WriteString("01")
			default:
				out.WriteString("1")
			}
		case 'd':
			if n >= 2 {
				out.WriteString("02")
			} else {
				out.WriteString("2")
			}
		case 'D':
			out.WriteString("002")
		case 'H', 'k':
			if n >= 2 {
				out.WriteString("15")
			} else {
				out.WriteString("15")
			}
		case 'h', 'K':
			if n >= 2 {
				out.WriteString("03")
			} else {
				out.WriteString("3")
			}
		case 'm':
			if n >= 2 {
				out.WriteString("04")
			} else {
				out.WriteString("4")
			}
		case 's':
			if n >= 2 {
				out.WriteString("05")
			} else {
				out.WriteString("5")
			}
		case 'S', 'n':
			// fractional seconds; Go requires a preceding '.' which the
			// pattern normally has.
			out.WriteString(strings.Repeat("0", n))
		case 'a':
			out.WriteString("PM")
		case 'E':
			if n >= 4 {
				out.WriteString("Monday")
			} else {
				out.WriteString("Mon")
			}
		case 'z', 'v':
			out.WriteString("MST")
		case 'Z':
			switch {
			case n >= 5:
				out.WriteString("-07:00")
			case n == 4:
				out.WriteString("MST")
			default:
				out.WriteString("-0700")
			}
		case 'X', 'x':
			switch {
			case n >= 3:
				out.WriteString("Z07:00")
			case n == 2:
				out.WriteString("Z0700")
			default:
				out.WriteString("Z07")
			}
		case 'V':
			out.WriteString("MST")
		default:
			out.WriteString(p[i:j])
		}
		i = j
	}
	return out.String()
}

func isPatternLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// date math ------------------------------------------------------------

var dateMathUnitRe = regexp.MustCompile(`^([+-])(\d+)([yMwdhHms])`)

// ParseDateMath parses an OpenSearch date math expression such as
// "now-1d/d" or "2024-01-01||+1M". roundUp selects the end of the rounded
// interval (used for upper range bounds).
func ParseDateMath(expr string, df *DateFormat, now time.Time, loc *time.Location, roundUp bool) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	var t time.Time
	var rest string
	if strings.HasPrefix(expr, "now") {
		t = now.In(loc)
		rest = expr[3:]
	} else if idx := strings.Index(expr, "||"); idx >= 0 {
		var err error
		t, err = parseInZone(df, expr[:idx], loc)
		if err != nil {
			return t, err
		}
		rest = expr[idx+2:]
	} else {
		t, err := parseInZone(df, expr, loc)
		if err != nil {
			return t, err
		}
		if roundUp {
			t = roundUpPartial(expr, t)
		}
		return t, nil
	}
	for rest != "" {
		if rest[0] == '/' {
			if len(rest) < 2 {
				return t, fmt.Errorf("invalid date math [%s]", expr)
			}
			t = roundDate(t, rest[1], roundUp)
			rest = rest[2:]
			continue
		}
		m := dateMathUnitRe.FindStringSubmatch(rest)
		if m == nil {
			return t, fmt.Errorf("invalid date math [%s]", expr)
		}
		n, _ := strconv.Atoi(m[2])
		if m[1] == "-" {
			n = -n
		}
		t = addUnit(t, m[3][0], n)
		rest = rest[len(m[0]):]
	}
	return t, nil
}

// explicitZoneRe matches strings whose time part ends with a zone.
var explicitZoneRe = regexp.MustCompile(`[T ]\d{2}(:\d{2}(:\d{2}([.,]\d+)?)?)?(Z|[+-]\d{2}(:?\d{2})?)$`)

// parseInZone parses a date string; when it carries no explicit zone the
// wall clock is interpreted in loc (the time_zone parameter).
func parseInZone(df *DateFormat, s string, loc *time.Location) (time.Time, error) {
	t, err := df.ParseString(s)
	if err != nil {
		return t, err
	}
	if loc != time.UTC && t.Location() == time.UTC && !explicitZoneRe.MatchString(strings.TrimSpace(s)) && !isDigits(strings.TrimSpace(s)) {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
	}
	return t, nil
}

// roundUpPartial rounds a partially specified date (e.g. "2024-01") up to the
// end of the implied interval, as OpenSearch does for lte/lt upper bounds.
func roundUpPartial(expr string, t time.Time) time.Time {
	m := optionalTimeRe.FindStringSubmatch(strings.TrimSpace(expr))
	if m == nil {
		return t
	}
	switch {
	case m[2] == "":
		return t.AddDate(1, 0, 0).Add(-time.Millisecond)
	case m[3] == "":
		return t.AddDate(0, 1, 0).Add(-time.Millisecond)
	case m[4] == "":
		return t.AddDate(0, 0, 1).Add(-time.Millisecond)
	case m[5] == "":
		return t.Add(time.Hour - time.Millisecond)
	case m[6] == "":
		return t.Add(time.Minute - time.Millisecond)
	case m[7] == "":
		return t.Add(time.Second - time.Millisecond)
	}
	return t
}

func addUnit(t time.Time, unit byte, n int) time.Time {
	switch unit {
	case 'y':
		return t.AddDate(n, 0, 0)
	case 'M':
		return t.AddDate(0, n, 0)
	case 'w':
		return t.AddDate(0, 0, 7*n)
	case 'd':
		return t.AddDate(0, 0, n)
	case 'h', 'H':
		return t.Add(time.Duration(n) * time.Hour)
	case 'm':
		return t.Add(time.Duration(n) * time.Minute)
	case 's':
		return t.Add(time.Duration(n) * time.Second)
	}
	return t
}

func roundDate(t time.Time, unit byte, up bool) time.Time {
	var start time.Time
	loc := t.Location()
	switch unit {
	case 'y':
		start = time.Date(t.Year(), 1, 1, 0, 0, 0, 0, loc)
	case 'M':
		start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
	case 'w':
		// ISO week starts on Monday
		wd := int(t.Weekday())
		if wd == 0 {
			wd = 7
		}
		start = time.Date(t.Year(), t.Month(), t.Day()-(wd-1), 0, 0, 0, 0, loc)
	case 'd':
		start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	case 'h', 'H':
		start = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
	case 'm':
		start = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	case 's':
		start = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
	default:
		return t
	}
	if !up {
		return start
	}
	return addUnit(start, unit, 1).Add(-time.Millisecond)
}

// parseTimeZone parses an OpenSearch time_zone parameter (+09:00, Asia/Tokyo, UTC).
func parseTimeZone(s string) (*time.Location, error) {
	if s == "" || s == "UTC" || s == "Z" {
		return time.UTC, nil
	}
	if (s[0] == '+' || s[0] == '-') && len(s) >= 3 {
		z := strings.ReplaceAll(s[1:], ":", "")
		hh, err := strconv.Atoi(z[:2])
		if err != nil {
			return nil, fmt.Errorf("invalid time zone [%s]", s)
		}
		mm := 0
		if len(z) >= 4 {
			mm, _ = strconv.Atoi(z[2:4])
		}
		off := hh*3600 + mm*60
		if s[0] == '-' {
			off = -off
		}
		return time.FixedZone(s, off), nil
	}
	loc, err := time.LoadLocation(s)
	if err != nil {
		return nil, fmt.Errorf("invalid time zone [%s]", s)
	}
	return loc, nil
}

// parseDuration parses OpenSearch time values (1d, 30s, 500ms, 1h).
func parseDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	units := []struct {
		suffix string
		d      time.Duration
	}{{"ms", time.Millisecond}, {"micros", time.Microsecond}, {"nanos", time.Nanosecond}, {"s", time.Second}, {"m", time.Minute}, {"h", time.Hour}, {"d", 24 * time.Hour}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			if err != nil {
				return 0, false
			}
			return time.Duration(n * float64(u.d)), true
		}
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(n) * time.Millisecond, true
	}
	return 0, false
}
