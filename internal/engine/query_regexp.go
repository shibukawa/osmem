package engine

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Lucene's RegExp syntax (regexp queries and /regex/ terms of query_string),
// wildcard patterns and fuzzy edit distances, matched against whole terms.

// Lucene RegExp syntax flags as OpenSearch 3.x configures them: the
// complement operator is only enabled when COMPLEMENT is requested
// explicitly (Lucene 10's DEPRECATED_COMPLEMENT is not part of ALL).
const (
	reFlagIntersection = 0x0001
	reFlagEmpty        = 0x0004
	reFlagAnyString    = 0x0008
	reFlagAutomaton    = 0x0010
	reFlagInterval     = 0x0020
	reFlagAll          = 0xff
	reFlagComplement   = 0x10000
)

// parseRegexpFlags resolves the flags parameter of a regexp query.
func parseRegexpFlags(flags string) (int, *Error) {
	if flags == "" {
		return reFlagAll, nil
	}
	magic := 0
	for _, s := range strings.Split(flags, "|") {
		if s == "" {
			continue
		}
		switch strings.ToUpper(s) {
		case "NONE":
			continue
		case "ALL":
			return reFlagAll, nil
		case "INTERSECTION":
			magic |= reFlagIntersection
		case "COMPLEMENT":
			magic |= reFlagComplement
		case "EMPTY":
			magic |= reFlagEmpty
		case "ANYSTRING":
			magic |= reFlagAnyString
		case "INTERVAL":
			magic |= reFlagInterval
		default:
			return 0, pIllegalArgument("Unknown regexp flag [%s]", s)
		}
	}
	return magic, nil
}

type reKind int

const (
	reSet reKind = iota // one code point from a set of ranges
	reAnyChar
	reNothing   // the empty language (#)
	reEmptyStr  // the empty string
	reAnyString // any string (@)
	reLiteral
	reInterval
	reConcat
	reUnion
	reInter
	reCompl
	reRepeat
)

type reNode struct {
	kind     reKind
	ranges   [][2]rune // reSet
	negated  bool      // reSet: every code point but the ranges
	lit      []rune    // reLiteral
	min, max int       // reRepeat (max < 0: unbounded); reInterval bounds
	digits   int       // reInterval
	a, b     *reNode
	id       int
}

// luceneRegexp is a parsed Lucene regular expression.
type luceneRegexp struct {
	root  *reNode
	nodes int
}

type reParser struct {
	s        []rune
	pos      int
	flags    int
	caseFold bool
	nodes    int
	depth    int // open groups, bounded by maxRegexpDepth
}

// maxRegexpDepth bounds the nesting of groups in a pattern: the parser is
// recursive per group, so an unbounded depth from a request could exhaust
// the stack, which Go cannot recover from.
const maxRegexpDepth = 1000

// compileLuceneRegexp parses a Lucene regular expression. caseInsensitive
// matches ASCII letters in either case. The error is the message of
// Lucene's IllegalArgumentException.
func compileLuceneRegexp(pattern string, flags int, caseInsensitive bool) (*luceneRegexp, string) {
	p := &reParser{s: []rune(pattern), flags: flags, caseFold: caseInsensitive}
	var root *reNode
	var msg string
	func() {
		defer func() {
			if r := recover(); r != nil {
				if e, ok := r.(reError); ok {
					msg = string(e)
					return
				}
				panic(r)
			}
		}()
		if len(p.s) == 0 {
			root = p.node(&reNode{kind: reEmptyStr})
			return
		}
		root = p.parseUnion()
		if p.pos < len(p.s) {
			panic(reError("end-of-string expected at position " + strconv.Itoa(p.pos)))
		}
	}()
	if msg != "" {
		return nil, msg
	}
	return &luceneRegexp{root: root, nodes: p.nodes}, ""
}

type reError string

func (p *reParser) node(n *reNode) *reNode {
	n.id = p.nodes
	p.nodes++
	return n
}

