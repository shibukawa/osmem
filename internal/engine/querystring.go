package engine

import (
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/search/query"
	index "github.com/blevesearch/bleve_index_api"
)

// query_string: Lucene's classic QueryParser grammar (with
// splitOnWhitespace=false, as OpenSearch configures it) and the field
// handling of OpenSearch's QueryStringQueryParser. simple_query_string:
// Lucene's SimpleQueryParser with OpenSearch's SimpleQueryStringQueryParser.

// lexer ------------------------------------------------------------------------

type qsKind int

const (
	qsEOF qsKind = iota
	qsAND
	qsOR
	qsNOT
	qsPLUS
	qsMINUS
	qsBAREOPER
	qsLPAREN
	qsRPAREN
	qsCOLON
	qsSTAR
	qsCARAT
	qsQUOTED
	qsTERM
	qsFUZZYSLOP
	qsPREFIXTERM
	qsWILDTERM
	qsREGEXPTERM
	qsRANGEINSTART
	qsRANGEEXSTART
	qsNUMBER
	qsRANGETO
	qsRANGEINEND
	qsRANGEEXEND
	qsRANGEQUOTED
	qsRANGEGOOP
)

var qsTokenImages = map[qsKind]string{
	qsEOF: "<EOF>", qsAND: "<AND>", qsOR: "<OR>", qsNOT: "<NOT>", qsPLUS: "\"+\"", qsMINUS: "\"-\"", qsBAREOPER: "<BAREOPER>",
	qsLPAREN: "\"(\"", qsRPAREN: "\")\"", qsCOLON: "\":\"", qsSTAR: "\"*\"", qsCARAT: "\"^\"", qsQUOTED: "<QUOTED>", qsTERM: "<TERM>",
	qsFUZZYSLOP: "<FUZZY_SLOP>", qsPREFIXTERM: "<PREFIXTERM>", qsWILDTERM: "<WILDTERM>", qsREGEXPTERM: "<REGEXPTERM>",
	qsRANGEINSTART: "\"[\"", qsRANGEEXSTART: "\"{\"", qsNUMBER: "<NUMBER>", qsRANGETO: "\"TO\"", qsRANGEINEND: "\"]\"",
	qsRANGEEXEND: "\"}\"", qsRANGEQUOTED: "<RANGE_QUOTED>", qsRANGEGOOP: "<RANGE_GOOP>",
}

type qsToken struct {
	kind  qsKind
	image string
	col   int // rune index of the token start
}

// qsSyntax is a failure of the classic query parser: the message of the
// ParseException (or TokenMgrError when lexical).
type qsSyntax struct {
	msg     string
	lexical bool
}

func (e *qsSyntax) Error() string { return e.msg }

func isQSWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '　'
}

func isQSTermStart(r rune) bool {
	if isQSWhitespace(r) {
		return false
	}
	return !strings.ContainsRune("+-!():^[]\"{}~*?\\/", r)
}

// lexQueryString tokenizes a query string with the lexical states of the
// classic query parser.
func lexQueryString(s string) ([]qsToken, *qsSyntax) {
	rs := []rune(s)
	var out []qsToken
	const (
		stDefault = iota
		stRange
		stBoost
	)
	state := stDefault
	i := 0
	// escaped reports the length of an escaped character at j (0 if none)
	escaped := func(j int) int {
		if j < len(rs) && rs[j] == '\\' {
			if j+1 < len(rs) {
				return 2
			}
			return -1
		}
		return 0
	}
	lexErr := func(prefix string, st int) *qsSyntax {
		lexState := 2
		switch st {
		case stBoost:
			lexState = 0
		case stRange:
			lexState = 1
		}
		msg := fmt.Sprintf("Lexical error at line 1, column %d.  Encountered: <EOF>", len(rs)+1)
		if prefix != "" {
			msg += " after prefix \"" + javaccEscape(prefix) + "\""
		}
		return &qsSyntax{msg: msg + fmt.Sprintf(" (in lexical state %d)", lexState), lexical: true}
	}
	for {
		if state != stBoost {
			for i < len(rs) && isQSWhitespace(rs[i]) {
				i++
			}
		}
		if i >= len(rs) {
			out = append(out, qsToken{kind: qsEOF, col: len(rs)})
			return out, nil
		}
		start := i
		switch state {
		case stBoost:
			j := i
			for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
				j++
			}
			if j == i {
				return nil, &qsSyntax{msg: fmt.Sprintf("Lexical error at line 1, column %d.  Encountered: '%s' (%d),", i+1, javaccEscape(string(rs[i])), rs[i]), lexical: true}
			}
			if j+1 < len(rs) && rs[j] == '.' && rs[j+1] >= '0' && rs[j+1] <= '9' {
				j++
				for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
					j++
				}
			}
			out = append(out, qsToken{kind: qsNUMBER, image: string(rs[i:j]), col: start})
			i = j
			state = stDefault
			continue
		case stRange:
			switch {
			case rs[i] == ']':
				out = append(out, qsToken{kind: qsRANGEINEND, image: "]", col: start})
				i++
				state = stDefault
				continue
			case rs[i] == '}':
				out = append(out, qsToken{kind: qsRANGEEXEND, image: "}", col: start})
				i++
				state = stDefault
				continue
			}
			// longest of RANGE_QUOTED and RANGE_GOOP, "TO" on ties
			quotedEnd := -1
			if rs[i] == '"' {
				j := i + 1
				n := 0
				for j < len(rs) {
					if rs[j] == '\\' && j+1 < len(rs) && rs[j+1] == '"' {
						j += 2
						n++
						continue
					}
					if rs[j] == '"' {
						break
					}
					j++
					n++
				}
				if j < len(rs) && n > 0 {
					quotedEnd = j + 1
				}
			}
			j := i
			for j < len(rs) && rs[j] != ' ' && rs[j] != ']' && rs[j] != '}' {
				j++
			}
			switch {
			case quotedEnd > j:
				out = append(out, qsToken{kind: qsRANGEQUOTED, image: string(rs[i:quotedEnd]), col: start})
				i = quotedEnd
			case j-i == 2 && rs[i] == 'T' && rs[i+1] == 'O':
				out = append(out, qsToken{kind: qsRANGETO, image: "TO", col: start})
				i = j
			default:
				out = append(out, qsToken{kind: qsRANGEGOOP, image: string(rs[i:j]), col: start})
				i = j
			}
			continue
		}
		type cand struct {
			kind qsKind
			n    int
		}
		var best cand
		try := func(k qsKind, n int) {
			if n > best.n {
				best = cand{k, n}
			}
		}
		// prefix tests on the rune slice: converting the rest of the input
		// to a string per token made lexing quadratic in the query length
		rest := rs[i:]
		switch {
		case runesHavePrefix(rest, "AND"):
			try(qsAND, 3)
		case runesHavePrefix(rest, "&&"):
			try(qsAND, 2)
		}
		switch {
		case runesHavePrefix(rest, "OR"):
			try(qsOR, 2)
		case runesHavePrefix(rest, "||"):
			try(qsOR, 2)
		}
		switch {
		case runesHavePrefix(rest, "NOT"):
			try(qsNOT, 3)
		case rs[i] == '!':
			try(qsNOT, 1)
		}
		if rs[i] == '+' {
			try(qsPLUS, 1)
		}
		if rs[i] == '-' {
			try(qsMINUS, 1)
		}
		if (rs[i] == '+' || rs[i] == '-' || rs[i] == '!') && i+1 < len(rs) && isQSWhitespace(rs[i+1]) {
			try(qsBAREOPER, 2)
		}
		switch rs[i] {
		case '(':
			try(qsLPAREN, 1)
		case ')':
			try(qsRPAREN, 1)
		case ':':
			try(qsCOLON, 1)
		case '*':
			try(qsSTAR, 1)
		case '^':
			try(qsCARAT, 1)
		case '[':
			try(qsRANGEINSTART, 1)
		case '{':
			try(qsRANGEEXSTART, 1)
		}
		unclosedQuote := false
		if rs[i] == '"' {
			j := i + 1
			closed := false
			for j < len(rs) {
				if e := escaped(j); e > 0 {
					j += e
					continue
				} else if e < 0 {
					break
				}
				if rs[j] == '"' {
					closed = true
					break
				}
				j++
			}
			if closed {
				try(qsQUOTED, j+1-i)
			} else {
				unclosedQuote = true
			}
		}
		// TERM: start char then term chars
		termLen := 0
		if e := escaped(i); e > 0 || (e == 0 && isQSTermStart(rs[i])) {
			j := i
			if e > 0 {
				j += e
			} else {
				j++
			}
			for j < len(rs) {
				if e := escaped(j); e > 0 {
					j += e
					continue
				} else if e < 0 {
					break
				}
				if isQSTermStart(rs[j]) || rs[j] == '-' || rs[j] == '+' {
					j++
					continue
				}
				break
			}
			termLen = j - i
			try(qsTERM, termLen)
			if j < len(rs) && rs[j] == '*' {
				try(qsPREFIXTERM, termLen+1)
			}
		}
		// FUZZY_SLOP
		if rs[i] == '~' {
			j := i + 1
			for j < len(rs) {
				if e := escaped(j); e > 0 {
					j += e
					continue
				} else if e < 0 {
					break
				}
				if isQSTermStart(rs[j]) || rs[j] == '-' || rs[j] == '+' {
					j++
					continue
				}
				break
			}
			try(qsFUZZYSLOP, j-i)
		}
		// WILDTERM
		if e := escaped(i); e > 0 || (e == 0 && (isQSTermStart(rs[i]) || rs[i] == '*' || rs[i] == '?')) {
			j := i
			if e > 0 {
				j += e
			} else {
				j++
			}
			for j < len(rs) {
				if e := escaped(j); e > 0 {
					j += e
					continue
				} else if e < 0 {
					break
				}
				if isQSTermStart(rs[j]) || rs[j] == '-' || rs[j] == '+' || rs[j] == '*' || rs[j] == '?' {
					j++
					continue
				}
				break
			}
			try(qsWILDTERM, j-i)
		}
		// REGEXPTERM
		if rs[i] == '/' {
			j := i + 1
			closed := false
			for j < len(rs) {
				if rs[j] == '\\' && j+1 < len(rs) && rs[j+1] == '/' {
					j += 2
					continue
				}
				if rs[j] == '/' {
					closed = true
					break
				}
				j++
			}
			if closed {
				try(qsREGEXPTERM, j+1-i)
			}
		}
		if best.n == 0 {
			if unclosedQuote {
				return nil, lexErr(string(rs[i:]), stDefault)
			}
			if rs[i] == '\\' {
				return nil, lexErr("\\", stDefault)
			}
			return nil, &qsSyntax{msg: fmt.Sprintf("Lexical error at line 1, column %d.  Encountered: '%s' (%d),", i+1, javaccEscape(string(rs[i])), rs[i]), lexical: true}
		}
		tok := qsToken{kind: best.kind, image: string(rs[i : i+best.n]), col: start}
		out = append(out, tok)
		i += best.n
		switch best.kind {
		case qsCARAT:
			state = stBoost
		case qsRANGEINSTART, qsRANGEEXSTART:
			state = stRange
		}
	}
}

