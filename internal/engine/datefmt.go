package engine

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// This file is a port of the parts of java.time.format.DateTimeFormatter
// (strict parsing, STRICT resolver) and OpenSearch's DateFormatters that
// date mappings use: named formats, Java patterns, epoch formats, the
// round-up parser of range queries and Java's printing rules.

type dtField int

const (
	dtYear dtField = iota
	dtYearOfEra
	dtEra
	dtMonth
	dtDay
	dtDayOfYear
	dtDayOfWeek
	dtWeekBasedYear
	dtWeekOfWeekBasedYear
	dtLocalDayOfWeek
	dtAmPm
	dtHourOfDay
	dtClockHourOfDay
	dtHourOfAmPm
	dtClockHourOfAmPm
	dtMinute
	dtSecond
	dtNano
	dtMilliOfDay
	dtNanoOfDay
	dtQuarter
	dtOffset
	dtEpochValue    // epoch millis or seconds (unsigned)
	dtEpochFraction // nanos of milli or nanos of second
	dtEpochNegative
	dtFieldCount
)

type signStyle int

const (
	signNormal signStyle = iota
	signAlways
	signNever
	signNotNegative
	signExceedsPad
)

func (s signStyle) parseAllowed(positive, fixedWidth bool) bool {
	switch s {
	case signNormal:
		return !positive
	case signAlways, signExceedsPad:
		return true
	}
	return false
}

// dtParsed holds the fields a parse produced.
type dtParsed struct {
	set      [dtFieldCount]bool
	val      [dtFieldCount]int64
	conflict bool
	loc      *time.Location
	zoneSet  bool
}

func (p *dtParsed) setField(f dtField, v int64) {
	if p.set[f] && p.val[f] != v {
		p.conflict = true
	}
	p.set[f] = true
	p.val[f] = v
}

type dtElem interface {
	// parse returns the position after the element or ^errorPosition.
	parse(p *dtParsed, text string, pos int) int
	// print appends the element for t; false when a field is unavailable.
	print(b *strings.Builder, t time.Time) bool
}

// dtSeq is a sequence of elements, optional sections included.
type dtSeq struct {
	elems    []dtElem
	optional bool
}

func (s *dtSeq) parse(p *dtParsed, text string, pos int) int {
	if s.optional {
		saved := *p
		cur := pos
		for _, e := range s.elems {
			cur = e.parse(p, text, cur)
			if cur < 0 {
				*p = saved
				return pos
			}
		}
		return cur
	}
	for _, e := range s.elems {
		pos = e.parse(p, text, pos)
		if pos < 0 {
			return pos
		}
	}
	return pos
}

func (s *dtSeq) print(b *strings.Builder, t time.Time) bool {
	if s.optional {
		var sub strings.Builder
		for _, e := range s.elems {
			if !e.print(&sub, t) {
				return true
			}
		}
		b.WriteString(sub.String())
		return true
	}
	for _, e := range s.elems {
		if !e.print(b, t) {
			return false
		}
	}
	return true
}

// literal ----------------------------------------------------------------

type dtLiteral struct{ s string }

func (l dtLiteral) parse(p *dtParsed, text string, pos int) int {
	if pos > len(text) {
		return ^pos
	}
	if !strings.HasPrefix(text[pos:], l.s) {
		return ^pos
	}
	return pos + len(l.s)
}

func (l dtLiteral) print(b *strings.Builder, t time.Time) bool {
	b.WriteString(l.s)
	return true
}

// numbers ------------------------------------------------------------------

type dtNumber struct {
	field           dtField
	minW, maxW      int
	sign            signStyle
	subsequentWidth int // -1: fixed width
	reducedBase     int // > 0 for reduced two digit years
}