func (p *reParser) check(flag int) bool { return p.flags&flag != 0 }

func (p *reParser) more() bool { return p.pos < len(p.s) }

func (p *reParser) peek(chars string) bool {
	return p.more() && strings.ContainsRune(chars, p.s[p.pos])
}

func (p *reParser) match(c rune) bool {
	if p.more() && p.s[p.pos] == c {
		p.pos++
		return true
	}
	return false
}

func (p *reParser) next() rune {
	if !p.more() {
		panic(reError("unexpected end-of-string"))
	}
	c := p.s[p.pos]
	p.pos++
	return c
}

// parseUnion, parseInter and parseConcat are iterative over their operands
// (Lucene's RegExp recurses once per operand, which for a long pattern from
// a request would recurse once per character); the trees they build are
// right-nested exactly like the recursive form.
func (p *reParser) parseUnion() *reNode {
	parts := []*reNode{p.parseInter()}
	for p.match('|') {
		parts = append(parts, p.parseInter())
	}
	return p.foldRight(reUnion, parts)
}

func (p *reParser) parseInter() *reNode {
	parts := []*reNode{p.parseConcat()}
	for p.check(reFlagIntersection) && p.match('&') {
		parts = append(parts, p.parseConcat())
	}
	return p.foldRight(reInter, parts)
}

func (p *reParser) parseConcat() *reNode {
	parts := []*reNode{p.parseRepeat()}
	for p.more() && !p.peek(")|") && (!p.check(reFlagIntersection) || !p.peek("&")) {
		e := p.parseRepeat()
		// adjacent literals fold into one node: a long literal pattern is
		// then one node instead of one per character
		if last := parts[len(parts)-1]; last.kind == reLiteral && e.kind == reLiteral {
			parts[len(parts)-1] = p.node(&reNode{kind: reLiteral, lit: append(append([]rune(nil), last.lit...), e.lit...)})
			continue
		}
		parts = append(parts, e)
	}
	return p.foldRight(reConcat, parts)
}

func (p *reParser) foldRight(kind reKind, parts []*reNode) *reNode {
	e := parts[len(parts)-1]
	for i := len(parts) - 2; i >= 0; i-- {
		e = p.node(&reNode{kind: kind, a: parts[i], b: e})
	}
	return e
}

func (p *reParser) parseRepeat() *reNode {
	e := p.parseCompl()
	for p.peek("?*+{") {
		switch {
		case p.match('?'):
			e = p.node(&reNode{kind: reRepeat, a: e, min: 0, max: 1})
		case p.match('*'):
			e = p.node(&reNode{kind: reRepeat, a: e, min: 0, max: -1})
		case p.match('+'):
			e = p.node(&reNode{kind: reRepeat, a: e, min: 1, max: -1})
		case p.match('{'):
			start := p.pos
			for p.peek("0123456789") {
				p.next()
			}
			if start == p.pos {
				panic(reError("integer expected at position " + strconv.Itoa(p.pos)))
			}
			n, err := strconv.Atoi(string(p.s[start:p.pos]))
			if err != nil {
				panic(reError("invalid repetition: " + string(p.s[start:p.pos]) + " is too large"))
			}
			m := -1
			if p.match(',') {
				start = p.pos
				for p.peek("0123456789") {
					p.next()
				}
				if start != p.pos {
					if m, err = strconv.Atoi(string(p.s[start:p.pos])); err != nil {
						panic(reError("invalid repetition: " + string(p.s[start:p.pos]) + " is too large"))
					}
				}
			} else {
				m = n
			}
			if !p.match('}') {
				panic(reError("expected '}' at position " + strconv.Itoa(p.pos)))
			}
			if m != -1 && n > m {
				panic(reError("invalid repetition: min (" + strconv.Itoa(n) + ") cannot be > max (" + strconv.Itoa(m) + ")"))
			}
			e = p.node(&reNode{kind: reRepeat, a: e, min: n, max: m})
		}
	}
	return e
}

