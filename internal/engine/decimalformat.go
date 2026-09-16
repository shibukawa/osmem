package engine

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// decimalFormat is java.text.DecimalFormat as OpenSearch applies it to
// numeric values ("format": "#,##0.00" on docvalue_fields and aggregations):
// literal prefixes and suffixes, grouping, minimum/maximum integer and
// fraction digits, percent and per-mille multipliers, a negative subpattern
// and scientific notation, with the default HALF_EVEN rounding decided on
// the exact binary value (0.15 formats as "0.1" with "0.0").
type decimalFormat struct {
	posPrefix, posSuffix string
	negPrefix, negSuffix string
	minInt, maxInt       int
	minFrac, maxFrac     int
	grouping             int
	multiplier           int
	scientific           bool
	minExp               int
}

const maxJavaIntegerDigits = math.MaxInt32

// parseDecimalFormat compiles a DecimalFormat pattern; ok is false for
// patterns DecimalFormat rejects.
func parseDecimalFormat(pattern string) (*decimalFormat, bool) {
	df := &decimalFormat{multiplier: 1}
	sub := splitDecimalSubpatterns(pattern)
	if len(sub) == 0 || len(sub) > 2 {
		return nil, false
	}
	prefix, number, suffix, mult, ok := splitDecimalSubpattern(sub[0])
	if !ok || number == "" {
		return nil, false
	}
	df.posPrefix, df.posSuffix, df.multiplier = prefix, suffix, mult
	if !df.applyNumber(number) {
		return nil, false
	}
	if len(sub) == 2 {
		np, _, ns, _, ok := splitDecimalSubpattern(sub[1])
		if !ok {
			return nil, false
		}
		df.negPrefix, df.negSuffix = np, ns
	} else {
		df.negPrefix, df.negSuffix = "-"+df.posPrefix, df.posSuffix
	}
	return df, true
}

// splitDecimalSubpatterns splits at the unquoted ';'.
func splitDecimalSubpatterns(p string) []string {
	var out []string
	quoted := false
	start := 0
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '\'':
			quoted = !quoted
		case ';':
			if !quoted {
				out = append(out, p[start:i])
				start = i + 1
			}
		}
	}
	return append(out, p[start:])
}

// splitDecimalSubpattern separates the literal prefix and suffix (with
// quotes resolved) from the number part.
func splitDecimalSubpattern(p string) (prefix, number, suffix string, multiplier int, ok bool) {
	multiplier = 1
	var pre, num, suf strings.Builder
	phase := 0
	runes := []rune(p)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\'' {
			if i+1 < len(runes) && runes[i+1] == '\'' {
				i++
				if phase == 0 {
					pre.WriteRune('\'')
				} else {
					suf.WriteRune('\'')
				}
				continue
			}
			j := i + 1
			for j < len(runes) && runes[j] != '\'' {
				j++
			}
			if j >= len(runes) {
				return "", "", "", 0, false
			}
			lit := string(runes[i+1 : j])
			if phase == 1 {
				phase = 2
			}
			if phase == 0 {
				pre.WriteString(lit)
			} else {
				suf.WriteString(lit)
			}
			i = j
			continue
		}
		isNumber := r == '#' || r == '0' || r == ',' || r == '.' || (r == 'E' && phase == 1)
		if phase == 1 && r == 'E' {
			num.WriteRune(r)
			for i+1 < len(runes) && runes[i+1] == '0' {
				i++
				num.WriteRune('0')
			}
			phase = 2
			continue
		}
		switch {
		case isNumber && phase < 2:
			phase = 1
			num.WriteRune(r)
		case phase == 1:
			phase = 2
			fallthrough
		default:
			switch r {
			case '%':
				multiplier = 100
			case '‰':
				multiplier = 1000
			}
			if phase == 0 {
				pre.WriteRune(r)
			} else {
				if isNumber {
					return "", "", "", 0, false
				}
				suf.WriteRune(r)
			}
		}
	}
	return pre.String(), num.String(), suf.String(), multiplier, true
}