func javaccEscape(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\t':
			sb.WriteString(`\t`)
		case '\r':
			sb.WriteString(`\r`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// discardEscapeChar is QueryParserBase.discardEscapeChar.
func discardEscapeChar(s string) (string, *qsSyntax) {
	rs := []rune(s)
	var sb strings.Builder
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\\' {
			sb.WriteRune(rs[i])
			continue
		}
		if i+1 >= len(rs) {
			return "", &qsSyntax{msg: "Term can not end with escape character."}
		}
		i++
		if rs[i] == 'u' {
			if i+4 >= len(rs) {
				return "", &qsSyntax{msg: "Truncated unicode escape sequence."}
			}
			n, err := strconv.ParseUint(string(rs[i+1:i+5]), 16, 32)
			if err != nil {
				return "", &qsSyntax{msg: "Non-hex character in Unicode escape sequence: " + string(rs[i+1])}
			}
			sb.WriteRune(rune(n))
			i += 4
			continue
		}
		sb.WriteRune(rs[i])
	}
	return sb.String(), nil
}

// luceneEscape is QueryParser.escape.
func luceneEscape(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\+-!():^[]"{}~*?|&/`, r) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func errQueryStringParse(text string, e *qsSyntax) *Error {
	inner := &Error{Type: "parse_exception", plain: true, Reason: e.msg}
	if e.lexical {
		inner = &Error{Type: "token_mgr_error", Reason: e.msg}
	}
	return &Error{Status: http.StatusBadRequest, Type: "query_shard_exception", Reason: "Failed to parse query [" + text + "]",
		Cause: &Error{Type: "parse_exception", plain: true, Reason: "Cannot parse '" + text + "': " + e.msg, Cause: inner}}
}

// parser -----------------------------------------------------------------------

type qsOccur int

const (
	occurShould qsOccur = iota
	occurMust
	occurMustNot
)

type qsClause struct {
	q     query.Query
	occur qsOccur
}

type qsParser struct {
	qb          *queryBuilder
	spec        *queryStringSpec
	toks        []qsToken
	pos         int
	field       *string       // the default field (nil: the fields and weights)
	weights     []fieldWeight // fields and weights
	lenient     bool
	depth       int // open groups, bounded by maxQueryStringDepth
	andOp       bool
	force       analysis.Analyzer
	quoteAn     analysis.Analyzer
	analyzer    string
	quoteAnName string
	tie         float64
}

// qsFailure carries a runtime error (not a syntax error) out of the parser.
type qsFailure struct {
	err error
}

func (p *qsParser) peek(k int) qsToken {
	if p.pos+k < len(p.toks) {
		return p.toks[p.pos+k]
	}
	return p.toks[len(p.toks)-1]
}

func (p *qsParser) next() qsToken {
	t := p.peek(0)
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *qsParser) unexpected(t qsToken) *qsSyntax {
	if t.kind == qsEOF {
		return &qsSyntax{msg: fmt.Sprintf("Encountered \"<EOF>\" at line 1, column %d.", t.col)}
	}
	return &qsSyntax{msg: fmt.Sprintf("Encountered \" %s \"%s \"\" at line 1, column %d.", qsTokenImages[t.kind], javaccEscape(t.image), t.col)}
}

func (p *qsParser) fail(err error) {
	panic(qsFailure{err})
}

func (p *qsParser) syntax(e *qsSyntax) {
	panic(e)
}

func runesHavePrefix(rs []rune, prefix string) bool {
	i := 0
	for _, r := range prefix {
		if i >= len(rs) || rs[i] != r {
			return false
		}
		i++
	}
	return true
}

// maxQueryStringDepth bounds parenthesised sub-queries: the parsers recurse
// per group and a request must not be able to exhaust the stack.
const maxQueryStringDepth = 1000

func allowedPostMultiTerm(k qsKind) bool {
	switch k {
	case qsCOLON, qsSTAR, qsFUZZYSLOP, qsCARAT, qsAND, qsOR:
		return false
	}
	return true
}

func (p *qsParser) isMultiTerm() bool {
	return p.peek(0).kind == qsTERM && p.peek(1).kind == qsTERM && allowedPostMultiTerm(p.peek(2).kind)
}

