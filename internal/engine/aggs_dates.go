package engine

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // embed the IANA database: some platforms (observed on windows-latest CI) don't reliably provide one of their own for time.LoadLocation
)

// Time zones, time values and date rounding of aggregations, following
// java.time and OpenSearch's Rounding.

// parseAggZone is the time_zone parameter of aggregations: ZoneId.of for
// strings, ZoneOffset.ofHours for numbers. It returns the zone and its id.
func parseAggZone(v any) (*time.Location, string, *Error) {
	if s, ok := v.(string); ok {
		return javaZoneOf(s)
	}
	n, _ := toFloat(v)
	h := int(n)
	if h < -18 || h > 18 {
		return nil, "", errJava(http.StatusBadRequest, "date_time_exception", fmt.Sprintf("Zone offset hours not in valid range: value %d is not in the range -18 to 18", h))
	}
	return fixedZone(h * 3600), offsetID(h * 3600), nil
}

func fixedZone(seconds int) *time.Location {
	if seconds == 0 {
		return time.UTC
	}
	return time.FixedZone(offsetID(seconds), seconds)
}

// offsetID is ZoneOffset.getId.
func offsetID(seconds int) string {
	if seconds == 0 {
		return "Z"
	}
	sign := "+"
	if seconds < 0 {
		sign = "-"
		seconds = -seconds
	}
	s := fmt.Sprintf("%s%02d:%02d", sign, seconds/3600, seconds/60%60)
	if seconds%60 != 0 {
		s += fmt.Sprintf(":%02d", seconds%60)
	}
	return s
}

var zoneRegionRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9~/._+-]+$`)

// javaZoneOf is ZoneId.of.
func javaZoneOf(id string) (*time.Location, string, *Error) {
	dateTimeErr := func(msg string) *Error { return errJava(http.StatusBadRequest, "date_time_exception", msg) }
	switch {
	case id == "Z":
		return time.UTC, "Z", nil
	case len(id) <= 1:
		return nil, "", dateTimeErr("Invalid ID for ZoneOffset, invalid format: " + id)
	case id[0] == '+' || id[0] == '-':
		secs, err := javaOffsetOf(id)
		if err != nil {
			return nil, "", err
		}
		return fixedZone(secs), offsetID(secs), nil
	case id == "UTC" || id == "GMT" || id == "UT":
		return time.UTC, id, nil
	}
	for _, prefix := range []string{"UTC", "GMT", "UT"} {
		if strings.HasPrefix(id, prefix) && len(id) > len(prefix) && (id[len(prefix)] == '+' || id[len(prefix)] == '-') {
			secs, err := javaOffsetOf(id[len(prefix):])
			if err != nil {
				return nil, "", err
			}
			if secs == 0 {
				return time.UTC, prefix, nil
			}
			name := prefix + offsetID(secs)
			return time.FixedZone(name, secs), name, nil
		}
	}
	if !zoneRegionRe.MatchString(id) {
		return nil, "", dateTimeErr("Invalid ID for region-based ZoneId, invalid format: " + id)
	}
	unknown := errJava(http.StatusBadRequest, "zone_rules_exception", "Unknown time-zone ID: "+id)
	if !knownZoneID(id) {
		return nil, "", unknown
	}
	loc, err := loadLocation(id)
	if err != nil {
		return nil, "", unknown
	}
	return loc, id, nil
}

// javaOffsetOf is ZoneOffset.of: ±h, ±hh, ±hh:mm, ±hhmm, ±hh:mm:ss, ±hhmmss.
func javaOffsetOf(id string) (int, *Error) {
	invalid := errJava(http.StatusBadRequest, "date_time_exception", "Invalid ID for ZoneOffset, invalid format: "+id)
	digits := func(s string) (int, bool) {
		n, err := strconv.Atoi(s)
		return n, err == nil && len(s) == 2 || err == nil && len(s) == 1
	}
	body := id[1:]
	var h, m, s int
	var ok bool
	switch len(body) {
	case 1, 2:
		h, ok = digits(body)
	case 4:
		if h, ok = digits(body[:2]); ok {
			m, ok = digits(body[2:])
		}
	case 5:
		if body[2] != ':' {
			return 0, invalid
		}
		if h, ok = digits(body[:2]); ok {
			m, ok = digits(body[3:])
		}
	case 6:
		if h, ok = digits(body[:2]); ok {
			if m, ok = digits(body[2:4]); ok {
				s, ok = digits(body[4:])
			}
		}
	case 8:
		if body[2] != ':' || body[5] != ':' {
			return 0, invalid
		}
		if h, ok = digits(body[:2]); ok {
			if m, ok = digits(body[3:5]); ok {
				s, ok = digits(body[6:])
			}
		}
	}
	if !ok || strings.ContainsAny(body, "+- ") {
		return 0, invalid
	}
	rangeErr := func(what string, v, lo, hi int) *Error {
		return errJava(http.StatusBadRequest, "date_time_exception", fmt.Sprintf("Zone offset %s not in valid range: value %d is not in the range %d to %d", what, v, lo, hi))
	}
	if h > 18 {
		return 0, rangeErr("hours", h, -18, 18)
	}
	if m > 59 {
		return 0, rangeErr("minutes", m, -59, 59)
	}
	if s > 59 {
		return 0, rangeErr("seconds", s, -59, 59)
	}
	total := h*3600 + m*60 + s
	if total > 18*3600 {
		return 0, errJava(http.StatusBadRequest, "date_time_exception", "Zone offset not in valid range: -18:00 to +18:00")
	}
	if id[0] == '-' {
		total = -total
	}
	return total, nil
}

// Java's tz database has no EST, HST and MST regions; matching is case
// sensitive even where the zoneinfo files live on a case-insensitive disk.
var (
	zoneIDsOnce sync.Once
	zoneIDs     map[string]bool
)

func knownZoneID(id string) bool {
	switch id {
	case "EST", "HST", "MST", "Factory", "localtime", "posixrules", "posix", "right":
		return false
	}
	zoneIDsOnce.Do(func() {
		root, err := filepath.EvalSymlinks("/usr/share/zoneinfo")
		if err != nil {
			return
		}
		zoneIDs = map[string]bool{}
		_ = filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err == nil {
				zoneIDs[filepath.ToSlash(rel)] = true
			}
			return nil
		})
	})
	if zoneIDs == nil {
		return true
	}
	return zoneIDs[id]
}

// time values ------------------------------------------------------------------

// parseTimeValueMillis is TimeValue.parseTimeValue(...).millis().
func parseTimeValueMillis(value, setting string) (int64, *Error) {
	normalized := strings.TrimSpace(strings.ToLower(value))
	parse := func(suffix string, unit int64, divide bool) (int64, *Error) {
		s := strings.TrimSpace(normalized[:len(normalized)-len(suffix)])
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			if _, ferr := strconv.ParseFloat(s, 64); ferr == nil && javaDoubleString(s) {
				return 0, &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception",
					Reason: "failed to parse [" + value + "], fractional time values are not supported", Cause: aggNumberFormatError(s)}
			}
			return 0, &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "failed to parse [" + value + "]", Cause: aggNumberFormatError(s)}
		}
		if n < -1 {
			return 0, errIllegalArgument("failed to parse setting [%s] with value [%s] as a time value: negative durations are not supported", setting, value)
		}
		if divide {
			return n / unit, nil
		}
		return n * unit, nil
	}
	switch {
	case strings.HasSuffix(normalized, "nanos"):
		return parse("nanos", 1000000, true)
	case strings.HasSuffix(normalized, "micros"):
		return parse("micros", 1000, true)
	case strings.HasSuffix(normalized, "ms"):
		return parse("ms", 1, false)
	case strings.HasSuffix(normalized, "s"):
		return parse("s", 1000, false)
	case strings.HasSuffix(value, "m"):
		return parse("m", 60000, false)
	case strings.HasSuffix(normalized, "h"):
		return parse("h", 3600000, false)
	case strings.HasSuffix(normalized, "d"):
		return parse("d", 86400000, false)
	case isMinusOneLiteral(normalized):
		return -1, nil
	case isZeroLiteral(normalized):
		return 0, nil
	}
	return 0, errIllegalArgument("failed to parse setting [%s] with value [%s] as a time value: unit is missing or unrecognized", setting, value)
}

// calendar units of date_histogram (DateHistogramAggregationBuilder.DATE_FIELD_UNITS)
const (
	unitSecond = iota + 1
	unitMinute
	unitHour
	unitDay
	unitWeek
	unitMonth
	unitQuarter
	unitYear
)

var calendarUnits = map[string]int{
	"year": unitYear, "1y": unitYear, "quarter": unitQuarter, "1q": unitQuarter, "month": unitMonth, "1M": unitMonth,
	"week": unitWeek, "1w": unitWeek, "day": unitDay, "1d": unitDay, "hour": unitHour, "1h": unitHour,
	"minute": unitMinute, "1m": unitMinute, "second": unitSecond, "1s": unitSecond,
}

var unitMillis = map[int]int64{unitSecond: 1000, unitMinute: 60000, unitHour: 3600000, unitDay: 86400000, unitWeek: 7 * 86400000,
	unitMonth: 2629746000, unitQuarter: 7889238000, unitYear: 31556952000}

// dateRounding is a prepared Rounding: a calendar unit or a fixed interval
// in a time zone, shifted by an offset.
type dateRounding struct {
	unit     int   // calendar unit, 0 for a fixed interval
	interval int64 // fixed interval in millis
	loc      *time.Location
	offset   int64
}

func (r *dateRounding) round(utc int64) int64 {
	return r.roundZoned(utc-r.offset) + r.offset
}

func (r *dateRounding) next(utc int64) int64 {
	return r.nextZoned(utc-r.offset) + r.offset
}

func (r *dateRounding) roundZoned(utc int64) int64 {
	if r.unit == 0 {
		return r.roundInterval(utc)
	}
	if r.unit >= unitDay {
		local := localOf(utc, r.loc)
		return firstTimeOnDay(truncateLocal(local, r.unit), r.loc)
	}
	instant := utc
	var truncated int64
	var ok bool
	// Bounded the same way periodAt's own loops are (see maxPeriodMerge): a
	// real zone never needs more than one or two steps back to a transition
	// whose rounded local time is already <= it, so this only guards against
	// previousTransition never reaching the beginning of time on a platform
	// whose ZoneBounds() misbehaves (observed hanging windows-latest CI).
	for i := 0; i < maxPeriodMerge; i++ {
		truncated, ok = r.truncateAsLocalTime(instant)
		prev, hasPrev := previousTransition(instant, r.loc)
		if !hasPrev {
			return truncated
		}
		if ok && prev <= truncated {
			return truncated
		}
		instant = prev - 1
	}
	return truncated
}

func (r *dateRounding) truncateAsLocalTime(instant int64) (int64, bool) {
	local := truncateLocal(localOf(instant, r.loc), r.unit)
	offsets := validOffsets(local, r.loc)
	for i := len(offsets) - 1; i >= 0; i-- {
		if res := local - int64(offsets[i])*1000; res <= instant {
			return res, true
		}
	}
	return 0, false
}

func (r *dateRounding) nextZoned(utc int64) int64 {
	if r.unit == 0 {
		offset := int64(offsetAt(utc, r.loc)) * 1000
		local := utc + r.interval + offset
		return zonedOfLocal(local, r.loc)
	}
	if r.unit >= unitDay {
		local := truncateLocal(localOf(utc, r.loc), r.unit)
		return firstTimeOnDay(nextRelevantMidnight(local, r.unit), r.loc)
	}
	step := unitMillis[r.unit]
	if next := r.roundZoned(utc + step); utc < next {
		return next
	}
	return r.roundZoned(utc + 2*step)
}

// roundInterval is TimeIntervalRounding.JavaTimeRounding.round.
func (r *dateRounding) roundInterval(utc int64) int64 {
	local := utc + int64(offsetAt(utc, r.loc))*1000
	rounded := roundKey(local, r.interval) * r.interval
	offsets := validOffsets(rounded, r.loc)
	if len(offsets) == 0 {
		return gapTransition(rounded, r.loc)
	}
	prev, hasPrev := previousTransition(utc+1, r.loc)
	for i := len(offsets) - 1; i >= 0; i-- {
		inst := rounded - int64(offsets[i])*1000
		if hasPrev && inst < prev {
			return r.roundInterval(prev - 1)
		}
		if utc >= inst {
			return inst
		}
	}
	return rounded - int64(offsets[0])*1000
}

func roundKey(value, interval int64) int64 {
	if value < 0 {
		return (value - interval + 1) / interval
	}
	return value / interval
}

// local date-times are represented as the epoch millis of the same wall
// clock in UTC.

func localOf(utc int64, loc *time.Location) int64 {
	return utc + int64(offsetAt(utc, loc))*1000
}

func offsetAt(utc int64, loc *time.Location) int {
	_, off := time.UnixMilli(utc).In(loc).Zone()
	return off
}

func truncateLocal(local int64, unit int) int64 {
	t := time.UnixMilli(local).UTC()
	var out time.Time
	switch unit {
	case unitSecond:
		return floorMillis(local, 1000)
	case unitMinute:
		return floorMillis(local, 60000)
	case unitHour:
		return floorMillis(local, 3600000)
	case unitDay:
		out = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case unitWeek:
		wd := int(t.Weekday())
		if wd == 0 {
			wd = 7
		}
		out = time.Date(t.Year(), t.Month(), t.Day()-(wd-1), 0, 0, 0, 0, time.UTC)
	case unitMonth:
		out = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	case unitQuarter:
		out = time.Date(t.Year(), time.Month((int(t.Month())-1)/3*3+1), 1, 0, 0, 0, 0, time.UTC)
	case unitYear:
		out = time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return out.UnixMilli()
}

func floorMillis(v, step int64) int64 {
	return roundKey(v, step) * step
}

func nextRelevantMidnight(local int64, unit int) int64 {
	t := time.UnixMilli(local).UTC()
	switch unit {
	case unitDay:
		t = t.AddDate(0, 0, 1)
	case unitWeek:
		t = t.AddDate(0, 0, 7)
	case unitMonth:
		t = t.AddDate(0, 1, 0)
	case unitQuarter:
		t = t.AddDate(0, 3, 0)
	case unitYear:
		t = t.AddDate(1, 0, 0)
	}
	return t.UnixMilli()
}

// firstTimeOnDay converts a local midnight to the first instant of that day.
func firstTimeOnDay(local int64, loc *time.Location) int64 {
	if offsets := validOffsets(local, loc); len(offsets) > 0 {
		return local - int64(offsets[0])*1000
	}
	return gapTransition(local, loc)
}

// zonePeriod is a span of instants with one offset.
type zonePeriod struct {
	start, end int64 // millis; math bounds when open
	offset     int
}

// maxPeriodMerge bounds the neighbour-merging loops in periodAt and the
// loops in validOffsets/gapTransition that step from period to period. A
// real zone's whole history never has more than a handful of consecutive
// same-offset periods (an abbreviation-only change) or DST transitions
// within the couple of days these scan, so this is never hit in practice;
// it only guards against a Location whose ZoneBounds() does not obey its
// documented contract (observed hanging windows-latest CI). It only bounds
// the WORK each call can do, deliberately not changing what value a call
// that finishes within the bound computes, since a platform difference in
// exactly which (still valid) neighbouring bound gets merged in should not
// change the result either.
const maxPeriodMerge = 1000

func periodAt(utc int64, loc *time.Location) zonePeriod {
	t := time.UnixMilli(utc).In(loc)
	_, off := t.Zone()
	p := zonePeriod{start: -1 << 62, end: 1 << 62, offset: off}
	start, end := t.ZoneBounds()
	if !start.IsZero() {
		p.start = start.UnixMilli()
	}
	if !end.IsZero() {
		p.end = end.UnixMilli()
	}
	// merge neighbouring periods with the same offset (abbreviation changes)
	for i := 0; p.start > -1<<62 && i < maxPeriodMerge; i++ {
		prev := time.UnixMilli(p.start - 1).In(loc)
		if _, o := prev.Zone(); o != off {
			break
		}
		s, _ := prev.ZoneBounds()
		if s.IsZero() {
			p.start = -1 << 62
			break
		}
		p.start = s.UnixMilli()
	}
	for i := 0; p.end < 1<<62 && i < maxPeriodMerge; i++ {
		next := time.UnixMilli(p.end).In(loc)
		if _, o := next.Zone(); o != off {
			break
		}
		_, e := next.ZoneBounds()
		if e.IsZero() {
			p.end = 1 << 62
			break
		}
		p.end = e.UnixMilli()
	}
	return p
}

// validOffsets is ZoneRules.getValidOffsets for a local date-time, in
// chronological order. The 52-hour window never spans more than a couple of
// periods for a real zone (at most one DST transition plus its neighbours);
// maxPeriodMerge again backstops a periodAt that fails to make progress.
func validOffsets(local int64, loc *time.Location) []int {
	var out []int
	seen := map[int64]bool{}
	at := local - 26*3600000
	for i := 0; at <= local+26*3600000 && i < maxPeriodMerge; i++ {
		p := periodAt(at, loc)
		if !seen[p.start] {
			seen[p.start] = true
			inst := local - int64(p.offset)*1000
			if inst >= p.start && inst < p.end {
				out = append(out, p.offset)
			}
		}
		if p.end >= 1<<62 {
			break
		}
		at = p.end
	}
	return out
}

// gapTransition is the instant of the transition that skipped a local time.
func gapTransition(local int64, loc *time.Location) int64 {
	before := periodAt(local-26*3600000, loc)
	for i := 0; before.end < 1<<62 && before.end <= local+26*3600000 && i < maxPeriodMerge; i++ {
		after := periodAt(before.end, loc)
		if after.offset > before.offset && local >= before.end+int64(before.offset)*1000 && local < before.end+int64(after.offset)*1000 {
			return before.end
		}
		before = after
	}
	return local - int64(offsetAt(local, loc))*1000
}

// previousTransition is ZoneRules.previousTransition: the last offset change
// strictly before the instant (transitions fall on whole seconds).
func previousTransition(utc int64, loc *time.Location) (int64, bool) {
	p := periodAt(utc-1, loc)
	if p.start <= -1<<62 {
		return 0, false
	}
	return p.start, true
}

// zonedOfLocal is ZonedDateTime.ofLocal(local, zone, null): the earlier
// offset in overlaps; in gaps the local time moves forward by the gap, which
// is the instant local - offsetBefore.
func zonedOfLocal(local int64, loc *time.Location) int64 {
	if offsets := validOffsets(local, loc); len(offsets) > 0 {
		return local - int64(offsets[0])*1000
	}
	t := gapTransition(local, loc)
	return local - int64(periodAt(t-1, loc).offset)*1000
}