func (n *dtNumber) parse(p *dtParsed, text string, pos int) int {
	length := len(text)
	if pos >= length {
		return ^pos
	}
	position := pos
	negative, positive := false, false
	switch text[position] {
	case '+':
		if !n.sign.parseAllowed(true, n.minW == n.maxW) {
			return ^position
		}
		positive = true
		position++
	case '-':
		if !n.sign.parseAllowed(false, n.minW == n.maxW) {
			return ^position
		}
		negative = true
		position++
	default:
		if n.sign == signAlways {
			return ^position
		}
	}
	effMin := n.minW
	effMax := n.maxW
	if n.subsequentWidth > 0 {
		effMax += n.subsequentWidth
	}
	minEnd := position + effMin
	if minEnd > length {
		return ^position
	}
	var total int64
	var totalBig *big.Int
	cur := position
	for pass := 0; pass < 2; pass++ {
		maxEnd := position + effMax
		if maxEnd > length {
			maxEnd = length
		}
		for cur < maxEnd {
			ch := text[cur]
			cur++
			if ch < '0' || ch > '9' {
				cur--
				if cur < minEnd {
					return ^position
				}
				break
			}
			digit := int64(ch - '0')
			if cur-position > 18 {
				if totalBig == nil {
					totalBig = big.NewInt(total)
				}
				totalBig.Mul(totalBig, big.NewInt(10)).Add(totalBig, big.NewInt(digit))
			} else {
				total = total*10 + digit
			}
		}
		if n.subsequentWidth > 0 && pass == 0 {
			parseLen := cur - position
			effMax = parseLen - n.subsequentWidth
			if effMax < effMin {
				effMax = effMin
			}
			cur = position
			total = 0
			totalBig = nil
			continue
		}
		break
	}
	if negative {
		if (totalBig != nil && totalBig.Sign() == 0) || (totalBig == nil && total == 0) {
			return ^(position - 1)
		}
		if totalBig != nil {
			totalBig.Neg(totalBig)
		} else {
			total = -total
		}
	} else if n.sign == signExceedsPad {
		parseLen := cur - position
		if positive {
			if parseLen <= n.minW {
				return ^(position - 1)
			}
		} else if parseLen > n.minW {
			return ^position
		}
	}
	if totalBig != nil {
		if totalBig.BitLen() > 63 {
			totalBig.Quo(totalBig, big.NewInt(10))
			cur--
		}
		total = totalBig.Int64()
	}
	if n.reducedBase > 0 && cur-position == n.minW && total >= 0 {
		rng := int64(1)
		for i := 0; i < n.minW; i++ {
			rng *= 10
		}
		base := int64(n.reducedBase)
		lastPart := base % rng
		basePart := base - lastPart
		total = basePart + total
		if total < base {
			total += rng
		}
	}
	p.setField(n.field, total)
	return cur
}

func (n *dtNumber) print(b *strings.Builder, t time.Time) bool {
	v, ok := dtFieldValue(t, n.field)
	if !ok {
		return false
	}
	if n.reducedBase > 0 {
		v = ((v % 100) + 100) % 100
		s := strconv.FormatInt(v, 10)
		b.WriteString(strings.Repeat("0", 2-len(s)) + s)
		return true
	}
	abs := v
	if abs < 0 {
		abs = -abs
	}
	s := strconv.FormatInt(abs, 10)
	if v >= 0 {
		if n.sign == signExceedsPad && n.minW < 19 && len(s) > n.minW {
			b.WriteByte('+')
		} else if n.sign == signAlways {
			b.WriteByte('+')
		}
	} else {
		b.WriteByte('-')
	}
	if len(s) < n.minW {
		b.WriteString(strings.Repeat("0", n.minW-len(s)))
	}
	b.WriteString(s)
	return true
}

// fractions -----------------------------------------------------------------

type dtFraction struct {
	field           dtField // dtNano or dtEpochFraction
	minW, maxW      int
	decimalPoint    bool
	subsequentWidth int
	scale           int // digits of the unit: 9 for nanos of second, 6 for nanos of milli
}

func (f *dtFraction) parse(p *dtParsed, text string, pos int) int {
	length := len(text)
	if pos > length {
		return ^pos
	}
	if f.decimalPoint {
		if pos == length || text[pos] != '.' {
			if f.minW > 0 {
				return ^pos
			}
			return pos
		}
		pos++
	}
	minEnd := pos + f.minW
	if minEnd > length {
		return ^pos
	}
	maxEnd := pos + f.maxW
	if maxEnd > length {
		maxEnd = length
	}
	var total int64
	cur := pos
	for cur < maxEnd {
		ch := text[cur]
		cur++
		if ch < '0' || ch > '9' {
			// JDK 21: the digits before the non digit must reach the minimum
			if cur-1 < minEnd {
				return ^pos
			}
			cur--
			break
		}
		total = total*10 + int64(ch-'0')
	}
	digits := cur - pos
	value := total
	for i := digits; i < f.scale; i++ {
		value *= 10
	}
	for i := f.scale; i < digits; i++ {
		value /= 10
	}
	p.setField(f.field, value)
	return cur
}