func canStartClause(k qsKind) bool {
	switch k {
	case qsAND, qsOR, qsPLUS, qsMINUS, qsNOT, qsTERM, qsSTAR, qsPREFIXTERM, qsWILDTERM, qsREGEXPTERM, qsNUMBER, qsBAREOPER,
		qsQUOTED, qsRANGEINSTART, qsRANGEEXSTART, qsLPAREN:
		return true
	}
	return false
}

// parse is TopLevelQuery.
func (p *qsParser) parse() (q query.Query, err error) {
	defer func() {
		if r := recover(); r != nil {
			switch t := r.(type) {
			case *qsSyntax:
				err = t
			case qsFailure:
				err = t.err
			default:
				panic(r)
			}
		}
	}()
	q = p.query(p.field)
	if t := p.peek(0); t.kind != qsEOF {
		p.syntax(p.unexpected(t))
	}
	return q, nil
}

func (p *qsParser) query(field *string) query.Query {
	var clauses []qsClause
	var first query.Query
	firstSet := false
	if p.isMultiTerm() {
		first = p.multiTerm(field, &clauses)
		firstSet = first != nil
	} else {
		mods := p.modifiers()
		q := p.clause(field)
		p.addClause(&clauses, 0, mods, q)
		if mods == 0 {
			first, firstSet = q, q != nil
		}
	}
	for {
		if p.isMultiTerm() {
			p.multiTerm(field, &clauses)
			continue
		}
		if !canStartClause(p.peek(0).kind) {
			break
		}
		conj := p.conjunction()
		mods := p.modifiers()
		q := p.clause(field)
		p.addClause(&clauses, conj, mods, q)
	}
	if len(clauses) == 1 && firstSet {
		return first
	}
	return p.booleanQuery(clauses)
}

const (
	conjAnd = 1
	conjOr  = 2
	modReq  = 1
	modNot  = 2
)

func (p *qsParser) conjunction() int {
	switch p.peek(0).kind {
	case qsAND:
		p.next()
		return conjAnd
	case qsOR:
		p.next()
		return conjOr
	}
	return 0
}

func (p *qsParser) modifiers() int {
	switch p.peek(0).kind {
	case qsPLUS:
		p.next()
		return modReq
	case qsMINUS, qsNOT:
		p.next()
		return modNot
	}
	return 0
}

// addClause is QueryParserBase.addClause.
func (p *qsParser) addClause(clauses *[]qsClause, conj, mods int, q query.Query) {
	cs := *clauses
	if len(cs) > 0 && conj == conjAnd {
		if cs[len(cs)-1].occur != occurMustNot {
			cs[len(cs)-1].occur = occurMust
		}
	}
	if len(cs) > 0 && p.andOp && conj == conjOr {
		if cs[len(cs)-1].occur != occurMustNot {
			cs[len(cs)-1].occur = occurShould
		}
	}
	*clauses = cs
	if q == nil {
		return
	}
	var required, prohibited bool
	if !p.andOp {
		prohibited = mods == modNot
		required = mods == modReq
		if conj == conjAnd && !prohibited {
			required = true
		}
	} else {
		prohibited = mods == modNot
		required = !prohibited && conj != conjOr
	}
	switch {
	case required && !prohibited:
		*clauses = append(*clauses, qsClause{q, occurMust})
	case !required && !prohibited:
		*clauses = append(*clauses, qsClause{q, occurShould})
	default:
		*clauses = append(*clauses, qsClause{q, occurMustNot})
	}
}

// addMultiTermClauses is QueryParserBase.addMultiTermClauses.
func (p *qsParser) addMultiTermClauses(clauses *[]qsClause, q query.Query) {
	if q == nil {
		return
	}
	occur := occurShould
	if p.andOp {
		occur = occurMust
	}
	if bq, ok := q.(*luceneBoolQuery); ok && len(bq.filter) == 0 && len(bq.mustNot) == 0 && bq.minShould == 0 {
		for _, c := range bq.must {
			*clauses = append(*clauses, qsClause{c, occurMust})
		}
		for _, c := range bq.should {
			*clauses = append(*clauses, qsClause{c, occurShould})
		}
		return
	}
	*clauses = append(*clauses, qsClause{q, occur})
}

// booleanQuery is getBooleanQuery with fixNegativeQueryIfNeeded.
func (p *qsParser) booleanQuery(clauses []qsClause) query.Query {
	if len(clauses) == 0 {
		return nil
	}
	bq := &luceneBoolQuery{}
	for _, c := range clauses {
		switch c.occur {
		case occurMust:
			bq.must = append(bq.must, c.q)
		case occurShould:
			bq.should = append(bq.should, c.q)
		default:
			bq.mustNot = append(bq.mustNot, c.q)
		}
	}
	return fixNegativeQuery(bq)
}

// fixNegativeQuery is Queries.fixNegativeQueryIfNeeded.
func fixNegativeQuery(q query.Query) query.Query {
	bq, ok := q.(*luceneBoolQuery)
	if !ok || len(bq.mustNot) == 0 || len(bq.must)+len(bq.should)+len(bq.filter) > 0 {
		return q
	}
	c := *bq
	c.filter = append(append([]query.Query(nil), bq.filter...), bleve.NewMatchAllQuery())
	return &c
}

func (p *qsParser) multiTerm(field *string, clauses *[]qsClause) query.Query {
	var sb strings.Builder
	sb.WriteString(p.next().image)
	for p.peek(0).kind == qsTERM && allowedPostMultiTerm(p.peek(1).kind) {
		sb.WriteByte(' ')
		sb.WriteString(p.next().image)
	}
	unescaped, serr := discardEscapeChar(sb.String())
	if serr != nil {
		p.syntax(serr)
	}
	q := p.fieldQuery(field, unescaped, false)
	p.addMultiTermClauses(clauses, q)
	return q
}

func (p *qsParser) clause(field *string) query.Query {
	if p.peek(1).kind == qsCOLON {
		switch p.peek(0).kind {
		case qsTERM:
			name, serr := discardEscapeChar(p.next().image)
			if serr != nil {
				p.syntax(serr)
			}
			p.next()
			field = &name
		case qsSTAR:
			p.next()
			p.next()
			star := "*"
			field = &star
		}
	}
	var q query.Query
	if p.peek(0).kind == qsLPAREN {
		p.next()
		p.depth++
		if p.depth > maxQueryStringDepth {
			p.fail(errParsing("query_string is nested too deeply (more than %d groups)", maxQueryStringDepth))
		}
		q = p.query(field)
		p.depth--
		if t := p.peek(0); t.kind != qsRPAREN {
			p.syntax(p.unexpected(t))
		}
		p.next()
		if p.peek(0).kind == qsCARAT {
			p.next()
			q = p.boost(q)
		}
		return q
	}
	return p.term(field)
}

func (p *qsParser) boost(q query.Query) query.Query {
	t := p.peek(0)
	if t.kind != qsNUMBER {
		p.syntax(p.unexpected(t))
	}
	p.next()
	f, err := strconv.ParseFloat(t.image, 32)
	if err != nil {
		f = 1
	}
	if q == nil {
		return nil
	}
	return newBoost(q, f)
}

// slopAndBoost parses the optional [^boost] [~slop] suffixes of a term.
func (p *qsParser) slopAndBoost() (fuzzy *qsToken, boost *qsToken) {
	switch p.peek(0).kind {
	case qsCARAT:
		p.next()
		b := p.peek(0)
		if b.kind != qsNUMBER {
			p.syntax(p.unexpected(b))
		}
		p.next()
		boost = &b
		if p.peek(0).kind == qsFUZZYSLOP {
			f := p.next()
			fuzzy = &f
		}
	case qsFUZZYSLOP:
		f := p.next()
		fuzzy = &f
		if p.peek(0).kind == qsCARAT {
			p.next()
			b := p.peek(0)
			if b.kind != qsNUMBER {
				p.syntax(p.unexpected(b))
			}
			p.next()
			boost = &b
		}
	}
	return
}