// applyNumber follows DecimalFormat.applyPattern for the number part.
func (df *decimalFormat) applyNumber(num string) bool {
	digitLeft, zeroDigits, digitRight := 0, 0, 0
	decimalPos, grouping := -1, -1
	for i := 0; i < len(num); i++ {
		switch num[i] {
		case '#':
			if zeroDigits > 0 {
				digitRight++
			} else {
				digitLeft++
			}
			if grouping >= 0 && decimalPos < 0 {
				grouping++
			}
		case '0':
			if digitRight > 0 {
				return false
			}
			zeroDigits++
			if grouping >= 0 && decimalPos < 0 {
				grouping++
			}
		case ',':
			if decimalPos >= 0 {
				return false
			}
			grouping = 0
		case '.':
			if decimalPos >= 0 {
				return false
			}
			decimalPos = digitLeft + zeroDigits + digitRight
		case 'E':
			df.scientific = true
			df.minExp = len(num) - i - 1
			i = len(num)
		}
	}
	if zeroDigits == 0 && digitLeft > 0 && decimalPos >= 0 {
		n := decimalPos
		if n == 0 {
			n++
		}
		digitRight = digitLeft - n
		digitLeft = n - 1
		zeroDigits = 1
	}
	if (decimalPos < 0 && digitRight > 0) || (decimalPos >= 0 && (decimalPos < digitLeft || decimalPos > digitLeft+zeroDigits)) || grouping == 0 {
		return false
	}
	total := digitLeft + zeroDigits + digitRight
	effectiveDecimal := total
	if decimalPos >= 0 {
		effectiveDecimal = decimalPos
	}
	df.minInt = effectiveDecimal - digitLeft
	df.maxInt = maxJavaIntegerDigits
	if df.scientific {
		df.maxInt = digitLeft + df.minInt
	}
	if decimalPos >= 0 {
		df.maxFrac = total - decimalPos
		df.minFrac = digitLeft + zeroDigits - decimalPos
	}
	if grouping > 0 {
		df.grouping = grouping
	}
	return true
}

// digitList is java.text.DigitList: value = 0.digits × 10^decimalAt.
type digitList struct {
	digits    []byte
	decimalAt int
	value     float64
	shortest  string // the 'e' rendering of value
	// roundedUp: the shortest digits are above the exact value; exact: they
	// are the exact value (see tie: only needed to break a HALF_EVEN tie)
	roundedUp, exact bool
	tieKnown         bool
}

func newDigitList(v float64) *digitList {
	dl := &digitList{value: v}
	if v == 0 {
		dl.exact, dl.tieKnown = true, true
		return dl
	}
	s := strconv.FormatFloat(v, 'e', -1, 64)
	e := strings.IndexByte(s, 'e')
	exp, _ := strconv.Atoi(s[e+1:])
	digits := strings.TrimRight(strings.Replace(s[:e], ".", "", 1), "0")
	dl.digits = []byte(digits)
	dl.decimalAt = exp + 1
	dl.shortest = s
	return dl
}

// tie compares the shortest digits with the exact binary value; the
// comparison needs big precision, so it is only done for a tie.
func (dl *digitList) tie() (roundedUp, exact bool) {
	if !dl.tieKnown {
		shortest, _, _ := big.ParseFloat(dl.shortest, 10, 2000, big.ToNearestEven)
		switch shortest.Cmp(new(big.Float).SetPrec(2000).SetFloat64(dl.value)) {
		case 1:
			dl.roundedUp = true
		case 0:
			dl.exact = true
		}
		dl.tieKnown = true
	}
	return dl.roundedUp, dl.exact
}

func (dl *digitList) isZero() bool {
	for _, d := range dl.digits {
		if d != '0' {
			return false
		}
	}
	return true
}

// shouldRoundUp is DigitList.shouldRoundUp for HALF_EVEN.
func (dl *digitList) shouldRoundUp(max int) bool {
	count := len(dl.digits)
	if max >= count {
		return false
	}
	switch {
	case dl.digits[max] > '5':
		return true
	case dl.digits[max] == '5':
		if max == count-1 {
			roundedUp, exact := dl.tie()
			if roundedUp {
				return false
			}
			if !exact {
				return true
			}
			return max > 0 && (dl.digits[max-1]-'0')%2 != 0
		}
		for i := max + 1; i < count; i++ {
			if dl.digits[i] != '0' {
				return true
			}
		}
	}
	return false
}

func (dl *digitList) round(max int) {
	if max < 0 || max >= len(dl.digits) {
		return
	}
	if dl.shouldRoundUp(max) {
		for {
			max--
			if max < 0 {
				dl.digits[0] = '1'
				dl.decimalAt++
				max = 0
				break
			}
			dl.digits[max]++
			if dl.digits[max] <= '9' {
				break
			}
		}
		max++
	}
	dl.digits = dl.digits[:max]
	for len(dl.digits) > 1 && dl.digits[len(dl.digits)-1] == '0' {
		dl.digits = dl.digits[:len(dl.digits)-1]
	}
}