func (p *reParser) parseCompl() *reNode {
	if p.check(reFlagComplement) && p.match('~') {
		return p.node(&reNode{kind: reCompl, a: p.parseCompl()})
	}
	return p.parseCharClassExp()
}

func (p *reParser) parseCharClassExp() *reNode {
	if p.match('[') {
		negate := p.match('^')
		set := p.node(&reNode{kind: reSet})
		p.parseCharClasses(set)
		if negate {
			set.negated = !set.negated
		}
		if !p.match(']') {
			panic(reError("expected ']' at position " + strconv.Itoa(p.pos)))
		}
		return set
	}
	return p.parseSimple()
}

func (p *reParser) parseCharClasses(set *reNode) {
	p.parseCharClass(set)
	for p.more() && !p.peek("]") {
		p.parseCharClass(set)
	}
}

func (p *reParser) parseCharClass(set *reNode) {
	if ranges, negated, ok := p.predefinedClass(); ok {
		if negated {
			// [^\d] style classes are not composable into a range list;
			// approximate by the complement of the class within the set
			set.ranges = append(set.ranges, complementRanges(ranges)...)
		} else {
			set.ranges = append(set.ranges, ranges...)
		}
		return
	}
	c := p.parseCharExp()
	if p.match('-') {
		to := p.parseCharExp()
		if c > to {
			panic(reError("invalid range: from (" + strconv.Itoa(int(c)) + ") cannot be > to (" + strconv.Itoa(int(to)) + ")"))
		}
		set.ranges = append(set.ranges, [2]rune{c, to})
		return
	}
	set.ranges = append(set.ranges, p.charRanges(c)...)
}

// charRanges is the set of a single character, both ASCII cases when case
// insensitive.
func (p *reParser) charRanges(c rune) [][2]rune {
	out := [][2]rune{{c, c}}
	if p.caseFold && c < 128 {
		if c >= 'a' && c <= 'z' {
			out = append(out, [2]rune{c - 32, c - 32})
		} else if c >= 'A' && c <= 'Z' {
			out = append(out, [2]rune{c + 32, c + 32})
		}
	}
	return out
}

var (
	reDigitRanges = [][2]rune{{'0', '9'}}
	reSpaceRanges = [][2]rune{{'\t', '\r'}, {' ', ' '}}
	reWordRanges  = [][2]rune{{'0', '9'}, {'A', 'Z'}, {'_', '_'}, {'a', 'z'}}
)

func complementRanges(ranges [][2]rune) [][2]rune {
	var out [][2]rune
	prev := rune(0)
	for _, r := range sortRanges(ranges) {
		if r[0] > prev {
			out = append(out, [2]rune{prev, r[0] - 1})
		}
		if r[1]+1 > prev {
			prev = r[1] + 1
		}
	}
	if prev <= utf8.MaxRune {
		out = append(out, [2]rune{prev, utf8.MaxRune})
	}
	return out
}

