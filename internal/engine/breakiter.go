package engine

import (
	"unicode"
)

// This file emulates java.text.BreakIterator.getSentenceInstance and
// getWordInstance for Locale.ROOT, the JDK's rule based iterators that
// OpenSearch's highlighters use to cut passages. Offsets are UTF-16 code
// unit indices, as in Java strings.
//
// The JDK iterates with two small state machines. Going forward it starts
// after the previous break, feeds the category of one character after the
// other, moves the break after every character that ends in an accepting
// state (or back to a saved position when a look-ahead rule completes) and
// stops when no transition is left. preceding and following first back up
// to a "safe" position with a backward machine (or reuse the last break they
// computed) and then iterate forward; when the safe position is not a real
// break the result can differ from plain forward iteration, which is kept
// here because the highlighters observe it. Format characters (and, for
// sentences, non-spacing and enclosing marks) are skipped without changing
// the state.

const breakDone = -1

// javaDone is CharacterIterator.DONE.
const javaDone = 0xffff

// breakIterator is the subset of java.text.BreakIterator the highlighters
// use.
type breakIterator interface {
	setText(text []uint16)
	first() int
	last() int
	current() int
	next() int
	preceding(offset int) int
	following(offset int) int
}

// breakRules is a pair of break state machines: state 0 stops, state 1
// starts.
type breakRules struct {
	category  func(r rune) int // -1 for characters that are skipped
	forward   [][]int8
	backward  [][]int8
	accepting uint32 // forward states after which a break may follow
	lookahead uint32 // forward states saving (or, when accepting, using) a candidate break
}

// ruleBreakIterator is sun.text.RuleBasedBreakIterator over a UTF-16 text.
type ruleBreakIterator struct {
	rules  *breakRules
	text   []uint16
	pos    int // the character iterator index
	cached int // cachedLastKnownBreak
}

func newSentenceBreakIterator() *ruleBreakIterator {
	return &ruleBreakIterator{rules: sentenceRules, cached: breakDone}
}

func newWordBreakIterator() *ruleBreakIterator {
	return &ruleBreakIterator{rules: wordRules, cached: breakDone}
}

func (it *ruleBreakIterator) setText(text []uint16) {
	it.text = text
	it.pos = 0
	it.cached = breakDone
}

func (it *ruleBreakIterator) first() int {
	it.pos = 0
	return 0
}

func (it *ruleBreakIterator) last() int {
	it.pos = len(it.text)
	return it.pos
}

func (it *ruleBreakIterator) current() int { return it.pos }

func (it *ruleBreakIterator) next() int { return it.handleNext() }

// codePointCount is the length of the character at the index (2 for a
// surrogate pair).
func (it *ruleBreakIterator) codePointCount() int {
	if it.pos < len(it.text) && it.text[it.pos] >= 0xd800 && it.text[it.pos] < 0xdc00 && it.pos+1 < len(it.text) {
		if d := it.text[it.pos+1]; d >= 0xdc00 && d <= 0xdfff {
			return 2
		}
	}
	return 1
}

func (it *ruleBreakIterator) getCurrent() rune {
	if it.pos >= len(it.text) {
		return javaDone
	}
	if it.codePointCount() == 2 {
		return (rune(it.text[it.pos])-0xd800)<<10 + rune(it.text[it.pos+1]) - 0xdc00 + 0x10000
	}
	return rune(it.text[it.pos])
}

func (it *ruleBreakIterator) getNext() rune {
	index := it.pos
	if index == len(it.text) {
		return javaDone
	}
	if index += it.codePointCount(); index >= len(it.text) {
		return javaDone
	}
	it.pos = index
	return it.getCurrent()
}

func (it *ruleBreakIterator) getNextIndex() int {
	if index := it.pos + it.codePointCount(); index < len(it.text) {
		return index
	}
	return len(it.text)
}

func (it *ruleBreakIterator) getPrevious() rune {
	if it.pos == 0 {
		return javaDone
	}
	it.pos--
	c2 := rune(it.text[it.pos])
	if c2 >= 0xdc00 && c2 <= 0xdfff && it.pos > 0 {
		it.pos--
		c1 := rune(it.text[it.pos])
		if c1 >= 0xd800 && c1 < 0xdc00 {
			return (c1-0xd800)<<10 + c2 - 0xdc00 + 0x10000
		}
		it.pos++
	}
	return c2
}