func applyBoostToken(q query.Query, b *qsToken) query.Query {
	if b == nil || q == nil {
		return q
	}
	f, err := strconv.ParseFloat(b.image, 32)
	if err != nil {
		f = 1
	}
	return newBoost(q, f)
}

func (p *qsParser) term(field *string) query.Query {
	t := p.peek(0)
	switch t.kind {
	case qsTERM, qsSTAR, qsPREFIXTERM, qsWILDTERM, qsREGEXPTERM, qsNUMBER, qsBAREOPER:
		p.next()
		image := t.image
		if t.kind == qsBAREOPER {
			image = image[:1]
		}
		fuzzy, boost := p.slopAndBoost()
		var q query.Query
		switch t.kind {
		case qsSTAR, qsWILDTERM:
			q = p.wildcardQuery(field, image)
		case qsPREFIXTERM:
			text, serr := discardEscapeChar(image[:len(image)-1])
			if serr != nil {
				p.syntax(serr)
			}
			q = p.prefixQuery(field, text)
		case qsREGEXPTERM:
			q = p.regexpQuery(field, image[1:len(image)-1])
		default:
			text, serr := discardEscapeChar(image)
			if serr != nil {
				p.syntax(serr)
			}
			if fuzzy != nil {
				q = p.fuzzyQuery(field, text, fuzzy.image)
			} else {
				q = p.fieldQuery(field, text, false)
			}
		}
		return applyBoostToken(q, boost)
	case qsRANGEINSTART, qsRANGEEXSTART:
		p.next()
		startInc := t.kind == qsRANGEINSTART
		g1 := p.peek(0)
		if g1.kind != qsRANGEGOOP && g1.kind != qsRANGEQUOTED && g1.kind != qsRANGETO {
			p.syntax(p.unexpected(g1))
		}
		p.next()
		if to := p.peek(0); to.kind != qsRANGETO {
			p.syntax(p.unexpected(to))
		}
		p.next()
		g2 := p.peek(0)
		if g2.kind != qsRANGEGOOP && g2.kind != qsRANGEQUOTED && g2.kind != qsRANGETO {
			p.syntax(p.unexpected(g2))
		}
		p.next()
		end := p.peek(0)
		if end.kind != qsRANGEINEND && end.kind != qsRANGEEXEND {
			p.syntax(p.unexpected(end))
		}
		p.next()
		var boost *qsToken
		if p.peek(0).kind == qsCARAT {
			p.next()
			b := p.peek(0)
			if b.kind != qsNUMBER {
				p.syntax(p.unexpected(b))
			}
			p.next()
			boost = &b
		}
		part := func(g qsToken) *string {
			img := g.image
			if g.kind == qsRANGEQUOTED {
				img = img[1 : len(img)-1]
			} else if img == "*" {
				return nil
			}
			s, serr := discardEscapeChar(img)
			if serr != nil {
				p.syntax(serr)
			}
			return &s
		}
		q := p.rangeQuery(field, part(g1), part(g2), startInc, end.kind == qsRANGEINEND)
		return applyBoostToken(q, boost)
	case qsQUOTED:
		p.next()
		fuzzy, boost := p.slopAndBoost()
		slop := p.spec.phraseSlop
		if fuzzy != nil {
			if f, err := strconv.ParseFloat(fuzzy.image[1:], 32); err == nil {
				slop = int(float32(f))
			}
		}
		text, serr := discardEscapeChar(t.image[1 : len(t.image)-1])
		if serr != nil {
			p.syntax(serr)
		}
		return applyBoostToken(p.phraseQuery(field, text, slop), boost)
	}
	p.syntax(p.unexpected(t))
	return nil
}

// fields -----------------------------------------------------------------------

// extractFields is QueryStringQueryParser.extractMultiFields.
func (p *qsParser) extractFields(field *string, quoted bool) []fieldWeight {
	suffix := ""
	if quoted {
		suffix = p.spec.quoteFieldSuffix
	}
	if field != nil {
		allFields := *field == "*"
		multi := strings.ContainsAny(*field, "*?")
		return p.qb.resolveFieldPattern(*field, 1, !allFields, !multi, suffix)
	}
	if quoted && suffix != "" {
		return p.qb.resolveFields(p.weights, suffix)
	}
	return p.weights
}

// resolveFieldPattern is QueryParserHelper.resolveMappingField.
func (qb *queryBuilder) resolveFieldPattern(pattern string, weight float64, acceptAllTypes, acceptMetadata bool, suffix string) []fieldWeight {
	names := []string{pattern}
	if strings.ContainsAny(pattern, "*?") {
		names = qb.ix.Mapping.leafFields(pattern)
	}
	var out []fieldWeight
	seen := map[string]int{}
	for _, name := range names {
		if suffix != "" {
			if _, _, ok := qb.ix.Mapping.resolve(name + suffix); ok {
				name += suffix
			}
		}
		f, _, ok := qb.ix.Mapping.resolve(name)
		if !ok {
			continue
		}
		if !acceptMetadata && strings.HasPrefix(name, "_") {
			continue
		}
		if !acceptAllTypes && !textSearchable(f) {
			continue
		}
		if i, dup := seen[name]; dup {
			out[i].boost *= weight
			continue
		}
		seen[name] = len(out)
		out = append(out, fieldWeight{field: name, boost: weight})
	}
	return out
}

func (p *qsParser) orTie(queries []query.Query) query.Query {
	var qs []query.Query
	for _, q := range queries {
		if q != nil {
			qs = append(qs, q)
		}
	}
	switch len(qs) {
	case 0:
		return nil
	case 1:
		return qs[0]
	}
	return &disMaxQuery{queries: qs, tie: p.tie}
}

func (p *qsParser) lenientOr(err error) query.Query {
	if p.lenient {
		return bleve.NewMatchNoneQuery()
	}
	p.fail(err)
	return nil
}

// options builds the MultiMatchQuery settings of the parser.
func (p *qsParser) options() *matchOptions {
	op := "or"
	if p.andOp {
		op = "and"
	}
	return &matchOptions{operator: op, lenient: p.lenient, zeroTermsNull: true, maxExpansions: 50, transpositions: true}
}

func (p *qsParser) fieldQuery(field *string, text string, quoted bool) query.Query {
	if quoted {
		return p.phraseQuery(field, text, p.spec.phraseSlop)
	}
	if field != nil && *field == "_exists_" {
		return p.existsQuery(text)
	}
	if field != nil && utf8.RuneCountInString(text) > 1 {
		switch {
		case strings.HasPrefix(text, ">="):
			if len(text) > 2 {
				return p.rangeQuery(field, strPtr(text[2:]), nil, true, true)
			}
			return p.rangeQuery(field, strPtr(text[1:]), nil, false, true)
		case strings.HasPrefix(text, ">"):
			return p.rangeQuery(field, strPtr(text[1:]), nil, false, true)
		case strings.HasPrefix(text, "<="):
			if len(text) > 2 {
				return p.rangeQuery(field, nil, strPtr(text[2:]), true, true)
			}
			return p.rangeQuery(field, nil, strPtr(text[1:]), true, false)
		case strings.HasPrefix(text, "<"):
			return p.rangeQuery(field, nil, strPtr(text[1:]), true, false)
		}
		if f, _, ok := p.qb.ix.Mapping.resolve(*field); ok && f.isDate() && p.spec.timeZone != "" {
			return p.rangeQuery(field, &text, &text, true, true)
		}
	}
	fields := p.extractFields(field, false)
	if len(fields) == 0 {
		return bleve.NewMatchNoneQuery()
	}
	q, err := p.multiMatch(fields, text, false, 0)
	if err != nil {
		p.fail(err)
	}
	return q
}

func strPtr(s string) *string { return &s }