func sortRanges(ranges [][2]rune) [][2]rune {
	out := append([][2]rune(nil), ranges...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j][0] < out[j-1][0]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// predefinedClass matches \d \D \s \S \w \W.
func (p *reParser) predefinedClass() ([][2]rune, bool, bool) {
	if p.pos+1 < len(p.s) && p.s[p.pos] == '\\' {
		switch p.s[p.pos+1] {
		case 'd':
			p.pos += 2
			return reDigitRanges, false, true
		case 'D':
			p.pos += 2
			return reDigitRanges, true, true
		case 's':
			p.pos += 2
			return reSpaceRanges, false, true
		case 'S':
			p.pos += 2
			return reSpaceRanges, true, true
		case 'w':
			p.pos += 2
			return reWordRanges, false, true
		case 'W':
			p.pos += 2
			return reWordRanges, true, true
		}
	}
	return nil, false, false
}

func (p *reParser) parseSimple() *reNode {
	switch {
	case p.match('.'):
		return p.node(&reNode{kind: reAnyChar})
	case p.check(reFlagEmpty) && p.match('#'):
		return p.node(&reNode{kind: reNothing})
	case p.check(reFlagAnyString) && p.match('@'):
		return p.node(&reNode{kind: reAnyString})
	case p.match('"'):
		start := p.pos
		for p.more() && !p.peek("\"") {
			p.next()
		}
		if !p.match('"') {
			panic(reError("expected '\"' at position " + strconv.Itoa(p.pos)))
		}
		return p.literal(p.s[start : p.pos-1])
	case p.match('('):
		if p.match(')') {
			return p.node(&reNode{kind: reEmptyStr})
		}
		p.depth++
		if p.depth > maxRegexpDepth {
			panic(reError("too many nested groups (more than " + strconv.Itoa(maxRegexpDepth) + ")"))
		}
		e := p.parseUnion()
		p.depth--
		if !p.match(')') {
			panic(reError("expected ')' at position " + strconv.Itoa(p.pos)))
		}
		return e
	case (p.check(reFlagAutomaton) || p.check(reFlagInterval)) && p.match('<'):
		start := p.pos
		for p.more() && !p.peek(">") {
			p.next()
		}
		if !p.match('>') {
			panic(reError("expected '>' at position " + strconv.Itoa(p.pos)))
		}
		s := string(p.s[start : p.pos-1])
		i := strings.IndexByte(s, '-')
		if i == -1 {
			if !p.check(reFlagAutomaton) {
				panic(reError("interval syntax error at position " + strconv.Itoa(p.pos-1)))
			}
			panic(reError("'" + s + "' not found"))
		}
		if !p.check(reFlagInterval) {
			panic(reError("illegal identifier at position " + strconv.Itoa(p.pos-1)))
		}
		if i == 0 || i == len(s)-1 || i != strings.LastIndexByte(s, '-') {
			panic(reError("interval syntax error at position " + strconv.Itoa(p.pos-1)))
		}
		smin, smax := s[:i], s[i+1:]
		imin, err1 := strconv.ParseInt(smin, 10, 32)
		imax, err2 := strconv.ParseInt(smax, 10, 32)
		if err1 != nil || err2 != nil {
			panic(reError("interval syntax error at position " + strconv.Itoa(p.pos-1)))
		}
		digits := 0
		if len(smin) == len(smax) {
			digits = len(smin)
		}
		if imin > imax {
			imin, imax = imax, imin
		}
		return p.node(&reNode{kind: reInterval, min: int(imin), max: int(imax), digits: digits})
	}
	if ranges, negated, ok := p.predefinedClass(); ok {
		return p.node(&reNode{kind: reSet, ranges: ranges, negated: negated})
	}
	c := p.parseCharExp()
	return p.node(&reNode{kind: reSet, ranges: p.charRanges(c)})
}

func (p *reParser) literal(rs []rune) *reNode {
	if !p.caseFold {
		return p.node(&reNode{kind: reLiteral, lit: append([]rune(nil), rs...)})
	}
	if len(rs) == 0 {
		return p.node(&reNode{kind: reEmptyStr})
	}
	var e *reNode
	for i := len(rs) - 1; i >= 0; i-- {
		c := p.node(&reNode{kind: reSet, ranges: p.charRanges(rs[i])})
		if e == nil {
			e = c
		} else {
			e = p.node(&reNode{kind: reConcat, a: c, b: e})
		}
	}
	return e
}

func (p *reParser) parseCharExp() rune {
	p.match('\\')
	return p.next()
}

// Matches reports whether the whole string is in the language.
func (re *luceneRegexp) Matches(s string) bool {
	m := &reMatcher{s: []rune(s), memo: map[reMemoKey]reEnds{}}
	ends := m.ends(re.root, 0)
	return len(ends) > 0 && ends[len(ends)-1] == len(m.s)
}

type reMemoKey struct {
	id, pos int
}

// reEnds is a sorted set of end positions. The memoised sets are shared and
// never mutated after they are built, so the operations below always
// return either an input or a fresh slice.
type reEnds []int

func (m *reMatcher) union(a, b reEnds) reEnds {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make(reEnds, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	out = append(out, a[i:]...)
	return append(out, b[j:]...)
}

func (m *reMatcher) intersect(a, b reEnds) reEnds {
	var out reEnds
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

type reMatcher struct {
	s    []rune
	memo map[reMemoKey]reEnds
}

// ends returns, for a start position, the end positions j such that
// s[pos:j] is in the language of the node. Sets are sparse so that the
// work is proportional to the positions actually reached rather than to
// nodes × len(s)², which a long pattern from a request could make huge.
func (m *reMatcher) ends(n *reNode, pos int) reEnds {
	key := reMemoKey{n.id, pos}
	if r, ok := m.memo[key]; ok {
		return r
	}
	size := len(m.s) + 1
	var out reEnds
	switch n.kind {
	case reSet:
		if pos < len(m.s) {
			c := m.s[pos]
			in := false
			for _, r := range n.ranges {
				if c >= r[0] && c <= r[1] {
					in = true
					break
				}
			}
			if in != n.negated {
				out = reEnds{pos + 1}
			}
		}
	case reAnyChar:
		if pos < len(m.s) {
			out = reEnds{pos + 1}
		}
	case reNothing:
	case reEmptyStr:
		out = reEnds{pos}
	case reAnyString:
		out = make(reEnds, 0, size-pos)
		for j := pos; j < size; j++ {
			out = append(out, j)
		}
	case reLiteral:
		if pos+len(n.lit) <= len(m.s) {
			ok := true
			for i, c := range n.lit {
				if m.s[pos+i] != c {
					ok = false
					break
				}
			}
			if ok {
				out = reEnds{pos + len(n.lit)}
			}
		}
	case reInterval:
		for j := pos + 1; j < size; j++ {
			c := m.s[j-1]
			if c < '0' || c > '9' {
				break
			}
			if n.digits > 0 && j-pos != n.digits {
				continue
			}
			digits := strings.TrimLeft(string(m.s[pos:j]), "0")
			if len(digits) > 10 {
				continue
			}
			v := int64(0)
			if digits != "" {
				v, _ = strconv.ParseInt(digits, 10, 64)
			}
			if v >= int64(n.min) && v <= int64(n.max) {
				out = append(out, j)
			}
		}
	case reConcat:
		for _, j := range m.ends(n.a, pos) {
			out = m.union(out, m.ends(n.b, j))
		}
	case reUnion:
		out = m.union(m.ends(n.a, pos), m.ends(n.b, pos))
	case reInter:
		out = m.intersect(m.ends(n.a, pos), m.ends(n.b, pos))
	case reCompl:
		a := m.ends(n.a, pos)
		i := 0
		for j := pos; j < size; j++ {
			for i < len(a) && a[i] < j {
				i++
			}
			if i < len(a) && a[i] == j {
				continue
			}
			out = append(out, j)
		}
	case reRepeat:
		frontier := reEnds{pos}
		if n.min == 0 {
			out = reEnds{pos}
		}
		for count := 1; n.max < 0 || count <= n.max; count++ {
			var next reEnds
			for _, j := range frontier {
				next = m.union(next, m.ends(n.a, j))
			}
			if len(next) == 0 {
				break
			}
			// at a fixed point the reachable set is the one for every
			// remaining count, so it is the result even below min:
			// {1000000000} of a nullable expression ends here instead of
			// iterating a billion times per term
			same := slices.Equal(next, frontier)
			if count >= n.min || same {
				merged := m.union(out, next)
				grew := len(merged) > len(out)
				out = merged
				if same || (n.max < 0 && !grew) {
					break
				}
			}
			frontier = next
		}
	}
	m.memo[key] = out
	return out
}

// wildcard patterns ------------------------------------------------------

type wildcardToken struct {
	kind byte // '*' any string, '?' any char, 'c' literal
	c    rune
}

// compileWildcard tokenizes a Lucene wildcard pattern: * and ? are
// wildcards, a backslash escapes the next character.
func compileWildcard(pattern string) []wildcardToken {
	rs := []rune(pattern)
	var out []wildcardToken
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '*':
			out = append(out, wildcardToken{kind: '*'})
		case '?':
			out = append(out, wildcardToken{kind: '?'})
		case '\\':
			if i+1 < len(rs) {
				i++
			}
			out = append(out, wildcardToken{kind: 'c', c: rs[i]})
		default:
			out = append(out, wildcardToken{kind: 'c', c: rs[i]})
		}
	}
	return out
}

// wildcardLiteralPrefix is the literal text before the first wildcard.
func wildcardLiteralPrefix(tokens []wildcardToken) string {
	var sb strings.Builder
	for _, t := range tokens {
		if t.kind != 'c' {
			break
		}
		sb.WriteRune(t.c)
	}
	return sb.String()
}

func wildcardMatches(tokens []wildcardToken, s string, caseInsensitive bool) bool {
	rs := []rune(s)
	// dp[j]: pattern prefix matches rs[:j]
	dp := make([]bool, len(rs)+1)
	dp[0] = true
	for _, t := range tokens {
		next := make([]bool, len(rs)+1)
		switch t.kind {
		case '*':
			acc := false
			for j := 0; j <= len(rs); j++ {
				acc = acc || dp[j]
				next[j] = acc
			}
		case '?':
			for j := 1; j <= len(rs); j++ {
				next[j] = dp[j-1]
			}
		default:
			for j := 1; j <= len(rs); j++ {
				if dp[j-1] && runeEqual(rs[j-1], t.c, caseInsensitive) {
					next[j] = true
				}
			}
		}
		dp = next
	}
	return dp[len(rs)]
}

func runeEqual(a, b rune, caseInsensitive bool) bool {
	if a == b {
		return true
	}
	if caseInsensitive && a < 128 && b < 128 {
		return asciiLower(a) == asciiLower(b)
	}
	return false
}

func asciiLower(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + 32
	}
	return r
}