func (it *ruleBreakIterator) category(c rune) int {
	if c >= 0x10000 && unicode.Is(unicode.Cf, c) {
		return 0 // the JDK's supplementary table has no ignore category
	}
	return it.rules.category(c)
}

func (it *ruleBreakIterator) handleNext() int {
	if it.pos == len(it.text) {
		return breakDone
	}
	br := it.rules
	result := it.getNextIndex()
	lookahead := 0
	state := 1
	c := it.getCurrent()
	for c != javaDone && state != 0 {
		if cat := it.category(c); cat >= 0 {
			state = int(br.forward[state][cat])
		}
		bit := uint32(1) << state
		switch {
		case br.lookahead&bit != 0 && br.accepting&bit != 0:
			result = lookahead
		case br.lookahead&bit != 0:
			lookahead = it.getNextIndex()
		case br.accepting&bit != 0:
			result = it.getNextIndex()
		}
		c = it.getNext()
	}
	if c == javaDone && lookahead == len(it.text) {
		result = lookahead
	}
	it.pos = result
	return result
}

func (it *ruleBreakIterator) handlePrevious() int {
	br := it.rules
	state, cat, lastCat := 1, 0, 0
	c := it.getCurrent()
	for c != javaDone && state != 0 {
		lastCat = cat
		cat = it.category(c)
		if cat >= 0 {
			state = int(br.backward[state][cat])
		}
		c = it.getPrevious()
	}
	if c != javaDone {
		it.getNext()
		if lastCat >= 0 {
			it.getNext()
		}
	}
	return it.pos
}

func (it *ruleBreakIterator) previous() int {
	if it.pos == 0 {
		return breakDone
	}
	start := it.pos
	lastResult := it.cached
	if lastResult >= start || lastResult <= breakDone {
		it.getPrevious()
		lastResult = it.handlePrevious()
	} else {
		it.pos = lastResult
	}
	result := lastResult
	for result != breakDone && result < start {
		lastResult = result
		result = it.handleNext()
	}
	it.pos = lastResult
	it.cached = lastResult
	return lastResult
}

// following returns the first boundary after offset (DONE at the end).
func (it *ruleBreakIterator) following(offset int) int {
	it.pos = offset
	if offset == 0 {
		it.cached = it.handleNext()
		return it.cached
	}
	result := it.cached
	if result >= offset || result <= breakDone {
		result = it.handlePrevious()
	} else {
		it.pos = result
	}
	for result != breakDone && result <= offset {
		result = it.handleNext()
	}
	it.cached = result
	return result
}

// preceding returns the last boundary before offset (DONE at the start).
func (it *ruleBreakIterator) preceding(offset int) int {
	it.pos = offset
	return it.previous()
}

// sentences ---------------------------------------------------------------

const (
	sbComma  = iota
	sbPara   // U+2029
	sbPeriod // . and the fullwidth period
	sbQuote  // " and ', opening or closing
	sbTerm   // ! ? and the ideographic and fullwidth terminators
	sbUpper  // letters that are not lowercase
	sbLower
	sbDanda
	sbOther
	sbDigit
	sbClose // closing punctuation
	sbOpen  // opening punctuation
	sbSpace
)

func sentenceCategory(r rune) int {
	switch r {
	case ',':
		return sbComma
	case 0x2029:
		return sbPara
	case '.', 0xff0e:
		return sbPeriod
	case '"', '\'':
		return sbQuote
	case '!', '?', 0x3002, 0xff01, 0xff1f:
		return sbTerm
	case 0x0964, 0x0965:
		return sbDanda
	case '\t', '\n', '\f', '\r', 0x2028:
		return sbSpace
	}
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return -1
	case unicode.Is(unicode.Ll, r):
		return sbLower
	case unicode.IsLetter(r):
		return sbUpper
	case unicode.IsNumber(r):
		return sbDigit
	case unicode.In(r, unicode.Pe, unicode.Pf):
		return sbClose
	case unicode.In(r, unicode.Ps, unicode.Pi):
		return sbOpen
	case unicode.Is(unicode.Zs, r):
		return sbSpace
	}
	return sbOther
}