// multiMatch is MultiMatchQuery.parse with the parser's settings.
func (p *qsParser) multiMatch(fields []fieldWeight, text string, phrase bool, slop int) (query.Query, error) {
	o := p.options()
	analyzer := p.analyzer
	if phrase {
		if p.quoteAnName != "" {
			analyzer = p.quoteAnName
		}
	}
	if !phrase && p.spec.typ == "cross_fields" {
		groups, err := p.qb.crossFieldsGroups(fields, text, analyzer, p.tie, o)
		if err != nil {
			return nil, err
		}
		return p.orTie(groups), nil
	}
	var qs []query.Query
	for _, fw := range fields {
		var q query.Query
		var err error
		switch {
		case phrase:
			q, err = p.qb.phraseField("query_string", fw.field, text, analyzer, slop, false, 50, o)
		case p.spec.typ == "phrase" || p.spec.typ == "phrase_prefix":
			q, err = p.qb.phraseField("query_string", fw.field, text, analyzer, p.spec.phraseSlop, p.spec.typ == "phrase_prefix", 50, o)
		default:
			q, err = p.qb.matchField("query_string", fw.field, text, analyzer, o, p.spec.typ == "bool_prefix")
		}
		if err != nil {
			return nil, err
		}
		if q != nil {
			qs = append(qs, newBoost(q, fw.boost))
		}
	}
	return p.orTie(qs), nil
}

func (p *qsParser) phraseQuery(field *string, text string, slop int) query.Query {
	if field != nil && *field == "_exists_" {
		return p.existsQuery(text)
	}
	fields := p.extractFields(field, true)
	if len(fields) == 0 {
		return bleve.NewMatchNoneQuery()
	}
	q, err := p.multiMatch(fields, text, true, slop)
	if err != nil {
		p.fail(err)
	}
	return q
}

func (p *qsParser) existsQuery(field string) query.Query {
	q := p.qb.existsQuery(field)
	if _, none := q.(*query.MatchNoneQuery); none {
		return q
	}
	return &constantScoreQuery{inner: q, score: 1}
}

// normalizeText is Analyzer.normalize with a field's search analyzer.
func (p *qsParser) normalizeText(f *Field, s string) string {
	an := p.force
	if an == nil {
		if f.Type == TypeText || f.Type == TypeMatchOnlyText || f.Type == TypeSearchAsYouType {
			an, _, _ = p.qb.searchAnalyzer("query_string", f, "", false)
		} else {
			return p.qb.normalizeForField(f, s)
		}
	}
	if an != nil && analyzerLowercases(an) {
		return strings.ToLower(s)
	}
	return s
}

// analyzerLowercases reports whether an analyzer has a lowercase filter
// (the normalization Lucene applies to multi-term query text).
func analyzerLowercases(an analysis.Analyzer) bool {
	da, ok := an.(*analysis.DefaultAnalyzer)
	if !ok {
		return true
	}
	// the answer is a property of the analyzer instance (analyzers live as
	// long as their index), and the reflective walk ran per normalised term
	if v, ok := lowercasingAnalyzers.Load(da); ok {
		return v.(bool)
	}
	lower := false
	for _, tf := range da.TokenFilters {
		if strings.Contains(strings.ToLower(reflect.TypeOf(tf).String()), "lowercase") {
			lower = true
			break
		}
	}
	lowercasingAnalyzers.Store(da, lower)
	return lower
}

var lowercasingAnalyzers sync.Map // *analysis.DefaultAnalyzer -> bool

func (p *qsParser) wildcardQuery(field *string, image string) query.Query {
	actual := field
	if actual == nil {
		actual = p.field
	}
	if image == "*" && actual != nil {
		if *actual == "*" {
			return &constantScoreQuery{inner: bleve.NewMatchAllQuery(), score: 1}
		}
		return p.existsQuery(*actual)
	}
	fields := p.extractFields(field, false)
	if len(fields) == 0 {
		return bleve.NewMatchNoneQuery()
	}
	var qs []query.Query
	for _, fw := range fields {
		var q query.Query
		if image == "*" {
			q = p.existsQuery(fw.field)
		} else {
			q = p.wildcardSingle(fw.field, image)
		}
		qs = append(qs, newBoost(q, fw.boost))
	}
	return p.orTie(qs)
}

func (p *qsParser) wildcardSingle(field, pattern string) query.Query {
	f, _, ok := p.qb.ix.Mapping.resolve(field)
	if !ok {
		return bleve.NewMatchNoneQuery()
	}
	if !p.spec.allowLeadingWildcard && (strings.HasPrefix(pattern, "*") || strings.HasPrefix(pattern, "?")) {
		p.syntax(&qsSyntax{msg: "'*' or '?' not allowed as first character in WildcardQuery"})
	}
	if !isStringField(f) {
		return p.lenientOr(stringQueryTypeError("wildcard", field, f))
	}
	normalized := p.normalizeWildcardText(f, pattern)
	tokens := compileWildcard(normalized)
	literal := wildcardLiteralPrefix(tokens)
	path := p.qb.ix.Mapping.searchPath(field)
	return &termsUnionQuery{field: path, constant: true, boost: 1, expand: wildcardExpansion(path, literal, tokens)}
}

func (p *qsParser) normalizeWildcardText(f *Field, pattern string) string {
	var sb, chunk strings.Builder
	flush := func() {
		if chunk.Len() > 0 {
			sb.WriteString(p.normalizeText(f, chunk.String()))
			chunk.Reset()
		}
	}
	rs := []rune(pattern)
	for i := 0; i < len(rs); i++ {
		switch {
		case rs[i] == '\\' && i+1 < len(rs):
			flush()
			sb.WriteRune(rs[i])
			sb.WriteRune(rs[i+1])
			i++
		case rs[i] == '*' || rs[i] == '?':
			flush()
			sb.WriteRune(rs[i])
		default:
			chunk.WriteRune(rs[i])
		}
	}
	flush()
	return sb.String()
}

func (p *qsParser) prefixQuery(field *string, text string) query.Query {
	fields := p.extractFields(field, false)
	if len(fields) == 0 {
		return bleve.NewMatchNoneQuery()
	}
	var qs []query.Query
	for _, fw := range fields {
		q := p.prefixSingle(fw.field, text)
		if q != nil {
			qs = append(qs, newBoost(q, fw.boost))
		}
	}
	return p.orTie(qs)
}

func (p *qsParser) prefixSingle(field, text string) query.Query {
	f, _, ok := p.qb.ix.Mapping.resolve(field)
	if !ok {
		return bleve.NewMatchNoneQuery()
	}
	if !isStringField(f) {
		return p.lenientOr(stringQueryTypeError("prefix", field, f))
	}
	tokenized := f.Type == TypeText || f.Type == TypeMatchOnlyText || f.Type == TypeSearchAsYouType
	field = p.qb.ix.Mapping.searchPath(field)
	if !tokenized {
		return &termsUnionQuery{field: field, constant: true, boost: 1, expand: prefixExpansion(field, p.qb.normalizeForField(f, text))}
	}
	if !p.spec.analyzeWildcard {
		return &termsUnionQuery{field: field, constant: true, boost: 1, expand: prefixExpansion(field, p.normalizeText(f, text))}
	}
	an := p.force
	if an == nil {
		an, _, _ = p.qb.searchAnalyzer("query_string", f, "", false)
	}
	groups := analyzeGroups(an, text)
	o := p.options()
	return analyzedPrefix(p.qb, field, f, groups, o, p.andOp)
}

