package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Lucene RegExp syntax (include/exclude of terms aggregations), parsed with
// Lucene's grammar and messages and translated to a Go regular expression.
// Complement is not part of the default syntax any more (Lucene 10).

type aggLuceneRegexp struct {
	res []*regexp.Regexp // the term must match all (top-level intersections)
}

func (lr *aggLuceneRegexp) match(s string) bool {
	for _, re := range lr.res {
		if !re.MatchString(s) {
			return false
		}
	}
	return true
}

// compileAggLuceneRegexp compiles a Lucene regular expression; errors carry
// Lucene's messages.
func compileAggLuceneRegexp(pattern string) (*aggLuceneRegexp, error) {
	p := &lucenePattern{s: []rune(pattern)}
	var parts []string
	if len(p.s) == 0 {
		parts = []string{""}
	} else {
		var err error
		if parts, err = p.parseTopLevel(); err != nil {
			return nil, err
		}
		if p.pos < len(p.s) {
			return nil, errIllegalArgument("end-of-string expected at position %d", p.charPos())
		}
	}
	lr := &aggLuceneRegexp{}
	for _, part := range parts {
		re, err := regexp.Compile(`^(?:` + part + `)$`)
		if err != nil {
			return nil, errUnsupported("regular expression [" + pattern + "]")
		}
		lr.res = append(lr.res, re)
	}
	return lr, nil
}

type lucenePattern struct {
	s   []rune
	pos int
}

// charPos is the position in Java chars (UTF-16 units).
func (p *lucenePattern) charPos() int {
	n := 0
	for _, r := range p.s[:p.pos] {
		n += aggRuneUTF16Len(r)
	}
	return n
}