func (f *dtFraction) print(b *strings.Builder, t time.Time) bool {
	v, ok := dtFieldValue(t, f.field)
	if !ok {
		return false
	}
	s := strconv.FormatInt(v, 10)
	s = strings.Repeat("0", f.scale-len(s)) + s
	s = strings.TrimRight(s, "0")
	if len(s) == 0 {
		if f.minW > 0 {
			if f.decimalPoint {
				b.WriteByte('.')
			}
			b.WriteString(strings.Repeat("0", f.minW))
		}
		return true
	}
	if len(s) < f.minW {
		s += strings.Repeat("0", f.minW-len(s))
	}
	if len(s) > f.maxW {
		s = s[:f.maxW]
	}
	if f.decimalPoint {
		b.WriteByte('.')
	}
	b.WriteString(s)
	return true
}

// text fields -------------------------------------------------------------

type dtText struct {
	field dtField
	names []string // index 0 is the value 1 (months, days) or 0 (ampm, era)
	base  int64
}

var (
	monthsShort = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	monthsFull  = []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	daysShort   = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	daysFull    = []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
	ampmNames   = []string{"AM", "PM"}
	eraShort    = []string{"BC", "AD"}
	eraFull     = []string{"Before Christ", "Anno Domini"}
)

func (x dtText) parse(p *dtParsed, text string, pos int) int {
	best, bestLen := -1, 0
	for i, name := range x.names {
		if len(name) > bestLen && strings.HasPrefix(text[pos:], name) {
			best, bestLen = i, len(name)
		}
	}
	if best < 0 {
		return ^pos
	}
	p.setField(x.field, int64(best)+x.base)
	return pos + bestLen
}

func (x dtText) print(b *strings.Builder, t time.Time) bool {
	v, ok := dtFieldValue(t, x.field)
	if !ok {
		return false
	}
	i := int(v - x.base)
	if i < 0 || i >= len(x.names) {
		return false
	}
	b.WriteString(x.names[i])
	return true
}

// offsets and zones -----------------------------------------------------------

// dtOffsetElem is OffsetIdPrinterParser.
type dtOffsetElem struct {
	pattern  string // +HH, +HHmm, +HH:mm, +HHMM, +HH:MM, +HHMMss, +HH:MM:ss, +HHMMSS, +HH:MM:SS
	noOffset string
}

func (o dtOffsetElem) parse(p *dtParsed, text string, pos int) int {
	length := len(text)
	if o.noOffset != "" {
		if pos == length {
			return ^pos
		}
		if strings.HasPrefix(text[pos:], o.noOffset) {
			p.setField(dtOffset, 0)
			return pos + len(o.noOffset)
		}
	}
	if pos >= length || (text[pos] != '+' && text[pos] != '-') {
		return ^pos
	}
	negative := text[pos] == '-'
	colon := strings.Contains(o.pattern, ":")
	cur := pos + 1
	two := func() (int, bool) {
		if cur+2 > length || !isASCIIDigit(text[cur]) || !isASCIIDigit(text[cur+1]) {
			return 0, false
		}
		v := int(text[cur]-'0')*10 + int(text[cur+1]-'0')
		cur += 2
		return v, true
	}
	part := func(mandatory bool) (int, bool) {
		save := cur
		if colon {
			if cur >= length || text[cur] != ':' {
				cur = save
				return 0, !mandatory
			}
			cur++
		}
		v, ok := two()
		if !ok {
			cur = save
			return 0, !mandatory
		}
		return v, true
	}
	hours, ok := two()
	if !ok {
		return ^pos
	}
	minutes, seconds := 0, 0
	switch o.pattern {
	case "+HH":
	case "+HHmm", "+HH:mm":
		if minutes, ok = part(false); !ok {
			return ^pos
		}
	case "+HHMM", "+HH:MM":
		if minutes, ok = part(true); !ok {
			return ^pos
		}
	case "+HHMMss", "+HH:MM:ss":
		if minutes, ok = part(true); !ok {
			return ^pos
		}
		if seconds, ok = part(false); !ok {
			return ^pos
		}
	case "+HHMMSS", "+HH:MM:SS":
		if minutes, ok = part(true); !ok {
			return ^pos
		}
		if seconds, ok = part(true); !ok {
			return ^pos
		}
	}
	if hours > 23 || minutes > 59 || seconds > 59 {
		return ^pos
	}
	total := int64(hours*3600 + minutes*60 + seconds)
	if total > 18*3600 {
		return ^pos
	}
	if negative {
		total = -total
	}
	p.setField(dtOffset, total)
	return cur
}