// analyzedPrefix builds the analyzed prefix query: terms, with a prefix on
// the last position.
func analyzedPrefix(qb *queryBuilder, field string, f *Field, groups []tokenGroup, o *matchOptions, and bool) query.Query {
	if len(groups) == 0 {
		return nil
	}
	if len(groups) == 1 && len(groups[0].terms) == 1 {
		return &termsUnionQuery{field: field, constant: true, boost: 1, expand: prefixExpansion(field, groups[0].terms[0])}
	}
	var clauses []query.Query
	for i, g := range groups {
		last := i == len(groups)-1
		var subs []query.Query
		for _, t := range g.terms {
			if last {
				subs = append(subs, &termsUnionQuery{field: field, constant: true, boost: 1, expand: prefixExpansion(field, t)})
			} else {
				tq := bleve.NewTermQuery(t)
				tq.SetField(field)
				subs = append(subs, &isolatedQuery{inner: tq})
			}
		}
		if len(subs) == 1 {
			clauses = append(clauses, subs[0])
		} else {
			clauses = append(clauses, &luceneBoolQuery{should: subs})
		}
	}
	if and {
		return &luceneBoolQuery{must: clauses}
	}
	return &luceneBoolQuery{should: clauses}
}

func wildcardExpansion(field, literal string, tokens []wildcardToken) func(i index.IndexReader) ([]string, []float64, error) {
	return func(i index.IndexReader) ([]string, []float64, error) {
		terms, err := dictTerms(i, field, literal)
		if err != nil {
			return nil, nil, err
		}
		var out []string
		for _, t := range terms {
			if wildcardMatches(tokens, t, false) {
				out = append(out, t)
			}
		}
		return out, nil, nil
	}
}

func (p *qsParser) regexpQuery(field *string, pattern string) query.Query {
	fields := p.extractFields(field, false)
	if len(fields) == 0 {
		return bleve.NewMatchNoneQuery()
	}
	var qs []query.Query
	for _, fw := range fields {
		f, _, ok := p.qb.ix.Mapping.resolve(fw.field)
		if !ok {
			qs = append(qs, bleve.NewMatchNoneQuery())
			continue
		}
		if !isStringField(f) {
			qs = append(qs, p.lenientOr(stringQueryTypeError("regexp", fw.field, f)))
			continue
		}
		re, msg := compileLuceneRegexp(p.normalizeText(f, pattern), reFlagAll, false)
		if msg != "" {
			qs = append(qs, p.lenientOr(errCreateQuery("illegal_argument_exception", msg)))
			continue
		}
		name := p.qb.ix.Mapping.searchPath(fw.field)
		q := &termsUnionQuery{field: name, constant: true, boost: 1, expand: func(i index.IndexReader) ([]string, []float64, error) {
			terms, err := dictTerms(i, name, "")
			if err != nil {
				return nil, nil, err
			}
			var out []string
			for _, t := range terms {
				if re.Matches(t) {
					out = append(out, t)
				}
			}
			return out, nil, nil
		}}
		qs = append(qs, newBoost(q, fw.boost))
	}
	return p.orTie(qs)
}

func (p *qsParser) fuzzyQuery(field *string, text, slop string) query.Query {
	var fz *fuzzinessSpec
	if slop == "~" {
		fz = p.spec.fuzziness
	} else {
		fz = &fuzzinessSpec{value: slop[1:]}
		if strings.EqualFold(slop[1:], "auto") {
			fz = &fuzzinessSpec{auto: true, low: 3, high: 6}
		} else if _, ok := javaDoubleOK(slop[1:]); !ok {
			p.fail(errCreateNumberFormat(strings.ToUpper(slop[1:])))
		}
	}
	fields := p.extractFields(field, false)
	if len(fields) == 0 {
		return bleve.NewMatchNoneQuery()
	}
	var qs []query.Query
	for _, fw := range fields {
		f, _, ok := p.qb.ix.Mapping.resolve(fw.field)
		if !ok {
			qs = append(qs, bleve.NewMatchNoneQuery())
			continue
		}
		if !isStringField(f) {
			qs = append(qs, p.lenientOr(errCreateQuery("illegal_argument_exception", "Can only use fuzzy queries on keyword and text fields - not on ["+fw.field+"] which is of type ["+f.Type+"]")))
			continue
		}
		term := p.normalizeText(f, text)
		q := p.qb.fuzzyTermQuery(p.qb.ix.Mapping.searchPath(fw.field), term, fz.distance(term), p.spec.fuzzyPrefixLength, max(1, p.spec.fuzzyMaxExpansions), p.spec.transpositions)
		qs = append(qs, newBoost(q, fw.boost))
	}
	return p.orTie(qs)
}

func (p *qsParser) rangeQuery(field *string, lo, hi *string, startInc, endInc bool) query.Query {
	fields := p.extractFields(field, false)
	if len(fields) == 0 {
		return bleve.NewMatchNoneQuery()
	}
	var qs []query.Query
	for _, fw := range fields {
		qs = append(qs, newBoost(p.rangeSingle(fw.field, lo, hi, startInc, endInc), fw.boost))
	}
	return p.orTie(qs)
}

func (p *qsParser) rangeSingle(field string, lo, hi *string, startInc, endInc bool) query.Query {
	f, _, ok := p.qb.ix.Mapping.resolve(field)
	if !ok {
		return bleve.NewMatchNoneQuery()
	}
	switch f.Type {
	case TypeGeoPoint, TypeGeoShape:
		return p.lenientOr(errCreateQuery("illegal_argument_exception", "Field ["+field+"] of type ["+f.Type+"] does not support range queries"))
	}
	params := M{}
	if lo != nil {
		v := *lo
		if !f.isNumeric() && !f.isDate() {
			v = p.normalizeText(f, v)
		}
		if startInc {
			params["gte"] = v
		} else {
			params["gt"] = v
		}
	}
	if hi != nil {
		v := *hi
		if !f.isNumeric() && !f.isDate() {
			v = p.normalizeText(f, v)
		}
		if endInc {
			params["lte"] = v
		} else {
			params["lt"] = v
		}
	}
	if p.spec.timeZone != "" {
		params["time_zone"] = p.spec.timeZone
	}
	var q query.Query
	if lo == nil && hi == nil {
		q = p.qb.existsQuery(field)
	} else {
		var err error
		if q, err = p.qb.rangeQuery(M{field: params}); err != nil {
			return p.lenientOr(err)
		}
	}
	if _, none := q.(*query.MatchNoneQuery); none {
		return q
	}
	return &constantScoreQuery{inner: q, score: 1}
}

// query_string -----------------------------------------------------------------