// Forward sentence states: 2 paragraph separator, 3 danda and spaces, 4 a
// sentence start after a candidate break, 5 period (candidate break after
// it), 6 period and space, 7 period and spaces, 8 opening punctuation or
// symbols after a candidate while the text continues, 9 period and quote,
// 10 opening characters that must be followed by a letter, 11 terminator,
// 12 terminator and spaces.
var sentenceRules = &breakRules{
	category: sentenceCategory,
	forward: [][]int8{
		// ,  PS  .  "   ?   A  a  । other 9  )   (  space
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{1, 2, 5, 1, 11, 1, 1, 3, 1, 1, 1, 1, 1},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{1, 2, 5, 9, 11, 1, 1, 8, 8, 1, 5, 8, 6},
		{1, 2, 5, 8, 11, 4, 1, 8, 8, 1, 1, 8, 7},
		{1, 2, 5, 10, 11, 4, 4, 10, 10, 1, 1, 10, 7},
		{1, 2, 5, 8, 11, 4, 4, 8, 8, 1, 1, 8, 1},
		{1, 2, 5, 9, 11, 4, 4, 10, 10, 1, 5, 10, 6},
		{0, 0, 0, 8, 0, 4, 4, 8, 8, 0, 0, 8, 0},
		{0, 2, 11, 11, 11, 0, 0, 0, 0, 0, 11, 0, 12},
		{0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 12},
	},
	backward: [][]int8{
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{9, 10, 10, 10, 10, 9, 3, 4, 4, 3, 10, 9, 10},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{9, 10, 10, 3, 2, 9, 3, 4, 4, 3, 7, 3, 5},
		{9, 10, 2, 4, 2, 9, 3, 4, 4, 3, 8, 4, 6},
		{9, 10, 10, 7, 2, 9, 3, 4, 4, 3, 7, 9, 5},
		{9, 10, 2, 8, 2, 9, 3, 4, 4, 3, 8, 9, 6},
		{9, 10, 10, 7, 2, 9, 3, 4, 4, 3, 7, 9, 10},
		{9, 10, 2, 8, 2, 9, 3, 4, 4, 3, 8, 9, 10},
		{9, 0, 10, 10, 0, 9, 3, 4, 4, 3, 10, 9, 10},
		{9, 0, 10, 10, 10, 9, 3, 4, 4, 3, 10, 9, 10},
	},
	accepting: 1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<11 | 1<<12,
	lookahead: 1<<4 | 1<<5 | 1<<6 | 1<<7 | 1<<9,
}

// words -------------------------------------------------------------------

const (
	wbControl  = iota
	wbKanaMark // combining voiced sound marks
	wbPeriod
	wbKanji
	wbLetter
	wbDanda
	wbPostNum // % & and per mille signs after a number
	wbCR
	wbQuote
	wbPreNum // # and currency signs before a number
	wbKanaSign
	wbMark // other non-spacing and enclosing marks
	wbDigit
	wbSpace
	wbHiragana
	wbKatakana
	wbLineSep
	wbTab
	wbSoftHyphen
	wbMidNum // , and the Arabic decimal separator inside numbers
	wbOther
	wbMidWord // dashes and connectors inside words
)

func wordCategory(r rune) int {
	switch r {
	case 0x3099, 0x309a:
		return wbKanaMark
	case '.':
		return wbPeriod
	case 0x3005:
		return wbKanji
	case 0x0964, 0x0965:
		return wbDanda
	case '%', '&', 0x00a2, 0x066a, 0x2030, 0x2031:
		return wbPostNum
	case '\r':
		return wbCR
	case '"', '\'':
		return wbQuote
	case '#':
		return wbPreNum
	case 0x309b, 0x309c, 0x30fb, 0x30fc:
		return wbKanaSign
	case '\n', '\f', 0x2028, 0x2029:
		return wbLineSep
	case '\t':
		return wbTab
	case 0x00ad:
		return wbSoftHyphen
	case ',', 0x066b:
		return wbMidNum
	case 0x2027:
		return wbMidWord
	}
	switch {
	case r >= 0x4e00 && r <= 0x9fa5, r >= 0xf900 && r <= 0xfa2d:
		return wbKanji
	case r >= 0x3041 && r <= 0x3094, r == 0x309d, r == 0x309e:
		return wbHiragana
	case r >= 0x30a1 && r <= 0x30fa, r == 0x30fd, r == 0x30fe:
		return wbKatakana
	case unicode.Is(unicode.Cf, r):
		return -1
	case unicode.In(r, unicode.Mn, unicode.Me):
		return wbMark
	case unicode.IsLetter(r), unicode.Is(unicode.Mc, r):
		return wbLetter
	case unicode.IsNumber(r):
		return wbDigit
	case unicode.Is(unicode.Sc, r):
		return wbPreNum
	case unicode.Is(unicode.Zs, r):
		return wbSpace
	case unicode.In(r, unicode.Pd, unicode.Pc):
		return wbMidWord
	case unicode.Is(unicode.Cc, r):
		return wbControl
	}
	return wbOther
}