// hasPrefixFold is strings.HasPrefix, ASCII case-insensitive when asked.
func hasPrefixFold(s, prefix string, caseInsensitive bool) bool {
	if !caseInsensitive {
		return strings.HasPrefix(s, prefix)
	}
	rs, ps := []rune(s), []rune(prefix)
	if len(ps) > len(rs) {
		return false
	}
	for i, c := range ps {
		if !runeEqual(rs[i], c, true) {
			return false
		}
	}
	return true
}

// fuzzy matching ---------------------------------------------------------

// editDistance is the Levenshtein distance between two code point
// sequences, counting an adjacent transposition as one edit when
// transpositions is set (Lucene's LevenshteinAutomata). It returns
// limit+1 as soon as the distance exceeds limit.
func editDistance(a, b []rune, transpositions bool, limit int) int {
	if d := len(a) - len(b); d > limit || -d > limit {
		return limit + 1
	}
	prev2 := make([]int, len(b)+1)
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			v := prev[j] + 1
			if cur[j-1]+1 < v {
				v = cur[j-1] + 1
			}
			if prev[j-1]+cost < v {
				v = prev[j-1] + cost
			}
			if transpositions && i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] && prev2[j-2]+1 < v {
				v = prev2[j-2] + 1
			}
			cur[j] = v
			if v < best {
				best = v
			}
		}
		if best > limit {
			return limit + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	if prev[len(b)] > limit {
		return limit + 1
	}
	return prev[len(b)]
}