func (qb *queryBuilder) queryStringToQuery(spec *queryStringSpec) (query.Query, error) {
	p := &qsParser{qb: qb, spec: spec, andOp: spec.defaultOperator == "and"}
	if spec.analyzer != "" {
		an, err := qb.ix.analysis.analyzerNamed(spec.analyzer)
		if err != nil {
			return nil, errQueryShard("[query_string] analyzer [%s] not found", spec.analyzer)
		}
		p.force, p.analyzer = an, spec.analyzer
	}
	if spec.quoteAnalyzer != "" {
		an, err := qb.ix.analysis.analyzerNamed(spec.quoteAnalyzer)
		if err != nil {
			return nil, errQueryShard("[query_string] quote_analyzer [%s] not found", spec.quoteAnalyzer)
		}
		p.quoteAn, p.quoteAnName = an, spec.quoteAnalyzer
	}
	for _, rw := range []*string{spec.fuzzyRewrite, spec.rewrite} {
		if rw != nil && !validRewrite(*rw) {
			return nil, errCreateQuery("illegal_argument_exception", "Failed to parse rewrite_method ["+*rw+"]")
		}
	}
	lenientDefault := getBool(getMap(getMap(qb.ix.Settings, "index"), "query_string"), "lenient", false)
	explicit := spec.lenient != nil
	lenient := lenientDefault
	if explicit {
		lenient = *spec.lenient
	}
	allFieldsLenient := func() bool {
		if explicit {
			return *spec.lenient
		}
		return true
	}
	star := "*"
	switch {
	case spec.hasDefaultField:
		if spec.defaultField == "*" {
			p.field = &star
			p.weights = qb.resolveFieldPattern("*", 1, false, false, "")
			lenient = allFieldsLenient()
		} else {
			df := spec.defaultField
			p.field = &df
		}
	case len(spec.fields) > 0:
		p.weights = qb.resolveFields(spec.fields, "")
		if hasAllFieldsWildcard(spec.fields) {
			lenient = allFieldsLenient()
		}
	default:
		var defaults []fieldWeight
		all := false
		for _, name := range qb.ix.defaultFields() {
			fw, err := parseFieldAndWeight(name)
			if err != nil {
				return nil, err
			}
			if fw.field == "*" {
				all = true
			}
			defaults = append(defaults, fw)
		}
		if all {
			p.field = &star
			p.weights = qb.resolveFieldPattern("*", 1, false, false, "")
			lenient = allFieldsLenient()
		} else {
			p.weights = qb.resolveFields(defaults, "")
		}
	}
	p.lenient = lenient
	p.tie = 0
	if spec.typ == "most_fields" || spec.typ == "bool_prefix" {
		p.tie = 1
	}
	if spec.tieBreaker != nil {
		p.tie = float64(float32(*spec.tieBreaker))
	}
	text := spec.query
	if spec.escape {
		text = luceneEscape(text)
	}
	if strings.TrimSpace(text) == "" {
		return bleve.NewMatchNoneQuery(), nil
	}
	toks, lexErr := lexQueryString(text)
	if lexErr != nil {
		return nil, errQueryStringParse(spec.query, lexErr)
	}
	p.toks = toks
	q, err := p.parse()
	if err != nil {
		if se, ok := err.(*qsSyntax); ok {
			return nil, errQueryStringParse(spec.query, se)
		}
		return nil, err
	}
	if q == nil {
		return nil, nil
	}
	q = fixNegativeQuery(q)
	return applyMSM(q, spec.msm)
}

// simple_query_string ------------------------------------------------------------

type sqsState struct {
	data     []rune
	index    int
	end      int
	top      query.Query
	not      int
	current  *qsOccur
	previous *qsOccur
}

type sqsParser struct {
	qb       *queryBuilder
	spec     *simpleQueryStringSpec
	weights  []fieldWeight
	force    analysis.Analyzer
	analyzer string
	lenient  bool
	flags    int
	defOp    qsOccur
	depth    int // open groups, bounded by maxQueryStringDepth
	err      error
}

func (s *sqsParser) has(flag int) bool { return s.flags&flag != 0 }

func (qb *queryBuilder) simpleQueryStringToQuery(spec *simpleQueryStringSpec) (query.Query, error) {
	s := &sqsParser{qb: qb, spec: spec, flags: spec.flags, defOp: occurShould}
	if spec.defaultOperator == "and" {
		s.defOp = occurMust
	}
	fields := spec.fields
	if len(fields) == 0 {
		for _, name := range qb.ix.defaultFields() {
			fw, err := parseFieldAndWeight(name)
			if err != nil {
				return nil, err
			}
			fields = append(fields, fw)
		}
	}
	s.weights = qb.resolveFields(fields, "")
	if spec.lenient != nil {
		s.lenient = *spec.lenient
	}
	if hasAllFieldsWildcard(fields) && spec.lenient == nil {
		s.lenient = true
	}
	if spec.analyzer != "" {
		an, err := qb.ix.analysis.analyzerNamed(spec.analyzer)
		if err != nil {
			return nil, errQueryShard("[simple_query_string] analyzer [%s] not found", spec.analyzer)
		}
		s.force, s.analyzer = an, spec.analyzer
	}
	text := spec.query
	var q query.Query
	if strings.TrimSpace(text) == "*" {
		q = &constantScoreQuery{inner: bleve.NewMatchAllQuery(), score: 1}
	} else {
		data := []rune(text)
		st := &sqsState{data: data, end: len(data)}
		s.parseSub(st)
		if s.err != nil {
			return nil, s.err
		}
		q = st.top
		if q == nil {
			q = bleve.NewMatchNoneQuery()
		}
	}
	if spec.msm != nil {
		if _, isBool := q.(*luceneBoolQuery); isBool {
			return applyMSM(q, spec.msm)
		}
	}
	return q, nil
}

func isSQSSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

func (s *sqsParser) parseSub(st *sqsState) {
	for st.index < st.end && s.err == nil {
		c := st.data[st.index]
		switch {
		case c == '(' && s.has(sqsPrecedence):
			s.consumeSub(st)
		case c == ')' && s.has(sqsPrecedence):
			st.index++
		case c == '"' && s.has(sqsPhrase):
			s.consumePhrase(st)
		case c == '+' && s.has(sqsAnd):
			if st.current == nil && st.top != nil {
				o := occurMust
				st.current = &o
			}
			st.index++
		case c == '|' && s.has(sqsOr):
			if st.current == nil && st.top != nil {
				o := occurShould
				st.current = &o
			}
			st.index++
		case c == '-' && s.has(sqsNot):
			st.not++
			st.index++
			continue
		case isSQSSpace(c) && s.has(sqsWhitespace):
			st.index++
		default:
			s.consumeToken(st)
		}
		st.not = 0
	}
}

func (s *sqsParser) consumeSub(st *sqsState) {
	st.index++
	start := st.index
	precedence := 1
	escaped := false
	for st.index < st.end {
		if !escaped {
			c := st.data[st.index]
			if c == '\\' && s.has(sqsEscape) {
				escaped = true
				st.index++
				continue
			} else if c == '(' {
				precedence++
			} else if c == ')' {
				precedence--
				if precedence == 0 {
					break
				}
			}
		}
		escaped = false
		st.index++
	}
	switch {
	case st.index == st.end:
		st.index = start
	case st.index == start:
		st.current = nil
		st.index++
	default:
		s.depth++
		if s.depth > maxQueryStringDepth {
			if s.err == nil {
				s.err = errParsing("simple_query_string is nested too deeply (more than %d groups)", maxQueryStringDepth)
			}
			st.index = st.end
			s.depth--
			return
		}
		sub := &sqsState{data: st.data, index: start, end: st.index}
		s.parseSub(sub)
		s.depth--
		s.buildTree(st, sub.top)
		st.index++
	}
}

func (s *sqsParser) consumePhrase(st *sqsState) {
	st.index++
	start := st.index
	var buf []rune
	escaped := false
	hasSlop := false
	for st.index < st.end {
		if !escaped {
			c := st.data[st.index]
			if c == '\\' && s.has(sqsEscape) {
				escaped = true
				st.index++
				continue
			} else if c == '"' {
				if st.end > st.index+1 && st.data[st.index+1] == '~' && s.has(sqsNear) {
					st.index++
					if st.end > st.index+1 {
						hasSlop = true
					}
				}
				break
			}
		}
		escaped = false
		buf = append(buf, st.data[st.index])
		st.index++
	}
	switch {
	case st.index == st.end:
		st.index = start
	case st.index == start:
		st.current = nil
		st.index++
	default:
		slop := 0
		if hasSlop {
			slop = s.parseFuzziness(st)
		}
		s.buildTree(st, s.newPhrase(string(buf), slop))
		st.index++
	}
}

func (s *sqsParser) tokenFinished(st *sqsState) bool {
	c := st.data[st.index]
	return (c == '"' && s.has(sqsPhrase)) || (c == '|' && s.has(sqsOr)) || (c == '+' && s.has(sqsAnd)) ||
		(c == '(' && s.has(sqsPrecedence)) || (c == ')' && s.has(sqsPrecedence)) || (isSQSSpace(c) && s.has(sqsWhitespace))
}