// Forward word states: 2 a single character, 3 letters, 4 letters and inner
// punctuation, 5 letters and danda, 6 digits, 7 digits and inner
// punctuation, 8 whitespace, 9 carriage return, 10-12 kana runs, 13 kanji
// run, 14 number prefix, 15 kanji, 16 a character with marks, 17 kana sign,
// 18 hiragana, 19 katakana.
var wordRules = &breakRules{
	category: wordCategory,
	forward: [][]int8{
		// ctl ゙ .  漢  a  ।  %  CR  "  $  ゛ mark 9 sp  ひ  カ  LF tab shy ,  other -
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{2, 12, 14, 15, 3, 16, 16, 9, 16, 14, 17, 2, 6, 8, 18, 19, 2, 8, 2, 16, 16, 16},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 3, 4, 0, 3, 5, 0, 0, 4, 0, 0, 3, 6, 0, 0, 0, 0, 0, 4, 0, 0, 4},
		{0, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 6, 7, 0, 3, 0, 2, 0, 7, 0, 0, 6, 6, 0, 0, 0, 0, 0, 0, 7, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 8, 0, 0, 0, 0, 0, 9, 0, 0, 0, 8, 0, 8, 0, 0, 2, 8, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0},
		{0, 10, 0, 0, 0, 0, 0, 0, 0, 0, 10, 0, 0, 0, 0, 10, 0, 0, 0, 0, 0, 0},
		{0, 11, 0, 0, 0, 0, 0, 0, 0, 0, 11, 0, 0, 0, 11, 0, 0, 0, 0, 0, 0, 0},
		{0, 12, 0, 0, 0, 0, 0, 0, 0, 0, 12, 0, 0, 0, 11, 10, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 13, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 16, 0, 0, 0, 0, 0, 0, 0, 0, 0, 16, 6, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 16, 0, 13, 0, 0, 0, 0, 0, 0, 0, 16, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 16, 0, 0, 0, 0, 0, 0, 0, 0, 0, 16, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 17, 0, 0, 0, 0, 0, 0, 0, 0, 12, 16, 0, 0, 11, 10, 0, 0, 0, 0, 0, 0},
		{0, 18, 0, 0, 0, 0, 0, 0, 0, 0, 11, 16, 0, 0, 11, 0, 0, 0, 0, 0, 0, 0},
		{0, 19, 0, 0, 0, 0, 0, 0, 0, 0, 10, 16, 0, 0, 0, 10, 0, 0, 0, 0, 0, 0},
	},
	backward: [][]int8{
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{2, 3, 4, 5, 6, 7, 8, 9, 4, 2, 10, 3, 11, 9, 12, 13, 14, 9, 7, 8, 2, 7},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 3, 4, 5, 6, 7, 8, 0, 4, 2, 10, 3, 11, 9, 12, 13, 0, 9, 0, 8, 2, 7},
		{0, 3, 0, 0, 6, 0, 0, 0, 0, 0, 0, 3, 11, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 3, 4, 0, 6, 0, 0, 0, 4, 0, 0, 3, 11, 0, 0, 0, 0, 0, 7, 0, 0, 7},
		{0, 3, 0, 0, 6, 0, 0, 0, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3, 11, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3, 0, 9, 0, 0, 0, 9, 0, 0, 0, 0},
		{0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 10, 0, 0, 0, 12, 13, 0, 0, 0, 0, 0, 0},
		{0, 3, 4, 0, 6, 7, 0, 0, 4, 2, 0, 3, 11, 0, 0, 0, 0, 0, 0, 8, 0, 0},
		{0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 10, 0, 0, 0, 12, 0, 0, 0, 0, 0, 0, 0},
		{0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 10, 0, 0, 0, 0, 13, 0, 0, 0, 0, 0, 0},
		{0, 3, 0, 0, 0, 0, 0, 9, 0, 0, 0, 3, 0, 9, 0, 0, 0, 9, 0, 0, 0, 0},
	},
	accepting: 0xfffff &^ (1<<0 | 1<<4 | 1<<7),
}