func (o dtOffsetElem) print(b *strings.Builder, t time.Time) bool {
	_, off := t.Zone()
	if off == 0 && o.noOffset != "" {
		b.WriteString(o.noOffset)
		return true
	}
	if off < 0 {
		b.WriteByte('-')
		off = -off
	} else {
		b.WriteByte('+')
	}
	h, m, s := off/3600, off/60%60, off%60
	colon := strings.Contains(o.pattern, ":")
	fmt.Fprintf(b, "%02d", h)
	withMinutes := strings.ContainsAny(o.pattern, "mM")
	if withMinutes && (m != 0 || s != 0 || strings.Contains(o.pattern, "MM")) {
		if colon {
			b.WriteByte(':')
		}
		fmt.Fprintf(b, "%02d", m)
		if strings.ContainsAny(o.pattern, "sS") && (s != 0 || strings.Contains(o.pattern, "SS")) {
			if colon {
				b.WriteByte(':')
			}
			fmt.Fprintf(b, "%02d", s)
		}
	}
	return true
}

// dtZoneElem is appendZoneOrOffsetId (zoneOnly for appendZoneId).
type dtZoneElem struct{ zoneOnly bool }

func (z dtZoneElem) parse(p *dtParsed, text string, pos int) int {
	length := len(text)
	if pos >= length {
		return ^pos
	}
	c := text[pos]
	if c == '+' || c == '-' {
		sub := &dtParsed{}
		end := dtOffsetElem{pattern: "+HH:MM:ss", noOffset: "Z"}.parse(sub, text, pos)
		if end < 0 {
			return ^pos
		}
		p.loc = time.FixedZone("", int(sub.val[dtOffset]))
		p.zoneSet = true
		return end
	}
	if strings.HasPrefix(text[pos:], "UTC") || strings.HasPrefix(text[pos:], "GMT") || strings.HasPrefix(text[pos:], "UT") {
		prefixLen := 2
		if strings.HasPrefix(text[pos:], "UTC") || strings.HasPrefix(text[pos:], "GMT") {
			prefixLen = 3
		}
		end := pos + prefixLen
		if end < length && (text[end] == '+' || text[end] == '-') {
			sub := &dtParsed{}
			oe := dtOffsetElem{pattern: "+HH:MM:ss", noOffset: "0"}.parse(sub, text, end)
			if oe >= 0 {
				p.loc = time.FixedZone("", int(sub.val[dtOffset]))
				p.zoneSet = true
				return oe
			}
		}
		p.loc = time.UTC
		p.zoneSet = true
		return end
	}
	// region ids: the longest prefix that names a zone
	end := pos
	for end < length {
		ch := text[end]
		if isASCIIDigit(ch) || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '/' || ch == '_' || ch == '-' || ch == '+' {
			end++
			continue
		}
		break
	}
	for e := end; e > pos+1; e-- {
		name := text[pos:e]
		if !strings.ContainsAny(name, "/") && name != "Z" {
			continue
		}
		if loc, err := loadLocation(name); err == nil {
			p.loc = loc
			p.zoneSet = true
			return e
		}
	}
	if c == 'Z' {
		p.loc = time.UTC
		p.zoneSet = true
		return pos + 1
	}
	return ^pos
}

func (z dtZoneElem) print(b *strings.Builder, t time.Time) bool {
	name, off := t.Zone()
	if t.Location() == time.UTC || (off == 0 && (name == "" || name == "UTC")) {
		b.WriteByte('Z')
		return true
	}
	if loc := t.Location().String(); strings.Contains(loc, "/") {
		b.WriteString(loc)
		return true
	}
	return dtOffsetElem{pattern: "+HH:MM:ss", noOffset: "Z"}.print(b, t)
}

// dtZoneText is the 'z' pattern letter.
type dtZoneText struct{}

func (dtZoneText) parse(p *dtParsed, text string, pos int) int {
	for _, name := range []string{"UTC", "GMT", "UT", "Z"} {
		if strings.HasPrefix(text[pos:], name) {
			p.loc = time.UTC
			p.zoneSet = true
			return pos + len(name)
		}
	}
	return dtZoneElem{}.parse(p, text, pos)
}

func (dtZoneText) print(b *strings.Builder, t time.Time) bool {
	name, _ := t.Zone()
	if t.Location() == time.UTC || name == "" {
		b.WriteString("Z")
		return true
	}
	b.WriteString(name)
	return true
}