// setFixed rounds to max fraction digits (DigitList.set with fixedPoint).
func (dl *digitList) setFixed(maxFrac int) {
	if len(dl.digits) == 0 {
		return
	}
	switch {
	case -dl.decimalAt > maxFrac:
		dl.digits = nil
	case -dl.decimalAt == maxFrac:
		if dl.shouldRoundUp(0) {
			dl.digits = []byte{'1'}
			dl.decimalAt++
		} else {
			dl.digits = nil
		}
	default:
		dl.round(maxFrac + dl.decimalAt)
	}
}

func (df *decimalFormat) format(v float64) string {
	if math.IsNaN(v) {
		return "NaN"
	}
	negative := v < 0 || (v == 0 && math.Signbit(v))
	v = math.Abs(v)
	if df.multiplier != 1 {
		v *= float64(df.multiplier)
	}
	var b strings.Builder
	if negative {
		b.WriteString(df.negPrefix)
	} else {
		b.WriteString(df.posPrefix)
	}
	if math.IsInf(v, 0) {
		b.WriteString("∞")
	} else if df.scientific {
		df.formatScientific(&b, v)
	} else {
		df.formatFixed(&b, v)
	}
	if negative {
		b.WriteString(df.negSuffix)
	} else {
		b.WriteString(df.posSuffix)
	}
	return b.String()
}

func (df *decimalFormat) formatFixed(b *strings.Builder, v float64) {
	dl := newDigitList(v)
	dl.setFixed(df.maxFrac)
	count := df.minInt
	digitIndex := 0
	if dl.decimalAt > 0 && count < dl.decimalAt {
		count = dl.decimalAt
	}
	if count > df.maxInt {
		count = df.maxInt
		digitIndex = dl.decimalAt - count
	}
	start := b.Len()
	for i := count - 1; i >= 0; i-- {
		if i < dl.decimalAt && digitIndex < len(dl.digits) {
			b.WriteByte(dl.digits[digitIndex])
			digitIndex++
		} else {
			b.WriteByte('0')
		}
		if df.grouping > 0 && i > 0 && i%df.grouping == 0 {
			b.WriteByte(',')
		}
	}
	fractionPresent := df.minFrac > 0 || digitIndex < len(dl.digits)
	if !fractionPresent && b.Len() == start {
		b.WriteByte('0')
	}
	if fractionPresent {
		b.WriteByte('.')
	}
	for i := 0; i < df.maxFrac; i++ {
		if i >= df.minFrac && digitIndex >= len(dl.digits) {
			break
		}
		if -1-i > dl.decimalAt-1 {
			b.WriteByte('0')
			continue
		}
		if digitIndex < len(dl.digits) {
			b.WriteByte(dl.digits[digitIndex])
			digitIndex++
		} else {
			b.WriteByte('0')
		}
	}
}

func (df *decimalFormat) formatScientific(b *strings.Builder, v float64) {
	dl := newDigitList(v)
	dl.round(df.maxInt + df.maxFrac)
	exponent := dl.decimalAt
	repeat := df.maxInt
	minIntDigits := df.minInt
	if repeat > 1 && repeat > df.minInt {
		if exponent >= 1 {
			exponent = ((exponent - 1) / repeat) * repeat
		} else {
			exponent = ((exponent - repeat) / repeat) * repeat
		}
		minIntDigits = 1
	} else {
		exponent -= minIntDigits
	}
	minDigits := df.minInt + df.minFrac
	integerDigits := dl.decimalAt - exponent
	if dl.isZero() {
		integerDigits = minIntDigits
	}
	if minDigits < integerDigits {
		minDigits = integerDigits
	}
	total := len(dl.digits)
	if minDigits > total {
		total = minDigits
	}
	for i := 0; i < total; i++ {
		if i == integerDigits {
			b.WriteByte('.')
		}
		if i < len(dl.digits) {
			b.WriteByte(dl.digits[i])
		} else {
			b.WriteByte('0')
		}
	}
	b.WriteByte('E')
	if dl.isZero() {
		exponent = 0
	}
	if exponent < 0 {
		exponent = -exponent
		b.WriteByte('-')
	}
	es := strconv.Itoa(exponent)
	for i := len(es); i < df.minExp; i++ {
		b.WriteByte('0')
	}
	b.WriteString(es)
}

// formatDecimal applies a DecimalFormat pattern; ok is false when the
// pattern is invalid. Callers formatting many values compile the pattern
// once with parseDecimalFormat instead.
func formatDecimal(pattern string, v float64) (string, bool) {
	df, ok := parseDecimalFormat(pattern)
	if !ok {
		return "", false
	}
	return df.format(v), true
}
