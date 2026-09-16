package engine

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// locationCache remembers time.LoadLocation results, which otherwise read
// the zone database on every call; misses are remembered too, since the
// date parsers probe several candidate names per date. It is emptied once
// it grows past maxLocationCache (names come from requests).
var (
	locationCache     sync.Map // name -> locationEntry
	locationCacheSize atomic.Int64
)

const maxLocationCache = 4096

type locationEntry struct {
	loc *time.Location
	err error
}

// loadLocation is a cached time.LoadLocation.
func loadLocation(name string) (*time.Location, error) {
	if e, ok := locationCache.Load(name); ok {
		entry := e.(locationEntry)
		return entry.loc, entry.err
	}
	loc, err := time.LoadLocation(name)
	if locationCacheSize.Add(1) > maxLocationCache {
		locationCache.Range(func(k, _ any) bool { locationCache.Delete(k); return true })
		locationCacheSize.Store(0)
	}
	locationCache.Store(name, locationEntry{loc, err})
	return loc, err
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

// date math ------------------------------------------------------------

const dateErrMath = 100

// ParseDateMath parses an OpenSearch date math expression such as
// "now-1d/d" or "2024-01-01||+1M" the way JavaDateMathParser does. roundUp
// selects the round-up parser (missing fields default to the end of the
// interval) and the end of rounded units, as used for upper range bounds;
// loc is the time_zone of dates without a zone and of the math.
func ParseDateMath(expr string, df *DateFormat, now time.Time, loc *time.Location, roundUp bool) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	if df == nil {
		df = ParseDateFormat(DefaultDateFormat)
	}
	var t time.Time
	var mathString string
	if strings.HasPrefix(expr, "now") {
		t = now.Truncate(time.Millisecond)
		mathString = expr[3:]
	} else {
		idx := strings.Index(expr, "||")
		if idx < 0 {
			res, err := df.parseDate(expr, roundUp, loc)
			if err != nil {
				return time.Time{}, err
			}
			return res.t, nil
		}
		res, err := df.parseDate(expr[:idx], false, loc)
		if err != nil {
			return time.Time{}, err
		}
		t = res.t
		mathString = expr[idx+2:]
	}
	return parseDateMathOps(mathString, t.In(loc), roundUp)
}

func dateMathError(format string, args ...any) *dateError {
	return &dateError{kind: dateErrMath, reason: fmt.Sprintf(format, args...)}
}

func parseDateMathOps(math string, t time.Time, roundUp bool) (time.Time, error) {
	for i := 0; i < len(math); {
		c := math[i]
		i++
		round := false
		sign := 1
		switch c {
		case '/':
			round = true
		case '+':
		case '-':
			sign = -1
		default:
			return t, dateMathError("operator not supported for date math [%s]", math)
		}
		if i >= len(math) {
			return t, dateMathError("truncated date math [%s]", math)
		}
		num := 1
		if isASCIIDigit(math[i]) {
			from := i
			for i < len(math) && isASCIIDigit(math[i]) {
				i++
			}
			if i >= len(math) {
				return t, dateMathError("truncated date math [%s]", math)
			}
			n, err := strconv.Atoi(math[from:i])
			if err != nil {
				return t, dateMathError("truncated date math [%s]", math)
			}
			num = n
		}
		if round && num != 1 {
			return t, dateMathError("rounding `/` can only be used on single unit types [%s]", math)
		}
		unit := math[i]
		i++
		if !strings.ContainsRune("yMwdhHms", rune(unit)) {
			return t, dateMathError("unit [%c] not supported for date math [%s]", unit, math)
		}
		if round {
			t = roundDate(t, unit, roundUp)
		} else {
			t = addUnit(t, unit, sign*num)
		}
	}
	return t, nil
}

// plusMonths is ZonedDateTime.plusMonths: the day is clamped to the length
// of the resulting month.
func plusMonths(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	total := int(m) - 1 + n
	ny := y + floorDiv(total, 12)
	nm := floorModInt(total, 12) + 1
	if max := int(daysIn(int64(ny), int64(nm))); d > max {
		d = max
	}
	return time.Date(ny, time.Month(nm), d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func floorModInt(a, b int) int { return a - floorDiv(a, b)*b }

func addUnit(t time.Time, unit byte, n int) time.Time {
	switch unit {
	case 'y':
		return plusMonths(t, 12*n)
	case 'M':
		return plusMonths(t, n)
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
	loc, err := loadLocation(s)
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