func (s *sqsParser) consumeToken(st *sqsState) {
	var buf []rune
	escaped, prefix, fuzzy := false, false, false
	for st.index < st.end {
		if !escaped {
			c := st.data[st.index]
			if c == '\\' && s.has(sqsEscape) {
				escaped = true
				prefix = false
				st.index++
				continue
			} else if s.tokenFinished(st) {
				break
			} else if len(buf) > 0 && c == '~' && s.has(sqsFuzzy) {
				fuzzy = true
				break
			}
			prefix = len(buf) > 0 && c == '*' && s.has(sqsPrefix)
		}
		escaped = false
		buf = append(buf, st.data[st.index])
		st.index++
	}
	if len(buf) == 0 {
		return
	}
	var branch query.Query
	switch {
	case fuzzy:
		token := string(buf)
		fz := s.parseFuzziness(st)
		if fz > 2 {
			fz = 2
		}
		if fz == 0 {
			branch = s.newDefault(token)
		} else {
			branch = s.newFuzzy(token, fz)
		}
	case prefix:
		branch = s.newPrefix(string(buf[:len(buf)-1]))
	default:
		branch = s.newDefault(string(buf))
	}
	s.buildTree(st, branch)
}

func (s *sqsParser) parseFuzziness(st *sqsState) int {
	var slop []rune
	if st.data[st.index] == '~' {
		for st.index < st.end {
			st.index++
			if st.index < st.end {
				if s.tokenFinished(st) {
					break
				}
				slop = append(slop, st.data[st.index])
			}
		}
		if len(slop) == 0 {
			return 2
		}
		n, err := strconv.Atoi(string(slop))
		if err != nil || n < 0 {
			return 0
		}
		return n
	}
	return 0
}

func (s *sqsParser) buildTree(st *sqsState, branch query.Query) {
	if branch == nil {
		return
	}
	if st.not%2 == 1 {
		branch = &luceneBoolQuery{mustNot: []query.Query{branch}, should: []query.Query{&constantScoreQuery{inner: bleve.NewMatchAllQuery(), score: 1}}}
	}
	if st.top == nil {
		st.top = branch
	} else {
		if st.current == nil {
			o := s.defOp
			st.current = &o
		}
		if st.previous == nil || *st.previous != *st.current {
			wrapper := &luceneBoolQuery{}
			addOccur(wrapper, st.top, *st.current)
			st.top = wrapper
		}
		bq := st.top.(*luceneBoolQuery)
		c := *bq
		c.must = append([]query.Query(nil), bq.must...)
		c.should = append([]query.Query(nil), bq.should...)
		addOccur(&c, branch, *st.current)
		st.top = &c
		prev := *st.current
		st.previous = &prev
	}
	st.current = nil
}

func addOccur(bq *luceneBoolQuery, q query.Query, o qsOccur) {
	if o == occurMust {
		bq.must = append(bq.must, q)
	} else {
		bq.should = append(bq.should, q)
	}
}

func (s *sqsParser) options() *matchOptions {
	op := "or"
	if s.defOp == occurMust {
		op = "and"
	}
	return &matchOptions{operator: op, lenient: s.lenient, zeroTermsNull: true, maxExpansions: 50, transpositions: true}
}

func (s *sqsParser) fail(err error) query.Query {
	if s.err == nil {
		s.err = err
	}
	return nil
}

func (s *sqsParser) dismax(qs []query.Query) query.Query {
	var out []query.Query
	for _, q := range qs {
		if q != nil {
			out = append(out, q)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return &disMaxQuery{queries: out, tie: 1}
}

func (s *sqsParser) newDefault(text string) query.Query {
	if len(s.weights) == 0 {
		return bleve.NewMatchNoneQuery()
	}
	o := s.options()
	var qs []query.Query
	for _, fw := range s.weights {
		q, err := s.qb.matchField("simple_query_string", fw.field, text, s.analyzer, o, false)
		if err != nil {
			return s.fail(err)
		}
		if q != nil {
			qs = append(qs, newBoost(q, fw.boost))
		}
	}
	switch len(qs) {
	case 0:
		return nil
	case 1:
		return qs[0]
	}
	return &disMaxQuery{queries: qs, tie: 0}
}

func (s *sqsParser) newPhrase(text string, slop int) query.Query {
	weights := s.weights
	if s.spec.quoteFieldSuffix != "" {
		weights = s.qb.resolveFields(s.weights, s.spec.quoteFieldSuffix)
	}
	if len(weights) == 0 {
		return bleve.NewMatchNoneQuery()
	}
	o := s.options()
	var qs []query.Query
	for _, fw := range weights {
		q, err := s.qb.phraseField("simple_query_string", fw.field, text, s.analyzer, slop, false, 50, o)
		if err != nil {
			return s.fail(err)
		}
		if q != nil {
			qs = append(qs, newBoost(q, fw.boost))
		}
	}
	switch len(qs) {
	case 0:
		return nil
	case 1:
		return qs[0]
	}
	return &disMaxQuery{queries: qs, tie: 0}
}

func (s *sqsParser) normalize(f *Field, text string) string {
	an := s.force
	if an == nil {
		if f.Type != TypeText && f.Type != TypeMatchOnlyText && f.Type != TypeSearchAsYouType {
			return s.qb.normalizeForField(f, text)
		}
		an, _, _ = s.qb.searchAnalyzer("simple_query_string", f, "", false)
	}
	if an != nil && analyzerLowercases(an) {
		return strings.ToLower(text)
	}
	return text
}

func (s *sqsParser) newFuzzy(text string, edits int) query.Query {
	var qs []query.Query
	for _, fw := range s.weights {
		f, _, ok := s.qb.ix.Mapping.resolve(fw.field)
		if !ok {
			qs = append(qs, bleve.NewMatchNoneQuery())
			continue
		}
		if !isStringField(f) {
			err := errCreateQuery("illegal_argument_exception", "Can only use fuzzy queries on keyword and text fields - not on ["+fw.field+"] which is of type ["+f.Type+"]")
			if s.lenient {
				qs = append(qs, bleve.NewMatchNoneQuery())
				continue
			}
			return s.fail(err)
		}
		term := s.normalize(f, text)
		q := s.qb.fuzzyTermQuery(s.qb.ix.Mapping.searchPath(fw.field), term, edits, s.spec.fuzzyPrefixLength, max(1, s.spec.fuzzyMaxExpansions), s.spec.transpositions)
		qs = append(qs, newBoost(q, fw.boost))
	}
	return s.dismax(qs)
}

func (s *sqsParser) newPrefix(text string) query.Query {
	var qs []query.Query
	for _, fw := range s.weights {
		f, _, ok := s.qb.ix.Mapping.resolve(fw.field)
		if !ok {
			qs = append(qs, bleve.NewMatchNoneQuery())
			continue
		}
		if !isStringField(f) {
			if s.lenient {
				qs = append(qs, bleve.NewMatchNoneQuery())
				continue
			}
			return s.fail(stringQueryTypeError("prefix", fw.field, f))
		}
		var q query.Query
		if s.spec.analyzeWildcard {
			an := s.force
			if an == nil {
				an, _, _ = s.qb.searchAnalyzer("simple_query_string", f, "", false)
			}
			if an == nil {
				q = &termsUnionQuery{field: s.qb.ix.Mapping.searchPath(fw.field), constant: true, boost: 1, expand: prefixExpansion(s.qb.ix.Mapping.searchPath(fw.field), s.normalize(f, text))}
			} else {
				q = analyzedPrefix(s.qb, fw.field, f, analyzeGroups(an, text), s.options(), s.defOp == occurMust)
			}
		} else {
			q = &termsUnionQuery{field: s.qb.ix.Mapping.searchPath(fw.field), constant: true, boost: 1, expand: prefixExpansion(s.qb.ix.Mapping.searchPath(fw.field), s.normalize(f, text))}
		}
		if q != nil {
			qs = append(qs, newBoost(q, fw.boost))
		}
	}
	return s.dismax(qs)
}