// epoch -------------------------------------------------------------------

type dtEpoch struct{ seconds bool }

func (e dtEpoch) parse(p *dtParsed, text string, pos int) int {
	length := len(text)
	cur := pos
	if cur < length && text[cur] == '-' {
		p.setField(dtEpochNegative, 1)
		cur++
	}
	start := cur
	for cur < length && cur-start < 19 && isASCIIDigit(text[cur]) {
		cur++
	}
	if cur == start {
		return ^start
	}
	v, err := strconv.ParseInt(text[start:cur], 10, 64)
	if err != nil {
		return ^start
	}
	p.setField(dtEpochValue, v)
	if cur < length && text[cur] == '.' {
		cur++
		maxDigits, scale := 6, 6
		if e.seconds {
			maxDigits, scale = 9, 9
		}
		fracStart := cur
		for cur < length && cur-fracStart < maxDigits && isASCIIDigit(text[cur]) {
			cur++
		}
		frac := int64(0)
		if cur > fracStart {
			frac, _ = strconv.ParseInt(text[fracStart:cur], 10, 64)
		}
		for i := cur - fracStart; i < scale; i++ {
			frac *= 10
		}
		p.setField(dtEpochFraction, frac)
	}
	return cur
}

func (e dtEpoch) print(b *strings.Builder, t time.Time) bool {
	secs := t.Unix()
	nanos := int64(t.Nanosecond())
	if e.seconds {
		b.WriteString(strconv.FormatInt(secs, 10))
		if nanos != 0 {
			s := strings.TrimRight(fmt.Sprintf("%09d", nanos), "0")
			b.WriteByte('.')
			b.WriteString(s)
		}
		return true
	}
	millis := secs*1000 + nanos/1e6
	b.WriteString(strconv.FormatInt(millis, 10))
	if rest := nanos % 1e6; rest != 0 {
		b.WriteByte('.')
		b.WriteString(strings.TrimRight(fmt.Sprintf("%06d", rest), "0"))
	}
	return true
}

// field access for printing -------------------------------------------------