func aggRuneUTF16Len(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

func (p *lucenePattern) more() bool { return p.pos < len(p.s) }

func (p *lucenePattern) peek(set string) bool {
	return p.more() && strings.ContainsRune(set, p.s[p.pos])
}

func (p *lucenePattern) match(c rune) bool {
	if p.more() && p.s[p.pos] == c {
		p.pos++
		return true
	}
	return false
}

func (p *lucenePattern) next() (rune, error) {
	if !p.more() {
		return 0, errIllegalArgument("unexpected end-of-string")
	}
	r := p.s[p.pos]
	p.pos++
	return r, nil
}

// parseTopLevel parses a union; a top-level intersection yields several
// expressions that must all match.
func (p *lucenePattern) parseTopLevel() ([]string, error) {
	first, err := p.parseInter(true)
	if err != nil {
		return nil, err
	}
	if !p.peek("|") {
		return first, nil
	}
	alts := []string{joinInter(first)}
	for p.match('|') {
		e, err := p.parseInter(false)
		if err != nil {
			return nil, err
		}
		alts = append(alts, joinInter(e))
	}
	return []string{strings.Join(alts, "|")}, nil
}

func joinInter(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	return "\x00intersection"
}

func (p *lucenePattern) parseUnion() (string, error) {
	parts, err := p.parseInter(false)
	if err != nil {
		return "", err
	}
	alts := []string{parts[0]}
	for p.match('|') {
		e, err := p.parseInter(false)
		if err != nil {
			return "", err
		}
		alts = append(alts, e[0])
	}
	if len(alts) == 1 {
		return alts[0], nil
	}
	return "(?:" + strings.Join(alts, "|") + ")", nil
}

func (p *lucenePattern) parseInter(top bool) ([]string, error) {
	first, err := p.parseConcat()
	if err != nil {
		return nil, err
	}
	out := []string{first}
	for p.match('&') {
		e, err := p.parseConcat()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if len(out) > 1 && !top {
		return nil, errUnsupported("intersections inside regular expressions")
	}
	return out, nil
}

func (p *lucenePattern) parseConcat() (string, error) {
	e, err := p.parseRepeat()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(e)
	for p.more() && !p.peek(")|") && !p.peek("&") {
		e, err := p.parseRepeat()
		if err != nil {
			return "", err
		}
		b.WriteString(e)
	}
	return b.String(), nil
}

func (p *lucenePattern) parseRepeat() (string, error) {
	e, err := p.parseCharClassExp()
	if err != nil {
		return "", err
	}
	for p.peek("?*+{") {
		switch {
		case p.match('?'):
			e = "(?:" + e + ")?"
		case p.match('*'):
			e = "(?:" + e + ")*"
		case p.match('+'):
			e = "(?:" + e + ")+"
		case p.match('{'):
			start := p.pos
			for p.peek("0123456789") {
				p.pos++
			}
			if start == p.pos {
				return "", errIllegalArgument("integer expected at position %d", p.charPos())
			}
			n, _ := strconv.Atoi(string(p.s[start:p.pos]))
			m := -1
			if p.match(',') {
				start = p.pos
				for p.peek("0123456789") {
					p.pos++
				}
				if start != p.pos {
					m, _ = strconv.Atoi(string(p.s[start:p.pos]))
				}
			} else {
				m = n
			}
			if !p.match('}') {
				return "", errIllegalArgument("expected '}' at position %d", p.charPos())
			}
			if m == -1 {
				e = fmt.Sprintf("(?:%s){%d,}", e, n)
			} else {
				if m < n {
					n, m = m, n
				}
				e = fmt.Sprintf("(?:%s){%d,%d}", e, n, m)
			}
		}
	}
	return e, nil
}

func (p *lucenePattern) parseCharClassExp() (string, error) {
	if !p.match('[') {
		return p.parseSimple()
	}
	negate := p.match('^')
	first, err := p.parseCharClass()
	if err != nil {
		return "", err
	}
	items := []string{first}
	for p.more() && !p.peek("]") {
		c, err := p.parseCharClass()
		if err != nil {
			return "", err
		}
		items = append(items, c)
	}
	if !p.match(']') {
		return "", errIllegalArgument("expected ']' at position %d", p.charPos())
	}
	if negate {
		return "[^" + strings.Join(items, "") + "]", nil
	}
	return "[" + strings.Join(items, "") + "]", nil
}

// predefined classes, valid inside and outside brackets
var lucenePredefined = map[rune]string{'d': `0-9`, 'D': `^0-9`, 's': " \t\n\r", 'S': "^ \t\n\r", 'w': `a-zA-Z_0-9`, 'W': `^a-zA-Z_0-9`}

func (p *lucenePattern) matchPredefined() (rune, bool, error) {
	if p.pos+1 < len(p.s)+1 && p.more() && p.s[p.pos] == '\\' {
		p.pos++
		if p.peek("dDwWsS") {
			c := p.s[p.pos]
			p.pos++
			return c, true, nil
		}
		if p.peek("\\") {
			p.pos++
			return '\\', false, nil
		}
		if p.peek("abcefghijklmnopqrtuvxyzABCEFGHIJKLMNOPQRTUVXYZ") {
			c := p.s[p.pos]
			p.pos++
			return 0, false, errIllegalArgument("invalid character class \\%c", c)
		}
		p.pos--
	}
	return 0, false, nil
}

func classRange(set string) string {
	if strings.HasPrefix(set, "^") {
		return "[^" + regexpClassEscape(set[1:]) + "]"
	}
	return "[" + regexpClassEscape(set) + "]"
}

func regexpClassEscape(s string) string {
	return strings.NewReplacer("\t", `\t`, "\n", `\n`, "\r", `\r`).Replace(s)
}

func (p *lucenePattern) parseCharClass() (string, error) {
	start := p.pos
	if c, ok, err := p.matchPredefined(); err != nil {
		return "", err
	} else if ok {
		set := lucenePredefined[c]
		if strings.HasPrefix(set, "^") {
			return "", errUnsupported("negated character classes inside brackets")
		}
		return regexpClassEscape(set), nil
	} else if p.pos != start {
		// an escaped backslash
		return quoteClassRune('\\'), nil
	}
	c, err := p.parseCharExp()
	if err != nil {
		return "", err
	}
	if p.match('-') {
		to, err := p.parseCharExp()
		if err != nil {
			return "", err
		}
		if to < c {
			return "", errIllegalArgument("invalid range: from (%d) cannot be > to (%d)", c, to)
		}
		return quoteClassRune(c) + "-" + quoteClassRune(to), nil
	}
	return quoteClassRune(c), nil
}

func quoteClassRune(r rune) string {
	switch r {
	case '\\', ']', '[', '^', '-':
		return `\` + string(r)
	}
	if r < 0x20 || r == utf8.RuneError {
		return fmt.Sprintf(`\x{%x}`, r)
	}
	return string(r)
}

func (p *lucenePattern) parseCharExp() (rune, error) {
	p.match('\\')
	return p.next()
}

func (p *lucenePattern) parseSimple() (string, error) {
	switch {
	case p.match('.'):
		return `(?s:.)`, nil
	case p.match('#'):
		return `[^\x{0}-\x{10FFFF}]`, nil
	case p.match('@'):
		return `(?s:.*)`, nil
	case p.match('"'):
		start := p.pos
		for p.more() && !p.peek(`"`) {
			p.pos++
		}
		if !p.match('"') {
			return "", errIllegalArgument("expected '\"' at position %d", p.charPos())
		}
		return regexp.QuoteMeta(string(p.s[start : p.pos-1])), nil
	case p.match('('):
		if p.match(')') {
			return "", nil
		}
		e, err := p.parseUnion()
		if err != nil {
			return "", err
		}
		if !p.match(')') {
			return "", errIllegalArgument("expected ')' at position %d", p.charPos())
		}
		return "(?:" + e + ")", nil
	case p.match('<'):
		start := p.pos
		for p.more() && !p.peek(">") {
			p.pos++
		}
		if !p.match('>') {
			return "", errIllegalArgument("expected '>' at position %d", p.charPos())
		}
		s := string(p.s[start : p.pos-1])
		i := strings.Index(s, "-")
		if i == -1 {
			return "", errIllegalArgument("'%s' not found", s)
		}
		syntax := errIllegalArgument("interval syntax error at position %d", p.charPos()-1)
		if i == 0 || i == len(s)-1 || i != strings.LastIndex(s, "-") {
			return "", syntax
		}
		lo, err1 := strconv.Atoi(s[:i])
		hi, err2 := strconv.Atoi(s[i+1:])
		if err1 != nil || err2 != nil {
			return "", syntax
		}
		digits := 0
		if len(s[:i]) == len(s[i+1:]) {
			digits = i
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		return decimalIntervalRegexp(lo, hi, digits), nil
	}
	if c, ok, err := p.matchPredefined(); err != nil {
		return "", err
	} else if ok {
		return classRange(lucenePredefined[c]), nil
	}
	c, err := p.parseCharExp()
	if err != nil {
		return "", err
	}
	return regexp.QuoteMeta(string(c)), nil
}

// decimalIntervalRegexp matches the decimal numbers in [lo, hi]: with digits
// > 0 zero-padded to that width, otherwise with any leading zeros.
func decimalIntervalRegexp(lo, hi, digits int) string {
	alts := splitNumericRange(lo, hi)
	body := "(?:" + strings.Join(alts, "|") + ")"
	if digits > 0 {
		// width-constrained: generate padded alternatives
		var padded []string
		for n := lo; n <= hi && len(padded) < 5000; n++ {
			padded = append(padded, fmt.Sprintf("%0*d", digits, n))
		}
		if hi-lo < 5000 {
			return "(?:" + strings.Join(padded, "|") + ")"
		}
	}
	return "0*" + body
}

// splitNumericRange builds regexps for the numbers of a range without
// leading zeros.
func splitNumericRange(lo, hi int) []string {
	if lo > hi {
		return nil
	}
	ls, hs := strconv.Itoa(lo), strconv.Itoa(hi)
	if len(ls) < len(hs) {
		pow := 1
		for i := 0; i < len(ls); i++ {
			pow *= 10
		}
		return append(splitNumericRange(lo, pow-1), splitNumericRange(pow, hi)...)
	}
	if lo == hi {
		return []string{ls}
	}
	// same length: common prefix, then ranges per digit
	i := 0
	for i < len(ls) && ls[i] == hs[i] {
		i++
	}
	prefix := ls[:i]
	if i == len(ls)-1 {
		return []string{fmt.Sprintf("%s[%c-%c]", prefix, ls[i], hs[i])}
	}
	rest := len(ls) - i - 1
	allLow := strings.Repeat("0", rest)
	allHigh := strings.Repeat("9", rest)
	var out []string
	loTail, hiTail := ls[i+1:], hs[i+1:]
	start, end := ls[i], hs[i]
	if loTail != allLow {
		out = append(out, prefixAll(prefix+string(start), splitFixed(loTail, allHigh))...)
		start++
	}
	if hiTail != allHigh {
		if start <= end-1 {
			out = append(out, fmt.Sprintf("%s[%c-%c]%s", prefix, start, end-1, strings.Repeat("[0-9]", rest)))
		}
		out = append(out, prefixAll(prefix+string(end), splitFixed(allLow, hiTail))...)
	} else if start <= end {
		out = append(out, fmt.Sprintf("%s[%c-%c]%s", prefix, start, end, strings.Repeat("[0-9]", rest)))
	}
	return out
}

// splitFixed covers the fixed-width digit strings between lo and hi.
func splitFixed(lo, hi string) []string {
	if len(lo) == 0 {
		return []string{""}
	}
	if lo == hi {
		return []string{lo}
	}
	if lo[0] == hi[0] {
		return prefixAll(lo[:1], splitFixed(lo[1:], hi[1:]))
	}
	rest := len(lo) - 1
	var out []string
	start, end := lo[0], hi[0]
	if lo[1:] != strings.Repeat("0", rest) {
		out = append(out, prefixAll(lo[:1], splitFixed(lo[1:], strings.Repeat("9", rest)))...)
		start++
	}
	if hi[1:] != strings.Repeat("9", rest) {
		if start <= end-1 {
			out = append(out, fmt.Sprintf("[%c-%c]%s", start, end-1, strings.Repeat("[0-9]", rest)))
		}
		out = append(out, prefixAll(hi[:1], splitFixed(strings.Repeat("0", rest), hi[1:]))...)
	} else if start <= end {
		out = append(out, fmt.Sprintf("[%c-%c]%s", start, end, strings.Repeat("[0-9]", rest)))
	}
	return out
}

func prefixAll(prefix string, list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = prefix + s
	}
	return out
}