func dtFieldValue(t time.Time, f dtField) (int64, bool) {
	switch f {
	case dtYear:
		return int64(t.Year()), true
	case dtYearOfEra:
		y := int64(t.Year())
		if y <= 0 {
			return 1 - y, true
		}
		return y, true
	case dtEra:
		if t.Year() <= 0 {
			return 0, true
		}
		return 1, true
	case dtMonth:
		return int64(t.Month()), true
	case dtDay:
		return int64(t.Day()), true
	case dtDayOfYear:
		return int64(t.YearDay()), true
	case dtDayOfWeek, dtLocalDayOfWeek:
		wd := int64(t.Weekday())
		if wd == 0 {
			wd = 7
		}
		return wd, true
	case dtWeekBasedYear:
		y, _ := t.ISOWeek()
		return int64(y), true
	case dtWeekOfWeekBasedYear:
		_, w := t.ISOWeek()
		return int64(w), true
	case dtAmPm:
		if t.Hour() >= 12 {
			return 1, true
		}
		return 0, true
	case dtHourOfDay:
		return int64(t.Hour()), true
	case dtClockHourOfDay:
		if t.Hour() == 0 {
			return 24, true
		}
		return int64(t.Hour()), true
	case dtHourOfAmPm:
		return int64(t.Hour() % 12), true
	case dtClockHourOfAmPm:
		h := t.Hour() % 12
		if h == 0 {
			h = 12
		}
		return int64(h), true
	case dtMinute:
		return int64(t.Minute()), true
	case dtSecond:
		return int64(t.Second()), true
	case dtNano:
		return int64(t.Nanosecond()), true
	case dtMilliOfDay:
		return int64(t.Hour()*3600000 + t.Minute()*60000 + t.Second()*1000 + t.Nanosecond()/1e6), true
	case dtNanoOfDay:
		return int64(t.Hour()*3600+t.Minute()*60+t.Second())*1e9 + int64(t.Nanosecond()), true
	case dtQuarter:
		return int64((int(t.Month())-1)/3 + 1), true
	case dtOffset:
		_, off := t.Zone()
		return int64(off), true
	}
	return 0, false
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

// builder -----------------------------------------------------------------------

type dtBuilder struct {
	seqs    []*dtSeq
	active  []int // valueParserIndex per open sequence
	hasYear bool  // the pattern uses YEAR (proleptic year), resolved strictly
	hasYOE  bool
	hasDOY  bool
	hasWeek bool
}

func newDTBuilder() *dtBuilder {
	return &dtBuilder{seqs: []*dtSeq{{}}, active: []int{-1}}
}

func (b *dtBuilder) cur() *dtSeq { return b.seqs[len(b.seqs)-1] }

func (b *dtBuilder) appendInternal(e dtElem) int {
	s := b.cur()
	s.elems = append(s.elems, e)
	b.active[len(b.active)-1] = -1
	return len(s.elems) - 1
}

func (b *dtBuilder) noteField(f dtField) {
	switch f {
	case dtYear:
		b.hasYear = true
	case dtYearOfEra:
		b.hasYOE = true
	case dtDayOfYear:
		b.hasDOY = true
	case dtWeekBasedYear, dtWeekOfWeekBasedYear:
		b.hasWeek = true
	}
}

// appendValue follows DateTimeFormatterBuilder.appendValue, including the
// adjacent value parsing of patterns such as yyyyMMdd.
func (b *dtBuilder) appendValue(n *dtNumber) {
	b.noteField(n.field)
	idx := b.active[len(b.active)-1]
	s := b.cur()
	if idx >= 0 {
		base := s.elems[idx]
		if n.minW == n.maxW && n.sign == signNotNegative {
			addSubsequent(base, n.maxW)
			n.subsequentWidth = -1
			b.appendInternal(n)
			b.active[len(b.active)-1] = idx
		} else {
			setFixed(base)
			b.active[len(b.active)-1] = b.appendInternal(n)
		}
		return
	}
	b.active[len(b.active)-1] = b.appendInternal(n)
}

func (b *dtBuilder) appendFraction(f *dtFraction) {
	if f.minW == f.maxW && !f.decimalPoint {
		idx := b.active[len(b.active)-1]
		s := b.cur()
		if idx >= 0 {
			addSubsequent(s.elems[idx], f.maxW)
			f.subsequentWidth = -1
			b.appendInternal(f)
			b.active[len(b.active)-1] = idx
			return
		}
		b.active[len(b.active)-1] = b.appendInternal(f)
		return
	}
	b.appendInternal(f)
}

func addSubsequent(e dtElem, w int) {
	switch t := e.(type) {
	case *dtNumber:
		if t.subsequentWidth >= 0 {
			t.subsequentWidth += w
		}
	case *dtFraction:
		if t.subsequentWidth >= 0 {
			t.subsequentWidth += w
		}
	}
}

func setFixed(e dtElem) {
	switch t := e.(type) {
	case *dtNumber:
		t.subsequentWidth = -1
	case *dtFraction:
		t.subsequentWidth = -1
	}
}

func (b *dtBuilder) num(f dtField, minW, maxW int, sign signStyle) *dtBuilder {
	b.appendValue(&dtNumber{field: f, minW: minW, maxW: maxW, sign: sign})
	return b
}

func (b *dtBuilder) lit(s string) *dtBuilder {
	b.appendInternal(dtLiteral{s: s})
	return b
}

func (b *dtBuilder) frac(minW, maxW int, point bool) *dtBuilder {
	b.appendFraction(&dtFraction{field: dtNano, minW: minW, maxW: maxW, decimalPoint: point, scale: 9})
	return b
}

func (b *dtBuilder) elem(e dtElem) *dtBuilder {
	b.appendInternal(e)
	return b
}

func (b *dtBuilder) opt() *dtBuilder {
	b.active[len(b.active)-1] = -1
	b.seqs = append(b.seqs, &dtSeq{optional: true})
	b.active = append(b.active, -1)
	return b
}

func (b *dtBuilder) end() *dtBuilder {
	s := b.cur()
	b.seqs = b.seqs[:len(b.seqs)-1]
	b.active = b.active[:len(b.active)-1]
	if len(s.elems) > 0 {
		b.appendInternal(s)
	}
	return b
}

func (b *dtBuilder) seq() *dtSeq {
	for len(b.seqs) > 1 {
		b.end()
	}
	return b.seqs[0]
}

// Java patterns ------------------------------------------------------------------------

type dtPatternError struct{ msg string }

func (e *dtPatternError) Error() string { return e.msg }

// compileJavaPattern parses a DateTimeFormatter pattern.
func compileJavaPattern(pattern string) (*dtBuilder, error) {
	b := newDTBuilder()
	for pos := 0; pos < len(pattern); pos++ {
		cur := pattern[pos]
		switch {
		case (cur >= 'A' && cur <= 'Z') || (cur >= 'a' && cur <= 'z'):
			start := pos
			pos++
			for pos < len(pattern) && pattern[pos] == cur {
				pos++
			}
			count := pos - start
			pos--
			if err := b.patternLetter(cur, count); err != nil {
				return nil, err
			}
		case cur == '\'':
			start := pos
			pos++
			for ; pos < len(pattern); pos++ {
				if pattern[pos] == '\'' {
					if pos+1 < len(pattern) && pattern[pos+1] == '\'' {
						pos++
					} else {
						break
					}
				}
			}
			if pos >= len(pattern) {
				return nil, &dtPatternError{"Pattern ends with an incomplete string literal: " + pattern}
			}
			str := pattern[start+1 : pos]
			if str == "" {
				b.lit("'")
			} else {
				b.lit(strings.ReplaceAll(str, "''", "'"))
			}
		case cur == '[':
			b.opt()
		case cur == ']':
			if len(b.seqs) == 1 {
				return nil, &dtPatternError{"Pattern invalid as it contains ] without previous ["}
			}
			b.end()
		case cur == '{' || cur == '}' || cur == '#':
			return nil, &dtPatternError{fmt.Sprintf("Pattern includes reserved character: '%c'", cur)}
		default:
			b.lit(string(cur))
		}
	}
	return b, nil
}

func tooMany(c byte) error { return &dtPatternError{fmt.Sprintf("Too many pattern letters: %c", c)} }

func (b *dtBuilder) patternLetter(cur byte, count int) error {
	switch cur {
	case 'u', 'y':
		field := dtYear
		if cur == 'y' {
			field = dtYearOfEra
		}
		switch {
		case count == 2:
			b.noteField(field)
			b.appendValue(&dtNumber{field: field, minW: 2, maxW: 2, sign: signNotNegative, reducedBase: 2000})
		case count < 4:
			b.num(field, count, 19, signNormal)
		default:
			b.num(field, count, 19, signExceedsPad)
		}
	case 'M', 'L':
		switch count {
		case 1:
			b.num(dtMonth, 1, 19, signNormal)
		case 2:
			b.num(dtMonth, 2, 2, signNotNegative)
		case 3:
			b.elem(dtText{field: dtMonth, names: monthsShort, base: 1})
		case 4:
			b.elem(dtText{field: dtMonth, names: monthsFull, base: 1})
		case 5:
			b.elem(dtText{field: dtMonth, names: []string{"J", "F", "M", "A", "M", "J", "J", "A", "S", "O", "N", "D"}, base: 1})
		default:
			return tooMany(cur)
		}
	case 'Q', 'q':
		switch count {
		case 1:
			b.num(dtQuarter, 1, 19, signNormal)
		case 2:
			b.num(dtQuarter, 2, 2, signNotNegative)
		case 3, 4, 5:
			b.elem(dtText{field: dtQuarter, names: []string{"Q1", "Q2", "Q3", "Q4"}, base: 1})
		default:
			return tooMany(cur)
		}
	case 'E':
		switch count {
		case 1, 2, 3:
			b.elem(dtText{field: dtDayOfWeek, names: daysShort, base: 1})
		case 4:
			b.elem(dtText{field: dtDayOfWeek, names: daysFull, base: 1})
		case 5:
			b.elem(dtText{field: dtDayOfWeek, names: []string{"M", "T", "W", "T", "F", "S", "S"}, base: 1})
		default:
			return tooMany(cur)
		}
	case 'e', 'c':
		switch count {
		case 1, 2:
			if cur == 'c' && count == 2 {
				return &dtPatternError{"Invalid pattern \"cc\""}
			}
			b.num(dtLocalDayOfWeek, count, count, signNotNegative)
		case 3:
			b.elem(dtText{field: dtDayOfWeek, names: daysShort, base: 1})
		case 4:
			b.elem(dtText{field: dtDayOfWeek, names: daysFull, base: 1})
		case 5:
			b.elem(dtText{field: dtDayOfWeek, names: []string{"M", "T", "W", "T", "F", "S", "S"}, base: 1})
		default:
			return tooMany(cur)
		}
	case 'a':
		if count != 1 {
			return tooMany(cur)
		}
		b.elem(dtText{field: dtAmPm, names: ampmNames})
	case 'G':
		switch count {
		case 1, 2, 3:
			b.elem(dtText{field: dtEra, names: eraShort})
		case 4:
			b.elem(dtText{field: dtEra, names: eraFull})
		case 5:
			b.elem(dtText{field: dtEra, names: []string{"B", "A"}})
		default:
			return tooMany(cur)
		}
	case 'S':
		b.appendFraction(&dtFraction{field: dtNano, minW: count, maxW: count, scale: 9})
	case 'F':
		if count != 1 {
			return tooMany(cur)
		}
		b.num(dtDay, 1, 19, signNormal)
	case 'd', 'h', 'H', 'k', 'K', 'm', 's':
		field := map[byte]dtField{'d': dtDay, 'h': dtClockHourOfAmPm, 'H': dtHourOfDay, 'k': dtClockHourOfDay, 'K': dtHourOfAmPm, 'm': dtMinute, 's': dtSecond}[cur]
		switch count {
		case 1:
			b.num(field, 1, 19, signNormal)
		case 2:
			b.num(field, 2, 2, signNotNegative)
		default:
			return tooMany(cur)
		}
	case 'D':
		switch count {
		case 1:
			b.num(dtDayOfYear, 1, 19, signNormal)
		case 2, 3:
			b.num(dtDayOfYear, count, 3, signNotNegative)
		default:
			return tooMany(cur)
		}
	case 'A':
		b.num(dtMilliOfDay, count, 19, signNotNegative)
	case 'n':
		b.num(dtNano, count, 19, signNotNegative)
	case 'N':
		b.num(dtNanoOfDay, count, 19, signNotNegative)
	case 'g':
		b.num(dtDay, count, 19, signNormal)
	case 'z':
		if count > 4 {
			return tooMany(cur)
		}
		b.elem(dtZoneText{})
	case 'V':
		if count != 2 {
			return &dtPatternError{"Pattern letter count must be 2: V"}
		}
		b.elem(dtZoneElem{zoneOnly: true})
	case 'v':
		if count != 1 && count != 4 {
			return &dtPatternError{"Wrong number of pattern letters: v"}
		}
		b.elem(dtZoneText{})
	case 'Z':
		switch {
		case count < 4:
			b.elem(dtOffsetElem{pattern: "+HHMM", noOffset: "+0000"})
		case count == 4:
			b.elem(dtOffsetElem{pattern: "+HH:MM:ss", noOffset: "GMT"})
		case count == 5:
			b.elem(dtOffsetElem{pattern: "+HH:MM:ss", noOffset: "Z"})
		default:
			return tooMany(cur)
		}
	case 'O':
		if count != 1 && count != 4 {
			return &dtPatternError{"Pattern letter count must be 1 or 4: O"}
		}
		b.elem(dtOffsetElem{pattern: "+HH:MM:ss", noOffset: "GMT"})
	case 'X', 'x':
		if count > 5 {
			return tooMany(cur)
		}
		patterns := []string{"+HH", "+HHmm", "+HH:mm", "+HHMM", "+HH:MM", "+HHMMss", "+HH:MM:ss", "+HHMMSS", "+HH:MM:SS"}
		// DateTimeFormatterBuilder: PATTERNS[count + (count == 1 ? 0 : 1)]
		idx := count
		if count != 1 {
			idx = count + 1
		}
		p := patterns[idx]
		zero := "Z"
		if cur == 'x' {
			switch {
			case count == 1:
				zero = "+00"
			case count%2 == 0:
				zero = "+0000"
			default:
				zero = "+00:00"
			}
		}
		b.elem(dtOffsetElem{pattern: p, noOffset: zero})
	case 'W':
		if count > 1 {
			return tooMany(cur)
		}
		b.num(dtWeekOfWeekBasedYear, 1, 1, signNotNegative)
	case 'w':
		if count > 2 {
			return tooMany(cur)
		}
		b.num(dtWeekOfWeekBasedYear, count, 2, signNotNegative)
	case 'Y':
		if count == 2 {
			b.noteField(dtWeekBasedYear)
			b.appendValue(&dtNumber{field: dtWeekBasedYear, minW: 2, maxW: 2, sign: signNotNegative, reducedBase: 2000})
		} else {
			sign := signNormal
			if count >= 4 {
				sign = signExceedsPad
			}
			b.num(dtWeekBasedYear, count, 19, sign)
		}
	case 'B':
		b.elem(dtText{field: dtAmPm, names: []string{"in the morning", "in the afternoon"}})
	case 'p':
		// padding: parsed as if absent
	default:
		return &dtPatternError{fmt.Sprintf("Unknown pattern letter: %c", cur)}
	}
	return nil
}
